-- The complete gonzbd schema, as one migration.
--
-- This file has now absorbed two chains. It first replaced 001-011, and then
-- 002-007: durable_runs and failed_articles (002), the removal of the
-- two-record durability store and the articles_done bitmaps (003), the par2
-- release reason (004, 005), the dispatch_jobs metadata and timestamp columns
-- (005), recovery_bytes and the retirement of the legacy jobs table (006), and
-- par2_recovered (007).
--
-- WHY COLLAPSE RATHER THAN EXTEND
--
-- A chain records how the schema was reached; this file records what it is.
-- Those differ once a migration deletes something, and by the end of 002-007
-- they had diverged badly: the chain CREATEd three tables it later DROPped
-- (jobs, article_facts, file_extents), declared 40 columns and 4 indexes that
-- no running build ever had, and spent one entire migration (004) adding a
-- column to a table two migrations later removed. A reader opening the schema
-- to learn the schema was told things that stopped being true further down.
--
-- The rationale drifted the same way. Every column added by ALTER since the
-- last collapse carried its justification in a separate file, so the table
-- definition showed a bare column and the reason for it lived somewhere the
-- reader had no cause to open. Collapsing puts each claim beside the thing it
-- explains, which is the only arrangement that keeps them checkable together.
--
-- WHY IT IS SAFE
--
-- Standing Design Rule 1: no installation is being upgraded. The on-disk
-- database from an earlier build is not migrated and cannot be read by this
-- version. There is no backfill anywhere below and none is possible.
--
-- "Cannot be read" is ENFORCED rather than assumed. goose keys on version
-- numbers alone, so a database written by any earlier build records versions
-- this file does not ship while version 1 reads as already applied: Up() would
-- apply nothing and return nil, and the daemon would come up clean against a
-- schema it cannot use. history.Open refuses a database whose recorded version
-- exceeds the highest migration the build ships (refuseUnknownSchema, db.go).
-- That bound is read from these filenames, not hard-coded, so it moves on its
-- own if a later migration is ever added.
--
-- This collapse was verified rather than asserted: the schema the old chain
-- produced and the schema this file produces were dumped from sqlite_master
-- and compared token-for-token. internal/history/testdata/schema.golden is the
-- standing form of that check -- it records every table, column, constraint
-- and foreign key, and TestMigrations_GoldenSchema fails on any change to
-- them. A constraint deleted from this file leaves no trace anywhere else, so
-- that test is the only thing standing behind the DDL below.

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
    report          TEXT,
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
    series          TEXT,
    md5sum          TEXT,
    password        TEXT,
    duplicate_key   TEXT,
    archive         INTEGER DEFAULT 0,
    time_added      INTEGER,
    -- nzb_backup is the basename of the gzipped NZB backup under admin/nzb/,
    -- recorded so a retry can resolve it unambiguously. It is deliberately not
    -- nzb_name: that column holds the filename the job was submitted under and
    -- is a compatibility surface -- it appears in the mode=history API response,
    -- the web UI's history row, the second positional argument handed to user
    -- post-processing scripts, and the history search predicate. The backup's
    -- own name can diverge from it when a forced duplicate add takes a .1/.2
    -- suffix to avoid overwriting an existing backup, so the two cannot share
    -- storage.
    nzb_backup      TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX idx_history_nzo_id ON history(nzo_id);
CREATE INDEX idx_history_archive_completed ON history(archive, completed DESC);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE queue_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
-- The per-file rows of a queued job's manifest.
--
-- This table carries almost no derived progress columns. max_written and
-- write_cursor used to live here, each a value summarising facts stored
-- elsewhere and each maintained independently of them. That is the direct
-- cause of #337 (one member of a set stored while its siblings are derived)
-- and #311 (a cursor serving as cache, authority, and scheduling hint at
-- once).
--
-- articles_done used to live here too, a per-article done/failed pair of
-- packed hex bitmaps rewritten wholesale on every job update. It was a third
-- copy of state the durability tables already held, and it is gone: article
-- resolution is now DERIVED -- done means covered by a durable_runs row,
-- failed means a failed_articles row, and neither means outstanding.
--
-- failed_bytes and bytes_downloaded survived that removal, and both are caches
-- rather than second authorities. Each caches a sum over one half of the
-- derived resolution above. Both are written by the same statement that writes
-- the rest of this row, so a sum cannot be persisted out of step with the row
-- it belongs to; and both are superseded wholesale on promotion, where
-- JobProgress.recompute ASSIGNS them from the manifest and the restored runs.
-- They exist for the NON-resident path, which has no manifest to recompute
-- from and would otherwise report an inflated remaining figure for every job
-- beyond maxActive.
--
-- failed_bytes has no home in the durability tables. failed_articles records
-- WHICH articles failed and never how many bytes they were, so no
-- recomputation from durable state can produce this figure. That was equally
-- true of the two-record store this replaced, for the same reason in different
-- words: a permanently failed article never decodes, so it never produced a
-- durability row at all.
--
-- bytes_downloaded could be derived from durable state, and was, until that
-- turned out to be the wrong QUANTITY rather than an unavailable one. The two
-- count different things. This column counts ENCODED bytes -- the NZB `bytes`
-- attribute summed over resolved articles -- because that is what it is
-- compared against: the remaining figure is this row's `bytes` minus this
-- column minus failed_bytes, and `bytes` is the encoded per-file total from
-- the same NZB. A durable run's `length` counts DECODED payload bytes, the
-- lengths the assembler actually wrote, which run a few percent lower. Seeding
-- one from the other made a non-resident job overstate its remaining bytes by
-- that margin, breaking the residency parity both columns exist to provide.
--
-- Keeping these is not a reversal of the removals above. bytes_downloaded was
-- once removed because RestoreRetryProgress assigned it and recompute then
-- overwrote it -- two writers maintaining one fact in parallel, which is the
-- S5 violation behind #306. That path is gone. A single writer caching a sum
-- of the same row's resolution is a cache; two writers maintaining a value in
-- parallel is the defect.
CREATE TABLE job_files (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id           TEXT NOT NULL,
    file_index       INTEGER NOT NULL,
    subject          TEXT NOT NULL,
    date             INTEGER NOT NULL,
    bytes            INTEGER NOT NULL,
    is_par2_recovery INTEGER NOT NULL DEFAULT 0,
    complete         INTEGER NOT NULL DEFAULT 0,
    filename         TEXT,
    assembled_crc32  INTEGER DEFAULT 0,
    article_count    INTEGER NOT NULL DEFAULT 0,
    fetch_policy     INTEGER NOT NULL DEFAULT 0 CHECK (fetch_policy BETWEEN 0 AND 2),
    failed_bytes     INTEGER NOT NULL DEFAULT 0,
    bytes_downloaded INTEGER NOT NULL DEFAULT 0,
    UNIQUE(job_id, file_index)
);

CREATE INDEX idx_job_files_job_id ON job_files(job_id);
-- +goose StatementEnd

-- +goose StatementBegin
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
-- One row per maximal run of articles that abut in both byte offset and
-- article index and were made durable together (fsynced, then recorded in the
-- same transaction as the barrier's commit).
--
-- This replaced a pairing of two tables: an immutable per-article fact
-- appended at DECODE time, and a per-file cache derived from those facts plus
-- the file's bytes. The per-article record asserted only "if these bytes are
-- present, they hash to this CRC32", with no ordering against the write, which
-- is precisely what let it describe bytes that were never written -- the
-- source of #389 and #421. A run is written only after the fsync that makes it
-- true, so the ordering is structural rather than a rule a writer must follow.
--
-- Last-write-wins, not append-only: merging is read-modify-write by
-- construction, so a commit here may delete and replace rows it owns. That is
-- safe because a row is only ever written after a completed fsync, never
-- before or during one, so a later commit can only describe MORE durable bytes
-- than an earlier one, not different ones.
--
-- first_art_idx and last_art_idx name which articles this run accounts for;
-- the resume set is the complement. offset and length bound the completion
-- truncate (max(offset+length)) and the overlap check (sum of length vs stat
-- size). crc32 is combined left-to-right over the run's articles via
-- crc32util.Combine; when a file collapses to one row starting at offset 0,
-- that row's crc32 IS the whole-file CRC -- no walk, no prefix state.
--
-- The store is the only place a run is ever built: RunStore.Commit takes
-- individual articles, not runs. See internal/durability/run.go for why a
-- second call site constructing runs would make the required dedup
-- unreachable.
--
-- These rows are keyed by job_id with no foreign key to cascade from, so they
-- are removed deliberately rather than automatically. A job that leaves the
-- queue drops them, EXCEPT a job that FAILED -- those are retained alongside
-- its history_job_files row, because a retry reuses the job ID over the same
-- partial file and the retained runs are what bound the completion truncate to
-- the whole file rather than to the handful of articles the retry re-fetched.
-- They are dropped with the history entry itself, in history.Repository.Delete.
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE failed_articles (
    job_id  TEXT    NOT NULL,
    art_idx INTEGER NOT NULL,
    PRIMARY KEY (job_id, art_idx)
) WITHOUT ROWID;
-- Articles that permanently failed, so a non-resident job can answer "which
-- articles are outstanding?" without its manifest.
--
-- One production writer. `git grep -n 'INTO failed_articles' -- '*.go'
-- ':!*_test.go'` returns one line: internal/app/dispatcher_wiring.go:107, the
-- INSERT OR IGNORE prepared inside appCheckpointStore.SaveBatch -- the sole
-- production implementation of the checkpoint.Store interface
-- (internal/checkpoint/checkpointer.go:25). Nothing in this package writes a
-- row.
--
-- Deletion is deliberately not so narrow, and calling the table "solely owned"
-- would misdescribe it. Rows are removed on the retry path
-- (internal/app/app.go:2149), by the durability sweep for a job that has left
-- both the queue and history-as-FAILED (internal/app/durability.go:1410), and
-- by history.Repository.Delete dropping a departed job's durability with it
-- (internal/history/repository.go:403). Every one of those is a job-scoped
-- delete; none of them writes a row.
--
-- It is a table rather than a packed bitmap column on job_files because its
-- reversal is per-article and per-job -- a bitmap column can only express that
-- by rewriting the whole blob, which is the articles_done fragility this
-- design removed rather than reintroduced here.
-- +goose StatementEnd

-- +goose StatementBegin
-- Per-file download progress retained for a FAILED job so a retry can refetch
-- only the articles that did not make it.
--
-- Why a separate table rather than keeping the job_files rows: job_files rows
-- do not outlive the queue-to-history transition where they sit. There is
-- deliberately no foreign key here either -- the owning row is history(nzo_id),
-- and the rows are removed explicitly when that entry is deleted.
--
-- Only failed jobs get rows. A job that succeeded has nothing to retry, and
-- writing this for every completed download is what made the format this
-- replaces grow without bound.
--
-- This is a progress overlay, not a second manifest: subject, date, bytes and
-- is_par2_recovery are absent because a retry rebuilds them by re-parsing the
-- NZB. article_count is kept solely so the overlay can be refused when the
-- re-parsed NZB does not line up with the retained durability rows.
--
-- Like job_files, this table carries no derived byte or cursor columns, and no
-- articles_done bitmap. Nothing a retry needs is lost: which articles
-- succeeded is derived from the durable_runs and failed_articles rows retained
-- for the same job, and the byte and cursor figures were only ever summaries
-- of that. Assigning stored copies first and then replaying, as removed code
-- did, counted the same article's bytes twice.
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
-- dispatch_jobs is internal/dispatch/store's table and nothing else's.
--
-- It began beside a `jobs` table owned by the since-deleted internal/queue,
-- duplicating that table's identity and header columns because two writers to
-- one row could let a lossy `status` projection disagree with the `state`
-- recorded here -- the second-writer smell Standing Design Rule 2 names. The
-- duplication was the stated price of the split and it has now been paid back:
-- `jobs` is gone and this table is the sole authoritative home of job
-- identity, header metadata, the state axes, and the download timestamps.
--
-- The axes are stored as the integer values of internal/job's uint8 enums.
-- They carry no CHECK constraint, and that is not an oversight: a restored row
-- is replayed forward through job.Job's own doors by dispatch.reconstruct, so
-- an illegal position, an inadmissible outcome for its state, or a `next` that
-- is not a legal edge is refused by the state machine itself. A CHECK could
-- only re-validate the enum RANGE -- it cannot know the transition table --
-- and would be a second enforcement point for one invariant. Contrast
-- job_files.fetch_policy above, whose CHECK is the only guard that value has.
--
-- `crossed` is absent on purpose: it is derived from `state` via
-- Attempt.crossed, and storing it would create a second source of truth that
-- could disagree with `state` after a restore.
CREATE TABLE dispatch_jobs (
    id          TEXT PRIMARY KEY,
    -- sort_key is queue order: a monotonic insertion sequence the dispatcher
    -- assigns once, at registration, and never revises. It survives a removal
    -- without renumbering because the only two operations that change the
    -- dispatcher's order are an append and an order-preserving delete.
    sort_key    INTEGER NOT NULL,

    -- Header: the display metadata job.Job does not carry, supplied by the
    -- caller at Add.
    name        TEXT NOT NULL,
    category    TEXT NOT NULL DEFAULT '',
    priority    INTEGER NOT NULL DEFAULT 0,
    bytes       INTEGER NOT NULL DEFAULT 0,

    -- Policy: what the job is permitted to do, resolved at ingestion. Stored
    -- resolved rather than as the upstream PP integer, because PP is external
    -- vocabulary that "does not exist past App" (internal/job/policy.go).
    verify      INTEGER NOT NULL DEFAULT 0,
    repair      INTEGER NOT NULL DEFAULT 0,
    unpack      INTEGER NOT NULL DEFAULT 0,
    delete_ok   INTEGER NOT NULL DEFAULT 0,

    -- The StateView axes. `assessed` is not derivable from `state`: Fetching
    -- with assessed set is a job that has been through Assessing and returned,
    -- which is a different position from a first-pass Fetching, and it decides
    -- the path reconstruct replays.
    state       INTEGER NOT NULL DEFAULT 0,
    next        INTEGER NOT NULL DEFAULT 0,
    activity    INTEGER NOT NULL DEFAULT 0,
    outcome     INTEGER NOT NULL DEFAULT 0,
    assessed    INTEGER NOT NULL DEFAULT 0,

    intent      INTEGER NOT NULL DEFAULT 0,

    -- The rest of the header, and the timestamps. These moved here from the
    -- retired `jobs` table, which is what made dispatch_jobs authoritative
    -- rather than a partial second copy.
    --
    -- `pp` sits awkwardly beside the resolved Policy columns above, and the
    -- tension is real rather than an oversight. The policy note says PP is
    -- external vocabulary that does not exist past App, and the Verify/Repair/
    -- Unpack/Delete columns are the resolved form the pipeline reads. This
    -- column is the unresolved integer, kept because the API reports it back
    -- verbatim (internal/api/queue.go:479).
    --
    -- It is not merely carried: internal/app/directunpack_orchestrator.go:56
    -- gates DirectUnpack on `row.Header.PP < 2`, which is a second reading of
    -- the same permission the Unpack column already resolves. That is one
    -- consumer, named here so the next person to touch it knows it exists
    -- rather than discovering it.
    filename          TEXT NOT NULL DEFAULT '',
    warning           TEXT NOT NULL DEFAULT '',
    script            TEXT NOT NULL DEFAULT '',
    password          TEXT NOT NULL DEFAULT '',
    pp                INTEGER NOT NULL DEFAULT 0,
    nzb_backup        TEXT NOT NULL DEFAULT '',
    url               TEXT NOT NULL DEFAULT '',
    md5               TEXT NOT NULL DEFAULT '',
    added             INTEGER NOT NULL DEFAULT 0,
    download_started  INTEGER NOT NULL DEFAULT 0,
    download_finished INTEGER NOT NULL DEFAULT 0,

    -- par2_release_reason records why the on-demand par2 verdict released or
    -- withheld this job's recovery volumes.
    --
    -- It is persisted because losing it across a restart stopped being
    -- cosmetic once the post-processing file list began branching on it. A job
    -- that reached a verdict is left at StatusVerifying, which is resident and
    -- re-enqueues for post-processing after a restart; with the reason gone,
    -- buildDownloadFileList falls through to the "verified clean from index"
    -- line for a job where nothing was verified at all.
    --
    -- What it is NOT: a repair result, and not something any control flow
    -- branches on BY TEXT. Only its emptiness is load-bearing -- a non-empty
    -- value is the marker that a verdict was reached, which is what
    -- distinguishes "volumes held because nothing could be identified" from
    -- "volumes still awaiting a verdict".
    par2_release_reason TEXT NOT NULL DEFAULT '',

    -- recovery_bytes is the repair capacity the job holds, counted from
    -- subjects matching the ".volNNN+MM.par2" convention -- the files that
    -- carry recovery slices -- and not the set's index file, which holds
    -- per-file checksums and no repair capacity. Counting the index overstated
    -- the figure everywhere it is read, including two gates that abort a job
    -- as beyond repair.
    --
    -- The convention is not a guarantee. The PAR2 specification recommends the
    -- .vol naming without requiring it, and par2 itself reads packets rather
    -- than filenames, so a job can hold recovery data this column does not
    -- count. Zero here means "nothing recognized", never "nothing to repair
    -- with" -- see JobProgress.HasPar2Files and the abort gates.
    --
    -- It is the is_par2_recovery aggregate over job_files and so could be
    -- derived, but it is kept on this row deliberately. A job_files aggregate
    -- fails soft, leaving its scalars at zero, and a zero recovery figure is
    -- not a missing reading -- it is a definite claim that the job has no
    -- repair capacity, which the UI renders as "No repair data" and both abort
    -- gates read as grounds to declare a job hopeless. That must not be
    -- reachable from a transient database error, so it lives on this row,
    -- which is read unconditionally and fails the load rather than degrading.
    recovery_bytes    INTEGER NOT NULL DEFAULT 0,

    -- par2_recovered records that on-demand par2 un-deferred this job's
    -- recovery volumes because repair was needed. Without it, a job that was
    -- repaired and then restarted before finalize printed no par2 summary line
    -- at all -- neither "fetched N recovery volume(s) for repair" nor anything
    -- else, because every arm of that switch tests either this flag or a
    -- held-volume count that un-deferring has already driven to zero.
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
DROP TABLE queue_meta;
DROP TABLE history;
-- +goose StatementEnd
