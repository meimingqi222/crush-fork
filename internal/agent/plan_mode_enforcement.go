package agent

import (
	"context"
	"strings"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

type planModeEnforcementDepthKey struct{}

const planModeToolDecisionReminder = `Plan mode turn ended without a required tool call.

You MUST choose exactly one next action now:
1. Call ` + "`request_user_input`" + ` to gather required clarification, OR
2. Call ` + "`resolve`" + ` with ` + "`action: \"apply\"`" + `, ` + "`reason`" + `, and ` + "`extra: { title: \"<slug>\" }`" + ` (the slug of your plan file) to finish planning and request approval

You NEVER output plain text in this turn.`

func isPlanModeEnforcementPrompt(prompt string) bool {
	return strings.TrimSpace(prompt) == strings.TrimSpace(planModeToolDecisionReminder)
}

func assistantCalledPlanRequiredTool(msgs []message.Message) bool {
	assistant := lastAssistantMessage(msgs)
	if assistant == nil {
		return false
	}
	for _, part := range assistant.Parts {
		call, ok := part.(message.ToolCall)
		if !ok {
			continue
		}
		switch call.Name {
		case tools.RequestUserInputToolName, tools.ResolveToolName:
			return true
		}
	}
	return false
}

func lastAssistantMessage(msgs []message.Message) *message.Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.Assistant {
			return &msgs[i]
		}
	}
	return nil
}

func assistantTurnFailed(msgs []message.Message) bool {
	assistant := lastAssistantMessage(msgs)
	if assistant == nil {
		return true
	}
	for _, part := range assistant.Parts {
		finish, ok := part.(message.Finish)
		if !ok {
			continue
		}
		switch finish.Reason {
		case message.FinishReasonError, message.FinishReasonCanceled:
			return true
		}
	}
	return false
}

func (c *coordinator) maybeEnforcePlanModeToolDecision(ctx context.Context, sessionID, prompt string, sess session.Session, guidedGoalSetup bool) error {
	if !sess.IsActivePlanMode() {
		return nil
	}
	if isPlanModeEnforcementPrompt(prompt) || guidedGoalSetup {
		return nil
	}
	depth, _ := ctx.Value(planModeEnforcementDepthKey{}).(int)
	if depth > 0 {
		return nil
	}

	msgs, err := c.messages.List(ctx, sessionID)
	if err != nil {
		return err
	}
	if assistantTurnFailed(msgs) || assistantCalledPlanRequiredTool(msgs) {
		return nil
	}

	enforceCtx := context.WithValue(ctx, planModeEnforcementDepthKey{}, depth+1)
	_, err = c.Run(enforceCtx, sessionID, planModeToolDecisionReminder)
	return err
}
