-- +goose Up
-- +goose StatementBegin
ALTER TABLE jobs ADD COLUMN download_started INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN download_finished INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE jobs DROP COLUMN download_finished;
ALTER TABLE jobs DROP COLUMN download_started;
-- +goose StatementEnd
