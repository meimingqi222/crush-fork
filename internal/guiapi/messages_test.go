package guiapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/sessionevent"
	"github.com/stretchr/testify/require"
)

func TestMessagePaginationStableDuringInsertAndDelete(t *testing.T) {
	t.Parallel()

	env := newContentTestEnvironment(t)
	insertRawMessages(t, env, 250, 1000)
	service := negotiatedContentService(t, env)

	first := callMessages(t, service, messagesParams{SessionID: env.session.ID})
	require.Len(t, first.Messages, defaultMessagePageLimit)
	require.True(t, first.HasMore)
	require.NotEmpty(t, first.NextCursor)
	for index := 1; index < len(first.Messages); index++ {
		require.Greater(t, first.Messages[index-1].ID, first.Messages[index].ID)
	}

	insertRawMessage(t, env, "newer-after-first-page", 2000, "newer")
	boundaryID := first.Messages[len(first.Messages)-1].ID
	_, err := env.conn.ExecContext(t.Context(), "DELETE FROM messages WHERE id = ?", boundaryID)
	require.NoError(t, err)

	seen := make(map[string]struct{}, 250)
	for _, item := range first.Messages {
		seen[item.ID] = struct{}{}
	}
	cursor := first.NextCursor
	for cursor != "" {
		page := callMessages(t, service, messagesParams{SessionID: env.session.ID, Limit: 37, BeforeCursor: cursor})
		for _, item := range page.Messages {
			_, duplicate := seen[item.ID]
			require.False(t, duplicate, "duplicate message %s", item.ID)
			require.NotEqual(t, "newer-after-first-page", item.ID)
			seen[item.ID] = struct{}{}
		}
		cursor = page.NextCursor
		if !page.HasMore {
			require.Empty(t, cursor)
		}
	}
	require.Len(t, seen, 250)
}

func TestMessagePaginationLimitCursorValidationAndRedaction(t *testing.T) {
	t.Parallel()

	env := newContentTestEnvironment(t)
	insertRawMessages(t, env, 250, 1000)
	service := negotiatedContentService(t, env)

	maximum := callMessages(t, service, messagesParams{SessionID: env.session.ID, Limit: 999})
	require.Len(t, maximum.Messages, maxMessagePageLimit)
	_, rpcErr := service.HandleExtension(t.Context(), "crush/session/messages", mustRawJSON(t, messagesParams{SessionID: env.session.ID, Limit: -1}))
	require.NotNil(t, rpcErr)
	_, rpcErr = service.HandleExtension(t.Context(), "crush/session/messages", mustRawJSON(t, messagesParams{SessionID: env.session.ID, BeforeCursor: "not-base64"}))
	require.NotNil(t, rpcErr)

	other, err := env.sessions.Create(t.Context(), "Other")
	require.NoError(t, err)
	_, rpcErr = service.HandleExtension(t.Context(), "crush/session/messages", mustRawJSON(t, messagesParams{SessionID: other.ID, BeforeCursor: maximum.NextCursor}))
	require.NotNil(t, rpcErr)
	require.Contains(t, rpcErr.Message, "beforeCursor")

	sensitive, err := env.messages.Create(t.Context(), env.session.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: strings.Repeat("界", 30_000)},
			message.ReasoningContent{Thinking: "hidden reasoning", Signature: "reasoning-secret", ThoughtSignature: "thought-secret"},
			message.ImageURLContent{URL: "https://example.invalid/image?token=signed-secret"},
			message.BinaryContent{Path: "C:/secret/path", MIMEType: "application/pdf", Data: []byte("binary-secret")},
			message.ToolCall{ID: "tool-1", Name: "read", Input: "tool-input-secret", Finished: true},
			message.ToolResult{ToolCallID: "tool-1", Name: "read", Content: "tool-result-secret", Metadata: "tool-metadata-secret"},
			message.Finish{Reason: message.FinishReasonEndTurn},
		},
	})
	require.NoError(t, err)
	page := callMessages(t, service, messagesParams{SessionID: env.session.ID, Limit: 1})
	require.Equal(t, sensitive.ID, page.Messages[0].ID)
	require.True(t, page.Messages[0].TextTruncated)
	require.LessOrEqual(t, len(page.Messages[0].Text), maxMessageTextBytes)
	require.True(t, utf8.ValidString(page.Messages[0].Text))
	require.True(t, page.Messages[0].HasReasoning)
	require.Len(t, page.Messages[0].Attachments, 2)
	require.Equal(t, 1, page.Messages[0].ToolResultCount)

	raw, err := json.Marshal(page)
	require.NoError(t, err)
	for _, secret := range []string{
		"hidden reasoning", "reasoning-secret", "thought-secret", "signed-secret",
		"binary-secret", "C:/secret/path", "tool-input-secret", "tool-result-secret", "tool-metadata-secret",
	} {
		require.NotContains(t, string(raw), secret)
	}
}

func TestSessionSearchIsBoundedAndCursorScoped(t *testing.T) {
	t.Parallel()

	env := newContentTestEnvironment(t)
	for index := range 35 {
		parts := fmt.Sprintf(`[{"type":"text","data":{"text":"needle result %03d %s"}}]`, index, strings.Repeat("x", 800))
		insertRawMessageWithParts(t, env, fmt.Sprintf("search-%03d", index), 1000+int64(index), parts)
	}
	service := negotiatedContentService(t, env)
	result := callSearch(t, service, searchParams{Query: "needle", SessionID: env.session.ID, Limit: 10})
	require.Len(t, result.Results, 10)
	require.True(t, result.HasMore)
	require.NotEmpty(t, result.NextCursor)
	for _, hit := range result.Results {
		require.LessOrEqual(t, len(hit.Preview), maxSearchPreviewBytes)
		require.True(t, hit.PreviewTruncated)
	}

	next := callSearch(t, service, searchParams{Query: "needle", SessionID: env.session.ID, Limit: 10, Cursor: result.NextCursor})
	firstIDs := make(map[string]struct{}, len(result.Results))
	for _, hit := range result.Results {
		firstIDs[hit.MessageID] = struct{}{}
	}
	for _, hit := range next.Results {
		_, duplicate := firstIDs[hit.MessageID]
		require.False(t, duplicate)
	}

	_, rpcErr := service.HandleExtension(t.Context(), "crush/session/search", mustRawJSON(t, searchParams{
		Query: "different", SessionID: env.session.ID, Cursor: result.NextCursor,
	}))
	require.NotNil(t, rpcErr)
	require.Contains(t, rpcErr.Message, "cursor")
}

func TestMessagePaginationQueryUsesCompositeIndex(t *testing.T) {
	t.Parallel()

	env := newContentTestEnvironment(t)
	rows, err := env.conn.QueryContext(t.Context(), `EXPLAIN QUERY PLAN
		SELECT * FROM messages
		WHERE session_id = ?
		  AND (? = 0 OR created_at < ? OR (created_at = ? AND id < ?))
		ORDER BY created_at DESC, id DESC LIMIT ?`, env.session.ID, 1, 1000, 1000, "message-00000100", 200)
	require.NoError(t, err)
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	plan := strings.Join(details, "\n")
	require.Contains(t, plan, "idx_messages_session_recent")
	require.NotContains(t, plan, "USE TEMP B-TREE")
}

func TestMessagePaginationMapsMissingSessionAndRedactsSourceFailure(t *testing.T) {
	t.Parallel()

	env := newContentTestEnvironment(t)
	service := negotiatedContentService(t, env)
	_, rpcErr := service.HandleExtension(t.Context(), "crush/session/messages", mustRawJSON(t, messagesParams{SessionID: "missing"}))
	require.Equal(t, errorSessionNotFound, rpcErr.Message)

	service.SetSessionContentSources(env.sessions, failingMessagePageReader{}, env.history)
	_, rpcErr = service.HandleExtension(t.Context(), "crush/session/messages", mustRawJSON(t, messagesParams{SessionID: env.session.ID}))
	require.Equal(t, errorQueryFailed, rpcErr.Message)
	require.Equal(t, errorQueryFailed, rpcErr.Data.(ErrorData).Code)
	require.True(t, rpcErr.Data.(ErrorData).Retryable)
	require.NotContains(t, rpcErr.Message, "database path secret")
}

type contentTestEnvironment struct {
	conn     *sql.DB
	db       *sql.DB
	sessions session.Service
	messages message.Service
	history  history.Service
	session  session.Session
}

func newContentTestEnvironment(t *testing.T) *contentTestEnvironment {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	queries := db.New(conn)
	sessions := session.NewService(queries, conn)
	sess, err := sessions.Create(t.Context(), "Pagination")
	require.NoError(t, err)
	return &contentTestEnvironment{
		conn: conn, db: conn, sessions: sessions, messages: message.NewService(queries),
		history: history.NewService(queries, conn), session: sess,
	}
}

func negotiatedContentService(t *testing.T, env *contentTestEnvironment) *Service {
	t.Helper()
	service := NewService(sessionevent.NewHub(sessionevent.Config{}))
	service.SetSessionContentSources(env.sessions, env.messages, env.history)
	require.Nil(t, service.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion,
		Features:        []Feature{FeatureSessionSync, FeatureSessionControl},
	})))
	t.Cleanup(service.Close)
	return service
}

func callMessages(t *testing.T, service *Service, params messagesParams) messagesResult {
	t.Helper()
	result, rpcErr := service.HandleExtension(t.Context(), "crush/session/messages", mustRawJSON(t, params))
	require.Nil(t, rpcErr)
	return result.(messagesResult)
}

func callSearch(t *testing.T, service *Service, params searchParams) searchResult {
	t.Helper()
	result, rpcErr := service.HandleExtension(t.Context(), "crush/session/search", mustRawJSON(t, params))
	require.Nil(t, rpcErr)
	return result.(searchResult)
}

func insertRawMessages(t *testing.T, env *contentTestEnvironment, count int, createdAt int64) {
	t.Helper()
	tx, err := env.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	for index := range count {
		_, err = tx.ExecContext(t.Context(), `INSERT INTO messages
			(id, session_id, role, parts, model, provider, created_at, updated_at)
			VALUES (?, ?, 'assistant', '[]', 'model', 'provider', ?, ?)`,
			fmt.Sprintf("message-%08d", index), env.session.ID, createdAt, createdAt)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
}

func insertRawMessage(t *testing.T, env *contentTestEnvironment, id string, createdAt int64, text string) {
	t.Helper()
	parts := fmt.Sprintf(`[{"type":"text","data":{"text":%q}}]`, text)
	insertRawMessageWithParts(t, env, id, createdAt, parts)
}

func insertRawMessageWithParts(t *testing.T, env *contentTestEnvironment, id string, createdAt int64, parts string) {
	t.Helper()
	_, err := env.db.ExecContext(t.Context(), `INSERT INTO messages
		(id, session_id, role, parts, model, provider, created_at, updated_at)
		VALUES (?, ?, 'assistant', ?, 'model', 'provider', ?, ?)`, id, env.session.ID, parts, createdAt, createdAt)
	require.NoError(t, err)
}

type failingMessagePageReader struct{}

func (failingMessagePageReader) ListBefore(context.Context, string, *message.PageCursor, int) ([]message.Message, error) {
	return nil, errors.New("database path secret")
}
