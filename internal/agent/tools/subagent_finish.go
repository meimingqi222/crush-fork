package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
)

//go:embed subagent_finish.md
var subagentFinishDescription []byte

const SubagentFinishToolName = "subagent_finish"

type SubagentFinishParams struct {
	Status       string          `json:"status" description:"Terminal subagent status: completed, completed_with_warnings, failed, canceled, or blocked"`
	Summary      string          `json:"summary,omitempty" description:"Human-readable completion summary"`
	Artifacts    []string        `json:"artifacts,omitempty" description:"Referenced output artifacts"`
	FilesTouched []string        `json:"files_touched,omitempty" description:"Workspace-relative or absolute file paths changed by the task"`
	PatchPlan    []string        `json:"patch_plan,omitempty" description:"Applied or proposed change steps"`
	TestResults  []string        `json:"test_results,omitempty" description:"Verification results"`
	Followups    []string        `json:"followups,omitempty" description:"Questions or next tasks for the coordinator"`
	Risks        []string        `json:"risks,omitempty" description:"Risks or caveats found during execution"`
	NextActions  []string        `json:"next_actions,omitempty" description:"Suggested next coordinator actions"`
	Confidence   string          `json:"confidence,omitempty" description:"Qualitative confidence label"`
	Error        string          `json:"error,omitempty" description:"Required for failed and blocked statuses"`
	Data         json.RawMessage `json:"data,omitempty" description:"Optional structured JSON payload"`
}

func NewSubagentFinishTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		SubagentFinishToolName,
		string(subagentFinishDescription),
		func(_ context.Context, params SubagentFinishParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			finish, err := validateSubagentFinishParams(params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			response := fantasy.NewTextResponse(cmpOr(strings.TrimSpace(finish.Summary), terminalStatusSummary(finish)))
			response.Metadata = message.ToolResult{Metadata: response.Metadata}.WithSubagentFinish(finish).Metadata
			return response, nil
		},
	)
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

	if (finish.Status == message.ToolResultSubtaskStatusCompleted || finish.Status == message.ToolResultSubtaskStatusCompletedWithWarnings) && finish.Summary == "" && len(finish.Data) == 0 {
		return message.ToolResultSubagentFinish{}, fmt.Errorf("summary is required for successful completion when data is empty")
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
