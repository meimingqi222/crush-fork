package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type CachedEmbedding struct {
	EventID   string
	Model     string
	Vector    []float64
	Dimension int
}

type EmbeddingStore struct {
	db      *sql.DB
	now     func() time.Time
	mu      sync.RWMutex
	pending map[string]struct{}
}

func NewEmbeddingStore(db *sql.DB) *EmbeddingStore {
	if db == nil {
		return nil
	}
	return &EmbeddingStore{
		db:      db,
		now:     time.Now,
		pending: make(map[string]struct{}),
	}
}

func (s *EmbeddingStore) Get(ctx context.Context, eventID, model string) ([]float64, error) {
	if s == nil || s.db == nil || eventID == "" {
		return nil, nil
	}
	var embeddingJSON string
	var dimension int
	err := s.db.QueryRowContext(ctx, `
		SELECT embedding_json, dimension FROM memory_embeddings
		WHERE event_id = ? AND model = ?
	`, eventID, model).Scan(&embeddingJSON, &dimension)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get embedding for %s: %w", eventID, err)
	}
	var vec []float64
	if err := json.Unmarshal([]byte(embeddingJSON), &vec); err != nil {
		slog.Debug("Failed to unmarshal cached embedding", "error", err, "event_id", eventID)
		return nil, nil
	}
	if len(vec) != dimension {
		slog.Debug("Cached embedding dimension mismatch",
			"event_id", eventID, "expected", dimension, "got", len(vec))
		return nil, nil
	}
	return vec, nil
}

func (s *EmbeddingStore) GetBatch(ctx context.Context, eventIDs []string, model string) (map[string][]float64, error) {
	result := make(map[string][]float64, len(eventIDs))
	if s == nil || s.db == nil || len(eventIDs) == 0 {
		return result, nil
	}

	for offset := 0; offset < len(eventIDs); offset += 500 {
		end := offset + 500
		if end > len(eventIDs) {
			end = len(eventIDs)
		}
		chunk := eventIDs[offset:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, model)
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query := fmt.Sprintf(`
			SELECT event_id, embedding_json, dimension
			FROM memory_embeddings
			WHERE model = ? AND event_id IN (%s)
		`, joinPlaceholders(placeholders))

		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return result, fmt.Errorf("batch get embeddings: %w", err)
		}
		for rows.Next() {
			var eventID string
			var embeddingJSON string
			var dimension int
			if err := rows.Scan(&eventID, &embeddingJSON, &dimension); err != nil {
				continue
			}
			var vec []float64
			if err := json.Unmarshal([]byte(embeddingJSON), &vec); err != nil {
				slog.Debug("Failed to unmarshal cached embedding", "error", err, "event_id", eventID)
				continue
			}
			if len(vec) == dimension {
				result[eventID] = vec
			}
		}
		rows.Close()
	}
	return result, nil
}

func (s *EmbeddingStore) Put(ctx context.Context, eventID, model string, vec []float64) error {
	if s == nil || s.db == nil || eventID == "" || len(vec) == 0 {
		return nil
	}
	data, err := json.Marshal(vec)
	if err != nil {
		return fmt.Errorf("marshal embedding for %s: %w", eventID, err)
	}
	now := s.now().Unix()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO memory_embeddings (event_id, model, embedding_json, dimension, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(event_id) DO UPDATE SET
			model = excluded.model,
			embedding_json = excluded.embedding_json,
			dimension = excluded.dimension,
			updated_at = excluded.updated_at
	`, eventID, model, string(data), len(vec), now, now)
	if err != nil {
		return fmt.Errorf("put embedding for %s: %w", eventID, err)
	}

	s.mu.Lock()
	delete(s.pending, eventID)
	s.mu.Unlock()
	return nil
}

func (s *EmbeddingStore) MarkPending(eventID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[eventID] = struct{}{}
}

func (s *EmbeddingStore) IsPending(eventID string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.pending[eventID]
	return ok
}

func (s *EmbeddingStore) FindMissing(ctx context.Context, eventIDs []string, model string) ([]string, error) {
	if s == nil || s.db == nil || len(eventIDs) == 0 {
		return eventIDs, nil
	}

	cached, err := s.GetBatch(ctx, eventIDs, model)
	if err != nil {
		return eventIDs, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	missing := make([]string, 0, len(eventIDs))
	for _, id := range eventIDs {
		if _, exists := cached[id]; exists {
			continue
		}
		if _, pending := s.pending[id]; pending {
			continue
		}
		missing = append(missing, id)
	}
	return missing, nil
}

func joinPlaceholders(placeholders []string) string {
	if len(placeholders) == 0 {
		return ""
	}
	result := placeholders[0]
	for i := 1; i < len(placeholders); i++ {
		result += ", " + placeholders[i]
	}
	return result
}