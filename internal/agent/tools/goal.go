package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/session"
)

//go:embed goal.md
var goalDescription string

// GoalToolName is the name of the goal tool.
const GoalToolName = "goal"

// GoalParams defines the parameters for the goal tool.
type GoalParams struct {
	Op          string `json:"op" description:"Operation: create, get, complete, pause, resume, drop, budget"`
	Objective   string `json:"objective,omitempty" description:"Goal objective text (required for create)"`
	TokenBudget *int64 `json:"token_budget,omitempty" description:"Token budget (0 for unlimited)"`
}

// GoalResponseMetadata is attached to tool responses for UI rendering.
type GoalResponseMetadata struct {
	Goal            session.Goal `json:"goal"`
	RemainingTokens int64        `json:"remaining_tokens"`
}

// NewGoalTool creates a new goal tool instance.
func NewGoalTool(sessions session.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		GoalToolName,
		string(goalDescription),
		func(ctx context.Context, params GoalParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("goal tool requires a session context")
			}

			currentSession, err := sessions.Get(ctx, sessionID)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to get session: %w", err)
			}

			now := time.Now().Unix()
			goal := currentSession.Goal

			switch params.Op {
			case "create":
				if goal.IsActive() || goal.Status == session.GoalStatusPaused {
					return fantasy.ToolResponse{}, fmt.Errorf("a goal is already active; drop or complete it first")
				}
				if params.Objective == "" {
					return fantasy.ToolResponse{}, fmt.Errorf("objective is required for create")
				}
				goal = session.Goal{
					Text:      params.Objective,
					Status:    session.GoalStatusActive,
					CreatedAt: now,
					UpdatedAt: now,
				}
				if params.TokenBudget != nil && *params.TokenBudget > 0 {
					goal.TokenBudget = *params.TokenBudget
				}

			case "get":
				if goal.Status == "" {
					return fantasy.ToolResponse{Content: "No goal is currently set."}, nil
				}

			case "complete":
				if !goal.IsActive() && goal.Status != session.GoalStatusBudgetLimited {
					return fantasy.ToolResponse{}, fmt.Errorf("no active goal to complete")
				}
				goal.Status = session.GoalStatusComplete
				goal.UpdatedAt = now

			case "pause":
				if !goal.IsActive() {
					return fantasy.ToolResponse{}, fmt.Errorf("no active goal to pause")
				}
				goal.Status = session.GoalStatusPaused
				goal.UpdatedAt = now

			case "resume":
				if goal.Status != session.GoalStatusPaused && goal.Status != session.GoalStatusBudgetLimited {
					return fantasy.ToolResponse{}, fmt.Errorf("no paused or budget-limited goal to resume")
				}
				goal.Status = session.GoalStatusActive
				goal.UpdatedAt = now

			case "drop":
				if goal.Status == "" {
					return fantasy.ToolResponse{}, fmt.Errorf("no goal to drop")
				}
				goal = session.Goal{}

			case "budget":
				if goal.Status == "" {
					return fantasy.ToolResponse{}, fmt.Errorf("no goal is currently set")
				}
				if params.TokenBudget == nil {
					return fantasy.ToolResponse{}, fmt.Errorf("token_budget is required for budget operation")
				}
				goal.TokenBudget = *params.TokenBudget
				if goal.IsBudgetExhausted() && goal.IsActive() {
					goal.Status = session.GoalStatusBudgetLimited
				} else if !goal.IsBudgetExhausted() && goal.Status == session.GoalStatusBudgetLimited {
					goal.Status = session.GoalStatusActive
				}
				goal.UpdatedAt = now

			default:
				return fantasy.ToolResponse{}, fmt.Errorf("unknown operation: %s (expected: create, get, complete, pause, resume, drop, budget)", params.Op)
			}

			currentSession.Goal = goal
			_, err = sessions.Save(ctx, currentSession)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to save goal: %w", err)
			}

			text := formatGoalResponse(goal, params.Op)
			metadata, err := json.Marshal(GoalResponseMetadata{
				Goal:            goal,
				RemainingTokens: goal.RemainingTokens(),
			})
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to encode goal metadata: %w", err)
			}

			return fantasy.ToolResponse{
				Content:  text,
				Metadata: string(metadata),
			}, nil
		},
	)
}

func formatGoalResponse(goal session.Goal, op string) string {
	switch op {
	case "create":
		text := fmt.Sprintf("Goal created: %s", goal.Text)
		if goal.HasBudget() {
			text += fmt.Sprintf("\nToken budget: %d", goal.TokenBudget)
		}
		return text
	case "get":
		text := fmt.Sprintf("Objective: %s\nStatus: %s", goal.Text, goal.Status)
		if goal.HasBudget() {
			text += fmt.Sprintf("\nToken budget: %d (used: %d, remaining: %d)",
				goal.TokenBudget, goal.TokensUsed, goal.RemainingTokens())
		}
		if goal.TimeSeconds > 0 {
			text += fmt.Sprintf("\nTime elapsed: %ds", goal.TimeSeconds)
		}
		return text
	case "complete":
		return fmt.Sprintf("Goal completed: %s", goal.Text)
	case "pause":
		return fmt.Sprintf("Goal paused: %s", goal.Text)
	case "resume":
		return fmt.Sprintf("Goal resumed: %s", goal.Text)
	case "drop":
		if strings.TrimSpace(goal.Text) == "" {
			return "Goal dropped."
		}
		return fmt.Sprintf("Goal dropped: %s", goal.Text)
	case "budget":
		text := fmt.Sprintf("Budget updated: %d", goal.TokenBudget)
		if goal.HasBudget() {
			text += fmt.Sprintf(" (used: %d, remaining: %d)", goal.TokensUsed, goal.RemainingTokens())
		}
		return text
	}
	return ""
}
