-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS memory_events (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    scope TEXT NOT NULL,
    kind TEXT NOT NULL,
    content TEXT NOT NULL,
    summary TEXT NOT NULL DEFAULT '',
    source_json TEXT NOT NULL DEFAULT '{}',
    source_hash TEXT NOT NULL DEFAULT '',
    confidence REAL NOT NULL DEFAULT 0.5 CHECK (confidence >= 0.0 AND confidence <= 1.0),
    importance REAL NOT NULL DEFAULT 0.5 CHECK (importance >= 0.0 AND importance <= 1.0),
    supersedes TEXT,
    tags_json TEXT NOT NULL DEFAULT '[]',
    watermark INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    expires_at INTEGER,
    UNIQUE(session_id, source_hash)
);

CREATE INDEX IF NOT EXISTS idx_memory_events_watermark ON memory_events (watermark);
CREATE INDEX IF NOT EXISTS idx_memory_events_scope_kind ON memory_events (scope, kind);
CREATE INDEX IF NOT EXISTS idx_memory_events_session ON memory_events (session_id);
CREATE INDEX IF NOT EXISTS idx_memory_events_created_at ON memory_events (created_at);

CREATE TABLE IF NOT EXISTS memory_sources (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    source_type TEXT NOT NULL DEFAULT '',
    cursor TEXT NOT NULL DEFAULT '',
    last_processed_message_id TEXT NOT NULL DEFAULT '',
    watermark INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS memory_jobs (
    id TEXT PRIMARY KEY,
    job_type TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','completed','failed')),
    owner TEXT NOT NULL DEFAULT '',
    lease_expires_at INTEGER,
    retry_count INTEGER NOT NULL DEFAULT 0 CHECK (retry_count >= 0),
    max_retries INTEGER NOT NULL DEFAULT 3 CHECK (max_retries >= 0),
    payload_json TEXT NOT NULL DEFAULT '{}',
    error_message TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_memory_jobs_status ON memory_jobs (status);
CREATE INDEX IF NOT EXISTS idx_memory_jobs_type_status ON memory_jobs (job_type, status);

CREATE TABLE IF NOT EXISTS memory_materialized_views (
    id TEXT PRIMARY KEY,
    view_name TEXT NOT NULL UNIQUE,
    watermark INTEGER NOT NULL DEFAULT 0,
    schema_version INTEGER NOT NULL DEFAULT 1 CHECK (schema_version >= 1),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS memory_materialized_views;
DROP TABLE IF EXISTS memory_jobs;
DROP TABLE IF EXISTS memory_sources;
DROP TABLE IF EXISTS memory_events;

-- +goose StatementEnd
