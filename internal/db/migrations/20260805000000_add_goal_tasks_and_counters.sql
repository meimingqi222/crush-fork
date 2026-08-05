-- +goose Up
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN goal_tasks TEXT NOT NULL DEFAULT '[]';
ALTER TABLE sessions ADD COLUMN goal_max_iterations INTEGER NOT NULL DEFAULT 50;
ALTER TABLE sessions ADD COLUMN goal_block_cap INTEGER NOT NULL DEFAULT 8;
ALTER TABLE sessions ADD COLUMN goal_iterations INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN goal_no_progress INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN goal_last_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN goal_verifier_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN goal_last_evaluator_met INTEGER;
ALTER TABLE sessions ADD COLUMN goal_last_evaluator_at INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN goal_last_evaluator_at;
ALTER TABLE sessions DROP COLUMN goal_last_evaluator_met;
ALTER TABLE sessions DROP COLUMN goal_verifier_id;
ALTER TABLE sessions DROP COLUMN goal_last_reason;
ALTER TABLE sessions DROP COLUMN goal_no_progress;
ALTER TABLE sessions DROP COLUMN goal_iterations;
ALTER TABLE sessions DROP COLUMN goal_block_cap;
ALTER TABLE sessions DROP COLUMN goal_max_iterations;
ALTER TABLE sessions DROP COLUMN goal_tasks;
-- +goose StatementEnd
