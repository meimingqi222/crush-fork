-- +goose Up
-- +goose StatementBegin

-- Memory triples store structured (subject, predicate, object) facts extracted
-- from conversations.  Unlike free-form MemoryEvent content, triples enable
-- precise subject/predicate queries and contradiction detection.

CREATE TABLE IF NOT EXISTS memory_triples (
    id TEXT PRIMARY KEY,
    subject TEXT NOT NULL,
    predicate TEXT NOT NULL,
    object TEXT NOT NULL,
    confidence REAL NOT NULL DEFAULT 1.0 CHECK (confidence >= 0.0 AND confidence <= 1.0),
    veracity TEXT NOT NULL DEFAULT 'unknown',
    valid_from INTEGER NOT NULL,
    valid_to INTEGER,
    source_event_id TEXT,
    scope TEXT NOT NULL DEFAULT 'project',
    created_at INTEGER NOT NULL DEFAULT (strftime('%s','now')),
    updated_at INTEGER NOT NULL DEFAULT (strftime('%s','now')),
    superseded_by TEXT
);

CREATE INDEX IF NOT EXISTS idx_triples_spo ON memory_triples (subject, predicate, object);
CREATE INDEX IF NOT EXISTS idx_triples_subject ON memory_triples (subject);
CREATE INDEX IF NOT EXISTS idx_triples_predicate ON memory_triples (predicate);
CREATE INDEX IF NOT EXISTS idx_triples_source ON memory_triples (source_event_id);
CREATE INDEX IF NOT EXISTS idx_triples_scope ON memory_triples (scope);

-- Memory edges store semantic relationships between memory events and triples.
-- Edge types include: related_to, contradicts, refines, depends_on, supersedes.

CREATE TABLE IF NOT EXISTS memory_edges (
    source_id TEXT NOT NULL,
    target_id TEXT NOT NULL,
    edge_type TEXT NOT NULL,
    weight REAL NOT NULL DEFAULT 0.5 CHECK (weight >= 0.0 AND weight <= 1.0),
    created_at INTEGER NOT NULL DEFAULT (strftime('%s','now')),
    PRIMARY KEY (source_id, target_id, edge_type)
);

CREATE INDEX IF NOT EXISTS idx_edges_source ON memory_edges (source_id);
CREATE INDEX IF NOT EXISTS idx_edges_target ON memory_edges (target_id);
CREATE INDEX IF NOT EXISTS idx_edges_type ON memory_edges (edge_type);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS memory_edges;
DROP TABLE IF EXISTS memory_triples;

-- +goose StatementEnd
