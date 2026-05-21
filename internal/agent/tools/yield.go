package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
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
	Status  string          `json:"status" description:"Terminal status: completed, completed_with_warnings, failed, canceled, or blocked"`
	Data    string          `json:"data,omitempty" description:"The complete result text to submit to the parent agent. Required unless status is failed or blocked."`
	Error   string          `json:"error,omitempty" description:"The error message. Required if status is failed or blocked."`
	Payload json.RawMessage `json:"payload,omitempty" description:"Optional structured JSON payload. This must conform to the expected schema if OutputSchema is defined."`
}

// YieldOption configures optional behavior for the yield tool.
type YieldOption func(*yieldConfig)

type yieldConfig struct {
	outputSchema any
}

// WithOutputSchema sets the JSON schema used to validate the payload field.
func WithOutputSchema(schema any) YieldOption {
	return func(cfg *yieldConfig) {
		cfg.outputSchema = schema
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

	// Track schema validation attempts per session to allow one retry before
	// force-accepting. This prevents infinite loops when the agent cannot
	// produce conforming output.
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

			sessionID := strings.TrimSpace(GetSessionFromContext(ctx))
			if sessionID != "" && messages != nil {
				if toolAlreadyCalled(ctx, messages, sessionID, func(tr message.ToolResult) bool {
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
			if (status != string(message.ToolResultSubtaskStatusFailed) && status != string(message.ToolResultSubtaskStatusBlocked)) && data == "" && len(params.Payload) == 0 {
				return fantasy.NewTextErrorResponse("data or payload is required for successful statuses"), nil
			}

			// Validate payload against output schema if configured.
			if compiledSchema != nil && len(params.Payload) > 0 {
				validationAttemptsMu.Lock()
				attempts := validationAttempts[sessionID]
				validationAttempts[sessionID] = attempts + 1
				validationAttemptsMu.Unlock()

				var dataValue any
				if unmarshalErr := json.Unmarshal(params.Payload, &dataValue); unmarshalErr != nil {
					// First failure: allow retry.
					if attempts == 0 {
						return fantasy.NewTextErrorResponse(
							fmt.Sprintf("Schema validation error: payload is not valid JSON: %s. Please fix the payload field and retry.", unmarshalErr.Error()),
						), nil
					}
					// Second failure: force-accept to avoid infinite loop.
				} else {
					result := compiledSchema.Validate(dataValue)
					if !result.IsValid() {
						// First failure: return error to allow retry.
						if attempts == 0 {
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
								fmt.Sprintf("Schema validation failed: %s. Please fix the payload field and retry.", errMsg),
							), nil
						}
						// Second failure: force-accept to avoid infinite loop.
					}
				}
			}

			summary := data
			if summary == "" && errVal != "" {
				summary = errVal
			}

			response := fantasy.NewTextResponse(summary)
			response.Metadata = message.ToolResult{Metadata: response.Metadata}.WithYield(message.ToolResultYield{
				Data:    data,
				Status:  status,
				Error:   errVal,
				Payload: params.Payload,
			}).Metadata

			// Signal the agent loop to terminate immediately after this tool call.
			response.StopTurn = true
			return response, nil
		},
	)
}
