package model

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/plan"
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
				return planCompactedForExecutionMsg{SessionID: sessionID, Plan: plan, Mode: planmode.ExecuteDirect, Err: err}
			}
			return planCompactedForExecutionMsg{SessionID: sessionID, Plan: plan, Mode: planmode.ExecuteDirect}
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

			// Create goal and initialize todo list from the plan so the agent does
			// not need to do it manually before starting work.
			if err := m.createGoalAndTasksFromPlan(context.Background(), sessionID, plan); err != nil {
				return util.ReportError(err)()
			}

			return planModeChangedMsg{SessionID: sessionID, Status: "Plan approved. Starting implementation.", Mode: session.CollaborationModeDefault}
		},
		m.runAgentMessage(planmode.BuildExecutionPrompt(plan, mode)),
	)
}

// createGoalAndTasksFromPlan parses the approved plan and creates the active
// goal and an initial todo list. It fails open: if parsing produces no tasks,
// the goal is still created from the plan title so the agent can initialize
// tasks manually.
func (m *UI) createGoalAndTasksFromPlan(ctx context.Context, sessionID, planText string) error {
	objective, tasks, _ := plan.ExtractGoalAndTasks(planText)
	if objective == "" {
		// Fall back to the first non-empty line as the goal objective.
		objective = strings.TrimSpace(planText)
		if idx := strings.Index(objective, "\n"); idx >= 0 {
			objective = strings.TrimSpace(objective[:idx])
		}
		if objective == "" {
			return nil
		}
	}

	goal, err := m.com.App.GoalRuntime.CreateGoal(ctx, sessionID, objective, 0)
	if err != nil {
		return fmt.Errorf("failed to create goal from plan: %w", err)
	}

	if len(tasks) == 0 {
		return nil
	}

	// Seed the goal's task list directly; this matches todo init semantics
	// without going through a tool round-trip.
	sess, err := m.com.App.Sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	if sess.Goal.ID != goal.ID {
		// The goal was replaced or the session is out of sync; do not overwrite.
		return nil
	}

	now := time.Now().Unix()
	goalTasks := make([]session.Task, 0, len(tasks))
	for _, t := range tasks {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		goalTasks = append(goalTasks, session.Task{
			ID:        t,
			Content:   t,
			Status:    session.TaskStatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		})
	}
	if len(goalTasks) == 0 {
		return nil
	}

	sess.Goal.Tasks = goalTasks
	sess.Goal.UpdatedAt = now
	if _, err := m.com.App.Sessions.Save(ctx, sess); err != nil {
		return fmt.Errorf("failed to save goal tasks: %w", err)
	}
	return nil
}
