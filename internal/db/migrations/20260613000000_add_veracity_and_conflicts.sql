-- +goose Up
-- +goose StatementBegin

-- Veracity label tracks how a memory fact was established, enabling
-- Bayesian confidence updates and conflict detection.  The column is
-- added with a safe default so existing rows continue to work.

ALTER TABLE memory_events ADD COLUMN veracity TEXT NOT NULL DEFAULT 'unknown';

-- Conflicts table records pairs of memories that contradict each other
-- (same subject/predicate, different object).  Unresolved conflicts
-- can be surfaced to the user or resolved automatically by confidence.

CREATE TABLE IF NOT EXISTS memory_conflicts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    fact_a_id TEXT NOT NULL,
    fact_b_id TEXT NOT NULL,
    conflict_type TEXT NOT NULL DEFAULT 'contradiction',
    resolution TEXT,
    resolved_at INTEGER,
    created_at INTEGER NOT NULL DEFAULT (strftime('%s','now')),
    updated_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);

CREATE INDEX IF NOT EXISTS idx_memory_conflicts_unresolved
    ON memory_conflicts (fact_a_id, fact_b_id)
    WHERE resolution IS NULL;

CREATE INDEX IF NOT EXISTS idx_memory_events_veracity ON memory_events (veracity);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS memory_conflicts;
DROP INDEX IF EXISTS idx_memory_events_veracity;

-- SQLite does not support DROP COLUMN before 3.35.0, so we recreate.
-- For safety, the down migration is a no-op for the veracity column.
-- A full rebuild would be needed to remove it.

-- +goose StatementEnd
