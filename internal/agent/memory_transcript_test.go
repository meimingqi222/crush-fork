package agent

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestBuildTranscriptWindow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		msgs      []message.Message
		wantEmpty bool
	}{
		{
			name:      "empty messages",
			msgs:      nil,
			wantEmpty: true,
		},
		{
			name: "user message only",
			msgs: []message.Message{
				newTestMessage("test-session", "msg-1", message.User, "Hello world"),
			},
			wantEmpty: false,
		},
		{
			name: "assistant message only",
			msgs: []message.Message{
				newTestMessage("test-session", "msg-1", message.Assistant, "Hi there!"),
			},
			wantEmpty: false,
		},
		{
			name: "mixed messages",
			msgs: []message.Message{
				newTestMessage("test-session", "msg-1", message.User, "Hello"),
				newTestMessage("test-session", "msg-2", message.Assistant, "Hi!"),
				newTestMessage("test-session", "msg-3", message.User, "How are you?"),
			},
			wantEmpty: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildTranscriptWindow(tt.msgs)
			if tt.wantEmpty {
				require.Empty(t, result)
			} else {
				require.NotEmpty(t, result)
			}
		})
	}
}

func TestBuildTranscriptWindowTruncation(t *testing.T) {
	t.Parallel()

	// Create messages that would exceed the limit.
	var msgs []message.Message
	for i := 0; i < 1000; i++ {
		msgs = append(msgs, newTestMessage(
			"test-session",
			fmt.Sprintf("msg-%d", i),
			message.User,
			"This is a test message that is reasonably long to test truncation. ",
		))
	}

	result := buildTranscriptWindow(msgs)
	require.NotEmpty(t, result)
	require.LessOrEqual(t, len([]rune(result)), transcriptWindowMaxRunes)
}

func TestRetainTranscriptWindow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newTestEventStore(t)
	sessionID := "test-session-1"

	msgs := []message.Message{
		newTestMessage(sessionID, "msg-1", message.User, "Hello"),
		newTestMessage(sessionID, "msg-2", message.Assistant, "Hi there!"),
	}

	err := retainTranscriptWindow(ctx, store, sessionID, msgs, 1)
	require.NoError(t, err)

	// Verify the event was stored.
	events, err := store.Query(ctx, engine.EventFilter{
		SessionID: &sessionID,
		Limit:     10,
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, engine.MemoryScopeSession, events[0].Scope)
	require.Equal(t, engine.MemoryKindReference, events[0].Kind)
	require.Contains(t, events[0].Tags, transcriptMemoryKind)
}

func TestBuildTranscriptRecallBlock(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newTestEventStore(t)
	sessionID := "test-session-2"

	// No events initially.
	result := buildTranscriptRecallBlock(ctx, store, sessionID)
	require.Empty(t, result)

	// Store a transcript window.
	msgs := []message.Message{
		newTestMessage(sessionID, "msg-1", message.User, "Test query"),
		newTestMessage(sessionID, "msg-2", message.Assistant, "Test response"),
	}
	err := retainTranscriptWindow(ctx, store, sessionID, msgs, 1)
	require.NoError(t, err)

	// Now should return content.
	result = buildTranscriptRecallBlock(ctx, store, sessionID)
	require.NotEmpty(t, result)
	require.Contains(t, result, "<transcript_context>")
	require.Contains(t, result, "</transcript_context>")
}

func TestFormatTranscriptRecall(t *testing.T) {
	t.Parallel()

	require.Empty(t, formatTranscriptRecall(""))
	require.Contains(t, formatTranscriptRecall("test content"), "<transcript_context>")
	require.Contains(t, formatTranscriptRecall("test content"), "test content")
	require.Contains(t, formatTranscriptRecall("test content"), "</transcript_context>")
}

// newTestEventStore creates an in-memory event store for testing.
func newTestEventStore(t *testing.T) engine.EventStore {
	t.Helper()
	db := newTestDB(t)
	return engine.NewSQLiteEventStore(db)
}

// newTestDB creates an in-memory SQLite database for testing.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	// Create schema.
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS memory_events (
			id TEXT PRIMARY KEY,
			session_id TEXT,
			scope TEXT,
			kind TEXT,
			content TEXT,
			summary TEXT,
			source_json TEXT,
			source_hash TEXT,
			confidence REAL,
			importance REAL,
			supersedes TEXT,
			tags_json TEXT,
			watermark INTEGER,
			created_at INTEGER,
			updated_at INTEGER,
			expires_at INTEGER
		);
		CREATE INDEX IF NOT EXISTS idx_memory_events_session ON memory_events (session_id);
	`)
	require.NoError(t, err)

	return db
}

// newTestMessage creates a test message.
func newTestMessage(sessionID, id string, role message.MessageRole, text string) message.Message {
	now := time.Now().Unix()
	return message.Message{
		ID:        id,
		SessionID: sessionID,
		Role:      role,
		CreatedAt: now,
		UpdatedAt: now,
		Parts:     []message.ContentPart{message.TextContent{Text: text}},
	}
}
