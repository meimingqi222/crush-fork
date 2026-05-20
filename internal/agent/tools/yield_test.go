package tools

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func newTestServices(t *testing.T) (session.Service, message.Service) {
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	// Close the SQLite connection before TempDir cleanup so Windows can
	// remove the database file without hitting a file-lock error.
	t.Cleanup(func() { _ = conn.Close() })
	q := db.New(conn)
	sessions := session.NewService(q, nil)
	messages := message.NewService(q)
	return sessions, messages
}

func TestYieldToolReturnsYieldMetadata(t *testing.T) {
	t.Parallel()

	tool := NewYieldTool(nil)
	input, err := json.Marshal(YieldParams{
		Data:   "full result text",
		Status: "completed_with_warnings",
	})
	require.NoError(t, err)

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-1", Name: YieldToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	yield, ok := message.ParseToolResultYield(resp.Metadata)
	require.True(t, ok)
	require.Equal(t, "full result text", yield.Data)
	require.Equal(t, "completed_with_warnings", yield.Status)
}

func TestYieldToolRejectsEmptyData(t *testing.T) {
	t.Parallel()

	tool := NewYieldTool(nil)
	input, err := json.Marshal(YieldParams{Status: "completed"})
	require.NoError(t, err)

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-1", Name: YieldToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "data is required")
}

func TestYieldToolRejectsInvalidStatus(t *testing.T) {
	t.Parallel()

	tool := NewYieldTool(nil)
	input, err := json.Marshal(YieldParams{Data: "result", Status: "done"})
	require.NoError(t, err)

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-1", Name: YieldToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "status must be one of")
}

func TestYieldToolDefaultsEmptyStatusToCompleted(t *testing.T) {
	t.Parallel()

	tool := NewYieldTool(nil)
	input, err := json.Marshal(YieldParams{Data: "result"})
	require.NoError(t, err)

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-1", Name: YieldToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	yield, ok := message.ParseToolResultYield(resp.Metadata)
	require.True(t, ok)
	require.Equal(t, "completed", yield.Status)
}

func TestYieldToolRejectsDuplicateCalls(t *testing.T) {
	t.Parallel()

	sessions, messages := newTestServices(t)
	tool := NewYieldTool(messages)

	sess, err := sessions.Create(t.Context(), "Test")
	require.NoError(t, err)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, sess.ID)
	input, err := json.Marshal(YieldParams{Data: "first", Status: "completed"})
	require.NoError(t, err)

	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Name: YieldToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	// Simulate the agent runtime persisting the yield tool result.
	_, err = messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{Name: YieldToolName}.WithYield(message.ToolResultYield{
				Data:   "first",
				Status: "completed",
			}),
		},
	})
	require.NoError(t, err)

	// Second call should be rejected.
	resp, err = tool.Run(ctx, fantasy.ToolCall{ID: "call-2", Name: YieldToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "yield has already been called")
}
