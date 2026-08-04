-- +goose Up
-- +goose StatementBegin
-- The basename of the gzipped NZB backup this job was written to under
-- admin/nzb/, recorded so a retry can resolve it unambiguously.
--
-- This is deliberately not history.nzb_name. That column holds the filename
-- the job was submitted under and is a compatibility surface: it appears in
-- the mode=history API response, the web UI's history row, the second
-- positional argument handed to user post-processing scripts, and the
-- history search predicate. The backup's own name can diverge from it when a
-- forced duplicate add takes a .1/.2 suffix to avoid overwriting an existing
-- backup, so the two cannot share storage.
ALTER TABLE jobs ADD COLUMN nzb_backup TEXT NOT NULL DEFAULT '';
ALTER TABLE history ADD COLUMN nzb_backup TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE jobs DROP COLUMN nzb_backup;
ALTER TABLE history DROP COLUMN nzb_backup;
-- +goose StatementEnd
