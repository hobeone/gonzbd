-- The complete gonzbd schema, as one migration.
--
-- This file replaces the former 001-011 chain. Those migrations were collapsed
-- rather than extended because the change they carry — removing the derived
-- columns from job_files — cannot be expressed as an ALTER without leaving the
-- old columns' rationale standing in files that must not be edited. Collapsing
-- is only safe because no installation is being upgraded: the on-disk database
-- is not migrated and cannot be read by this version, and a from-scratch
-- reinstall was explicitly authorised. There is no backfill anywhere below and
-- none is possible.
--
-- Rationale that survived the collapse is restated inline against the table it
-- describes, because these comment blocks are now the only place those claims
-- live.

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
    -- is a compatibility surface — it appears in the mode=history API response,
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
CREATE TABLE jobs (
    id                TEXT PRIMARY KEY,
    filename          TEXT NOT NULL,
    name              TEXT NOT NULL,
    password          TEXT,
    url               TEXT,
    category          TEXT,
    priority          INTEGER NOT NULL,
    status            TEXT NOT NULL,
    pp                INTEGER NOT NULL,
    script            TEXT,
    time_added        INTEGER NOT NULL,
    md5               TEXT NOT NULL,
    avg_age           INTEGER NOT NULL,
    groups            TEXT,
    meta              TEXT,
    warning           TEXT,
    postproc          INTEGER NOT NULL DEFAULT 0,
    sort_key          INTEGER NOT NULL,
    download_started  INTEGER NOT NULL DEFAULT 0,
    download_finished INTEGER NOT NULL DEFAULT 0,
    nzb_backup        TEXT NOT NULL DEFAULT '',
    -- recovery_bytes/recovery_files count only subjects matching the
    -- ".volNNN+MM.par2" convention — the files that carry recovery slices —
    -- and not the set's index file, which holds per-file checksums and no
    -- repair capacity. Counting the index overstated the figure everywhere it
    -- is read, including two gates that abort a job as beyond repair.
    --
    -- The convention is not a guarantee. The PAR2 specification recommends the
    -- .vol naming without requiring it, and par2 itself reads packets rather
    -- than filenames, so a job can hold recovery data these columns do not
    -- count. Zero here means "nothing recognized", never "nothing to repair
    -- with" — see JobProgress.HasPar2Files and the abort gates.
    --
    -- These are exactly the is_par2_recovery aggregate over job_files and so
    -- could be derived, but they are kept on the jobs row deliberately.
    -- SQLiteStore.Get's job_files aggregate fails soft, logging and leaving
    -- its scalars at zero. A zero recovery figure is not a missing reading —
    -- it is a definite claim that the job has no repair capacity, which the UI
    -- renders as "No repair data" and both abort gates read as grounds to
    -- declare a job hopeless. That must not be reachable from a transient
    -- database error, so these two live on the jobs row, which is read
    -- unconditionally and fails the load rather than degrading.
    recovery_bytes    INTEGER NOT NULL DEFAULT 0,
    recovery_files    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_jobs_sort_key ON jobs(sort_key);
CREATE INDEX idx_jobs_name ON jobs(name);
CREATE INDEX idx_jobs_md5 ON jobs(md5);
-- +goose StatementEnd

-- +goose StatementBegin
-- The per-file rows of a queued job's manifest.
--
-- This table carries no derived progress columns. bytes_downloaded,
-- failed_bytes, max_written, and write_cursor used to live here, each a value
-- summarising facts stored elsewhere and each maintained independently of
-- them. That is the direct cause of #306 (a stored figure loaded and then
-- overwritten by a recompute), #337 (one member of a set stored while its
-- siblings are derived), and #311 (a cursor serving as cache, authority, and
-- scheduling hint at once). The facts now live in article_facts and the
-- summaries in file_extents, which is labelled cache.
CREATE TABLE job_files (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id           TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    file_index       INTEGER NOT NULL,
    subject          TEXT NOT NULL,
    date             INTEGER NOT NULL,
    bytes            INTEGER NOT NULL,
    is_par2_recovery INTEGER NOT NULL DEFAULT 0,
    complete         INTEGER NOT NULL DEFAULT 0,
    filename         TEXT,
    assembled_crc32  INTEGER DEFAULT 0,
    articles_done    TEXT,
    article_count    INTEGER NOT NULL DEFAULT 0,
    fetch_policy     INTEGER NOT NULL DEFAULT 0 CHECK (fetch_policy BETWEEN 0 AND 2),
    UNIQUE(job_id, file_index)
);

CREATE INDEX idx_job_files_job_id ON job_files(job_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE article_facts (
    job_id    TEXT    NOT NULL,
    art_idx   INTEGER NOT NULL,
    file_idx  INTEGER NOT NULL,
    offset    INTEGER NOT NULL,
    length    INTEGER NOT NULL,
    crc32     INTEGER NOT NULL,
    has_crc   INTEGER NOT NULL,
    PRIMARY KEY (job_id, art_idx)
) WITHOUT ROWID;
-- Class A. Append-only and immutable: a row is never updated, because an
-- article's decoded offset, length, and CRC never change once discovered.
-- Idempotency is the primary key's job — Append uses INSERT OR IGNORE.
-- These rows assert nothing about bytes being present on disk, which is
-- why they may be committed at any time with no fsync ordering.
CREATE INDEX idx_article_facts_file ON article_facts(job_id, file_idx, offset);
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
    bytes_failed   INTEGER NOT NULL DEFAULT 0,
    size           INTEGER NOT NULL DEFAULT 0,
    mod_time_ns    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (job_id, file_idx)
) WITHOUT ROWID;
-- Class B: a cache. Never authoritative — where this disagrees with a
-- recomputation from what it is derived from, the recomputation is correct by
-- definition. Written only by durability.Barrier, only after the fsync that
-- makes its claims true. size and mod_time_ns stamp the file at commit; a
-- mismatch against the file as it exists now invalidates every other column
-- here.
--
-- durable_bitmap, verified_to, prefix_crc, has_prefix_crc, and bytes_durable
-- are derived from article_facts plus the file's actual bytes, and that pair
-- is what a recomputation reads.
--
-- bytes_failed is the exception, and the discard rule above does NOT extend
-- to it. A permanently failed article never decodes, so it never writes an
-- article_facts row — by design: Class A is recorded at decode, and a lost
-- failure ack is safe precisely because a restart re-attempts the article. No
-- recomputation from article_facts plus the file's bytes can produce a
-- non-zero bytes_failed, so treating a recomputed zero as correct would
-- discard a real failure count and report a damaged job as intact. Its
-- authority is the job's per-article failure record — the failed half of
-- job_files.articles_done — and this column caches that instead.
-- +goose StatementEnd

-- +goose StatementBegin
-- Per-file download progress retained for a FAILED job so a retry can refetch
-- only the articles that did not make it.
--
-- Why a separate table rather than keeping the job_files rows: job_files is
-- REFERENCES jobs(id) ON DELETE CASCADE and MoveToHistory deletes the jobs
-- row, so those rows cannot outlive the queue-to-history transition where they
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
--
-- Like job_files, this table carries no derived byte or cursor columns.
-- write_cursor, bytes_downloaded, and max_written used to be copied here from
-- job_files when a failed job moved to history; with those columns gone from
-- job_files there is nothing to copy, and retaining them would mean storing a
-- constant zero under a name that claims to describe an earlier attempt.
--
-- Nothing a retry needs is lost. articles_done is the record of which articles
-- succeeded, and the byte and cursor figures were only ever summaries of it:
-- RestoreRetryProgress replays the bitmap through markDone/markFailed, which
-- reconstructs them. Assigning the stored copies first and then replaying, as
-- the removed code did, counted the same article's bytes twice.
CREATE TABLE history_job_files (
    job_id           TEXT NOT NULL,
    file_index       INTEGER NOT NULL,
    complete         INTEGER NOT NULL DEFAULT 0,
    filename         TEXT,
    assembled_crc32  INTEGER DEFAULT 0,
    articles_done    TEXT,
    article_count    INTEGER NOT NULL DEFAULT 0,
    fetch_policy     INTEGER NOT NULL DEFAULT 0 CHECK (fetch_policy BETWEEN 0 AND 2),
    PRIMARY KEY (job_id, file_index)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE history_job_files;
DROP TABLE file_extents;
DROP TABLE article_facts;
DROP TABLE job_files;
DROP TABLE jobs;
DROP TABLE queue_meta;
DROP TABLE history;
-- +goose StatementEnd
