package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/kaptinlin/jsonschema"
)

//go:embed subagent_finish.md
var subagentFinishDescription []byte

const SubagentFinishToolName = "subagent_finish"

// SubagentFinishParams is the input schema for the subagent_finish tool.
type SubagentFinishParams struct {
	Status       string          `json:"status" description:"Terminal subagent status: completed, completed_with_warnings, failed, canceled, or blocked"`
	Summary      string          `json:"summary" description:"Human-readable completion summary"`
	Artifacts    []string        `json:"artifacts,omitempty" description:"Referenced output artifacts"`
	FilesTouched []string        `json:"files_touched,omitempty" description:"Workspace-relative or absolute file paths changed by the task"`
	PatchPlan    []string        `json:"patch_plan,omitempty" description:"Applied or proposed change steps"`
	TestResults  []string        `json:"test_results,omitempty" description:"Verification results"`
	Followups    []string        `json:"followups,omitempty" description:"Questions or next tasks for the coordinator"`
	Risks        []string        `json:"risks,omitempty" description:"Risks or caveats found during execution"`
	NextActions  []string        `json:"next_actions,omitempty" description:"Suggested next coordinator actions"`
	Confidence   string          `json:"confidence,omitempty" description:"Qualitative confidence label"`
	Error        string          `json:"error,omitempty" description:"Required for failed and blocked statuses"`
	Data         json.RawMessage `json:"data,omitempty" description:"Optional structured JSON payload validated against OutputSchema if defined"`
}

// SubagentFinishOption configures optional behavior for the subagent_finish tool.
type SubagentFinishOption func(*subagentFinishConfig)

type subagentFinishConfig struct {
	outputSchema any
}

// WithOutputSchema sets the JSON schema used to validate the data field.
func WithOutputSchema(schema any) SubagentFinishOption {
	return func(cfg *subagentFinishConfig) {
		cfg.outputSchema = schema
	}
}

// NewSubagentFinishTool creates the subagent_finish tool. The tool signals
// loop termination (StopTurn) on successful completion.
func NewSubagentFinishTool(messages message.Service, opts ...SubagentFinishOption) fantasy.AgentTool {
	var cfg subagentFinishConfig
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
		SubagentFinishToolName,
		string(subagentFinishDescription),
		func(ctx context.Context, params SubagentFinishParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := strings.TrimSpace(GetSessionFromContext(ctx))
			if sessionID != "" && messages != nil {
				if toolAlreadyCalled(ctx, messages, sessionID, func(tr message.ToolResult) bool {
					_, ok := tr.SubagentFinish()
					return ok
				}) {
					return fantasy.NewTextErrorResponse("subagent_finish has already been called for this session. Do not call it again."), nil
				}
			}

			finish, err := validateSubagentFinishParams(params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			// Validate data against output schema if configured.
			if compiledSchema != nil && len(finish.Data) > 0 {
				validationAttemptsMu.Lock()
				attempts := validationAttempts[sessionID]
				validationAttempts[sessionID] = attempts + 1
				validationAttemptsMu.Unlock()

				var dataValue any
				if unmarshalErr := json.Unmarshal(finish.Data, &dataValue); unmarshalErr != nil {
					// First failure: allow retry.
					if attempts == 0 {
						return fantasy.NewTextErrorResponse(
							fmt.Sprintf("Schema validation error: data is not valid JSON: %s. Please fix the data field and retry.", unmarshalErr.Error()),
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
								errMsg = "data does not conform to the expected output schema"
							}
							return fantasy.NewTextErrorResponse(
								fmt.Sprintf("Schema validation failed: %s. Please fix the data field and retry.", errMsg),
							), nil
						}
						// Second failure: force-accept to avoid infinite loop.
					}
				}
			}

			response := fantasy.NewTextResponse(cmpOr(strings.TrimSpace(finish.Summary), terminalStatusSummary(finish)))
			response.Metadata = message.ToolResult{Metadata: response.Metadata}.WithSubagentFinish(finish).Metadata
			// Signal the agent loop to terminate immediately after this tool call.
			response.StopTurn = true
			return response, nil
		},
	)
}

// toolAlreadyCalled checks whether a prior successful tool invocation exists
// in this session. The checker should inspect the ToolResult metadata to
// confirm the call actually succeeded (e.g., via SubagentFinish() or Yield()).
func toolAlreadyCalled(ctx context.Context, messages message.Service, sessionID string, checker func(message.ToolResult) bool) bool {
	msgs, err := messages.List(ctx, sessionID)
	if err != nil {
		return false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != message.Tool {
			continue
		}
		for _, toolResult := range msgs[i].ToolResults() {
			if checker(toolResult) {
				return true
			}
		}
	}
	return false
}

func validateSubagentFinishParams(params SubagentFinishParams) (message.ToolResultSubagentFinish, error) {
	finish := message.ToolResultSubagentFinish{
		Status:       message.ToolResultSubtaskStatus(strings.TrimSpace(params.Status)),
		Summary:      strings.TrimSpace(params.Summary),
		Artifacts:    dedupeTrimmed(params.Artifacts),
		FilesTouched: dedupeTrimmed(params.FilesTouched),
		PatchPlan:    dedupeTrimmed(params.PatchPlan),
		TestResults:  dedupeTrimmed(params.TestResults),
		Followups:    dedupeTrimmed(params.Followups),
		Risks:        dedupeTrimmed(params.Risks),
		NextActions:  dedupeTrimmed(params.NextActions),
		Confidence:   strings.TrimSpace(params.Confidence),
		Error:        strings.TrimSpace(params.Error),
		Data:         params.Data,
	}

	switch finish.Status {
	case message.ToolResultSubtaskStatusCompleted,
		message.ToolResultSubtaskStatusCompletedWithWarnings,
		message.ToolResultSubtaskStatusFailed,
		message.ToolResultSubtaskStatusCanceled,
		message.ToolResultSubtaskStatusBlocked:
	default:
		return message.ToolResultSubagentFinish{}, fmt.Errorf("status must be one of completed, completed_with_warnings, failed, canceled, or blocked")
	}

	if (finish.Status == message.ToolResultSubtaskStatusFailed || finish.Status == message.ToolResultSubtaskStatusBlocked) && finish.Error == "" {
		return message.ToolResultSubagentFinish{}, fmt.Errorf("error is required for failed and blocked statuses")
	}
	if len(finish.Data) > 0 && !json.Valid(finish.Data) {
		return message.ToolResultSubagentFinish{}, fmt.Errorf("data must be valid JSON")
	}
	return finish, nil
}

func dedupeTrimmed(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func terminalStatusSummary(finish message.ToolResultSubagentFinish) string {
	switch finish.Status {
	case message.ToolResultSubtaskStatusCompleted:
		return "Subagent completed."
	case message.ToolResultSubtaskStatusCompletedWithWarnings:
		return "Subagent completed with warnings."
	case message.ToolResultSubtaskStatusFailed:
		return cmpOr(finish.Error, "Subagent failed.")
	case message.ToolResultSubtaskStatusCanceled:
		return "Subagent canceled."
	case message.ToolResultSubtaskStatusBlocked:
		return cmpOr(finish.Error, "Subagent blocked.")
	default:
		return "Subagent finished."
	}
}

func cmpOr(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
