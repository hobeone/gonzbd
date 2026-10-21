# Download Durability & Storage Contract

This document is the contract for `internal/durability`, `internal/storagefault`,
`internal/assembler` and `internal/directunpack`, and for the code in
`internal/app` that records written articles (`record.go`) and verifies them
on restart (`verify.go`, `residency.go`): what it means for a downloaded article
to be *done*, when that claim may be made, what survives a crash, how a restart
re-derives its work set, and how a storage fault reaches the user.

`docs/ARCHITECTURE.md` places these packages in the download pipeline.
`docs/job-lifecycle.md` owns residency and the file table, manifest and progress tiers, which
this contract depends on and does not restate.

**This states the contract in the present tense.** Where the code and this
document disagree, the code is wrong and the gap is a bug, not a documentation
error. Known gaps are named in *Accepted limitations* at the end, rather than
left for a reader to discover.

## Why this exists

The costs are asymmetric, and that asymmetry is the whole design:

- **Over-fetching is a cost.** An article re-downloaded needlessly wastes
  bandwidth and time. It is bounded, visible and recoverable.
- **Over-claiming is a defect.** `ForEachUnfinishedArticle` skips any article
  whose `done` bit is set, so a `Done` that survives a restart for bytes that
  are not on disk is never re-dispatched, and the file completes with a hole in
  it. Without par2 it is silent.

So every rule below resolves ambiguity toward re-fetching.

Resume must still work inside a file: files reach several GB, and refetching a
partial file whole on every restart is not acceptable. The record that makes
that possible is **loose and cheap to write, and proven by reading it back
at the first successful hydration after a restart, and again on a retry.** It replaced an article-level
durability barrier that fsynced and committed on a cadence. That design could
not be made sound against the failure it existed for: on Linux a writeback
error is reported to one `fsync` and the failed pages are then marked clean,
so an fsync retried on the same descriptor returned success over lost bytes
(#760), and its stat-size gate at restart could not see a fallocated zero
region.

## Non-negotiables and invariant labels

| | Statement |
|---|---|
| **NN1** | Never deliver a corrupt or incomplete file as complete — including for a post with no par2. |
| **NN2** | Queue rows and NZB backups keep their formats and owners; a manifest holds a job's article list alone, and its owner is unchanged (the per-file structure is `job_files`, §6). |
| **NN3** | Hostile-input checks are kept: Message-ID validation, `offsetOutOfRange` with its `ExpectedSize/8` slack, path sanitisation. |
| **NN4** | Standing Design Rule 3: a bad article costs only its own bytes. |
| **NN5** | No `docs/sabnzbd_spec.md` §10 field changes. |

**Why the quickcheck shortcut is sound (NN1).** The repair stage skips par2
when quickcheck matches the CRC combined from a file's rows (*The whole-file
CRC*). That is sound because every row of a `complete=1` file describes bytes
written before the writing handle's fsync (§2); every row of a `complete=0`
file became `Done` after a restart only by a device read that matched its CRC
(§3); and a file whose fsync failed has no rows once its untrust's delete
lands, which no `complete=1` for the file can precede (§4). A change that
marks a file complete before its fsync, installs rows without the read, or
lets `complete=1` land over rows an fsync fault left behind reopens the hole:
par2 would be skipped over a file whose bytes are not what the CRC describes.

Sections below also cite short labels (`S3`, `A1`, `R19`, …) for rules that
predate this record. This table is their definition.

| Label | Rule |
|---|---|
| S3 | Absence of evidence is absence: an article whose state cannot be established from what is on disk is Outstanding. |
| S5 | Exactly one authoritative representation per fact; a fact stored in two places is a design defect. |
| S6 | A completion trim may shrink a file, never grow it. |
| A1 | A storage fault is never recorded as an article fault, nor the reverse. |
| A2 | Every failure has a subject and a disposition; no path may log-and-continue. **One named exception** — see *Hand-over to post-processing*. |
| B1 | Bounded rework after a crash: what the last record flush did not reach, plus whatever fails verification. |
| B2 | Bounded memory: held for in-flight article data, independent of job size and job count. |
| X1 | Single writer per file: exactly one component owns a file's handle and its derived state. |
| R12 | Duplicate delivery of an article is idempotent. |
| R18 | Write/sync failures are classified by subject and retryability. |
| R19 | Retryable-storage → the job stalls, the reason is surfaced, articles stay Outstanding, re-evaluated on an interval and on user action. |
| R20 | Permanent-storage → the job fails with that reason; no article is marked failed. |
| R21 | No storage fault may alter the health percentage or the failed-byte count. |
| R27 | A stalled job surfaces a reason the user can act on. |
| R28 | An invariant violation fails loudly; it must never degrade silently. |

`git grep -oE '\b(S[1-7]|A[12]|B[1-4]|X[1-3]|R[0-9]+)\b' docs/durability-contract.md | sort -u`
is the enumeration behind this table's row set: every label this document
cites, and no other.

## The record

Two tables in `history.db`, both owned by `durability.Store`
(`internal/history/migrations/001_initial.sql` has their columns):

- **`written_articles`** — one row per article whose decoded bytes were handed
  to `pwrite` at `(offset, length)` and for which `pwrite` returned nil, with
  the decoder's CRC of those bytes. **It is not a durability claim**: a row may
  describe bytes the kernel never flushed. The decoder computes the CRC itself
  whether or not the poster supplied one, so every row has a CRC to verify.
- **`job_files`** — one row per file: its structure (`subject`, `bytes`,
  `article_count`), written once at admission (§6), and the `complete`,
  `filename` and `fetch_policy` the recorder updates. The structure is what the
  restore pass builds the job's file table from (§3); hydration builds the
  manifest over that table.

`complete=1` means every article of the file is resolved (written or failed)
and the file was fsynced on a handle that wrote or verified it (§2).

Nothing else about download progress is persisted. A permanently failed
article is recorded in memory only (`Job.MarkArticleFailed`); after a restart
it is Outstanding again, except in a `complete=1` file, whose failed set is
derived on install as the complement of its rows (§3). The whole-file CRC is
not stored either; it is derived from the rows (*The whole-file CRC*), so
there is no second copy to drift (S5).

Both tables are kept for a FAILED history entry, so a retry can find its rows
and the filenames that locate their bytes. Everything else about their
lifetime is the reclaim rule's (§6).

### The state of an article

```
             dispatched              pwrite returned nil
  Outstanding ─────────► Emitted ─────────────────────────► Done (in memory)
       ▲                    │                                 │ row buffered by the
       │                    │ write fault: Emitted cleared    │ recorder, flushed to
       │◄───────────────────┘ (OnArticlesUnwritten)            │ written_articles
       │                                                      │
       │◄──── a failed close-time fsync, or a failed ─────────┤
       │      completion finish: the file is untrusted (§4)   │
       │                                                      ▼
       │                                           ON RESTART: Done again only if
       └────────────────────────────────────────── its bytes read back with its CRC,
                    otherwise Outstanding           or its file is complete=1 (§3)
```

In this process an article is `Done` once its write returned: that is what
lets the job make progress and its file reach `TotalParts`. Nothing outside
the process sees that `Done` before the file is complete — `complete=1` waits
for the fsync (§2), and a restart does not trust a row it has not read (§3).

`Emitted` is the transient dispatch state and is not persisted at all; see
`docs/nntp-downloader-contract.md` §5.

## The tiers

| Tier | Component | Responsibility | Synchronization |
|---|---|---|---|
| **Ingest** | `Assembler.WriteArticle` and the control-message senders | Enqueue `WriteRequest` items into a bounded channel (`reqs`, cap 2048). | Channel send with `select` on `stopCh` and `ctx.Done()`. `wg.Add(1)` tracks every in-flight sender so `Stop()` drains cleanly. |
| **Worker** | `Assembler.worker` goroutine | Owns the open-file map and every `fileWriter`. Routes requests, counts parts, checks disk space, finishes completed files. | Single goroutine (X1). No locks over file handles. |
| **Writer** | `assembler.fileWriter` (one per open file) | Owns one file's handle, its writes, its owned byte ranges, its completion finish and its close-time fsync. | Worker-owned; never touched from another goroutine. |
| **Recorder** | `app.recorder` (`internal/app/record.go`) | Buffers one row per written article and one state per dirty file, and writes them through `Store.ApplyRecord`. | `mu` over the buffers; `wmu` serialises its writes (§1). |
| **Verifier** | `verifyJobFiles` (`internal/app/verify.go`), called from `appResidency.Hydrate` and `Application.verifyRetry` | Reads every recorded article of an incomplete file back from the device before the job's content is attached. | Runs on the hydrating goroutine; touches no job and no row itself. |
| **Fault routing** | `internal/storagefault`, `Application.Stall` / `Fail` | Turns a storage error into a stalled or failed job with a reason a user can act on — never into a failed article. | — |
| **DirectUnpack** | `internal/directunpack` | Streams RAR extraction as whole volumes complete. Reads finished files, never partial article data. | Mutex over volume tracking and kill state; blocking `volumeReady` channel. |

## Mandatory invariants

### 1. One writer updates the record

`Store.ApplyRecord` is the only production writer of `written_articles` rows
and the only `UPDATE` of `job_files` (`complete`, `filename` and
`fetch_policy`), and the recorder is its only caller:
`git grep -n -E '\.ApplyRecord[(]' -- '*.go' ':!*_test.go'`
returns 3 lines, all in `internal/app/record.go` (`flushLocked`, `apply` and
`untrust`). `git grep -n -E 'INTO job_files|UPDATE job_files' -- '*.go' ':!*_test.go'`
returns 3 lines: the two `UPDATE`s in `written.go`, and the one `INSERT`, in
`Store.Admit` (`progress.go`). `Store.Admit` writes each file's row once at
admission, with its structure and initial fetch policy (§6); the reclaim rule
deletes `job_files` rows (§6, `internal/durability/reclaim.go`).

The recorder has two ways in:

- **`noteWritten`**, the body of the assembler's `OnArticleWritten`, called on
  the worker once an article's `pwrite` returned nil. It appends the row to the
  pending buffer **first** and only then calls `Job.MarkArticleWritten`, so
  nothing persisted depends on the Done bit. `ErrNotResident` from it is
  expected for a job evicted mid-write and is logged at Debug.
- **`markDirty`**, called with a file's current `complete`, `filename` and
  `fetch_policy` read from the job's progress (`Application.markFileDirty`), so
  `complete` reads true only once the file was finished (§2). `flush` also
  calls it first for every file whose fetch policy changed since its last
  take, for each job the dispatcher holds (`recorder.takeFetchPolicyChanges`,
  `Job.TakeFetchPolicyChanges`), so no path that moves a policy has to mark
  it.

And three ways out:

- **`flush`** takes **one** snapshot of the pending rows, dirty files and owed
  deletes under `mu`, so a file's `complete=1` cannot land without the rows
  noted before it, nor with rows an untrust removed, and writes it in one
  transaction with `mu` released. On a store error it merges the snapshot
  back before releasing `wmu`. It runs every
  `defaultRecordInterval` (5 s) from `recorder.run`, under `app.ctx`, which
  is what bounds a crash's rework (B1); and synchronously at four
  sites — `git grep -n -E 'recorder\.flush\(' -- '*.go' ':!*_test.go'` returns
  4 lines:
  - `Application.Shutdown`, after the assembler and the dispatcher have
    stopped, on Shutdown's step budget;
  - `reportDownloadComplete`, before the report that moves a complete job from
    `Fetching` to `Assessing`, bounded by `recorderFlushTimeout`: the
    dispatcher persists the new position on its next tick, and restore
    re-checks no completeness for a job past `Fetching`, so a file's rows and
    `complete=1` must be in SQLite first. A flush that fails is logged and the
    report still goes out, since no later report is owed and the job would hold
    its lease at `Fetching`; the window then stays open until the next
    successful flush. A `runFetch` report that reads the job complete between
    `completeFinalizedFile`'s `MarkFileComplete` and `markFileDirty` flushes
    the last file's rows but not its `complete` flag; a crash there costs the
    next start one verification read of that file, which has rows and fsynced
    bytes. That read does not finish a last file with a permanently failed
    article, since failures are not restored for a `complete=0` file, so the
    job restores at `Assessing` incomplete
    (`TestReportDownloadComplete_RecordsTheFileBeforeTheReport`,
    `TestReportDownloadComplete_ReportsDespiteAFailedFlush`);
  - `enqueuePostProc`, after `CloseJobHandles` and before post-processing can
    change the bytes the rows describe, bounded by `recorderFlushTimeout`
    (2 s), since it holds the post-processing admission;
  - `persistAndCommit`, after `historyRepo.Add` (so a failed `Add` does not
    lose progress) and before `RemoveJob` and reclaim, bounded by
    `recorderFlushTimeout`.
- **`apply`** commits verdicts, and optionally whole file states, for one job
  synchronously. It holds `wmu` from its purge through `ApplyRecord`, and the
  purge removes what is still buffered for the file it names — a `DeleteAll`
  verdict's pending rows, a `DeleteArtIdxs` verdict's named rows, the pending
  `complete` of a verdict that sets or clears it — so no flush can write back
  what the verdict removed. Its callers are verification and the retry:
  `git grep -n -E 'recorder\.apply\(' -- '*.go' ':!*_test.go'` returns 4
  lines. Neither names an instance that owes a delete (below): a hydration
  verifies a job with no progress in this process, so one that wrote no row
  here, and a retry's instance is new.
- **`untrust`** removes one file's record (§4). It purges what is still
  buffered for the file under `mu` **first**, records the file's
  `DeleteAll` + `ClearComplete` verdict as **owed** by the job instance, and
  only then takes `wmu` and writes the verdict. The debt is settled by the
  write that carries it and succeeds — the untrust's own, or a later `flush`,
  which carries every owed verdict of a current instance ahead of its rows in
  the same batch, leaving `complete` to a file state the batch also carries,
  since that state was marked after the untrust. A flush that fails keeps the
  debt and, in its remerge, puts back nothing of a file an untrust overran
  while it was in flight. An untrust that lands while a write carrying its
  file's earlier debt is in flight is a new debt: each is numbered, and a
  write settles only the number it carried. The debt of an instance the
  dispatcher no longer holds is dropped with its rows: a retry verifies every
  row before it registers, and the reclaim rule deletes a departed job's rows
  or keeps them for that retry.

**The wait for `wmu` is bounded by the waiter's own context.** `wmu` is
deliberately held across `ApplyRecord`, by every `flush`, `apply` and
`untrust` call:
`git grep -n -E '(recorder|\br)\.(flush|apply|untrust)\(' -- '*.go' ':!*_test.go'`
returns 10 lines. Five of them run under a deadline: Shutdown's flush (its step
budget), the download-complete report's, the hand-over's and the finalizer's
flushes (`recorderFlushTimeout`), and the untrust (`untrustTimeout`). The other
five have none, except that the hydration's commit has one when the hydration
comes from a rename (`SetName`), not from the tick or the startup reconciliation:

| Holder | Context |
|---|---|
| `recorder.run`'s periodic flush | `app.ctx`, which ends at Shutdown |
| a hydration's commit (`appResidency.commit`) | `app.ctx` from the tick and the startup reconciliation, so no deadline from either; a context with `setNameLoadTimeout` (10 minutes) from `SetName`'s `LoadProgress`, which ends the wait, not a blocked syscall |
| `verifyRetry`'s two applies | the retry's: an API request's context, or `app.ctx` for the automatic retry |
| `retryHistoryJob`'s final commit | `context.Background()` |

So `flush`, `apply` and `untrust` take it through
`lockWriter`, which gives up when the caller's context ends and returns its
error; a `flush` or `apply` that gave up took no snapshot and purged nothing,
and an `untrust` that gave up has purged and owes. That is what
bounds an untrust on the assembler's single worker (§4) whatever holds the
lock.

Two guards keep a row off a job it does not belong to:

- **The instance check.** A background flush drops the buffered state of a
  `*Job` that is no longer what the dispatcher holds under its ID, which is
  how a departed instance's rows stay off a retry that reuses the ID. `apply`
  does not consult it: its caller names the instance, and a retry commits
  before `Dispatcher.Add`, when the dispatcher does not hold the rebuilt job
  yet.
- **The `job_files` guard.** `ApplyRecord` inserts a batch's rows only when the
  job still has a `job_files` row, so a flush racing a departure's reclaim
  cannot resurrect rows nothing reaches.

Rows are written `INSERT OR REPLACE`: the most recent successful `pwrite` of an
article is authoritative, and a stale row must not shadow a refetch (#421).

### 2. `complete=1` is written only after the file's fsync

> **`complete=1` means every article of the file is resolved and the file was
> fsynced, on its writing handle or by the verifier. It is never written before
> that fsync, and never for a file whose fsync failed.**

The completion trigger is `processRequest`: `partsWritten >= TotalParts`, where
`TotalParts` is `Job.CountUnfinishedArticles` at `pipeline.registerFile`, so it
excludes articles a restart verified. `Assembler.finalizeFile` then, on the
worker and on the writing handle:

1. `fileWriter.finish` → `fsutil.ShrinkAndSync`: fsync; if the file is longer
   than `ownedRanges.maxEnd` — the end of the last byte range this writer
   claimed or was seeded with (§5) — and that end is positive, truncate to it;
   fsync again. It never grows a file (S6), and a file with no owned range is
   left at its preallocated size.
2. Close the handle.
3. Only if both succeeded: tombstone the file in `completed` and call
   `OnFileComplete`.

The handle is closed at completion; nothing keeps it open for a later step.
The one other place a file is finished is the verifier's `finishFileByPath`
(§3), which applies the same truncate rule (`fsutil.ShrinkAfterSync`) to bytes
it has just fsynced and read back from the device, and never to Done bits that
came from a `pwrite` — and only while the file is at its preallocated size,
since by the next start post-processing may have replaced the file (§3
*What*).

`OnFileComplete` reaches `Application.completeFinalizedFile`, which settles the
whole-file CRC, peeks the archive, feeds DirectUnpack, calls
`Job.MarkFileComplete`, marks the file dirty for the recorder, and reports the
download complete when it was the job's last file. `complete=1` therefore
reaches SQLite at the next flush; a crash before it costs one verification read
of the file at the next start, which finishes it by path unless the file has a
permanently failed article (see the `reportDownloadComplete` flush site in §1's
list of flush sites). The report that
leaves `Fetching` and the hand-over to post-processing do not wait for the
background cadence: `reportDownloadComplete` and `enqueuePostProc` flush
synchronously.

**A truncated failed tail is still charged.** Failed bytes come from manifest
article sizes (`JobProgress.chargeOf` → `FileProgress.FailedBytes`; an
article refused for overlap is charged less, only its uncovered bytes), and
nothing on the verdict path reads a file's size on disk. A no-par2 post with a
failed tail reaches `RepairNoCapacity`; a post with par2 is repaired.

Any error from the finish or the close is a **completion fault** (§4).

### 3. A restart trusts only what it read back

**A Done bit after a restart exists only because a device read matched its
row's CRC, or because its file was `complete=1`** (and not reset by a retry).

#### When

Before the first tick, the restore pass (`Application.restoreProgress`) gives
every restored job whose structure it can read its file table, built from its `job_files` rows
(`fileTableFromRows`), and a progress record seeded with each file's recorded
row count, `complete` flag, filename and fetch policy, with every file
unverified. **It sets no article bit.** A job whose rows are missing, gapped or unreadable
is given an operational error and, if it has an open attempt, settled Failed
through `Dispatcher.SettleFailed`; a job that has never run is left without a
table, and the tick's first hydration of it fails for want of one
(`failUnrestorable`; `Hydrate` refuses a job without a table) and settles it
Failed.

**A recorded count sits beside the bits and is not one.** The restore read
(`Store.SeedRows`) returns, with each file's `job_files` row, how many
`written_articles` rows the file has, and the seed keeps that number on the file
(`FileProgress.Recorded`). While the file is unverified, its downloaded bytes
are estimated as `Bytes × Recorded / ArticleCount` (`fileBytesDownloaded`), in
the NZB's encoded unit and clamped to `[0, Bytes]`; installing the file's
verification replaces the estimate with the maintained figure and zeroes the
count. A count of rows is not a read-back: it can overstate what the device
still holds, and the verification that follows may delete rows. That is why it
may only feed an estimate. In `internal/job`, `git grep -n -F '.Recorded' --
internal/job ':!*_test.go' ':!*.spec'` returns 3 lines: the seed and the
zeroing write it, and `fileBytesDownloaded` is the one read. The statement
above is unchanged by it. The pin is `TestRestoreProgress_SeedsNoDoneBit`,
mutated by `internal/app/testdata/restore_progress.spec`.

Verification runs **before** the job's articles are attached, and its outcome
decides whether attaching happens at all. `appResidency.Hydrate`, on the branch
where a file is not yet verified in this process (`verifyAndAttach`):

1. builds the manifest from the attached table and the article list in the
   manifest file, and reads the job's `job_files` and `written_articles` rows;
2. calls `verifyJobFiles`, which reads bytes and returns a verdict without
   touching the job or a row;
3. commits the verdict's deletes and `complete` changes through `recorder.apply`;
4. only then calls `Job.AttachArticles` and installs what was verified for
   every file (`installVerification`), which marks each file verified.

If any of steps 1 to 3 fails or is cancelled, no article is attached and no file
is marked verified, so the next hydration starts again from step 1. A failure
in step 4 is per file: `Job.InstallFileVerification` returns an error having
changed nothing (the job is not resident, or the index names no file),
`installVerification` logs it and goes on, and that file stays unverified with
the job's articles attached. `Job.AllVerified` is then false, so the next
hydration after an eviction verifies again. So **a job `Hydrate` attached was
verified first**, and `Hydrate`'s early `AttachArticles` for a job whose every
file is verified is sound. A job gains progress in three other ways, and only the first is not
verified when it arrives: the restore pass seeds it with every file unverified
and sets no article bit, so the job's first hydration takes the verifying
branch; a freshly ingested job has no rows and is verified from the start; a
retry's rebuilt job arrives flagged verified (it is built as an ingest is) and
`verifyRetry` reads its rows back itself.

Every path that attaches a job's articles (`Job.AttachArticles`) to a job with
rows goes through this, or verifies itself:

- the dispatcher's tick (`reconcileResidency`), for a job that holds what its
  position requires;
- `Dispatcher.SetName` → `LoadProgress`. A rename is refused once
  `Job.DownloadBegun()` is true and moves nothing on disk; verification runs
  first, so the restored filenames make the job "begun" (`verifyJobFiles`
  deletes a complete=0 file's rows recorded without a filename rather than
  keep them) and a rename can never leave rows pointing at a stale path.
  `LoadProgress` calls `Hydrate` even for a job with progress, so it waits
  for a hydration that has attached content but not yet installed its
  filenames. `SetName` bounds its load by `setNameLoadTimeout` (10 minutes); a
  rename that exceeds it fails, attaches nothing, and can be retried
  (`registry.go`);
- `Application.fileOwedUnwantedFailures` → `appResidency.Hydrate`, before the
  first tick, for a restored job the archive peek owes a filing;
- `Application.verifyRetry`, for a retried job: it is rebuilt by
  `BuildIngestJob`, which attaches its files and articles, so `Hydrate` would return
  early and never verify it (see *Retry* below).

`git grep -n -E '\.Hydrate\(' -- '*.go' ':!*_test.go'` returns 3 lines, one for
each of the first three paths (`tick.go`, `registry.go`, `startup_reconcile.go`).
The file table and the seeded progress are attached
earlier and separately, by the restore pass (above), which does not go through
this. Of the four paths, the tick is the residency rule; `SetName`'s
`LoadProgress` and `fileOwedUnwantedFailures`' direct `Hydrate` load a job the
rule would not (`SetName`'s: any job, paused or not holding what its position
requires, which the tick would not hydrate; `fileOwedUnwantedFailures`': a job
before the first tick), and `verifyRetry`
verifies a job built with its articles.

A paused job is not hydrated by the tick even while it holds a compute slot
(`reconcileResidency`'s `IntentPause` test). Without that, a job parked by a
verification fault would be re-read, and fault again, on every tick. A job
restored paused at `Fetching` is therefore not verified at startup. It reports
the recorded-count estimate (`Bytes × Recorded / ArticleCount`) until it is
resumed, whose tick then hydrates and verifies it, or until a rename loads it
(`SetName`'s `LoadProgress`), after which it stays resident while paused
(`reconcileResidency`'s eviction arm skips `IntentPause`).

#### What

Every file with `complete=0` and at least one row is read, whatever the job's
state and the file's fetch policy: a crash between a file's completion and the
next flush can leave such a file in a job that has already left `Fetching`
(the flush before the download-complete report narrows that window to a report
whose flush failed and a `runFetch` report racing the last completion, both
described with that flush's site above), so no state makes the read unnecessary. For each such file, in order:

| Case | Outcome |
|---|---|
| empty `filename` | every row deleted: none can be located |
| opening the job directory returns `ENOENT` | **verification fault** (below); on a retry every row of the file deleted, like the row below |
| opening the job directory fails otherwise | verification fault |
| `open` of the file inside the open directory returns `ENOENT` | every row deleted; the file's articles are Outstanding |
| any other `open` error, including a name that leads out of the job directory or any link (symlink or hard link) in the file's place | verification fault |
| `fsync` on the fresh descriptor fails | every row deleted: the file is untrusted |
| a row with `offset < 0` or `length < 0` | that row deleted unread |
| a zero-length row | verified when its CRC is 0, otherwise deleted; it claims no range, so it never fails another article |
| a row whose bytes read back with its CRC | verified |
| a row whose CRC differs, or a short read at EOF | that row deleted; its article is Outstanding |
| rows whose ranges intersect | in offset order, the first matching row is kept; each other article is **failed** and its row deleted, so a restart cannot alternate between them |
| any other read error, or a cancelled context | verification fault |

**A missing directory is a fault, not an absence.** `ENOENT` proves a file is
gone only inside a directory that exists. A download root on an NFS mount that
has not come up at boot makes every file of every job `ENOENT`; reading that as
absence would delete every job's recorded progress. A job whose directory the
user deleted by hand therefore parks rather than refetching at a hydration, and
the operator's resume re-verifies it. **A retry differs** (*Retry* below): it is
the user's explicit act, and `restoreFailedDir` has already put back a
directory it could, so a directory still missing (`ENOENT` opening it) is
taken as absence. **A retry cannot tell an unmounted download root from a
deleted job directory**. On an unmounted share `restoreFailedDir` finds
neither the `_FAILED_` directory nor the job directory and moves nothing, and
`readBackFile` gets `ENOENT` opening the job directory, so the retry
drops every file's rows and refetches the job, though the bytes may still be
on the share. Any other error opening the directory stays a verification
fault on both paths. The decision lives in one place, `readBackFile`'s `retry`
parameter.

**A job file is always a regular file with one link; any link in its place
is refused.** Every open of a job file — the writer's and both of the
verifier's — goes through `fsutil.OpenNoFollow` on an `os.Root` on the job
directory, for the file's recorded name, which must be a single path
component:

- the open is one `openat(2)` on the directory with `O_NOFOLLOW`, so a
  **symlink** in the file's place fails with `ELOOP` wherever it points, out
  of the job directory or at a sibling inside it. `os.Root` alone is not
  enough: it follows a link whose target stays inside the root, and ignores
  `O_NOFOLLOW` for it;
- the opened descriptor is then `fstat`ed, and anything but a regular file
  with a link count of one is refused, so a **hard link** sharing a sibling's
  inode — a regular file `O_NOFOLLOW` cannot see — is refused too. Both
  checks are on the open itself, with no window between a check and a use.
  The open is non-blocking, so a FIFO in the file's place cannot hang it, and
  it never carries `O_TRUNC`, which would act before the check;
- a name that is not one component (`..` included) never reaches `openat`.

The `os.Root` confines the file name only. The job directory is `DownloadDir`
joined with the job's name, already cleaned by `filepath.Join`, and it stays
under `DownloadDir` because the dispatcher admits only a name that is one safe
path component (`Dispatcher.admitNameLocked`).

**A file's name is chosen once and recorded, and the writer and the verifier
resolve it from the record verbatim afterwards.** The first time `registerFile` names a file,
`pipeline.newJobFileLocation` sanitizes its subject under the sanitize options
then in force — one component whatever they hold, because the sanitizers never
use a replacement string `fsutil.CheckReplacement` refuses — and the name is
recorded (`Job.RecordFileFilename`). A later registration and both verifiers
resolve the recorded name with `pipeline.jobFileLocation`, which does not
sanitize it again: the options may have changed since the file was named, and
re-sanitizing would look for a file the writer never wrote, which the verifier
reads as absent and refetches (`TestRecordedFileName_SurvivesASanitizeChange`).
It only checks that the name is one path component
(`fsutil.CheckOneComponent`, the check `OpenNoFollow` applies), since the name
is joined into a path; a name it refuses is a verification fault, and a failed
registration.

On the verifier (`readBackFile`, `finishFileByPath`) a refusal is an open error
other than `ENOENT`, so a verification fault on both paths — never absence, and
never a read, fsync or truncate of another file. A job directory that exists
but cannot be opened for reading is a fault too, since the directory is opened
before the file. The job directory itself is opened as resolved: the download
root and the sanitized job name are trusted configuration, and a symlink there
is followed.

On the writer, `registerFile` resolves each file as above
(`pipeline.newJobFileLocation` the first time, `pipeline.jobFileLocation`
after) and hands the assembler the
job directory and the name as `FileInfo.Dir` and `FileInfo.Name`.
`openTargetFile` creates the directory, then opens the name `O_WRONLY|O_CREATE`
(`openInDir`). A refusal takes the path of any other failed open: a routed
`open` fault, and the article handed back as unwritten
(`TestOpenTargetFile_RefusesToWriteOutOfTheJobDirectory`). So no bytes of one
file reach another through a link (Standing Design Rule 3). The first time a
file is named, `uniqueJobFileName` checks the name with `Lstat` through an
`os.Root` on the job directory, so a name already held by a file or a link
gets a numeric suffix rather than being reused; a recorded name is not
re-checked, and the open is what refuses a link planted after it was chosen.

The fresh descriptor's fsync reports a writeback error **no earlier fsync has
reported** (Linux ≥ 4.16). On Linux the file's cache is then dropped
(`POSIX_FADV_DONTNEED`), so the reads come from the device rather than from
pages marked clean after a failed writeback; elsewhere `dropPageCache` is a
no-op. Rows are read in offset order through one reused 1 MiB buffer.

A file whose articles are then all resolved is **finished by path**: the
`fileFinishable` predicate (`FetchAlways`, not complete, a non-empty article
range, every article resolved) decides it, `finishFileByPath` runs
`fsutil.ShrinkAfterSync` with the end of the last verified row — truncate if
the file is longer, then fsync, with no fsync before the truncate and none
when nothing was truncated, since the read-back's fsync on the fresh
descriptor already landed the file and the verifier writes nothing to it
in between — and only then does the verdict carry `SetComplete`. A stat,
fsync or truncate error while finishing is a verification fault.

**The finish truncates only a file at its preallocated size** (`preallocSize`,
the file table's declared byte count, which `registerFile` also hands the
assembler as `FileInfo.ExpectedSize`). The trim exists to drop the zeros
preallocation left past the last article (*Pre-allocation*). A file longer
than its last row's end at any other size holds bytes preallocation did not
put there. One way to produce it: par2 replaces the file with a repaired copy,
in a job whose `complete=1` and queue state never reached SQLite, so the job
can restore at any state, `Fetching` included, and the last article's row is
a correct but short prefix. Every row then matches the repaired copy, and
cutting it to the row's end would deliver it short once the par2 set is gone
(#829). So the guard keys on the file, not on the job's restored state. Such
a file is finished and set complete at its size. The repaired copy is already
at the size par2's index records, so identification accepts it; what keeps
quickcheck from passing it clean is the CRC compare (`verifyIdentified` in
`internal/par2/assess.go`): the CRC settled from its rows covers only the
short prefix, so it mismatches par2's whole-file CRC and the file goes to
par2. A file whose rows cover all of its true bytes and which carries a stale
tail is a different case: with par2 its length differs from the index's, so
verification counts it `Unverified` whatever its CRC and it goes to par2;
without par2 it is delivered with the tail (*Accepted limitations* 9).

A `complete=1` file is not read. Its rows are installed as they stand
(`Job.InstallFileVerification` with `Complete` set): each row's article is Done, **every other article
of the file's range is failed** — the failed set is the complement of the rows
— the file is Complete, and its whole-file CRC is settled. The failed set is
installed during hydration, before anything can consult it (the archive peek,
the DirectUnpack feed).

A row that names an article outside its file's range, or has an invalid offset
or length, is dropped at install (`placeRows`) and costs its own article
(Standing Design Rule 3).

#### A verification fault parks the job; it never fails it

`verifyJobFiles` returns an `*errVerifyFault` naming the file or directory.
`Hydrate` classifies it through `storagefault.Classify`, parks the job through
its `parked` callback, `Application.parkVerifyFault` — whether the fault
classifies retryable or permanent — and returns an error wrapping
`dispatch.ErrResidencyFault`, which `reconcileResidency` does not settle. The
job stays non-resident and paused, with a reason naming the file in the queue
listing's `stall_reason`, which begins `Stalled (resume to retry): `.

`parkVerifyFault` is `Stall` with the record marked `stallVerify`, which a
re-evaluation leaves in place, pause and reason both, until the job itself is
resumed (*Stalls and their re-evaluation*). Resuming the job hydrates and
verifies it again. A sector that stays unreadable therefore keeps the job
parked until the user resumes that job, and each such resume costs one
read-back pass. Deleting the file sends it down the `ENOENT` arm and
refetches it.

A context error is returned as it is, parks nothing, and settles nothing.

#### Files the verifier finished

`installVerification` installs each file's outcome in two phases while the
manifest is certainly attached. First `Job.InstallFileVerification` applies,
under one hold of the job's content lock, the recorded filename, the recorded
fetch policy (hydration only), the rows, the intersection's failed articles
and, for a file whose verdict carries `SetComplete`, the settled CRC — the
settle comes last because it derives the CRC from the installed rows and
releases them. No reader sees part of that step. Then, for a finished file,
with no job lock held, it runs the archive peek, and only then calls
`MarkFileComplete` — the mark is what lets the download-complete report take
the job to Assessing, so a last file marked first could reach post-processing
unpeeked. The completion is then queued on
`internalFileComplete` as `FileComplete{Resumed: true}`
(`enqueueResumedCompletion`), and its consumer skips the settle, the peek, the
mark and the DirectUnpack feed, so it lands even if the job is evicted first.

The peek's failure message is a job-level fact: the first non-empty message of
a hydration is carried, as `FileComplete.FailMsg`, on every Resumed completion
of that hydration, so the job is filed with the flagged names whichever
completion arrives first.

When the channel is full, the send moves to its own goroutine that gives up
when `app.ctx` is cancelled. It cannot block the caller:
`fileOwedUnwantedFailures` runs inside `Dispatcher.StartWith`, before
`watchCompletions` starts. A completion it gives up is re-derived by the next
start's verification.

#### Retry

`retryHistoryJob` verifies the rebuilt job before registering it
(`verifyRetry`):

1. **Shape check.** Every row must name an article inside its file's range in
   the rebuilt job's file table; on a mismatch every row of the job is deleted,
   unread.
2. **`complete` is cleared on every file**, because post-processing may have
   repaired, moved or deleted the bytes since they were written. A retry
   therefore reads every file with rows once. A file quickcheck moved into a
   par2 subdirectory is at a path its `filename` does not name, so it takes the
   `ENOENT` arm and is refetched whole.
3. `verifyJobFiles` with `retry` set: an article an intersection failed is not
   counted resolved, because `Job.ResetForRetry` is about to clear that
   failure, and a finished file would contradict it. Cleared and fetched again,
   it re-collides with the seeded winner (§5) and is refused. A file whose
   directory is missing takes the `ENOENT` arm instead of the verification
   fault (*What* above): its rows are deleted and it is refetched
   (`TestRetryHistoryJob_AfterDownloadDirDeleted`), whether the directory was
   deleted or its download root is not mounted. Any other error opening the
   directory is still a verification fault, and the retry aborts.

Each step's verdicts are committed before the next. Then `ResetForRetry`,
`Assembler.ForgetJob`, the manifest write, `seedJobFiles`, and a synchronous
`apply` of every file's state, all before `Dispatcher.Add`; the files the
verifier finished are owed a Resumed completion once the job is registered.

The retry passes no archive peek: its job is not registered when
`installVerification` runs, and the peek reads the registered job's state. A
retried job's verifier-finished files were peeked live in the failed attempt
under the same rules, and post-unpack removal is the backstop.

**Two fetch policies are two facts.** `job_files.fetch_policy` records what the
failed attempt fetched, and the rebuilt job's policy records what this attempt
will fetch. `fileFinishable` reads the stored one: a recovery volume a damage
verdict released and that was fetched whole is finished, and the rebuilt job
then holds it as `FetchIfNeeded` with `Complete` set
(`TestRetryHistoryJob_ResumesCompletedFilesFromTheRecord`).

### 4. An fsync fault untrusts the file

A failed fsync means none of the file's written bytes can be vouched for, and
a second fsync on any descriptor may return nil over the lost pages. So the
**file** is the unit: it is untrusted and refetched whole. Untrusting is
`OnFileUntrusted` → `Application.handleFileUntrusted`, synchronously on the
worker, in this order:

1. The file's rows and `complete` are removed from SQLite through
   `recorder.untrust` (a `DeleteAll` + `ClearComplete` verdict), which first
   purges what is still buffered for it and records the delete as owed (§1).
   This has to land before the job can be
   evicted and re-hydrated: hydration restores `complete` from `job_files` and
   would otherwise reinstate a stale `complete=1`. It is bounded by
   `untrustTimeout` (2 s), the wait for `wmu` included (§1), shorter than
   `closeHandlesTimeout` so a close that untrusts a file keeps time to report
   its fault.
2. `Job.UntrustFile` returns every Done article of the file to Outstanding,
   clears `Complete` and the CRC, and releases the resident rows. An article
   already failed stays failed: `markNotDone` refuses a failed article.
3. `pipeline.forgetFile` drops the cached `FileInfo`, so the first refetched
   article re-registers the file — recounting `TotalParts` and seeding owned
   ranges from the now-empty rows — and opens a fresh writer.

If the SQLite write fails, or the wait for `wmu` runs out, what stays in
SQLite is rows flushed earlier with `complete` still 0: the purge dropped the
buffered ones before the wait, and `complete=1` is written only after a
successful fsync. The delete stays owed, and every later write for the job
instance carries it ahead of the file's rows until one succeeds (§1), so a
refetch that completes the file in this process cannot write `complete=1`
over a row of the failed episode — the row an article that now fails
permanently would otherwise leave behind, to be installed Done at the next
start and chained into a whole-file CRC over bytes the fsync disowned
(#827). A restart before any such write finds the rows with `complete=0` and
reads each back before trusting it.

The assembler untrusts a file at three points (`noteFileUntrusted` has three
callers in `internal/assembler/assembler.go`):

| Where | What failed | Also |
|---|---|---|
| `finalizeFile` | the completion finish or close (§2) | **no tombstone** — a tombstone would route every refetched article to `handleLateDuplicate`, which writes nothing once the handle is gone, so the refetch would never converge; the fault goes to `OnWriteFault` → Stall or Fail |
| the close-handles arm of `CloseJobHandles` | the close-time fsync or close | the fault is sent on the control message's ack (*Hand-over to post-processing*) |
| `syncAndCloseAll`, at worker exit | the close-time fsync or close | the fault is not routed: there is no caller left to answer |

**The untrust is the whole of the release after a failed fsync.** The writer
keeps no record of which articles an fsync covered: Linux reports a writeback
error to a descriptor once and marks the failed pages clean, so a failed fsync
is answered for the whole file. Its articles were written, so they are `Done`
rather than `Emitted`, and `OnArticlesUnwritten` has nothing to clear for them;
`Job.UntrustFile` clears their `Done` bits. It returns `ErrNotResident` for a
job whose manifest is evicted, and the release is then skipped; a job is
evicted with writes still in flight only once it has settled or been removed,
and neither is dispatched again in this process. `fileWriter.sync` is called only
inside `syncAndClose` (`git grep -n 'w\.sync()' -- 'internal/assembler/*.go'
':!*_test.go'` returns 1 line), which closes the writer right after, so the
writer and its owned ranges are discarded with it. A redelivery of any of the
file's articles registers the file afresh and lands on a new writer whose
`owned` is seeded from `FileRows`, which the untrust emptied.

These two fsyncs, and the completion finish, are what report errors first. So
no file whose error was already consumed reaches a restart with rows, and the
verifier's own fsync and cache drop are a second line of defence rather than
the only one.

### 5. Overlapping articles: first writer wins, by byte range

`fileWriter.owned` (`ownedRanges`, `internal/assembler/ranges.go`) is the
single owner of which article wrote which bytes of a file.

- **Within an episode, a range is claimed only after its write returned nil**
  (`writeOne`); the ranges seeded at open are the other entries (below). An
  article whose write faulted owns nothing, so it cannot cause a later article
  to be refused.
- **An arrival whose `[off, off+len)` intersects a range another article owns
  is refused** (`acceptArticle`, through `ownerOf`) and failed permanently. Its
  part still counts toward `TotalParts`, and par2 is charged the bytes of its
  range no other article's range covers (`ownedRanges.covered`, #432).
- **An owner never arrives at the overlap check again.** Claiming a range
  resolves its article `Written`, which keeps it in the writer's `seenDone`,
  and `admit` drops a redelivery of an article in `seenDone` before
  `acceptArticle` runs. So a range is owned against every arrival, and `claim`
  panics on any intersection
  (`TestFileWriter_RangeOwnersStayAdmitted`, #930).
- **A zero-length article claims nothing.** It is reported written with
  `n == 0`, and its row (length 0, CRC 0) is valid: the next verification
  verifies it, and `InstallFileVerification` installs it, as for any row
  (`WrittenRow.HasValidShape` is the one shape rule). It is never refetched on
  that account.
- Intersection is detected, not only a shared start offset
  (`TestOverlap_PartialRangeOverlapIsRefused`,
  `TestOverlap_ContainedOverlapStillCompletesTheFile`).

**Within one open-file episode this is enforced by the writer. Across a
restart it is enforced by seeding.** `pipeline.registerFile` passes the file's
resident rows — those a restart verified, and any this process wrote since —
as `FileInfo.Owned`, and `openTargetFile` hands them to `seedOwned` before any
write. Seeded ranges are owned by `seededOwner`, whose `artIdx` (-1) names
no manifest article. A zero-length row is skipped silently
(it claims no range). A malformed seeded range (negative offset or length, or an
overflowing end) is dropped with a warning, which costs only a refetch of its
articles.

The verifier keeps only disjoint rows (§3), so seeding never has to choose
between two articles.

**The one known NN4 exception:** when a bogus article arrives first, its good
neighbours are refused. A par2 post repairs them; a no-par2 post ends at
`RepairNoCapacity`, which is the honest NN1 outcome (#759).

### 6. The record's rows have one lifecycle rule

**Admission.** `seedJobFiles` → `Store.Admit` inserts every `job_files` row in
one transaction at submission, before `Dispatcher.Add`. It writes each file's
structure — subject, bytes, article count, from the job's `FileTable` — as well
as its fetch policy, and leaves `complete` and `filename` empty for the recorder
to fill in; a row that already exists is left whole. It is a precondition
for the recorder: an `UPDATE` matching no row is not an error, so a file with
no seed row would silently persist nothing. A retry re-seeds, inserting only
missing rows.

**Readers.** `Store.FileRows` reads one job's rows (hydration, `app.go`'s
retry read), and `Store.SeedRows` reads every job's at once, with the count of
`written_articles` rows per file, for the restore pass
(`Application.restoreProgress`, once per `Start`). `git grep -n 'FROM
job_files' -- internal ':!*_test.go'` returns 3 lines: those two reads and
`insertRows`' existence check, which reads no column. The restore pass's
read names a job whose row does not read as its column types in a second
return value and drops that job's rows, so the restore pass settles that job
Failed and no other; only a query or iteration failure fails the call. It is
one query, not a transaction across the two tables, which is sound
because `Application.Start` starts the assembler, which feeds the recorder,
only after `StartWith` returns.

**Departure.** The reclaim rule (`internal/durability/reclaim.go`) deletes a
job's rows from both tables when nothing reaches the job — no queue row, and
no FAILED history entry. `Store.Reclaim` applies it after a departure and
`Store.SweepOrphans` at startup, before anything can call `Admit`; there is no
periodic sweep, because one would reclaim a job between `Admit` and
`Dispatcher.Add`. The rule re-derives its answer from the queue and history as
they are, so a crash or a missed call can delay a reclaim but never make one
wrong. Neither table has a foreign key, so nothing removes rows implicitly.

**Verdicts.** Between the two, rows leave only through `ApplyRecord`'s
verdicts: verification's deletes (§3) and an untrust (§4).

### 7. A storage fault never marks an article failed

This is A1, and it is a hard rule. `ENOSPC`, `EIO`, `EROFS` and a wedged mount
are conditions of *storage*. They say nothing about any article's availability
on any server.

`storagefault.Classify(op, path, err)` produces a `*Fault` carrying the
operation, the path, and whether the condition is `Permanent`:

| Fault | Route | Articles |
|---|---|---|
| write fault (`pwrite`), or the file could not be resolved, created or opened | `OnArticlesUnwritten` for the one article, then `OnWriteFault` → `Stall` if retryable, `Fail` if permanent | the article returns to **Outstanding** |
| completion fault (§2) | untrust (§4), then `OnWriteFault` → `Stall` / `Fail` | the file's articles return to **Outstanding** |
| close-time fault at the hand-over | untrust, and the run fails with it (*Hand-over to post-processing*) | — |
| verification fault (§3) | at a hydration, always `parkVerifyFault` (a `Stall` lifted only once the job is resumed), never `Fail`; on a retry, the retry is aborted | nothing attached |
| a `Stall` or `parkVerifyFault` for an instance handed to post-processing | no stall record, but the instance is still paused; the reason goes to `offerFaultToAdmission` (*Hand-over to post-processing*) | as the fault's own row |

In no case is `Job.MarkArticleFailed` called, the failed-byte count touched, or
the job's reported health degraded (R21). Attributing a full disk to the
article would burn its retry budget over something a user often fixes in ten
seconds.

**A hydration's verification fault always stalls, even when it classifies
permanent.** The job has no progress in this process yet, so
failing it would send it to history and discard the bytes an earlier run left
on disk, over an `EACCES` on a mount that has not finished coming up.

`OnWriteFault` routes on another goroutine (`handleWriteFault`): run inline on
the worker, `Fail` → `CloseJobHandles` would wait for the very worker calling
it. The job instance is resolved on the worker before the hand-off, and `Stall`
and `Fail` act on that instance alone, through the dispatcher's instance doors
(`PauseInstance`, `SetFailReason`, `ResumeInstance`): an instance no longer
registered under its ID is neither parked nor failed, so a retry registered
under the ID before the goroutine runs is left alone
(`TestHandleWriteFault_ActsOnTheAttemptRegisteredOnTheWorker`,
`TestStallAndFail_LeaveALaterInstanceAlone`).

The one thing that must **not** go through fault routing is a bookkeeping
defect — a row or an index the manifest does not have. Those fail loudly as
ordinary errors (A2, R28) or, where Standing Design Rule 3 bounds the cost, are
dropped at a cost of their own article (`placeRows`, `seedOwned`). Routing them
as storage faults would blame a disk for a numbering bug, which is the A1
conflation in reverse.

## The whole-file CRC

`fileCRCFromRows` (`internal/job/verified.go`) is the only computation of a
file's whole-file CRC. It combines per-article CRCs with `crc32util.Combine`
**if and only if** every article of the file's range has exactly one row, none
failed, the first row is at offset 0, and each row starts exactly where the
previous one ends. Otherwise it returns `NoCRC` and par2 does a full verify.
Such a chain cannot overlap and cannot leave a gap.

`Job.SettleFileCRC` applies it and stores the result on
`FileProgress.AssembledCRC32` (zero when underivable, which par2 reads as
`NoCRC`), then releases the file's resident rows. It is called from
`completeFinalizedFile` (and the `jobtest.SeedFileCRC` test helper). It
sorts and combines the rows with the job's `contentMu` released, so the
assembler's `MarkArticleWritten` does not wait on the combine, and stores the
result only if the file's resident rows are unchanged when it re-takes the
lock; otherwise it derives the CRC again under the lock from the rows as they
are then (`TestSettleFileCRC_RefusesAStaleCombine`,
`TestSettleFileCRC_CombinesWithoutTheLock`). For files a restart finished or
found `complete=1`, `Job.InstallFileVerification` runs the same derivation
(`settleFileCRC`) under the lock it already holds.

The per-article rows a file needs for it are kept resident in
`JobProgress.written` until the CRC is settled — installed by
`InstallFileVerification` (or `InstallVerified`) and appended by
`MarkArticleWritten` — so nothing on the worker path reads SQLite. A stored
slice is never edited in place, so a `Progress()` clone shares them. A row
also leaves with its article's Done bit: `JobProgress.clearDone` is the one
clearer of that bit and drops the row with it, so a resident row's article is
Done, and `MarkArticleWritten` appends a newly-Done article's row without
searching the file's rows for an earlier one.

**Its consumer is `par2.Assess`**, reached through `FileAssembledCRC32` from
`internal/app/par2names.go` and `internal/postproc/stage_quickcheck.go`. A
`Clean` outcome makes `stage_repair.go` skip the par2 verify+repair subprocess
entirely, and `par2Verdict` returning `outcomeClean` leaves the deferred
recovery volumes unfetched. A file with no CRC reads `NoCRC` and takes the
conservative branch (`outcomeRepair`), provided the file was identified
against the par2 index at all; one `Identify` cannot match reads
`outcomeUnknown`, and its volumes are held rather than fetched or discarded
(`docs/post-processing-contract.md` § "On-Demand Par2: Fetch Policy and Verdict").

A permanently failed article — interior or at the tail — leaves no row, so the
file reports no CRC. That is the correct answer rather than a gap: a partial
CRC recorded as the file's would report corruption for a file that is merely
incomplete (#349, #387).

## Lifecycle hooks

| Event | Behaviour |
|---|---|
| Write fault | The article is resolved `RolledBack` (`fileWriter.resolve`) and its part rolls back, `OnArticlesUnwritten` clears its Emitted bit, `OnWriteFault` → Stall or Fail. No row is written. |
| Completion fault | §4: untrusted, no tombstone, `FileInfo` dropped, Stall or Fail. On resume the file's articles are Outstanding and are refetched. |
| Pause, low disk, server penalty | Nothing writes the record; no hook. |
| Reload (`ReloadDownloader`) | Stop the old downloader; `setCompletions(nil)`, which returns once every buffered result has reached the assembler's queue; `Assembler.Quiesce`, answered once everything queued ahead of it has been processed and its callbacks have run; `ClearEmittedForReload(false)` for every job not handed off to post-processing (neither admitted nor past its admission); start the new downloader, paused first if the application is paused, and swap it in, both under `app.mu`. An Emitted bit still set after the quiesce covers only an article whose bytes never reached `pwrite`. If the quiesce fails, the clear is skipped and those articles stay Emitted until the next start. |
| Remove (`RemoveJob`) | `CancelJob` closes the job's handles; reclaim deletes its rows. A racing flush is stopped by the instance check and the `job_files` guard, and any orphan rows by the next start's `SweepOrphans`. |
| Retry | §3 *Retry*. |
| Clean shutdown | §*Clean shutdown*. |
| Hand-over to post-processing | §*Hand-over to post-processing*. |

## Hand-over to post-processing

`enqueuePostProc` admits the job, then calls `CloseJobHandles`, then flushes
the recorder synchronously, then forgets the job's cached `FileInfo`.

`CloseJobHandles`' close arm `Sync`s and `Close`s every open handle of the job
without deleting, untrusts a file whose fsync or close failed (§4), tombstones
the files and the job, and **sends any close-time fault on its ack**.
`enqueuePostProc` reads it: any `*storagefault.Fault` in the joined error,
permanent or retryable, becomes the run's failure reason, because nothing ever
retries a fault observed here and the lost bytes would otherwise reach
par2/unrar as a hole. It is offered to the admission before `beginHandOver`
seals it, so the run's `FailMsg` carries it unless the admission already has a
reason. Any other error — `closeHandlesTimeout` expiring with no fault
observed — is logged at `Warn` and the run goes on. **This is A2's one named
exception**: no fault was observed, and the handles may still flush later
(`TestEnqueuePostProc_ACloseFaultFailsTheRun`,
`TestEnqueuePostProc_ACloseFaultIsNotedBehindAnEarlierReason`,
`TestEnqueuePostProc_ACloseTimeoutRunsTheStages`).

The close-time fault is **reported, not routed**: the job is already admitted,
so `Fail` cannot hand it over again and `Stall` would record a stall for a job
whose files post-processing is using. **So is a fault routed after the
admission.** A write
in flight at the hand-over can raise a fault whose `Stall` runs after `admit`,
and a hydration can raise a verification fault (`parkVerifyFault`). `stall`
writes no record for an instance that is admitted or whose admission has ended,
and passes the fault's reason to `offerFaultToAdmission`, the close arm's helper
too, which offers it to the admission and logs where it went. `stall` still
pauses the instance, since a verification fault relies on the pause to stop
`Dispatcher.reconcileResidency` re-hydrating the job every tick, and the
post-processor runs a paused job regardless; the
`admitOutcome` constants say where each outcome sends it
(`TestStall_Admission`,
`TestPostProcAdmissions_NoReasonAfterTheNotesAreTaken`). The reason is
offered, not dropped, because of A2: the downloader skips an admitted job, so
an article the fault rolled back is never fetched again, and the run's
`FailMsg` or a note in its history entry is the fault's only disposition.

**A job instance whose handles `CloseJobHandles` closed is not downloaded
again.** The job-level tombstone drops any article still in flight, and the
downloader skips an admitted job (`downloader.Options.HandedOff`). A retry
under the same ID is a new instance; `RetryHistoryJob`'s `ForgetJob` clears the
assembler's tombstones for it, and a retry whose `ForgetJob` fails is aborted.

## Clean shutdown

`Application.Shutdown` stops the downloader, then the assembler, then cancels
the context, waits for its goroutines, stops post-processing and the
dispatcher, and **then** flushes the recorder.

`Assembler.Stop` drains the request channel and then runs `syncAndCloseAll`:
each open file is fsynced and closed, and one whose fsync or close failed is
untrusted (§4). Partial files are closed without firing `OnFileComplete`. The
flush comes after the assembler has stopped, so it carries the row of every
write the drain processed.

**No job is parked during shutdown.** `Application.stopping` is set at the top
of `Shutdown`, before any of its steps, and `Application.stall`, the body of
both `Stall` and `parkVerifyFault`, refuses to pause a job while it is set, or
once `app.ctx` is cancelled. The pause would be
the one that cannot be undone: it is persisted, the stall list that would
re-evaluate it is in memory and dies with the process, and nothing at startup
resumes it. A pause the shutdown persisted stays in force after the restart, so
that job is not verified until it is resumed or a rename loads it (§3), and it
reports the recorded-count estimate, not a verified figure, until then. A job
that was resumed is verified at its first hydration.

**A permanent fault still fails the job**, and `Application.Fail` has no
stopping guard, because nothing it persists positions the job for
post-processing. Its hand-off is held in memory: a post-processor still running
files the job as Failed, and one that stops first takes the hand-off with it,
so the job restarts at the state it was in, its outstanding articles offered
again. For a job at `Assessing`, or due there, `Fail` leaves its reason, also
in memory, for the job's Assessing worker, which hands the job over in place of
its verdict (`docs/post-processing-contract.md` § "Pipeline Architecture & Queue
Scheduling"). Of the fields `Fail` writes, `Header.FailReason` is the persisted
one, and a restarted job keeps it until it leaves the queue.

## Stalls and their re-evaluation

`Application.Stall` records a reason, then pauses the job (R19, R27), unless
the job is admitted to post-processing or past its admission (below). It
releases only a `Fetching` job's lease; a job that has moved on keeps its
worker, and the pause gates its next move. `reevaluateStall` runs on an
interval (`stallRecheckInterval`, 30 s) and on user action
(`ReevaluateStalls`, from the API's per-job resume and from both queue-wide
resumes, the user's and the low-disk auto-resume, which share
`resumeLocked`). It retries nothing: a
write or completion fault left the affected articles Outstanding, and a
verification fault left the job non-resident, so a resumed job refetches and
re-verifies through the ordinary paths. If the condition has not cleared, the
next fault parks the job again. The record is cleared before the job is
resumed, so a fault the resumed job raises at once records a stall of its own.

**A verification fault is lifted only by a resume of the job.**
`stallRecord.kind` is the latest fault's: `stallRetryable` from `Stall`,
`stallVerify` from `parkVerifyFault`. A re-evaluation leaves a `stallVerify`
record as it is, pause and reason, while the job's intent is `IntentPause`,
whatever asked for the re-evaluation: the interval, a queue-wide resume or
the low-disk auto-resume (neither changes a job's intent), or a per-job
resume of a different job (`ReevaluateStalls` re-evaluates every record).
Only a per-job resume of the job itself (`mode=queue&name=resume` naming it)
sets its intent to `IntentRun`, and the re-evaluation that resume asks for
then lifts the record. Resuming such a job on the interval would have the
tick hydrate it again, re-reading every recorded row on the dispatcher's
goroutine until it met the same unreadable range, once per interval for as
long as the fault lasted (#828). Clearing the record of a job the user had
paused would leave it paused with no reason, and nothing would re-verify it.

If the resumed job's hydration faults before that re-evaluation runs, the
new stall finds the job's intent back at `IntentPause` and is kept, so a
resume costs one read-back pass. The re-evaluation reads the intent and then
clears the record in two steps, so a fault landing between them is cleared
with it, and the job is left paused with no reason.

**The surfaced reason says which kind a stall is (R27).** The record stores the
latest fault's text and kind together (`setStallReasonLocked`), and
`stallRecord.reason()` renders the surfaced text on every read, leading it with
`stallKind.reasonPrefix()`: `Stalled (resume to retry): ` for a
`stallVerify` record, which waiting does not lift, and `Stalled: ` for a
`stallRetryable` one, which the interval lifts. The queue listing's
`stall_reason` carries the text unchanged and the UI (`QueueRow.svelte`)
renders it as given, so the distinction has no field of its own
(`TestStallReason_SaysWhetherAResumeIsNeeded`,
`TestStallReason_FollowsTheLatestFaultsKind`).

**A re-evaluation only resumes a job THIS application parked.**
`stallRecord.parked` is set only by `Stall` and `parkVerifyFault`, which share
one body (`Application.stall`), and only when the job's intent was
not already `IntentPause` when `Stall` fired — a pause the user made first is
left in place, and the re-evaluation clears only the reason (a verification
fault's reason stays until the user resumes the job, above). The intent `Stall`
reads can be the user's pause, the application's own (a duplicate NZB or a
paused-priority ingest pauses the job on add), or `Stall`'s earlier pause, and
a later fault never releases a claim `Stall` already made. While `Stall`'s own
pause stands, a pause the user adds on top is indistinguishable from it, and
the re-evaluation resumes it.

Two races are accepted. A user pause landing between `Stall`'s intent read and
its `PauseInstance` is claimed by `Stall` and resumed once the fault clears. A user
resume landing in the same window leaves `Stall`'s `PauseInstance` pausing the job
with no owner: it stays paused after the fault clears, with no reason shown.
Closing the second needs an atomic prior-intent swap in the dispatcher.

The claim is in memory and the intent is persisted. A job `Stall` paused before
a restart comes back as a pause nobody owns.

**The hand-over to post-processing clears the job's stall record at once.**
`enqueuePostProc`, the door every hand-over goes through
(`git grep -n 'enqueuePostProc[(]' -- 'internal/*.go' ':!*_test.go'` returns
its definition and three callers: `finalizeRegistered`, the body of `Fail`,
`finalizeHopeless` and `maybeFinalizeJob`; `appRunner.runPostProc`; and
`fileOwedUnwantedFailure`), clears the record when it admits the job or defers it to its
Assessing worker (`handOverStall`). So the record that was there at the
hand-over does not go on reporting the job stalled, and no re-evaluation of
it resumes the job, or a retry later registered under its ID. **No record is
made after an admission either.** The record's one writer,
`noteStallUnlessHandedOff`, writes under the admission lock and refuses an
instance that is admitted or whose admission has ended
(`postProcAdmissions.unlessHandedOff`), so the clear, which follows the
admission, sees the final record. Where the refused fault goes is
§ "Hand-over to post-processing". An admitted job can be paused with no record,
by a refused fault's pause or by one that passed the gate just before the
admission and lands after the clear: the post-processor runs it paused or not,
and with no record nothing resumes the ID later. **A deferral is not an
admission**, so a fault after `handOverStall` defers the job to its Assessing
worker is recorded and pauses it, and a retryable one holds that worker until
the next re-evaluation resumes it. A deferred job `Stall` paused before the
deferral is resumed by the hand-over, since
the tick launches no Assessing worker for a paused job and the record that
would have resumed it is gone; a pause the user made is kept
(`TestFinalizeHopeless_ClearsTheStallOfTheJobItHandsOver`,
`TestFinalizeHopeless_OnAStalledJobAwaitingAssessing_ResumesItForAssessing`).
A job removed by `RemoveJob` keeps its record until the next re-evaluation
finds it gone and forgets it.

**A user Resume is outside the guarantee.** `mode=queue&name=resume` and
`name=resume_all` unpause the job and then ask for a re-evaluation, because a
user who has cleared the condition is entitled to have their job run. If it
has not cleared, the job runs until its next fault parks it again, and every
such fault is surfaced.

## Pre-allocation

Pre-allocation reduces per-write filesystem metadata overhead and fragmentation.
It is platform-specific:

| Platform | Mechanism | Failure behaviour |
|---|---|---|
| **Linux** | `fallocate(2)` — reserves contiguous extents without zeroing | skips pre-allocation on `EOPNOTSUPP` (NFS before v4.2 or without server `ALLOCATE` support, older FUSE) |
| **Non-Linux** | no-op | `WriteAt` at arbitrary offsets creates sparse holes on first write |

Calling `ftruncate` when `fallocate(2)` is unsupported reserves no physical
extents on sparse filesystems and adds a `Stat`+`Truncate` metadata round-trip
before the first write; `WriteAt` at an arbitrary offset already creates sparse
holes directly.

It uses `FileInfo.ExpectedSize`, the NZB's declared **encoded** byte count, which
runs ~2% above the file's decoded size. That difference is why a completed file
on a `fallocate`-capable filesystem must be trimmed: left in place it is trailing
zeros, which par2 reports as damage on a download that was perfectly healthy. The
trim bound is the end of the file's last owned range (§2); `openFile` records no
high-water mark or resume state of its own. A restart's finish by path trims
only a file still at exactly this size (§3 *What*).

Every first open in an open episode calls `preallocateFile` again, including a
reopen of an existing partial (the handle is `O_WRONLY|O_CREATE`, never
`O_TRUNC`), and a file can legitimately already be larger than `ExpectedSize` —
`offsetOutOfRange`'s slack allows a decoded write up to
`1 + 1/offsetSlackDivisor` of it. `fallocate(2)` with `mode=0` is grow-only in
the kernel and never shrinks a file that is already at least `size` bytes.
Pre-allocation reserves space ahead of writes, the completion trim removes the
encoded/decoded slack once writing is done, and neither performs the other's
mutation.

A fallocated region that was never written reads back as zeros, which fail the
CRC of any row that claims them: verification needs no size gate.

`SupportsSparse()` (`sparse.go`) probes whether the target filesystem supports
sparse files by creating a temporary file, truncating it to 1 MiB and checking
`st_blocks * 512 < apparent_size`. It is an **informational probe** used at
startup for logging; it does not gate pre-allocation.

## Write path

Every accepted article is written synchronously: `acceptArticle` range-checks
it and checks its owned range (§5), and `fileWriter.accept` calls `writeOne`,
which issues one `WriteAt` and, only if it returned without error, claims the
range. `handleSuccessArticle` then
resolves the article `Written` and `routeOutcome` calls `OnArticleWritten`
with the range and the decoder's CRC. There is no
assembler-side buffering of decoded articles, no coalescing and no
memory-pressure flush (`git grep -n 'writeOne(' -- internal ':!*_test.go'`
finds 2 lines: the definition and its one call, in `accept`).

The decoded bytes the assembler holds ahead of the disk are the queued requests
in the request channel (`reqs`, see *Memory & allocation budget*), the one
request the worker is processing, and one request per `WriteArticle` caller
blocked on the `reqs` send. No assembler field stores article bytes outside
`reqs`: `WriteRequest.Data` is the package's one `[]byte` struct field
(`git grep -n -E '^\s+\w+\s+\[\]byte\b' -- 'internal/assembler/*.go' ':!*_test.go'`
returns 1 line), and `reqs` is the one field typed to hold a `WriteRequest`
(`git grep -n 'chan WriteRequest' -- 'internal/assembler/*.go' ':!*_test.go'`
returns 2 lines: the field and its `make`). Buffers still upstream of
`WriteArticle`, in the downloader's decode path, are not counted here.

What bounds those bytes is the downloader's held-bytes budget
(`docs/nntp-downloader-contract.md` § "Held-bytes budget"), which charges an
article from before its fetch until the worker has finished with it: the
worker calls a request's `WriteRequest.Release` once `dispatchRequest` returns
for it, on every disposition; a request still queued when the worker exits
at shutdown is not released. `reqs` itself counts requests, so on its own it
would hold 2048 of them, whatever their size.

Decoder buffers are returned to `sync.Pool` (`decoder.PutBuffer`) on every path,
including every failure path.

## Duplicate and late-article handling

- **Per-writer `seenDone` / `seenFailed`**: dedup by `ArtIdx`, membership-only
  (R12). A duplicate of an accepted article is refused by `fileWriter.admit`;
  the caller releases its buffer and returns, since re-writing it would be a
  second `WriteAt` over the same range. Byte ranges are owned by
  `fileWriter.owned`, a different index for a different question: not "has
  this article been seen" but "who wrote this byte range" (§5).
- **One entry, one exit.** `admit` is the only way an article takes on a part,
  and `resolve(artIdx, disposition)` the only way its accounting settles, with
  one arm per disposition: `Written` (the admission stands), `Rejected` (the
  part kept, `seenDone` → `seenFailed`), `FetchFailed` (a part charged unless
  the article holds one, `seenFailed` set, never admitted), `RolledBack`
  (`seenDone` cleared, the part given back if the admission charged it).
  `acceptArticle` returns the disposition it decided as a value, so every
  return path names one; a zero disposition, or a `Written`, `Rejected` or
  `RolledBack` for an article not in `seenDone` (never admitted, or already
  resolved out), is refused by `resolve` with a panic (R28).
  `TestFileWriterAccounting_HasOneOwner` enumerates the functions that touch
  the two sets and the counter.
- A write that **fails** is resolved `RolledBack`: its article leaves
  `seenDone`, gives its part back and does **not** enter `seenFailed`: a
  storage fault says nothing about the article (A1), and recording it failed
  made its redelivery take the already-counted-as-failed path, leaving the
  file's part total permanently short. Its Emitted bit is cleared by
  `OnArticlesUnwritten`, not by the writer — `ForEachUnfinishedArticle` skips
  a set Emitted bit, so the route is what makes it Outstanding.
- An article already counted as permanently **failed** keeps its part through a
  rollback: the `FetchFailed` charged it, and a redelivery is admitted so its
  bytes are written without charging a second one — `admit` decides that from
  what the writer holds, not from anything the caller says.
- **Cross-state dedup**: an `ArtIdx` previously counted as a success arriving as
  a failure (or vice versa) does not increment `partsWritten` again, which is
  the identity `partsWritten == |seenDone ∪ seenFailed|`
  (`TestFileWriter_PartsIsTheSizeOfTheUnion`).
- **Late articles**: an article for a file already in the `completed` tombstone
  is handled by `handleLateDuplicate` — data returned to the pool, no disk
  write. The writer was closed at completion, so the article is reported
  rejected: `Job.MarkArticleFailed` is a no-op for an article already Done, and
  one the job holds no record of (its write failed and it was re-dispatched
  after the file finished) resolves permanently failed rather than staying
  Emitted forever.

## Control messages

Each is a `WriteRequest` with an unexported `ackCh` set and a sentinel `FileIdx`
(declared together in `internal/assembler/control.go`), and each is synchronous
from the caller's perspective: the caller blocks until the worker, which owns
every file handle, has done the work and answered.

| Control | Encoding | Worker behaviour |
|---|---|---|
| **CancelJob** | `fileIdxCancelJob` (-1), `MessageID=jobID`, `disposition` | closes all open files for the job and *deletes* them under `DeleteFiles` or leaves them on disk under `KeepFiles`; tombstones the job in `cancelledJobs` under **both** |
| **CloseJobHandles** | `fileIdxCloseHandles` (-2), `MessageID=jobID` | `Sync`s and `Close`s handles *without deleting*, untrusts a file whose fsync or close failed, tombstones the files and the job, **sends any close-time fault on `ackCh`**. Used when a job enters post-processing |
| **ForgetJob** | `fileIdxForgetJob` (-4), `MessageID=jobID` | drops the job's file and job tombstones so a retry under the same ID can write again; open handles are left alone |
| **Quiesce** | `fileIdxQuiesce` (-5) | answers once everything queued ahead of it has been processed |

The sentinel is the encoding, not the proof: `ackCh` is unexported, so no value
built outside `internal/assembler` is taken for a control message. One
goroutine owning every handle is invariant X1 — a mutex over the open-file map
would put `WriteAt` and `fsync` inside a critical section, which is what
`scripts/check_lock_io` exists to catch.

## Disk-space pre-flight

`checkDiskSpace` runs every 16 `WriteRequest` items (`diskCheckInterval`), and is
skipped entirely when `MinFreeBytes` is zero. It probes the directory of each
file still in the worker's open set. `finalizeFile` closes a completed file and
drops it from that set before the check, so a request that completes the last
open file checks nothing that turn. Two distinct timeouts bound it:

- **Caller timeout** (`diskCheckTimeout = 5s`) — each per-directory `FreeBytes`
  call bounds how long the worker blocks waiting for a result.
- **Cache TTL** (`DefaultDiskProbeTTL = 5s`) — `DiskProbe` caches a completed
  `statfs` for this long before launching a new probe. The two values match today
  but serve independent purposes.

**The assembler's probe reduces its cached reading by the assembler's own
writes.** A reading up to `DefaultDiskProbeTTL` old does not see what has been
written since, and at disk speed that is gigabytes (#987). `routeOutcome` calls
`DiskProbe.NoteWritten` with the length of every article it reports written, and
`FreeBytes` returns the reading less the bytes noted since its probe was
launched, floored at zero. The application's own `DiskProbe` (auto-resume,
`DownloadDirFreeBytes`) is never told of writes and reports the raw reading.

The estimate is not exact. Until the next probe it can be too low or too high.

Too low (can pause early, which the app's unadjusted auto-resume reading then
lifts):

- Every successful article write counts, so a file that `fallocate` already
  sized (`preallocate_linux.go`) is counted again as it fills, and an overwrite
  of bytes already on disk is counted as new.
- Bytes written while a `statfs` is in flight stay counted, though the reading
  may already include them.
- The total is not split by directory: job directories share a filesystem, so
  a write to one lowers every directory's estimate, and directories on
  different filesystems lower each other's too.

Too high:

- A write that faults is not counted, although part of it may have landed, and
  neither are writes by other processes or by par2 and unpack.
- A filesystem whose `statfs` ignores data still in the client's write-back
  cache (NFS) reports more than is really free, and bytes noted before the
  probe was launched are not subtracted a second time.

A fresh probe replaces the estimate with its own reading.

`NoteWritten` is a single atomic add (`DiskProbe.wrote`): it takes no lock and
performs no I/O.

`DiskProbe` keeps **at most one outstanding `statfs` goroutine per directory**:
repeated calls against a stuck mount return the cached result or the timeout
error rather than accumulating goroutines. Stale entries are evicted after 10
minutes (`diskProbeEvictAfter`).

While the estimate is below `MinFreeBytes` the `OnLowDisk` callback fires at every
check; `handleLowDisk` warns only on the first call after a resume. **The
assembler does not pause itself** — the callback owns that decision — and it
continues processing requests in the channel. The callback,
`Application.handleLowDisk`, applies the queue-wide pause and pauses the
downloader; `docs/job-lifecycle.md` § "10. Rendering, and the translation to
SABnzbd" has how it lifts.

## Offset bounds checking

`offsetOutOfRange` rejects a `WriteRequest` whose offset is negative, whose
`offset+length` overflows `int64`, or whose write extends past
`ExpectedSize + ExpectedSize/8` (12.5% slack). This prevents a hostile NNTP server
from inflating a file's apparent size with a crafted yEnc `=ypart begin=` header
(NN3). A rejected write returns its buffer to the pool and claims no range.

It is an ARTICLE fault, not a storage fault, so it resolves against the article
(A1): `OnArticleRejected` carries it to `Job.MarkArticleFailed`, which
charges its bytes to the job's failed-byte count and clears its `Emitted` bit
so nothing waits on a re-dispatch that will never come. It changes no fetch
policy: the Assessing verdict decides the par2 release. A rejection can land
after the dispatcher evicted the job; it is still recorded, as bits alone, and
the byte charge follows at the next hydration — see `docs/job-lifecycle.md` § "Residency: the three tiers".

The rejected article still **counts toward its file's part total**: it will
never arrive again, so a file that declines to count it can never reach
`TotalParts`, and the job sits at 100% with zero outstanding articles. Counting
it claims nothing — it was never written, so it owns no range and the
completion trim never reaches past it.

## DirectUnpack streaming contract

DirectUnpack is a **volume-level** streaming extractor, not an article-level one.
It reads fully assembled RAR volume files from disk; it never reads partial
articles or sparse regions.

1. **Volume completion signal**: `OnFileComplete` reports a volume only after
   the assembler has fsynced, trimmed and closed it (§2), so rarengine never sees
   pre-allocation's trailing zeros. `completeFinalizedFile` feeds the volume
   before it marks the file complete, and so before the download-finished
   report (`Dispatcher.AdvanceFrom`) from which the tick can launch the job's
   post-processing, whose `enqueuePostProc` collects the unpacker
   (`TestCompleteFinalizedFile_FeedsTheLastVolumeBeforeReportingTheDownload`). A feed
   after that collect finds the job admitted, and `maybeStart` starts no
   unpacker for an admitted job, so the collected one would wait for the
   volume (`TestHandOff_CompletionAfterTheCollect_StartsNoUnpacker`).
   Nor does `maybeStart` start one for a job instance `RemoveJob` has marked
   removed: `RemoveJob` aborts the job's unpacker once, after the mark, so one
   started by a feed landing after that abort would run until shutdown
   (`TestRemoveJob_CompletionAfterTheAbort_StartsNoUnpacker`).
2. **After a restart DirectUnpack is a best-effort accelerator.** Its state is
   in memory only, and a fresh unpacker starts only when it is fed volume 1.
   Files that were `complete=1` before the restart, and files the verifier
   finished (`FileComplete.Resumed`), are **not** fed to it; post-processing's
   normal unpack is the backstop.
3. **Volume waiting**: `waitForVolume()` blocks on `volumeReady` until the
   requested volume number appears in `completedVols`, and returns immediately if
   the set is in `corruptSets`, or, for any volume, once `Wait` or `Abort` has
   ended the input (item 8).
4. **Sequential volume feeding**: `startVolumeFeed()` opens completed volumes in
   order and sends their `*os.File` handles to `rarengine.NewReader(volumesChan)`.
5. **Corrupt volume handling**: `MarkCorrupt(setname, reason)` is called by
   `directUnpackOrchestrator.maybeStart`, ahead of that volume's `Add`, when
   the volume was assembled from a download with missing or failed articles.
   Once marked, the set can never be reported as successfully extracted —
   `waitForVolume` checks on each wake, and `extractSet` re-checks after
   extraction as a backstop for volumes that arrived before the corruption was
   detected.
6. **Non-RAR handling**: `extractSet` calls `rarheader.Version()` on the first
   volume's magic bytes. Any error — including I/O errors, not just format
   mismatches — yields `errNotRAR` and the set is recorded as `SkippedSet`, not
   failed, and left to the normal unpack stage. That stage extracts RAR3 only
   through an external `unrar` binary it finds; its pure-Go path is
   `rarengine` too, and fails a RAR3 set.
7. **Format support**: `rarengine` (pure Go RAR5). Other formats, legacy
   RAR2/RAR3, and non-RAR files identified by filename go to post-processing.
8. **Abort/kill**: `Abort()` sets `killed`, records each queued set as failed
   ("aborted (not started)"), signals the reader goroutine and waits for
   `run()` to exit. If `run()` was never started it closes `done` directly.
   Every outcome is written by `settleLocked`, and the current set's by `run()`
   once `extractSet` returns: a set that published is a success even when
   `Abort` landed meanwhile, since its members are in the job directory, and a
   set that did not is an "aborted" failure. Sets that finished before `Abort`
   keep their outcomes, so no set is reported both extracted and failed
   (`TestDirectUnpack_AbortKeepsASetPublishedBeforeIt`,
   `TestDirectUnpack_AbortBeforeTheVerdict`). A set enters the queue once:
   `Add` admits a set that is not queued, current or settled (`setAdmitted`),
   and `makeCurrentLocked` moves it from the queue to current, for the first
   set `Add` starts and each later one `run()` pops, so a set whose later
   volumes arrive before volume 1 is extracted once
   (`TestDirectUnpack_MultiVolume_OutOfOrder`). `enqueuePostProc`
   aborts the unpacker of a job handed to post-processing before its download
   finished (`Job.IsComplete` false: `Application.Fail`, a hopeless
   callback): the downloader no longer fetches an admitted job, so the
   abort stops its extraction at once. A job whose download finished has
   had every feed it will get, so its unpacker is awaited
   (`TestFail_FromFetching_AbortsTheDirectUnpack`,
   `TestHandOver_AfterTheDownloadFinished_KeepsTheDirectUnpackResults`).
   `enqueuePostProc` reads `Job.IsComplete` right after the admission,
   before `pipeline.forgetJob` drops the paths a feed resolves and before
   the collect: a file completing after that read leaves the job reading
   incomplete, so the last file of a download that finishes during the
   hand-off cannot leave the collected unpacker awaited without its volume
   (`TestHandOff_LastFileCompletingAtTheCollect_LeavesNoUnpackerAwaitedWithoutAVolume`).
   `run()` does not exit when its queue empties: it waits for another set
   until `Wait` or `Abort` ends the input, so a set whose volume 1 arrives
   after the previous set settled is still extracted
   (`TestDirectUnpack_SetAddedAfterTheQueueEmptiedIsExtracted`). `Wait`
   declares that every volume has been added, which the awaited unpacker of a
   finished download satisfies, and `Add` refuses a volume once `Wait` or
   `Abort` has ended the input, or the unpacker has finished
   (`TestDirectUnpack_AddAfterWaitIsRefused`,
   `TestDirectUnpack_AddWhileWaitIsExtractingIsRefused`,
   `TestDirectUnpack_AddAfterAStartedRunFinishedIsRefused`). End of input is
   final for every volume up to a set's highest listed volume
   (`totalVolumes`, read from the job's filenames): a set missing one of
   them, the first or a later one, when `Wait` ends the input is recorded as
   failed, "volume N of set S never arrived" (`volumeMissing`), and
   post-processing re-extracts it. A set whose final volumes are absent from
   the job's filenames is not known to be missing them: the feed ends after
   the highest listed volume, and the set fails with the RAR reader's error
   that the volume stream closed, which post-processing re-extracts the
   same way.
   `waitForVolume` returns that for a volume not yet added once the input has
   ended, and `Wait` on an unpacker whose `run()` never started settles its
   queue itself and closes `done`
   (`TestDirectUnpack_WaitSettlesALateSetWithoutVolume1`,
   `TestDirectUnpack_WaitSettlesSetsWhoseFirstVolumeNeverArrived`,
   `TestDirectUnpack_WaitSettlesASetMissingALaterVolume`). So a finished
   download whose unpacker was not fed a volume, whether the volume is absent
   from the NZB below the highest listed one or a feed was skipped (`maybeStart` returning before `Add`,
   or `completeFinalizedFile`'s `direct_unpack`/`enable_unrar` gate), does
   not hold the job at the hand-over
   (`TestHandOver_AVolumeNeverFedToTheDirectUnpack_DoesNotHoldTheJob`). When
   the volume feed fails, `extractSet` reports the feed's error rather than
   the reader's, which only sees the volume stream end.
9. **Path traversal safety**: `extractEntries` opens an `os.Root` anchored at
   the set's staging directory and writes every entry through it, so archive
   entries with `..` components, absolute paths, or symlinked path components
   cannot escape.
10. **A set is staged, and published whole or not at all, except for names it
    replaced under `overwrite_files`** (#855): `extractSet`
    extracts each set through `unpack.ExtractStaged` into a
    `.gonzbd-tmp-unpack-*` directory inside `extractDir` (the job directory).
    Only when extraction, the volume feed and the corrupt-set backstop all pass
    are the set's regular files renamed into `extractDir` by
    `publishStagedExtraction`; under `flat_unpack` without `overwrite_files` a
    name the job directory already holds is published under a `_N` name, as
    `ExtractMember` does inside the stage. A failed or aborted set has its stage
    removed and leaves nothing in the job directory, so the unpack stage's
    re-extraction of it meets no earlier copy
    (`TestDirectUnpack_FailedSetLeavesNothingInTheJobDir`,
    `TestDirectUnpack_AbortedSetLeavesNothingInTheJobDir`,
    `TestDirectUnpack_FailedSetReextractedFlatHasNoDuplicate`). A stage left
    by a crash is removed by `unpack.CleanupStageDirs`, which `sweepTempFiles`
    runs at the start of post-processing and `ExtractStaged` runs before
    making a stage. A publish that fails part way is undone by
    `publishStagedExtraction`: it removes the files it renamed into names
    that were free and the directories it created, and keeps a directory that existed before
    (`TestExtractStaged_APartialPublishLeavesNothing`); a removal that fails
    is returned in the error and leaves that entry in place. The undo
    removes only names that were free before the publish; under
    `overwrite_files` a name the publish replaced keeps the replacement, as
    without the undo, so the earlier content is lost at the rename and the
    name holds this failed set's member (it can be a file an earlier set of
    the same job published). Members that did reach the job directory are extracted again by
    the unpack stage whenever the set is missing from `DirectUnpackSets`, and
    under `flat_unpack` without `overwrite_files` a re-extraction, by any
    engine, writes `_N` copies of them. The case found: a restart before
    post-processing, which loses the in-memory result (item 2). A set that
    succeeded before `Abort` keeps its success (item 8).

## Memory & allocation budget

B2 bounds what is held for in-flight article data; the record's own buffers
are bounded by the flush cadence and by file size, as below.

| Component | Bound / strategy |
|---|---|
| Write channel (`reqs`) | 2048 requests (`defaultQueueSize`); backpressures the pipeline when disk I/O is slow. Its bytes are bounded by the downloader's held-bytes budget (256 MiB by default) together with everything else between the fetch and the write, not by its depth. |
| Recorder buffers | one row per article written since the last flush, one state per dirty file, and one owed delete per untrusted file whose delete has not landed. A flush that keeps failing re-merges its snapshot, so they grow while SQLite refuses writes. |
| `JobProgress.written` | one row per written or verified article of a file whose CRC is not yet settled; released at settle or untrust. |
| `internalFileComplete` | cap 128. A full channel moves a Resumed completion to its own goroutine (§3). |
| Decoder buffers | every `req.Data` returns to `decoder.PutBuffer` after write, error or discard, and `req.Release` gives its bytes back to the held-bytes budget. |
| Disk probe cache | one `probeState` per directory, evicted after 10 minutes; at most one outstanding `statfs` per directory. |
| Record rows | about one `written_articles` row per article, removed by the reclaim rule (§6). |

## Failure & degradation rules

- **Write error (`pwrite` failure)** — the article is *not* Done and *not*
  failed. Two things then have to happen, and they travel on **separate**
  callbacks:

  | Callback | Carries | Does |
  |---|---|---|
  | `Options.OnArticlesUnwritten` | the article the failure rolled back | clears its Emitted bit, returning it to Outstanding |
  | `Options.OnWriteFault` | the classified `*storagefault.Fault`, no article | stalls or fails the job on the usual R18 rule |

  A failed fsync takes neither: its articles were written, so they are `Done`,
  and the untrust of their file returns them (§4); its fault goes to
  `OnWriteFault` from `finalizeFile`, on the close-handles ack, or nowhere at
  worker exit.
  A restart also clears a stranded Emitted bit, by not persisting it
  (`JobProgress` is not serialized), and so does a downloader reload
  (`git grep -n 'j\.ClearEmittedForReload(' -- '*.go' ':!*_test.go'` finds 1
  line, in `internal/app/reloader.go`).
- **`FileInfo` resolution, `MkdirAll`, or `OpenFile` failure** — the article's
  data is returned to the pool, its Emitted bit is cleared through
  `OnArticlesUnwritten`, and the fault is routed through `OnWriteFault`. The
  file is never opened and never appears in `open`, so nothing later could
  surface it.
- **CancelJob** — closes open files and tombstones the job so subsequent
  articles are discarded. The `ackCh` synchronisation lets the caller delete the
  job directory the moment `CancelJob` returns. `KeepFiles` does **not** `Sync`:
  nothing reads a removed job's files, so the fsync would only stall ingest for
  every other job on the single worker goroutine. What `KeepFiles` leaves is a
  partial with no manifest or record left to interpret it.
- **Shutdown (`Stop`)** — closes `stopCh`, waits for in-flight senders, drains
  remaining channel items, then fsyncs and closes every open file, untrusting
  any whose fsync or close failed. Partial files are closed without firing
  `OnFileComplete`.

## Accepted limitations

These are known, deliberate, and **not** claims about correctness. They are
recorded here so the next reader does not mistake them for design.

1. **Verification runs inside the dispatcher's tick.** It also runs in `Start`,
   for a job owed an unwanted-extension filing (`fileOwedUnwantedFailures`), and
   in a rename's caller (`SetName`, bounded by `setNameLoadTimeout`, 10
   minutes); those are the three `Hydrate` callers (§3). A multi-GB resume holds
   back that tick's launches. It is a per-job cost paid at a restart, a
   retry or a resume, not a steady-state one: a job is verified at its first
   successful hydration after a restart (for a job restored paused, the
   user's resume or a rename), again on a retry, and once per
   user resume of the job while it is parked on a verification fault (§3),
   which no stall re-evaluation resumes by itself. If it
   matters, the follow-up is a `verified` gate in the dispatch plan with the
   read on the Fetching worker.

2. **An unreadable sector, or a missing download directory, keeps the job
   parked until the user resumes that job** (§3), with the fault as its
   `stall_reason`. A queue-wide resume, the low-disk auto-resume and a
   resume of another job leave it parked (*Stalls and their
   re-evaluation*); each resume of the job costs one read-back pass.
   Deleting the file refetches it; a
   directory the user deleted by hand also parks rather than refetching. A
   retry differs: it cannot tell an unmounted download root from a deleted
   job directory, and either way deletes the recorded rows and refetches
   (§3 *Retry*). A retry run while the share is down therefore refetches a job
   whose bytes are still on the share. An error other than `ENOENT` opening
   the job directory stays a fault on both paths.

3. **A file whose last article was written while its job was evicted is not
   marked complete in this process.** The CRC settle, the archive peek and
   the DirectUnpack feed that precede the mark need the manifest and do
   nothing for an evicted job, so `completeFinalizedFile` checks residency
   before the mark and reports the completion undelivered (`MarkFileComplete`
   itself needs only progress, so an eviction after that check still takes
   the mark); the file's
   rows are in the record, so the next start's verification finishes it by
   path. The window is reachable only through `Shutdown`, which evicts after
   the assembler has stopped, and `Remove`, after which no job is left to
   mark. A paused job keeps its manifest and the tick evicts only a job that
   holds nothing, so no running job is left unmarked in this process.

4. **A bogus article that arrives first wins its byte range** (§5), and its good
   neighbours are refused. A par2 post repairs them; a no-par2 post ends at
   `RepairNoCapacity`.

5. **A retried job's verifier-finished files are not re-peeked** (§3 *Retry*).
   They were peeked live in the failed attempt; post-unpack removal is the
   backstop if the rules changed in between.

6. **A remote NFS/SMB mount can stall untimed calls.** A `pwrite` or `fsync`
   that does not return blocks the assembler's single worker, and so every
   job's writes; a verification read blocks the tick; a rename's caller
   (`Dispatcher.SetName`'s `LoadProgress`, under `setNameLoadTimeout`, which
   bounds the wait but cannot interrupt the syscall); `Start`, through
   `Application.fileOwedUnwantedFailures`; and a removal
   (`mode=queue&name=delete`) waiting in `appResidency.Evict` for the
   hydration it cancelled: the hydration's read-back stops at its next row,
   but a read-back read blocked in the syscall holds the removal until that
   read returns, and whatever verdicts the hydration committed reach SQLite
   before the removal's reclaim deletes the rows; a manifest read or removal on the filesystem hosting `admin_dir`
   blocks its caller, except a hydration's manifest read, which its caller
   abandons when its context ends (`appResidency.readArticles`) and leaves
   to return on its own goroutine.
   The bounded waits are the callers' (`closeHandlesTimeout`,
   `untrustTimeout`, `recorderFlushTimeout`, the disk probe, Shutdown's step
   budgets), not the syscalls'. A stalled record write holds `wmu` until its
   own context ends, which for the five holders §1 lists without a deadline —
   the recorder's periodic flush, a hydration's commit (from the tick or the
   startup reconciliation; a rename's has `setNameLoadTimeout`),
   `verifyRetry`'s two applies and `retryHistoryJob`'s final commit — may be never; a waiter for
   `wmu` gives up when its own context ends (§1).

7. **The crash suite does not test fsync-to-platter.** See below.

8. **`admin_dir` must support hard links, and a crash can leave a staging
   file.** An NZB backup (`admin/nzb/<name>.gz`) is staged under
   `.nzb-*.staging` and hard-linked to its name, so that two ingests never
   share one (`writeNZBBackup`, `internal/app/app.go`). Where the link fails
   for any reason but an existing name (FAT/exFAT, some network or FUSE
   mounts), no backup is written, the job is not retryable, and AddJob logs a
   warning naming the requirement. It does not fall back to another publish
   path, which would put a partial file under a real backup name on a crash.
   A crash between staging and the link leaves the `.staging` file, which no
   sweep removes; the directory is not fsynced after the link. Manifests are
   written temp → fsync → rename (`fsutil.WriteGzAtomic`).

9. **A restart's finish by path keeps bytes past the last row of a file not
   at its preallocated size** (§3 *What*). Where those bytes are not the
   file's — a stale tail left by an earlier, longer write of the file's last
   article whose row a later, shorter write replaced, on a file preallocation
   did not size or that a write had already grown past it — they stay. The
   rows then cover all of the file's true bytes, so the CRC settled from them
   is the true file's. **Where the post has par2 the tail does not pass
   clean** (#837). `par2.IdentifyWithOptions` claims a name match
   (`claimName`: `MatchName`, `MatchFlattenedName`) whatever its length and
   marks one that differs from the packet's (`Identified.lengthDiffers`, where
   the packet records a length); `verifyIdentified` counts a marked file
   `Unverified` and never compares its CRC. The other passes (pass 0, its
   par2 path; pass 2, Hash16k; pass 3, keyed on `{CRC32, size}`) do not
   claim a file whose length differs from a recorded one. So the stale-tail file is identified
   as its entry, `Accounted()` holds, and `app.par2Verdict` reaches its
   final branch with `Unverified > 0`: `outcomeRepair`, which fetches the
   recovery volumes, and quickcheck marks the job damaged, so par2 runs and
   trims or repairs the file. A single-file post whose one file is short (a
   failed last article) takes the same branch; it is not
   `NothingIdentified`, which would hold the volumes instead. A
   post without par2 delivers the tail. The
   opposite choice cut par2-repaired files short (#829), and nothing recorded
   at a restart tells the two apart: the decoded size the poster declared is
   not persisted.

## What the crash suite actually pins

`test/crash/` (build tag `crash`, Linux only) runs the real daemon as a child
process and kills it. It is the strongest evidence in the repository for this
contract, and its scope is narrower than "durability":

**It pins the process boundary.** A SIGKILL destroys the process's in-memory
buffers for real, with no flush, so a row recorded ahead of its bytes would
have no bytes in the file afterwards, and the restarted daemon resolving it
would show in the CRC read-back.

**It does not pin fsync-to-platter.** No unprivileged userspace call can discard
dirty page-cache data: `POSIX_FADV_DONTNEED` invalidates clean pages and skips
dirty ones, `/proc/sys/vm/drop_caches` skips them too, and `O_DIRECT` flushes
first. Real coverage needs a device the test can cut underneath the filesystem —
a device-mapper `log-writes` or `flakey` target — which needs root.

`ENOSPC` is likewise not covered by the crash suite; the stall path is covered
in-process instead. Both gaps are tracked as issue **#363**.

`docs/TESTING.md` §3a is the full account, including the per-test table and what
a green run does and does not bound.

## Invariants

1. A Done bit after a restart exists only because a device read matched its
   row's CRC, or because its file was `complete=1` and not reset by a retry.
2. `complete=1` is written only after the file's fsync and trim, and never for
   a file whose fsync failed.
3. A file whose fsync failed has no rows and no Done bits in memory once its
   untrust has run, none in the recorder's buffer once the untrust purged it,
   and none in SQLite once a write carrying its delete succeeded — the
   untrust's own, or a later flush, which carries the owed delete ahead of
   the file's rows; until then the rows it leaves have `complete=0`, no write
   can set `complete=1` over them, and a restart reads each back before any
   is trusted (§4).
4. No two written articles of a file have intersecting byte ranges, and an
   article owns a range only once its write succeeded.
5. The whole-file CRC exists only for a gapless, non-overlapping chain of rows
   covering every article of the file with none failed.
6. One writer updates the record (the recorder, through `ApplyRecord`, the only
   `UPDATE` of `job_files`); `Admit` only inserts each file's row, with its
   structure and initial fetch policy; the reclaim rule deletes.
7. A storage fault never marks an article failed, nor degrades the health
   percentage or the failed-byte count (A1, R21; §7).
