# Download Durability & Storage Contract

This document is the contract for `internal/durability`, `internal/storagefault`,
`internal/assembler` and `internal/directunpack`, and for the code in
`internal/app` that records written articles (`record.go`) and verifies them
on restart (`verify.go`, `residency.go`): what it means for a downloaded article
to be *done*, when that claim may be made, what survives a crash, how a restart
re-derives its work set, and how a storage fault reaches the user.

`docs/ARCHITECTURE.md` places these packages in the download pipeline.
`docs/job-lifecycle.md` owns residency and the manifest/progress split, which
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
| **NN2** | Queue rows, manifests and NZB backups keep their formats and owners. |
| **NN3** | Hostile-input checks are kept: Message-ID validation, `offsetOutOfRange` with its `ExpectedSize/8` slack, path sanitisation. |
| **NN4** | Standing Design Rule 3: a bad article costs only its own bytes. |
| **NN5** | No `docs/sabnzbd_spec.md` §10 field changes. |

**Why the quickcheck shortcut is sound (NN1).** The repair stage skips par2
when quickcheck matches the CRC combined from a file's rows (*The whole-file
CRC*). That is sound because every row of a `complete=1` file describes bytes
written before the writing handle's fsync (§2); every row of a `complete=0`
file became `Done` after a restart only by a device read that matched its CRC
(§3); and a file whose fsync failed has no rows (§4). A change that marks a
file complete before its fsync, installs rows without the read, or keeps rows
past an fsync fault reopens the hole: par2 would be skipped over a file whose
bytes are not what the CRC describes.

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
- **`job_files`** — one row per file: `complete`, `filename` and
  `fetch_policy`.

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
| **Worker** | `Assembler.worker` goroutine | Owns the open-file map and every `FileWriter`. Routes requests, counts parts, checks disk space, finishes completed files. | Single goroutine (X1). No locks over file handles. |
| **Writer** | `assembler.FileWriter` (one per open file) | Owns one file's handle, its writes, its owned byte ranges, its completion finish and its close-time fsync. | Worker-owned; never touched from another goroutine. |
| **Recorder** | `app.recorder` (`internal/app/record.go`) | Buffers one row per written article and one state per dirty file, and writes them through `Store.ApplyRecord`. | `mu` over the buffers; `wmu` serialises its writes (§1). |
| **Verifier** | `verifyJobFiles` (`internal/app/verify.go`), called from `appResidency.Hydrate` and `Application.verifyRetry` | Reads every recorded article of an incomplete file back from the device before the job's content is attached. | Runs on the hydrating goroutine; touches no job and no row itself. |
| **Fault routing** | `internal/storagefault`, `Application.Stall` / `Fail` | Turns a storage error into a stalled or failed job with a reason a user can act on — never into a failed article. | — |
| **DirectUnpack** | `internal/directunpack` | Streams RAR extraction as whole volumes complete. Reads finished files, never partial article data. | Mutex over volume tracking and kill state; blocking `volumeReady` channel. |

## Mandatory invariants

### 1. One writer updates the record

`Store.ApplyRecord` is the only production writer of `written_articles` rows
and of `job_files`' `complete`, `filename` and `fetch_policy`, and the recorder
is its only caller: `git grep -n -E '\.ApplyRecord[(]' -- '*.go' ':!*_test.go'`
returns 2 lines, both in `internal/app/record.go` (`flushLocked` and `apply`).
`Store.Admit` is the one other writer of `job_files`, and it only inserts the
seed rows (§6).

The recorder has two ways in:

- **`noteWritten`**, the body of the assembler's `OnArticleWritten`, called on
  the worker once an article's `pwrite` returned nil. It appends the row to the
  pending buffer **first** and only then calls `Job.MarkArticleWritten`, so
  nothing persisted depends on the Done bit. `ErrNotResident` from it is
  expected for a job evicted mid-write and is logged at Debug.
- **`markDirty`**, called with a file's current `complete`, `filename` and
  `fetch_policy` read from the job's progress (`Application.markFileDirty`), so
  `complete` reads true only once the file was finished (§2).

And two ways out:

- **`flush`** takes **one** snapshot of the pending rows and dirty files under
  `mu`, so a file's `complete=1` cannot land without the rows noted before it,
  and writes it in one transaction with `mu` released. On a store error it
  merges the snapshot back before releasing `wmu`. It runs every
  `defaultRecordInterval` (5 s) from `recorder.run` — which is what bounds a
  crash's rework (B1) — and synchronously at three
  sites — `git grep -n -E 'recorder\.flush\(' -- '*.go' ':!*_test.go'` returns
  3 lines:
  - `Application.Shutdown`, after the assembler and the dispatcher have
    stopped;
  - `enqueuePostProc`, after `CloseJobHandles` and before post-processing can
    change the bytes the rows describe;
  - `persistAndCommit`, after `historyRepo.Add` (so a failed `Add` does not
    lose progress) and before `RemoveJob` and reclaim, on its own 2 s budget.
- **`apply`** commits verdicts, and optionally whole file states, for one job
  synchronously. It holds `wmu` from its purge through `ApplyRecord`, and the
  purge removes what is still buffered for the file it names — a `DeleteAll`
  verdict's pending rows, a `DeleteArtIdxs` verdict's named rows, the pending
  `complete` of a verdict that sets or clears it — so no flush can write back
  what the verdict removed. Its callers are verification, the retry, and the
  untrust of a file (§4): `git grep -n -E 'recorder\.apply\(' -- '*.go' ':!*_test.go'`
  returns 5 lines.

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

1. `FileWriter.finish` → `fsutil.ShrinkAndSync`: fsync; if the file is longer
   than `ownedRanges.maxEnd` — the end of the last byte range this writer
   claimed or was seeded with (§5) — and that end is positive, truncate to it;
   fsync again. It never grows a file (S6), and a file with no owned range is
   left at its preallocated size.
2. Close the handle.
3. Only if both succeeded: tombstone the file in `completed` and call
   `OnFileComplete`.

The handle is closed at completion; nothing keeps it open for a later step.
The one other place a file is finished is the verifier's `finishFileByPath`
(§3), which runs the same `ShrinkAndSync` over bytes it has just read back from
the device, and never over Done bits that came from a `pwrite`.

`OnFileComplete` reaches `Application.completeFinalizedFile`, which settles the
whole-file CRC, peeks the archive, feeds DirectUnpack, calls
`Job.MarkFileComplete`, marks the file dirty for the recorder, and reports the
download complete when it was the job's last file. `complete=1` therefore
reaches SQLite at the next flush; a crash before it costs one verification read
of the file at the next start, which finishes it by path. The hand-over to
post-processing does not wait for the background cadence: `enqueuePostProc`
flushes synchronously.

**A truncated failed tail is still charged.** Failed bytes come from manifest
article sizes (`JobProgress.markFailed` → `FileProgress.FailedBytes`), and
nothing on the verdict path reads a file's size on disk. A no-par2 post with a
failed tail reaches `RepairNoCapacity`; a post with par2 is repaired.

Any error from the finish or the close is a **completion fault** (§4).

### 3. A restart trusts only what it read back

**A Done bit after a restart exists only because a device read matched its
row's CRC, or because its file was `complete=1`** (and not reset by a retry).

#### When

Verification runs **before** the job's content is attached, and its outcome
decides whether attaching happens at all. `appResidency.Hydrate`, on the branch
where the job has no progress yet (`verifyAndAttach`):

1. reads the job's `job_files` and `written_articles` rows;
2. calls `verifyJobFiles`, which reads bytes and returns a verdict without
   touching the job or a row;
3. commits the verdict's deletes and `complete` changes through `recorder.apply`;
4. only then calls `Job.AttachContent` and installs what was verified
   (`installVerification`).

If any step fails or is cancelled, nothing is attached and the next hydration
starts again from step 1. So **a job `Hydrate` attached was verified first**,
and `Hydrate`'s early `RestoreContent` for a job with progress is sound: the
other ways a job gains progress verify too, or have nothing to verify — a
freshly ingested job has no rows, and a retry verifies its rebuilt job itself
(`verifyRetry`).

Every path that attaches content to a job with rows goes through this, or
verifies itself:

- the dispatcher's tick (`reconcileResidency`), for a job that holds what its
  position requires;
- `hydratePausedJobs`, inside `Start` before the first tick: a job restored at
  `Fetching` with `IntentPause` is hydrated, and so verified, so that
  `mode=queue` reports its progress rather than 0 %;
- `Dispatcher.SetName` → `LoadProgress`. A rename is refused once
  `Job.DownloadBegun()` is true and moves nothing on disk; verification runs
  first, so verified rows make the job "begun" and a rename can never leave
  rows pointing at a stale path;
- `Application.verifyRetry`, for a retried job: it is rebuilt by
  `BuildIngestJob`, which attaches its manifest, so `Hydrate` would return
  early and never verify it (see *Retry* below).

A paused job is not hydrated by the tick even while it holds a compute slot
(`reconcileResidency`'s `IntentPause` test). Without that, a job parked by a
verification fault would be re-read, and fault again, on every tick.

#### What

Every file with `complete=0` and at least one row is read, whatever the job's
state and the file's fetch policy: a crash between a file's completion and the
next flush can leave such a file in a job that has already left `Fetching`, so
no state makes the read unnecessary. For each such file, in order:

| Case | Outcome |
|---|---|
| empty `filename` | every row deleted: none can be located |
| `open` returns `ENOENT` **and the file's directory exists** | every row deleted; the file's articles are Outstanding |
| `open` returns `ENOENT` and the directory is missing, or `stat` of it fails | **verification fault** (below); on a retry a missing directory (`ENOENT` from the `stat`) deletes every row of the file, like the row above |
| any other `open` error | verification fault |
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
the user's explicit act, `restoreFailedDir` has already put back a directory it
could, and a directory still missing means the recorded bytes are gone. The
decision lives in one place, `readBackFile`'s `retry` parameter.

The fresh descriptor's fsync reports a writeback error **no earlier fsync has
reported** (Linux ≥ 4.16). On Linux the file's cache is then dropped
(`POSIX_FADV_DONTNEED`), so the reads come from the device rather than from
pages marked clean after a failed writeback; elsewhere `dropPageCache` is a
no-op. Rows are read in offset order through one reused 1 MiB buffer.

A file whose articles are then all resolved is **finished by path**: the
`fileFinishable` predicate (`FetchAlways`, not complete, a non-empty article
range, every article resolved) decides it, `finishFileByPath` runs
`ShrinkAndSync` with the end of the last verified row, and only then does the
verdict carry `SetComplete`. An fsync or truncate error while finishing is a
verification fault.

A `complete=1` file is not read. Its rows are installed as they stand
(`Job.InstallCompleteFile`): each row's article is Done, **every other article
of the file's range is failed** — the failed set is the complement of the rows
— the file is Complete, and its whole-file CRC is settled. The failed set is
installed during hydration, before anything can consult it (the archive peek,
the DirectUnpack feed).

A row that names an article outside its file's range, or has an invalid offset
or length, is dropped at install (`placeRows`) and costs its own article
(Standing Design Rule 3).

#### A verification fault parks the job; it never fails it

`verifyJobFiles` returns an `*errVerifyFault` naming the file or directory.
`Hydrate` classifies it through `storagefault.Classify`, calls `Application.Stall`
with it — whether the fault classifies retryable or permanent — and returns an
error wrapping `dispatch.ErrResidencyFault`, which `reconcileResidency` does not
settle. The job stays non-resident and paused with a reason naming the file.
Resuming it hydrates and verifies again. A sector that stays unreadable keeps
the job parked until the operator acts — deleting the file sends it down the
`ENOENT` arm and refetches it.

A context error is returned as it is, parks nothing, and settles nothing.

#### Files the verifier finished

`installVerification` handles each file whose verdict carries `SetComplete`
while the manifest is certainly attached: it settles the file's CRC, runs the
archive peek, and only then calls `MarkFileComplete` — the mark is what lets
the download-complete report take the job to Assessing, so a last file marked
first could reach post-processing unpeeked. The completion is then queued on
`internalFileComplete` as `FileComplete{Resumed: true}`
(`enqueueResumedCompletion`), and its consumer skips the settle, the peek, the
mark and the DirectUnpack feed, so it lands even if the job is evicted first.

The peek's failure message is a job-level fact: the first non-empty message of
a hydration is carried, as `FileComplete.FailMsg`, on every Resumed completion
of that hydration, so the job is filed with the flagged names whichever
completion arrives first.

When the channel is full, the send moves to its own goroutine that gives up
when `app.ctx` is cancelled. It cannot block the caller: `hydratePausedJobs`
runs inside `Dispatcher.StartWith`, before `watchCompletions` starts. A
completion it gives up is re-derived by the next start's verification.

#### Retry

`retryHistoryJob` verifies the rebuilt job before registering it
(`verifyRetry`):

1. **Shape check.** Every row must name an article inside its file's range of
   the re-parsed manifest; on a mismatch every row of the job is deleted,
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
   directory no longer exists takes the `ENOENT` arm instead of the
   verification fault (*What* above): its rows are deleted and it is refetched
   (`TestRetryHistoryJob_AfterDownloadDirDeleted`).

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
   `recorder.apply` (a `DeleteAll` + `ClearComplete` verdict), which also
   purges what is still buffered for it. This has to land before the job can be
   evicted and re-hydrated: hydration restores `complete` from `job_files` and
   would otherwise reinstate a stale `complete=1`. It is bounded by
   `untrustTimeout` (2 s), shorter than `closeHandlesTimeout` so a close that
   untrusts a file keeps time to report its fault.
2. `Job.UntrustFile` returns every Done article of the file to Outstanding,
   clears `Complete` and the CRC, and releases the resident rows. An article
   already failed stays failed: `markNotDone` refuses a failed article.
3. `pipeline.forgetFile` drops the cached `FileInfo`, so the first refetched
   article re-registers the file — recounting `TotalParts` and seeding owned
   ranges from the now-empty rows — and opens a fresh writer.

If the SQLite write fails, what stays there is rows flushed earlier with
`complete` still 0 (the purge already dropped the buffered ones, and
`complete=1` is written only after a successful fsync), and the next start
reads them back before trusting any.

The assembler untrusts a file at three points (`noteFileUntrusted` has three
callers in `internal/assembler/assembler.go`):

| Where | What failed | Also |
|---|---|---|
| `finalizeFile` | the completion finish or close (§2) | **no tombstone** — a tombstone would route every refetched article to `handleLateDuplicate`, which writes nothing once the handle is gone, so the refetch would never converge; the fault goes to `OnWriteFault` → Stall or Fail |
| the close-handles arm of `CloseJobHandles` | the close-time fsync or close | the fault is sent on the control message's ack (*Hand-over to post-processing*) |
| `drainAndCloseAll`, at worker exit | the close-time fsync or close | the fault is not routed: there is no caller left to answer |

**A failed close-time fsync also rolls back the articles it covered.** A
`FileWriter` keeps `unsynced`, the articles written since the last successful
`Sync`. A failed `Sync` moves every one into `poisoned` (`poisonSync`), and
`releasePoisoned` returns them to Outstanding through `OnArticlesUnwritten`. In
production `FileWriter.Sync` fails only inside `drainAndClose`, its one
production call (`git grep -n 'w\.Sync()' -- 'internal/assembler/*.go'
':!*_test.go'` returns 1 line), which closes the writer right after, so the
writer and its owned ranges are discarded with it. The file is then untrusted
as above: its rows and `complete` are removed, and its articles return to
Outstanding. A redelivery of any of them registers the file afresh and lands
on a new writer whose `owned` is seeded from `FileRows`, which the untrust
emptied.

These two fsyncs, and the completion finish, are what report errors first. So
no file whose error was already consumed reaches a restart with rows, and the
verifier's own fsync and cache drop are a second line of defence rather than
the only one.

### 5. Overlapping articles: first writer wins, by byte range

`FileWriter.owned` (`ownedRanges`, `internal/assembler/ranges.go`) is the
single owner of which article wrote which bytes of a file.

- **A range is claimed only after its write returned nil** (`writeOne`). An
  article whose write faulted owns nothing, so it cannot cause a later article
  to be refused.
- **An arrival whose `[off, off+len)` intersects a range another article owns
  is refused** (`acceptArticle`, through `ownerOf`) and failed permanently. Its
  part still counts toward `TotalParts`, and its bytes are charged to par2. A
  range owned by the same `ArtIdx` is a re-accept, not a collision.
- **A zero-length article claims nothing.** It is reported written with
  `n == 0`, and its row (length 0, CRC 0) is valid: the next verification
  verifies it, and `InstallCompleteFile` installs it, as for any row
  (`WrittenRow.HasValidShape` is the one shape rule). It is never refetched on
  that account.
- Intersection is detected, not only a shared start offset
  (`TestOverlap_PartialRangeOverwritesADurableArticle`,
  `TestOverlap_ContainedOverlapStillCompletesTheFile`).

**Within one open-file episode this is enforced by the writer. Across a
restart it is enforced by seeding.** `pipeline.registerFile` passes the file's
resident rows — those a restart verified, and any this process wrote since —
as `FileInfo.Owned`, and `openTargetFile` hands them to `seedOwned` before any
write. Seeded ranges are owned by `seededOwner`, whose
`artIdx` is -1 — `articleID.sameArticle` compares the index alone, so a zero
sentinel would wave article 0 through. An invalid seeded range is dropped with
a warning, which costs only a refetch of its articles.

The verifier keeps only disjoint rows (§3), so seeding never has to choose
between two articles.

**The one known NN4 exception:** when a bogus article arrives first, its good
neighbours are refused. A par2 post repairs them; a no-par2 post ends at
`RepairNoCapacity`, which is the honest NN1 outcome (#759).

### 6. The record's rows have one lifecycle rule

**Admission.** `seedJobFiles` → `Store.Admit` inserts every `job_files` row in
one transaction at submission, before `Dispatcher.Add`. It is a precondition
for the recorder: an `UPDATE` matching no row is not an error, so a file with
no seed row would silently persist nothing. A retry re-seeds, inserting only
missing rows.

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
| verification fault (§3) | at a hydration, always `Stall`, never `Fail`; on a retry, the retry is aborted | nothing attached |

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
it.

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
`NoCRC`), then releases the file's resident rows. Its callers are
`completeFinalizedFile` and, for files a restart finished or found
`complete=1`, `installVerification` and `InstallCompleteFile`.

The per-article rows a file needs for it are kept resident in
`JobProgress.written` until the CRC is settled — installed by
`InstallVerified` / `InstallCompleteFile` and appended by
`MarkArticleWritten` — so nothing on the worker path reads SQLite. A stored
slice is never edited in place, so a `Progress()` clone shares them.

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
| Write fault | The part rolls back (`FileWriter.fail`), `OnArticlesUnwritten` clears the article's Emitted bit, `OnWriteFault` → Stall or Fail. No row is written. |
| Completion fault | §4: untrusted, no tombstone, `FileInfo` dropped, Stall or Fail. On resume the file's articles are Outstanding and are refetched. |
| Pause, low disk, server penalty | Nothing writes the record; no hook. |
| Reload (`ReloadDownloader`) | Stop the old downloader; `setCompletions(nil)`, which returns once every buffered result has reached the assembler's queue; `Assembler.Quiesce`, answered once everything queued ahead of it has been processed and its callbacks have run; `ClearEmittedForReload(false)` for every job not admitted to post-processing; start the new downloader. An Emitted bit still set after the quiesce covers only an article whose bytes never reached `pwrite`. If the quiesce fails, the clear is skipped and those articles stay Emitted until the next start. |
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
so `Fail` cannot hand it over again and `Stall` would pause a job whose files
post-processing is using.

**A job instance whose handles `CloseJobHandles` closed is not downloaded
again.** The job-level tombstone drops any article still in flight, and the
downloader skips an admitted job (`downloader.Options.HandedOff`). A retry
under the same ID is a new instance; `RetryHistoryJob`'s `ForgetJob` clears the
assembler's tombstones for it, and a retry whose `ForgetJob` fails is aborted.

## Clean shutdown

`Application.Shutdown` stops the downloader, then the assembler, then cancels
the context, waits for its goroutines, stops post-processing and the
dispatcher, and **then** flushes the recorder.

`Assembler.Stop` drains the request channel and then runs `drainAndCloseAll`:
each open file is fsynced and closed, and one whose fsync or close failed is
untrusted (§4). Partial files are closed without firing `OnFileComplete`. The
flush comes after the assembler has stopped, so it carries the row of every
write the drain processed.

**No job is parked during shutdown.** `Application.stopping` is set at the top
of `Shutdown`, before any of its steps, and `Application.Stall` refuses to
pause a job while it is set, or once `app.ctx` is cancelled. The pause would be
the one that cannot be undone: it is persisted, the stall list that would
re-evaluate it is in memory and dies with the process, and nothing at startup
resumes it. The next start verifies the job's files regardless.

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

`Application.Stall` records a reason, then pauses the job (R19, R27). It
releases only a `Fetching` job's lease; a job that has moved on keeps its
worker, and the pause gates its next move. `reevaluateStall` runs on an
interval (`stallRecheckInterval`, 30 s) and on user action
(`ReevaluateStalls`, from the API's resume handlers). It retries nothing: a
write or completion fault left the affected articles Outstanding, and a
verification fault left the job non-resident, so a resumed job refetches and
re-verifies through the ordinary paths. If the condition has not cleared, the
next fault parks the job again.

**A re-evaluation only resumes a job THIS application parked.**
`stallRecord.parked` is set only by `Stall`, and only when the job's intent was
not already `IntentPause` when `Stall` fired — a pause the user made first is
left in place, and the re-evaluation clears only the reason. The intent `Stall`
reads can be the user's pause, the application's own (a duplicate NZB or a
paused-priority ingest pauses the job on add), or `Stall`'s earlier pause, and
a later fault never releases a claim `Stall` already made. While `Stall`'s own
pause stands, a pause the user adds on top is indistinguishable from it, and
the re-evaluation resumes it.

Two races are accepted. A user pause landing between `Stall`'s intent read and
its `PauseJob` is claimed by `Stall` and resumed once the fault clears. A user
resume landing in the same window leaves `Stall`'s `PauseJob` pausing the job
with no owner: it stays paused after the fault clears, with no reason shown.
Closing the second needs an atomic prior-intent swap in the dispatcher.

The claim is in memory and the intent is persisted. A job `Stall` paused before
a restart comes back as a pause nobody owns.

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
high-water mark or resume state of its own.

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
it and checks its owned range (§5), and `FileWriter.Accept` calls `writeOne`,
which issues one `WriteAt` and, only if it returned without error, claims the
range and notes the article in `unsynced`. `acceptArticle` then calls
`OnArticleWritten` with the range and the decoder's CRC. There is no
assembler-side buffering of decoded articles, no coalescing and no
memory-pressure flush (`git grep -n 'writeOne(' -- internal ':!*_test.go'`
finds 2 lines: the definition and its one call, in `Accept`).

The only memory the assembler holds ahead of the disk is the request channel
(`reqs`, see *Memory & allocation budget*).

Decoder buffers are returned to `sync.Pool` (`decoder.PutBuffer`) on every path,
including every failure path.

## Duplicate and late-article handling

- **Per-writer `seenDone` / `seenFailed`**: dedup by `ArtIdx`, membership-only
  (R12). A duplicate of an accepted article releases its buffer and returns;
  re-writing it would be a second `WriteAt` over the same range. Byte ranges
  are owned by `FileWriter.owned`, a different index for a different question:
  not "has this article been seen" but "who wrote this byte range" (§5).
- A write that **fails** moves its article out of `seenDone`, gives its part
  back (`rollbackPart`, which `fail` calls) and does **not** put it in
  `seenFailed`: a storage fault says nothing about the article (A1), and
  recording it failed made its redelivery take the already-counted-as-failed
  branch, leaving the file's part total permanently short. Its Emitted bit is
  cleared by `OnArticlesUnwritten`, not by the writer — `ForEachUnfinishedArticle`
  skips a set Emitted bit, so the route is what makes it Outstanding.
- An article already counted as permanently **failed** keeps its part through a
  rollback: `admitPermanentFailure` charged it, and a redelivery writes bytes
  without charging a second one (`admitRetryOfFailed`).
- **Cross-state dedup**: an `ArtIdx` previously counted as a success arriving as
  a failure (or vice versa) does not increment `partsWritten` again.
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
skipped entirely when `MinFreeBytes` is zero. Two distinct timeouts bound it:

- **Caller timeout** (`diskCheckTimeout = 5s`) — each per-directory `FreeBytes`
  call bounds how long the worker blocks waiting for a result.
- **Cache TTL** (`DefaultDiskProbeTTL = 5s`) — `DiskProbe` caches a completed
  `statfs` for this long before launching a new probe. The two values match today
  but serve independent purposes.

`DiskProbe` keeps **at most one outstanding `statfs` goroutine per directory**:
repeated calls against a stuck mount return the cached result or the timeout
error rather than accumulating goroutines. Stale entries are evicted after 10
minutes (`diskProbeEvictAfter`).

When free space drops below `MinFreeBytes` the `OnLowDisk` callback fires. **The
assembler does not pause itself** — the callback owns that decision — and it
continues processing requests in the channel.

## Offset bounds checking

`offsetOutOfRange` rejects a `WriteRequest` whose offset is negative, whose
`offset+length` overflows `int64`, or whose write extends past
`ExpectedSize + ExpectedSize/8` (12.5% slack). This prevents a hostile NNTP server
from inflating a file's apparent size with a crafted yEnc `=ypart begin=` header
(NN3). A rejected write returns its buffer to the pool and claims no range.

It is an ARTICLE fault, not a storage fault, so it resolves against the article
(A1): `OnArticleRejected` carries it to `Job.MarkArticleFailed`, which
charges its bytes to the job's failed-byte count, releases on-demand par2, and
clears its `Emitted` bit so nothing waits on a re-dispatch that will never come.
A rejection can land after the dispatcher evicted the job; it is still
recorded, as bits alone, and the byte charge follows at the next hydration
while the par2 release does not — see `docs/job-lifecycle.md` § "Residency: the three tiers".

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
   the assembler has fsynced, trimmed and closed it (§2), so unrar never sees
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
   the set is in `corruptSets`.
4. **Sequential volume feeding**: `startVolumeFeed()` opens completed volumes in
   order and sends their `*os.File` handles to `rarengine.NewReader(volumesChan)`.
5. **Corrupt volume handling**: `MarkCorrupt(setname, reason)` is called by the
   queue when a volume was assembled from a download with missing or failed
   articles. Once marked, the set can never be reported as successfully
   extracted — `waitForVolume` checks on each wake, and `extractSet` re-checks
   after extraction as a backstop for volumes that arrived before the corruption
   was detected.
6. **Non-RAR handling**: `extractSet` calls `rarheader.Version()` on the first
   volume's magic bytes. Any error — including I/O errors, not just format
   mismatches — yields `errNotRAR` and the set is recorded as `SkippedSet`, not
   failed; the normal unpack stage's external `unrar` handles it.
7. **Format support**: `rarengine` (pure Go RAR5). Other formats, legacy
   RAR2/RAR3, and non-RAR files identified by filename go to post-processing.
8. **Abort/kill**: `Abort()` sets `killed`, records failures for the current and
   queued sets, clears success results, and signals the reader goroutine. If
   `run()` was never started it closes `done` directly. `enqueuePostProc`
   aborts the unpacker of a job handed to post-processing before its download
   finished (`Job.IsComplete` false: `Application.Fail`, a hopeless
   callback), because the downloader no longer fetches an admitted job and
   `waitForVolume` would wait for its missing volumes forever. A job whose
   download finished has every volume, so its unpacker is awaited
   (`TestFail_FromFetching_AbortsTheDirectUnpack`,
   `TestHandOver_AfterTheDownloadFinished_KeepsTheDirectUnpackResults`).
   `enqueuePostProc` reads `Job.IsComplete` right after the admission,
   before `pipeline.forgetJob` drops the paths a feed resolves and before
   the collect: a file completing after that read leaves the job reading
   incomplete, so the last file of a download that finishes during the
   hand-off cannot leave the collected unpacker awaited without its volume
   (`TestHandOff_LastFileCompletingAtTheCollect_LeavesNoUnpackerAwaitedWithoutAVolume`).
9. **Path traversal safety**: `extractEntries` opens an `os.Root` anchored at
   `extractDir` and writes every entry through it, so archive entries with `..`
   components, absolute paths, or symlinked path components cannot escape.

## Memory & allocation budget

B2 bounds what is held for in-flight article data; the record's own buffers
are bounded by the flush cadence and by file size, as below.

| Component | Bound / strategy |
|---|---|
| Write channel (`reqs`) | 2048 requests (`defaultQueueSize`). At 128 KiB articles, ~256 MB worst-case buffered; backpressures the downloader when disk I/O is slow. |
| `FileWriter.unsynced` | one `int32` per article written to the file in its open episode: only a successful `Sync` clears it, and the only `Sync` is at close. |
| Recorder buffers | one row per article written since the last flush, and one state per dirty file. A flush that keeps failing re-merges its snapshot, so they grow while SQLite refuses writes. |
| `JobProgress.written` | one row per written or verified article of a file whose CRC is not yet settled; released at settle or untrust. |
| `internalFileComplete` | cap 128. A full channel moves a Resumed completion to its own goroutine (§3). |
| Decoder buffers | every `req.Data` returns to `decoder.PutBuffer` after write, error or discard. |
| Disk probe cache | one `probeState` per directory, evicted after 10 minutes; at most one outstanding `statfs` per directory. |
| Record rows | about one `written_articles` row per article, removed by the reclaim rule (§6). |

## Failure & degradation rules

- **Write error (`pwrite` failure)** — the article is *not* Done and *not*
  failed. Two things then have to happen, and they travel on **separate**
  callbacks:

  | Callback | Carries | Does |
  |---|---|---|
  | `Options.OnArticlesUnwritten` | every article the failure rolled back | clears their Emitted bits, returning them to Outstanding |
  | `Options.OnWriteFault` | the classified `*storagefault.Fault`, no article | stalls or fails the job on the usual R18 rule |

  They are separate because they are needed in different combinations: a fault
  raised inside `Accept` needs both, for one article; a failed close-time fsync
  rolls back a set of articles, and its fault goes on the close-handles ack, or
  nowhere at worker exit.
  A restart also clears a stranded Emitted bit, by not persisting it
  (`jobProgressJSON` has no `emitted` field), and so does a downloader reload
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

1. **Verification runs inside the dispatcher's tick.** A multi-GB resume holds
   back that tick's launches. It is a per-job startup cost — a job is verified
   at its first successful hydration after a restart, and again on a retry —
   not a steady-state one. If it
   matters, the follow-up is a `verified` gate in the dispatch plan with the
   read on the Fetching worker.

2. **An unreadable sector, or a missing download directory, keeps the job
   parked until the operator acts** (§3). Deleting the file refetches it; a
   directory the user deleted by hand also parks rather than refetching. A
   retry differs: a missing download directory there deletes the recorded
   rows and refetches (§3 *Retry*).

3. **A file whose last article was written while its job was evicted is not
   marked complete in this process.** `MarkFileComplete` needs the manifest,
   so `completeFinalizedFile` reports the completion undelivered; the file's
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
   job's writes; a verification read blocks the tick; a manifest read or
   removal on the filesystem hosting `admin_dir` blocks its caller. The
   bounded waits are the callers' (`closeHandlesTimeout`, `untrustTimeout`,
   the disk probe, Shutdown's step budgets), not the syscalls'.

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
3. A file whose fsync failed has no rows and no Done bits in memory or in the
   recorder's buffer once its untrust has run, and none in SQLite once the
   untrust's `apply` succeeded; rows a failed `apply` leaves have `complete=0`
   and are read back before any is trusted (§4).
4. No two written articles of a file have intersecting byte ranges, and an
   article owns a range only once its write succeeded.
5. The whole-file CRC exists only for a gapless, non-overlapping chain of rows
   covering every article of the file with none failed.
6. One writer updates the record (the recorder, through `ApplyRecord`); `Admit`
   only inserts the seed; the reclaim rule deletes.
