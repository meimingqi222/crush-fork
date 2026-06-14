package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// sqliteEventStore is the SQLite-backed implementation of EventStore.
type sqliteEventStore struct {
	db *sql.DB
}

// NewSQLiteEventStore creates a new EventStore backed by the given SQLite database.
func NewSQLiteEventStore(db *sql.DB) EventStore {
	return &sqliteEventStore{db: db}
}

func (s *sqliteEventStore) Append(ctx context.Context, event MemoryEvent) error {
	if event.ID == "" {
		return fmt.Errorf("event ID is required")
	}

	sourceJSON, err := json.Marshal(event.Source)
	if err != nil {
		return fmt.Errorf("marshaling source: %w", err)
	}

	sourceHash, err := sourceHash(event)
	if err != nil {
		return fmt.Errorf("hashing source: %w", err)
	}
	tagsJSON, err := json.Marshal(event.Tags)
	if err != nil {
		return fmt.Errorf("marshaling tags: %w", err)
	}

	now := time.Now().Unix()
	createdAt := now
	if !event.CreatedAt.IsZero() {
		createdAt = event.CreatedAt.Unix()
	}
	updatedAt := now
	if !event.UpdatedAt.IsZero() {
		updatedAt = event.UpdatedAt.Unix()
	}
	var expiresAt *int64
	if event.ExpiresAt != nil {
		v := event.ExpiresAt.Unix()
		expiresAt = &v
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO memory_events
			(id, session_id, scope, kind, content, summary, source_json, source_hash,
			 confidence, importance, veracity, supersedes, tags_json, watermark, created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			COALESCE((SELECT MAX(watermark) FROM memory_events) + 1, 1), ?, ?, ?)`,
		event.ID,
		event.Source.SessionID,
		string(event.Scope),
		string(event.Kind),
		event.Content,
		event.Summary,
		string(sourceJSON),
		sourceHash,
		event.Confidence,
		event.Importance,
		string(event.Veracity),
		event.Supersedes,
		string(tagsJSON),
		createdAt,
		updatedAt,
		expiresAt,
	)
	if err != nil {
		return fmt.Errorf("inserting memory event: %w", err)
	}

	return nil
}

func (s *sqliteEventStore) Query(ctx context.Context, filter EventFilter) ([]MemoryEvent, error) {
	var conditions []string
	var args []any

	if filter.MinWatermark > 0 {
		conditions = append(conditions, "watermark > ?")
		args = append(args, filter.MinWatermark)
	}
	if filter.Scope != nil {
		conditions = append(conditions, "scope = ?")
		args = append(args, string(*filter.Scope))
	}
	if filter.Kind != nil {
		conditions = append(conditions, "kind = ?")
		args = append(args, string(*filter.Kind))
	}
	if filter.SessionID != nil {
		conditions = append(conditions, "session_id = ?")
		args = append(args, *filter.SessionID)
	}
	for _, tag := range filter.Tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		conditions = append(conditions, "EXISTS (SELECT 1 FROM json_each(tags_json) WHERE value = ?)")
		args = append(args, tag)
	}
	if filter.AfterTime != nil {
		conditions = append(conditions, "created_at >= ?")
		args = append(args, *filter.AfterTime)
	}
	if filter.BeforeTime != nil {
		conditions = append(conditions, "created_at < ?")
		args = append(args, *filter.BeforeTime)
	}
	if !filter.IncludeExpired {
		conditions = append(conditions, "(expires_at IS NULL OR expires_at = 0 OR expires_at > ?)")
		args = append(args, time.Now().Unix())
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	orderDirection := "ASC"
	if filter.OrderDesc {
		orderDirection = "DESC"
	}
	query := fmt.Sprintf(`
		SELECT id, session_id, scope, kind, content, summary, source_json, source_hash,
		       confidence, importance, veracity, supersedes, tags_json, watermark, created_at, updated_at, expires_at
		FROM memory_events
		%s
		ORDER BY watermark %s
		LIMIT ?`, whereClause, orderDirection)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying memory events: %w", err)
	}
	defer rows.Close()

	var events []MemoryEvent
	for rows.Next() {
		var (
			event      MemoryEvent
			sourceJSON string
			sourceHash string
			tagsJSON   string
			createdAt  int64
			updatedAt  int64
			expiresAt  sql.NullInt64
		)
		if err := rows.Scan(
			&event.ID,
			&event.Source.SessionID,
			&event.Scope,
			&event.Kind,
			&event.Content,
			&event.Summary,
			&sourceJSON,
			&sourceHash,
			&event.Confidence,
			&event.Importance,
			&event.Veracity,
			&event.Supersedes,
			&tagsJSON,
			&event.Watermark,
			&createdAt,
			&updatedAt,
			&expiresAt,
		); err != nil {
			return nil, fmt.Errorf("scanning memory event: %w", err)
		}

		if err := json.Unmarshal([]byte(sourceJSON), &event.Source); err != nil {
			return nil, fmt.Errorf("unmarshaling source: %w", err)
		}
		if err := json.Unmarshal([]byte(tagsJSON), &event.Tags); err != nil {
			return nil, fmt.Errorf("unmarshaling tags: %w", err)
		}

		event.CreatedAt = time.Unix(createdAt, 0)
		event.UpdatedAt = time.Unix(updatedAt, 0)
		if expiresAt.Valid {
			t := time.Unix(expiresAt.Int64, 0)
			event.ExpiresAt = &t
		}

		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating memory events: %w", err)
	}

	return events, nil
}

func (s *sqliteEventStore) GetByID(ctx context.Context, id string) (*MemoryEvent, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, session_id, scope, kind, content, summary, source_json, source_hash,
		       confidence, importance, veracity, supersedes, tags_json, watermark, created_at, updated_at, expires_at
		FROM memory_events
		WHERE id = ?`, id)

	var (
		event      MemoryEvent
		sourceJSON string
		sourceHash string
		tagsJSON   string
		createdAt  int64
		updatedAt  int64
		expiresAt  sql.NullInt64
	)
	err := row.Scan(
		&event.ID,
		&event.Source.SessionID,
		&event.Scope,
		&event.Kind,
		&event.Content,
		&event.Summary,
		&sourceJSON,
		&sourceHash,
		&event.Confidence,
		&event.Importance,
		&event.Veracity,
		&event.Supersedes,
		&tagsJSON,
		&event.Watermark,
		&createdAt,
		&updatedAt,
		&expiresAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting memory event by id: %w", err)
	}

	if err := json.Unmarshal([]byte(sourceJSON), &event.Source); err != nil {
		return nil, fmt.Errorf("unmarshaling source: %w", err)
	}
	if err := json.Unmarshal([]byte(tagsJSON), &event.Tags); err != nil {
		return nil, fmt.Errorf("unmarshaling tags: %w", err)
	}

	event.CreatedAt = time.Unix(createdAt, 0)
	event.UpdatedAt = time.Unix(updatedAt, 0)
	if expiresAt.Valid {
		t := time.Unix(expiresAt.Int64, 0)
		event.ExpiresAt = &t
	}

	return &event, nil
}

func (s *sqliteEventStore) GetMaxWatermark(ctx context.Context) (int64, error) {
	var watermark int64
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(watermark), 0) FROM memory_events").Scan(&watermark)
	if err != nil {
		return 0, fmt.Errorf("getting max watermark: %w", err)
	}
	return watermark, nil
}

// RecentSessions returns distinct session IDs ordered by most-recent event
// time descending. When sinceUnix is zero, the time filter is skipped.
func (s *sqliteEventStore) RecentSessions(ctx context.Context, sinceUnix int64, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	var (
		rows *sql.Rows
		err  error
	)
	if sinceUnix > 0 {
		rows, err = s.db.QueryContext(ctx, `
			SELECT session_id, MAX(created_at) AS latest
			FROM memory_events
			WHERE session_id IS NOT NULL AND session_id != ''
			  AND created_at >= ?
			GROUP BY session_id
			ORDER BY latest DESC
			LIMIT ?`, sinceUnix, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT session_id, MAX(created_at) AS latest
			FROM memory_events
			WHERE session_id IS NOT NULL AND session_id != ''
			GROUP BY session_id
			ORDER BY latest DESC
			LIMIT ?`, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("listing recent sessions: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var (
			sessionID string
			latest    int64
		)
		if err := rows.Scan(&sessionID, &latest); err != nil {
			return nil, fmt.Errorf("scanning recent session row: %w", err)
		}
		if sessionID == "" {
			continue
		}
		ids = append(ids, sessionID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating recent sessions: %w", err)
	}
	return ids, nil
}

func (s *sqliteEventStore) Close() error {
	return nil
}

// sourceHash produces a deterministic hash for event-level idempotency.
func sourceHash(event MemoryEvent) (string, error) {
	payload := struct {
		Source  MemorySourceRef `json:"source"`
		Scope   MemoryScope     `json:"scope"`
		Kind    MemoryKind      `json:"kind"`
		Content string          `json:"content"`
		Summary string          `json:"summary"`
		Tags    []string        `json:"tags,omitempty"`
	}{
		Source:  event.Source,
		Scope:   event.Scope,
		Kind:    event.Kind,
		Content: strings.TrimSpace(event.Content),
		Summary: strings.TrimSpace(event.Summary),
		Tags:    event.Tags,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
