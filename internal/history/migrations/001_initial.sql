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
-- (jobs, article_facts, file_extents), and one entire migration (004) added a
-- column to a table two migrations later removed. Counting what 001 declared
-- and the chain then destroyed: jobs' 23 columns plus the one 004 added to it,
-- article_facts' 6, file_extents' 9, and articles_done on two tables -- 41
-- columns, and 4 indexes (idx_jobs_sort_key, idx_jobs_name, idx_jobs_md5,
-- idx_article_facts_file).
--
-- Every one of those shipped and worked; the point is not that they were never
-- real but that 001 was the wrong place to go on reading about them. Someone
-- opening the schema to learn the schema was told things the next file down
-- had already undone.
--
-- The rationale drifted the same way. Every column added by ALTER since the
-- last collapse carried its justification in a separate file, so the table
-- definition showed a bare column and the reason for it lived somewhere the
-- reader had no cause to open. Collapsing puts each claim beside the thing it
-- explains, which is the only arrangement that keeps them checkable together.
--
-- WHAT THE COLLAPSE ALSO REMOVED
--
-- Four things had no consumer and are gone rather than carried forward:
--
--   * queue_meta, a whole table. No .go file read it, wrote it, or named it.
--     It survived the previous collapse as inertia.
--   * history.report, history.series, history.duplicate_key. Each was declared
--     on history.Entry, written by the INSERT, and scanned back -- and read by
--     nothing. They round-tripped the zero value. Entry carries no JSON tags,
--     so they were not an incidental API surface either; the mode=history
--     response is built field by field and never mentioned them.
--
-- The first audit stopped there and should not have. Its method -- trace each
-- column to a Go struct field, look for a reader of that field -- cannot clear
-- a table whose columns duplicate the manifest, because a field populated from
-- the manifest reads as live whether or not any query selects the column.
--
-- Asking the SQL instead is the stronger question, and it found six more:
--   git grep -n 'FROM job_files' -- '*.go' ':!*_test.go'
-- returns one production read, internal/app/residency.go:108, selecting five
-- columns. subject, date, bytes, is_par2_recovery, failed_bytes and
-- bytes_downloaded were written on every checkpoint and read by nothing. They
-- are gone too; see job_files' own comment block below for why the argument
-- that had been keeping the last two was wrong rather than merely stale.
--
-- The lesson is the method, not the count. "Which Go field reads this?" and
-- "which query selects this?" are different questions, and for a table that
-- mirrors another artifact only the second one is decisive.
--
-- The stated reason for keeping them was schema parity with the upstream
-- Python implementation. Only `series` actually carried that annotation --
-- docs/sabnzbd_spec.md marked it "(deprecated; keep for migration)", while
-- `report` was described as an internal status string and `duplicate_key` was
-- unannotated -- but parity is the only reading under which any of the three
-- earns its place, since nothing here ever wrote them.
--
-- Standing Design Rule 1 abolishes that migration, and history.Open fails on
-- an upstream file rather than adopting it. Be exact about WHICH mechanism
-- does that, because the obvious answer is wrong: refuseUnknownSchema compares
-- goose version numbers, and an upstream file records none at all, so it
-- passes that guard and is rejected a moment later by this migration's own
-- CREATE TABLE history failing against a file that already has one. Either way
-- there is no upgrade path, and a column kept for one is not parity.
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
-- them. It is the broadest check rather than the only one: the nine subtests
-- of TestMigrations_SchemaShape pin the properties this design turns on
-- individually, and internal/history/schema_test.go pins the history column
-- count. A constraint deleted from this file leaves no trace anywhere else,
-- which is why the golden is worth regenerating deliberately and reading the
-- resulting diff rather than accepting it.

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
-- The per-file RESULTS of downloading a job: what each file turned out to be
-- called, whether it finished, and what it hashed to.
--
-- Read that description carefully, because it is narrower than the table used
-- to be and the narrowing is the point. This is not a copy of the manifest.
-- Anything the manifest already says -- a file's subject, its date, its
-- NZB-declared size, whether it is a par2 volume -- does not belong here,
-- because the manifest is on disk beside the database and is loaded before any
-- of these rows are read.
--
-- WHAT IS ACTUALLY READ, which is the only test that matters:
--   git grep -n 'FROM job_files' -- '*.go' ':!*_test.go'
-- returns one production query, internal/app/residency.go:108, selecting
-- file_index, filename, complete, assembled_crc32 and fetch_policy. Every one
-- of those is a RESULT: discovered from the yEnc header, decided by the
-- assembler, computed over the assembled bytes, or chosen by the on-demand
-- par2 policy. None is recoverable from the manifest, which is why the table
-- exists at all.
--
-- WHAT WAS REMOVED, AND WHY THE STATED REASON FOR IT WAS WRONG.
--
-- subject, date, bytes, is_par2_recovery, failed_bytes and bytes_downloaded
-- were all written on every checkpoint and read by nothing. They are gone.
--
-- The first four were manifest data, duplicated. The last two were defended at
-- length, and the defence does not survive checking. It ran: failed_bytes is
-- "the one per-file byte figure the durability record cannot supply", because
-- failed_articles records WHICH articles failed and never how many bytes they
-- were, and a permanently failed article never decodes so no durable run
-- covers it either.
--
-- Both halves of that are true and the conclusion still does not follow. The
-- byte count of a failed article is m.ArticleBytes(i) -- the manifest knows
-- every article's size whether or not it was ever fetched -- and
-- failed_articles supplies exactly the set of i. JobProgress.markFailed does
-- precisely that sum at internal/job/progress.go:1047. Manifest crossed with
-- failed_articles is sufficient, and the same holds for bytes_downloaded via
-- markDone.
--
-- The deeper reason they could never have been read is structural. Both were
-- said to "exist for the NON-resident path", which has no manifest to
-- recompute from. There is no such path. Progress is only ever constructed
-- with a manifest in hand: appResidency.Hydrate attaches the manifest FIRST
-- and only then calls restoreJobFiles and restoreResolution, and
-- ApplyResolution finishes by calling JobProgress.recompute(m), which rederives
-- every per-file byte figure from the manifest and the resolution bitsets. At
-- the one moment these columns could have been consulted, the authority they
-- were caching is already loaded.
--
-- A job with no manifest has no JobProgress either, and reports its full size
-- as remaining (internal/dispatch/registry.go:426). That is a real gap in what
-- a restored, unhydrated job displays -- but these columns could not close it,
-- because closing it means building a progress-without-manifest constructor
-- that has never existed. Deleting them removes a cache; it does not remove a
-- capability.
--
-- article_count went the same way, and the reasoning is worth keeping because
-- it was nearly kept for the wrong reason. It had no production reader either;
-- its one consumer was test/crash/harness.go, which needs each file's article
-- count to turn the global indices in durable_runs and failed_articles into
-- file-local ordinals.
--
-- The first argument for keeping it was that the harness reads stable storage
-- with the daemon dead and must not borrow the daemon's own decoder, so it
-- could not be asked to read the manifest instead. That misapplies the
-- harness's own rule, which is about not sharing DERIVATION LOGIC -- it
-- re-implements the done/failed derivation rather than calling into it -- and
-- says nothing about where structural input comes from.
--
-- The right answer was neither this column nor the manifest. The harness
-- BUILDS the NZB it serves, so len(h.MsgIDs[fileIdx]) is the article count,
-- known before the daemon starts. Reading the count back from here meant
-- deriving file boundaries from the daemon's copy of a structure the test
-- already knew, so a daemon that recorded the wrong count would have been
-- checked against its own mistake. The harness now uses its fixture, which is
-- a stronger test and leaves this column with no reader at all.
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
-- They are dropped with the history entry itself, in
-- history.Repository.Delete, and also by RetryHistoryJob when it declines to
-- apply the retained progress (internal/app/app.go:2144 calling
-- dropJobDurability). internal/app/durability.go:1387 enumerates the deleters
-- in full; two named here are not the whole set.
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
-- ':!*_test.go'` returns one line: internal/app/dispatcher_wiring.go:112, the
-- INSERT OR IGNORE prepared inside appCheckpointStore.SaveBatch -- the sole
-- production implementation of the checkpoint.Store interface
-- (declared at internal/checkpoint/checkpointer.go:24, its SaveBatch method on
-- the next line). Nothing in this package writes a row.
--
-- Deletion is deliberately not so narrow, and calling the table "solely owned"
-- would misdescribe it. Rows are removed on the retry path
-- (internal/app/app.go:2150), by the durability sweep for a job that has left
-- both the queue and history-as-FAILED (internal/app/durability.go:1410), and
-- by history.Repository.Delete dropping a departed job's durability with it
-- (internal/history/repository.go:400). Every one of those is a job-scoped
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
-- re-parsed NZB does not line up with it: retainedMatchesManifest
-- (internal/app/app.go:2244) compares these rows' count against the re-parsed
-- manifest's file range, file by file. It does not consult durable_runs or
-- failed_articles.
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
    -- assigns once, at registration. It survives a removal without renumbering
    -- because the only two operations that change the dispatcher's order are
    -- an append and an order-preserving delete.
    --
    -- Nothing revises it TODAY, and that is a statement about the operations
    -- that exist rather than a property of the design -- see entry.seq in
    -- internal/dispatch/registry.go, which says so at length and records that
    -- it will not stay true: spec §4.7 and /api?mode=switch require arbitrary
    -- reordering, and a reorder has to renumber, because restore rebuilds
    -- order from this column alone.
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
    -- It is not merely carried, and not by one caller. The persisted integer
    -- is handed into postproc.Job (internal/app/app.go:1974) and gates stages
    -- there: postproc.go:459 skips a stage via shouldSkipForPP, and
    -- stage_script.go:149 passes it to user scripts as PPFlags.
    -- internal/app/directunpack_orchestrator.go:56 gates DirectUnpack on
    -- `row.Header.PP < 2` separately.
    --
    -- Each of those is a second reading of a permission the resolved Verify /
    -- Repair / Unpack columns above already express, which is the owner-model
    -- tension named rather than resolved here: collapsing them is a behaviour
    -- change and belongs to whoever takes that on, not to a schema rewrite.
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
    -- It is derived, not measured here: Job.recoveryBytes is assigned from
    -- Manifest.RecoveryBytes() (internal/job/content.go:49 and :88), which
    -- returns the manifest's own par2RecoveryBytes. There is no SQL aggregate
    -- over job_files.is_par2_recovery anywhere -- git grep -n 'is_par2_recovery'
    -- over non-test .go files returns an INSERT column list, a JSON tag and a
    -- comment.
    --
    -- It is persisted on this row rather than recomputed on load because a
    -- non-resident job has no manifest to recompute from, and because a zero
    -- recovery figure is not a missing reading -- it is a definite claim that
    -- the job has no repair capacity, which the UI renders as "No repair data"
    -- and both abort gates read as grounds to declare a job hopeless. That
    -- must not be reachable from a degraded read, so it lives on a row that is
    -- read unconditionally and fails the load rather than returning zero.
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
DROP TABLE history;
-- +goose StatementEnd
