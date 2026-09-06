-- Persist recovery_bytes in dispatch_jobs and drop legacy jobs table.
--
-- This migration supersedes 002_durable_runs.sql (designating
-- checkpoint.Store.SaveBatch as the writer of failed_articles)
-- and 005_dispatch_metadata.sql by physically dropping the retired
-- legacy jobs table.
--
-- +goose Up
-- +goose StatementBegin
ALTER TABLE dispatch_jobs ADD COLUMN recovery_bytes INTEGER NOT NULL DEFAULT 0;
DROP TABLE jobs;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- In-place downgrade is not supported.
-- +goose StatementEnd
