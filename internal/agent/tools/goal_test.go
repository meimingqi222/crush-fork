package tools

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func newGoalSessionService(t *testing.T) (session.Service, *goal.Runtime) {
	t.Helper()
	conn, err := db.Connect(context.Background(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, conn.Close())
	})
	svc := session.NewService(db.New(conn), conn)
	return svc, goal.NewRuntime(svc)
}

func runGoalTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params GoalParams) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Name: GoalToolName, Input: string(input)})
	require.NoError(t, err)
	return resp
}

func parseGoalMetadata(t *testing.T, resp fantasy.ToolResponse) GoalResponseMetadata {
	t.Helper()
	var meta GoalResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	return meta
}

func TestGoalToolCreateSetsIDAndBudget(t *testing.T) {
	t.Parallel()

	sessions, runtime := newGoalSessionService(t)
	sess, err := sessions.Create(context.Background(), "goal-create")
	require.NoError(t, err)

	tool := NewGoalTool(sessions, runtime)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, sess.ID)

	budget := int64(200000)
	resp := runGoalTool(t, tool, ctx, GoalParams{Op: "create", Objective: "Refactor auth module", TokenBudget: &budget})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "Goal created")

	meta := parseGoalMetadata(t, resp)
	require.NotEmpty(t, meta.Goal.ID)
	require.Equal(t, "Refactor auth module", meta.Goal.Text)
	require.Equal(t, session.GoalStatusActive, meta.Goal.Status)
	require.Equal(t, budget, meta.Goal.TokenBudget)

	loaded, err := sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, meta.Goal.ID, loaded.Goal.ID)
}

func TestGoalToolReplacePreservesStatsAndGeneratesNewID(t *testing.T) {
	t.Parallel()

	sessions, runtime := newGoalSessionService(t)
	sess, err := sessions.Create(context.Background(), "goal-replace")
	require.NoError(t, err)

	tool := NewGoalTool(sessions, runtime)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, sess.ID)

	budget := int64(100000)
	createResp := runGoalTool(t, tool, ctx, GoalParams{Op: "create", Objective: "Original goal", TokenBudget: &budget})
	require.False(t, createResp.IsError)
	created := parseGoalMetadata(t, createResp).Goal

	sess.Goal = created
	sess.Goal.TokensUsed = 5000
	sess.Goal.TimeSeconds = 120
	_, err = sessions.Save(context.Background(), sess)
	require.NoError(t, err)

	newBudget := int64(150000)
	replaceResp := runGoalTool(t, tool, ctx, GoalParams{Op: "replace", Objective: "Updated goal", TokenBudget: &newBudget})
	require.False(t, replaceResp.IsError)
	require.Contains(t, replaceResp.Content, "Goal replaced")

	replaced := parseGoalMetadata(t, replaceResp).Goal
	require.NotEmpty(t, replaced.ID)
	require.NotEqual(t, created.ID, replaced.ID)
	require.Equal(t, "Updated goal", replaced.Text)
	require.Equal(t, session.GoalStatusActive, replaced.Status)
	require.Equal(t, int64(5000), replaced.TokensUsed)
	require.Equal(t, int64(120), replaced.TimeSeconds)
	require.Equal(t, newBudget, replaced.TokenBudget)
}

func TestGoalToolCreateRejectsPlanMode(t *testing.T) {
	t.Parallel()

	sessions, runtime := newGoalSessionService(t)
	sess, err := sessions.Create(context.Background(), "goal-plan-blocked")
	require.NoError(t, err)
	_, err = sessions.UpdateCollaborationMode(context.Background(), sess.ID, session.CollaborationModePlan)
	require.NoError(t, err)

	tool := NewGoalTool(sessions, runtime)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, sess.ID)

	_, err = tool.Run(ctx, fantasy.ToolCall{
		ID:    "call-plan",
		Name:  GoalToolName,
		Input: `{"op":"create","objective":"Ship it"}`,
	})
	require.ErrorIs(t, err, session.ErrPlanBlocksGoalMode)
}

func TestGoalToolReplaceWithoutGoalActsLikeCreate(t *testing.T) {
	t.Parallel()

	sessions, runtime := newGoalSessionService(t)
	sess, err := sessions.Create(context.Background(), "goal-replace-empty")
	require.NoError(t, err)

	tool := NewGoalTool(sessions, runtime)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, sess.ID)

	budget := int64(50000)
	resp := runGoalTool(t, tool, ctx, GoalParams{Op: "replace", Objective: "Fresh goal", TokenBudget: &budget})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "Goal replaced")

	meta := parseGoalMetadata(t, resp)
	require.NotEmpty(t, meta.Goal.ID)
	require.Equal(t, "Fresh goal", meta.Goal.Text)
	require.Equal(t, session.GoalStatusActive, meta.Goal.Status)
	require.Equal(t, budget, meta.Goal.TokenBudget)
}
