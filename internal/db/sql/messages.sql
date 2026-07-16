-- name: GetMessage :one
SELECT *
FROM messages
WHERE id = ? LIMIT 1;

-- name: GetRetrySourceMessage :one
SELECT user.*
FROM messages AS target
JOIN messages AS user ON user.session_id = target.session_id
WHERE target.id = sqlc.arg(message_id)
  AND target.session_id = sqlc.arg(session_id)
  AND target.role = 'assistant'
  AND user.role = 'user'
  AND (
    user.created_at < target.created_at
    OR (user.created_at = target.created_at AND user.rowid <= target.rowid)
  )
ORDER BY user.created_at DESC, user.rowid DESC
LIMIT 1;

-- name: ListMessagesBySession :many
SELECT *
FROM messages
WHERE session_id = ?
ORDER BY created_at ASC, rowid ASC;

-- name: CountMessagesBySession :one
SELECT COUNT(*)
FROM messages
WHERE session_id = ?;

-- name: ListMessagesBySessionPage :many
SELECT *
FROM messages
WHERE session_id = ?
ORDER BY created_at ASC, rowid ASC
LIMIT sqlc.arg(limit) OFFSET sqlc.arg(offset);

-- name: ListRecentMessagesBySession :many
SELECT *
FROM messages
WHERE session_id = ?
ORDER BY created_at DESC, rowid DESC
LIMIT sqlc.arg(limit);

-- name: ListMessagesBefore :many
SELECT *
FROM messages
WHERE session_id = sqlc.arg(session_id)
  AND (
    sqlc.arg(has_cursor) = 0
    OR created_at < sqlc.arg(before_created_at)
    OR (created_at = sqlc.arg(before_created_at) AND id < sqlc.arg(before_id))
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(limit);

-- name: CreateMessage :one
INSERT INTO messages (
    id,
    session_id,
    role,
    parts,
    model,
    provider,
    is_summary_message,
    created_at,
    updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, strftime('%s', 'now'), strftime('%s', 'now')
)
RETURNING *;

-- name: UpdateMessage :exec
UPDATE messages
SET
    parts = ?,
    finished_at = ?,
    input_tokens = ?,
    output_tokens = ?,
    reasoning_tokens = ?,
    cache_read_tokens = ?,
    cache_write_tokens = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;


-- name: DeleteMessage :exec
DELETE FROM messages
WHERE id = ?;

-- name: DeleteSessionMessages :exec
DELETE FROM messages
WHERE session_id = ?;

-- name: ListUserMessagesBySession :many
SELECT *
FROM messages
WHERE session_id = ? AND role = 'user'
ORDER BY created_at DESC;

-- name: ListAllUserMessages :many
SELECT *
FROM messages
WHERE role = 'user'
ORDER BY created_at DESC;

-- name: SearchMessages :many
SELECT *
FROM messages
WHERE (sqlc.arg(session_id) = '' OR session_id = sqlc.arg(session_id))
  AND EXISTS (
    SELECT 1
    FROM json_each(messages.parts)
    WHERE json_extract(json_each.value, '$.type') = 'text'
      AND lower(COALESCE(json_extract(json_each.value, '$.data.text'), '')) LIKE lower('%' || sqlc.arg(query) || '%')
  )
ORDER BY created_at DESC, rowid DESC
LIMIT sqlc.arg(limit);

-- name: SearchMessagesBefore :many
SELECT *
FROM messages
WHERE (sqlc.arg(session_id) = '' OR session_id = sqlc.arg(session_id))
  AND (
    sqlc.arg(has_cursor) = 0
    OR created_at < sqlc.arg(before_created_at)
    OR (created_at = sqlc.arg(before_created_at) AND id < sqlc.arg(before_id))
  )
  AND EXISTS (
    SELECT 1
    FROM json_each(messages.parts)
    WHERE json_extract(json_each.value, '$.type') = 'text'
      AND lower(COALESCE(json_extract(json_each.value, '$.data.text'), '')) LIKE lower('%' || sqlc.arg(query) || '%')
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(limit);
