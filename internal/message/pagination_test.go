package message

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/session"
)

func TestGetRetrySourceReturnsNearestPrecedingUserMessage(t *testing.T) {
	conn, err := db.Connect(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	queries := db.New(conn)
	sessions := session.NewService(queries, conn)
	messages := NewService(queries)
	sess, err := sessions.Create(t.Context(), "retry")
	if err != nil {
		t.Fatal(err)
	}
	_, err = messages.Create(t.Context(), sess.ID, CreateMessageParams{Role: User, Parts: []ContentPart{TextContent{Text: "first"}}})
	if err != nil {
		t.Fatal(err)
	}
	latest, err := messages.Create(t.Context(), sess.ID, CreateMessageParams{
		Role: User,
		Parts: []ContentPart{
			TextContent{Text: "retry this"},
			BinaryContent{Path: "image.png", MIMEType: "image/png", Data: []byte("image")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := messages.Create(t.Context(), sess.ID, CreateMessageParams{Role: Assistant, Parts: []ContentPart{TextContent{Text: "failed"}, Finish{Reason: FinishReasonError}}})
	if err != nil {
		t.Fatal(err)
	}
	source, err := messages.GetRetrySource(t.Context(), sess.ID, assistant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if source.ID != latest.ID || source.Content().Text != "retry this" || len(source.BinaryContent()) != 1 {
		t.Fatalf("unexpected retry source: %#v", source)
	}
	_, err = messages.GetRetrySource(t.Context(), sess.ID, latest.ID)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected user target to be rejected, got %v", err)
	}
}

func BenchmarkSessionMessagePage(b *testing.B) {
	ctx := context.Background()
	conn, err := db.Connect(ctx, b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = conn.Close() })
	queries := db.New(conn)
	sessions := session.NewService(queries, conn)
	messages := NewService(queries)
	sess, err := sessions.Create(ctx, "10,000 message pagination benchmark")
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

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		page, err := messages.ListBefore(ctx, sess.ID, nil, 201)
		if err != nil {
			b.Fatal(err)
		}
		if len(page) != 201 {
			b.Fatalf("expected 201 messages, got %d", len(page))
		}
	}
}
