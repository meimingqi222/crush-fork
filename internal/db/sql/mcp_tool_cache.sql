-- name: GetMCPToolCache :one
SELECT server_name, config_hash, tools_json, cached_at FROM mcp_tool_cache
WHERE server_name = ?;

-- name: UpsertMCPToolCache :exec
INSERT INTO mcp_tool_cache (server_name, config_hash, tools_json)
VALUES (?, ?, ?)
ON CONFLICT(server_name) DO UPDATE SET
    config_hash = excluded.config_hash,
    tools_json = excluded.tools_json,
    cached_at = strftime('%s','now');

-- name: DeleteMCPToolCache :exec
DELETE FROM mcp_tool_cache
WHERE server_name = ?;
