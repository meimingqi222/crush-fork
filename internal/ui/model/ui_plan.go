package model

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/planmode"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/util"
)

func (m *UI) maybeOpenProposedPlanDialog(msg message.Message) tea.Cmd {
	if m.session == nil || m.session.CollaborationMode != session.CollaborationModePlan {
		return nil
	}
	if msg.FinishPart() == nil {
		return nil
	}
	reason := msg.FinishPart().Reason
	if reason != message.FinishReasonEndTurn && reason != message.FinishReasonToolUse {
		return nil
	}
	title, hasPlanTool := hasResolveApply(msg)
	if !hasPlanTool {
		return nil
	}
	if m.lastPromptedPlanMsg == msg.ID {
		return nil
	}
	m.lastPromptedPlanMsg = msg.ID
	planFilePath := strings.TrimSpace(m.session.PlanFilePath)
	if planFilePath == "" {
		return util.ReportWarn("Plan file path is missing; cannot review the proposed plan.")
	}
	return m.loadPlanReview(msg.SessionID, planFilePath, title)
}

func (m *UI) loadPlanReview(sessionID, planFilePath, title string) tea.Cmd {
	return func() tea.Msg {
		data, err := os.ReadFile(planFilePath)
		if err != nil {
			return planReviewLoadedMsg{SessionID: sessionID, Err: err}
		}
		return planReviewLoadedMsg{SessionID: sessionID, Plan: string(data), Title: title}
	}
}

type resolveToolInput struct {
	Action string `json:"action"`
	Extra  struct {
		Title string `json:"title"`
	} `json:"extra"`
}

// findResolveApplyToolCall returns the plan title from a resolve tool call
// whose action is "apply".
func findResolveApplyToolCall(msg message.Message) (string, bool) {
	for _, tc := range msg.ToolCalls() {
		if tc.Name != agenttools.ResolveToolName {
			continue
		}
		var input resolveToolInput
		if err := json.Unmarshal([]byte(tc.Input), &input); err != nil {
			continue
		}
		if input.Action != "apply" {
			continue
		}
		return input.Extra.Title, true
	}
	return "", false
}

// hasResolveApply reports whether the message contains a resolve(action="apply")
// tool call. It returns an optional title when resolve provided one.
func hasResolveApply(msg message.Message) (string, bool) {
	return findResolveApplyToolCall(msg)
}

func (m *UI) executeApprovedPlan(sessionID, plan string, mode planmode.ExecutionContextMode) tea.Cmd {
	if mode == planmode.ExecuteWithCompact {
		return func() tea.Msg {
			if err := m.com.App.AgentCoordinator.Summarize(context.Background(), sessionID, nil); err != nil {
				return planCompactedForExecutionMsg{SessionID: sessionID, Plan: plan, Err: err}
			}
			return planCompactedForExecutionMsg{SessionID: sessionID, Plan: plan}
		}
	}
	return tea.Sequence(
		func() tea.Msg {
			_, err := m.com.App.Sessions.UpdateCollaborationMode(context.Background(), sessionID, session.CollaborationModeDefault)
			if err != nil {
				return util.ReportError(err)()
			}
			if m.session != nil && m.session.ID == sessionID {
				m.session.CollaborationMode = session.CollaborationModeDefault
			}
			return planModeChangedMsg{SessionID: sessionID, Status: "Plan approved. Starting implementation.", Mode: session.CollaborationModeDefault}
		},
		m.runAgentMessage(planmode.BuildExecutionPrompt(plan, mode)),
	)
}
