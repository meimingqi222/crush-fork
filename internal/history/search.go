package history

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
)

const (
	DefaultSearchLimit = 20
	MaxSearchLimit     = 100
)

type SearchParams struct {
	Query           string
	SessionID       string
	Limit           int
	BeforeCreatedAt int64
	BeforeID        string
	AllowOneExtra   bool
}

type MessageSearchResult struct {
	ID        string
	SessionID string
	Role      message.MessageRole
	Text      string
	CreatedAt int64
}

func (s *service) SearchMessages(ctx context.Context, params SearchParams) ([]MessageSearchResult, error) {
	return s.searchMessages(ctx, params, false)
}

// SearchMessagesPage uses a stable created_at/id keyset cursor for GUI paging.
func (s *service) SearchMessagesPage(ctx context.Context, params SearchParams) ([]MessageSearchResult, error) {
	return s.searchMessages(ctx, params, true)
}

func (s *service) searchMessages(ctx context.Context, params SearchParams, keyset bool) ([]MessageSearchResult, error) {
	query := strings.TrimSpace(params.Query)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}

	limit := cmp.Or(params.Limit, DefaultSearchLimit)
	if limit < 1 {
		limit = DefaultSearchLimit
	}
	maxLimit := MaxSearchLimit
	if params.AllowOneExtra {
		maxLimit++
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	if !keyset {
		dbMessages, err := s.q.SearchMessages(ctx, db.SearchMessagesParams{
			SessionID: params.SessionID,
			Query:     sql.NullString{String: query, Valid: true},
			Limit:     int64(limit),
		})
		if err != nil {
			return nil, err
		}
		return searchResults(dbMessages)
	}
	dbParams := db.SearchMessagesBeforeParams{
		SessionID: params.SessionID,
		HasCursor: int64(0),
		Query:     sql.NullString{String: query, Valid: true},
		Limit:     int64(limit),
	}
	if params.BeforeID != "" {
		dbParams.HasCursor = 1
		dbParams.BeforeCreatedAt = params.BeforeCreatedAt
		dbParams.BeforeID = params.BeforeID
	}
	dbMessages, err := s.q.SearchMessagesBefore(ctx, dbParams)
	if err != nil {
		return nil, err
	}
	return searchResults(dbMessages)
}

func searchResults(dbMessages []db.Message) ([]MessageSearchResult, error) {
	results := make([]MessageSearchResult, 0, len(dbMessages))
	for _, item := range dbMessages {
		text, err := extractTextFromParts(item.Parts)
		if err != nil {
			return nil, err
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		results = append(results, MessageSearchResult{
			ID:        item.ID,
			SessionID: item.SessionID,
			Role:      message.MessageRole(item.Role),
			Text:      text,
			CreatedAt: item.CreatedAt,
		})
	}

	return results, nil
}

func extractTextFromParts(partsJSON string) (string, error) {
	var wrapped []struct {
		Type string `json:"type"`
		Data struct {
			Text string `json:"text"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(partsJSON), &wrapped); err != nil {
		return "", err
	}
	for _, part := range wrapped {
		if part.Type == "text" {
			return part.Data.Text, nil
		}
	}
	return "", nil
}
