-- Persist par2_recovered in dispatch_jobs.
--
-- JobProgress.par2Recovered records that on-demand par2 un-deferred a job's
-- recovery volumes because repair was needed. It was the last piece of the
-- on-demand par2 state with no durable home: par2_release_reason arrived in
-- 005 and recovery_bytes in 006, but this flag lived only in memory, so a job
-- that was repaired and then restarted before finalize printed no par2 summary
-- line at all -- neither "fetched N recovery volume(s) for repair" nor
-- anything else, because every arm of that switch tests either the flag or a
-- held-volume count that un-deferring has already driven to zero.
--
-- This migration supersedes nothing. 005 and 006 each corrected a claim frozen
-- in an earlier file; this one only adds a column, and saying otherwise would
-- freeze a false statement in a file that must not be edited afterwards.
--
-- Note for anyone reading this while planning a downgrade: the chain does not
-- run backwards past 006, whose Down is a comment declaring in-place downgrade
-- unsupported. This migration's own Down is exact, but it is the last one that
-- is.
--
-- +goose Up
-- +goose StatementBegin
ALTER TABLE dispatch_jobs ADD COLUMN par2_recovered INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE dispatch_jobs DROP COLUMN par2_recovered;
-- +goose StatementEnd
