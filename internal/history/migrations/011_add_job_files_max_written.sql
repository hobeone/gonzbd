-- Add max_written: a file's decoded high-water mark, the highest byte position
-- the assembler has written.
--
-- The assembler truncates a completed file to that mark, to strip the trailing
-- zeros pre-allocation leaves behind — it sizes the file to the NZB's yEnc
-- ENCODED byte count, ~2% above the decoded payload, and par2 reports those
-- zeros as damage. Until now the mark lived only in memory, so a resumed run
-- started it at zero and rebuilt it from the articles that run received. A
-- resumed file's TotalParts counts only the articles still outstanding, so it
-- completes as soon as those arrive, and the truncate then cut the file down to
-- whatever this run happened to receive — discarding data an earlier run had
-- already written and fsynced. See #342.
--
-- write_cursor does not solve this. It is the CONTIGUOUS frontier, so it
-- normally lags the high-water mark: articles arriving out of order leave it
-- behind, which is the usual case for a multi-connection download. The
-- assembler still uses it as a fallback seed, but it is not a bound on the
-- file either — drainFile advances it across everything it hands back, before
-- any write is attempted — so finalizeFile refuses to truncate upward rather
-- than trusting it.
--
-- There is no backfill, and none is possible. The figure describes bytes on
-- disk, and nothing already stored records it: bytes and bytes_downloaded are
-- both yEnc-encoded NZB counts, and stat-ing the file returns the inflated
-- pre-allocated size rather than the decoded extent, precisely because the
-- truncate that would have trimmed it is the step that did not happen. A job
-- already in flight when this migration applies therefore resumes with 0 here,
-- which is exactly today's behaviour — the fix takes effect for files whose
-- next write happens after the upgrade, and the write_cursor floor covers the
-- in-order case meanwhile.
--
-- The column is added to history_job_files as well. That table retains a failed
-- job's per-file progress so a retry can resume rather than refetch, and a
-- column present in job_files but absent there would be silently dropped on
-- every retry — the one path where resume state matters most.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE job_files ADD COLUMN max_written INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE history_job_files ADD COLUMN max_written INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- Down drops both. No data is lost that can be reconstructed: the figure is
-- re-derived from scratch on the next run, and a down-migrated database simply
-- returns to truncating resumed files to the current run's extent.
-- +goose Down
-- +goose StatementBegin
ALTER TABLE job_files DROP COLUMN max_written;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE history_job_files DROP COLUMN max_written;
-- +goose StatementEnd
