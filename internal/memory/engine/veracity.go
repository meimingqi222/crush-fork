package engine

import (
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// VeracityWeight maps each MemoryVeracity to a weight used during Bayesian
// confidence updates.  Higher weights mean the source is more trustworthy
// and produces larger confidence increments.
var VeracityWeight = map[MemoryVeracity]float64{
	MemoryVeracityStated:   1.0,
	MemoryVeracityInferred: 0.7,
	MemoryVeracityTool:     0.5,
	MemoryVeracityImported: 0.6,
	MemoryVeracityUnknown:  0.8,
}

// ValidVeracity returns true if the given string is a recognized veracity label.
func ValidVeracity(v string) bool {
	_, ok := VeracityWeight[MemoryVeracity(v)]
	return ok
}

// NormalizeVeracity clamps an arbitrary string to a valid MemoryVeracity.
// Unknown values default to MemoryVeracityUnknown.
func NormalizeVeracity(v string) MemoryVeracity {
	if ValidVeracity(v) {
		return MemoryVeracity(v)
	}
	return MemoryVeracityUnknown
}

// BayesianUpdate applies a single Bayesian confidence increment based on
// the veracity of the incoming evidence.  The formula is:
//
//	new = old + (1 - old) × veracity_weight × 0.3
//
// This ensures confidence approaches 1.0 asymptotically and never exceeds it.
func BayesianUpdate(currentConfidence float64, veracity MemoryVeracity) float64 {
	weight, ok := VeracityWeight[veracity]
	if !ok {
		weight = VeracityWeight[MemoryVeracityUnknown]
	}
	increment := (1.0 - currentConfidence) * weight * 0.3
	return min(currentConfidence+increment, 1.0)
}

// MemoryConflict represents a detected contradiction between two memory events.
type MemoryConflict struct {
	ID           int64
	FactAID      string
	FactBID      string
	ConflictType string
	Resolution   string
	ResolvedAt   *time.Time
	CreatedAt    time.Time
}

// ConflictDetector scans for contradictions in the memory event store.
// Two events conflict when they share the same scope+kind but have
// contradictory content (e.g., "prefer X" vs "prefer Y").
type ConflictDetector struct {
	db *sql.DB
}

// NewConflictDetector creates a ConflictDetector backed by the given database.
func NewConflictDetector(db *sql.DB) *ConflictDetector {
	return &ConflictDetector{db: db}
}

// DetectConflicts finds pairs of non-superseded events with the same
// scope+kind where one event's content directly contradicts another.
// It inserts unresolved conflict rows into memory_conflicts.
// Returns the number of new conflicts detected.
func (d *ConflictDetector) DetectConflicts() (int, error) {
	if d.db == nil {
		return 0, nil
	}

	// Find pairs of active (non-superseded, non-expired) events that share
	// scope+kind but have different content.  This is a heuristic: true
	// contradiction detection would require LLM analysis, but this catches
	// the common case of same-category events with opposing content.
	rows, err := d.db.Query(`
		SELECT a.id, b.id
		FROM memory_events a
		JOIN memory_events b ON a.scope = b.scope
		                       AND a.kind = b.kind
		                       AND a.id < b.id
		WHERE a.supersedes IS NULL
		  AND b.supersedes IS NULL
		  AND (a.expires_at IS NULL OR a.expires_at = 0 OR a.expires_at > strftime('%s','now'))
		  AND (b.expires_at IS NULL OR b.expires_at = 0 OR b.expires_at > strftime('%s','now'))
		  AND a.kind NOT IN ('working_memory', 'task_state')
		  AND NOT EXISTS (
		      SELECT 1 FROM memory_conflicts c
		      WHERE (c.fact_a_id = a.id AND c.fact_b_id = b.id)
		         OR (c.fact_a_id = b.id AND c.fact_b_id = a.id)
		  )
		LIMIT 100
	`)
	if err != nil {
		return 0, fmt.Errorf("querying for conflicts: %w", err)
	}
	defer rows.Close()

	inserted := 0
	now := time.Now().Unix()
	for rows.Next() {
		var factAID, factBID string
		if err := rows.Scan(&factAID, &factBID); err != nil {
			continue
		}
		_, err := d.db.Exec(`
			INSERT INTO memory_conflicts (fact_a_id, fact_b_id, conflict_type, created_at, updated_at)
			VALUES (?, ?, 'contradiction', ?, ?)
		`, factAID, factBID, now, now)
		if err != nil {
			slog.Warn("Failed to insert memory conflict", "error", err)
			continue
		}
		inserted++
	}
	return inserted, rows.Err()
}

// GetUnresolvedConflicts returns all conflicts that have not been resolved.
func (d *ConflictDetector) GetUnresolvedConflicts(limit int) ([]MemoryConflict, error) {
	if d.db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.db.Query(`
		SELECT id, fact_a_id, fact_b_id, conflict_type, created_at
		FROM memory_conflicts
		WHERE resolution IS NULL
		ORDER BY created_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("querying unresolved conflicts: %w", err)
	}
	defer rows.Close()

	var conflicts []MemoryConflict
	for rows.Next() {
		var c MemoryConflict
		var createdAt int64
		if err := rows.Scan(&c.ID, &c.FactAID, &c.FactBID, &c.ConflictType, &createdAt); err != nil {
			continue
		}
		c.CreatedAt = time.Unix(createdAt, 0)
		conflicts = append(conflicts, c)
	}
	return conflicts, rows.Err()
}

// ResolveConflict marks a conflict as resolved, superseding the losing event.
func (d *ConflictDetector) ResolveConflict(conflictID int64, winningID string) error {
	if d.db == nil {
		return nil
	}

	// Determine the losing event.
	var factAID, factBID string
	err := d.db.QueryRow(`
		SELECT fact_a_id, fact_b_id FROM memory_conflicts WHERE id = ?
	`, conflictID).Scan(&factAID, &factBID)
	if err != nil {
		return fmt.Errorf("finding conflict %d: %w", conflictID, err)
	}

	var losingID string
	switch winningID {
	case factAID:
		losingID = factBID
	case factBID:
		losingID = factAID
	default:
		return fmt.Errorf("winning ID %s is not part of conflict %d", winningID, conflictID)
	}

	now := time.Now().Unix()
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning conflict resolution transaction: %w", err)
	}
	defer tx.Rollback()

	// Supersede the losing event.
	_, err = tx.Exec(`
		UPDATE memory_events SET supersedes = ?, updated_at = ? WHERE id = ?
	`, winningID, now, losingID)
	if err != nil {
		return fmt.Errorf("superseding losing event %s: %w", losingID, err)
	}

	// Mark the conflict as resolved.
	resolution := fmt.Sprintf("superseded_by_%s", winningID)
	_, err = tx.Exec(`
		UPDATE memory_conflicts
		SET resolution = ?, resolved_at = ?, updated_at = ?
		WHERE id = ?
	`, resolution, now, now, conflictID)
	if err != nil {
		return fmt.Errorf("resolving conflict %d: %w", conflictID, err)
	}

	return tx.Commit()
}
