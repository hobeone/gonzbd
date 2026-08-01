-- +goose Up
-- +goose StatementBegin
ALTER TABLE job_files ADD COLUMN article_count INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE job_files DROP COLUMN article_count;
-- +goose StatementEnd
