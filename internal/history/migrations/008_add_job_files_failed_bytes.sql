-- +goose Up
-- +goose StatementBegin
ALTER TABLE job_files ADD COLUMN failed_bytes INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE job_files DROP COLUMN failed_bytes;
-- +goose StatementEnd
