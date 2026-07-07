package agent

import (
	"testing"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestAssistantCalledPlanRequiredTool(t *testing.T) {
	t.Parallel()

	msgs := []message.Message{{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{Name: tools.ResolveToolName},
			message.Finish{Reason: message.FinishReasonEndTurn},
		},
	}}
	require.True(t, assistantCalledPlanRequiredTool(msgs))

	msgs[0].Parts = []message.ContentPart{
		message.TextContent{Text: "Here is the plan"},
		message.Finish{Reason: message.FinishReasonEndTurn},
	}
	require.False(t, assistantCalledPlanRequiredTool(msgs))
}

func TestIsPlanModeEnforcementPrompt(t *testing.T) {
	t.Parallel()

	require.True(t, isPlanModeEnforcementPrompt(planModeToolDecisionReminder))
	require.False(t, isPlanModeEnforcementPrompt("Continue work on the active goal."))
}
