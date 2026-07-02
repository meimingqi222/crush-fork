-- name: GetMCPOAuthToken :one
SELECT server_name, access_token, refresh_token, expires_at, created_at, updated_at FROM mcp_oauth_tokens
WHERE server_name = ?;

-- name: UpsertMCPOAuthToken :exec
INSERT INTO mcp_oauth_tokens (server_name, access_token, refresh_token, expires_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(server_name) DO UPDATE SET
    access_token = excluded.access_token,
    refresh_token = excluded.refresh_token,
    expires_at = excluded.expires_at,
    updated_at = strftime('%s','now');

-- name: DeleteMCPOAuthToken :exec
DELETE FROM mcp_oauth_tokens
WHERE server_name = ?;
