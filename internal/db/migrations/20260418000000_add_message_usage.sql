-- +goose Up
-- +goose StatementBegin
ALTER TABLE messages ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN reasoning_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE messages DROP COLUMN cache_write_tokens;
ALTER TABLE messages DROP COLUMN cache_read_tokens;
ALTER TABLE messages DROP COLUMN reasoning_tokens;
ALTER TABLE messages DROP COLUMN output_tokens;
ALTER TABLE messages DROP COLUMN input_tokens;
-- +goose StatementEnd
