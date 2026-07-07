package goal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/session"
)

// TokenUsage captures the token counters needed for goal budget accounting.
// It mirrors the fields from fantasy.Usage that contribute to consumed budget.
type TokenUsage struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// TokenUsageFromFantasy converts a fantasy.Usage into a goal.TokenUsage.
func TokenUsageFromFantasy(u fantasy.Usage) TokenUsage {
	return TokenUsage{
		Input:      u.InputTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.CacheReadTokens,
		CacheWrite: u.CacheCreationTokens,
	}
}

// Runtime tracks goal execution state across agent turns, including token
// budget accounting, time tracking, and continuation logic.
type Runtime struct {
	sessions session.Service

	mu        sync.Mutex
	snapshot  turnSnapshot
	wallClock wallClock
}

type turnSnapshot struct {
	turnID       string
	baseline     TokenUsage
	activeGoalID string
}

type wallClock struct {
	activeGoalID    string
	lastAccountedAt int64
}

// NewRuntime creates a new goal runtime.
func NewRuntime(sessions session.Service) *Runtime {
	return &Runtime{
		sessions: sessions,
	}
}

// OnTurnStart records the baseline token usage and active goal ID at the start
// of an agent turn. The baseline is subtracted from the usage passed to
// PostTurn to compute the delta for the turn.
func (r *Runtime) OnTurnStart(ctx context.Context, sessionID, turnID string, baseline TokenUsage) error {
	sess, err := r.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshot = turnSnapshot{
		turnID:       turnID,
		baseline:     baseline,
		activeGoalID: sess.Goal.ID,
	}
	return nil
}

// PostTurn updates goal token usage after an agent turn completes using a
// current usage snapshot. It computes the delta relative to the baseline
// recorded by OnTurnStart and adds it to the goal budget. Cache read tokens
// are excluded from the delta; cache write tokens are included.
//
// It returns the updated goal and whether the budget was exhausted by this
// turn. If the active goal has been replaced since OnTurnStart was called, no
// token accounting is performed.
func (r *Runtime) PostTurn(ctx context.Context, sessionID string, currentUsage TokenUsage) (session.Goal, bool, error) {
	r.mu.Lock()
	snapshot := r.snapshot
	r.snapshot = turnSnapshot{}
	r.mu.Unlock()

	sess, err := r.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.Goal{}, false, fmt.Errorf("failed to get session: %w", err)
	}

	goal := sess.Goal
	if !goal.IsActive() {
		return goal, false, nil
	}

	// Validate that the token usage belongs to the current goal. If the goal
	// was replaced during the turn, skip accounting so tokens are not charged
	// against the new goal.
	if snapshot.turnID != "" && snapshot.activeGoalID != "" && snapshot.activeGoalID != goal.ID {
		return goal, false, nil
	}

	baseline := snapshot.baseline
	delta := max(0, currentUsage.Input-baseline.Input) +
		max(0, currentUsage.Output-baseline.Output) +
		max(0, currentUsage.CacheWrite-baseline.CacheWrite)

	now := time.Now().Unix()
	goal.TokensUsed += delta
	r.settleWallClockToNow(goal.ID, &goal, now)
	goal.UpdatedAt = now

	// Continue timing if the goal is still active.
	if goal.IsActive() {
		r.startWallClock(goal.ID, now)
	}

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

// PostTurnWithIntegers is a backward-compatible wrapper around PostTurn for
// callers that only have prompt and completion token counts. Cache write
// tokens are not available through this path, so they are not included.
func (r *Runtime) PostTurnWithIntegers(ctx context.Context, sessionID string, promptTokens, completionTokens int64) (session.Goal, bool, error) {
	return r.PostTurn(ctx, sessionID, TokenUsage{Input: promptTokens, Output: completionTokens})
}

// CreateGoal creates a new active goal for the session and starts timing it.
func (r *Runtime) CreateGoal(ctx context.Context, sessionID, objective string, budget int64) (session.Goal, error) {
	if objective == "" {
		return session.Goal{}, errors.New("objective is required")
	}

	sess, err := r.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.Goal{}, fmt.Errorf("failed to get session: %w", err)
	}
	if err := sess.ValidateGoalWork(); err != nil {
		return session.Goal{}, err
	}

	goal := sess.Goal
	if goal.IsActive() || goal.Status == session.GoalStatusPaused {
		return session.Goal{}, errors.New("a goal is already active; drop or complete it first")
	}

	now := time.Now().Unix()
	goal = session.Goal{
		ID:        session.NewGoalID(),
		Text:      objective,
		Status:    session.GoalStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if budget > 0 {
		goal.TokenBudget = budget
	}

	r.startWallClock(goal.ID, now)
	sess.Goal = goal
	updated, err := r.sessions.Save(ctx, sess)
	if err != nil {
		return session.Goal{}, fmt.Errorf("failed to save goal: %w", err)
	}
	return updated.Goal, nil
}

// ReplaceGoal updates the current goal's text and budget while preserving
// TokensUsed and TimeSeconds. The old goal's active time is settled before the
// replacement, and timing starts for the new goal.
func (r *Runtime) ReplaceGoal(ctx context.Context, sessionID, objective string, budget int64) (session.Goal, error) {
	if objective == "" {
		return session.Goal{}, errors.New("objective is required")
	}

	sess, err := r.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.Goal{}, fmt.Errorf("failed to get session: %w", err)
	}
	if err := sess.ValidateGoalWork(); err != nil {
		return session.Goal{}, err
	}

	oldGoal := sess.Goal
	now := time.Now().Unix()

	preservedTokens := int64(0)
	preservedTime := int64(0)
	if oldGoal.Status != "" {
		// Settle any active time on the existing goal before replacing it.
		if oldGoal.IsActive() {
			r.settleWallClockToNow(oldGoal.ID, &oldGoal, now)
		}
		preservedTokens = oldGoal.TokensUsed
		preservedTime = oldGoal.TimeSeconds
	}

	goal := session.Goal{
		ID:          session.NewGoalID(),
		Text:        objective,
		Status:      session.GoalStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
		TokensUsed:  preservedTokens,
		TimeSeconds: preservedTime,
	}
	if budget > 0 {
		goal.TokenBudget = budget
	}

	r.startWallClock(goal.ID, now)
	sess.Goal = goal
	updated, err := r.sessions.Save(ctx, sess)
	if err != nil {
		return session.Goal{}, fmt.Errorf("failed to save goal: %w", err)
	}
	return updated.Goal, nil
}

// PauseGoal pauses the active goal and settles its active wall-clock time.
func (r *Runtime) PauseGoal(ctx context.Context, sessionID string) (session.Goal, error) {
	sess, err := r.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.Goal{}, fmt.Errorf("failed to get session: %w", err)
	}

	goal := sess.Goal
	if !goal.IsActive() {
		return session.Goal{}, errors.New("no active goal to pause")
	}

	now := time.Now().Unix()
	r.settleWallClockToNow(goal.ID, &goal, now)
	goal.Status = session.GoalStatusPaused
	goal.UpdatedAt = now

	sess.Goal = goal
	updated, err := r.sessions.Save(ctx, sess)
	if err != nil {
		return session.Goal{}, fmt.Errorf("failed to save goal: %w", err)
	}
	return updated.Goal, nil
}

// ResumeGoal resumes a paused or budget-limited goal and restarts the wall
// clock.
func (r *Runtime) ResumeGoal(ctx context.Context, sessionID string) (session.Goal, error) {
	sess, err := r.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.Goal{}, fmt.Errorf("failed to get session: %w", err)
	}
	if err := sess.ValidateGoalWork(); err != nil {
		return session.Goal{}, err
	}

	goal := sess.Goal
	if goal.Status != session.GoalStatusPaused && goal.Status != session.GoalStatusBudgetLimited {
		return session.Goal{}, errors.New("no paused or budget-limited goal to resume")
	}

	now := time.Now().Unix()
	goal.Status = session.GoalStatusActive
	goal.UpdatedAt = now
	r.startWallClock(goal.ID, now)

	sess.Goal = goal
	updated, err := r.sessions.Save(ctx, sess)
	if err != nil {
		return session.Goal{}, fmt.Errorf("failed to save goal: %w", err)
	}
	return updated.Goal, nil
}

// CompleteGoal marks the active goal as complete and settles its active time.
func (r *Runtime) CompleteGoal(ctx context.Context, sessionID string) (session.Goal, error) {
	sess, err := r.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.Goal{}, fmt.Errorf("failed to get session: %w", err)
	}

	goal := sess.Goal
	if !goal.IsActive() && goal.Status != session.GoalStatusBudgetLimited {
		return session.Goal{}, errors.New("no active goal to complete")
	}

	now := time.Now().Unix()
	r.settleWallClockToNow(goal.ID, &goal, now)
	goal.Status = session.GoalStatusComplete
	goal.UpdatedAt = now

	sess.Goal = goal
	updated, err := r.sessions.Save(ctx, sess)
	if err != nil {
		return session.Goal{}, fmt.Errorf("failed to save goal: %w", err)
	}
	return updated.Goal, nil
}

// DropGoal marks the current goal as dropped and settles any active time
// first. TokensUsed and TimeSeconds are preserved so the session keeps a
// record of the work performed before the goal was discarded.
func (r *Runtime) DropGoal(ctx context.Context, sessionID string) (session.Goal, error) {
	sess, err := r.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.Goal{}, fmt.Errorf("failed to get session: %w", err)
	}

	goal := sess.Goal
	if goal.Status == "" {
		return session.Goal{}, errors.New("no goal to drop")
	}

	now := time.Now().Unix()
	if goal.IsActive() {
		r.settleWallClockToNow(goal.ID, &goal, now)
	}
	r.stopWallClock()

	goal.Status = session.GoalStatusDropped
	goal.UpdatedAt = now
	sess.Goal = goal
	updated, err := r.sessions.Save(ctx, sess)
	if err != nil {
		return session.Goal{}, fmt.Errorf("failed to save goal: %w", err)
	}
	return updated.Goal, nil
}

// SetBudgetGoal updates the token budget for the current goal.
func (r *Runtime) SetBudgetGoal(ctx context.Context, sessionID string, budget int64) (session.Goal, error) {
	sess, err := r.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.Goal{}, fmt.Errorf("failed to get session: %w", err)
	}

	goal := sess.Goal
	if goal.Status == "" {
		return session.Goal{}, errors.New("no goal is currently set")
	}

	wasActive := goal.IsActive()
	goal.TokenBudget = budget
	if goal.IsBudgetExhausted() && goal.IsActive() {
		goal.Status = session.GoalStatusBudgetLimited
	} else if !goal.IsBudgetExhausted() && goal.Status == session.GoalStatusBudgetLimited {
		goal.Status = session.GoalStatusActive
	}

	now := time.Now().Unix()
	goal.UpdatedAt = now

	if wasActive && !goal.IsActive() {
		// Budget exhaustion moved the goal out of active; settle active time.
		r.settleWallClockToNow(goal.ID, &goal, now)
	} else if !wasActive && goal.IsActive() {
		// Budget increase moved the goal back to active; restart the clock.
		r.startWallClock(goal.ID, now)
	}

	sess.Goal = goal
	updated, err := r.sessions.Save(ctx, sess)
	if err != nil {
		return session.Goal{}, fmt.Errorf("failed to save goal: %w", err)
	}
	return updated.Goal, nil
}

// PauseActiveGoalOnLoad pauses an active goal when its session is resumed from
// storage. It settles any pending wall-clock time and returns a user-visible
// notice. If preserve is true, the goal is left untouched so that automatic
// continuation chains are not interrupted.
func (r *Runtime) PauseActiveGoalOnLoad(ctx context.Context, sessionID string, preserve bool) (session.Goal, string, error) {
	if preserve {
		return session.Goal{}, "", nil
	}

	sess, err := r.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.Goal{}, "", fmt.Errorf("failed to get session: %w", err)
	}

	goal := sess.Goal
	if !goal.IsActive() {
		return goal, "", nil
	}

	now := time.Now().Unix()
	r.settleWallClockToNow(goal.ID, &goal, now)
	goal.Status = session.GoalStatusPaused
	goal.UpdatedAt = now

	sess.Goal = goal
	updated, err := r.sessions.Save(ctx, sess)
	if err != nil {
		return goal, "", fmt.Errorf("failed to save goal: %w", err)
	}
	return updated.Goal, "Active goal paused. Use /goal resume to continue.", nil
}

// startWallClock begins tracking active time for goalID from the given
// timestamp.
func (r *Runtime) startWallClock(goalID string, now int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if goalID == "" {
		return
	}
	r.wallClock.activeGoalID = goalID
	r.wallClock.lastAccountedAt = now
}

// stopWallClock stops active time tracking.
func (r *Runtime) stopWallClock() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.wallClock.activeGoalID = ""
	r.wallClock.lastAccountedAt = 0
}

// settleWallClockToNow adds any elapsed active time since the last accounted
// timestamp to goal.TimeSeconds when goalID matches the active wall-clock
// goal. If the runtime has no active wall-clock state for this goal, it falls
// back to the goal's UpdatedAt timestamp so that sessions resumed from storage
// still account for time spent active before the load.
func (r *Runtime) settleWallClockToNow(goalID string, goal *session.Goal, now int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if goalID == "" || !goal.IsActive() {
		return
	}

	var elapsed int64
	if r.wallClock.activeGoalID == goalID && r.wallClock.lastAccountedAt > 0 {
		if now > r.wallClock.lastAccountedAt {
			elapsed = now - r.wallClock.lastAccountedAt
		}
	} else if goal.UpdatedAt > 0 && now > goal.UpdatedAt {
		elapsed = now - goal.UpdatedAt
	}

	if elapsed > 0 {
		goal.TimeSeconds += elapsed
	}
	r.wallClock.activeGoalID = ""
	r.wallClock.lastAccountedAt = 0
}

// accountWallClock is a convenience helper that settles active time for the
// given goal and restarts the clock if the goal is still active.
func (r *Runtime) accountWallClock(goalID string, goal *session.Goal) {
	now := time.Now().Unix()
	r.settleWallClockToNow(goalID, goal, now)
	if goal.IsActive() {
		r.startWallClock(goalID, now)
	}
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
		sb.WriteString(fmt.Sprintf("Active time: %ds\n", goal.TimeSeconds))
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
