package agent

import (
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

type testTranscriptRetainer struct {
	calls     int
	content   string
	sessionID string
	turnCount int
}

func (r *testTranscriptRetainer) RetainTranscript(_ context.Context, sessionID string, turnCount int, content string) error {
	r.calls++
	r.sessionID = sessionID
	r.turnCount = turnCount
	r.content = content
	return nil
}

func TestCoordinatorTranscriptAfterTurnRetainsEveryNTurns(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })

	cfg, err := config.Init(t.TempDir(), "", false)
	require.NoError(t, err)
	cfg.Config().Options.Memory = &config.MemoryConfig{Backend: "hindsight", RetainEveryNTurns: 2}

	queries := db.New(conn)
	messages := message.NewService(queries)
	sess, err := queries.CreateSession(t.Context(), db.CreateSessionParams{
		ID:    "sess-transcript",
		Title: "transcript",
		Kind:  "normal",
	})
	require.NoError(t, err)
	_, err = messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	})
	require.NoError(t, err)
	_, err = messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "hi"}},
	})
	require.NoError(t, err)

	eng := engine.New(conn, engine.Config{Enabled: true, Backend: "hindsight"})
	retainer := &testTranscriptRetainer{}
	eng.SetTranscriptRetainer(retainer)

	coord := &coordinator{
		cfg:                  cfg,
		messages:             messages,
		memoryEngine:         eng,
		transcriptTurnCounts: make(map[string]int),
	}

	coord.transcriptAfterTurn(t.Context(), sess.ID)
	require.Zero(t, retainer.calls)

	coord.transcriptAfterTurn(t.Context(), sess.ID)
	require.Equal(t, 1, retainer.calls)
	require.Equal(t, sess.ID, retainer.sessionID)
	require.Equal(t, 2, retainer.turnCount)
	require.Contains(t, retainer.content, "USER: hello")
	require.Contains(t, retainer.content, "ASSISTANT: hi")

	coord.clearTranscriptTurnCountForSession(sess.ID)
	require.Empty(t, coord.transcriptTurnCounts)
}
