package goal

import (
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestOldSessionGoalCompatibility verifies that a goal created without the
// new fields (MaxIterations, BlockCap, Iterations, NoProgress, LastEvaluatorMet)
// -- simulating an old session -- can still be loaded, have tasks added, and
// be completed. This ensures backward compatibility.
func TestOldSessionGoalCompatibility(t *testing.T) {
	t.Parallel()

	svc, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	// Create a goal with the new runtime (which sets all fields).
	_, err := runtime.CreateGoal(ctx, sessionID, "Test old compat", 10000)
	require.NoError(t, err)

	// Manually clear the new fields to simulate an old session goal.
	loaded, err := svc.Get(ctx, sessionID)
	require.NoError(t, err)
	loaded.Goal.MaxIterations = 0
	loaded.Goal.BlockCap = 0
	loaded.Goal.Iterations = 0
	loaded.Goal.NoProgress = 0
	loaded.Goal.LastEvaluatorMet = nil
	loaded.Goal.LastEvaluatorAt = 0
	loaded.Goal.LastReason = ""
	_, err = svc.Save(ctx, loaded)
	require.NoError(t, err)

	// Add tasks to the old-style goal.
	_, err = runtime.AddGoalTask(ctx, sessionID, "Old task A")
	require.NoError(t, err)
	_, err = runtime.AddGoalTask(ctx, sessionID, "Old task B")
	require.NoError(t, err)

	// Complete one task with evidence.
	_, err = runtime.CompleteGoalTask(ctx, sessionID, "Old task A", "did it")
	require.NoError(t, err)

	// Drop the other with a reason.
	loaded, err = svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, loaded.Goal.Tasks, 2)
	loaded.Goal.Tasks[1].Status = session.TaskStatusDropped
	loaded.Goal.Tasks[1].DropReason = "not needed"
	_, err = svc.Save(ctx, loaded)
	require.NoError(t, err)

	// CanCompleteGoal should pass (1 completed with evidence, 1 dropped with reason,
	// drop ratio = 50% which is at the threshold).
	loaded, err = svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.NoError(t, CanCompleteGoal(loaded.Goal))

	// CompleteGoal should succeed.
	_, err = runtime.CompleteGoal(ctx, sessionID)
	require.NoError(t, err)

	loaded, err = svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, session.GoalStatusComplete, loaded.Goal.Status)
}

// TestGoalTaskGateBlocksCompletionWithPendingTask verifies that a goal with
// a pending task cannot be completed, even if the evaluator says met.
func TestGoalTaskGateBlocksCompletionWithPendingTask(t *testing.T) {
	t.Parallel()

	svc, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Test gate", 10000)
	require.NoError(t, err)

	// Add a task but leave it pending.
	_, err = runtime.AddGoalTask(ctx, sessionID, "Pending task")
	require.NoError(t, err)

	// Set evaluator as met.
	loaded, err := svc.Get(ctx, sessionID)
	require.NoError(t, err)
	met := true
	loaded.Goal.LastEvaluatorMet = &met
	_, err = svc.Save(ctx, loaded)
	require.NoError(t, err)

	// CanCompleteGoal should fail because there's a pending task.
	loaded, err = svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Error(t, CanCompleteGoal(loaded.Goal))
}

// TestGoalTaskGateBlockCapTerminatesOnStall verifies that a goal with
// BlockCap set will be stalled after BlockCap consecutive turns without
// progress, preventing infinite continuation.
func TestGoalTaskGateBlockCapTerminatesOnStall(t *testing.T) {
	t.Parallel()

	svc, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Test block cap", 10000)
	require.NoError(t, err)

	loaded, err := svc.Get(ctx, sessionID)
	require.NoError(t, err)
	loaded.Goal.BlockCap = 2
	loaded.Goal.Tasks = []session.Task{
		{Content: "A", Status: session.TaskStatusPending},
	}
	_, err = svc.Save(ctx, loaded)
	require.NoError(t, err)

	// Two turns without progress.
	for i := 1; i <= 2; i++ {
		require.NoError(t, runtime.OnTurnStart(ctx, sessionID, "turn-"+string(rune('0'+i)), TokenUsage{}))
		goal, _, err := runtime.PostTurn(ctx, sessionID, TokenUsage{Input: 10})
		require.NoError(t, err)
		if i < 2 {
			require.Equal(t, session.GoalStatusActive, goal.Status, "should still be active at turn %d", i)
		}
	}

	// After BlockCap turns without progress, the goal should be stalled.
	loaded, err = svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, session.GoalStatusStalled, loaded.Goal.Status)
	require.False(t, NeedsContinuation(loaded.Goal), "stalled goal should not need continuation")
}

// TestSubagentTaskUpdateDoesNotCorruptParent verifies that when a subagent
// submits a task update, it does not immediately modify the parent's goal
// state. The update should only be applied when the parent's next turn starts.
func TestSubagentTaskUpdateDoesNotCorruptParent(t *testing.T) {
	t.Parallel()

	svc, runtime, sessionID := newRuntimeTestSession(t)
	ctx := context.Background()

	_, err := runtime.CreateGoal(ctx, sessionID, "Test subagent isolation", 10000)
	require.NoError(t, err)

	loaded, err := svc.Get(ctx, sessionID)
	require.NoError(t, err)
	loaded.Goal.Tasks = []session.Task{
		{Content: "Task A", Status: session.TaskStatusPending},
	}
	_, err = svc.Save(ctx, loaded)
	require.NoError(t, err)

	// Subagent marks task as completed.
	require.NoError(t, runtime.ApplySubagentTaskUpdate(sessionID, SubagentTaskUpdate{
		TaskContent: "Task A",
		NewStatus:   session.TaskStatusCompleted,
		Evidence:    "done by subagent",
		ToolCallID:  "call-1",
	}))

	// Parent session should NOT yet see the update (it's queued).
	loaded, err = svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, session.TaskStatusPending, loaded.Goal.Tasks[0].Status,
		"parent should not see subagent update before OnTurnStart")

	// OnTurnStart applies the queued update.
	require.NoError(t, runtime.OnTurnStart(ctx, sessionID, "turn-1", TokenUsage{}))
	loaded, err = svc.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, session.TaskStatusCompleted, loaded.Goal.Tasks[0].Status)
	require.Equal(t, "done by subagent", loaded.Goal.Tasks[0].Evidence)
}
