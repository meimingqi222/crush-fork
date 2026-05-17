-- +goose Up
-- +goose StatementBegin

-- FTS5 virtual table for full-text search on memory events.
-- Uses porter tokenizer for stemming (e.g., "running" matches "run").
CREATE VIRTUAL TABLE IF NOT EXISTS memory_events_fts USING fts5(
    id UNINDEXED,
    content,
    summary,
    scope,
    kind,
    tags,
    tokenize='porter unicode61'
);

-- Helper: flatten a JSON array stored in tags_json into a space-separated
-- string suitable for FTS5 tokenization.  json_extract('$') returns the raw
-- JSON text (e.g. '["go","test"]') which the tokenizer cannot split
-- correctly, so we use json_each + group_concat instead.
--
-- COALESCE handles the case where tags_json is NULL or '[]'.

-- Trigger to keep FTS index in sync on insert.
CREATE TRIGGER IF NOT EXISTS memory_events_fts_insert AFTER INSERT ON memory_events
BEGIN
    INSERT INTO memory_events_fts (id, content, summary, scope, kind, tags)
    VALUES (
        NEW.id,
        NEW.content,
        NEW.summary,
        NEW.scope,
        NEW.kind,
        COALESCE((SELECT group_concat(value, ' ') FROM json_each(NEW.tags_json)), '')
    );
END;

-- Trigger to keep FTS index in sync on update.
CREATE TRIGGER IF NOT EXISTS memory_events_fts_update AFTER UPDATE ON memory_events
BEGIN
    UPDATE memory_events_fts SET
        content = NEW.content,
        summary = NEW.summary,
        scope = NEW.scope,
        kind = NEW.kind,
        tags = COALESCE((SELECT group_concat(value, ' ') FROM json_each(NEW.tags_json)), '')
    WHERE id = NEW.id;
END;

-- Trigger to keep FTS index in sync on delete.
CREATE TRIGGER IF NOT EXISTS memory_events_fts_delete AFTER DELETE ON memory_events
BEGIN
    DELETE FROM memory_events_fts WHERE id = OLD.id;
END;

-- Populate existing events into FTS (for migration).
INSERT INTO memory_events_fts (id, content, summary, scope, kind, tags)
SELECT
    e.id,
    e.content,
    e.summary,
    e.scope,
    e.kind,
    COALESCE((SELECT group_concat(value, ' ') FROM json_each(e.tags_json)), '')
FROM memory_events e;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TRIGGER IF EXISTS memory_events_fts_delete;
DROP TRIGGER IF EXISTS memory_events_fts_update;
DROP TRIGGER IF EXISTS memory_events_fts_insert;
DROP TABLE IF EXISTS memory_events_fts;

-- +goose StatementEnd
