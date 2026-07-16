package guiapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/acp"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

const (
	defaultMessagePageLimit = 50
	maxMessagePageLimit     = 200
	maxMessageTextBytes     = 64 * 1024
	maxPageTextBytes        = 1024 * 1024
	maxCursorBytes          = 2048
	defaultSearchLimit      = 20
	maxSearchLimit          = 100
	maxSearchPreviewBytes   = 512
)

const errorQueryFailed = "CRUSH_QUERY_FAILED"

// SessionReader verifies session ownership/existence for content requests.
type SessionReader interface {
	Get(context.Context, string) (session.Session, error)
}

// MessagePageReader provides indexed reverse-chronological keyset pages.
type MessagePageReader interface {
	ListBefore(context.Context, string, *message.PageCursor, int) ([]message.Message, error)
}

// MessageSearchReader provides bounded keyset search results.
type MessageSearchReader interface {
	SearchMessagesPage(context.Context, history.SearchParams) ([]history.MessageSearchResult, error)
}

type messagesParams struct {
	SessionID    string `json:"sessionId"`
	Limit        int    `json:"limit,omitempty"`
	BeforeCursor string `json:"beforeCursor,omitempty"`
}

type messagesResult struct {
	Messages   []pageMessage `json:"messages"`
	NextCursor string        `json:"nextCursor,omitempty"`
	HasMore    bool          `json:"hasMore"`
}

type pageMessage struct {
	ID              string               `json:"id"`
	Role            string               `json:"role"`
	Text            string               `json:"text,omitempty"`
	TextTruncated   bool                 `json:"textTruncated,omitempty"`
	HasReasoning    bool                 `json:"hasReasoning,omitempty"`
	Attachments     []attachmentMetadata `json:"attachments"`
	ToolCalls       []toolCallMetadata   `json:"toolCalls"`
	ToolResultCount int                  `json:"toolResultCount,omitempty"`
	FinishReason    string               `json:"finishReason,omitempty"`
	Model           string               `json:"model,omitempty"`
	Provider        string               `json:"provider,omitempty"`
	Usage           usageMetadata        `json:"usage"`
	CreatedAt       int64                `json:"createdAt"`
	UpdatedAt       int64                `json:"updatedAt"`
}

type attachmentMetadata struct {
	Kind     string `json:"kind"`
	MIMEType string `json:"mimeType,omitempty"`
}

type toolCallMetadata struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Finished bool   `json:"finished"`
}

type usageMetadata struct {
	InputTokens      int64 `json:"inputTokens,omitempty"`
	OutputTokens     int64 `json:"outputTokens,omitempty"`
	ReasoningTokens  int64 `json:"reasoningTokens,omitempty"`
	CacheReadTokens  int64 `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`
}

type searchParams struct {
	Query     string `json:"query"`
	SessionID string `json:"sessionId,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type searchResult struct {
	Results    []searchHit `json:"results"`
	NextCursor string      `json:"nextCursor,omitempty"`
	HasMore    bool        `json:"hasMore"`
}

type searchHit struct {
	Kind             string `json:"kind"`
	MessageID        string `json:"messageId"`
	SessionID        string `json:"sessionId"`
	Role             string `json:"role"`
	Preview          string `json:"preview"`
	PreviewTruncated bool   `json:"previewTruncated,omitempty"`
	CreatedAt        int64  `json:"createdAt"`
}

type opaqueCursor struct {
	Version   int    `json:"v"`
	Kind      string `json:"kind"`
	SessionID string `json:"sessionId,omitempty"`
	Scope     string `json:"scope,omitempty"`
	CreatedAt int64  `json:"createdAt"`
	ID        string `json:"id"`
}

func (s *Service) SetSessionContentSources(sessions SessionReader, messages MessagePageReader, search MessageSearchReader) {
	s.mu.Lock()
	s.sessions = sessions
	s.messagePages = messages
	s.messageSearch = search
	s.mu.Unlock()
}

func (s *Service) registerMessageHandlers() {
	s.routes["crush/session/messages"] = route{feature: FeatureSessionSync, handler: s.handleMessages}
	s.routes["crush/session/search"] = route{feature: FeatureSessionControl, handler: s.handleSearch}
}

func (s *Service) handleMessages(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params messagesParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if params.SessionID == "" {
		return nil, invalidParams(errors.New("sessionId is required"))
	}
	limit, err := boundedLimit(params.Limit, defaultMessagePageLimit, maxMessagePageLimit)
	if err != nil {
		return nil, invalidParams(err)
	}

	s.mu.RLock()
	sessions := s.sessions
	pages := s.messagePages
	s.mu.RUnlock()
	if sessions == nil || pages == nil {
		return nil, sourceUnavailable("message pagination service is unavailable")
	}
	if _, err := sessions.Get(ctx, params.SessionID); err != nil {
		return nil, sessionSourceError(params.SessionID, err)
	}

	var before *message.PageCursor
	if params.BeforeCursor != "" {
		cursor, err := decodeCursor(params.BeforeCursor, "messages", params.SessionID, "")
		if err != nil {
			return nil, invalidParams(fmt.Errorf("invalid beforeCursor: %w", err))
		}
		before = &message.PageCursor{CreatedAt: cursor.CreatedAt, ID: cursor.ID}
	}
	items, err := pages.ListBefore(ctx, params.SessionID, before, limit+1)
	if err != nil {
		return nil, sourceFailure()
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	result := messagesResult{Messages: make([]pageMessage, len(items)), HasMore: hasMore}
	remainingText := maxPageTextBytes
	for index := range items {
		result.Messages[index], remainingText = projectPageMessage(&items[index], remainingText)
	}
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		result.NextCursor = encodeCursor(opaqueCursor{Version: 1, Kind: "messages", SessionID: params.SessionID, CreatedAt: last.CreatedAt, ID: last.ID})
	}
	return result, nil
}

func (s *Service) handleSearch(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params searchParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	query := strings.TrimSpace(params.Query)
	if query == "" {
		return nil, invalidParams(errors.New("query is required"))
	}
	limit, err := boundedLimit(params.Limit, defaultSearchLimit, maxSearchLimit)
	if err != nil {
		return nil, invalidParams(err)
	}
	scope := searchScope(query, params.SessionID)

	s.mu.RLock()
	sessions := s.sessions
	search := s.messageSearch
	s.mu.RUnlock()
	if search == nil {
		return nil, sourceUnavailable("session search service is unavailable")
	}
	if params.SessionID != "" {
		if sessions == nil {
			return nil, sourceUnavailable("session service is unavailable")
		}
		if _, err := sessions.Get(ctx, params.SessionID); err != nil {
			return nil, sessionSourceError(params.SessionID, err)
		}
	}
	searchRequest := history.SearchParams{Query: query, SessionID: params.SessionID, Limit: limit + 1, AllowOneExtra: true}
	if params.Cursor != "" {
		cursor, err := decodeCursor(params.Cursor, "search", params.SessionID, scope)
		if err != nil {
			return nil, invalidParams(fmt.Errorf("invalid cursor: %w", err))
		}
		searchRequest.BeforeCreatedAt = cursor.CreatedAt
		searchRequest.BeforeID = cursor.ID
	}
	items, err := search.SearchMessagesPage(ctx, searchRequest)
	if err != nil {
		return nil, sourceFailure()
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	result := searchResult{Results: make([]searchHit, len(items)), HasMore: hasMore}
	for index, item := range items {
		preview, truncated := truncatePageText(item.Text, maxSearchPreviewBytes)
		result.Results[index] = searchHit{
			Kind:             "message",
			MessageID:        item.ID,
			SessionID:        item.SessionID,
			Role:             string(item.Role),
			Preview:          preview,
			PreviewTruncated: truncated,
			CreatedAt:        item.CreatedAt,
		}
	}
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		result.NextCursor = encodeCursor(opaqueCursor{Version: 1, Kind: "search", SessionID: params.SessionID, Scope: scope, CreatedAt: last.CreatedAt, ID: last.ID})
	}
	return result, nil
}

func projectPageMessage(item *message.Message, remainingText int) (pageMessage, int) {
	textLimit := min(maxMessageTextBytes, max(remainingText, 0))
	text, truncated := truncatePageText(item.Content().Text, textLimit)
	remainingText -= len(text)
	attachments := make([]attachmentMetadata, 0, len(item.ImageURLContent())+len(item.BinaryContent()))
	for range item.ImageURLContent() {
		attachments = append(attachments, attachmentMetadata{Kind: "image"})
	}
	for _, attachment := range item.BinaryContent() {
		attachments = append(attachments, attachmentMetadata{Kind: "binary", MIMEType: attachment.MIMEType})
	}
	toolCalls := item.ToolCalls()
	toolMetadata := make([]toolCallMetadata, len(toolCalls))
	for index, call := range toolCalls {
		toolMetadata[index] = toolCallMetadata{ID: call.ID, Name: call.Name, Finished: call.Finished}
	}
	return pageMessage{
		ID:              item.ID,
		Role:            string(item.Role),
		Text:            text,
		TextTruncated:   truncated,
		HasReasoning:    item.ReasoningContent().Thinking != "",
		Attachments:     attachments,
		ToolCalls:       toolMetadata,
		ToolResultCount: len(item.ToolResults()),
		FinishReason:    string(item.FinishReason()),
		Model:           item.Model,
		Provider:        item.Provider,
		Usage: usageMetadata{
			InputTokens:      item.Usage.InputTokens,
			OutputTokens:     item.Usage.OutputTokens,
			ReasoningTokens:  item.Usage.ReasoningTokens,
			CacheReadTokens:  item.Usage.CacheReadTokens,
			CacheWriteTokens: item.Usage.CacheWriteTokens,
		},
		CreatedAt: item.CreatedAt,
		UpdatedAt: item.UpdatedAt,
	}, remainingText
}

func boundedLimit(value, defaultValue, maxValue int) (int, error) {
	if value < 0 {
		return 0, errors.New("limit must not be negative")
	}
	if value == 0 {
		return defaultValue, nil
	}
	return min(value, maxValue), nil
}

func encodeCursor(cursor opaqueCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(encoded, kind, sessionID, scope string) (opaqueCursor, error) {
	if len(encoded) > maxCursorBytes {
		return opaqueCursor{}, errors.New("cursor is too large")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return opaqueCursor{}, errors.New("cursor encoding is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var cursor opaqueCursor
	if err := decoder.Decode(&cursor); err != nil {
		return opaqueCursor{}, errors.New("cursor payload is invalid")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return opaqueCursor{}, errors.New("cursor has trailing data")
	}
	if cursor.Version != 1 || cursor.Kind != kind || cursor.SessionID != sessionID || cursor.Scope != scope || cursor.ID == "" {
		return opaqueCursor{}, errors.New("cursor does not match this request")
	}
	return cursor, nil
}

func searchScope(query, sessionID string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(query)) + "\x00" + sessionID))
	return hex.EncodeToString(sum[:16])
}

func truncatePageText(value string, limit int) (string, bool) {
	if limit <= 0 {
		return "", value != ""
	}
	if len(value) <= limit {
		return value, false
	}
	end := limit
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end], true
}

func sessionSourceError(sessionID string, err error) *acp.RPCError {
	if errors.Is(err, sql.ErrNoRows) {
		return protocolError(-32021, errorSessionNotFound, map[string]any{"sessionId": sessionID})
	}
	return sourceFailure()
}

func sourceUnavailable(message string) *acp.RPCError {
	return &acp.RPCError{Code: acp.CodeInternalError, Message: message}
}

func sourceFailure() *acp.RPCError {
	return &acp.RPCError{Code: acp.CodeInternalError, Message: errorQueryFailed, Data: ErrorData{Code: errorQueryFailed, Retryable: true}}
}
