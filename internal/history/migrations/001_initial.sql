-- The complete gonzbd schema.
--
-- This is the only migration. A second one is a decision rather than a routine
-- addition: history.Open refuses any database recording a version above the
-- highest migration shipped (refuseUnknownSchema, db.go), and that bound is
-- read from these filenames, so adding a file changes which databases open.
-- Standing Design Rule 1 governs the rest -- no installation is upgraded,
-- there is no backfill anywhere below, and none is possible.
--
-- internal/history/testdata/schema.golden is a dump of what this file
-- produces, down to every constraint and foreign key. It is the only thing
-- that notices a constraint quietly disappearing from the DDL here.

-- +goose Up

-- +goose StatementBegin
CREATE TABLE history (
    id              INTEGER PRIMARY KEY,
    completed       INTEGER,
    name            TEXT,
    nzb_name        TEXT,
    category        TEXT,
    pp              TEXT,
    script          TEXT,
    url             TEXT,
    status          TEXT,
    nzo_id          TEXT UNIQUE,
    storage         TEXT,
    path            TEXT,
    script_log      BLOB,
    script_line     TEXT,
    download_time   INTEGER,
    postproc_time   INTEGER,
    stage_log       TEXT,
    downloaded      INTEGER,
    completeness    INTEGER,
    fail_message    TEXT,
    url_info        TEXT,
    bytes           INTEGER,
    meta            TEXT,
    md5sum          TEXT,
    password        TEXT,
    archive         INTEGER DEFAULT 0,
    time_added      INTEGER,
    -- The basename of the gzipped NZB backup under admin/nzb/, so a retry can
    -- resolve it unambiguously. Deliberately not nzb_name: that column holds
    -- the filename the job was submitted under and is a compatibility surface
    -- (the mode=history response, the UI history row, the second positional
    -- argument to user scripts, the search predicate), while the backup's own
    -- name takes a .1/.2 suffix when a forced duplicate add would otherwise
    -- overwrite an existing backup. The two can differ, so they cannot share
    -- storage.
    nzb_backup      TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX idx_history_nzo_id ON history(nzo_id);
CREATE INDEX idx_history_archive_completed ON history(archive, completed DESC);
-- +goose StatementEnd

-- +goose StatementBegin
-- The per-file RESULTS of downloading a job: what each file turned out to be
-- called, whether it finished, what it hashed to, and whether it is fetched.
--
-- Every column is something no other artifact holds. filename is discovered
-- from the yEnc header, assembled_crc32 computed over the assembled bytes,
-- complete decided by the assembler, fetch_policy chosen by the on-demand par2
-- policy. Anything the manifest already says does not belong here: the
-- manifest is loaded before these rows are read, so a copy would only ever be
-- the stale one.
--
-- The fetch_policy CHECK is the only guard that value has -- neither
-- SetFileFetchPolicy nor RestoreFileMeta range-checks it.
--
-- UNIQUE(job_id, file_index) is also the access path, which is why there is no
-- separate index on job_id.
-- `git grep -nE 'INTO job_files|UPDATE job_files|FROM job_files' -- '*.go'
-- ':!*_test.go'` returns five statements: the INSERT, UPDATE, DELETE and
-- SELECT in internal/app, plus a SELECT in test/crash/harness.go, which that
-- filter keeps because it is build-tagged rather than named _test.go. Every
-- one keys on `job_id` or on `job_id AND file_index`, and both are prefixes of
-- that index -- so a second B-tree on job_id alone would be maintained on
-- every write to answer a lookup the first one already answers.
-- history_job_files reaches the same arrangement through
-- PRIMARY KEY (job_id, file_index).
CREATE TABLE job_files (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id           TEXT NOT NULL,
    file_index       INTEGER NOT NULL,
    complete         INTEGER NOT NULL DEFAULT 0,
    filename         TEXT,
    assembled_crc32  INTEGER DEFAULT 0,
    fetch_policy     INTEGER NOT NULL DEFAULT 0 CHECK (fetch_policy BETWEEN 0 AND 2),
    UNIQUE(job_id, file_index)
);
-- +goose StatementEnd

-- +goose StatementBegin
-- One row per maximal run of articles that abut in both byte offset and
-- article index and were made durable together: fsynced, then recorded in the
-- same transaction as the barrier's commit.
--
-- A row is only ever written AFTER the fsync that makes it true, never before
-- or during one. That ordering is what the table is for, and it is what makes
-- last-write-wins safe here -- merging is read-modify-write, so a commit may
-- delete and replace rows it owns, but a later commit can only ever describe
-- MORE durable bytes than an earlier one, never different ones.
--
-- first_art_idx and last_art_idx name which articles the run accounts for; the
-- resume set is the complement. offset and length bound the completion
-- truncate (max(offset+length)) and the overlap check (sum of length against
-- stat size). crc32 is combined left-to-right over the run's articles via
-- crc32util.Combine, so a file that collapses to one row starting at offset 0
-- has its whole-file CRC in that row -- no walk, no prefix state.
--
-- Runs are built in one place: RunStore.Commit takes individual articles
-- rather than runs, so there is no second caller able to construct one. See
-- internal/durability/run.go.
--
-- Keyed by job_id with no foreign key, so rows are removed deliberately rather
-- than by cascade. A job leaving the queue drops them, EXCEPT a job that
-- FAILED: those are retained beside its history_job_files row, because a retry
-- reuses the job ID over the same partial file and the retained runs bound the
-- completion truncate to the whole file rather than to the handful of articles
-- the retry re-fetched. history.Repository.Delete drops them with the history
-- entry, and RetryHistoryJob drops them when it declines to apply the retained
-- progress.
CREATE TABLE durable_runs (
    job_id        TEXT    NOT NULL,
    file_idx      INTEGER NOT NULL,
    first_art_idx INTEGER NOT NULL,
    last_art_idx  INTEGER NOT NULL,
    offset        INTEGER NOT NULL,
    length        INTEGER NOT NULL,
    crc32         INTEGER NOT NULL,
    PRIMARY KEY (job_id, file_idx, offset)
) WITHOUT ROWID;
-- +goose StatementEnd

-- +goose StatementBegin
-- Articles that permanently failed. With durable_runs it gives a job's article
-- resolution: done means covered by a run, failed means a row here, neither
-- means outstanding.
--
-- One production writer -- `git grep -n 'INTO failed_articles' -- '*.go'
-- ':!*_test.go'` returns one line, the INSERT OR IGNORE inside
-- appCheckpointStore.SaveBatch. Deletion is wider and job-scoped: the retry
-- path, the durability sweep for a job that has left both the queue and
-- history-as-FAILED, and history.Repository.Delete. None of those writes a
-- row.
--
-- A table rather than a bitmap column on job_files because its reversal is
-- per-article and per-job, which a packed blob can only express by rewriting
-- the whole thing.
CREATE TABLE failed_articles (
    job_id  TEXT    NOT NULL,
    art_idx INTEGER NOT NULL,
    PRIMARY KEY (job_id, art_idx)
) WITHOUT ROWID;
-- +goose StatementEnd

-- +goose StatementBegin
-- Per-file download progress retained for a FAILED job, so a retry refetches
-- only the articles that did not make it.
--
-- Separate from job_files because those rows do not outlive the
-- queue-to-history transition. No foreign key here either: the owning row is
-- history(nzo_id), and these are removed explicitly when it is deleted. Only
-- failed jobs get rows -- a job that succeeded has nothing to retry.
--
-- A progress overlay, not a second manifest. article_count is the exception to
-- that and is here for one purpose: retainedMatchesManifest compares it
-- against the re-parsed NZB's file ranges, so an overlay that no longer lines
-- up with the NZB is refused rather than applied to the wrong articles.
CREATE TABLE history_job_files (
    job_id           TEXT NOT NULL,
    file_index       INTEGER NOT NULL,
    complete         INTEGER NOT NULL DEFAULT 0,
    filename         TEXT,
    assembled_crc32  INTEGER DEFAULT 0,
    article_count    INTEGER NOT NULL DEFAULT 0,
    fetch_policy     INTEGER NOT NULL DEFAULT 0 CHECK (fetch_policy BETWEEN 0 AND 2),
    PRIMARY KEY (job_id, file_index)
);
-- +goose StatementEnd

-- +goose StatementBegin
-- internal/dispatch/store's table and nothing else's: job identity, the header
-- metadata job.Job does not carry, the resolved policy, the state axes, and
-- the download timestamps.
--
-- The axes hold the integer values of internal/job's uint8 enums and carry no
-- CHECK constraint. That is deliberate: a restored row is replayed forward
-- through job.Job's own doors by dispatch.reconstruct, so an illegal position,
-- an inadmissible outcome for its state, or a `next` that is not a legal edge
-- is refused by the state machine. A CHECK could only re-validate the enum
-- RANGE -- it cannot know the transition table -- and would be a second
-- enforcement point for one invariant.
--
-- `crossed` is absent because it is derived from `state` via Attempt.crossed.
-- Storing it would create a second source of truth able to disagree after a
-- restore.
CREATE TABLE dispatch_jobs (
    id          TEXT PRIMARY KEY,
    -- Queue order: a monotonic insertion sequence assigned once, at
    -- registration. It survives a removal without renumbering because the only
    -- two operations that change the dispatcher's order are an append and an
    -- order-preserving delete. Nothing revises it today, which is a statement
    -- about the operations that exist rather than a property of the design --
    -- entry.seq in internal/dispatch/registry.go records why arbitrary
    -- reordering will require renumbering, since restore rebuilds order from
    -- this column alone.
    sort_key    INTEGER NOT NULL,

    -- Header: display metadata supplied by the caller at Add.
    name        TEXT NOT NULL,
    category    TEXT NOT NULL DEFAULT '',
    priority    INTEGER NOT NULL DEFAULT 0,
    bytes       INTEGER NOT NULL DEFAULT 0,

    -- Policy: what the job is permitted to do, resolved at ingestion. Stored
    -- resolved rather than as the upstream PP integer, which is external
    -- vocabulary that does not exist past App (internal/job/policy.go).
    verify      INTEGER NOT NULL DEFAULT 0,
    repair      INTEGER NOT NULL DEFAULT 0,
    unpack      INTEGER NOT NULL DEFAULT 0,
    delete_ok   INTEGER NOT NULL DEFAULT 0,

    -- The StateView axes. `assessed` is not derivable from `state`: Fetching
    -- with assessed set is a job that has been through Assessing and returned,
    -- a different position from a first-pass Fetching, and it decides the path
    -- reconstruct replays.
    state       INTEGER NOT NULL DEFAULT 0,
    next        INTEGER NOT NULL DEFAULT 0,
    activity    INTEGER NOT NULL DEFAULT 0,
    outcome     INTEGER NOT NULL DEFAULT 0,
    assessed    INTEGER NOT NULL DEFAULT 0,

    intent      INTEGER NOT NULL DEFAULT 0,

    filename          TEXT NOT NULL DEFAULT '',
    -- Five independent, single-owner facts replacing what used to be one
    -- append-only `warning` string. Each is written by exactly one caller
    -- and never combined with another: ingest_anomaly and duplicate_reason
    -- describe the NZB as ingested and never change after (BuildIngestJob
    -- and AddJob's detectDuplicateNZB, respectively); post_anomaly is set by
    -- postAnomaly when the assembler or durability barrier finds a
    -- byte-accounting collision after the job has started downloading
    -- (#379); fail_reason is set by Fail when a permanent storage fault
    -- stops the job (R20/R27), and is live only for the window before
    -- finalization writes history.Entry.FailMessage; operational_error is
    -- the one queue-management case, set by persistAndCommit when
    -- dispatcher removal fails after finalization. No app build has ever
    -- shipped the single `warning` column, so this is a straight field
    -- split in the base migration rather than a follow-up one.
    ingest_anomaly    TEXT NOT NULL DEFAULT '',
    post_anomaly      TEXT NOT NULL DEFAULT '',
    fail_reason       TEXT NOT NULL DEFAULT '',
    duplicate_reason  TEXT NOT NULL DEFAULT '',
    operational_error TEXT NOT NULL DEFAULT '',
    script            TEXT NOT NULL DEFAULT '',
    password          TEXT NOT NULL DEFAULT '',
    -- The unresolved upstream PP integer, kept because the API reports it back
    -- verbatim. It is also read directly: DirectUnpack gates on `pp < 2`, and
    -- postproc skips stages by it. Those are second readings of permissions
    -- the resolved columns above already express.
    pp                INTEGER NOT NULL DEFAULT 0,
    nzb_backup        TEXT NOT NULL DEFAULT '',
    url               TEXT NOT NULL DEFAULT '',
    md5               TEXT NOT NULL DEFAULT '',
    added             INTEGER NOT NULL DEFAULT 0,
    download_started  INTEGER NOT NULL DEFAULT 0,
    download_finished INTEGER NOT NULL DEFAULT 0,

    -- Why the on-demand par2 verdict released or withheld this job's recovery
    -- volumes. Only its EMPTINESS is load-bearing: a non-empty value marks
    -- that a verdict was reached, which distinguishes "volumes held because
    -- nothing could be identified" from "volumes still awaiting a verdict".
    -- No control flow branches on the text. It is persisted because a job that
    -- reached a verdict sits at StatusVerifying, which re-enqueues for
    -- post-processing after a restart; without it buildDownloadFileList falls
    -- through to the "verified clean from index" line for a job where nothing
    -- was verified.
    par2_release_reason TEXT NOT NULL DEFAULT '',

    -- Repair capacity, counted from subjects matching ".volNNN+MM.par2" -- the
    -- files carrying recovery slices -- and not the set's index file, which
    -- holds per-file checksums and no capacity. The convention is not a
    -- guarantee: PAR2 recommends the .vol naming without requiring it, and
    -- par2 reads packets rather than filenames, so zero here means "nothing
    -- recognized", never "nothing to repair with".
    --
    -- Derived from the manifest (Manifest.RecoveryBytes) rather than measured
    -- here, but persisted rather than recomputed on load, because a
    -- non-resident job has no manifest and because zero is not a missing
    -- reading -- it is a definite claim of no repair capacity, which the UI
    -- renders as "No repair data" and both abort gates read as grounds to call
    -- a job hopeless. That must not be reachable from a degraded read.
    recovery_bytes    INTEGER NOT NULL DEFAULT 0,

    -- Set when on-demand par2 un-deferred this job's recovery volumes because
    -- repair was needed. Without it a job repaired and then restarted before
    -- finalize prints no par2 summary line at all: every arm of that switch
    -- tests either this flag or a held-volume count that un-deferring has
    -- already driven to zero.
    par2_recovered    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_dispatch_jobs_sort_key ON dispatch_jobs(sort_key);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE dispatch_jobs;
DROP TABLE history_job_files;
DROP TABLE failed_articles;
DROP TABLE durable_runs;
DROP TABLE job_files;
DROP TABLE history;
-- +goose StatementEnd
