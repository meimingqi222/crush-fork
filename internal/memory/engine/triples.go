package engine

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Triple represents a structured (subject, predicate, object) fact extracted
// from conversation.  Triples enable precise subject/predicate queries and
// contradiction detection that free-form MemoryEvent content cannot support.
type Triple struct {
	ID            string         `json:"id"`
	Subject       string         `json:"subject"`
	Predicate     string         `json:"predicate"`
	Object        string         `json:"object"`
	Confidence    float64        `json:"confidence"`
	Veracity      MemoryVeracity `json:"veracity,omitempty"`
	ValidFrom     time.Time      `json:"valid_from"`
	ValidTo       *time.Time     `json:"valid_to,omitempty"`
	SourceEventID string         `json:"source_event_id,omitempty"`
	Scope         MemoryScope    `json:"scope"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	SupersededBy  string         `json:"superseded_by,omitempty"`
}

// EdgeType defines the type of semantic relationship between two memory items.
type EdgeType string

const (
	EdgeRelatedTo   EdgeType = "related_to"
	EdgeContradicts EdgeType = "contradicts"
	EdgeRefines     EdgeType = "refines"
	EdgeDependsOn   EdgeType = "depends_on"
)

// Edge represents a directed semantic relationship between two memory items
// (events or triples).
type Edge struct {
	SourceID  string    `json:"source_id"`
	TargetID  string    `json:"target_id"`
	EdgeType  EdgeType  `json:"edge_type"`
	Weight    float64   `json:"weight"`
	CreatedAt time.Time `json:"created_at"`
}

// TripleStore manages storage and retrieval of memory triples and edges.
type TripleStore struct {
	db *sql.DB
}

// NewTripleStore creates a TripleStore backed by the given database.
func NewTripleStore(db *sql.DB) *TripleStore {
	return &TripleStore{db: db}
}

// AddTriple inserts a new triple into the store.  If a triple with the same
// (subject, predicate, object) already exists and is not superseded, the
// existing triple's confidence is updated via Bayesian update instead.
func (s *TripleStore) AddTriple(ctx context.Context, triple Triple) error {
	if s.db == nil {
		return nil
	}

	id := triple.ID
	if id == "" {
		id = fmt.Sprintf("tri-%d", time.Now().UnixNano())
	}
	now := time.Now().Unix()
	validFrom := triple.ValidFrom.Unix()
	if validFrom == 0 {
		validFrom = now
	}

	// Check for existing active triple with same SPO.
	var existingID string
	var existingConf float64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, confidence FROM memory_triples
		WHERE subject = ? AND predicate = ? AND object = ?
		  AND superseded_by IS NULL
		  AND (valid_to IS NULL OR valid_to = 0 OR valid_to > strftime('%s','now'))
		LIMIT 1
	`, triple.Subject, triple.Predicate, triple.Object).Scan(&existingID, &existingConf)

	if err == nil {
		// Duplicate found: update confidence via Bayesian update.
		newConf := BayesianUpdate(existingConf, triple.Veracity)
		_, err := s.db.ExecContext(ctx, `
			UPDATE memory_triples SET confidence = ?, veracity = ?, updated_at = ?
			WHERE id = ?
		`, newConf, string(triple.Veracity), now, existingID)
		return err
	}

	if err != sql.ErrNoRows {
		return fmt.Errorf("checking existing triple: %w", err)
	}

	// No duplicate: insert new.
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO memory_triples
			(id, subject, predicate, object, confidence, veracity, valid_from, source_event_id, scope, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, triple.Subject, triple.Predicate, triple.Object,
		triple.Confidence, string(triple.Veracity), validFrom,
		triple.SourceEventID, string(triple.Scope), now, now)
	return err
}

// QueryTriples retrieves triples matching the given filters.  Empty strings
// act as wildcards.
func (s *TripleStore) QueryTriples(ctx context.Context, subject, predicate string, limit int) ([]Triple, error) {
	if s.db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}

	conditions := []string{
		"superseded_by IS NULL",
		"(valid_to IS NULL OR valid_to = 0 OR valid_to > strftime('%s','now'))",
	}
	args := []any{}

	if subject != "" {
		conditions = append(conditions, "subject = ?")
		args = append(args, subject)
	}
	if predicate != "" {
		conditions = append(conditions, "predicate = ?")
		args = append(args, predicate)
	}

	where := strings.Join(conditions, " AND ")
	query := fmt.Sprintf(`
		SELECT id, subject, predicate, object, confidence, veracity,
		       valid_from, source_event_id, scope, created_at, updated_at
		FROM memory_triples
		WHERE %s
		ORDER BY confidence DESC, created_at DESC
		LIMIT ?
	`, where)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying triples: %w", err)
	}
	defer rows.Close()

	var triples []Triple
	for rows.Next() {
		var t Triple
		var validFrom, createdAt, updatedAt int64
		var sourceEventID sql.NullString
		if err := rows.Scan(
			&t.ID, &t.Subject, &t.Predicate, &t.Object,
			&t.Confidence, &t.Veracity, &validFrom,
			&sourceEventID, &t.Scope, &createdAt, &updatedAt,
		); err != nil {
			continue
		}
		t.ValidFrom = time.Unix(validFrom, 0)
		t.CreatedAt = time.Unix(createdAt, 0)
		t.UpdatedAt = time.Unix(updatedAt, 0)
		if sourceEventID.Valid {
			t.SourceEventID = sourceEventID.String
		}
		triples = append(triples, t)
	}
	return triples, rows.Err()
}

// AddEdge creates a semantic edge between two memory items.
func (s *TripleStore) AddEdge(ctx context.Context, edge Edge) error {
	if s.db == nil {
		return nil
	}
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO memory_edges (source_id, target_id, edge_type, weight, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, edge.SourceID, edge.TargetID, string(edge.EdgeType), edge.Weight, now)
	return err
}

// GraphQuery traverses the memory graph starting from seed IDs, following
// edges up to maxHops hops.  It returns all reachable items (events + triples).
func (s *TripleStore) GraphQuery(ctx context.Context, seedIDs []string, maxHops int, edgeTypes []EdgeType) ([]MemoryEvent, []Triple, error) {
	if s.db == nil || len(seedIDs) == 0 {
		return nil, nil, nil
	}
	if maxHops <= 0 {
		maxHops = 2
	}

	// Build edge type filter.
	typeFilter := ""
	typeArgs := []any{}
	if len(edgeTypes) > 0 {
		placeholders := make([]string, len(edgeTypes))
		for i, et := range edgeTypes {
			placeholders[i] = "?"
			typeArgs = append(typeArgs, string(et))
		}
		typeFilter = " AND edge_type IN (" + strings.Join(placeholders, ",") + ")"
	}

	// BFS traversal.
	visited := make(map[string]bool)
	for _, id := range seedIDs {
		visited[id] = true
	}
	currentLevel := seedIDs

	for hop := 0; hop < maxHops; hop++ {
		if len(currentLevel) == 0 {
			break
		}

		placeholders := make([]string, len(currentLevel))
		args := make([]any, len(currentLevel))
		for i, id := range currentLevel {
			placeholders[i] = "?"
			args[i] = id
		}
		inClause := strings.Join(placeholders, ",")

		query := fmt.Sprintf(`
			SELECT target_id FROM memory_edges
			WHERE source_id IN (%s)%s
			UNION
			SELECT source_id FROM memory_edges
			WHERE target_id IN (%s)%s
		`, inClause, typeFilter, inClause, typeFilter)

		allArgs := append(args, typeArgs...)
		allArgs = append(allArgs, args...)
		allArgs = append(allArgs, typeArgs...)

		rows, err := s.db.QueryContext(ctx, query, allArgs...)
		if err != nil {
			return nil, nil, fmt.Errorf("graph traversal hop %d: %w", hop, err)
		}

		var nextLevel []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				continue
			}
			if !visited[id] {
				visited[id] = true
				nextLevel = append(nextLevel, id)
			}
		}
		rows.Close()
		currentLevel = nextLevel
	}

	// Fetch all visited events and triples.
	allIDs := make([]string, 0, len(visited))
	for id := range visited {
		allIDs = append(allIDs, id)
	}

	var events []MemoryEvent
	var triples []Triple

	// Fetch events.
	if len(allIDs) > 0 {
		placeholders := make([]string, len(allIDs))
		args := make([]any, len(allIDs))
		for i, id := range allIDs {
			placeholders[i] = "?"
			args[i] = id
		}
		inClause := strings.Join(placeholders, ",")

		eventRows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
			SELECT id, session_id, scope, kind, content, summary,
			       source_json, source_hash, confidence, importance,
			       veracity, supersedes, tags_json, watermark, created_at, updated_at, expires_at
			FROM memory_events
			WHERE id IN (%s)
		`, inClause), args...)
		if err == nil {
			defer eventRows.Close()
			for eventRows.Next() {
				var evt MemoryEvent
				var sourceJSON, sourceHash, tagsJSON string
				var createdAt, updatedAt int64
				var expiresAt sql.NullInt64
				if err := eventRows.Scan(
					&evt.ID, &evt.Source.SessionID, &evt.Scope, &evt.Kind,
					&evt.Content, &evt.Summary, &sourceJSON, &sourceHash,
					&evt.Confidence, &evt.Importance, &evt.Veracity,
					&evt.Supersedes, &tagsJSON, &evt.Watermark,
					&createdAt, &updatedAt, &expiresAt,
				); err != nil {
					continue
				}
				evt.CreatedAt = time.Unix(createdAt, 0)
				evt.UpdatedAt = time.Unix(updatedAt, 0)
				events = append(events, evt)
			}
		}
	}

	// Fetch triples.
	if len(allIDs) > 0 {
		placeholders := make([]string, len(allIDs))
		args := make([]any, len(allIDs))
		for i, id := range allIDs {
			placeholders[i] = "?"
			args[i] = id
		}
		inClause := strings.Join(placeholders, ",")

		tripleRows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
			SELECT id, subject, predicate, object, confidence, veracity,
			       valid_from, source_event_id, scope, created_at, updated_at
			FROM memory_triples
			WHERE id IN (%s)
		`, inClause), args...)
		if err == nil {
			defer tripleRows.Close()
			for tripleRows.Next() {
				var t Triple
				var validFrom, createdAt, updatedAt int64
				var sourceEventID sql.NullString
				if err := tripleRows.Scan(
					&t.ID, &t.Subject, &t.Predicate, &t.Object,
					&t.Confidence, &t.Veracity, &validFrom,
					&sourceEventID, &t.Scope, &createdAt, &updatedAt,
				); err != nil {
					continue
				}
				t.ValidFrom = time.Unix(validFrom, 0)
				t.CreatedAt = time.Unix(createdAt, 0)
				t.UpdatedAt = time.Unix(updatedAt, 0)
				if sourceEventID.Valid {
					t.SourceEventID = sourceEventID.String
				}
				triples = append(triples, t)
			}
		}
	}

	slog.Debug("Graph query completed",
		"seeds", len(seedIDs), "hops", maxHops,
		"events", len(events), "triples", len(triples))
	return events, triples, nil
}

// DetectTripleConflicts finds triples with the same (subject, predicate) but
// different objects, indicating a contradiction.
func (s *TripleStore) DetectTripleConflicts(ctx context.Context) (int, error) {
	if s.db == nil {
		return 0, nil
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, b.id
		FROM memory_triples a
		JOIN memory_triples b ON a.subject = b.subject
		                       AND a.predicate = b.predicate
		                       AND a.object != b.object
		                       AND a.id < b.id
		WHERE a.superseded_by IS NULL
		  AND b.superseded_by IS NULL
		  AND (a.valid_to IS NULL OR a.valid_to = 0 OR a.valid_to > strftime('%s','now'))
		  AND (b.valid_to IS NULL OR b.valid_to = 0 OR b.valid_to > strftime('%s','now'))
		LIMIT 100
	`)
	if err != nil {
		return 0, fmt.Errorf("detecting triple conflicts: %w", err)
	}
	defer rows.Close()

	inserted := 0
	now := time.Now().Unix()
	for rows.Next() {
		var factAID, factBID string
		if err := rows.Scan(&factAID, &factBID); err != nil {
			continue
		}
		_, err := s.db.ExecContext(ctx, `
			INSERT OR IGNORE INTO memory_conflicts (fact_a_id, fact_b_id, conflict_type, created_at, updated_at)
			VALUES (?, ?, 'contradiction', ?, ?)
		`, factAID, factBID, now, now)
		if err != nil {
			slog.Warn("Failed to insert triple conflict", "error", err)
			continue
		}
		inserted++
	}
	return inserted, rows.Err()
}
