-- name: AppendMemoryEvent :exec
INSERT OR IGNORE INTO memory_events (
    id, session_id, scope, kind, content, summary, source_json, source_hash,
    confidence, importance, supersedes, tags_json, watermark, created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?,
    ?, ?, ?, ?,
    COALESCE((SELECT MAX(watermark) FROM memory_events) + 1, 1), ?, ?
);

-- name: GetMemoryEventByID :one
SELECT id, session_id, scope, kind, content, summary, source_json, source_hash,
       confidence, importance, supersedes, tags_json, watermark, created_at, updated_at
FROM memory_events
WHERE id = ?;

-- name: ListMemoryEventsByWatermark :many
SELECT id, session_id, scope, kind, content, summary, source_json, source_hash,
       confidence, importance, supersedes, tags_json, watermark, created_at, updated_at
FROM memory_events
WHERE watermark > ?
ORDER BY watermark ASC
LIMIT ?;

-- name: ListMemoryEventsFiltered :many
SELECT id, session_id, scope, kind, content, summary, source_json, source_hash,
       confidence, importance, supersedes, tags_json, watermark, created_at, updated_at
FROM memory_events
WHERE watermark > ?
  AND (?
    = '' OR scope = ?)
  AND (?
    = '' OR kind = ?)
  AND (? = 0 OR created_at >= ?)
  AND (? = 0 OR created_at < ?)
ORDER BY watermark ASC
LIMIT ?;

-- name: GetMaxMemoryEventWatermark :one
SELECT COALESCE(MAX(watermark), 0) FROM memory_events;

-- name: UpsertMemorySource :exec
INSERT INTO memory_sources (id, name, source_type, cursor, last_processed_message_id, watermark, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
    cursor = excluded.cursor,
    last_processed_message_id = excluded.last_processed_message_id,
    watermark = excluded.watermark,
    updated_at = excluded.updated_at;

-- name: GetMemorySourceByName :one
SELECT id, name, source_type, cursor, last_processed_message_id, watermark, created_at, updated_at
FROM memory_sources
WHERE name = ?;

-- name: ListMemorySources :many
SELECT id, name, source_type, cursor, last_processed_message_id, watermark, created_at, updated_at
FROM memory_sources
ORDER BY name;

-- name: CreateMemoryJob :exec
INSERT INTO memory_jobs (id, job_type, status, owner, lease_expires_at, retry_count, max_retries, payload_json, error_message, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListMemoryJobsByStatus :many
SELECT id, job_type, status, owner, lease_expires_at, retry_count, max_retries, payload_json, error_message, created_at, updated_at
FROM memory_jobs
WHERE status = ?
ORDER BY created_at DESC;

-- name: ListRecentMemoryJobs :many
SELECT id, job_type, status, owner, lease_expires_at, retry_count, max_retries, payload_json, error_message, created_at, updated_at
FROM memory_jobs
ORDER BY created_at DESC
LIMIT ?;

-- name: UpdateMemoryJobStatus :exec
UPDATE memory_jobs
SET status = ?, owner = ?, lease_expires_at = ?, retry_count = ?, error_message = ?, updated_at = ?
WHERE id = ?;

-- name: UpsertMaterializedView :exec
INSERT INTO memory_materialized_views (id, view_name, watermark, schema_version, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(view_name) DO UPDATE SET
    watermark = excluded.watermark,
    schema_version = excluded.schema_version,
    updated_at = excluded.updated_at;

-- name: GetMaterializedView :one
SELECT id, view_name, watermark, schema_version, created_at, updated_at
FROM memory_materialized_views
WHERE view_name = ?;

-- name: ListMaterializedViews :many
SELECT id, view_name, watermark, schema_version, created_at, updated_at
FROM memory_materialized_views
ORDER BY view_name;

-- name: ResetMaterializedViewWatermark :exec
UPDATE memory_materialized_views
SET watermark = 0, updated_at = ?
WHERE view_name = ?;
