-- +goose Up
ALTER TABLE sessions ADD COLUMN inference_config TEXT NOT NULL DEFAULT '{}';
ALTER TABLE sessions ADD COLUMN inference_revision INTEGER NOT NULL DEFAULT 0 CHECK (inference_revision >= 0);

-- +goose Down
ALTER TABLE sessions DROP COLUMN inference_revision;
ALTER TABLE sessions DROP COLUMN inference_config;
