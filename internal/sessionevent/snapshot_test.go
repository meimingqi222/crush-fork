package sessionevent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestSnapshotIsBoundedAndContainsRuntimeProjection(t *testing.T) {
	t.Parallel()

	sessions := &snapshotSessionReader{session: session.Session{
		ID:                "session-1",
		Title:             "Snapshot",
		Kind:              session.KindNormal,
		CollaborationMode: session.CollaborationModePlan,
		PermissionMode:    session.PermissionModeAuto,
		PromptTokens:      12,
		CompletionTokens:  34,
		MessageCount:      10_000,
	}}
	messages := &snapshotMessageReader{}
	for index := range SnapshotMessageLimit {
		messages.page = append(messages.page, message.Message{
			ID:        fmt.Sprintf("message-%d", index+9_980),
			SessionID: "session-1",
			Role:      message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: strings.Repeat("界", 300)},
				message.BinaryContent{MIMEType: "application/octet-stream", Data: []byte("secret-binary")},
			},
		})
	}
	resources := make([]ResourceSummary, SnapshotResourceLimit+5)
	for index := range resources {
		resources[index] = ResourceSummary{ID: fmt.Sprintf("resource-%d", index), Status: "connected"}
	}
	runtime := fixedRuntimeSource{projection: RuntimeSnapshot{
		Busy:         true,
		ActiveTurnID: "turn-9",
		QueueCount:   3,
		QueuePaused:  true,
		Model:        "gpt-test",
		Provider:     "mock",
		MCPServers:   resources,
		Terminals:    resources,
	}}
	hub := NewHub(Config{})
	_, err := hub.Publish("session-1", NewEvent{SessionRevision: 7, Kind: KindSessionUpdated})
	require.NoError(t, err)
	_, err = hub.Publish("session-1", NewEvent{Kind: KindMessageDelta})
	require.NoError(t, err)

	snapshot, err := NewSnapshotService(sessions, messages, runtime, hub).Snapshot(t.Context(), "session-1")
	require.NoError(t, err)
	require.Equal(t, 1, sessions.calls)
	require.Equal(t, 1, messages.calls)
	require.Equal(t, SnapshotMessageLimit, messages.limit)
	require.Len(t, snapshot.Messages, SnapshotMessageLimit)
	require.Equal(t, int64(10_000), snapshot.Session.MessageCount)
	require.Equal(t, "running", snapshot.Status)
	require.NotNil(t, snapshot.ActiveTurn)
	// Regression: a busy session used to always produce activeTurn.id == "",
	// which the gui-acp client correctly rejects as an invalid snapshot,
	// producing an unbreakable prepare-fail retry loop for the whole turn.
	require.NotEmpty(t, snapshot.ActiveTurn.ID)
	require.Equal(t, "turn-9", snapshot.ActiveTurn.ID)
	require.Equal(t, QueueSummary{Count: 3, Paused: true}, snapshot.Queue)
	require.Equal(t, InferenceConfig{Model: "gpt-test", Provider: "mock"}, snapshot.EffectiveConfig)
	require.Len(t, snapshot.MCPServers, SnapshotResourceLimit)
	require.Len(t, snapshot.Terminals, SnapshotResourceLimit)
	require.Equal(t, uint64(2), snapshot.LatestSequence)
	require.Equal(t, uint64(7), snapshot.SessionRevision)
	require.True(t, snapshot.Messages[0].PreviewTruncated)
	require.LessOrEqual(t, len(snapshot.Messages[0].Preview), SnapshotPreviewBytes)
	require.True(t, utf8.ValidString(snapshot.Messages[0].Preview))
	require.Equal(t, 1, snapshot.Messages[0].AttachmentCount)

	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "secret-binary")
	require.NotContains(t, string(raw), "application/octet-stream")
}

func TestSnapshotUsesOneBoundedMessageQueryForEmptySession(t *testing.T) {
	t.Parallel()

	messages := &snapshotMessageReader{}
	snapshot, err := NewSnapshotService(
		&snapshotSessionReader{session: session.Session{ID: "empty", Kind: session.KindNormal}},
		messages,
		nil,
		nil,
	).Snapshot(t.Context(), "empty")
	require.NoError(t, err)
	require.Equal(t, 1, messages.calls)
	require.Empty(t, snapshot.Messages)
	require.NotNil(t, snapshot.MCPServers)
	require.NotNil(t, snapshot.Terminals)
}

func TestSnapshotProjectsPersistedTimestampsAsMilliseconds(t *testing.T) {
	t.Parallel()

	snapshot, err := NewSnapshotService(
		&snapshotSessionReader{session: session.Session{
			ID: "session", Kind: session.KindNormal, CreatedAt: 1_783_910_000, UpdatedAt: 1_783_910_123,
		}},
		&snapshotMessageReader{page: []message.Message{{
			ID: "message", CreatedAt: 1_783_910_456, UpdatedAt: 1_783_910_789,
		}}},
		nil,
		nil,
	).Snapshot(t.Context(), "session")
	require.NoError(t, err)
	require.Equal(t, int64(1_783_910_000_000), snapshot.Session.CreatedAt)
	require.Equal(t, int64(1_783_910_123_000), snapshot.Session.UpdatedAt)
	require.Equal(t, int64(1_783_910_456_000), snapshot.Messages[0].CreatedAt)
	require.Equal(t, int64(1_783_910_789_000), snapshot.Messages[0].UpdatedAt)
}

func TestSnapshotActiveDraftMatchesSequenceCutAndIsBounded(t *testing.T) {
	t.Parallel()

	hub := NewHub(Config{})
	t.Cleanup(hub.Close)
	_, err := hub.Publish("session", NewEvent{
		Kind: KindMessageCreated, Payload: MessageEvent{MessageID: "message"},
	})
	require.NoError(t, err)
	oversized := strings.Repeat("a", SnapshotDraftBytes-1) + "界"
	_, err = hub.Publish("session", NewEvent{
		Kind: KindMessageDelta, Payload: TextDelta{MessageID: "message", Text: oversized},
	})
	require.NoError(t, err)
	_, err = hub.Publish("session", NewEvent{
		Kind: KindUsageUpdated, Payload: UsageEvent{OutputTokens: 1},
	})
	require.NoError(t, err)

	snapshot, err := NewSnapshotService(
		&snapshotSessionReader{session: session.Session{ID: "session", Kind: session.KindNormal}},
		&snapshotMessageReader{},
		fixedRuntimeSource{projection: RuntimeSnapshot{Busy: true}},
		hub,
	).Snapshot(t.Context(), "session")
	require.NoError(t, err)
	require.NotNil(t, snapshot.ActiveTurn)
	require.Equal(t, "message", snapshot.ActiveTurn.MessageID)
	require.NotNil(t, snapshot.ActiveTurn.Draft)
	require.Equal(t, snapshot.LatestSequence, snapshot.ActiveTurn.Draft.CapturedSequence)
	require.True(t, snapshot.ActiveTurn.Draft.Truncated)
	require.Len(t, snapshot.ActiveTurn.Draft.Text, SnapshotDraftBytes-1)
	require.True(t, utf8.ValidString(snapshot.ActiveTurn.Draft.Text))
}

func TestSnapshotReadsOnlyTheIndexedMessageTail(t *testing.T) {
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	sessions := session.NewService(db.New(conn), conn)
	messages := message.NewService(db.New(conn))
	sess, err := sessions.Create(t.Context(), "Tail")
	require.NoError(t, err)
	tx, err := conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	for index := range 25 {
		_, err = tx.ExecContext(t.Context(), `INSERT INTO messages
			(id, session_id, role, parts, model, provider, created_at, updated_at)
			VALUES (?, ?, 'assistant', '[]', 'test-model', 'test-provider', ?, ?)`,
			fmt.Sprintf("message-%08d", index), sess.ID, index, index)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())

	snapshot, err := NewSnapshotService(sessions, messages, nil, nil).Snapshot(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, snapshot.Messages, SnapshotMessageLimit)
	require.Equal(t, "message-00000005", snapshot.Messages[0].ID)
	require.Equal(t, "message-00000024", snapshot.Messages[19].ID)
	require.Equal(t, int64(25), snapshot.Session.MessageCount)
}

func BenchmarkSessionSnapshot(b *testing.B) {
	ctx := context.Background()
	conn, err := db.Connect(ctx, b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = conn.Close() })
	sessions := session.NewService(db.New(conn), conn)
	messages := message.NewService(db.New(conn))
	sess, err := sessions.Create(ctx, "10,000 message benchmark")
	if err != nil {
		b.Fatal(err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	for index := range 10_000 {
		_, err = tx.ExecContext(ctx, `INSERT INTO messages
			(id, session_id, role, parts, model, provider, created_at, updated_at)
			VALUES (?, ?, 'assistant', '[]', 'benchmark-model', 'benchmark-provider', ?, ?)`,
			fmt.Sprintf("message-%08d", index), sess.ID, index, index)
		if err != nil {
			_ = tx.Rollback()
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	service := NewSnapshotService(sessions, messages, nil, NewHub(Config{}))

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := service.Snapshot(ctx, sess.ID); err != nil {
			b.Fatal(err)
		}
	}
}

type snapshotSessionReader struct {
	session session.Session
	err     error
	calls   int
}

func (r *snapshotSessionReader) Get(context.Context, string) (session.Session, error) {
	r.calls++
	return r.session, r.err
}

type snapshotMessageReader struct {
	page  []message.Message
	err   error
	calls int
	limit int
}

func (r *snapshotMessageReader) ListRecent(_ context.Context, _ string, limit int) ([]message.Message, error) {
	r.calls++
	r.limit = limit
	return append([]message.Message(nil), r.page...), r.err
}

type fixedRuntimeSource struct{ projection RuntimeSnapshot }

func (s fixedRuntimeSource) SnapshotRuntime(string) RuntimeSnapshot { return s.projection }
