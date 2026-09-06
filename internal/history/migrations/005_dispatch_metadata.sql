-- Persists job metadata, timestamps, and par2_release_reason in dispatch_jobs.
--
-- This migration supersedes the frozen comments in 004_par2_release_reason.sql
-- and makes dispatch_jobs the sole authoritative owner of all job metadata
-- (filename, warning, script, password, pp, nzb_backup, url, md5) and timestamps
-- (added, download_started, download_finished) plus par2_release_reason, retiring
-- the legacy jobs table.
--
-- +goose Up
-- +goose StatementBegin
ALTER TABLE dispatch_jobs ADD COLUMN filename TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatch_jobs ADD COLUMN warning TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatch_jobs ADD COLUMN script TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatch_jobs ADD COLUMN password TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatch_jobs ADD COLUMN pp INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dispatch_jobs ADD COLUMN nzb_backup TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatch_jobs ADD COLUMN url TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatch_jobs ADD COLUMN md5 TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatch_jobs ADD COLUMN added INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dispatch_jobs ADD COLUMN download_started INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dispatch_jobs ADD COLUMN download_finished INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dispatch_jobs ADD COLUMN par2_release_reason TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE dispatch_jobs DROP COLUMN par2_release_reason;
ALTER TABLE dispatch_jobs DROP COLUMN download_finished;
ALTER TABLE dispatch_jobs DROP COLUMN download_started;
ALTER TABLE dispatch_jobs DROP COLUMN added;
ALTER TABLE dispatch_jobs DROP COLUMN md5;
ALTER TABLE dispatch_jobs DROP COLUMN url;
ALTER TABLE dispatch_jobs DROP COLUMN nzb_backup;
ALTER TABLE dispatch_jobs DROP COLUMN pp;
ALTER TABLE dispatch_jobs DROP COLUMN password;
ALTER TABLE dispatch_jobs DROP COLUMN script;
ALTER TABLE dispatch_jobs DROP COLUMN warning;
ALTER TABLE dispatch_jobs DROP COLUMN filename;
-- +goose StatementEnd
