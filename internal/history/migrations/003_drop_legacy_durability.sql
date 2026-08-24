-- Drops the two-record durability store 002 replaced, and the queue's
-- independent per-article copy of the same state.
--
-- 002 only CREATEd durable_runs and failed_articles; every reader and writer
-- has now moved to them, so article_facts and file_extents have neither. The
-- split is what made the flip landable as one commit: a single migration that
-- both created the replacement and dropped the original could not be
-- half-applied.
--
-- See docs/superpowers/specs/2026-08-22-single-durability-record-design.md
-- §1-§2 for why the two-writer shape was the source of #389 and #421.
--
-- +goose Up
-- +goose StatementBegin
DROP TABLE article_facts;
-- Class A. An immutable per-article fact appended at DECODE time, asserting
-- only "if these bytes are present, they hash to this CRC32" — with no
-- ordering against the write (R2), which is precisely what let it describe
-- bytes that were never written. durable_runs supersedes it with a record
-- written only after the fsync that makes it true.
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE file_extents;
-- Class B. A per-file cache of values derived from Class A plus the file's
-- bytes: a durable bitmap, a verified-prefix length and its CRC, a byte
-- count, and an (size, mtime) validity stamp. Every one of those is either
-- carried by a durable_runs row or deleted outright — see the design doc's
-- "What this deletes", and §3.4 for why S7's mtime half is deleted rather
-- than merely unused.
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE job_files DROP COLUMN articles_done;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE history_job_files DROP COLUMN articles_done;
-- The queue's own per-article done/failed state, packed as two concatenated
-- hex bitmaps. It was a THIRD copy of what the two durability tables already
-- held between them, maintained by a wholesale re-serialisation on every job
-- update. Article resolution is now derived: done means covered by a
-- durable_runs row, failed means a failed_articles row, and neither means
-- outstanding.
--
-- This supersedes 001_initial.sql's job_files comment block, which describes
-- articles_done as "the authoritative record of which of the file's articles
-- are resolved and which permanently failed" and explains failed_bytes and
-- bytes_downloaded as caches of sums over it. Those two columns STAY and are
-- still caches, but of the derived resolution above rather than of a column
-- on their own row. They remain written by the same statement that writes the
-- rest of the row, so neither can be persisted out of step with what it
-- summarises, and both are still superseded wholesale by JobProgress.recompute
-- on promotion.
--
-- 001's claim that failed_bytes "cannot live in file_extents ... a permanently
-- failed article never decodes, so it never writes an article_facts row at
-- all" is likewise superseded in its reasoning and unchanged in its
-- conclusion: failed_articles now records the failure, but it records only
-- WHICH articles failed, never how many bytes they were, so the column is
-- still the only home for the figure a non-resident job reports.
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE history_job_files ADD COLUMN articles_done TEXT;
ALTER TABLE job_files ADD COLUMN articles_done TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE file_extents (
    job_id         TEXT    NOT NULL,
    file_idx       INTEGER NOT NULL,
    durable_bitmap BLOB    NOT NULL,
    verified_to    INTEGER NOT NULL DEFAULT 0,
    prefix_crc     INTEGER NOT NULL DEFAULT 0,
    has_prefix_crc INTEGER NOT NULL DEFAULT 0,
    bytes_durable  INTEGER NOT NULL DEFAULT 0,
    size           INTEGER NOT NULL DEFAULT 0,
    mod_time_ns    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (job_id, file_idx)
) WITHOUT ROWID;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE article_facts (
    job_id   TEXT    NOT NULL,
    art_idx  INTEGER NOT NULL,
    file_idx INTEGER NOT NULL,
    offset   INTEGER NOT NULL,
    length   INTEGER NOT NULL,
    crc32    INTEGER NOT NULL,
    PRIMARY KEY (job_id, art_idx)
) WITHOUT ROWID;
CREATE INDEX idx_article_facts_file ON article_facts(job_id, file_idx, offset);
-- The Down direction restores the SHAPE of these tables and none of their
-- contents, which is all a down migration can honestly offer here: the rows
-- were derived from files this daemon wrote, and nothing in the database can
-- reconstruct them. A rolled-back installation re-downloads.
-- +goose StatementEnd
