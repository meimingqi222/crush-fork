package goal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/session"
)

const (
	defaultMaxIterations int64 = 50
	defaultBlockCap      int64 = 8
)

// countCompletedTasks returns the number of completed tasks in a task list.
func countCompletedTasks(tasks []session.Task) int {
	count := 0
	for _, t := range tasks {
		if t.Status == session.TaskStatusCompleted {
			count++
		}
	}
	return count
}

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
// budget accounting, time tracking, and continuation logic. All in-memory
// state is keyed by session ID so that concurrent sessions with active goals
// do not interfere with each other's baseline or wall-clock accounting.
// SubagentTaskUpdate describes a task status change submitted by a subagent.
// The parent agent applies queued updates at the start of its next turn.
type SubagentTaskUpdate struct {
	TaskContent string
	NewStatus   session.TaskStatus
	Evidence    string
	Reason      string
	ToolCallID  string
	Timestamp   int64
}

type Runtime struct {
	sessions session.Service

	mu              sync.Mutex
	snapshots       map[string]turnSnapshot
	wallClocks      map[string]wallClock
	pendingSubagent map[string][]SubagentTaskUpdate

	// config overrides the hardcoded defaults for MaxIterations and BlockCap
	// when creating or replacing goals. If nil, the package-level defaults
	// (50 / 8) are used.
	config *RuntimeConfig
}

// RuntimeConfig holds configurable goal runtime parameters.
type RuntimeConfig struct {
	MaxIterations int64
	BlockCap      int64
	TaskGate      string // "strict", "lax", "off"; empty defaults to "strict"
}

// SetConfig sets the runtime configuration. Safe to call before the first
// goal is created; concurrent calls while goals are active are not supported.
func (r *Runtime) SetConfig(cfg RuntimeConfig) {
	r.config = &cfg
}

func (r *Runtime) effectiveMaxIterations() int64 {
	if r.config != nil && r.config.MaxIterations > 0 {
		return r.config.MaxIterations
	}
	return defaultMaxIterations
}

func (r *Runtime) effectiveBlockCap() int64 {
	if r.config != nil && r.config.BlockCap > 0 {
		return r.config.BlockCap
	}
	return defaultBlockCap
}

func (r *Runtime) effectiveTaskGate() string {
	if r.config != nil && r.config.TaskGate != "" {
		return r.config.TaskGate
	}
	return "strict"
}

type turnSnapshot struct {
	turnID       string
	baseline     TokenUsage
	activeGoalID string
	tasks        []session.Task
	noProgress   int64
}

type wallClock struct {
	activeGoalID    string
	lastAccountedAt int64
}

// NewRuntime creates a new goal runtime.
func NewRuntime(sessions session.Service) *Runtime {
	return &Runtime{
		sessions:        sessions,
		snapshots:       make(map[string]turnSnapshot),
		wallClocks:      make(map[string]wallClock),
		pendingSubagent: make(map[string][]SubagentTaskUpdate),
	}
}

// DeleteSession removes the in-memory goal state for a deleted session.
func (r *Runtime) DeleteSession(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.snapshots, sessionID)
	delete(r.wallClocks, sessionID)
}

// ApplySubagentTaskUpdate queues a task status update submitted by a subagent.
// The update is not immediately persisted; it is applied to the parent session
// at the start of the parent agent's next turn.
func (r *Runtime) ApplySubagentTaskUpdate(parentSessionID string, update SubagentTaskUpdate) error {
	if parentSessionID == "" {
		return errors.New("parent session ID is required for subagent task update")
	}
	if update.TaskContent == "" {
		return errors.New("task content is required for subagent task update")
	}
	if update.NewStatus == "" {
		return errors.New("new status is required for subagent task update")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.pendingSubagent[parentSessionID] = append(r.pendingSubagent[parentSessionID], update)
	return nil
}

// ApplyPendingSubagentUpdates applies all queued subagent task updates for the
// given parent session. It is safe to call concurrently with
// ApplySubagentTaskUpdate because queued updates are copied under the lock
// before the session is modified.
func (r *Runtime) ApplyPendingSubagentUpdates(ctx context.Context, parentSessionID string) error {
	r.mu.Lock()
	pending := r.pendingSubagent[parentSessionID]
	delete(r.pendingSubagent, parentSessionID)
	r.mu.Unlock()

	if len(pending) == 0 {
		return nil
	}

	sess, err := r.sessions.Get(ctx, parentSessionID)
	if err != nil {
		return fmt.Errorf("failed to get parent session: %w", err)
	}
	if !sess.Goal.IsActive() {
		return nil
	}

	now := time.Now().Unix()
	goal := sess.Goal
	tasks := goal.Tasks
	if tasks == nil {
		tasks = []session.Task{}
	}

	changed := false
	for _, update := range pending {
		idx := -1
		for i, t := range tasks {
			if strings.EqualFold(t.Content, update.TaskContent) || strings.EqualFold(t.ID, update.TaskContent) {
				idx = i
				break
			}
		}
		if idx < 0 {
			// Subagents cannot create new tasks; only init/add (parent-only)
			// may add tasks. Silently skip the unknown update so the parent
			// agent sees a warning in its next todo view rather than a
			// corrupted task list.
			slog.Warn("Subagent task update references unknown task; skipping", "task_content", update.TaskContent, "parent_session_id", parentSessionID)
			continue
		}

		old := tasks[idx].Status
		if old == session.TaskStatusCompleted && update.NewStatus == session.TaskStatusCompleted {
			// Idempotent completion: only backfill evidence if missing.
			if tasks[idx].Evidence == "" && update.Evidence != "" {
				tasks[idx].Evidence = update.Evidence
				tasks[idx].UpdatedAt = now
				changed = true
			}
			continue
		}

		tasks[idx].Status = update.NewStatus
		tasks[idx].UpdatedAt = now
		if update.NewStatus == session.TaskStatusCompleted {
			tasks[idx].CompletedAt = now
			tasks[idx].Evidence = update.Evidence
			goal.NoProgress = 0
		} else if update.NewStatus == session.TaskStatusInProgress {
			goal.NoProgress = 0
		} else if update.NewStatus == session.TaskStatusDropped {
			if old != session.TaskStatusDropped {
				goal.NoProgress++
				tasks[idx].DropReason = update.Reason
			}
		}
		changed = true
	}

	if !changed {
		return nil
	}

	goal.Tasks = session.NormalizeTasksForStorage(tasks)
	goal.UpdatedAt = now
	sess.Goal = goal
	if _, err := r.sessions.Save(ctx, sess); err != nil {
		return fmt.Errorf("failed to save parent session after subagent updates: %w", err)
	}
	return nil
}

// OnTurnStart records the baseline token usage and active goal ID at the start
// of an agent turn. The baseline is subtracted from the usage passed to
// PostTurn to compute the delta for the turn.
func (r *Runtime) OnTurnStart(ctx context.Context, sessionID, turnID string, baseline TokenUsage) error {
	if err := r.ApplyPendingSubagentUpdates(ctx, sessionID); err != nil {
		return err
	}

	sess, err := r.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshots[sessionID] = turnSnapshot{
		turnID:       turnID,
		baseline:     baseline,
		activeGoalID: sess.Goal.ID,
		tasks:        slices.Clone(sess.Goal.Tasks),
		noProgress:   sess.Goal.NoProgress,
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
	snapshot := r.snapshots[sessionID]
	delete(r.snapshots, sessionID)
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
	r.settleWallClockToNow(sessionID, goal.ID, &goal, now)
	goal.Iterations++
	goal.UpdatedAt = now

	// Evaluate progress relative to the turn start. A task moving to completed
	// counts as progress and resets the stall counter. If the counter was not
	// touched by any tool this turn and no progress is detected, increment it.
	completedBefore := countCompletedTasks(snapshot.tasks)
	completedNow := countCompletedTasks(goal.Tasks)
	if completedNow > completedBefore {
		goal.NoProgress = 0
	} else if goal.NoProgress == snapshot.noProgress {
		goal.NoProgress++
	}

	// Check termination conditions.
	budgetExhausted := false
	if goal.IsBudgetExhausted() && goal.Status != session.GoalStatusBudgetLimited {
		goal.Status = session.GoalStatusBudgetLimited
		budgetExhausted = true
	}
	if goal.Iterations >= goal.MaxIterations {
		TerminateGoal(&goal, session.GoalStatusDropped, fmt.Sprintf("Reached maximum iterations (%d)", goal.MaxIterations))
	}
	if goal.NoProgress >= goal.BlockCap {
		TerminateGoal(&goal, session.GoalStatusStalled, fmt.Sprintf("No progress for %d consecutive turns", goal.NoProgress))
	}

	// Continue timing if the goal is still active.
	if goal.IsActive() {
		r.startWallClock(sessionID, goal.ID, now)
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
		ID:            session.NewGoalID(),
		Text:          objective,
		Status:        session.GoalStatusActive,
		MaxIterations: r.effectiveMaxIterations(),
		BlockCap:      r.effectiveBlockCap(),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if budget > 0 {
		goal.TokenBudget = budget
	}

	r.startWallClock(sessionID, goal.ID, now)
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
			r.settleWallClockToNow(sessionID, oldGoal.ID, &oldGoal, now)
		}
		preservedTokens = oldGoal.TokensUsed
		preservedTime = oldGoal.TimeSeconds
	}

	goal := session.Goal{
		ID:            session.NewGoalID(),
		Text:          objective,
		Status:        session.GoalStatusActive,
		CreatedAt:     now,
		UpdatedAt:     now,
		TokensUsed:    preservedTokens,
		TimeSeconds:   preservedTime,
		MaxIterations: r.effectiveMaxIterations(),
		BlockCap:      r.effectiveBlockCap(),
	}
	if budget > 0 {
		goal.TokenBudget = budget
	}

	r.startWallClock(sessionID, goal.ID, now)
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
	r.settleWallClockToNow(sessionID, goal.ID, &goal, now)
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
	r.startWallClock(sessionID, goal.ID, now)

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

	if err := CanCompleteGoalWithGate(goal, r.effectiveTaskGate()); err != nil {
		return session.Goal{}, err
	}

	now := time.Now().Unix()
	r.settleWallClockToNow(sessionID, goal.ID, &goal, now)
	goal.Status = session.GoalStatusComplete
	goal.UpdatedAt = now

	sess.Goal = goal
	updated, err := r.sessions.Save(ctx, sess)
	if err != nil {
		return session.Goal{}, fmt.Errorf("failed to save goal: %w", err)
	}
	return updated.Goal, nil
}

// AddGoalTask appends a single task to the active goal's task list. It is
// used by the /goal add slash command. Duplicate content (case-insensitive)
// is rejected.
func (r *Runtime) AddGoalTask(ctx context.Context, sessionID, content string) (session.Goal, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return session.Goal{}, errors.New("task content is required")
	}
	sess, err := r.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.Goal{}, fmt.Errorf("failed to get session: %w", err)
	}
	if !sess.Goal.IsActive() {
		return session.Goal{}, errors.New("no active goal to add a task to")
	}
	for _, t := range sess.Goal.Tasks {
		if strings.EqualFold(t.Content, content) {
			return session.Goal{}, fmt.Errorf("duplicate task %q", content)
		}
	}
	now := time.Now().Unix()
	sess.Goal.Tasks = append(sess.Goal.Tasks, session.Task{
		ID:        content,
		Content:   content,
		Status:    session.TaskStatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	})
	sess.Goal.UpdatedAt = now
	updated, err := r.sessions.Save(ctx, sess)
	if err != nil {
		return session.Goal{}, fmt.Errorf("failed to save goal task: %w", err)
	}
	return updated.Goal, nil
}

// CompleteGoalTask marks a task as completed by content or index. It is used
// by the /goal done slash command. Evidence is optional but required by the
// strict task gate at goal completion time.
func (r *Runtime) CompleteGoalTask(ctx context.Context, sessionID, contentOrIndex, evidence string) (session.Goal, error) {
	sess, err := r.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.Goal{}, fmt.Errorf("failed to get session: %w", err)
	}
	if !sess.Goal.IsActive() {
		return session.Goal{}, errors.New("no active goal")
	}
	tasks := sess.Goal.Tasks
	idx := -1
	if contentOrIndex != "" {
		for i, t := range tasks {
			if strings.EqualFold(t.Content, contentOrIndex) || strings.EqualFold(t.ID, contentOrIndex) {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		return session.Goal{}, fmt.Errorf("no task found matching %q", contentOrIndex)
	}
	now := time.Now().Unix()
	wasCompleted := tasks[idx].Status == session.TaskStatusCompleted
	tasks[idx].Status = session.TaskStatusCompleted
	if !wasCompleted {
		tasks[idx].CompletedAt = now
		sess.Goal.NoProgress = 0
	}
	tasks[idx].UpdatedAt = now
	if evidence = strings.TrimSpace(evidence); evidence != "" {
		tasks[idx].Evidence = evidence
	}
	sess.Goal.UpdatedAt = now
	updated, err := r.sessions.Save(ctx, sess)
	if err != nil {
		return session.Goal{}, fmt.Errorf("failed to save goal task: %w", err)
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
		r.settleWallClockToNow(sessionID, goal.ID, &goal, now)
	}
	r.stopWallClock(sessionID)

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
		r.settleWallClockToNow(sessionID, goal.ID, &goal, now)
	} else if !wasActive && goal.IsActive() {
		// Budget increase moved the goal back to active; restart the clock.
		r.startWallClock(sessionID, goal.ID, now)
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
	r.settleWallClockToNow(sessionID, goal.ID, &goal, now)
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
// timestamp, keyed by sessionID so concurrent sessions keep independent clocks.
func (r *Runtime) startWallClock(sessionID, goalID string, now int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if goalID == "" {
		return
	}
	r.wallClocks[sessionID] = wallClock{
		activeGoalID:    goalID,
		lastAccountedAt: now,
	}
}

// stopWallClock stops active time tracking for the given session.
func (r *Runtime) stopWallClock(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.wallClocks, sessionID)
}

// settleWallClockToNow adds any elapsed active time since the last accounted
// timestamp to goal.TimeSeconds when goalID matches the active wall-clock
// goal for sessionID. If the runtime has no active wall-clock state for this
// session/goal, it falls back to the goal's UpdatedAt timestamp so that
// sessions resumed from storage still account for time spent active before
// the load.
func (r *Runtime) settleWallClockToNow(sessionID, goalID string, goal *session.Goal, now int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if goalID == "" || !goal.IsActive() {
		return
	}

	wc := r.wallClocks[sessionID]
	var elapsed int64
	if wc.activeGoalID == goalID && wc.lastAccountedAt > 0 {
		if now > wc.lastAccountedAt {
			elapsed = now - wc.lastAccountedAt
		}
	} else if goal.UpdatedAt > 0 && now > goal.UpdatedAt {
		elapsed = now - goal.UpdatedAt
	}

	if elapsed > 0 {
		goal.TimeSeconds += elapsed
	}
	delete(r.wallClocks, sessionID)
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

// IsGuidedGoalPrompt reports whether prompt was produced by the guided-goal
// dialog. The coordinator calls this once at entry to set the
// GuidedGoalSetup flag on SessionAgentCall; downstream code (continuation
// chaining, plan-mode enforcement) checks that flag instead of re-scanning
// the user-controllable prompt text.
func IsGuidedGoalPrompt(prompt string) bool {
	return strings.HasPrefix(strings.TrimSpace(prompt), "<guided_goal>")
}

// ShouldChainContinuation reports whether another autonomous goal turn should
// run after the current one completes. User-initiated prompts do not chain;
// only internal steers and guided-goal setup kick off autonomous continuation.
// The guidedGoalSetup flag is set by the coordinator from IsGuidedGoalPrompt,
// decoupling continuation triggering from substring matching on
// user-controllable prompt text.
func ShouldChainContinuation(prompt string, depth int, guidedGoalSetup bool) bool {
	if IsSteerPrompt(prompt) {
		return true
	}
	return depth == 0 && guidedGoalSetup
}

// CanCompleteGoal returns an error if the goal is not eligible for completion.
// It enforces the strict task gate: every completed task must have evidence,
// every dropped task must have a reason, and dropped tasks cannot outnumber
// completed tasks.
func CanCompleteGoal(g session.Goal) error {
	return CanCompleteGoalWithGate(g, "strict")
}

// CanCompleteGoalWithGate applies the task completion gate at the given strictness:
//   - "strict": enforce evidence, drop reason, and drop ratio (default).
//   - "lax": only check that no task is pending/in_progress/blocked; evidence
//     and drop ratio are not enforced.
//   - "off": no checks at all.
func CanCompleteGoalWithGate(g session.Goal, gate string) error {
	switch gate {
	case "", "off":
		return nil
	case "lax":
		if len(g.Tasks) == 0 {
			return nil
		}
		for i, t := range g.Tasks {
			if t.Status != session.TaskStatusCompleted && t.Status != session.TaskStatusDropped {
				return fmt.Errorf("cannot complete goal: task %d %q is still %s", i, t.Content, t.Status)
			}
		}
		if g.LastEvaluatorMet != nil && !*g.LastEvaluatorMet {
			return fmt.Errorf("cannot complete goal: evaluator verdict is not met (reason: %s)", g.LastReason)
		}
		return nil
	default: // "strict"
	}

	if len(g.Tasks) == 0 {
		return nil
	}

	dropped := 0
	completed := 0
	for i, t := range g.Tasks {
		if t.Status != session.TaskStatusCompleted && t.Status != session.TaskStatusDropped {
			return fmt.Errorf("cannot complete goal: task %d %q is still %s", i, t.Content, t.Status)
		}
		if t.Status == session.TaskStatusDropped {
			dropped++
			if strings.TrimSpace(t.DropReason) == "" {
				return fmt.Errorf("cannot complete goal: dropped task %d %q is missing a drop reason", i, t.Content)
			}
		} else {
			completed++
			if strings.TrimSpace(t.Evidence) == "" {
				return fmt.Errorf("cannot complete goal: completed task %d %q is missing evidence", i, t.Content)
			}
		}
	}

	if len(g.Tasks) > 0 && dropped > len(g.Tasks)/2 {
		return fmt.Errorf("cannot complete goal: too many dropped tasks (%d/%d); add and complete new tasks or drop the goal", dropped, len(g.Tasks))
	}

	if g.LastEvaluatorMet != nil && !*g.LastEvaluatorMet {
		return fmt.Errorf("cannot complete goal: evaluator verdict is not met (reason: %s)", g.LastReason)
	}

	return nil
}

// TerminateGoal marks the goal with a terminal status and records the reason.
func TerminateGoal(g *session.Goal, status session.GoalStatus, reason string) {
	g.Status = status
	g.LastReason = reason
	g.UpdatedAt = time.Now().Unix()
}

// BuildTaskReminder generates a short system reminder injected when actionable
// tasks remain but the agent has not operated on them this turn. Returns an
// empty string if no tasks are actionable.
func BuildTaskReminder(goal session.Goal) string {
	if !session.HasActionableTasks(goal.Tasks) {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[Task reminder] Actionable goal tasks remain. Finish them with real evidence, or update their status truthfully before considering the goal complete.\n")
	for _, t := range goal.Tasks {
		if t.Status == session.TaskStatusInProgress || t.Status == session.TaskStatusPending {
			sb.WriteString(fmt.Sprintf("  - [%s] %s\n", t.Status, t.Content))
		}
	}
	return sb.String()
}

// BuildContinuationPrompt generates a hidden steer message injected between
// turns to prompt the agent to continue working on the goal.
func BuildContinuationPrompt(goal session.Goal) string {
	return BuildContinuationPromptWithTaskLimit(goal, 0)
}

// BuildContinuationPromptWithTaskLimit is like BuildContinuationPrompt but
// caps the number of tasks rendered in the prompt. A limit of 0 or less
// means no cap. Actionable tasks (in_progress, pending, blocked) are always
// rendered first; completed and dropped tasks are truncated when the limit
// is exceeded.
func BuildContinuationPromptWithTaskLimit(goal session.Goal, maxTasks int) string {
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
	sb.WriteString(fmt.Sprintf("Iterations: %d / %d\n", goal.Iterations, goal.MaxIterations))
	sb.WriteString(fmt.Sprintf("No-progress streak: %d / %d\n", goal.NoProgress, goal.BlockCap))

	if len(goal.Tasks) > 0 {
		// When a task limit is configured, prioritize actionable tasks
		// (in_progress, pending, blocked) and truncate completed/dropped
		// tasks to stay within the budget.
		rendered := goal.Tasks
		truncated := 0
		if maxTasks > 0 && len(goal.Tasks) > maxTasks {
			actionable := make([]session.Task, 0, len(goal.Tasks))
			terminal := make([]session.Task, 0, len(goal.Tasks))
			for _, t := range goal.Tasks {
				switch t.Status {
				case session.TaskStatusInProgress, session.TaskStatusPending, session.TaskStatusBlocked:
					actionable = append(actionable, t)
				default:
					terminal = append(terminal, t)
				}
			}
			remaining := maxTasks - len(actionable)
			if remaining < 0 {
				remaining = 0
			}
			if remaining > len(terminal) {
				remaining = len(terminal)
			}
			rendered = append(actionable, terminal[:remaining]...)
			truncated = len(goal.Tasks) - len(rendered)
		}
		sb.WriteString("\nTasks:\n")
		for i, t := range rendered {
			switch t.Status {
			case session.TaskStatusInProgress:
				sb.WriteString(fmt.Sprintf("%d. [ACTIVE] %s\n", i+1, t.Content))
			case session.TaskStatusPending:
				sb.WriteString(fmt.Sprintf("%d. [pending] %s\n", i+1, t.Content))
			case session.TaskStatusCompleted:
				sb.WriteString(fmt.Sprintf("%d. [completed] %s (evidence: %s)\n", i+1, t.Content, t.Evidence))
			case session.TaskStatusBlocked:
				sb.WriteString(fmt.Sprintf("%d. [blocked] %s (blocker: %s)\n", i+1, t.Content, t.Blocker))
			case session.TaskStatusDropped:
				sb.WriteString(fmt.Sprintf("%d. [dropped] %s (reason: %s)\n", i+1, t.Content, t.DropReason))
			}
		}
		if truncated > 0 {
			sb.WriteString(fmt.Sprintf("... (%d additional completed/dropped tasks omitted)\n", truncated))
		}
		for _, t := range rendered {
			if t.Status == session.TaskStatusInProgress {
				sb.WriteString(fmt.Sprintf("\nCurrent active task: %s\n", t.Content))
				break
			}
		}
	}

	sb.WriteString("\n")
	sb.WriteString("Work instructions:\n")
	sb.WriteString("- Use the `todo` tool with op `start` to focus on one task at a time.\n")
	sb.WriteString("- Use the `todo` tool with op `done` and `evidence` when a task is truly complete.\n")
	sb.WriteString("- Use the `todo` tool with op `drop` and a `reason` only if a task is infeasible.\n")
	sb.WriteString("- Never call goal({op:\"complete\"}) until every task is done or dropped with a reason.\n")
	sb.WriteString("\n")
	sb.WriteString("Completion audit protocol:\n")
	sb.WriteString("1. Restate the objective as concrete deliverables.\n")
	sb.WriteString("2. Map each deliverable to evidence.\n")
	sb.WriteString("3. Inspect actual current state (read files, run commands, check tests).\n")
	sb.WriteString("4. Match verification scope to claim scope.\n")
	sb.WriteString("5. Treat uncertainty as not-yet-achieved.\n")
	sb.WriteString("6. Budget exhaustion is not completion.\n")
	sb.WriteString("\n")
	sb.WriteString("If work is not done, keep working. Never call goal({op:\"complete\"}) just because the budget is low or a turn is ending.\n")
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
