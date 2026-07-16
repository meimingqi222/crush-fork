package session

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestDesktopFlagsPersistWithoutChangingExistingSessionModes(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	service := NewService(db.New(conn), conn)

	created, err := service.Create(t.Context(), "desktop")
	require.NoError(t, err)
	created, err = service.UpdatePermissionMode(t.Context(), created.ID, PermissionModeYolo)
	require.NoError(t, err)

	archived, err := service.SetArchived(t.Context(), created.ID, true)
	require.NoError(t, err)
	require.True(t, archived.Archived)
	require.False(t, archived.Pinned)
	require.Equal(t, PermissionModeYolo, archived.PermissionMode)

	pinned, err := service.SetPinned(t.Context(), created.ID, true)
	require.NoError(t, err)
	require.True(t, pinned.Archived)
	require.True(t, pinned.Pinned)
	require.Equal(t, PermissionModeYolo, pinned.PermissionMode)

	loaded, err := service.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, loaded.Archived)
	require.True(t, loaded.Pinned)
	require.Equal(t, PermissionModeYolo, loaded.PermissionMode)
}

func TestInferenceOverridesPersistAndUseCompareAndSwapRevision(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	service := NewService(db.New(conn), conn)
	created, err := service.Create(t.Context(), "inference")
	require.NoError(t, err)

	maxTokens := int64(4096)
	temperature := 0.25
	updated, err := service.UpdateInference(t.Context(), created.ID, 0, InferenceOverrides{
		Model: "model-a", Provider: "provider-a", MaxOutputTokens: &maxTokens, Temperature: &temperature,
	})
	require.NoError(t, err)
	require.Equal(t, uint64(1), updated.InferenceRevision)
	require.Equal(t, "model-a", updated.Inference.Model)
	require.Equal(t, int64(4096), *updated.Inference.MaxOutputTokens)

	var wait sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wait.Go(func() {
			_, updateErr := service.UpdateInference(t.Context(), created.ID, 1, InferenceOverrides{})
			errs <- updateErr
		})
	}
	wait.Wait()
	close(errs)
	success, conflicts := 0, 0
	for updateErr := range errs {
		switch {
		case updateErr == nil:
			success++
		case errors.Is(updateErr, ErrInferenceConflict):
			conflicts++
		default:
			require.NoError(t, updateErr)
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, conflicts)
	loaded, err := service.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, uint64(2), loaded.InferenceRevision)
	require.Equal(t, InferenceOverrides{}, loaded.Inference)
}

func TestForkUsesCompletedTurnBoundaryAndSupportsWholeSession(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	queries := db.New(conn)
	sessions := NewService(queries, conn)
	messages := message.NewService(queries)

	source, err := sessions.Create(t.Context(), "source")
	require.NoError(t, err)
	firstUser := createDesktopMessage(t, messages, source.ID, message.User, "first")
	createDesktopMessage(t, messages, source.ID, message.Assistant, "first answer")
	createDesktopMessage(t, messages, source.ID, message.User, "second")
	createDesktopMessage(t, messages, source.ID, message.Assistant, "second answer")

	bounded, err := sessions.Fork(t.Context(), source.ID, firstUser.ID)
	require.NoError(t, err)
	boundedMessages, err := messages.List(t.Context(), bounded.ID)
	require.NoError(t, err)
	require.Len(t, boundedMessages, 2)
	require.Equal(t, []string{"first", "first answer"}, []string{boundedMessages[0].Content().Text, boundedMessages[1].Content().Text})
	for _, item := range boundedMessages {
		require.NoError(t, uuid.Validate(item.ID))
	}

	whole, err := sessions.Fork(t.Context(), source.ID, "")
	require.NoError(t, err)
	wholeMessages, err := messages.List(t.Context(), whole.ID)
	require.NoError(t, err)
	require.Len(t, wholeMessages, 4)

	_, err = sessions.Fork(t.Context(), source.ID, "not-in-source")
	require.Error(t, err)
}

func createDesktopMessage(t *testing.T, service message.Service, sessionID string, role message.MessageRole, text string) message.Message {
	t.Helper()
	created, err := service.Create(t.Context(), sessionID, message.CreateMessageParams{
		Role: role, Parts: []message.ContentPart{message.TextContent{Text: text}},
	})
	require.NoError(t, err)
	return created
}

func BenchmarkSessionFork10K(b *testing.B) {
	conn, err := db.Connect(b.Context(), b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = conn.Close() })
	service := NewService(db.New(conn), conn)
	source, err := service.Create(b.Context(), "10k fork")
	if err != nil {
		b.Fatal(err)
	}
	tx, err := conn.BeginTx(b.Context(), nil)
	if err != nil {
		b.Fatal(err)
	}
	for index := range 10_000 {
		role := "assistant"
		if index%2 == 0 {
			role = "user"
		}
		_, err = tx.ExecContext(b.Context(), `INSERT INTO messages
			(id, session_id, role, parts, created_at, updated_at)
			VALUES (?, ?, ?, '[]', ?, ?)`, fmt.Sprintf("source-%08d", index), source.ID, role, index, index)
		if err != nil {
			_ = tx.Rollback()
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		forked, err := service.Fork(b.Context(), source.ID, "")
		if err != nil {
			b.Fatal(err)
		}
		if forked.MessageCount != 10_000 {
			b.Fatalf("expected 10,000 copied messages, got %d", forked.MessageCount)
		}
	}
}
