-- +goose Up
-- +goose StatementBegin
-- Per-file download progress retained for a FAILED job so a retry can refetch
-- only the articles that did not make it.
--
-- Why a separate table rather than keeping the job_files rows: job_files is
-- REFERENCES jobs(id) ON DELETE CASCADE and MoveToHistory deletes the jobs
-- row, so those rows cannot outlive the queue→history transition where they
-- sit. There is deliberately no foreign key here either — the owning row is
-- history(nzo_id), and the rows are removed explicitly when that entry is
-- deleted.
--
-- Only failed jobs get rows. A job that succeeded has nothing to retry, and
-- writing this for every completed download is what made the format this
-- replaces grow without bound.
--
-- This is a progress overlay, not a second manifest: subject, date, bytes and
-- is_par2_recovery are absent because a retry rebuilds them by re-parsing the
-- NZB. article_count is kept solely so the overlay can be refused when the
-- re-parsed NZB does not line up with the bitmap.
CREATE TABLE history_job_files (
    job_id           TEXT NOT NULL,
    file_index       INTEGER NOT NULL,
    complete         INTEGER NOT NULL DEFAULT 0,
    deferred         INTEGER NOT NULL DEFAULT 0,
    write_cursor     INTEGER NOT NULL DEFAULT 0,
    bytes_downloaded INTEGER NOT NULL DEFAULT 0,
    filename         TEXT,
    assembled_crc32  INTEGER DEFAULT 0,
    articles_done    TEXT,
    article_count    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (job_id, file_index)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE history_job_files;
-- +goose StatementEnd
