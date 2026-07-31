-- +goose Up
-- +goose StatementBegin
ALTER TABLE job_files ADD COLUMN articles_failed TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE job_files DROP COLUMN articles_failed;
-- +goose StatementEnd
