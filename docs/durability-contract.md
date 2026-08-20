# Download Durability & Storage Contract

This document is the contract for `internal/durability`, `internal/storagefault`,
`internal/assembler` and `internal/directunpack`: what it means for a downloaded
article to be *done*, when that claim may be made, what survives a crash, how a
restart re-derives its work set, and how a storage fault reaches the user.

It replaces `docs/assembler-storage-contract.md`, which described a model in
which the write path acked articles, combined per-article CRCs into a whole-file
CRC, and truncated a completed file to this run's high-water mark. None of that
is true any more.

`docs/ARCHITECTURE.md` places these packages in the download pipeline.
`docs/queue-lifecycle.md` owns residency and the manifest/progress split, which
this contract depends on and does not restate.

**This states the contract in the present tense.** Where the code and this
document disagree, the code is wrong and the gap is a bug, not a documentation
error. Known gaps are named in *Accepted limitations* and *Open gaps* at the
end, rather than left for a reader to discover.

## Upgrading from a pre-durability build

**A job that was mid-download when you upgraded re-downloads from scratch,
once.** This is the design's own answer under S3, not an oversight, and nothing
softer preserves the invariant.

Such a job has `job_files.articles_done` bits from the old build, but no
`article_facts` rows and no `file_extents`. The startup sweep therefore has no
recorded region for any of its articles, cannot verify a single byte, and
returns every article to Outstanding. The partial files on disk are overwritten
in place by the re-download.

The alternative — "there is no evidence, so trust the column" — is
indistinguishable at runtime from a lost or truncated fact log, which is
precisely the case #362 exists to catch: a partial file that was truncated or
deleted out of band finishing as a complete file with a zero-filled hole in it,
silently. There is no signal that separates "this job predates the fact log"
from "this job's fact log is gone", so any rule that trusts the column in the
first case also trusts it in the second.

Completed jobs, history, configuration and the queue's ordering are unaffected.
Only in-progress downloads pay, and only once.

## Why this exists

Nine open issues (#306, #311, #337, #344, #349, #353, #355, #356, #357) and five
of the eight merged fixes before them (#305, #315, #341, #343, #350) were two
defects wearing different clothes — see
`docs/superpowers/specs/2026-08-11-download-durability-design.md` for the full
derivation. The dominant one is **a fact recorded before the thing it asserts
becomes true**: an article marked `Done` while its bytes sat in a memory buffer
(#355, refiled as #356), a truncate bound describing one process's writes applied
to a file built by several (#342, #350), a CRC over a subrange reported as the
CRC of a file (#349).

The costs are asymmetric, and that asymmetry is the whole design:

- **Over-fetching is a cost.** An article re-downloaded needlessly wastes
  bandwidth and time. It is bounded, visible and recoverable.
- **Over-claiming is a defect.** `ForEachUnfinishedArticle` skips any article
  whose `done` bit is set, and `ResetForRetry` clears `done` only where `failed`
  is also set — so a `Done` published for bytes that never landed is permanent.
  No later run re-dispatches that article, and the file stays short. Without
  par2 it is silent.

So every rule below resolves ambiguity toward re-fetching, and the one claim
that can over-claim — "this article is on disk" — is made in exactly one place
and is enforced by the compiler rather than by review.

## The two classes of fact

Everything persisted about download progress is either Class A or Class B. Which
class a fact belongs to determines whether it needs a barrier.

| | **Class A — `durability.ArticleFact`** | **Class B — `durability.FileExtent`** |
|---|---|---|
| Content | `article → {FileIdx, Offset, Length, CRC32}` | durable bitmap, `VerifiedTo`, `PrefixCRC`, `BytesDurable`, `Size`, `ModTimeNs` |
| Asserts | *If* the bytes at `[Offset, Offset+Length)` are present, they hash to `CRC32`. **Nothing about presence.** | Those bytes **are** present on stable storage. |
| True from | the moment the article is decoded — and forever after | only after a completed `fsync` |
| Ordering vs. the write | **none** (R2). May be committed before, during or after the write, or when the write never happens at all | strictly after the `fsync` (S1) |
| Barrier required | no | yes during a download — `durability.Barrier` is the only writer once articles are arriving. `durability.Resumer` also writes, at startup only, when a recomputation disproves the stored record (see §Resume write-back). |
| Authoritative | yes (S5) | **never**. Where it disagrees with a recomputation, the recomputation is correct by definition (S4) |
| Losing a suffix costs | a re-fetch (R3), and a skipped truncate if the file completes first (§4) | a recomputation |
| Stored in | `article_facts` | `file_extents` |

Two consequences worth stating explicitly, because both have been got wrong:

- **Class A is appended at decode time and is deliberately not ordered against
  the write** (`pipeline.appendArticleFacts`). Adding an ordering there would
  destroy the property that makes Class A cheap. The append runs on a
  `context.WithoutCancel` copy of the caller's context, bounded by
  `factAppendTimeout` (5s), because a shutdown that cancels the pipeline while
  an article is still being applied can still let the write land — and a fact
  lost to that race makes a resume unable to prove bytes that are on disk.
- **`FileExtent` has no failed-byte field, and cannot.** A permanently failed
  article never decodes, so it never writes an `ArticleFact`, so no
  recomputation from Class A could reproduce such a figure — it would be the one
  field S4 could not be applied to. It is cached in `job_files.failed_bytes`
  instead, beside the `articles_done` bits it sums. `internal/history/migrations/001_initial.sql`
  records the same reasoning at the schema.

## The state of an article

```
                  decoded                writeAt returned nil        fsync returned nil
  Outstanding ───────────────► Decoded ──────────────────────► Written ──────────────────► Durable
       ▲                          │                               │                          │
       │                          │ appends a Class A fact        │                          │ Barrier mints a
       │                          │ (no barrier, no ordering)     │                          │ DurableProof
       │                          ▼                               │                          ▼
       │                    article_facts ──────────┐             │                    Queue.AckDurable
       │                                            │             │                          │
       └────────────────────────────────────────────┼─────────────┘                          ▼
          a write failure, a storage fault, or a    │                                  ARTICLE IS DONE
          restart that cannot prove the bytes       │                                        ▲
          returns the article to Outstanding        │                                        │
          — never Failed                            │  ON RESTART, the SECOND way in:        │
                                                    │  Resumer re-reads the file and checks  │
                                                    └─►the bytes against the recorded CRC; ──┘
                                                       Queue.ReplaceFromResume then resolves
                                                       it — no barrier, no proof, no fsync by
                                                       this process. Evidence is stable
                                                       storage. See §1.
```

There are therefore **two** ways an article becomes Done, and only the first
goes through a barrier. The resume path is not a loophole — it is the only way
to credit bytes an earlier process wrote, which no proof can express — but any
statement of the form "X is the only thing that resolves an article" is false
unless it says *during a download*.

`Decoded`, `Written` and `Durable` are three different things and the design
turns on not conflating them:

- **Decoded** is what the downloader produced. It says nothing about disk.
- **Written** is `FileWriter.noteWritten` — the bytes came back from `WriteAt`
  without an error. It is the *only* evidence the barrier has, and it is not
  durability: the page cache can still lose it.
- **Durable** is what a completed `Sync` covers, and only the barrier can say
  so.

`Emitted` is the fourth, transient state and is not persisted at all; see
`docs/nntp-downloader-contract.md` §5.

## The tiers

| Tier | Component | Responsibility | Synchronization |
|---|---|---|---|
| **Ingest** | `Assembler.WriteArticle` / `CancelJob` / `CloseJobHandles` | Enqueue `WriteRequest` items into a bounded channel (`reqs`, cap 2048). Control messages for cancel and close-handles. | Channel send with `select` on `stopCh` and `ctx.Done()`. `wg.Add(1)` tracks every in-flight sender so `Stop()` drains cleanly. |
| **Worker** | `Assembler.worker` goroutine | Owns the open-file map, the shared write cache, and every `FileWriter`. Routes requests, counts parts, checks disk space, performs barrier operations. | Single goroutine (X1). No locks over file handles. |
| **Writer** | `assembler.FileWriter` (one per open file) | Owns one file's handle, its share of the write cache, its coalescing, its pre-allocation. Reports `Written`. | Worker-owned; never touched from another goroutine. |
| **Barrier** | `durability.Barrier` | The only place `Written → Durable → Resolved` happens **during a download**. Drains, fsyncs, commits Class B, mints the proof. Not the only place an article becomes Done: the Resume tier below resolves articles too, with no barrier and no proof — see the state diagram above and §1. | Holds no lock of its own; the cadence owner serialises it per job (`Application.jobBarrierLock`). |
| **Cadence** | `Application.runCheckpoint`, `noteJobBytes` | *When* a barrier runs. Time bound, byte bound, file completion, clean shutdown. | One goroutine; per-job mutex around each barrier. |
| **Resume** | `durability.Resumer`, `Application.resumeAllJobs` | What is actually on disk, at startup, from stable storage alone — and, through `Queue.ReplaceFromResume`, the second path by which an article becomes Done. Authoritative over the files it is passed: it *clears* bits it cannot verify. | Per-file, shares no state between calls. |
| **Fault routing** | `internal/storagefault`, `Application.Stall` / `Fail` | Turns a storage error into a stalled or failed job with a reason a user can act on — never into a failed article. | — |
| **DirectUnpack** | `internal/directunpack` | Streams RAR extraction as whole volumes complete. Reads assembled files, never partial article data. | Mutex over volume tracking and kill state; blocking `volumeReady` channel. |

## Mandatory invariants

### 1. One barrier, one proof, one ack

`Queue.AckDurable` takes a `durability.DurableProof`. `DurableProof` has no
exported fields and no exported constructor, so **no package outside
`internal/durability` can create a proof that names any article**. "Ack only
after fsync" is therefore not a rule six call sites must each remember; it is a
signature no outside caller can satisfy with a non-empty payload.

State the bound precisely, because an earlier version of this section did not.
Go permits a composite literal with no field values even when every field is
unexported, so `durability.DurableProof{}` compiles in any package — this was
checked by compiling it, not reasoned about. Such a proof is necessarily empty,
and `Queue.AckDurable` returns `nil` without touching an article when
`Articles()` is empty. The compiler bounds the **payload**; a one-line early
return makes an empty payload inert. That early return is therefore part of the
invariant, and
`TestAckDurable_ExternallyConstructibleEmptyProofAcksNothing` pins it.

Inside `internal/durability` the guarantee is package-scoped: `newProof` is
reachable from anywhere in the package, and exactly two functions call it —
`Barrier.Run` and `Barrier.FinalizeFile`. That pair is what review has to hold.

**This gate covers one door.** `Queue.SeedFromExtents` and
`Queue.ReplaceFromResume` also reach `markDone`, take a fully exported
`durability.FileExtent` whose `Bitmap` is exported and settable, and are
callable from any package with no barrier and no proof. That is deliberate —
their evidence is stable storage re-read at startup, which is exactly the kind
of evidence a proof cannot represent — but it means "ack before fsync is code
that does not compile" is true of `AckDurable` and **false as a statement about
the queue as a whole**. The seeding doors are held by their contracts and by
`TestSeedFromExtents_StaysAdditive` /
`TestSeedFromCommittedExtents_DoesNotClearAnAckThisProcessMade`, not by the
compiler.

This replaces a design in which the assembler could ack from six places, each
independently responsible for knowing that acceptance into a buffer is not
evidence about disk. That is why the same defect kept being refiled.

### 2. The barrier's order is the invariant

`Barrier.Run` performs, for one job:

```
  phase 1: Drain every open file          — no claim of any kind yet
  phase 2: Sync  every open file          — only now may anything be claimed
  phase 3: build one FileExtent per file  — derived, not yet persisted
  phase 4: ExtentStore.Commit (atomic)    — then, and only then, AckDurable
```

Every file is synced before any file's extent is built, so a barrier that fails
on the second file's sync has claimed nothing about the first either. Nothing may
be inserted between the commit and the ack: the commit is what makes the proof
true after a crash.

**A failed barrier claims nothing** (R7). It acks no article and leaves the
previously committed cache wholly intact, because `ExtentStore.Commit` is atomic
and is the last thing that can fail before the ack.

### 3. A drain is at-least-once, and a report survives a failed sync

`SyncTarget.Drain` may re-report an article a previous `Drain` already returned,
and the barrier's apply absorbs the duplicate (R12) — `ext.Durable.Set` is
idempotent and `BytesDurable` is charged only on a 0→1 transition.

`FileWriter` keeps two slices to make that true: `written` (reported by no
`Drain` yet) and `reported` (handed to a `Drain`, not yet confirmed by a
`Sync`). **Only `Confirm` discards `reported`**, and the barrier calls it solely
once the extent is committed and the articles are acked. A barrier that drains
and then fails — at the sync, the extent commit, the ack, or the truncate —
re-reports on the next attempt.

Releasing on the `Sync` would cover only the first of those. The fsync makes the
bytes durable, but the commit and the ack still follow it, and a failure between
them left the retry with nothing to re-report while the bytes sat on disk
unacked — the file could then never complete for the life of the handle, because
a redelivery is dropped as a duplicate.

This is load-bearing rather than tidy. For a file still being written, losing a
report costs a re-fetch. For a **completed** file it costs bytes: the retry
drains nothing, so the durable extent `FinalizeFile` trims to sits below bytes
that are genuinely on disk, and the truncate destroys them.

The split between the two slices is what keeps an article written *between* a
`Drain` and its `Sync` from being discarded by that `Sync`: it is still in
`written`, which `Sync` does not touch.

### 4. The truncate bound is derived from Class A, and only ever shrinks

`Barrier.FinalizeFile` trims a completed file to **the highest end offset among
its durable articles** — `max(Offset+Length)` over the facts whose durable bit is
set. Three quantities are easy to confuse here and the distinction is
load-bearing:

| Quantity | What it is | Why it is not the bound |
|---|---|---|
| this run's high-water mark | the highest byte *this process* wrote | on a resumed file it sits below what earlier runs wrote; truncating to it discards them (#342, #350) |
| `FileExtent.VerifiedTo` | the **gapless prefix** from byte 0 | stalls at the first permanently failed article; a 40 GB file with a hole at 2 GB would be cut to 2 GB, destroying exactly the blocks par2 repairs from |
| the durable extent | `max(Offset+Length)` over durable facts | **this is the bound** |

Deriving it from Class A rather than storing it means there is no second copy to
drift (S5), and it is correct by construction: every fact counted has an article
behind it whose bytes a completed fsync covered.

`FileWriter.Truncate` refuses a bound above the file on disk rather than clamping
(S6). Growing appends zeros, which asserts content that exists nowhere, and a job
with no par2 has no repair stage to notice.

**Two exceptions, and both are the same hazard from opposite sides.** The
durable set and the fact log can disagree in either direction, and each
direction makes a different bound unsafe.

*A recorded article that is not durable.* If a completed file's recorded facts
are not *all* durable, `FinalizeFile` trims to the **recorded** extent instead.
In the healthy case the two sets coincide — every article of a completed file is
either durable or permanently failed, and a failed article wrote no fact. A gap
means this is a retry of a finalize whose earlier attempt consumed the writer's
drain report without committing the bits it earned. Those bytes are on disk, the
durable bound sits below them, and trimming to it would destroy them silently.

*A durable article the fact log does not name.* Reachable because the Class A
append is independent of the write (R2): `pipeline.appendArticleFacts` logs its
error and lets the write proceed, so an article can reach disk, be drained, be
fsynced and earn a truthful durable bit while having no fact. **Both** bounds
above are computed by walking facts, so both walk past its bytes; when it holds
the file's top offset they land below its end, and `buildExtent` has already
added it to the ack set, so the truncate destroys bytes that are simultaneously
marked Done. `FinalizeFile` counts these and **declines to truncate at all** —
no bound derived from an incomplete record can be trusted, and since S6 only
ever shrinks, declining to shrink is always safe.

In both cases trailing zeros par2 reports as damage is a visible, repairable
cost; downloaded bytes gone is not.

### 5. A storage fault never marks an article failed

This is A1, and it is a hard rule. `ENOSPC`, `EIO`, `EROFS` and a wedged mount
are conditions of *storage*. They say nothing about any article's availability on
any server.

`internal/storagefault.Classify(op, path, err)` produces a `*Fault` carrying the
operation, the path, and whether the condition is `Permanent`.
`Barrier.routeFault` dispatches it:

| Classification | Route | Job outcome | Articles |
|---|---|---|---|
| retryable | `Stallable.Stall` → `Application.Stall` | paused, with a surfaced reason naming the file (R27); re-evaluated on an interval and on user action (R19) | stay **Outstanding** |
| permanent | `Stallable.Fail` → `Application.Fail` | stopped, reason carried into history (R20) | stay **Outstanding** |

In neither case is `Queue.AckPermanentFailure` called, the failed-byte count
touched, or the job's reported health degraded (R21). Attributing a full disk to
the article would burn its retry budget over something a user often fixes in ten
seconds.

`routeFault` also **returns** the fault as its error, marked with
`durability.ErrFaultRouted`, and `Application.routeFinalizeFailure` reads that
marker as proof it was already dispatched.

The marker replaced an inference from the error's *shape* — "the chain contains
a `*storagefault.Fault`" — which held only while `routeFault` was the one thing
that let a fault escape the barrier. It is not: the `SyncTarget` boundary mints
its own fault when the worker does not answer, and `filewriter.go` and
`assembler.go` both mint faults via `Classify`. One of those read as
"already handled" was silently swallowed, and the job carried on with a
completed file that was never trimmed. A new fault site inside
`internal/durability` still routes through `routeFault`; one that does not is
now visibly unrouted rather than indistinguishable from a routed one.

The one thing that must **not** go through `Stallable` is a bookkeeping defect —
an article with no file-local ordinal, a target reporting zero articles. Those
fail loudly as ordinary errors (A2, R28). Routing them through the fault path
would blame storage for a numbering bug, which is the A1 conflation in reverse.

### 6. Class B is a cache, and a recomputation wins

`FileExtent` carries `Size` and `ModTimeNs` as an S7 validity stamp. On resume,
`durability.Resumer` compares them against the file as it exists now:

- **Both match** → the cache is adopted without reading a byte. This is the fast
  path, and correctness never depends on it being right.
- **Either differs** → fall through to recomputation, which reads each recorded
  region and checks it against the CRC the fact log recorded at decode time.

Both halves of the stamp are load-bearing: a truncation moves the size, an
in-place edit of the same length moves only the mtime.

`HasPrefixCRC` is **re-checked** against the file's current size rather than
adopted, because the flag can outlive its condition: a resume commits it true for
a whole file, an article then lands beyond a hole so the file grows while
`VerifiedTo` does not, and the barrier clears the flag only when `VerifiedTo`
*changes*. Adopting a stale one would report a partial extent's CRC as the file's
(#349).

`Bitmap` widths are the other place the cache can over-claim. `ExtentStore`
rebuilds each bitmap at its full **byte** width — `Load` and `LoadFile` alike,
since neither stores the bit count — which is always a multiple of 64, so
`Bitmap`'s tail-word mask never fires and padding bits in a damaged blob would
survive into `Count()`. Every consumer that knows the file's true article count
therefore re-derives through `BitmapFromBytes` rather than adopting what the
store returned — `Barrier.priorExtent`, `Resumer.committedExtent` and
`queue.fileDurableBitmap` each do this, and each documents it. A stored bitmap *narrower* than the article count is zero-padded,
which reads as "not durable yet": the safe direction under S3.

### 7. Absence of evidence is absence

S3. An article a restart cannot prove is Outstanding. A region outside the file
as it exists now, a CRC that does not match, a missing file, a missing fact — all
resolve the same way, and none of them is an error.

This is why the startup sweep is **authoritative** rather than additive, which is
the fix for #362. `Store.RestoreJobProgress` marks done every article in
`job_files.articles_done` before any of this runs, and that column is a *belief*
a previous process wrote. `durability.Resumer` answers the same question from the
file's bytes, and S4 makes its answer correct by definition. With only an additive
entry point the belief always won, so a truncated or deleted partial finished as a
complete file with a zero-filled hole in it and no warning.

There are consequently two seeding entry points on `Queue`, and **they must not
be merged**:

| Entry point | Caller | Contract |
|---|---|---|
| `ReplaceFromResume` | `Application.resumeAllJobs` (startup sweep) | **authoritative over the files it is passed** — sets *and clears* for those, and leaves a file absent from the slice entirely alone. The only caller that has just read the files' bytes. |
| `SeedFromExtents` | `Application.reevaluateStall` phase 3 | **additive** — only ever sets. Replaying an ack whose fsync already landed; it has verified nothing. |

The union of the two contracts is either #362 (a stale bit outliving the
recomputation that disproved it) or a stall recovery that throws away live acks.
`TestSeedFromExtents_StaysAdditive` and
`TestSeedFromCommittedExtents_DoesNotClearAnAckThisProcessMade` are the guards,
and they are the only tests in the repository that redden when the two are
merged.

`ReplaceFromResume` never clears a permanently failed article: its bytes were
never on disk, so their absence is the recorded outcome and not new information.
It clears a file's `Complete` flag and its `AssembledCRC32` only where a bit was
actually cleared — `Complete` means "the assembler is finished with this file",
not "every article arrived", so it cannot be re-derived from the article bits.

### 8. A checkpoint is bounded by the open-file set, not by job size

R8. `SyncTarget.Files()` returns the job's **currently open** files, and
`Assembler.OpenJobIDs` returns the jobs holding any. A barrier fsyncs open files,
not every file the job will eventually produce. The set comes from the assembler
rather than from job status because "has an open file" is the assembler's fact,
and deriving it from the queue would be a second representation free to drift
(S5).

### 9. Barrier work is serialised per job

`Barrier.Run` holds no lock — it does I/O throughout, and the project bans I/O
under a lock — so `Application.jobBarrierLock` guarantees at most one barrier in
flight per job. Two concurrent barriers over one job would interleave the
read-modify-write of its `FileExtent`: both load the same stored bitmap, each adds
its own bits, and the second commit overwrites the first, so the committed cache
describes neither run and the articles the loser acked are durable with no bit to
say so.

The lock is **per job**, not global: a barrier is a few dozen fsyncs, and one
job's slow mount must not park every other job's checkpoint. `FinalizeFile` takes
it too — it is a barrier by another name, same `buildExtent`, same
`ExtentStore.Commit`.

### 9a. Only storage conditions reach `Stallable` — the `SyncTarget` boundary rule

`storagefault.Classify` defaults everything it does not recognise to
*retryable*, so any non-storage error reaching it comes back as a storage fault
and parks a healthy job naming a disk that did not fail. The rule that prevents
it is a **boundary** rule, not a list of call sites: a `SyncTarget`
implementation returns either a `*storagefault.Fault`, or an error wrapping one
of two sentinels.

| Sentinel | Meaning | Barrier's response |
|---|---|---|
| `durability.ErrFileNotOpen` | the file was closed between the barrier listing it and calling on it | drop that file from the run, surface nothing |
| `durability.ErrTargetUnavailable` | the operation never ran, for a reason that is not about storage — a stopped assembler, a caller that stopped waiting | abandon the run, surface nothing |

`Barrier.raise` is the single place that applies it, and every fault site in
`barrier.go` goes through it. Six sites were getting this wrong independently,
which is why the rule sits on the interface rather than at each of them.

**A timeout splits, and getting the split wrong is what parked healthy jobs.**
The implementation's *own* bound expiring — the worker did not answer within
`barrierOpTimeout` — *is* evidence about storage: the worker is parked in a
syscall against a mount that is not answering, and R19 requires that to be
surfaced. The *caller's* deadline expiring is not: the caller chose to stop
waiting, and the clean-shutdown checkpoint always does. `jobSyncTarget.submit`
converts the first into a fault and wraps the second in
`ErrTargetUnavailable`.

Dropping a file drops it from **every** collection the run holds, not only from
its drain reports. `Barrier.Run` releases each surviving file's report with
`Confirm` at the end, so a file left in the set after its report was discarded
has that report released — with nothing committed and nothing acked, destroying
the re-report R12 relies on.

It is never a fault. Files leave the open set for three deliberate reasons — a
completed finalize closing its handle, a cancelled job, a job entering
post-processing — and every one of them drains and syncs first, so there is
nothing left to checkpoint when they succeed. A close-time drain CAN fail, and
then `Close` discards the writer's retained report: the file leaves the open set
with articles written but never acked, and nothing can checkpoint them. That is
reported on the ack now rather than swallowed, but it is a hole in the
"nothing left" claim, not covered by it. The race is structural rather than exotic:
`finalizeCompletedFile` releases the per-job barrier mutex before its deferred
`CloseFile`, so a checkpoint can hold the lock, take the file from `Files()`,
and have the close processed before its own `Drain`.

Classifying it as storage parks a healthy job with a reason naming a device
that did not fail and an operator action that does not exist — the A1
conflation running in reverse.

### 10. Every barrier syscall on the critical path is timeout-bounded

B4/R22. **Every** operation submitted to the worker carries `barrierOpTimeout`
(5s, matching `diskCheckTimeout`) on the *wait* for the worker's reply, applied
by `jobSyncTarget.submit` itself. It is imposed by `internal/assembler` rather
than by the caller, because a wedged worker cannot answer whatever deadline it
was given — and it sits in `submit` rather than in each method because
per-caller wrapping is how `OpenJobIDs` came to have none at all.

`Drain`, `Sync` and `Truncate` also take the caller's context, which bounds
them further where it is shorter. `Application.checkpointAll` gives each job its
own, sized to the checkpoint cadence on the periodic path and to a share of
`shutdownCheckpointTimeout` on the shutdown path.

The bound is **per job**, not per sweep. A sweep-wide budget would let one
wedged mount consume the time of every job behind it, turning a single bad
mount into a queue-wide outage by a different route.

What that bounds is the wait, not the syscall: Go cannot interrupt a blocked
`fstat`, so the worker stays stuck either way. That is the intended division —
**a wedged mount stalls the job, never the process.**

`SyncTarget.Path` deliberately does **not** go through the worker. It is called
from the fault-routing path, and a wedged worker is precisely the condition that
gets it called, so asking the worker would be asking the thing that is stuck.
It reads `Options.FileInfo` and returns `""` rather than an error when it cannot
resolve. Nothing may branch on its value; it is diagnostic only.

The timeout handler does not call it either, for the same reason one step
further out: `Options.FileInfo` reaches the queue, which can hydrate a manifest
from disk, so resolving a path there would block the bound on the condition it
is reporting. A fault minted by `submit` therefore carries an empty path, and
`Barrier.raise` fills it in — the barrier already has it, and it is not the
thing that is stuck.

## The checkpoint cadence

*What* a checkpoint means lives in `durability.Barrier`. *When* one happens lives
in `internal/app`, so that "when to checkpoint" stays a policy question.

R6 names five triggers. Four are implemented; the fifth is listed so its
absence is visible rather than assumed:

| Trigger | Implementation | Bound |
|---|---|---|
| Time | `runCheckpoint`'s ticker → `checkpointAll` | `downloads.checkpoint_interval`, default **30s** (`constants.DefaultCheckpointInterval`) |
| Volume | `noteJobBytes` → `barrierKick` → `checkpointJob` | `downloads.checkpoint_bytes`, default **64 MiB** (`constants.DefaultCheckpointBytes`) |
| File completion | `Application.handleFileComplete` → `finalizeCompletedFile` → `Barrier.FinalizeFile` | per file |
| Clean shutdown | `Application.shutdownCheckpoint` → `checkpointAllShare` | `shutdownCheckpointTimeout` (10s) for the **whole sweep**, divided evenly among the jobs it visits |
| Pause | **not implemented as a trigger.** No code path runs a barrier on pause; a paused job simply stops writing, and its buffered bytes wait for the next interval tick or for shutdown. R6 names it and nothing satisfies it. | — |

The two bounds answer different failure shapes and neither subsumes the other.
The time bound is what limits rework on a slow link, where 30 seconds is a few
articles; the byte bound is what limits it on a fast one, where 30 seconds can be
a gigabyte. The barrier fires on whichever arrives first.

**Neither can be disabled.** `checkpointSettings` substitutes the default for a
zero or negative value. A barrier is the only thing that acks a downloaded
article *while the job is running*, so with checkpoints off a job makes no
visible progress and holds every article Outstanding until it stops.

It does **not** follow that the work is re-fetched, and an earlier version of
this paragraph claimed it did. `Resume` falls through to `recompute` when no
committed extent exists, and `recompute` re-derives the done-set from Class A
facts and the bytes on disk. What is lost is the point of Class B — a restart
must then re-read every partial file in full — plus whatever the write cache had
not flushed when the process died, which has no fact-and-bytes pair to recover
from. "Off" trades a bounded fsync cadence for an unbounded startup read and
real re-fetch.

Three details that have each been got wrong once:

- **The byte accumulator resets before the run, not after.** An article written
  while the barrier is in flight belongs to the *next* window.
- **A dropped kick is not a lost kick.** `barrierKick` is a non-blocking send;
  the accumulator is not reset by `noteJobBytes`, so the next article re-raises
  it and the interval tick covers the job regardless.
- **`lastBarrier` stamps only a barrier that returned nil, and only one that
  actually ran.** "The barrier ran and failed" and "no barrier ran at all" are
  different facts. Folding the second into the first's nil-error case is how a
  job on a dead mount came to report a fresh stamp every 30 seconds — the exact
  inversion of what R26 asks that figure to distinguish. When `checkpointJob`
  finds no sync target it also declines to reset the accumulator, because
  zeroing it would report zero pending bytes beside a stale timestamp, two
  figures agreeing that nothing is at risk at the moment when everything is.

**`bytes_durable` and `bytes_pending` are not in the same unit**, and R26 asks
only that the rework window be *visible*, not that it be commensurable with the
durable total. `bytes_durable` comes from the job's progress —
`expected - failed - remaining` over NZB-declared, yEnc-**encoded** sizes, the
same unit as `size`/`sizeleft` beside it. `bytes_pending` accumulates
`len(data)` per accepted article: **decoded** bytes, the ones on disk, because
B1's volume bound measures rework at risk. Neither can move to the other's
unit. Reading `bytes_durable` from `file_extents.bytes_durable` — a decoded
figure carrying the same name — is the substitution `docs/queue-lifecycle.md`
records as having overstated every non-resident job's remaining bytes; and
re-basing the accumulator on declared sizes would corrupt the cadence trigger
it exists to drive. The API contract already forbids summing them; the unit
difference is a second, independent reason, and it also rules out a ratio or a
difference.

The queue save follows the barrier rather than running on its own timer, because
the barrier is what produces something worth saving: an ack marks articles done
in memory, and until the queue is written a crash re-fetches them anyway.

`shutdownCheckpoint` runs **after the downloader has stopped and before the
assembler does** — the only window where no new article can arrive and the file
handles the barrier needs still exist. Without it, everything downloaded since
the last barrier is re-fetched on the next start: up to a full checkpoint window
thrown away on every deliberate restart, which is the cost B1 bounds for a crash
and nobody should pay for a clean stop.

Its budget is **divided**, not repeated. Passing `shutdownCheckpointTimeout` as
both the sweep's context and each job's budget looks per-job and is not:
`context.WithTimeout` cannot exceed its parent, so a first job consuming most of
the 10s leaves every job behind it with an already-expired context and an
immediate failure — paying exactly the re-fetch cost the paragraph above says
nobody should. The periodic sweep keeps a *fixed* per-job budget instead,
because it has no overall deadline to divide and one job's slow mount must not
shrink every other job's budget on every tick.

**No job is parked by this checkpoint.** `Application.stopping` is set at the
top of `Shutdown`, before any of its steps, and `Application.Stall` refuses to
pause a job while it is set. The pause would be the one that cannot be undone:
Shutdown's final `queue.Save` persists it, the stall list that would re-evaluate
it is in-memory and dies with the process, and the startup sweep skips the job
because its phase is no longer active — so a healthy job comes back Paused
forever after a slow but perfectly normal stop. The guard used to test
`app.ctx.Err()`, which `app.cancel()` sets two steps *later*, so it was inert on
exactly this path.

## File completion and the handoff

The assembler **no longer closes a file when its last part arrives**. It
tombstones the file, fires `OnFileComplete`, and leaves the handle open, because
`Barrier.FinalizeFile` has to `Drain`, `Sync`, `Truncate` and `Stat` it through
that handle. A file closed at completion can never be trimmed back to its decoded
extent.

The sequence is:

```
  worker: partsWritten == TotalParts
        └─► tombstone in `completed`, OnFileComplete  (handle still OPEN)
              └─► Application.handleFileComplete
                    ├─ filePathFor            (resolved BEFORE the finalize — a
                    │                          permanently faulted job drops its
                    │                          cached FileInfo, so a path asked
                    │                          for afterwards comes back empty)
                    ├─ finalizeCompletedFile
                    │     ├─ Barrier.FinalizeFile   (drain, sync, extent, trim,
                    │     │                          re-sync, re-stat, commit, ack)
                    │     └─ Assembler.CloseFile    (ONLY on success)
                    └─ completeFinalizedFile
                          ├─ Queue.MarkFileComplete
                          └─ DirectUnpack handoff
```

**`CloseFile` now answers, and the answer is logged rather than acted on.** Its
`opClose` arm used to leave the reply error `nil`, so a close whose `Drain`,
`Sync` or `Close` had failed reported success and the file was marked complete
and fed to DirectUnpack and post-processing with bytes that were not all on
disk. It reports the failure now — preferring a permanent errno over the first
one, so an `ENOSPC` drain followed by an `EROFS` close is not described as a
condition that waiting can clear.

Both callers still log rather than act on it, but the reason is narrower than
"post-hoc" and the first draft of this paragraph overstated it. On the path the
argument describes — a finalize that ran to completion — the barrier has
drained, synced, truncated, committed the extent and acked the articles, so
acting on the redundant second fsync's fault would race the completion it is
part of, and on a permanent errno would carry a 100%-complete, fully acked job
into history as failed.

That is **not** every entry path. `finalizeCompletedFile`'s defer also runs
after `app.barrier == nil`, after a nil sync target, and after the
assembler-stopped and not-in-`open` early returns; and `retryFinalize` reaches
it on a job whose extent was committed but never acked. On all of those the
close-time `Drain` is the file's FIRST flush and the fault is not post-hoc at
all. `Warn` is the floor there, not `Debug`, and the completion should not
proceed past it — see #374. The close-time fault is
also **not** routed to `Stallable` from inside the assembler — it carries no
`ErrFaultRouted` marker, so routing it would park the job a second time for a
condition the barrier had already routed, and on the `CloseJobHandles` path it
would arrive at `StatusVerifying`, which neither `Stall` nor `Fail` can act on.

**A failed finalize stops the completion.** The file is not marked complete,
DirectUnpack is not fed it, and the job does not finalize — because none of those
can be undone once done, while a stalled job can be resumed by an operator who
has fixed the mount. `ErrNotFinalized` exists so the caller can tell "there was
nothing to finalize" from "we could not find out whether there was anything to
finalize"; the second must never proceed, or a `barrierOpTimeout` on a wedged
mount ships a file with pre-allocation's trailing zeros intact and par2 reports a
healthy download as damaged.

**The handle is retained on the failing path.** `Application.reevaluateStall`
retries the finalize on an interval and on user resume, and every operation it
needs goes through that handle; nothing reopens a file the assembler has
tombstoned. Closing it there would leave the stall unable to clear for the rest of
the process.

### The retained-fd bound, and the boundary it holds within

The retained set is **cumulative**, not the concurrently-open set: one fd per
completed-but-unfinalized file. Its ceiling is the files that had already
completed, or were already queued on `internalFileComplete` (cap 128), when the
fault hit.

**That bound, and the claim that a job is never unpaused while a finalize is
failing, hold while the job is parked.** `reevaluateStall` does not resume a job
until every interrupted finalize has landed, so the automatic cadence cannot grow
the set.

**A re-evaluation only resumes a job THIS application parked.** A stall record
exists for reasons that involve no pause of ours: a *user* pause evicts the job,
the next checkpoint's `AckDurable` fails with `ErrJobNotResident`, and
`noteNeedsSeed` creates one. Resuming on that undid the user's pause within one
interval with no log saying so — and it could not settle, because handles stay
open through a pause (`CloseJobHandles` runs only from `maybeFinalize`), so the
next checkpoint failed the same way and recreated the record as fast as it was
cleared. `stallRecord.parked` is set only by the paths that pause the job
themselves.

**A user Resume is the boundary, and is deliberately outside the guarantee.**
`mode=queue&name=resume` and `name=resume_all` (`internal/api/queue.go`) unpause
the job and *then* ask for a re-evaluation, because a user who has cleared the
condition is entitled to have their job run. If it has not cleared, the job
downloads until the next re-evaluation parks it again, completing more files and
retaining a handle for each — bounded by one re-evaluation interval's worth of
downloading per Resume, and not silent: every one of those files raises its own
routed fault.

`CancelJob`, `CloseJobHandles` and the worker's own shutdown drain all still
release the handles, and post-processing's unlink cannot become an NFS
silly-rename because a parked job does not reach post-processing.

## Restart

`Application.resumeAllJobs` runs **once, synchronously, inside `Start`** — after
`queue.Load` and **before the downloader can dispatch**. The ordering is the whole
point: a seed that lands after dispatch has begun still marks the right articles
done, but the request for them is already on the wire.

For each job it sweeps, per file:

1. Resolve the path from the filename the queue already recorded
   (`pipeline.jobFilePath`, same `JoinSafe` sanitisation the writer used). A file
   whose filename was never resolved is **skipped**, contributing no extent —
   no process ever opened a path for it, so there is nothing to have proved
   absent.
2. `durability.Resumer.Resume` — stat, adopt-or-recompute, per §6 above.
3. Collect a `FileExtent` carrying only `FileIdx` and `Durable`, because those are
   the only two fields `ReplaceFromResume` reads.

Then `Queue.ReplaceFromResume` installs the finding. It is authoritative
**over the files it is given, and only those**: it loops over the `exts` slice,
so for each file named there an article the resume did not verify goes back to
Outstanding, and the job's derived figures are recomputed from the bitmaps so its
reported health matches its per-article state.

### Resume write-back

A **recomputed** result is committed back over the Class B record it disproved,
from inside `Resumer.Resume`. An **adopted** result is not: it came from the
stored row, so rewriting it would restate its own contents.

A **missing file** is committed back too, as the empty result it produces.
Absence is the strongest disproof a resume can hold — not one article's bytes
are on disk — and the resurrection chain below does not care how the row was
disproved. Left standing, the row survives the file: the assembler recreates
it, `priorExtent` ORs the stale bitmap as its base, `buildExtent` stamps a
fresh `Size`/`ModTimeNs`, and the next start's fast path adopts articles this
process never wrote. The row is cleared only when one exists; a file that never
had a record does not get a zeroed one minted for it, because "a resume
examined this file and disproved every bit" is a claim about evidence that was
never gathered. The stamp written is `(0, 0)`, which cannot match any real
file, so the next resume that finds one recomputes (S4).

This makes the `Resumer` a second writer of Class B, and the design originally
forbade it: *"a committed extent claims a completed fsync stands behind it, and
a resume does not perform that fsync."* Two things are wrong with that.

The premise conflates the mechanism with the property. An `fsync` exists to
make bytes survive a restart. Reading those bytes back **after** a restart and
matching their recorded CRC observes that property directly, with strictly
better evidence than a syscall's return value.

The stated cost of omitting the write-back — *"re-verification on the next
restart is bounded rework"* — does not exist. Nothing clears a bit in
`file_extents`: `Durable.Set` is the only bit mutation in the package.
`Barrier.priorExtent` adopts the stored bitmap as an **OR-base**, so the next
checkpoint re-commits the disproven bit together with a fresh `Size`/`ModTimeNs`
from its own `Stat` — a stamp that then validates against the file. The next
start's fast path adopts it without reading a byte, and `reevaluateStall`'s
third phase replays the same rows through the additive `SeedFromExtents`. A bit
the recomputation disproved does not cost rework; it comes back.

Safe as a second writer because of **when** it runs: the startup sweep
completes before the downloader can dispatch, so no barrier is running for any
job and there is exactly one writer at that moment.

Every field of the written-back extent comes from the resume's own answer,
including `BytesDurable` — merging any part of the stored row forward would
preserve the claim being corrected, and committing without `BytesDurable` would
zero the figure the API reports for the file after a recomputation and leave it
overstated after a restart. The no-merge rule is what makes the missing-file
case work at all: every field of that result is a zero value, so a merge would
be indistinguishable from not clearing the row.

**A correction that CLEARS a bit is persisted before it returns**, and that is
load-bearing rather than tidy. Every re-hydration in the queue re-reads
`job_files.articles_done` unconditionally, so a cleared bit that lives only in
memory is undone by the next eviction and re-promotion — which the sweep
reaches without any concurrency, since it calls `Stall` on a job whose other
file faulted and `Stall` pauses the job, evicting the manifest. A bit the sweep
merely *sets* is not persisted here: losing it costs a re-fetch, which is the
safe direction under S3.

A file **absent** from that slice is not touched at all. Absence is silence, not
a finding of absence — and three ordinary cases produce it: a file whose filename
was never resolved (step 1 above), a file the sweep did not reach before a
storage fault, and every file of a job the phase or residency bound skipped. The
distinction is load-bearing in the safe direction: clearing on behalf of a file
nobody read would turn one unreadable mount into a full re-download of the job.

Two things are never cleared even for a file that *is* named: a permanently
failed article (its bytes were never on disk, so their absence is the recorded
outcome rather than new information), and a file's `Complete` flag where no bit
was actually cleared — `Complete` means "the assembler is finished with this
file", not "every article arrived", so it cannot be re-derived from the bits.

**Running only at startup is complete.** A job admitted later has no committed
extents to seed from, and a job's extents cannot change while it is not running —
only a barrier commits Class B, and a barrier runs only for a job with open
files. So a job promoted hours after startup is still correctly seeded by the
sweep that ran before it was promoted.

**The sweep never commits an extent.** A committed extent asserts that a completed
fsync stands behind it, and a resume proves what is on disk without performing
that fsync. Paying the verification again on the next restart is bounded rework
and is the correct cost.

**A fault does not discard the files already resumed.** `resumeJobFiles` returns
the extents gathered *before* the fault, `resumeAllJobs` seeds them, and only then
stalls the job. Returning early and discarding them turned a transient NFS flap on
file 7 of 20 into a permanent loss of ground for all 20: the stall pauses the job,
a paused job is not resident, and a non-resident job is skipped by every future
sweep — which only runs at startup anyway.

**A startup fault always stalls, even when it classifies permanent** — which is
deliberately *not* what `Barrier.routeFault` does. The two answer different
questions: the barrier asks "is this condition recoverable", while startup asks
"is there work to protect by failing". At startup there is none — nothing has been
downloaded in this process — so failing would send a job to history and discard the
bytes an earlier run left on disk, over an `EACCES` on a mount that has not
finished coming up at boot.

### Which jobs the sweep covers

The bound is on STATUS, not on phase and not on residency (`sweptStatus`):
**Downloading, Fetching and Paused**.

- Not phase, because `PhaseActive` excludes **Paused**, and a paused job is the
  case that needs the sweep most: it is mid-download, nothing but the assembler
  has ever written its files, and `Application.Stall` is what puts jobs there.
  Skipping it let #362 survive in that branch — the disproven Done bits were
  never corrected, `priorExtent` ORs the stored bitmap as its base, so the next
  checkpoint re-committed them with a fresh matching stamp and the file
  finalized over a hole. It also made `stallLost`'s own "restart gonzbd to
  resume this job from its committed extents" unable to work.
- Not residency either — `JobPhase.IsResident` is also true for
  `PhaseProcessing`, and in those phases something other than the assembler owns
  the job's files: par2 repairs a file **in place**, unpack reads it, the move
  relocates it out of the download directory entirely. The property the sweep
  needs is *the assembler is the only writer of these files*.

A swept job that is **not resident** — every paused one — is hydrated for the
duration and evicted again, so residency is unchanged from outside.
`Application.resumeAllJobs` takes a hydrated clone through `SnapshotJob` to read
the manifest, and `Queue.ReplaceFromResume` hydrates the live job itself to
apply the correction. Startup is when this is cheapest and safest: nothing else
holds a manifest and no article is being dispatched.

## Pre-allocation

Pre-allocation reduces per-write filesystem metadata overhead and fragmentation.
It is platform-specific:

| Platform | Mechanism | Failure behaviour |
|---|---|---|
| **Linux** | `fallocate(2)` — reserves contiguous extents without zeroing | falls back to `ftruncate` (sparse file) on `ENOTSUP`/`EOPNOTSUPP` (NFS, tmpfs, older FUSE) |
| **Non-Linux** | `ftruncate` — sparse on APFS, HFS+, ext4, xfs, btrfs | may allocate real blocks on a non-sparse filesystem; acceptable, since the file will be filled |

It uses `FileInfo.ExpectedSize`, the NZB's declared **encoded** byte count, which
runs ~2% above the file's decoded size. That difference is exactly why a completed
file must be trimmed: left in place it is trailing zeros, which par2 reports as
damage on a download that was perfectly healthy. The trim bound comes from the
durable facts (§4), never from a high-water mark the assembler maintained — there
is no longer any such figure, and `openFile` records no resume state at all.

`SupportsSparse()` (`sparse.go`) probes whether the target filesystem supports
sparse files by creating a temporary file, truncating it to 1 MiB and checking
`st_blocks * 512 < apparent_size`. It is an **informational probe** used at
startup for logging; it does not gate pre-allocation. The assembler always
attempts `fallocate`/`ftruncate` regardless of the result.

## Write coalescing cache

When `Options.WriteCacheBytes > 0`, the shared `writeCache` buffers decoded
articles in memory and coalesces contiguous runs into larger `WriteAt` calls.

The cache is **assembler-wide, not per-writer**, because the memory bound in B2 is
global across files: `forceFlushLargest` has to compare files against each other,
and the coalescing scratch buffer is reused across all of them.

- **Buffering**: each article is stored in `fileBuf.articles[offset]`, keyed by
  byte offset; total memory is tracked in `writeCache.used`.
- **A zero-length article is refused, and the contiguous scan stops at one.**
  `offsetInRange` admits an empty write, so nothing upstream rules one out.
  Buffering it would wedge the scan, which advances by the length of the article
  at the cursor: a zero-length entry there never moves it and the loop never
  terminates — on the worker goroutine that owns every file handle, so it takes
  all assembly with it. `buffer()` returns `cached == false` so the caller writes
  it inline, where the `WriteAt` is a no-op. `buildContiguousRun` also breaks on
  one rather than trusting that.
- **Contiguous flush**: after each `buffer()`, `flushContiguous()` scans from the
  file's `writeCursor` for a contiguous run ≥ 512 KiB (`contiguousRunSize`),
  coalesces it into the reusable `scratchBuf` and writes it as a single
  `WriteAt`.
- **Pressure relief**: at `used > 90%` of the limit, the file with the most
  buffered data is force-flushed regardless of contiguity, articles written
  individually in offset order.
- **A drain advances the cursor and keeps the file's entry.** `drainFile()` moves
  `writeCursor` past every article it returns — gaps included — and clears the
  entry rather than deleting it, so the cursor survives into the next round of
  buffering. An entry deleted here would be recreated at cursor 0, an offset
  whose article was just written and will never be re-buffered, stranding the
  scan for the rest of the file (#311). An article arriving later below the
  advanced cursor is still buffered and still written by the next drain; it just
  does not join a coalesced run.

**`writeCursor` is an in-memory coalescing frontier and nothing else.** It is not
persisted, not seeded from anything at open, and is not evidence about disk:
`drainFile` advances it past gaps and before any write is attempted, so it sits
above the bytes actually written whenever a write then fails. Collapsing the
frontier and the durability anchor into one value is what made the old
`write_cursor` column unusable, and the two questions are now answered separately
— `FileExtent.Durable` for resume, `FileExtent.VerifiedTo` for the CRC anchor.

**The cache is on by default at 64 MiB** — `constants.DefaultWriteCacheBytes`,
seeded into `Downloads.WriteCacheSize` by `config.Default()` and threaded to
`Options.WriteCacheBytes` in `app.New`. Setting `write_cache_size: 0` disables
it, and each article is then written directly through `FileWriter.writeOne`. The
default is a tuning choice, not a durability one: the barrier drains the cache
before every fsync, so neither setting changes what may be claimed. See
*Open gaps* for what is and is not measured about the win.

Decoder buffers are returned to `sync.Pool` (`decoder.PutBuffer`) on every path,
including every failure path.

## Duplicate and late-article handling

- **Per-writer `seenDone` / `seenFailed`**: dedup by `ArtIdx`, membership-only.

  This used to say `seenDone`'s value was "the offset the first copy was
  accepted at, which the duplicate branch needs". Both halves were wrong. The
  duplicate branch never asked: `handleSuccessArticle` releases the buffer and
  returns, because the answer changes nothing — either way the second copy's
  bytes are redundant and re-writing them is a second `WriteAt` over the same
  range. The value was written and never read, and #375 removed it.

  Offsets are owned by `acceptedAt` instead, which is a different index for a
  different question: not "has this article been seen" but "who owns this byte
  range, and have their bytes been written". See the collision rules below.
- A write path that **fails** moves its articles out of `seenDone` and does
  **not** put them in `seenFailed`. An earlier version of this rule said it did,
  which contradicts the roll-back rule below and the behaviour of
  `FileWriter.fail`: recording a failed write as a failed ARTICLE made the
  redelivery take the already-counted-as-failed branch, written but not counted,
  leaving the file's part total permanently short. There is no ack in either
  direction on a failed write (A1). Absence from the next `Drain` is
  necessary but **not sufficient** to leave the article Outstanding: its
  Emitted bit survives, and `ForEachUnfinishedArticle` skips a set Emitted bit.
  The fault's route is what clears it — see the write-error rule below.
- **Two articles claiming one offset** resolve one of two ways, and which one
  depends on whether the incumbent has been reported Written. Detection lives in
  `FileWriter.acceptedAt`, an offset→owner index recorded in `Accept`, and a
  collision is decided by **identity**: an offset already owned by the same
  article is a re-accept after a rollback, not a collision.

  Detection used to live in `writeCache.buffer` and keyed on cache residency,
  which missed the ordinary in-order case entirely — the first article was
  flushed and evicted before its duplicate arrived, so both were counted and the
  file completed with one part's bytes overwritten (#383). Detection is
  per-open-episode, the same residency as `seenDone`.

  - **Incumbent written → the offset is SETTLED and the ARRIVAL is rejected**
    (`offsetSettledBy`, checked in `acceptArticle`). Its bytes back a durable
    claim: the pipeline recorded a Class A fact naming its CRC at that offset,
    and the barrier will ack it. Letting a later article overwrite the range
    makes that fact unverifiable on restart, and failing the incumbent as well
    would give one article two terminal dispositions — permanently failed *and*
    acked durable. The arrival is resolved permanently failed, keeps its part
    (it will never arrive again), and its bytes are charged to par2.

    The `written` flag is **latched on the offset**, not derived from
    `w.written`/`w.reported`. `Confirm` empties both once the articles are
    acked, and an acked article holds the strongest claim there is — a derived
    check would read the empty set as *no* claim and displace it one checkpoint
    later.

  - **Incumbent still buffered → the INCUMBENT is displaced**, which is what the
    write cache always did. It made no claim, so failing it corrects the
    writer's own accounting and nothing durable. It is resolved *permanently
    failed* rather than returned to Outstanding — re-fetching it reproduces the
    collision, observed as a ping-pong that never settles.

    It **keeps its part**, and is counted for one through
    `admitPermanentFailure` if it does not already hold one. `TotalParts` counts
    manifest segments, so two segments claiming one offset are two parts the
    file waits for; a file that stopped counting the loser could never reach
    `TotalParts`, which left it permanently one short (#386). Every displaced
    article goes through the same call, keyed on `ArtIdx`: there is no
    Message-ID-specific case to exempt, because `seenDone` and `seenFailed` are
    keyed on `ArtIdx` — which an NZB can never leave empty — not on the
    Message-ID an earlier version of this code needed a separate exemption for.

    `handleSuccessArticle`'s and `handleLateDuplicate`'s dedup arms test
    `seenDone` before `seenFailed`, so a redelivery of the loser takes the
    duplicate arm — matched by `ArtIdx` — rather than being re-written and
    displacing the winner in turn. Before F1's re-key, those arms were gated on
    a non-empty Message-ID, so an article carrying none needed a separate
    record, `FileWriter.resolvedUntracked`, to get the same protection: without
    it, every redelivery of such an article was counted afresh and displaced
    the current owner in turn — the count climbed one per copy until it reached
    `TotalParts` over a segment that had never arrived, and the file was
    finalized short. `resolvedUntracked` is gone; `seenFailed` alone now does
    that job for every article, tracked or not.

    Its buffered bytes go with it, through `writeCache.discardAt`, and that call
    is load-bearing rather than tidy. `wc.buffer` evicts the entry itself
    whenever it accepts the arrival, but it refuses a zero-length article
    *before* touching `fb.articles` — so without the explicit discard the
    incumbent stays cached, and the next `Drain` writes its bytes and hands them
    to the barrier to ack durable, for an article already reported permanently
    failed. Detection used to BE the eviction, so the two could not disagree;
    moving detection ahead of the cache separated them.

  Either way the first collision on a file raises `Options.OnPostAnomaly`, which
  the app routes to `job.Warning`. That is diagnosis, not accounting: it states
  that two segments claim one byte range without asserting the post is
  malformed, because a redundant posting and a server-mangled `=ypart begin=`
  produce the same observation and yEnc checksums the payload, never the header.
- **Cross-state dedup**: an `ArtIdx` previously counted as a success arriving as
  a failure (or vice versa) does not increment `partsWritten` again.
- **Late articles**: an article for a file already in the `completed` tombstone is
  handled by `handleLateDuplicate` — data returned to the pool, no disk write, no
  claim.

## Control messages

All three are encoded as sentinel `FileIdx` values on `WriteRequest` and are
synchronous from the caller's perspective: the caller blocks until the worker,
which owns every file handle, has done the work and answered. The sentinels are
declared together in `internal/assembler/synctarget.go`; the numbers below are
the encoding, and the names are what the code reads.

| Control | Encoding | Worker behaviour |
|---|---|---|
| **CancelJob** | `JobID=""`, `FileIdx=fileIdxCancelJob` (-1), `MessageID=jobID` | closes and *deletes* all open files for the job, tombstones the job in `cancelledJobs`, discards cached articles, closes `ackCh` |
| **CloseJobHandles** | `JobID=""`, `FileIdx=fileIdxCloseHandles` (-2), `MessageID=jobID` | drains, `Sync`s and `Close`s handles *without deleting*, tombstones the files, **sends any close-time fault on `ackCh`** and closes it. Used when a job enters post-processing or par2 repair |
| **Barrier op** | `JobID=""`, `FileIdx=fileIdxSyncOp` (-3), `syncOp` payload | `Files`, `Jobs`, `Drain`, `Sync`, `Stat`, `Truncate`, `Close` on one file, on the worker goroutine |

The barrier-op indirection is invariant X1, not ceremony. One goroutine owns all
the state, so the barrier can reach a file's cache and handle without a lock. The
alternative — a mutex over the open-file map and the writers — would put `WriteAt`
and `fsync` inside a critical section, which is both a contention disaster on the
hot path and exactly what `scripts/check_lock_io` exists to catch.

Closing a file that is already gone is a **no-op, not an error**: a race with
`CancelJob`, `CloseJobHandles` or shutdown is the expected outcome, not a
disagreement. A file the barrier believes open but the worker does not **is** an
error, reported rather than routed through `Stallable`.

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
from inflating a file's apparent size with a crafted yEnc `=ypart begin=` header.
A rejected write returns its buffer to the pool and makes no claim about its
bytes.

It is an ARTICLE fault, not a storage fault, so it resolves against the article
(A1): `OnArticleRejected` carries it to `Queue.AckPermanentFailure`, which
charges its bytes to the job's failed-byte count, releases on-demand par2, and
clears its `Emitted` bit so nothing waits on a re-dispatch that will never come.

The rejected article still **counts toward its file's part total**. That looks
like the wrong direction and is not: it will never arrive again, so a file that
declines to count it can never reach `TotalParts`, `OnFileComplete` never fires,
and the job sits at 100% with zero outstanding articles across restarts.
Counting it claims nothing — no Class A fact was decoded, so no durable bit is
earned and no fact-derived truncate bound reaches past it. This is what a
permanently failed article already does through `handleFatalArticle`.

## DirectUnpack streaming contract

DirectUnpack is a **volume-level** streaming extractor, not an article-level one.
It reads fully assembled RAR volume files from disk; it never reads partial
articles or sparse regions.

1. **Volume completion signal**: `OnFileComplete` reports a volume whose parts
   have all been written, with the handle **still open**. The volume is
   nevertheless complete, fsynced, trimmed and closed by the time DirectUnpack
   sees it, because `handleFileComplete` runs `finalizeCompletedFile` *before*
   handing the event to the orchestrator. That ordering is load-bearing: unrar
   reading a file that still carried pre-allocation's trailing zeros would see a
   corrupt volume. When the finalize fails, DirectUnpack is not reached at all
   and the handle is not closed (see the handoff section above).
2. **Volume waiting**: `waitForVolume()` blocks on `volumeReady` until the
   requested volume number appears in `completedVols`, and returns immediately if
   the set is in `corruptSets`.
3. **Sequential volume feeding**: `startVolumeFeed()` opens completed volumes in
   order and sends their `*os.File` handles to `rarengine.StreamDecompressor`.
4. **Corrupt volume handling**: `MarkCorrupt(setname, reason)` is called by the
   queue when a volume was assembled from a download with missing or failed
   articles. Once marked, the set can never be reported as successfully
   extracted — `waitForVolume` checks on each wake, and `extractSet` re-checks
   after extraction as a backstop for volumes that arrived before the corruption
   was detected.
5. **Non-RAR handling**: `extractSet` calls `rarheader.Version()` on the first
   volume's magic bytes. Any error — including I/O errors, not just format
   mismatches — yields `errNotRAR` and the set is recorded as `SkippedSet`, not
   failed; the normal unpack stage's external `unrar` handles it.
6. **Format support**: `rarengine` (pure Go RAR3/RAR5). Other formats, legacy
   RAR2, and non-RAR files identified by filename go to post-processing.
7. **Abort/kill**: `Abort()` sets `killed`, records failures for the current and
   queued sets, clears success results, and signals the reader goroutine. If
   `run()` was never started it closes `done` directly.
8. **Path traversal safety**: `extractEntries` opens an `os.Root` anchored at
   `extractDir` and writes every entry through it, so archive entries with `..`
   components, absolute paths, or symlinked path components cannot escape.

## Memory & allocation budget

| Component | Bound / strategy |
|---|---|
| Write channel (`reqs`) | 2048 requests (`defaultQueueSize`). At 128 KiB articles, ~256 MB worst-case buffered; backpressures the downloader when disk I/O is slow. |
| Write cache | `Options.WriteCacheBytes`, pressure relief at 90%. Default 64 MiB (`constants.DefaultWriteCacheBytes`); `write_cache_size: 0` disables it. |
| Contiguous flush threshold | 512 KiB (`contiguousRunSize`). Shorter runs stay buffered. |
| Coalescing scratch buffer | one reusable `[]byte` per `writeCache`, grown to the largest flush. |
| `FileWriter.written` / `.reported` | bounded by one checkpoint window; `reported` accumulates only between *successful* syncs, and a job whose syncs are failing stalls (R19) and stops writing. |
| `internalFileComplete` | cap 128. **Not** a bound on retained fds — see the handoff section. |
| Decoder buffers | every `req.Data` returns to `decoder.PutBuffer` after write, error or discard. |
| Disk probe cache | one `probeState` per directory, evicted after 10 minutes; at most one outstanding `statfs` per directory. |
| Per-job barrier state | `jobBarrierMu`, `jobBarrierBytes` and `lastBarrier` are dropped by `forgetJobBarrierState` when a job leaves the assembler's business — otherwise one entry per job ever downloaded, for the life of the process. The mutex's deletion is **deferred while anyone holds it**: dropping it let the next caller mint a second mutex for the same job, which serialises nothing, and the delete is reachable from inside a live barrier via `routeFault → Fail → maybeFinalize → enqueuePostProc`. |
| Durability rows | `article_facts` and `file_extents`, deleted per job by `deleteJobDurability`. Neither table has a foreign key to the queue, so nothing removes them implicitly. `SQLiteStore.Prune` is the backstop: on every queue save it deletes rows whose job is in neither `jobs` nor history-as-`Failed`, which catches a crash in the window between a job leaving the queue and its rows being deleted. The `Failed` exception is load-bearing -- a retry bounds `FinalizeFile`'s truncate with those rows. |

## Failure & degradation rules

- **Write error (`pwrite` failure)** — the article is *not* acked and *not*
  failed. Two things then have to happen, and they travel on **separate**
  callbacks:

  | Callback | Carries | Does |
  |---|---|---|
  | `Options.OnArticlesUnwritten` | every article the failure rolled back | clears their Emitted bits, returning them to Outstanding |
  | `Options.OnWriteFault` | the classified `*storagefault.Fault`, no article | stalls or fails the job on the usual R18 rule |

  They are separate because they are needed in different combinations. A fault
  raised inside `Accept` needs both. A `Drain` or `Sync` failure reaches the
  **barrier**, which routes the fault — but the rolled-back article set never
  crosses the `SyncTarget` interface, so the assembler still owes the first.
  `OnWriteFault` used to carry a single article index and do both, so every
  batch failure reported one article and rolled the rest back silently: they
  were left neither Done, nor Failed, nor Outstanding, and only a restart's
  `ClearAllEmitted` recovered them.

  A rolled-back article also **gives back its part**. `partsWritten` is
  incremented when an article is *accepted*, so leaving the count in place put
  the file one part closer to `TotalParts` with nothing behind it — and a later
  article could take it to completion, firing `OnFileComplete` over bytes that
  never reached `WriteAt` with `failedBytes` unchanged, so the job reported
  100% health.

  The give-back is **not** part of the routing above, and reading it as such is
  how the two used to drift apart. `partsWritten` lives on `FileWriter`, beside
  the `seenDone`/`seenFailed` sets it is derived from, and `FileWriter`
  applies the decrement in the same statement pair that clears the article's
  `seenDone` entry — in `rollbackPart`, which `fail` calls before recording what
  becomes of the article. `failDisplaced` does not: it resolves its article
  rather than rolling it back, so it counts through `admitPermanentFailure`
  instead. The routing callbacks decide *disposition* only. An article
  already counted as permanently **failed** keeps its part through a roll-back:
  `admitPermanentFailure` charged it, a redelivery writes bytes without
  charging a second one, and decrementing there leaves the file one part short
  of `TotalParts` forever.

  `FileWriter.Close` **returns** whatever is left in the rolled-back set
  alongside its error, so the set cannot be dropped by omission at the moment
  the writer stops existing. It is empty at both call sites on every reachable
  path; a non-empty one means a producer was added that nothing drains.
  `drainAndClose` routes it — that file is closing normally and its articles
  are still wanted — while the job-cancel arm drops it, because the file is
  unlinked on the next line and the job is leaving the queue. Both report at
  Error, with the article indices, since the cancel arm's log is the only
  record that survives the drop.

  A coalesced run rolls back **every** article merged into it, not just the one
  whose arrival triggered the flush: `buildContiguousRun` pooled the originals
  before the write was attempted, so reporting only the trigger would leave the
  rest believed written with their bytes freed. The same holds for everything
  after a failed write in a drain. A cache displacement contributes to the same
  set without rolling anything back.

  A rolled-back article is **not** put in `seenFailed`. A storage fault says
  nothing about the article's availability (A1), and recording it failed made
  its redelivery take the "already counted as failed" branch — written but not
  counted — leaving the file's part total permanently short. A *displaced*
  article is put there, and that is not a counter-example: it is resolved
  rather than rolled back, so its redelivery should be recognised and refused
  rather than written again.
- **Drain stops at the first write failure**, returning the articles that *did*
  land plus the fault, so the barrier sees both what it may claim and why the
  drain stopped. Continuing would be optimistic: a storage fault is a condition
  of the device. Articles after the failure are released to the pool and rolled
  back, so a re-delivery is not mistaken for a duplicate.
- **`FileInfo` resolution, `MkdirAll`, or `OpenFile` failure** — the article's
  data is returned to the pool, its Emitted bit is cleared through
  `OnArticlesUnwritten`, and the fault is routed. The file is never opened and
  never appears in `open`, so no barrier operation can ever surface it — which
  is why this path routes for itself rather than leaving it to the barrier.
- **CancelJob** — closes and deletes open files, tombstones the job so subsequent
  articles are discarded. The `ackCh` synchronisation lets the caller delete the
  job directory the moment `CancelJob` returns.
- **Shutdown (`Stop`)** — closes `stopCh`, waits for in-flight senders, drains
  remaining channel items, flushes the write cache, and closes all open files.
  Partial files are closed without firing `OnFileComplete`.

## Accepted limitations

These are known, deliberate, and **not** claims about correctness. They are
recorded here so the next reader does not mistake them for design.

1. **The startup sweep skips non-resident jobs.** `ReplaceFromResume` needs a
   resident manifest. **Resolved:** a swept job is hydrated for the duration of
   the correction and evicted again, so the durability subsystem's own fault
   response manufacturing that state — `Application.Stall` → `Queue.Pause` →
   eviction — no longer takes the job out of the sweep's reach. What remains
   true is that the sweep is startup-only: a job stalled after startup is not
   re-swept until the next one.

2. **`StatusFetching` is swept and is not download-only.**
   `constants.StatusFetching` means "downloading extra par2 files for repair" —
   a repair-time status. The bound is sound today only because **nothing
   assigns it**: it exists in the transition table, the phase mapping and the
   API's vocabulary, and no code path sets it. That is a fact about the writers,
   not an invariant the type enforces. The first code that starts setting it
   puts a repair-time job inside the window `sweptStatus` trusts, and must
   remove it from that list. The other way in is any non-assembler writer
   arriving while a job is Downloading or Paused — a DirectUnpack that wrote
   back into its source rather than reading it, or a repair moved earlier than
   download-complete.

3. **The SPLIT case in stall recovery.** `reevaluateStall` phase 3
   (`seedFromCommittedExtents`) logs and returns on failure, while phase 4 still
   delivers the completion. The result is a file marked `Complete` with some of
   its articles still Outstanding — `IsComplete` is file-based
   (`internal/queue/job.go`), so the two do not have to agree. The cost is wrong
   figures and a wasted re-fetch, not corruption or a short file. Recorded and
   unfixed.

4. **A file with a hole reports no whole-file CRC.** `durability.verifiedPrefix`
   combines the Class A facts into `FileExtent.PrefixCRC` during the walk it
   already performs, and `HasPrefixCRC` is set when that prefix reaches the
   file's end **and the walk consumed every recorded fact**. Both clauses are
   required, and `verifiedPrefix` is the only place the claim is *created* —
   the barrier used to derive the flag at its call sites from `VerifiedTo` and
   `Size` alone, which cannot see a fact the walk never reached, so an article
   overlapping a sibling without extending the file published a CRC of the
   bytes that should have been written (#387). One place *narrows* the created
   claim without re-deriving it: `Resumer`'s cache fast path additionally
   requires `ext.VerifiedTo == fi.Size()`, because a file whose size changed
   under a cached extent has invalidated it. Narrowing a claim the guard
   already allowed cannot manufacture one it refused, which is why that site
   is safe and the barrier's old call-site derivation was not.
   `Application.recordAssembledCRC` threads the result to
   `Queue.SetFileCRC32` when the file finalizes. A permanently failed article
   leaves a hole, the prefix stops there, and no whole-file value exists to
   record — `FileProgress.AssembledCRC32` stays zero, which is the documented
   "unavailable" value (#349), so QuickCheck reads `NoCRC` and
   `par2NeedsRecovery` conservatively returns true for that file. This is the
   correct answer rather than a gap: a partial prefix recorded as the file's CRC
   would report corruption for a file that is merely incomplete.

5. **The crash suite does not test fsync-to-platter.** See below.

## What the crash suite actually pins

`test/crash/` (build tag `crash`, Linux only, six tests) runs the real daemon as
a child process and kills it. It is the strongest evidence in the repository for
this contract, and its scope is narrower than "durability":

**It pins the assembler's in-process write cache.** A SIGKILL destroys that cache
for real, with no flush, so an article acked before its bytes left the process has
no bytes in the file afterwards and the CRC read-back sees it.

**It does not pin fsync-to-platter.** No unprivileged userspace call can discard
dirty page-cache data: `POSIX_FADV_DONTNEED` invalidates clean pages and skips
dirty ones, `/proc/sys/vm/drop_caches` skips them too, and `O_DIRECT` flushes
first. This was verified empirically, not reasoned: **removing the `Sync()`
syscall entirely left the suite byte-identical to baseline.** Real coverage needs
a device the test can cut underneath the filesystem — a device-mapper
`log-writes` or `flakey` target — which needs root.

`ENOSPC` is likewise not covered by the crash suite; the stall path is covered
in-process instead. Both gaps are tracked as issue **#363**.

`docs/TESTING.md` §3a is the full account, including the per-test table and what
a green run does and does not bound.

## Status

### Landed

- `internal/durability`: `Barrier` (checkpoint and `FinalizeFile`), `Resumer`,
  `DurableProof`, `Bitmap`, SQLite `FactLog` and `ExtentStore`.
- `internal/storagefault`: classification into retryable/permanent with the
  operation and path attached.
- Compiler-enforced ack path: `Queue.AckDurable(durability.DurableProof)` —
  enforced on the proof's *payload*, and on that one door. See §1 for the exact
  bound and for the seeding doors it does not cover.
- `assembler.FileWriter` — per-file ownership with no authority to ack, record a
  CRC, decide completion, or truncate.
- Barrier operations over the assembler's control channel (`fileIdxSyncOp`),
  timeout-bounded.
- Checkpoint cadence: time bound, byte bound, file completion, clean shutdown,
  with `lastBarrier`/`PendingBytes` surfaced through the API and UI.
- Authoritative startup sweep (`resumeAllJobs` → `Queue.ReplaceFromResume`) and
  the additive stall-recovery replay (`SeedFromExtents`).
- Storage-fault stall/fail with a surfaced, actionable reason and interval-based
  re-evaluation.
- `article_facts` and `file_extents` tables; `job_files.bytes_downloaded`,
  `max_written` and `write_cursor` removed.
- Crash-consistency suite (`test/crash/`, six tests).

### Open gaps

- **The startup sweep is startup-only and resident-only.** See *Accepted
  limitations* #1.
- **`ENOSPC` and page-cache loss are untested** (#363).
- **In-flight coalescing still stalls on a permanently failed article.** A gap
  the download will never fill leaves `buildContiguousRun` stranded at the cursor
  until the next drain re-anchors it. #311 fixed the pressure-drain route to the
  same symptom; this route remains. Its cost is memory residency rather than
  syscalls, and two designs targeting it directly were measured and found worse
  than leaving it alone. Measure the residency cost before attempting it again.
- **Write coalescing has no measured win on a local filesystem.** A sweep of
  `WriteAt` chunk sizes over the same payload found wall-clock flat on btrfs and
  *worse* for large chunks on tmpfs, because coalescing trades N syscalls for one
  syscall plus a second memcpy of the same bytes. The mechanism is sound where a
  write is expensive per call — NFS/SMB, where each `pwrite` is a round trip —
  and the local-filesystem case is unmeasured rather than known-good. Note that
  the cache is **on by default at 64 MiB**, so this gap describes the shipped
  configuration: the work is to measure it on a local filesystem and either
  justify the default or change it. Do not read this bullet as advice to turn
  the cache off; nothing here establishes that off is better, only that on is
  unproven locally.
