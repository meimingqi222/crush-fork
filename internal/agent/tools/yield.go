package tools

import (
	"context"
	_ "embed"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
)

//go:embed yield.md
var yieldDescription []byte

const YieldToolName = "yield"

// YieldParams is the input for the yield tool.
type YieldParams struct {
	Data   string `json:"data" description:"The complete result text to submit to the parent agent. Include all findings, analysis, and conclusions."`
	Status string `json:"status" description:"Terminal status: completed, completed_with_warnings, failed, canceled, or blocked"`
}

func NewYieldTool(messages message.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		YieldToolName,
		string(yieldDescription),
		func(ctx context.Context, params YieldParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			data := strings.TrimSpace(params.Data)
			status := strings.TrimSpace(params.Status)
			if data == "" {
				return fantasy.NewTextErrorResponse("data is required"), nil
			}

			sessionID := strings.TrimSpace(GetSessionFromContext(ctx))
			if sessionID != "" && messages != nil {
				if toolAlreadyCalled(ctx, messages, sessionID, func(tr message.ToolResult) bool {
					_, ok := tr.Yield()
					return ok
				}) {
					return fantasy.NewTextErrorResponse("yield has already been called for this session. Do not call it again."), nil
				}
			}

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

			response := fantasy.NewTextResponse("Result submitted.")
			response.Metadata = message.ToolResult{Metadata: response.Metadata}.WithYield(message.ToolResultYield{Data: data, Status: status}).Metadata
			return response, nil
		},
	)
}
