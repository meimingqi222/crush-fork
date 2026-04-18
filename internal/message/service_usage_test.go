package message

import (
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestServiceUpdatePersistsUsage(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(context.Background(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = conn.Close()
	})

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := NewService(q)

	sess, err := sessions.Create(t.Context(), "usage")
	require.NoError(t, err)

	msg, err := messages.Create(t.Context(), sess.ID, CreateMessageParams{
		Role:  Assistant,
		Parts: []ContentPart{TextContent{Text: "hello"}},
		Model: "claude-sonnet-4-5",
	})
	require.NoError(t, err)

	msg.SetUsage(Usage{
		InputTokens:      1200,
		OutputTokens:     320,
		ReasoningTokens:  80,
		CacheReadTokens:  6400,
		CacheWriteTokens: 2400,
	})
	msg.AddFinish(FinishReasonEndTurn, "", "")
	require.NoError(t, messages.Update(t.Context(), msg))

	reloaded, err := messages.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Equal(t, msg.Usage, reloaded.Usage)
	require.Equal(t, int64(10400), reloaded.Usage.TotalTokens())
}
