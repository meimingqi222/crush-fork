package goal

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/session"
)

// Runtime tracks goal execution state across agent turns, including token
// budget accounting, time tracking, and continuation logic.
type Runtime struct {
	sessions session.Service
}

// NewRuntime creates a new goal runtime.
func NewRuntime(sessions session.Service) *Runtime {
	return &Runtime{
		sessions: sessions,
	}
}

// PostTurn updates goal token usage after an agent turn completes. The token
// values are the usage for the completed run, not cumulative session totals.
// Returns the updated goal and whether the budget was exhausted by this turn.
func (r *Runtime) PostTurn(ctx context.Context, sessionID string, promptTokens, completionTokens int64) (session.Goal, bool, error) {
	sess, err := r.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.Goal{}, false, fmt.Errorf("failed to get session: %w", err)
	}

	goal := sess.Goal
	if !goal.IsActive() {
		return goal, false, nil
	}

	now := time.Now().Unix()
	delta := max(0, promptTokens) + max(0, completionTokens)
	goal.TokensUsed += delta
	if goal.CreatedAt > 0 {
		goal.TimeSeconds = max(0, now-goal.CreatedAt)
	}
	goal.UpdatedAt = now

	// Check budget exhaustion.
	budgetExhausted := false
	if goal.IsBudgetExhausted() && goal.Status != session.GoalStatusBudgetLimited {
		goal.Status = session.GoalStatusBudgetLimited
		budgetExhausted = true
	}

	sess.Goal = goal
	_, err = r.sessions.Save(ctx, sess)
	if err != nil {
		return goal, false, fmt.Errorf("failed to save goal state: %w", err)
	}

	return goal, budgetExhausted, nil
}

// NeedsContinuation returns true if the goal is still active and needs
// another turn of autonomous work.
func NeedsContinuation(goal session.Goal) bool {
	return goal.Status == session.GoalStatusActive
}

// IsSteerPrompt reports whether prompt is an internal goal continuation steer.
func IsSteerPrompt(prompt string) bool {
	prompt = strings.TrimSpace(prompt)
	return strings.HasPrefix(prompt, "Continue work on the active goal.") ||
		strings.HasPrefix(prompt, "Token budget exhausted for the active goal.")
}

// ShouldChainContinuation reports whether another autonomous goal turn should
// run after the current one completes. User-initiated prompts do not chain;
// only internal steers and guided-goal setup kick off autonomous continuation.
func ShouldChainContinuation(prompt string, depth int) bool {
	if IsSteerPrompt(prompt) {
		return true
	}
	return depth == 0 && strings.Contains(prompt, "<guided_goal>")
}

// BuildContinuationPrompt generates a hidden steer message injected between
// turns to prompt the agent to continue working on the goal.
func BuildContinuationPrompt(goal session.Goal) string {
	var sb strings.Builder
	sb.WriteString("Continue work on the active goal.\n\n")
	sb.WriteString(fmt.Sprintf("Objective: %s\n", goal.Text))
	if goal.HasBudget() {
		sb.WriteString(fmt.Sprintf("Token budget: %d used / %d total (%d remaining)\n",
			goal.TokensUsed, goal.TokenBudget, goal.RemainingTokens()))
	}
	if goal.TimeSeconds > 0 {
		sb.WriteString(fmt.Sprintf("Time elapsed: %ds\n", goal.TimeSeconds))
	}
	sb.WriteString("\n")
	sb.WriteString("Completion audit protocol:\n")
	sb.WriteString("1. Restate the objective as concrete deliverables.\n")
	sb.WriteString("2. Map each deliverable to evidence.\n")
	sb.WriteString("3. Inspect actual current state (read files, run commands, check tests).\n")
	sb.WriteString("4. Match verification scope to claim scope.\n")
	sb.WriteString("5. Treat uncertainty as not-yet-achieved.\n")
	sb.WriteString("6. Budget exhaustion is not completion.\n")
	sb.WriteString("\n")
	sb.WriteString("If work is not done, keep working. Never call goal({op:\"complete\"}) just because the budget is low.\n")
	return sb.String()
}

// BuildBudgetLimitPrompt generates a steer message injected when the goal
// hits its token budget.
func BuildBudgetLimitPrompt(goal session.Goal) string {
	var sb strings.Builder
	sb.WriteString("Token budget exhausted for the active goal.\n\n")
	sb.WriteString(fmt.Sprintf("Objective: %s\n", goal.Text))
	sb.WriteString(fmt.Sprintf("Tokens used: %d / %d\n\n", goal.TokensUsed, goal.TokenBudget))
	sb.WriteString("Stop new substantive work. Summarize progress:\n")
	sb.WriteString("- What has been completed.\n")
	sb.WriteString("- What remains.\n")
	sb.WriteString("- Any blockers.\n\n")
	sb.WriteString("Do NOT call goal({op:\"complete\"}) unless the objective is genuinely met.\n")
	return sb.String()
}
