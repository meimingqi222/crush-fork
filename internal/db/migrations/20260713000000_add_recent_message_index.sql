-- +goose Up
CREATE INDEX IF NOT EXISTS idx_messages_session_recent
ON messages (session_id, created_at DESC, id DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_messages_session_recent;
