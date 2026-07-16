-- +goose Up
ALTER TABLE sessions ADD COLUMN archived INTEGER NOT NULL DEFAULT 0 CHECK (archived IN (0, 1));
ALTER TABLE sessions ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0 CHECK (pinned IN (0, 1));

-- +goose Down
ALTER TABLE sessions DROP COLUMN pinned;
ALTER TABLE sessions DROP COLUMN archived;
