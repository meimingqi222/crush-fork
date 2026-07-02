-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS mcp_oauth_tokens (
    server_name TEXT PRIMARY KEY,
    access_token TEXT NOT NULL,
    refresh_token TEXT NOT NULL,
    expires_at INTEGER,
    created_at INTEGER NOT NULL DEFAULT (strftime('%s','now')),
    updated_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS mcp_oauth_tokens;
-- +goose StatementEnd
