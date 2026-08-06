package tools

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func runTodoTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params TodoParams) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Name: TodoToolName, Input: string(input)})
	require.NoError(t, err)
	return resp
}

func runTodoToolWithError(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params TodoParams) (fantasy.ToolResponse, string) {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Name: TodoToolName, Input: string(input)})
	if err != nil {
		return resp, err.Error()
	}
	return resp, resp.Content
}

func parseTodoMetadata(t *testing.T, resp fantasy.ToolResponse) TodoResponseMetadata {
	t.Helper()
	var meta TodoResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	return meta
}

func TestTodoTool_InitAndCompleteWithEvidence(t *testing.T) {
	t.Parallel()

	sessions, runtime, _ := newGoalSessionService(t)
	sess, err := sessions.Create(context.Background(), "todo-init")
	require.NoError(t, err)

	goalTool := NewGoalTool(sessions, runtime)
	todoTool := NewTodoTool(sessions, runtime, 0)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, sess.ID)

	runGoalTool(t, goalTool, ctx, GoalParams{Op: "create", Objective: "Refactor auth module"})

	initResp := runTodoTool(t, todoTool, ctx, TodoParams{Op: "init", Items: []string{"Update password hashing", "Add unit tests"}})
	require.False(t, initResp.IsError)
	meta := parseTodoMetadata(t, initResp)
	require.Len(t, meta.Tasks, 2)
	require.Equal(t, session.TaskStatusPending, meta.Tasks[0].Status)

	startResp := runTodoTool(t, todoTool, ctx, TodoParams{Op: "start", Index: 0})
	require.False(t, startResp.IsError)
	startMeta := parseTodoMetadata(t, startResp)
	require.Equal(t, session.TaskStatusInProgress, startMeta.Tasks[0].Status)

	// Missing evidence should not fail the todo tool, but the goal completion
	// gate should reject it.
	doneResp := runTodoTool(t, todoTool, ctx, TodoParams{Op: "done", Index: 0})
	require.False(t, doneResp.IsError)

	_, err = runtime.CompleteGoal(context.Background(), sess.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing evidence")

	// Idempotent done with evidence.
	evidence := "Updated bcrypt.go and ran go test ./auth"
	doneResp = runTodoTool(t, todoTool, ctx, TodoParams{Op: "done", Index: 0, Evidence: evidence})
	require.False(t, doneResp.IsError)
	doneMeta := parseTodoMetadata(t, doneResp)
	require.Equal(t, evidence, doneMeta.Tasks[0].Evidence)

	runTodoTool(t, todoTool, ctx, TodoParams{Op: "done", Index: 1, Evidence: "Added auth_test.go"})

	completed, err := runtime.CompleteGoal(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, session.GoalStatusComplete, completed.Status)
}

func TestTodoTool_NoProgressDrivenByDropAndDone(t *testing.T) {
	t.Parallel()

	sessions, runtime, _ := newGoalSessionService(t)
	sess, err := sessions.Create(context.Background(), "todo-no-progress")
	require.NoError(t, err)

	goalTool := NewGoalTool(sessions, runtime)
	todoTool := NewTodoTool(sessions, runtime, 0)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, sess.ID)

	runGoalTool(t, goalTool, ctx, GoalParams{Op: "create", Objective: "Refactor auth module"})
	runTodoTool(t, todoTool, ctx, TodoParams{Op: "init", Items: []string{"Task A", "Task B"}})

	// Dropping a task increments NoProgress.
	runTodoTool(t, todoTool, ctx, TodoParams{Op: "drop", Index: 0, Reason: "Out of scope"})
	loaded, err := sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), loaded.Goal.NoProgress)

	// Completing the other task resets NoProgress.
	runTodoTool(t, todoTool, ctx, TodoParams{Op: "done", Index: 1, Evidence: "Done"})
	loaded, err = sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, int64(0), loaded.Goal.NoProgress)

	// Idempotent done does not reset from an already-completed state because
	// the task was already completed.
	runTodoTool(t, todoTool, ctx, TodoParams{Op: "done", Index: 1, Evidence: "Updated evidence"})
	loaded, err = sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, int64(0), loaded.Goal.NoProgress)
}

func TestTodoTool_DropCompletedIsRejected(t *testing.T) {
	t.Parallel()

	sessions, runtime, _ := newGoalSessionService(t)
	sess, err := sessions.Create(context.Background(), "todo-drop-completed")
	require.NoError(t, err)

	goalTool := NewGoalTool(sessions, runtime)
	todoTool := NewTodoTool(sessions, runtime, 0)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, sess.ID)

	runGoalTool(t, goalTool, ctx, GoalParams{Op: "create", Objective: "Refactor auth module"})
	runTodoTool(t, todoTool, ctx, TodoParams{Op: "init", Items: []string{"Task A"}})
	runTodoTool(t, todoTool, ctx, TodoParams{Op: "done", Index: 0, Evidence: "Done"})

	_, errText := runTodoToolWithError(t, todoTool, ctx, TodoParams{Op: "drop", Index: 0, Reason: "Oops"})
	require.Contains(t, errText, "cannot drop a completed task")
}

func TestTodoTool_DropRequiresReason(t *testing.T) {
	t.Parallel()

	sessions, runtime, _ := newGoalSessionService(t)
	sess, err := sessions.Create(context.Background(), "todo-drop-reason")
	require.NoError(t, err)

	goalTool := NewGoalTool(sessions, runtime)
	todoTool := NewTodoTool(sessions, runtime, 0)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, sess.ID)

	runGoalTool(t, goalTool, ctx, GoalParams{Op: "create", Objective: "Refactor auth module"})
	runTodoTool(t, todoTool, ctx, TodoParams{Op: "init", Items: []string{"Task A"}})

	_, errText := runTodoToolWithError(t, todoTool, ctx, TodoParams{Op: "drop", Index: 0})
	require.Contains(t, errText, "drop requires a reason")
}

func TestTodoTool_InitRejectsExistingTasks(t *testing.T) {
	t.Parallel()

	sessions, runtime, _ := newGoalSessionService(t)
	sess, err := sessions.Create(context.Background(), "todo-init-twice")
	require.NoError(t, err)

	goalTool := NewGoalTool(sessions, runtime)
	todoTool := NewTodoTool(sessions, runtime, 0)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, sess.ID)

	runGoalTool(t, goalTool, ctx, GoalParams{Op: "create", Objective: "Refactor auth module"})
	runTodoTool(t, todoTool, ctx, TodoParams{Op: "init", Items: []string{"Task A"}})

	_, errText := runTodoToolWithError(t, todoTool, ctx, TodoParams{Op: "init", Items: []string{"Task B"}})
	require.Contains(t, errText, "goal already has tasks")
}

func TestTodoTool_InitRejectsDuplicateItems(t *testing.T) {
	t.Parallel()

	sessions, runtime, _ := newGoalSessionService(t)
	sess, err := sessions.Create(context.Background(), "todo-init-dup")
	require.NoError(t, err)

	goalTool := NewGoalTool(sessions, runtime)
	todoTool := NewTodoTool(sessions, runtime, 0)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, sess.ID)

	runGoalTool(t, goalTool, ctx, GoalParams{Op: "create", Objective: "Refactor auth module"})

	_, errText := runTodoToolWithError(t, todoTool, ctx, TodoParams{Op: "init", Items: []string{"Task A", "Task A"}})
	require.Contains(t, errText, "duplicate task")

	// Verify no partial state was written.
	loaded, err := sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Empty(t, loaded.Goal.Tasks)
}
