-- Persists when a job was added to the queue, on dispatch_jobs rather than
-- jobs: internal/dispatch.Header is the header-tier row this column belongs
-- to, and jobs.time_added is internal/queue's column, on its way out with the
-- rest of that package (see 001_initial.sql's dispatch_jobs comment block).
--
-- Added by task-9's fix round: internal/downloader's propagation-delay gate
-- (skip a freshly-posted file until it has had time to reach every server)
-- was keyed on this quantity via queue.UnfinishedArticle.JobAdded before the
-- swap, and job.Job carries no equivalent timestamp -- Added is the
-- dispatch-tier home for it, supplied by the caller at Add the same way
-- Header.Name/Category/Priority/Bytes already are.
--
-- Encoded as an INTEGER unix timestamp, matching jobs.time_added and
-- job_progress.download_started/download_finished (001_initial.sql) and the
-- isJobStamp() convention in internal/job/progress.go: 0 reads as "absent",
-- so a pre-existing row with no Added value decodes to time.Time{} rather
-- than to an ambiguous epoch instant. Standing Design Rule 1 applies --
-- rows an earlier build wrote may be assumed to satisfy this column's
-- invariants -- so a restored row with Added=0 is simply a job whose
-- propagation-delay clock reads as zero, not a migration hazard.
--
-- +goose Up
-- +goose StatementBegin
ALTER TABLE dispatch_jobs ADD COLUMN added INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE dispatch_jobs DROP COLUMN added;
-- +goose StatementEnd
