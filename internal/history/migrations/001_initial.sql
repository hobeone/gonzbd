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
-- "Cannot be read by this version" is ENFORCED, and was not when this comment
-- was first written. goose keys on version numbers alone, so a database from
-- any earlier build records versions 2..11 that no longer exist while version
-- 1 reads as already applied: Up() applied nothing and returned nil, and the
-- daemon came up clean with no article_facts and no file_extents. Every
-- barrier then failed with a plain error that does not stall, nothing was ever
-- acked, and no job ever completed. history.Open now refuses a database whose
-- recorded version exceeds the highest migration the build ships
-- (refuseUnknownSchema). If a later migration is added, that bound moves with
-- it automatically — it is read from these filenames, not hard-coded.
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
-- This table carries almost no derived progress columns. max_written and
-- write_cursor used to live here, each a value summarising facts stored
-- elsewhere and each maintained independently of them. That is the direct
-- cause of #337 (one member of a set stored while its siblings are derived)
-- and #311 (a cursor serving as cache, authority, and scheduling hint at
-- once). Those facts now live in article_facts and their summaries in
-- file_extents, which is labelled cache.
--
-- failed_bytes and bytes_downloaded are the two exceptions, and both are
-- caches rather than second authorities. Each caches a sum over one half of
-- this same row's articles_done, which is the authoritative record of which
-- of the file's articles are resolved and which permanently failed. Both are
-- written by the single statement that writes articles_done, so the sum and
-- the bits it sums cannot be persisted out of step; and both are superseded
-- wholesale on promotion, where JobProgress.recompute ASSIGNS them from the
-- manifest and the restored bitmaps. They exist for the NON-resident path,
-- which has no manifest to recompute from and would otherwise report an
-- inflated remaining figure for every job beyond maxActive.
--
-- failed_bytes cannot live in file_extents with the other summaries: every
-- column there is derived from article_facts plus the file's bytes, and a
-- permanently failed article never decodes, so it never writes an
-- article_facts row at all. No recomputation from Class A can produce this
-- figure, which is why file_extents has no column for it.
--
-- bytes_downloaded could be derived from Class A, and was: it was read from
-- file_extents.bytes_durable until that turned out to be the wrong QUANTITY
-- rather than an unavailable one. The two count different things. This column
-- counts ENCODED bytes — the NZB `bytes` attribute summed over resolved
-- articles — because that is what it is compared against: the queue's
-- remaining figure is this row's `bytes` minus this column minus failed_bytes,
-- and `bytes` is the encoded per-file total from the same NZB.
-- file_extents.bytes_durable counts DECODED payload bytes, the lengths the
-- assembler actually wrote, which run a few percent lower. Seeding one from
-- the other made a non-resident job overstate its remaining bytes by that
-- margin, breaking the residency parity both columns exist to provide.
--
-- Restoring these is not a reversal of the removal above. bytes_downloaded was
-- removed because RestoreRetryProgress assigned it and recompute then
-- overwrote it — two writers maintaining one fact in parallel, which is the
-- S5 violation behind #306. That path is gone. A single writer caching a sum
-- of the same row's authoritative bits is a cache; two writers maintaining a
-- value in parallel is the defect. These are the first.
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
    failed_bytes     INTEGER NOT NULL DEFAULT 0,
    bytes_downloaded INTEGER NOT NULL DEFAULT 0,
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
    PRIMARY KEY (job_id, art_idx)
) WITHOUT ROWID;
-- Class A. Append-only and immutable: a row is never updated, because an
-- article's decoded offset, length, and CRC never change once discovered.
--
-- Lifecycle, shared with file_extents: these rows are keyed by job_id with no
-- foreign key to cascade from, so they are removed deliberately rather than
-- automatically. A job that leaves the queue drops them, EXCEPT a job that
-- FAILED — those are retained alongside its history_job_files row, because a
-- retry reuses the job ID over the same partial file and the retained facts
-- are what bound the completion truncate to the whole file rather than to the
-- handful of articles the retry re-fetched. They are dropped with the history
-- entry itself, in history.Delete.
-- Idempotency is the primary key's job — Append uses INSERT OR IGNORE.
-- These rows assert nothing about bytes being present on disk, which is
-- why they may be committed at any time with no fsync ordering.
--
-- crc32 is NOT NULL and always meaningful. There is no has_crc companion
-- column, and none is needed: every decode path that yields bytes also
-- yields a checksum over them. yEnc's decoder computes one over the decoded
-- output (the article's own trailer is only a transfer check, enforced
-- before the bytes get here), and the UU path computes one explicitly in
-- downloader.decodePayload for exactly this row. A crc32 of 0 therefore
-- means "these bytes hash to zero", never "unknown".
--
-- This is deliberately unlike file_extents.has_prefix_crc below, which stays.
-- A whole-file CRC genuinely can be unavailable — a verified prefix short of
-- the file's end has no whole-file value to report — so that one needs the
-- unavailable-versus-zero distinction R23 asks for. The per-article case does
-- not, because the per-article CRC is never absent.
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
    size           INTEGER NOT NULL DEFAULT 0,
    mod_time_ns    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (job_id, file_idx)
) WITHOUT ROWID;
-- Class B: a cache. Never authoritative — where this disagrees with a
-- recomputation from what it is derived from, the recomputation is correct by
-- definition.
--
-- Two writers, and only two. durability.Barrier writes during a download, only
-- after the fsync that makes its claims true. durability.Resumer writes at
-- startup, and only when a recomputation DISPROVED the stored row: reading the
-- bytes back after a restart and matching their recorded CRC observes the
-- property an fsync exists to produce, so the correction is written over the
-- record it disproved rather than left to resurrect. Nothing clears a bit
-- otherwise, and Barrier.priorExtent ORs this bitmap as its base.
--
-- size and mod_time_ns stamp the file at commit; a mismatch against the file
-- as it exists now invalidates every other column here.
--
-- durable_bitmap, verified_to, prefix_crc, has_prefix_crc, and bytes_durable
-- are derived from article_facts plus the file's actual bytes, and that pair
-- is what a recomputation reads.
--
-- has_prefix_crc means "prefix_crc is a verified WHOLE-FILE CRC", not merely
-- "prefix_crc is populated". It is set only when the verification run consumed
-- every recorded fact for the file AND its gapless prefix reached the file's
-- end; anything short of that is unavailable (R23), which is an honest answer
-- and must stay distinguishable from a CRC of zero. prefix_crc still covers
-- exactly [0, verified_to) whether or not the flag is set, so a reader must
-- not infer the range from the flag. The looser reading — "the CRC of
-- whatever prefix we have" — is what lets a partial extent's CRC be reported
-- as the file's, which is the defect this column exists to prevent.
--
-- There is deliberately no failed-byte column here. A permanently failed
-- article never decodes, so it never writes an article_facts row, and no
-- recomputation from Class A can produce that figure — which would make it the
-- one column the discard rule above could not apply to. It is cached in
-- job_files.failed_bytes instead, beside the articles_done bits it sums.
--
-- bytes_durable is DECODED payload bytes: the summed Length of the article
-- facts an fsync proved, which is the quantity a recomputation from Class A
-- reproduces. It is deliberately NOT the figure the queue subtracts to get a
-- job's remaining bytes — that arithmetic is against job_files.bytes, the
-- encoded NZB total, and so uses the encoded cache in
-- job_files.bytes_downloaded. The two are not interchangeable and differ by
-- the encoding overhead.
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

-- +goose StatementBegin
-- dispatch_jobs is internal/dispatch/store's table and nothing else's.
--
-- It is deliberately NOT columns on `jobs`: that table is internal/queue's
-- until B2.4 deletes the package, and its `status` column is a lossy
-- projection of the same position `state` records here. Two writers to one row
-- could let the two disagree, which is the second-writer smell Standing Design
-- Rule 2 names. The duplicated identity and header columns are the price, and
-- they go away with `jobs`.
--
-- The axes are stored as the integer values of internal/job's uint8 enums.
-- They carry no CHECK constraint, and that is not an oversight: a restored row
-- is replayed forward through job.Job's own doors by dispatch.reconstruct, so
-- an illegal position, an inadmissible outcome for its state, or a `next` that
-- is not a legal edge is refused by the state machine itself. A CHECK could
-- only re-validate the enum RANGE — it cannot know the transition table — and
-- would be a second enforcement point for one invariant. Contrast
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

    intent      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_dispatch_jobs_sort_key ON dispatch_jobs(sort_key);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE dispatch_jobs;
DROP TABLE history_job_files;
DROP TABLE file_extents;
DROP TABLE article_facts;
DROP TABLE job_files;
DROP TABLE jobs;
DROP TABLE queue_meta;
DROP TABLE history;
-- +goose StatementEnd
