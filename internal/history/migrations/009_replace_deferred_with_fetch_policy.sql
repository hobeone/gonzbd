-- +goose Up
-- +goose StatementBegin
ALTER TABLE job_files ADD COLUMN fetch_policy INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE job_files SET fetch_policy = 1 WHERE deferred = 1;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE job_files DROP COLUMN deferred;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE history_job_files ADD COLUMN fetch_policy INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE history_job_files SET fetch_policy = 1 WHERE deferred = 1;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE history_job_files DROP COLUMN deferred;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE job_files ADD COLUMN deferred INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE job_files SET deferred = 1 WHERE fetch_policy = 1;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE job_files DROP COLUMN fetch_policy;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE history_job_files ADD COLUMN deferred INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE history_job_files SET deferred = 1 WHERE fetch_policy = 1;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE history_job_files DROP COLUMN fetch_policy;
-- +goose StatementEnd
