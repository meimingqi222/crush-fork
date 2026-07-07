-- +goose Up
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN goal_id TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN goal_id;
-- +goose StatementEnd
