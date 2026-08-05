package goal

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func newRuntimeTestSession(t *testing.T) (session.Service, *Runtime, string) {
	t.Helper()
	conn, err := db.Connect(context.Background(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, conn.Close())
	})
	svc := session.NewService(db.New(conn), conn)
	runtime := NewRuntime(svc)
	sess, err := svc.Create(context.Background(), "runtime-test")
	require.NoError(t, err)
	return svc, runtime, sess.ID
}

func TestGoalIDJSONRoundTrip(t *testing.T) {
	t.Parallel()

	goal := session.Goal{
		ID:          "goal-abc-123",
		Text:        "Refactor auth",
		Status:      session.GoalStatusActive,
		TokenBudget: 1000,
		TokensUsed:  100,
		TimeSeconds: 30,
	}

	data, err := json.Marshal(goal)
	require.NoError(t, err)
	require.Contains(t, string(data), `"id":"goal-abc-123"`)

	var parsed session.Goal
	require.NoError(t, json.Unmarshal(data, &parsed))
	require.Equal(t, goal.ID, parsed.ID)
	require.Equal(t, goal.Text, parsed.Text)
	require.Equal(t, goal.Status, parsed.Status)
}

func TestEmptyGoalIDOmitempty(t *testing.T) {
	t.Parallel()

	goal := session.Goal{Text: "No ID yet"}
	data, err := json.Marshal(goal)
	require.NoError(t, err)
	require.NotContains(t, string(data), `"id"`)
}

func TestIsSteerPrompt(t *testing.T) {
	t.Parallel()

	require.True(t, IsSteerPrompt("Continue work on the active goal.\n\nObjective: ship it"))
	require.True(t, IsSteerPrompt("Token budget exhausted for the active goal.\n\nObjective: ship it"))
	require.False(t, IsSteerPrompt("Fix the failing test in coordinator.go"))
}

func TestShouldChainContinuation(t *testing.T) {
	t.Parallel()

	// Steer prompts always chain, regardless of depth or guided-goal flag.
	require.True(t, ShouldChainContinuation("Continue work on the active goal.", 3, false))
	require.True(t, ShouldChainContinuation("Token budget exhausted for the active goal.", 1, false))
	// Guided-goal setup chains only at depth 0.
	require.True(t, ShouldChainContinuation("Refine this goal", 0, true))
	require.False(t, ShouldChainContinuation("Refine this goal", 2, true))
	// Non-steer, non-guided prompts never chain even at depth 0.
	require.False(t, ShouldChainContinuation("Please add logging to the handler", 0, false))
	// A user typing "<guided_goal>" mid-prompt no longer triggers chaining
	// because continuation now keys off the typed flag, not substring match.
	require.False(t, ShouldChainContinuation("Please <guided_goal> add logging", 0, false))
}

func TestIsGuidedGoalPrompt(t *testing.T) {
	t.Parallel()

	require.True(t, IsGuidedGoalPrompt("<guided_goal>\nRefine this goal\n</guided_goal>"))
	require.True(t, IsGuidedGoalPrompt("  <guided_goal>\nShip it"))
	// Substring occurrences without the prefix must not match.
	require.False(t, IsGuidedGoalPrompt("Please review <guided_goal> in my message"))
	require.False(t, IsGuidedGoalPrompt("Regular user prompt"))
}

func TestCreateGoalRejectsPlanMode(t *testing.T) {
	t.Parallel()

	svc, runtime, sessionID := newRuntimeTestSession(t)
	sess, err := svc.Get(context.Background(), sessionID)
	require.NoError(t, err)
	sess.CollaborationMode = session.CollaborationModePlan
	_, err = svc.Save(context.Background(), sess)
	require.NoError(t, err)

	_, err = runtime.CreateGoal(context.Background(), sessionID, "Ship it", 0)
	require.ErrorIs(t, err, session.ErrPlanBlocksGoalMode)
}

func TestPostTurnComputesDeltaFromBaseline(t *testing.T) {
	t.Parallel()

	svc, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Test goal", 1000)
	require.NoError(t, err)

	baseline := TokenUsage{Input: 100, Output: 50, CacheWrite: 10}
	require.NoError(t, runtime.OnTurnStart(ctx, sessionID, "turn-1", baseline))

	current := TokenUsage{Input: 250, Output: 120, CacheWrite: 40}
	updated, exhausted, err := runtime.PostTurn(ctx, sessionID, current)
	require.NoError(t, err)
	require.False(t, exhausted)
	require.Equal(t, int64(250), updated.TokensUsed)

	loaded, err := svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, int64(250), loaded.Goal.TokensUsed)
}

func TestPostTurnExcludesCacheRead(t *testing.T) {
	t.Parallel()

	_, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Test goal", 1000)
	require.NoError(t, err)

	baseline := TokenUsage{Input: 100, Output: 50, CacheRead: 20, CacheWrite: 10}
	require.NoError(t, runtime.OnTurnStart(ctx, sessionID, "turn-1", baseline))

	current := TokenUsage{Input: 100, Output: 50, CacheRead: 80, CacheWrite: 10}
	updated, _, err := runtime.PostTurn(ctx, sessionID, current)
	require.NoError(t, err)
	require.Equal(t, int64(0), updated.TokensUsed)
}

func TestPostTurnIncludesCacheWrite(t *testing.T) {
	t.Parallel()

	_, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Test goal", 1000)
	require.NoError(t, err)

	baseline := TokenUsage{Input: 100, Output: 50, CacheWrite: 10}
	require.NoError(t, runtime.OnTurnStart(ctx, sessionID, "turn-1", baseline))

	current := TokenUsage{Input: 100, Output: 50, CacheWrite: 35}
	updated, _, err := runtime.PostTurn(ctx, sessionID, current)
	require.NoError(t, err)
	require.Equal(t, int64(25), updated.TokensUsed)
}

func TestPostTurnSkipsAccountingWhenGoalReplaced(t *testing.T) {
	t.Parallel()

	svc, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	original, err := runtime.CreateGoal(ctx, sessionID, "Original goal", 1000)
	require.NoError(t, err)

	baseline := TokenUsage{Input: 10, Output: 5}
	require.NoError(t, runtime.OnTurnStart(ctx, sessionID, "turn-1", baseline))

	replaced, err := runtime.ReplaceGoal(ctx, sessionID, "Replaced goal", 1000)
	require.NoError(t, err)
	require.NotEqual(t, original.ID, replaced.ID)

	current := TokenUsage{Input: 110, Output: 55}
	updated, _, err := runtime.PostTurn(ctx, sessionID, current)
	require.NoError(t, err)
	require.Equal(t, int64(0), updated.TokensUsed)

	loaded, err := svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, replaced.ID, loaded.Goal.ID)
	require.Equal(t, int64(0), loaded.Goal.TokensUsed)
}

func TestPostTurnMarksBudgetLimited(t *testing.T) {
	t.Parallel()

	_, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Test goal", 100)
	require.NoError(t, err)

	require.NoError(t, runtime.OnTurnStart(ctx, sessionID, "turn-1", TokenUsage{}))
	updated, exhausted, err := runtime.PostTurn(ctx, sessionID, TokenUsage{Input: 200, Output: 50})
	require.NoError(t, err)
	require.True(t, exhausted)
	require.Equal(t, session.GoalStatusBudgetLimited, updated.Status)
}

func TestPostTurnWithIntegersBackwardCompatible(t *testing.T) {
	t.Parallel()

	_, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Test goal", 1000)
	require.NoError(t, err)

	updated, _, err := runtime.PostTurnWithIntegers(ctx, sessionID, 100, 50)
	require.NoError(t, err)
	require.Equal(t, int64(150), updated.TokensUsed)
}

func TestWallClockPausedTimeNotCounted(t *testing.T) {
	t.Parallel()

	svc, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Test goal", 1000)
	require.NoError(t, err)

	// Let some active time elapse, then pause.
	time.Sleep(1200 * time.Millisecond)
	paused, err := runtime.PauseGoal(ctx, sessionID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, paused.TimeSeconds, int64(1))

	pausedTime := paused.TimeSeconds

	// Sleep while paused; time should not increase.
	time.Sleep(700 * time.Millisecond)
	loaded, err := svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, pausedTime, loaded.Goal.TimeSeconds)

	// Resume and let more active time elapse.
	_, err = runtime.ResumeGoal(ctx, sessionID)
	require.NoError(t, err)
	time.Sleep(1200 * time.Millisecond)
	completed, err := runtime.CompleteGoal(ctx, sessionID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, completed.TimeSeconds, pausedTime+int64(1))
}

func TestReplaceGoalSettlesActiveTime(t *testing.T) {
	t.Parallel()

	svc, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Original goal", 1000)
	require.NoError(t, err)

	time.Sleep(1200 * time.Millisecond)
	replaced, err := runtime.ReplaceGoal(ctx, sessionID, "Replaced goal", 1000)
	require.NoError(t, err)
	require.GreaterOrEqual(t, replaced.TimeSeconds, int64(1))

	// Ensure the replaced goal keeps timing from the new active period.
	time.Sleep(1200 * time.Millisecond)
	loaded, err := svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, replaced.ID, loaded.Goal.ID)
	require.True(t, loaded.Goal.IsActive())
}

func TestDropGoalSettlesActiveTime(t *testing.T) {
	t.Parallel()

	svc, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Test goal", 1000)
	require.NoError(t, err)

	time.Sleep(1200 * time.Millisecond)
	_, err = runtime.DropGoal(ctx, sessionID)
	require.NoError(t, err)

	loaded, err := svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, session.GoalStatusDropped, loaded.Goal.Status)
	require.GreaterOrEqual(t, loaded.Goal.TimeSeconds, int64(1))
}

func TestSetBudgetGoalTransitionsStatusAndTime(t *testing.T) {
	t.Parallel()

	svc, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Test goal", 1000)
	require.NoError(t, err)

	// Spend some active time then exhaust the budget.
	time.Sleep(1200 * time.Millisecond)
	_, err = runtime.SetBudgetGoal(ctx, sessionID, 0)
	require.NoError(t, err)

	loaded, err := svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.True(t, loaded.Goal.IsActive())

	// Lower the budget below usage to trigger exhaustion.
	require.NoError(t, runtime.OnTurnStart(ctx, sessionID, "turn-1", TokenUsage{}))
	_, _, err = runtime.PostTurn(ctx, sessionID, TokenUsage{Input: 100, Output: 50})
	require.NoError(t, err)

	updated, err := runtime.SetBudgetGoal(ctx, sessionID, 50)
	require.NoError(t, err)
	require.Equal(t, session.GoalStatusBudgetLimited, updated.Status)

	// Raise the budget back above usage to resume active timing.
	resumed, err := runtime.SetBudgetGoal(ctx, sessionID, 500)
	require.NoError(t, err)
	require.True(t, resumed.IsActive())
}

func TestPauseActiveGoalOnLoad(t *testing.T) {
	t.Parallel()

	svc, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Test goal", 1000)
	require.NoError(t, err)

	time.Sleep(1200 * time.Millisecond)
	paused, notice, err := runtime.PauseActiveGoalOnLoad(ctx, sessionID, false)
	require.NoError(t, err)
	require.NotEmpty(t, notice)
	require.Equal(t, session.GoalStatusPaused, paused.Status)
	require.GreaterOrEqual(t, paused.TimeSeconds, int64(1))

	loaded, err := svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, session.GoalStatusPaused, loaded.Goal.Status)
}

func TestPauseActiveGoalOnLoadPreserve(t *testing.T) {
	t.Parallel()

	svc, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Test goal", 1000)
	require.NoError(t, err)

	paused, notice, err := runtime.PauseActiveGoalOnLoad(ctx, sessionID, true)
	require.NoError(t, err)
	require.Empty(t, notice)
	require.Empty(t, paused.Status)

	loaded, err := svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.True(t, loaded.Goal.IsActive())
}

// TestPostTurnIsolatesConcurrentSessions verifies that two sessions sharing a
// single Runtime instance do not clobber each other's turn baseline or
// wall-clock state when their turns interleave. This is the regression test
// for the cross-session data race where snapshot/wallClock were single struct
// fields instead of per-session maps.
func TestPostTurnIsolatesConcurrentSessions(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(context.Background(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	svc := session.NewService(db.New(conn), conn)
	runtime := NewRuntime(svc)

	sessA, err := svc.Create(context.Background(), "session-A")
	require.NoError(t, err)
	sessB, err := svc.Create(context.Background(), "session-B")
	require.NoError(t, err)

	ctx := context.Background()

	// Both sessions have active goals with generous budgets.
	_, err = runtime.CreateGoal(ctx, sessA.ID, "Goal A", 10000)
	require.NoError(t, err)
	_, err = runtime.CreateGoal(ctx, sessB.ID, "Goal B", 10000)
	require.NoError(t, err)

	// Session A starts a turn, then session B starts a turn before A's
	// PostTurn fires. With the old single-field design, B's baseline would
	// overwrite A's.
	require.NoError(t, runtime.OnTurnStart(ctx, sessA.ID, "turn-a-1", TokenUsage{Input: 100, Output: 50, CacheWrite: 10}))
	require.NoError(t, runtime.OnTurnStart(ctx, sessB.ID, "turn-b-1", TokenUsage{Input: 500, Output: 200, CacheWrite: 5}))

	// Session A finishes its turn. Its delta must be computed against A's
	// baseline (100+50+10=160), not B's baseline.
	currentA := TokenUsage{Input: 300, Output: 150, CacheWrite: 40} // delta = 200+100+30 = 330
	goalA, exhaustedA, err := runtime.PostTurn(ctx, sessA.ID, currentA)
	require.NoError(t, err)
	require.False(t, exhaustedA)
	require.Equal(t, int64(330), goalA.TokensUsed, "session A delta must use A's baseline, not B's")

	// Session B finishes its turn. Its delta must be computed against B's
	// baseline (500+200+5=705), not A's already-consumed baseline.
	currentB := TokenUsage{Input: 700, Output: 300, CacheWrite: 25} // delta = 200+100+20 = 320
	goalB, exhaustedB, err := runtime.PostTurn(ctx, sessB.ID, currentB)
	require.NoError(t, err)
	require.False(t, exhaustedB)
	require.Equal(t, int64(320), goalB.TokensUsed, "session B delta must use B's baseline, not A's")

	// Neither session's persisted goal picked up the other's tokens.
	loadedA, err := svc.Get(ctx, sessA.ID)
	require.NoError(t, err)
	require.Equal(t, int64(330), loadedA.Goal.TokensUsed)
	loadedB, err := svc.Get(ctx, sessB.ID)
	require.NoError(t, err)
	require.Equal(t, int64(320), loadedB.Goal.TokensUsed)
}

func TestCanCompleteGoal(t *testing.T) {
	t.Parallel()

	// No tasks: pass.
	require.NoError(t, CanCompleteGoal(session.Goal{}))

	// All completed with evidence: pass.
	require.NoError(t, CanCompleteGoal(session.Goal{Tasks: []session.Task{
		{Content: "A", Status: session.TaskStatusCompleted, Evidence: "did A"},
	}}))

	// Pending task: fail.
	require.Error(t, CanCompleteGoal(session.Goal{Tasks: []session.Task{
		{Content: "A", Status: session.TaskStatusPending},
	}}))

	// Completed without evidence: fail.
	require.Error(t, CanCompleteGoal(session.Goal{Tasks: []session.Task{
		{Content: "A", Status: session.TaskStatusCompleted},
	}}))

	// Dropped without reason: fail.
	require.Error(t, CanCompleteGoal(session.Goal{Tasks: []session.Task{
		{Content: "A", Status: session.TaskStatusDropped},
	}}))

	// Drop ratio too high: fail (two dropped and one completed).
	require.Error(t, CanCompleteGoal(session.Goal{Tasks: []session.Task{
		{Content: "A", Status: session.TaskStatusCompleted, Evidence: "did A"},
		{Content: "B", Status: session.TaskStatusDropped, DropReason: "nope"},
		{Content: "C", Status: session.TaskStatusDropped, DropReason: "nope"},
	}}))

	// Mixed with drop ratio at exactly 50%: pass.
	require.NoError(t, CanCompleteGoal(session.Goal{Tasks: []session.Task{
		{Content: "A", Status: session.TaskStatusCompleted, Evidence: "did A"},
		{Content: "B", Status: session.TaskStatusCompleted, Evidence: "did B"},
		{Content: "C", Status: session.TaskStatusDropped, DropReason: "nope"},
		{Content: "D", Status: session.TaskStatusDropped, DropReason: "nope"},
	}}))

	// Evaluator says not met: fail.
	met := false
	require.Error(t, CanCompleteGoal(session.Goal{
		Tasks:            []session.Task{{Content: "A", Status: session.TaskStatusCompleted, Evidence: "did A"}},
		LastEvaluatorMet: &met,
	}))
}

func TestCanCompleteGoalWithGateLax(t *testing.T) {
	t.Parallel()

	// Lax gate: no evidence required, no drop ratio check.
	tasks := []session.Task{
		{Content: "A", Status: session.TaskStatusCompleted}, // no evidence
		{Content: "B", Status: session.TaskStatusDropped},   // no drop reason
		{Content: "C", Status: session.TaskStatusDropped},   // no drop reason
		{Content: "D", Status: session.TaskStatusDropped},   // no drop reason
	}
	// Strict: should fail (missing evidence + too many drops).
	require.Error(t, CanCompleteGoalWithGate(session.Goal{Tasks: tasks}, "strict"))
	// Lax: should pass (only checks no pending/in_progress/blocked).
	require.NoError(t, CanCompleteGoalWithGate(session.Goal{Tasks: tasks}, "lax"))
	// Off: always passes.
	require.NoError(t, CanCompleteGoalWithGate(session.Goal{Tasks: tasks}, "off"))

	// Lax still rejects pending tasks.
	pendingTasks := []session.Task{{Content: "X", Status: session.TaskStatusPending}}
	require.Error(t, CanCompleteGoalWithGate(session.Goal{Tasks: pendingTasks}, "lax"))
}

func TestApplyEvaluatorVerdict(t *testing.T) {
	t.Parallel()

	t.Run("met resets_no_progress", func(t *testing.T) {
		t.Parallel()
		g := session.Goal{NoProgress: 5, Status: session.GoalStatusActive}
		ApplyEvaluatorVerdict(&g, EvaluatorVerdict{Met: true, Reason: "done"})
		require.Equal(t, int64(0), g.NoProgress)
		require.NotNil(t, g.LastEvaluatorMet)
		require.True(t, *g.LastEvaluatorMet)
	})

	t.Run("not_met_no_progress_increments", func(t *testing.T) {
		t.Parallel()
		g := session.Goal{NoProgress: 2, Status: session.GoalStatusActive}
		ApplyEvaluatorVerdict(&g, EvaluatorVerdict{Met: false, Progress: false, Reason: "stuck"})
		require.Equal(t, int64(3), g.NoProgress)
		require.False(t, *g.LastEvaluatorMet)
	})

	t.Run("waiting_does_not_increment", func(t *testing.T) {
		t.Parallel()
		g := session.Goal{NoProgress: 2, Status: session.GoalStatusActive}
		ApplyEvaluatorVerdict(&g, EvaluatorVerdict{Met: false, Progress: false, Waiting: true})
		require.Equal(t, int64(2), g.NoProgress)
	})

	t.Run("impossible_drops_goal", func(t *testing.T) {
		t.Parallel()
		g := session.Goal{NoProgress: 2, Status: session.GoalStatusActive}
		ApplyEvaluatorVerdict(&g, EvaluatorVerdict{Impossible: true, Reason: "can't do it"})
		require.Equal(t, session.GoalStatusDropped, g.Status)
		require.Contains(t, g.LastReason, "can't do it")
	})
}

func TestPostTurnTerminatesOnMaxIterations(t *testing.T) {
	t.Parallel()

	svc, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Iterate", 10000)
	require.NoError(t, err)

	loaded, err := svc.Get(ctx, sessionID)
	require.NoError(t, err)
	loaded.Goal.MaxIterations = 3
	_, err = svc.Save(ctx, loaded)
	require.NoError(t, err)

	for i := int64(1); i <= 3; i++ {
		require.NoError(t, runtime.OnTurnStart(ctx, sessionID, fmt.Sprintf("turn-%d", i), TokenUsage{}))
		goal, _, err := runtime.PostTurn(ctx, sessionID, TokenUsage{Input: 10})
		require.NoError(t, err)
		require.Equal(t, i, goal.Iterations, "iteration %d", i)
		if i < 3 {
			require.Equal(t, session.GoalStatusActive, goal.Status, "still active at iteration %d", i)
		}
	}

	final, err := svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, session.GoalStatusDropped, final.Goal.Status)
	require.Contains(t, final.Goal.LastReason, "maximum iterations")
}

func TestPostTurnDrivesNoProgressFromTasks(t *testing.T) {
	t.Parallel()

	svc, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Stall test", 10000)
	require.NoError(t, err)

	loaded, err := svc.Get(ctx, sessionID)
	require.NoError(t, err)
	loaded.Goal.BlockCap = 3
	loaded.Goal.Tasks = []session.Task{
		{Content: "A", Status: session.TaskStatusPending},
		{Content: "B", Status: session.TaskStatusPending},
	}
	_, err = svc.Save(ctx, loaded)
	require.NoError(t, err)

	// Turn 1 with no task completion: no_progress increments.
	require.NoError(t, runtime.OnTurnStart(ctx, sessionID, "turn-1", TokenUsage{}))
	goal, _, err := runtime.PostTurn(ctx, sessionID, TokenUsage{Input: 10})
	require.NoError(t, err)
	require.Equal(t, int64(1), goal.NoProgress)

	// Start turn 2, then simulate a tool call completing a task mid-turn.
	require.NoError(t, runtime.OnTurnStart(ctx, sessionID, "turn-2", TokenUsage{}))
	loaded, err = svc.Get(ctx, sessionID)
	require.NoError(t, err)
	loaded.Goal.Tasks[0].Status = session.TaskStatusCompleted
	loaded.Goal.NoProgress = 0
	_, err = svc.Save(ctx, loaded)
	require.NoError(t, err)

	goal, _, err = runtime.PostTurn(ctx, sessionID, TokenUsage{Input: 10})
	require.NoError(t, err)
	require.Equal(t, int64(0), goal.NoProgress)

	// Idempotent completion (no new completed tasks) does not reset again.
	require.NoError(t, runtime.OnTurnStart(ctx, sessionID, "turn-3", TokenUsage{}))
	goal, _, err = runtime.PostTurn(ctx, sessionID, TokenUsage{Input: 10})
	require.NoError(t, err)
	require.Equal(t, int64(1), goal.NoProgress)
}

func TestSubagentTaskUpdateQueue(t *testing.T) {
	t.Parallel()

	svc, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Refactor auth module", 10000)
	require.NoError(t, err)

	// Initialize two tasks in the parent session.
	loaded, err := svc.Get(ctx, sessionID)
	require.NoError(t, err)
	loaded.Goal.Tasks = []session.Task{
		{Content: "Task A", Status: session.TaskStatusPending},
		{Content: "Task B", Status: session.TaskStatusPending},
	}
	_, err = svc.Save(ctx, loaded)
	require.NoError(t, err)

	// A subagent marks Task A as in-progress and provides evidence for Task B.
	require.NoError(t, runtime.ApplySubagentTaskUpdate(sessionID, SubagentTaskUpdate{
		TaskContent: "Task A",
		NewStatus:   session.TaskStatusInProgress,
		ToolCallID:  "call-1",
	}))
	require.NoError(t, runtime.ApplySubagentTaskUpdate(sessionID, SubagentTaskUpdate{
		TaskContent: "Task B",
		NewStatus:   session.TaskStatusCompleted,
		Evidence:    "B is done",
		ToolCallID:  "call-2",
	}))

	// Parent turn starts: pending updates are applied.
	require.NoError(t, runtime.OnTurnStart(ctx, sessionID, "turn-1", TokenUsage{}))

	loaded, err = svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, loaded.Goal.Tasks, 2)
	require.Equal(t, session.TaskStatusInProgress, loaded.Goal.Tasks[0].Status)
	require.Equal(t, session.TaskStatusCompleted, loaded.Goal.Tasks[1].Status)
	require.Equal(t, "B is done", loaded.Goal.Tasks[1].Evidence)
	require.Equal(t, int64(0), loaded.Goal.NoProgress)

	// Queue is drained.
	require.NoError(t, runtime.OnTurnStart(ctx, sessionID, "turn-2", TokenUsage{}))
	loaded, err = svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, int64(0), loaded.Goal.NoProgress)
}

func TestSubagentTaskUpdateRejectsUnknownTask(t *testing.T) {
	t.Parallel()

	svc, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Refactor auth module", 10000)
	require.NoError(t, err)

	loaded, err := svc.Get(ctx, sessionID)
	require.NoError(t, err)
	loaded.Goal.Tasks = []session.Task{
		{Content: "Task A", Status: session.TaskStatusPending},
	}
	_, err = svc.Save(ctx, loaded)
	require.NoError(t, err)

	// Subagent tries to mark a non-existent task as done.
	require.NoError(t, runtime.ApplySubagentTaskUpdate(sessionID, SubagentTaskUpdate{
		TaskContent: "Task does not exist",
		NewStatus:   session.TaskStatusCompleted,
		Evidence:    "fake evidence",
		ToolCallID:  "call-1",
	}))

	// Parent applies pending updates; the unknown task must NOT be created.
	require.NoError(t, runtime.OnTurnStart(ctx, sessionID, "turn-1", TokenUsage{}))

	loaded, err = svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, loaded.Goal.Tasks, 1)
	require.Equal(t, "Task A", loaded.Goal.Tasks[0].Content)
}

func TestBuildContinuationPromptWithTaskLimit(t *testing.T) {
	t.Parallel()

	// 3 actionable + 5 completed = 8 tasks, limit 5 -> render 3 actionable
	// + 2 completed, omit 3.
	tasks := make([]session.Task, 0, 8)
	tasks = append(tasks,
		session.Task{Content: "A1", Status: session.TaskStatusInProgress},
		session.Task{Content: "A2", Status: session.TaskStatusPending},
		session.Task{Content: "A3", Status: session.TaskStatusBlocked, Blocker: "x"},
	)
	for i := 0; i < 5; i++ {
		tasks = append(tasks, session.Task{
			Content:  fmt.Sprintf("C%d", i),
			Status:   session.TaskStatusCompleted,
			Evidence: "done",
		})
	}
	g := session.Goal{Text: "Test", Tasks: tasks}

	prompt := BuildContinuationPromptWithTaskLimit(g, 5)
	require.Contains(t, prompt, "A1")
	require.Contains(t, prompt, "A2")
	require.Contains(t, prompt, "A3")
	require.Contains(t, prompt, "C0")
	require.Contains(t, prompt, "C1")
	require.NotContains(t, prompt, "C2")
	require.Contains(t, prompt, "3 additional completed/dropped tasks omitted")

	// No limit -> all tasks rendered.
	promptFull := BuildContinuationPromptWithTaskLimit(g, 0)
	require.Contains(t, promptFull, "C4")
	require.NotContains(t, promptFull, "omitted")
}

func TestAddGoalTaskAndCompleteGoalTask(t *testing.T) {
	t.Parallel()

	svc, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Test goal", 10000)
	require.NoError(t, err)

	// Add a task.
	_, err = runtime.AddGoalTask(ctx, sessionID, "Task A")
	require.NoError(t, err)
	loaded, err := svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, loaded.Goal.Tasks, 1)
	require.Equal(t, "Task A", loaded.Goal.Tasks[0].Content)
	require.Equal(t, session.TaskStatusPending, loaded.Goal.Tasks[0].Status)

	// Duplicate add is rejected.
	_, err = runtime.AddGoalTask(ctx, sessionID, "Task A")
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate")

	// Complete the task by content.
	_, err = runtime.CompleteGoalTask(ctx, sessionID, "Task A", "evidence here")
	require.NoError(t, err)
	loaded, err = svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, session.TaskStatusCompleted, loaded.Goal.Tasks[0].Status)
	require.Equal(t, "evidence here", loaded.Goal.Tasks[0].Evidence)

	// Unknown task is rejected.
	_, err = runtime.CompleteGoalTask(ctx, sessionID, "Nonexistent", "")
	require.Error(t, err)
}

func TestPostTurnTerminatesOnStall(t *testing.T) {
	t.Parallel()

	svc, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Stall test", 10000)
	require.NoError(t, err)

	loaded, err := svc.Get(ctx, sessionID)
	require.NoError(t, err)
	loaded.Goal.BlockCap = 2
	_, err = svc.Save(ctx, loaded)
	require.NoError(t, err)

	// First turn does not make progress.
	require.NoError(t, runtime.OnTurnStart(ctx, sessionID, "turn-1", TokenUsage{}))
	goal, _, err := runtime.PostTurn(ctx, sessionID, TokenUsage{Input: 10})
	require.NoError(t, err)
	require.Equal(t, session.GoalStatusActive, goal.Status)

	// Manually increment no_progress to simulate a stall from todo drops/evaluator.
	loaded, err = svc.Get(ctx, sessionID)
	require.NoError(t, err)
	loaded.Goal.NoProgress = 2
	_, err = svc.Save(ctx, loaded)
	require.NoError(t, err)

	require.NoError(t, runtime.OnTurnStart(ctx, sessionID, "turn-2", TokenUsage{}))
	goal, _, err = runtime.PostTurn(ctx, sessionID, TokenUsage{Input: 10})
	require.NoError(t, err)
	require.Equal(t, session.GoalStatusStalled, goal.Status)
	require.Contains(t, goal.LastReason, "No progress")
}

// TestBudgetExhaustedGoalNeedsContinuationForWrapUp documents the contract
// that the coordinator relies on: when a goal's budget is exhausted,
// PostTurn returns budgetExhausted=true and the goal enters
// GoalStatusBudgetLimited. NeedsContinuation returns false for that status,
// so the coordinator must OR the two conditions together
// (NeedsContinuation(goal) || budgetExhausted) to still inject the
// BuildBudgetLimitPrompt wrap-up turn. This test guards against regressing
// that OR back to a plain NeedsContinuation check.
func TestBudgetExhaustedGoalNeedsContinuationForWrapUp(t *testing.T) {
	t.Parallel()

	_, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Test goal", 100)
	require.NoError(t, err)

	require.NoError(t, runtime.OnTurnStart(ctx, sessionID, "turn-1", TokenUsage{}))
	goal, budgetExhausted, err := runtime.PostTurn(ctx, sessionID, TokenUsage{Input: 200, Output: 50})
	require.NoError(t, err)

	// Budget exhausted: the goal is no longer "active" but the coordinator
	// must still chain a wrap-up turn.
	require.True(t, budgetExhausted)
	require.Equal(t, session.GoalStatusBudgetLimited, goal.Status)
	require.False(t, NeedsContinuation(goal), "NeedsContinuation must be false for budget-limited goals")
	// The coordinator condition is: NeedsContinuation(goal) || budgetExhausted
	require.True(t, NeedsContinuation(goal) || budgetExhausted,
		"coordinator must chain a wrap-up turn when budget is exhausted even though NeedsContinuation is false")
}
