package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/session"
)

//go:embed goal.md
var goalDescription string

// GoalToolName is the name of the goal tool.
const GoalToolName = "goal"

// GoalParams defines the parameters for the goal tool.
type GoalParams struct {
	Op          string `json:"op" description:"Operation: create, replace, get, complete, pause, resume, drop, budget"`
	Objective   string `json:"objective,omitempty" description:"Goal objective text (required for create and replace)"`
	TokenBudget *int64 `json:"token_budget,omitempty" description:"Token budget (0 for unlimited, optional for replace)"`
}

// GoalResponseMetadata is attached to tool responses for UI rendering.
type GoalResponseMetadata struct {
	Goal            session.Goal `json:"goal"`
	RemainingTokens int64        `json:"remaining_tokens"`
}

// NewGoalTool creates a new goal tool instance.
func NewGoalTool(sessions session.Service, runtime *goal.Runtime) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		GoalToolName,
		string(goalDescription),
		func(ctx context.Context, params GoalParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if isSubagent := IsSubagentFromContext(ctx); isSubagent {
				return handleSubagentGoal(ctx, sessions, params)
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("goal tool requires a session context")
			}

			var (
				goalResult session.Goal
				err        error
			)

			budget := int64(0)
			if params.TokenBudget != nil {
				budget = *params.TokenBudget
			}

			switch params.Op {
			case "create":
				goalResult, err = runtime.CreateGoal(ctx, sessionID, params.Objective, budget)
			case "replace":
				goalResult, err = runtime.ReplaceGoal(ctx, sessionID, params.Objective, budget)
			case "get":
				currentSession, getErr := sessions.Get(ctx, sessionID)
				if getErr != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("failed to get session: %w", getErr)
				}
				goalResult = currentSession.Goal
				if goalResult.Status == "" {
					return fantasy.ToolResponse{Content: "No goal is currently set."}, nil
				}
			case "complete":
				goalResult, err = runtime.CompleteGoal(ctx, sessionID)
			case "pause":
				goalResult, err = runtime.PauseGoal(ctx, sessionID)
			case "resume":
				goalResult, err = runtime.ResumeGoal(ctx, sessionID)
			case "drop":
				goalResult, err = runtime.DropGoal(ctx, sessionID)
			case "budget":
				if params.TokenBudget == nil {
					return fantasy.ToolResponse{}, fmt.Errorf("token_budget is required for budget operation")
				}
				goalResult, err = runtime.SetBudgetGoal(ctx, sessionID, *params.TokenBudget)
			default:
				return fantasy.ToolResponse{}, fmt.Errorf("unknown operation: %s (expected: create, replace, get, complete, pause, resume, drop, budget)", params.Op)
			}

			if err != nil {
				return fantasy.ToolResponse{}, err
			}

			text := formatGoalResponse(goalResult, params.Op)
			metadata, err := json.Marshal(GoalResponseMetadata{
				Goal:            goalResult,
				RemainingTokens: goalResult.RemainingTokens(),
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

// handleSubagentGoal restricts goal operations from subagents to read-only
// access. Subagents cannot create, replace, complete, pause, resume, drop, or
// budget the parent goal.
func handleSubagentGoal(ctx context.Context, sessions session.Service, params GoalParams) (fantasy.ToolResponse, error) {
	if params.Op != "get" {
		return fantasy.ToolResponse{}, fmt.Errorf("subagent goal tool only supports op=%q; %q is not allowed", "get", params.Op)
	}

	parentSessionID := GetParentSessionIDFromContext(ctx)
	if parentSessionID == "" {
		parentSessionID = GetSessionFromContext(ctx)
	}
	if parentSessionID == "" {
		return fantasy.ToolResponse{}, fmt.Errorf("goal tool in subagent mode requires a parent session context")
	}

	sess, err := sessions.Get(ctx, parentSessionID)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to get parent session: %w", err)
	}

	goalResult := sess.Goal
	if goalResult.Status == "" {
		return fantasy.ToolResponse{Content: "No goal is currently set in the parent session."}, nil
	}

	text := formatGoalResponse(goalResult, "get")
	metadata, err := json.Marshal(GoalResponseMetadata{
		Goal:            goalResult,
		RemainingTokens: goalResult.RemainingTokens(),
	})
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to encode goal metadata: %w", err)
	}
	return fantasy.ToolResponse{Content: text, Metadata: string(metadata)}, nil
}

func formatGoalResponse(goal session.Goal, op string) string {
	switch op {
	case "create":
		text := fmt.Sprintf("Goal created: %s", goal.Text)
		if goal.HasBudget() {
			text += fmt.Sprintf("\nToken budget: %d", goal.TokenBudget)
		}
		return text
	case "replace":
		text := fmt.Sprintf("Goal replaced: %s", goal.Text)
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
			text += fmt.Sprintf("\nActive time: %ds", goal.TimeSeconds)
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
