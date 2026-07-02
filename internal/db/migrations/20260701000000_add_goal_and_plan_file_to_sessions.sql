-- +goose Up
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN plan_file_path TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN goal_text TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN goal_status TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN goal_token_budget INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN goal_tokens_used INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN goal_time_seconds INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN goal_created_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN goal_updated_at INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN goal_updated_at;
ALTER TABLE sessions DROP COLUMN goal_created_at;
ALTER TABLE sessions DROP COLUMN goal_time_seconds;
ALTER TABLE sessions DROP COLUMN goal_tokens_used;
ALTER TABLE sessions DROP COLUMN goal_token_budget;
ALTER TABLE sessions DROP COLUMN goal_status;
ALTER TABLE sessions DROP COLUMN goal_text;
ALTER TABLE sessions DROP COLUMN plan_file_path;
-- +goose StatementEnd
