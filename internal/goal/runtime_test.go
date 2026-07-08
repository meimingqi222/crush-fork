package goal

import (
	"context"
	"encoding/json"
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

	require.True(t, ShouldChainContinuation("Continue work on the active goal.", 3))
	require.True(t, ShouldChainContinuation("<guided_goal>\nRefine this goal\n</guided_goal>", 0))
	require.False(t, ShouldChainContinuation("Please add logging to the handler", 0))
	require.False(t, ShouldChainContinuation("Please add logging to the handler", 2))
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
