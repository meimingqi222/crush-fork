-- +goose Up
-- +goose StatementBegin

-- Memory embeddings stores pre-computed embedding vectors for memory events,
-- keyed by event ID and embedding model name. This avoids re-embedding all
-- candidates on every recall query, which is critical for performance when
-- using remote embedding APIs. When the configured model changes, old vectors
-- are invalidated and regenerated in the background.

CREATE TABLE IF NOT EXISTS memory_embeddings (
    event_id TEXT NOT NULL PRIMARY KEY,
    model TEXT NOT NULL DEFAULT '',
    embedding_json TEXT NOT NULL,
    dimension INTEGER NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (strftime('%s','now')),
    updated_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);

CREATE INDEX IF NOT EXISTS idx_embeddings_model ON memory_embeddings (model);
CREATE INDEX IF NOT EXISTS idx_embeddings_event ON memory_embeddings (event_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS memory_embeddings;

-- +goose StatementEnd