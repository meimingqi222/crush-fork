package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"charm.land/fantasy"
	"github.com/kaptinlin/jsonschema"

	"github.com/charmbracelet/crush/internal/message"
)

//go:embed yield.md
var yieldDescription []byte

const YieldToolName = "yield"

// YieldParams is the input for the yield tool.
type YieldParams struct {
	Status string `json:"status" description:"Terminal status: completed, completed_with_warnings, failed, canceled, or blocked"`
	Data   string `json:"data,omitempty" description:"The complete result text to submit to the parent agent. Required unless status is failed or blocked."`
	Error  string `json:"error,omitempty" description:"The error message. Required if status is failed or blocked."`
	// Payload is declared as `any` (not json.RawMessage) on purpose: the tool
	// input schema is reflected from this struct, and json.RawMessage is a
	// []byte, which reflects to {"type":"array","items":{"type":"integer"}}.
	// That made the advertised schema contradict the output schema injected
	// into the system prompt, so every structured yield failed validation at
	// least once. `any` reflects to {"type":"object"}.
	Payload any `json:"payload,omitempty" description:"Optional structured JSON object. This must conform to the expected output schema if OutputSchema is defined."`
}

// YieldOption configures optional behavior for the yield tool.
type YieldOption func(*yieldConfig)

type yieldConfig struct {
	outputSchema     any
	payloadProjector func(payload json.RawMessage, schema any) string
}

// WithOutputSchema sets the JSON schema used to validate the payload field.
func WithOutputSchema(schema any) YieldOption {
	return func(cfg *yieldConfig) {
		cfg.outputSchema = schema
	}
}

// WithPayloadProjector sets the deterministic payload-to-text projector used
// to fill the tool response's Content when the model only submits a payload
// (no data). The projector must be a pure function -- it must not call a
// model. Package agent supplies the canonical implementation
// (projectYieldPayload) when registering this tool for subagents; the
// indirection exists because internal/agent imports internal/agent/tools,
// so the projector cannot live here without an import cycle.
func WithPayloadProjector(projector func(payload json.RawMessage, schema any) string) YieldOption {
	return func(cfg *yieldConfig) {
		cfg.payloadProjector = projector
	}
}

func NewYieldTool(messages message.Service, opts ...YieldOption) fantasy.AgentTool {
	var cfg yieldConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	// Compile the output schema once if provided.
	var compiledSchema *jsonschema.Schema
	if cfg.outputSchema != nil {
		schemaBytes, err := json.Marshal(cfg.outputSchema)
		if err == nil && len(schemaBytes) > 2 { // Skip empty objects "{}".
			compiler := jsonschema.NewCompiler()
			compiled, compileErr := compiler.Compile(schemaBytes)
			if compileErr == nil {
				compiledSchema = compiled
			}
		}
	}

	// maxSchemaRetries is the number of times the model is allowed to retry
	// a payload that fails schema validation before the result is
	// force-accepted. This mirrors oh-my-pi's approach of giving the model
	// multiple chances with detailed feedback before accepting whatever it
	// produced — preventing hard failures while still encouraging conforming
	// output.
	const maxSchemaRetries = 3

	var (
		validationAttempts   = make(map[string]int)
		validationAttemptsMu sync.Mutex
	)

	return fantasy.NewAgentTool(
		YieldToolName,
		string(yieldDescription),
		func(ctx context.Context, params YieldParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			status := strings.TrimSpace(params.Status)
			data := strings.TrimSpace(params.Data)
			errVal := strings.TrimSpace(params.Error)
			payload := normalizeYieldPayload(params.Payload)

			sessionID := strings.TrimSpace(GetSessionFromContext(ctx))
			if sessionID != "" && messages != nil {
				if toolAlreadyCalled(ctx, messages, sessionID, func(tr message.ToolResult) bool {
					if tr.Name != YieldToolName {
						return false
					}
					_, ok := tr.Yield()
					return ok
				}) {
					return fantasy.NewTextErrorResponse("yield has already been called for this session. Do not call it again."), nil
				}
			}

			// Validate terminal statuses.
			switch message.ToolResultSubtaskStatus(status) {
			case message.ToolResultSubtaskStatusCompleted,
				message.ToolResultSubtaskStatusCompletedWithWarnings,
				message.ToolResultSubtaskStatusFailed,
				message.ToolResultSubtaskStatusCanceled,
				message.ToolResultSubtaskStatusBlocked:
			default:
				if status == "" {
					status = "completed"
				} else {
					return fantasy.NewTextErrorResponse("status must be one of completed, completed_with_warnings, failed, canceled, or blocked"), nil
				}
			}

			// Validate required fields based on status.
			if (status == string(message.ToolResultSubtaskStatusFailed) || status == string(message.ToolResultSubtaskStatusBlocked)) && errVal == "" {
				return fantasy.NewTextErrorResponse("error is required for failed and blocked statuses"), nil
			}
			if (status != string(message.ToolResultSubtaskStatusFailed) && status != string(message.ToolResultSubtaskStatusBlocked)) && data == "" && len(payload) == 0 {
				return fantasy.NewTextErrorResponse("data or payload is required for successful statuses"), nil
			}

			// Validate payload against output schema if configured.
			if compiledSchema != nil && len(payload) > 0 {
				validationAttemptsMu.Lock()
				attempts := validationAttempts[sessionID]
				validationAttempts[sessionID] = attempts + 1
				validationAttemptsMu.Unlock()

				var dataValue any
				if unmarshalErr := json.Unmarshal(payload, &dataValue); unmarshalErr != nil {
					if attempts < maxSchemaRetries {
						remaining := maxSchemaRetries - attempts
						return fantasy.NewTextErrorResponse(
							fmt.Sprintf("Schema validation error: payload is not valid JSON: %s. Please fix the payload field and retry (%d attempt(s) remaining).", unmarshalErr.Error(), remaining),
						), nil
					}
					// Exhausted retries: force-accept to avoid infinite loop.
				} else {
					result := compiledSchema.Validate(dataValue)
					if !result.IsValid() {
						// Try to auto-repair the payload (inject missing required
						// string fields, remove unrecognized fields, coerce types).
						if repaired, repairErr := repairPayloadAgainstSchema(payload, cfg.outputSchema); repairErr == nil && repaired != nil {
							payload = repaired
							// Re-validate the repaired payload.
							var repairedValue any
							if json.Unmarshal(repaired, &repairedValue) == nil {
								repairedResult := compiledSchema.Validate(repairedValue)
								if repairedResult.IsValid() {
									// Repair succeeded, proceed with repaired payload.
									result = repairedResult
								}
							}
						}
					}
					if !result.IsValid() {
						if attempts < maxSchemaRetries {
							remaining := maxSchemaRetries - attempts
							errors := result.DetailedErrors()
							var errParts []string
							for path, msg := range errors {
								errParts = append(errParts, fmt.Sprintf("%s: %s", path, msg))
							}
							errMsg := strings.Join(errParts, "; ")
							if errMsg == "" {
								errMsg = "payload does not conform to the expected output schema"
							}
							return fantasy.NewTextErrorResponse(
								fmt.Sprintf("Schema validation failed: %s. Please fix the payload field and retry (%d attempt(s) remaining).", errMsg, remaining),
							), nil
						}
						// Exhausted retries: force-accept to avoid infinite loop.
					}
				}
			}

			summary := data
			if summary == "" && errVal != "" {
				summary = errVal
			}
			if summary == "" && len(payload) > 0 && cfg.payloadProjector != nil {
				// Deterministic runtime-generated text, not model-provided
				// data: it fills Content only, Data stays empty so the two
				// are never confused downstream.
				summary = cfg.payloadProjector(payload, cfg.outputSchema)
			}

			response := fantasy.NewTextResponse(summary)
			response.Metadata = message.ToolResult{Metadata: response.Metadata}.WithYield(message.ToolResultYield{
				Data:    data,
				Status:  status,
				Error:   errVal,
				Payload: payload,
			}).Metadata

			// Signal the agent loop to terminate immediately after this tool call.
			response.StopTurn = true
			return response, nil
		},
	)
}

// normalizeYieldPayload converts the decoded payload argument into raw JSON.
// Models sometimes send the payload as a JSON-encoded string instead of an
// object; in that case the string is decoded so schema validation sees the
// structure rather than a string. Returns nil when there is no payload.
func normalizeYieldPayload(value any) json.RawMessage {
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return nil
		}
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			var decoded any
			if json.Unmarshal([]byte(trimmed), &decoded) == nil {
				return json.RawMessage(trimmed)
			}
		}
		encoded, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return encoded
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		if string(encoded) == "null" {
			return nil
		}
		return encoded
	}
}

// repairPayloadAgainstSchema attempts to fix a JSON payload that failed schema
// validation by injecting default values for missing required fields and
// removing fields not defined in the schema.
//
// The schema is expected to be a JSON Schema object (map[string]any) with
// "properties" and "required" keys, as produced by json.Marshal on a struct
// or a hand-written map.
//
// Repair strategies:
//   - Missing required string fields: inject ""
//   - Missing required array fields: inject []
//   - Missing required number fields: inject 0
//   - Missing required boolean fields: inject false
//   - Missing required object fields: inject {}
//   - Unrecognized fields (not in schema properties): remove
//   - String-to-number/boolean coercion for mismatched types
func repairPayloadAgainstSchema(payload json.RawMessage, schema any) (json.RawMessage, error) {
	if len(payload) == 0 || schema == nil {
		return nil, nil
	}

	// Marshal schema to a map.
	schemaBytes, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	var schemaMap map[string]any
	if err := json.Unmarshal(schemaBytes, &schemaMap); err != nil {
		return nil, err
	}

	// Parse payload as a map.
	var payloadMap map[string]any
	if err := json.Unmarshal(payload, &payloadMap); err != nil {
		return nil, err
	}

	modified := false
	properties, _ := schemaMap["properties"].(map[string]any)

	// Inject defaults for missing required fields.
	if requiredList, ok := schemaMap["required"].([]any); ok {
		for _, req := range requiredList {
			reqName, ok := req.(string)
			if !ok {
				continue
			}
			if _, exists := payloadMap[reqName]; exists {
				continue
			}
			// Field is missing — determine type from schema.
			propSchema, hasSchema := properties[reqName].(map[string]any)
			if !hasSchema {
				// No schema for this field, inject empty string as safe default.
				payloadMap[reqName] = ""
				modified = true
				continue
			}
			propType, _ := propSchema["type"].(string)
			switch propType {
			case "string":
				payloadMap[reqName] = ""
			case "array":
				payloadMap[reqName] = []any{}
			case "number", "integer":
				payloadMap[reqName] = 0
			case "boolean":
				payloadMap[reqName] = false
			case "object":
				payloadMap[reqName] = map[string]any{}
			default:
				payloadMap[reqName] = ""
			}
			modified = true
		}
	}

	// Remove fields not defined in schema properties (if properties exist).
	if properties != nil {
		for key := range payloadMap {
			if _, known := properties[key]; !known {
				delete(payloadMap, key)
				modified = true
			}
		}
	}

	// Type coercion: fix mismatched types for known properties.
	if properties != nil {
		for key, val := range payloadMap {
			propSchema, ok := properties[key].(map[string]any)
			if !ok {
				continue
			}
			expectedType, _ := propSchema["type"].(string)
			if coerced, ok := coercePayloadValue(val, expectedType); ok {
				payloadMap[key] = coerced
				modified = true
			}
		}
	}

	if !modified {
		return nil, nil
	}

	return json.Marshal(payloadMap)
}

// coercePayloadValue attempts to coerce a value to the expected JSON Schema
// type. Returns the coerced value and true if coercion was performed,
// or the original value and false if no coercion was needed or possible.
func coercePayloadValue(val any, expectedType string) (any, bool) {
	switch expectedType {
	case "number", "integer":
		switch v := val.(type) {
		case string:
			if n, err := strconv.ParseFloat(v, 64); err == nil {
				return n, true
			}
		case bool:
			if v {
				return 1.0, true
			}
			return 0.0, true
		}
	case "boolean":
		if v, ok := val.(string); ok {
			lower := strings.ToLower(v)
			if slices.Contains([]string{"true", "1", "yes"}, lower) {
				return true, true
			}
			if slices.Contains([]string{"false", "0", "no", ""}, lower) {
				return false, true
			}
		}
	case "string":
		switch v := val.(type) {
		case float64:
			return fmt.Sprintf("%v", v), true
		case bool:
			return fmt.Sprintf("%v", v), true
		}
	case "array":
		if _, ok := val.([]any); !ok {
			return []any{val}, true
		}
	case "object":
		if _, ok := val.(map[string]any); !ok {
			return map[string]any{"value": val}, true
		}
	}
	return val, false
}
