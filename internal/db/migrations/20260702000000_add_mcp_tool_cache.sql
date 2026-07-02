-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS mcp_tool_cache (
    server_name TEXT PRIMARY KEY,
    config_hash TEXT NOT NULL,
    tools_json TEXT NOT NULL,
    cached_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS mcp_tool_cache;
-- +goose StatementEnd
