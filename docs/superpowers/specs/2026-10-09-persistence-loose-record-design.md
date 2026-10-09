# Loose Article Record, Verified on Restart — Design

**Status:** Direction settled with the maintainer on 2026-10-09. This document
is the argument the implementation plan
(`docs/superpowers/plans/2026-10-09-persistence-loose-record-plan.md`) is
written against. Tree read at `6d41f03f`.

**Related issues:** #759 (overlapping byte ranges splice a no-par2 file) is
fixed in-process by an early stage and across restarts by the switch. #760 (an
fsync retry after a failure acknowledges unwritten bytes) is fixed by the
switch. #764 (`synchronous(FULL)`) is independent.

## 1. The problem

Resuming a download after a crash or an ordinary restart must not refetch what
is already on disk: files reach several GB, and both SABnzbd and NZBGet resume
inside a file. Whole-file trust — keep a file only once it is finished and
renamed into place — was considered and rejected for that reason.

Today that resume is bought with an article-level durability protocol:

- a **barrier** (`internal/durability/barrier.go`) that drains the assembler,
  fsyncs, stats, commits `durable_runs`, acknowledges Done bits and confirms,
  every 30 s / 64 MiB and at each file completion;
- the **checkpointer** (`internal/checkpoint`), the resumer's stat-size gate
  (`internal/durability/resume.go`), trim-to-runs, the stall finalize-recovery
  half of `internal/app/stall.go`, the #417 reload guard, and the
  `failed_articles`, `history_job_files` and `job_files.assembled_crc32`
  bookkeeping.

That layer is about 6,600 production lines (measured per file with `wc -l` at
`6d41f03f`; §7 has the table) and is still not sound against the failure it
exists for: on Linux a writeback error is reported to one `fsync` and the
failed pages are then marked clean, so a barrier that retries the fsync on the
same descriptor sees success over lost bytes (#760). The stat-size gate cannot
see a fallocated zero region. Stall recovery can end in "restart gonzbd to
resume".

## 2. The change

**Record what was written, loosely and cheaply; prove it by reading it back
once, at the first hydration after a restart.**

```sql
-- One row per article whose decoded bytes were handed to pwrite at
-- (offset, length) and for which pwrite returned nil. NOT a durability
-- claim: a row may describe bytes the kernel never flushed. A row becomes a
-- Done bit only by a CRC match during verification, or by its file being
-- complete=1.
CREATE TABLE written_articles (
    job_id   TEXT    NOT NULL,
    file_idx INTEGER NOT NULL,
    art_idx  INTEGER NOT NULL,   -- manifest-global index
    offset   INTEGER NOT NULL,   -- decoded byte offset
    length   INTEGER NOT NULL,
    crc32    INTEGER NOT NULL,   -- decoder-computed CRC of the decoded bytes
    PRIMARY KEY (job_id, file_idx, art_idx)
) WITHOUT ROWID;

CREATE TABLE job_files (
    job_id       TEXT    NOT NULL,
    file_index   INTEGER NOT NULL,
    complete     INTEGER NOT NULL DEFAULT 0,  -- see §3.4
    filename     TEXT    NOT NULL DEFAULT '',
    fetch_policy INTEGER NOT NULL DEFAULT 0 CHECK (fetch_policy BETWEEN 0 AND 2),
    PRIMARY KEY (job_id, file_index)
) WITHOUT ROWID;
```

Deleted: `durable_runs`, `failed_articles`, `history_job_files`,
`job_files.assembled_crc32`. `dispatch_jobs` and the par2 columns are
untouched. Both surviving tables are kept for a FAILED history entry under
`reclaim.go`'s `keptForFailedEntry` rule, and
`TestPerJobTables_CoversEveryJobKeyedTable` is updated to say so.

The decoder computes the article CRC itself (`internal/decoder/decoder.go`,
the `CRC` field over the decoded data) whether or not the poster supplied
`pcrc32`, so every written article has a CRC to verify against.

### Why per article

yEnc offsets are not in the NZB, and nothing but a per-article CRC localises a
mismatch below the file. `(offset, length, crc32)` per article is therefore the
smallest record that supports resume inside a file. The cost is roughly one row
per article (≈20k rows for a 20k-article job, **estimated** 1.5–2 MB); §9 makes
measuring it a merge condition.

## 3. Decisions

### 3.1 Non-negotiables this design is measured against

| | Statement |
|---|---|
| **NN1** | Never deliver a corrupt or incomplete file as complete — including for a post with no par2. |
| **NN2** | Queue rows, manifests and NZB backups keep their current formats and owners. |
| **NN3** | Hostile-input checks are kept: Message-ID validation, `offsetOutOfRange` with its `ExpectedSize/8` slack, path sanitisation. |
| **NN4** | Standing Design Rule 3: a bad article costs only its own bytes. |
| **NN5** | No `docs/sabnzbd_spec.md` §10 field changes. |

### 3.2 Writers — one owner for the record

1. **`Options.OnArticleWritten(jobID, fileIdx, artIdx, off, n, crc)`** — called
   by the assembler after `writeAt` returned nil, on the worker goroutine, with
   no I/O. The app handler, under one mutex, appends a delta row and then calls
   `Job.MarkArticleDone`. The delta is appended first so that nothing persisted
   depends on the Done bit. `ErrNotResident` from `MarkArticleDone` is expected
   at clean shutdown (manifests are evicted before the assembler's final drain)
   and is logged at Debug.
2. **The flusher** replaces `internal/checkpoint` and is the only writer of
   `written_articles` and of `job_files` updates. One goroutine; every 5 s and
   once at clean shutdown after `assembler.Stop`. It snapshots deltas and dirty
   files in **one** critical section under the handler's mutex — so `complete=1`
   can never land without the last article's row — and writes them in one
   transaction **outside** that mutex. On error it re-merges and logs, as the
   checkpointer does today.
3. **Two guards on every delta row:**
   - `INSERT OR REPLACE ... WHERE EXISTS (SELECT 1 FROM job_files WHERE job_id = ?)`,
     which stops a flush racing a removal. Its precondition is the admission
     seed: `seedJobFiles` → `durability.Store.Admit` inserts every `job_files`
     row in one transaction at submission, before `Dispatcher.Add`. The seed is
     kept, so the guard always finds a row while the job is registered.
   - **An instance check on the background flush:** a delta produced by a
     `*Job` that is no longer `dispatcher.Job(id)` is dropped before the
     transaction. The `EXISTS` guard alone does not cover a history retry, which
     re-seeds `job_files` under the same ID; the instance check does. The
     synchronous path (item 5) does not consult it: its caller names the
     instance explicitly and is authoritative, because a retry commits its
     verdict before `dispatcher.Add`, when `dispatcher.Job(id)` does not yet
     return the rebuilt job.
   `OR REPLACE`, not `OR IGNORE`: the most recent successful `pwrite` is
   authoritative, and a stale row from an aborted hydration must not shadow a
   refetch (the #421 mechanism).
4. **Synchronous flushes:** `retryHistoryJob` before `Add`; `enqueuePostProc`
   before the hand-over (unchanged); and, new, in `persistAndCommit` where
   `checkpointer.Prune` sits today, after `historyRepo.Add` and before
   `RemoveJob` and reclaim, with its own context budget (§3.8).
5. **Verifier results and file untrusting** reach SQLite through the flusher's
   synchronous path, not through a second writer. The verifier and the
   completion-fault path hand the flusher a per-file verdict (rows to delete,
   `complete` to clear) and wait for it to commit.

`MarkArticleFailed` stays in memory only. `durability.Store.Admit`'s insert is
the one other writer of `job_files`, and it creates rows rather than updating
them.

### 3.3 Verification — `verifyJobFiles`

**When.** Verification runs **before** the job's content is attached, and its
outcome decides whether attaching happens at all:

1. `appResidency.Hydrate` reads the manifest (as today), then — on the branch
   where the job has no progress yet — calls `verifyJobFiles` with the manifest,
   the `job_files` rows and the written rows, which
   reads rows and bytes and returns a verdict without touching the job.
2. The verdict's SQLite effects (rows to delete, `complete` to clear) are
   committed through the flusher's synchronous path.
3. Only then are `AttachContent(m)` and `InstallVerified(verdict)` called.

If verification or its commit fails or is cancelled, `Hydrate` returns the
error with nothing attached: the job is not resident, and the next hydration
starts again from step 1. So **a job with progress has always been verified**,
and `HasProgress()` selecting `RestoreContent` is sound — the trap of a job
that is attached but unverified, which `Hydrate`'s early return on `Resident()`
would otherwise hide, cannot arise. Every other path that attaches content
either has no rows (a new submission through `BuildIngestJob`) or calls
`verifyJobFiles` itself before attaching (a retry, below).

Callers:

- `appResidency.Hydrate` on its attach branch. That covers the first hydration
  of a job restored at startup, the startup loop for paused jobs (below), and
  `Dispatcher.SetName` → `loadProgressForRename`.
- `retryHistoryJob`, directly, after `assembler.ForgetJob` and before
  `dispatcher.Add`. A retried job is built by `BuildIngestJob`, which attaches
  its manifest, so `Hydrate` would return early and never verify it.

**What.** Every file with `complete=0` and at least one row is read, whatever
the job's state and the file's fetch policy. A crash between a file's
completion and the next flush can leave such a file in a job that has already
left `Fetching`, and an on-demand par2 volume can carry rows while its policy
change is still unflushed, so no state or policy makes a read unnecessary.

For each such file:

1. `path := jobFilePath(j.Name(), filename)`. Rows store a filename, never a
   path. An empty filename means no row can exist, so the file is skipped.
2. `os.Open(path)`. `ENOENT` is definitive: every row for the file is
   deleted and its articles are Outstanding.
3. `fsync(fd)`. On Linux ≥ 4.16 a newly opened descriptor is told about a
   writeback error it has not yet seen. An error makes the file **untrusted**:
   all its rows are deleted and its articles are Outstanding. Then
   `posix_fadvise(fd, 0, 0, POSIX_FADV_DONTNEED)`, so that the reads come from
   the device rather than from pages that were marked clean after a failed
   writeback.
4. Rows in offset order; `pread` each range through a reused 1 MiB buffer.
   - A full read whose CRC matches → Done, installed by
     `Job.InstallVerified(fileIdx, rows)` (which carries
     `artIdx, offset, length, crc32` for §3.5).
   - A full read whose CRC differs, or a short read at EOF → the row is
     deleted and the article is Outstanding.
   - If two rows' ranges intersect, the one whose readback matches is kept and
     the other article is marked **failed**, not Outstanding, so a restart
     cannot alternate between them.
5. If every article in the file's range is now resolved (the
   `strandedComplete` predicate, the single owner of "needs finishing"), finish
   it by path (§3.4) inside the pass and put `complete=1` in the verdict, so the
   flag is committed only after that fsync. After attaching, `Hydrate` marks
   the file complete in memory and enqueues a `FileComplete` with a new field
   `Resumed: true` on `app.internalFileComplete`, so the completion work runs
   on that consumer rather than on the dispatcher tick. `Resumed` tells
   `completeFinalizedFile` to skip the DirectUnpack feed (§3.4).

**Only a definitive comparison changes state.** Any other error — EIO, ESTALE,
a cancelled context — aborts verification for the job with nothing changed,
classified through `storagefault.Classify`, and nothing is attached. Results
are applied once, through the flusher, in one transaction at the end of the
pass.

For a file with `complete=1` (and not reset by a retry, §3.7): every row's article is Done, the failed set is
the complement of its rows, the whole-file CRC is derived by §3.5, and no bytes
are read. **The failed set is derived before anything calls
`hasFailedArticle`**, because the archive peek and the DirectUnpack feed both
consult it.

No size gate and no resumer: a sparse hole, a fallocated zero region or a
truncated tail fails its CRC and costs only its own articles.

**Paused jobs.** A `Fetching` job restored with `IntentPause` is not hydrated
until it is resumed, so `mode=queue` would report 0 % for it. Before the first
tick, every such job is hydrated (and therefore verified) and marked resident,
in the shape `loadProgressForRename` already uses.

**Rename.** `SetName` refuses a job once `DownloadBegun()` is true, and a rename
moves nothing on disk. Verification runs inside `loadProgressForRename`, before
that test, so verified rows make the job "begun" and the rename is refused. A
rename can therefore never leave rows that point at a stale path. The cost is
that a rename of a never-resumed job blocks that API request for the read.

**Placement cost accepted.** Verification runs inside the dispatcher tick, so a
multi-GB resume holds back that tick's launches. **Measured** locally at
0.7–2.0 s per 4 GiB (NVMe/btrfs, offset-sorted vs random order); a cold read is
the relevant figure (§9). This is a per-job startup cost, not a steady-state
one. If it matters, the follow-up is a `verified` gate in `buildDispatchPlan`
with the read on the Fetching worker.

### 3.4 Completion — the single ordering rule

> **`complete=1` means every article of the file is resolved (written or
> failed) and the file was fsynced on its writing handle. It is never written
> before that fsync, and never for a file whose fsync failed.**

A crash before the flush that carries `complete=1` costs one verification read
of that file at the next start (§3.3); nothing depends on the flag reaching
SQLite before the job's next state change, except the hand-over to
post-processing, which `enqueuePostProc` already flushes synchronously.

The trigger is unchanged: `processRequest` counts parts and finalizes at
`parts >= TotalParts`, where `TotalParts` comes from
`CountUnfinishedArticles` at registration and now excludes verified articles.

`finishFile`, in the assembler worker on the open handle, before
`OnFileComplete`:

1. `fsync`.
2. `maxEnd := max(offset+length)` over the articles this writer accepted plus
   the file's verified rows (passed in `FileInfo` at `registerFile`). If
   `maxEnd > 0` and the file is larger, `ftruncate(maxEnd)` and `fsync` again.
   It never grows a file, and a file with no accepted article is left alone.
   This is today's `boundOver` rule restated.
3. If any two written ranges intersect, charge both articles as failed bytes so
   the file cannot reach Assessing as intact.
4. Close, set the tombstone, call `OnFileComplete`.

**A truncated failed tail is still charged.** Failed bytes come from manifest
article sizes (`markFailed` → `FileProgress.FailedBytes` →
`ContentFailedBytes` → `RepairStateFrom`), and nothing on the verdict path
reads a file's size on disk. A no-par2 post with a failed tail reaches
`RepairNoCapacity`; a post with par2 is repaired.

**Any error** goes to `OnWriteFault` with the classified fault (Stall or Fail).
The handle is closed, the **tombstone is set anyway** so that a late duplicate
cannot reopen the file, `complete` stays 0, and **the file is untrusted**: its
Done bits are cleared and its rows deleted, through the flusher's synchronous
path (§3.2 item 5). The clear has to reach SQLite before the job can be
evicted, because the hydration-time `job_files` restore only ever sets
`complete` and would otherwise reinstate a stale `complete=1`. The file is
refetched whole. That is the coarsest unit, chosen because the error is
reported once and a second fsync on any descriptor returns 0.

**Finishing by path** — open, fsync, truncate if larger, fsync, close — is
called only from §3.3 step 5, over Done bits that the readback in that same pass
produced from the device. It is never called over Done bits that came from
`pwrite`.

After the fsync: `handleFileComplete` → `completeFinalizedFile` (derive the CRC,
archive peek, DirectUnpack feed unless `Resumed`, `MarkFileComplete`, dirty
mark, `reportDownloadComplete`). `finalizeCompletedFile` — the barrier half of
today's `handleFileComplete` — is deleted, since the worker has already
finished the file. `noteUndeliveredCompletion` is deleted: a refused
completion is re-derived at the next hydration by §3.3 step 5.

**DirectUnpack after a restart** is a best-effort accelerator. Its state is in
memory only, and a fresh unpacker starts only when it is fed volume 1. Files
that were `complete=1` before the restart, and files the verifier finishes, are
**not** fed to it; post-processing's normal unpack is the backstop. Feeding the
same volume twice is idempotent, so the live path needs no change.

### 3.5 The whole-file CRC is one function

`fileCRCFromRows(job, fileIdx)` is the only computation. `completeFinalizedFile`
and the `complete=1` branch of verification call it, and the four production
call sites of `FileAssembledCRC32` are re-pointed at it
(`git grep -n 'FileAssembledCRC32(' -- '*.go' ':!*_test.go'` returns seven
lines: those four — in `dispatcher_wiring.go`, `job_finalizer.go`,
`par2names.go` and `internal/postproc/stage_quickcheck.go` — the definition, and two
interface methods). It combines per-article CRCs
with `crc32util.Combine` if and only if: every article in the file's range has
a row **and none failed**; the first row is at offset 0; and **each row starts
exactly where the previous one ends**. Otherwise it returns `NoCRC` and par2
does a full verify. Such a chain cannot overlap and cannot leave a row over.
That is today's single-run predicate restated, pinned by a test that straddling
rows yield `NoCRC`.

The per-article CRCs for a file are kept resident in `JobProgress` as
`[]writtenArticle` until `complete=1` — installed by `InstallVerified` and
appended by the `OnArticleWritten` handler — so nothing on the worker path reads
SQLite. **Estimated** +16 B per unfinished article, released at completion.

**Why the quickcheck shortcut stays sound.** The repair stage skips par2 when
quickcheck matches the CRC combined from rows. That is sound because every row
of a `complete=1` file describes bytes written before §3.4's fsync; every row of
a `complete=0` file became Done only after §3.3's device read; and a file whose
fsync ever failed has no rows. A change that marks a file complete before the
fsync, applies rows without the read, or keeps rows past an fsync fault reopens
the hole NZBGet has. This paragraph belongs in `durability-contract.md` at NN1.

### 3.6 Overlapping articles

- A duplicate delivery of an accepted article is dropped by `seenDone`
  (unchanged).
- **First-writer-wins over byte intervals.** `acceptedAt` becomes a per-file
  sorted interval set, **seeded at `registerFile` from the file's verified
  rows**, so a loser after a restart is refused exactly as a loser in-process
  is. A range is **claimed only after its write returned nil**, so an article
  whose write faulted owns nothing and cannot cause a later article to be
  refused. Seeded ranges are owned by a sentinel whose `artIdx` is -1, because
  `articleID.sameArticle` compares the index alone and a zero sentinel would
  wave article 0 through. An arrival whose `[off, off+len)` intersects an owned range is refused
  and failed permanently. The refused article is a hole: a par2 post repairs
  it, and a no-par2 post ends at `RepairNoCapacity`, which is the honest NN1
  outcome (#759).
- **The one known NN4 exception:** when a bogus article arrives first, its good
  neighbours are refused. This is unchanged from today and recorded, not fixed.
- With no write cache an incumbent's write completes before a rival is
  considered, so `failDisplaced` and `discardAt` become unreachable and are
  deleted.
- `offsetOutOfRange` with its slack is unchanged (NN3).

### 3.7 Lifecycle hooks

| Hook | Behaviour |
|---|---|
| Write fault (`pwrite` error) | Unchanged: the part rolls back, `OnArticlesUnwritten` clears Emitted, `OnWriteFault` → Stall/Fail. No row is written. |
| Completion fault | §3.4: tombstone; file untrusted through the flusher; Stall. `reevaluateStall` keeps only its parking half: on resume the file's articles are Outstanding and are refetched. The "restart gonzbd to resume" dead end disappears. |
| Pause, low-disk, server penalty | Nothing writes the record; no hook. |
| Reload (`ReloadDownloader`) | Stop the old downloader; `setCompletions(nil)`; a new `assembler.Quiesce(ctx)` control message on the request queue, answered when reached, so everything queued ahead has been written and its callbacks have run; `ClearEmittedForReload(false)` for every non-admitted job; start the new downloader. The #417 guard and its byte accounting go: an Emitted bit now covers only an article whose bytes have not reached `pwrite`. |
| Remove (`RemoveJob`) | Unchanged; reclaim deletes rows under the `keptForFailedEntry` rule. A racing flush is stopped by the instance check and the `EXISTS` guard; any orphan rows are removed by `sweepOrphans` at startup. |
| Retry (`retryHistoryJob`) | Rebuild from the NZB; restore `_FAILED_`; reclaim; shape-check rows against the re-parsed manifest (every `(file_idx, art_idx)` inside the file's range, and per-file article counts derived from the manifest), deleting the job's rows on a mismatch; set `complete=0` on **every** `job_files` row, because post-processing may have repaired, moved or deleted the bytes since they were written, so a retry reads the whole job once; `verifyJobFiles`; `ResetForRetry`; `ForgetJob`. A file quickcheck moved into a par2 subdirectory is at a path its `filename` does not name, so it takes the `ENOENT` arm and is refetched whole — accepted, as today. An article failed by an intersection is cleared by `ResetForRetry` and fetched again; it re-collides with the seeded winner and is refused, so it cannot displace it. |
| Clean shutdown | `shutdownCheckpoint` and its 10 s budget go; `drainAndCloseAll` keeps its per-file fsync; the flusher runs once after `assembler.Stop`. No clean-shutdown flag. |
| Hand-over to post-processing | `CloseJobHandles` (drain, fsync, close) and `enqueuePostProc`'s synchronous flush are unchanged. |

### 3.8 The flush in `persistAndCommit`

No `sync.Mutex` is held there. What is held is the per-job transition claim
and the dispatcher occupancy lease, and `historyRepo.Add` already runs a SQLite
transaction at the same point. The flush:

- runs **after** `Add`, so a failed `Add` does not lose progress, and **before**
  `RemoveJob` and reclaim;
- takes its own context budget, and the function's documented sub-budgets
  (`Add`, the flush, the two remove steps, reclaim) are re-summed against the
  occupancy and shutdown step timeouts in the same change.

`check_lock_io` cannot prove this, because it tracks only `Lock()` spans and one
level of `*Locked` callees; the argument above is the evidence.
`Dispatcher.RemoveJob` already writes SQLite under `d.storeMu`, so the
dispatcher side is not free of I/O under a lock either.

### 3.9 History

`history_job_files` has one reader, `retryHistoryJob` (through
`historyFileProgress` → `RetainedFiles`), and no API, UI or stats reader.
Deleting it changes no §10 field and no history slot field. `historyRepo.Add`
loses its per-file progress argument, and `retainedProgressFor` — the finalizer's
read of the manifest from disk — is deleted.

A FAILED history entry exposes no failed-article count, today or afterwards.
After the change, what a retry can recover is the `job_files` rows and the
written rows; failed articles are not persisted and come back Outstanding.

### 3.10 API, config, UI

- `bytes_pending` and `last_barrier_unix` are deleted. They are gonzbd-only
  slot fields (not in §10) that read barrier state, and the bundled UI's
  "written but not yet fsynced" block in `QueueRow.svelte` goes with them.
- `bytes_durable` keeps its key, but it now means "written", not "fsynced". Its
  doc comments are rewritten; nothing claims the meaning is unchanged.
- `checkpoint_interval` and `checkpoint_bytes` are deleted.
- `test/crash/harness.go`'s oracle is rewritten, not deleted.
  `TestSIGKILL_NoArticleIsResolvedWithoutItsBytes` re-pins as "after a kill,
  every article the restarted daemon reports Done hashes to its recorded CRC";
  `TestSIGKILL_ReworkStaysWithinTheCheckpointBound` re-pins as "no verified
  article is refetched".

## 4. Invariants

> **Why there is no "outside `Fetching`" shortcut.** An earlier draft applied
> rows without reading them once a job had left `Fetching`, on the argument
> that `complete=1` always reached SQLite first. Nothing flushes synchronously
> on the `Fetching → Assessing` transition or on early abort (the
> `CheckEarlyAbort` branch in `internal/app/pipeline.go`), so that argument
> does not hold. Every `complete=0` file with rows is read instead (§3.3),
> which needs no new hook.

1. A Done bit after a restart exists only because a device read matched its
   CRC, or because its file was `complete=1`.
2. `complete=1` is written only after the writing handle's fsync and truncate,
   and never over a file whose fsync failed.
3. No row becomes a Done bit after a restart without a device read, whatever
   the job's state, unless its file is `complete=1` and was not reset by a
   retry.
4. A file whose fsync failed has no rows and no Done bits, in memory or in
   SQLite.
5. No two written articles of a file have intersecting byte ranges, and an
   article owns a range only once its write succeeded.
6. The whole-file CRC exists only for a gapless, non-overlapping chain of rows
   covering every article of the file with none failed.
7. One writer updates the record (the flusher); `Admit` only inserts the seed.

## 5. Alternatives considered

| Option | Why not |
|---|---|
| Keep the current barrier | ~6,600 lines plus about 48 mutate specs (§7), and still unsound against an fsync failure (#760). |
| Whole-file trust | Rejected by the maintainer: every in-flight multi-GB file is refetched after any restart. |
| Blind trust in the record (NZBGet) | Fails NN1: a record ahead of the disk feeds the quickcheck shortcut, and a no-par2 post is never read at all. |
| Verify every file at completion | Adds a full read of every file to the no-crash path. |
| A clean-shutdown flag that skips the read | Saves one read, at the cost of a second durable ordering rule. |
| Drop `complete`; verify every file with rows | A 90 %-done 50 GB job reads ~45 GB on every restart, inside the tick. |

## 6. Prior art

| | Persisted | Verified against the file on load? | Known hole |
|---|---|---|---|
| SABnzbd | Pickled job with per-article `on_disk` and CRC, rate-limited, no fsync | No | A record ahead of the disk is trusted |
| NZBGet | Per-article status, saved about once a second, no fsync | No; keeps an output file on a size match | A preallocated file with lost pages passes |
| This design | Per-article offset, length, CRC; `complete` after fsync | Yes, every incomplete file with rows | Page-cache drop semantics on NFS after a server-side write error are unverified (§9) |

## 7. What this deletes

**Measured** = `wc -l` per file at `6d41f03f`; **approx.** = attributed by file
name, not re-measured.

| Mechanism | Prod LOC | Disposition |
|---|---|---|
| Barrier, proof, sync target (`internal/durability/{barrier,proof,synctarget,doc}.go`) | 901 (measured) | Deleted |
| `durable_runs` store and run types | 599 (measured) | Deleted |
| Resumer and trim | 264 (measured) | Deleted; finishing by path replaces trim |
| `failed_articles`, `history_job_files`, `assembled_crc32` and their readers | 236 (measured) | Deleted |
| Checkpointer | 337 (measured) | Replaced by the flusher (~80 lines) |
| Assembler barrier op boundary (`internal/assembler/synctarget.go`) | 534 (measured) | Deleted; `Quiesce` ~20 lines |
| FileWriter barrier half and write cache | 1,342 (measured; ~400 survive) | Interval set +~30 |
| App barrier orchestration (`internal/app/durability.go`) | 1,166 of 1,736 (measured) | Stall/Fail, reclaim, sweep stay |
| Stall finalize-recovery half | 356 of 633 (measured) | Parking half stays |
| Startup resume sweep | 519 (measured) | Paused-job loop (~10 lines) |
| Job doors consuming runs and proofs | 161 (measured) | `InstallVerified` (~40) |
| #417 reload guard | 101 (measured) | ~15 lines survive |
| **Total** | **~6,600 gross; est. ~5,400 net** | |

About 48 mutate specs anchor in files this rewrites or deletes (a symbol grep
over the spec text; the exact set is whatever `mutate --check-all` reports).

## 8. Kept

The Emitted bit and its clears; first-writer-wins (now over intervals);
`offsetOutOfRange`; fallocate pre-allocation with no `ftruncate` fallback on
`EOPNOTSUPP`; storage-fault classification and the Stall/Fail split;
`keptForFailedEntry` and `sweepOrphans`; `job_files.filename`/`fetch_policy`
and the admission seed; `completeFinalizedFile`'s order; `strandedComplete`;
`CloseJobHandles` and `drainAndClose`; residency and `pipeline.isCurrent`; the
decoder CRC; the crash harness, with its oracle rewritten.

## 9. Open questions and accepted costs

- **Cold-read cost.** The local figure was taken after `FADV_DONTNEED` on
  btrfs, which does not guarantee eviction. The NFS figure (~4.6 s per 4 GiB)
  is projected from a measured 800 MiB/s sequential read. The maintainer has
  ruled that NFS reads are fine; a cold-cache measurement is still taken before
  the switch merges, and recorded in its PR.
- **WAL and delta volume.** Measure WAL growth and flusher transaction time
  with several concurrent jobs before the switch merges. The delta buffer
  re-merges on a failed flush and so grows while SQLite is failing; bound it
  (stall the job past a cap) if the measurement shows it can grow large.
- **NFS cache drop.** The behaviour of `FADV_DONTNEED` after a server-side
  write error on NFS 4.1 is unverified. The fsync in §3.3 step 3 still catches
  a reported error.
- **Failed articles of incomplete files are re-asked after each restart.**
  The cost is bounded per restart. If it bites, add a failed-article column to
  the `job_files` flush.
- **A file relocated by quickcheck is refetched on retry.** Updating
  `job_files.filename` at relocation is about 10 lines if resume-on-retry is
  later wanted there.
