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

Such a job has no `durable_runs` rows. Article resolution is **derived** from
those rows — `done` means "covered by a run" — so with none, every article is
Outstanding and the partial files on disk are overwritten in place by the
re-download.

There is nothing to override, and that is the point of deriving the answer
rather than storing it separately. An earlier shape kept a `job_files.articles_done`
bitmap beside the durability record, and "there is no evidence, so trust the
column" was indistinguishable at runtime from a lost or truncated record —
precisely the case #362 exists to catch: a partial file that was truncated or
deleted out of band finishing as a complete file with a zero-filled hole in it,
silently. The column is gone (`003_drop_legacy_durability.sql`), so the
question no longer arises.

Completed jobs, history, configuration and the queue's ordering are unaffected.
Only in-progress downloads pay, and only once.

## Why this exists

Nine open issues (#306, #311, #337, #344, #349, #353, #355, #356, #357) and five
of the eight merged fixes before them (#305, #315, #341, #343, #350) were two
defects wearing different clothes — see
`docs/superpowers/specs/2026-08-11-download-durability-design.md` for the full
derivation. The dominant one is **a claim recorded before the thing it asserts
becomes true**: an article marked `Done` while its bytes sat in a memory buffer
(#355, refiled as #356), a truncate bound describing one process's writes applied
to a file built by several (#342, #350), a CRC over a subrange reported as the
CRC of a file (#349).

**A second round of that same family is why there is now one record rather than
two** (#389, #421, #423). The first design kept a per-article record written at
decode time — before the write, and unordered against it — beside a per-file one
written after the fsync. Two writers describing one download disagreed, and each
disagreement was a defect of exactly the shape above. See § *One record*.

The costs are asymmetric, and that asymmetry is the whole design:

- **Over-fetching is a cost.** An article re-downloaded needlessly wastes
  bandwidth and time. It is bounded, visible and recoverable.
- **Over-claiming is a defect.** `ForEachUnfinishedArticle` skips any article
  whose `done` bit is set, and `ResetForRetry` clears `done` only where `failed`
  is also set — so a `Done` published for bytes that never landed is permanent.
  No later run re-dispatches that article, and the file stays short. Without
  par2 it is silent.

So every rule below resolves ambiguity toward re-fetching. The one claim that
can over-claim — "this article is on disk" — is now **written in exactly one
place**: `durability.Barrier`, after an `fsync` it performed, and nothing else
puts content into a `durable_runs` row. Stating the bound precisely, because a looser
version of this sentence has misled before: what the *compiler* enforces is
narrower than that, covering `Queue.AckDurable`'s proof payload and that door
only (§1). That nothing else puts content into the record is enforced by there
being one such writer, not by the type system — and it is a claim about
CONTENT, not about the table, which several paths delete from (§6).

## One record

Everything persisted about download progress that concerns *bytes on disk* is
one record, in one table, and exactly one thing can put content into it.

```
durable_runs(job_id, file_idx, first_art_idx, last_art_idx, offset, length, crc32)
```

One row per **run**: a maximal span of articles that abut in **both** byte
offset and article index and were made durable by the same `fsync`. Adjacent
rows merge — combine the CRCs, sum the lengths, widen the index range — so a
file whose articles arrive in order collapses toward a single row at offset 0
whose `crc32` is the whole-file CRC.

| | **`durability.Run`** |
|---|---|
| Content | `{FileIdx, FirstArtIdx, LastArtIdx, Offset, Length, CRC32}` |
| Asserts | the bytes at `[Offset, Offset+Length)` **are** present on stable storage, and they hash to `CRC32` |
| True from | only after a completed `fsync` |
| Ordering vs. the write | strictly after the `fsync` (S1, S2) |
| Written by | `durability.Barrier`, and nothing else **inserts or amends a run's content**. Deletion is a separate, wider set — see below. |
| Authoritative | **yes** — gated on one `stat` per file at startup (S4, inverted; see §6) |
| Losing a suffix costs | a re-fetch (R3), which is the routine cost of an unclean shutdown |
| Stored in | `durable_runs` |

**This replaced two records with independent writers, and the replacement is
the design.** Until this change the same download was described twice: a
per-article Class A `ArticleFact` appended at decode with no ordering against
the write, and a per-file Class B `FileExtent` committed by the barrier after
the fsync. Two writers describing one thing can disagree, and every
disagreement was a defect — #389 recorded an article the assembler then
rejected, #421 recorded a bogus yEnc offset permanently because the store was
append-only and re-fetching was the one mechanism `INSERT OR IGNORE` ignored.
Both classes are gone, along with the contiguity apparatus that existed to
reconcile them: `verifiedPrefix`, the abutment walk, `durableAt`, the durable
`Bitmap`, and both of `FinalizeFile`'s guards. The full argument is
`docs/superpowers/specs/2026-08-22-single-durability-record-design.md`.

The second table is **`failed_articles`** — `{job_id, art_idx}`, one row per
permanently failed article. It is not a durability record and `internal/durability`
never touches it: a failed article never decodes, so nothing was ever written
for it and no run could cover it, and `internal/queue` is its sole writer.

One consequence worth stating explicitly, because it has been got wrong:
**neither record carries a failed-byte figure, and neither can.** No sum over
runs can produce it, because a failed article has no run; and `failed_articles`
carries an index, not a size, while the NZB-declared size behind it lives in
the manifest a non-resident job does not hold. It is cached in
`job_files.failed_bytes` instead.
`internal/history/migrations/003_drop_legacy_durability.sql` records that
reasoning at the schema, superseding `001_initial.sql`'s version of it.

### What changed against the previous contract

A reader who knows the two-record version will otherwise carry forward rules
that no longer hold. Two are **deleted**, two are **amended**, one **improves**,
and one is new. Each change is argued at the section named.

| Rule | Was | Now |
|---|---|---|
| **R1** — the record is immutable | `INSERT OR IGNORE`; a re-delivery could not correct a bad row, which is #421 | **deleted.** Merging is read-modify-write, and the newest `fsync` is the newest truth. The threat R1 guarded — a record describing bytes that were never written — is *unreachable* rather than rare, because the record is now built from what a completed `Drain` reported after a completed `fsync`. |
| **R2** — the record is not ordered against the write | may be committed before, during or after the write, or when the write never happened at all | **deleted.** The record exists only after a completed `fsync` (S1, S2). |
| **S4** — a recomputation beats the stored record | the done-set was rebuilt from the per-article facts plus a disk read, and the stored record was *never* authoritative | **INVERTED.** The record is authoritative, gated on one `stat`. See §6 — this is the change most likely to be misread. |
| **S7** — the validity stamp | `(size, mtime)`; a mismatch fell through to a recomputation | **narrowed to size**, and `ModTimeNs` is deleted. See §6 for why the *response* to a mismatch, not the stamp, decides this. |
| **S5** — no second copy to drift | two records, two `FinalizeFile` guards | **improved.** One record; both guards and `file_extents` gone (§4). |
| **Writers of record CONTENT** | **two** — the barrier's commit, and `Resumer.writeBack` | **one** — the barrier. The resume only *deletes*, and so do four other paths, none of which can make a row assert anything (§6). |
| **S1, S2, S6, R3** | — | unchanged. R3 is now the routine cost of an unclean shutdown rather than an edge case. |

## The state of an article

```
                  decoded                writeAt returned nil        fsync returned nil
  Outstanding ───────────────► Decoded ──────────────────────► Written ──────────────────► Durable
       ▲                          │                               │                          │
       │                          │ nothing is persisted          │                          │ Barrier commits the
       │                          │ here — the CRC just rides     │                          │ RUN, then mints a
       │                          │ along to the drain report     │                          │ DurableProof
       │                          │                               │                          ▼
       │                          │                               │                    durable_runs
       │                          │                               │                          │
       └──────────────────────────┴───────────────────────────────┘                          ▼
          a write failure, a storage fault, or a                                     Queue.AckDurable
          restart with no run covering the article                                           │
          returns the article to Outstanding                                                 ▼
          — never Failed                                                              ARTICLE IS DONE
                                                                                             ▲
                                       ON RESTART, the SECOND way in:                        │
                                       the runs a previous process committed are ────────────┘
                                       adopted, gated on one stat per file, and
                                       Queue.ReplaceFromRuns resolves the
                                       articles they cover — no barrier, no
                                       proof, no fsync by this process. See §6.
```

There are therefore **two** ways an article becomes Done, and only the first
goes through a barrier. The resume path is not a loophole — it is the only way
to credit bytes an earlier process wrote, which no proof can express — but any
statement of the form "X is the only thing that resolves an article" is false
unless it says *during a download*.

Note what the resume path is **not**, because the shape it replaced worked
differently: it does not read the file back and check a CRC. It stats the file,
and if the file is at least as long as its runs claim it adopts them whole. See
§6.

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
| **Barrier** | `durability.Barrier` | The only place `Written → Durable → Resolved` happens **during a download**. Drains, fsyncs, commits the runs, mints the proof. Not the only place an article becomes Done: the Resume tier below resolves articles too, with no barrier and no proof — see the state diagram above and §1. | Holds no lock of its own; the cadence owner serialises it per job (`Application.jobBarrierLock`). |
| **Cadence** | `Application.runCheckpoint`, `noteJobBytes` | *When* a barrier runs. Time bound, byte bound, file completion, clean shutdown. | One goroutine; per-job mutex around each barrier. |
| **Resume** | `durability.Resumer`, `Application.resumeAllJobs` | One `stat` per file at startup, and no reads — and, through `Queue.ReplaceFromRuns`, the second path by which an article becomes Done. Its whole mutation budget is **deletion**: a file shorter than its runs claim has them dropped. Authoritative over the files it is passed: it *clears* bits no surviving run covers. | Per-file, shares no state between calls. |
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

**This gate covers one door.** `Queue.SeedFromRuns` and
`Queue.ReplaceFromRuns` also reach `markDone`, and are callable from any
package with no barrier and no proof. That is deliberate — their evidence is
the runs a barrier's fsync already recorded, which is exactly the kind of
evidence a proof cannot represent — but it means "ack before fsync is code that
does not compile" is true of `AckDurable` and **false as a statement about the
queue as a whole**. The seeding doors are held by their contracts and by
`TestSeedFromRuns_StaysAdditive` /
`TestSeedFromCommittedRuns_DoesNotClearAnAckThisProcessMade`, not by the
compiler.

**How much narrower those doors got, exactly.** `durability.Run` is an exported
struct with exported fields, so any package can build one — the narrowing is
*not* that the type is unforgeable, and a claim that only the store constructs
one would be the same overstatement this section already had to retract about
`FileExtent`. What changed is that a `Run` carries an article **range** which
`runsCoverage` validates against the job's own manifest, refusing a run whose
`[FirstArtIdx, LastArtIdx]` falls outside the named file's article range. The
`FileExtent` it replaced carried a fully exported, settable `Bitmap` whose set
bits were taken at face value.

This replaces a design in which the assembler could ack from six places, each
independently responsible for knowing that acceptance into a buffer is not
evidence about disk. That is why the same defect kept being refiled.

### 2. The barrier's order is the invariant

`Barrier.Run` performs, for one job:

```
  phase 1: Drain every open file          — no claim of any kind yet
  phase 2: Sync  every open file          — only now may anything be claimed
  phase 3: Stat  every open file          — collect the drained articles + sizes
  phase 4: RunStore.Commit (atomic)       — then, and only then, AckDurable
```

Every file is synced before anything is collected, so a barrier that fails on
the second file's sync has claimed nothing about the first either. Nothing may
be inserted between the commit and the ack: the commit is what makes the proof
true after a crash.

**The barrier does not build the runs.** `Commit` takes the drained *articles*,
and the store is the one place a run is ever constructed from them, inside that
same transaction. Deciding which articles form a run is derived state and it
gets one owner (Standing Rule 2) — but the reason is stronger than tidiness. A
drain is at-least-once (§3), so the drained set overlaps what is already
stored, and the dedup has to happen at **article** granularity **before**
grouping. Grouped first, a re-delivery of articles 5–9 arriving beside
genuinely new 10–12 forms one run `[5,12]` that no stored row covers, so no
whole-run check drops it; it inserts beside the stored row and `Σ length` then
exceeds the file's true size — a permanent false overlap finding on a healthy
file. Subtracting covered `art_idx` values first leaves `[10,12]`, which is the
truth. One owner, one order: **subtract, then sort, then group, then merge.**

**A failed barrier claims nothing** (R7). It acks no article and leaves the
stored rows wholly intact, because `RunStore.Commit` is atomic and is the last
thing that can fail before the ack.

### 3. A drain is at-least-once, and a report survives a failed sync

`SyncTarget.Drain` may re-report an article a previous `Drain` already returned,
and `RunStore.Commit` absorbs the duplicate (R12): an article whose `ArtIdx` a
stored row already covers is dropped before grouping, so it is never inserted
twice and never widens `Σ length`. See §2 for why that subtraction has to
happen at article granularity rather than per run.

`FileWriter` keeps two slices to make that true: `written` (reported by no
`Drain` yet) and `reported` (handed to a `Drain`, not yet confirmed by a
`Sync`). **Only `Confirm` discards `reported`**, and the barrier calls it solely
once the runs are committed and the articles are acked. A barrier that drains
and then fails — at the sync, the run commit, the ack, or the truncate —
re-reports on the next attempt.

Releasing on the `Sync` would cover only the first of those. The fsync makes the
bytes durable, but the commit and the ack still follow it, and a failure between
them left the retry with nothing to re-report while the bytes sat on disk
unacked — the file could then never complete for the life of the handle, because
a redelivery is dropped as a duplicate.

This is load-bearing rather than tidy. For a file still being written, losing a
report costs a re-fetch. For a **completed** file it costs bytes: the retry
drains nothing, so the bound `FinalizeFile` trims to sits below bytes that are
genuinely on disk, and the truncate destroys them.

The split between the two slices is what keeps an article written *between* a
`Drain` and its `Sync` from being discarded by that `Sync`: it is still in
`written`, which `Sync` does not touch.

### 4. The truncate bound is `max(offset+length)` over the runs, and only ever shrinks

`Barrier.FinalizeFile` trims a completed file to **the highest end offset any
run claims** — `max(Offset+Length)` over the file's stored rows *union* the
articles this call just drained and fsynced. Two quantities are easy to confuse
with it and the distinction is load-bearing:

| Quantity | What it is | Why it is not the bound |
|---|---|---|
| this run's high-water mark | the highest byte *this process* wrote | on a resumed file it sits below what earlier runs wrote; truncating to it discards them (#342, #350) |
| the file's first run, or its gapless prefix | the span from byte 0 to the first hole | stalls at the first permanently failed article; a 40 GB file with a hole at 2 GB would be cut to 2 GB, destroying exactly the blocks par2 repairs from |
| `max(offset+length)` over every run | the top of the highest run | **this is the bound** |

The bound is taken **before** the commit and over both sources, rather than
after it over one: the commit has to be the last thing that can fail before the
ack (§2), so a truncate placed after it would sit between the two statements
nothing may be inserted between — and one placed after the *ack* would leave an
untrimmed file behind on any crash, which par2's `QuickCheck` reads as a
missing file and works to reconstruct.

`FileWriter.Truncate` refuses a bound above the file on disk rather than clamping
(S6). Growing appends zeros, which asserts content that exists nowhere, and a job
with no par2 has no repair stage to notice.

**Both of the old `FinalizeFile` guards are gone, and this is S5 improving.**
They existed because the durable set and the fact log could disagree in either
direction: a recorded article that was not durable made the durable bound
destroy real bytes, and a durable article the fact log did not name made *both*
bounds walk past bytes that were simultaneously marked Done. Neither state is
reachable now. There is one record, written from the drained set after the
fsync, so "recorded" and "durable" are the same set by construction — there is
no second copy to drift. Under Standing Rule 2 that is the preferred shape:
where a check and an owner would both work, take the owner.

#### The whole-file CRC is a query, not a walk

A file's whole-file CRC **exists exactly when the file holds one row, that row
starts at offset 0, and it covers every article of the file**; its `crc32` is
the value. `crc32util.Combine` is
zlib's `crc32_combine` and is associative, so a run's CRC is built pairwise as
articles join it, across restarts — nothing reads the file.
`Application.recordAssembledCRC` threads the value to `Queue.SetFileCRC32FromRuns` when
the file finalizes.

**Its consumer is `par2.VerifyCRCs`, not `par2.QuickCheck`.** The two are easy
to conflate because the post-processing *stage* is named quickcheck.
`par2.QuickCheck` the **function** is filename relocation — it moves flat
downloads into the subdirectory paths a par2 manifest references — and where it
compares a checksum it computes one itself from disk (`tryMatchCRC32File` takes
a path). It never reads `AssembledCRC32`. What our value feeds is
`par2.VerifyCRCsWithOptions`, called from `stage_quickcheck.go` and from
`app.par2NeedsRecovery`, where it saves one read of each file.

What that verdict then buys is real and worth naming rather than
under-claiming: a `Clean` outcome makes `stage_repair.go` skip the par2
verify+repair subprocess entirely, and `par2NeedsRecovery` returning false
leaves the deferred recovery volumes unfetched. Neither is reachable for a file
this record supplies no CRC for — it reads `NoCRC`, and both take the
conservative branch.

**The predicate has three conditions: one row, at offset 0, covering every
article of the file.** All three, and each closes a shape the others do not.
Together they are `prefixWalk.consumedAll` restated in the vocabulary of runs —
*did the record account for every article of this file?* — which is the
guarantee this change had to carry across, and #387 is what it is for.

`Queue.SetFileCRC32FromRuns` owns the whole predicate. It takes the runs rather
than a `uint32` deliberately: a setter accepting a bare value cannot refuse a
wrong one, and the CRC's meaning is entirely a property of the record it came
from, so the value and its evidence arrive together or the invariant lives only
in its callers' comments.

**A row count, not a span.** An overlapping article is *written* rather than
refused (see below), and it abuts nothing, so it gets a row of its own. A file
whose articles tile `[0,1000)` into one merged row plus a displaced article at
`[450,550)` in a second row would satisfy a span-shaped predicate — a row does
start at 0, and its length does equal the maximum — and publish a CRC combined
from the *original* articles while foreign bytes occupy 450–550. par2 would then
match a manifest whose bytes are not what is on disk — and given the bypass
above, the repair stage is skipped on that verdict and `par2NeedsRecovery`
leaves the volumes unfetched, so nothing later looks.

**And article coverage, not geometry**, because the row count alone does not
reach the *exact-offset* duplicate. Two articles claiming one offset cannot both
be stored — `(job_id, file_idx, offset)` is the primary key — so
`mergeAdjacentRuns` drops one, and a **single row at offset 0** survives. A
length check against the file's size would be *inert* here: `FinalizeFile`
derives its truncate bound from `max(offset+length)` over the same rows, so
`size == Length` holds by construction. `Σ length` equals the size too, so the
overlap check below sees nothing either. Every condition stated in *bytes* is
satisfied, and the CRC would be published over articles whose bytes another
article has overwritten.

The coverage condition closes that exactly rather than heuristically. A dropped
entry removes an article index from the record entirely; no other article
carries that index, since `ArtIdx` is the manifest's unique global index; and a
merge extends a span only to `LastArtIdx+1`, never skipping. So the dropped
article is in no run's span, and no single run can cover the file's range.

A permanently failed article — interior *or* at the tail — fails the same
condition, and needs no exception for it. The record does not account for every
article, so no CRC is published, `QuickCheck` reads `NoCRC`, and the repair path
runs.

#### Overlaps are detected at completion, and `Σ length` has a blind spot

A run is built only from articles that abut exactly, so an overlapping article
never merges into one. It is still written, and the overlap is caught at
completion by comparing the recorded lengths against the file:

| | Meaning |
|---|---|
| `Σ length > stat size` | **definite overlap** — articles wrote over each other |
| `Σ length == stat size` | **no evidence of overlap** — not proof of a clean tiling; see below |
| `Σ length < stat size` | articles are missing or failed, the ordinary incomplete case |

**Record the gap rather than let a reader assume the middle row is a
guarantee.** `Σ length` is a sum, so an N-byte overlap and an N-byte hole
cancel and land on equality: the check reports *no evidence of overlap* on a
file holding both. The prefix walk this replaced compared adjacent extents
structurally and saw the overlap regardless; this arithmetic cannot.

Two things bound the loss, and neither is this check:

- A hole means a gap between rows, so such a file has **more than one row** —
  and no single run covers its whole article range. The whole-file CRC is
  withheld on either condition alone. The #387 outcome is closed structurally by
  a different guard, not by this sum.
- The file is incomplete either way, so par2 fetches recovery volumes and
  repairs both defects.

What is lost is a *warning* on a file the user is already told is incomplete.

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
a run naming articles outside the file's own range in the manifest
(`queue.runsCoverage`), an ack naming an index the manifest does not have. Those
fail loudly as ordinary errors (A2, R28). Routing them through the fault path
would blame storage for a numbering bug, which is the A1 conflation in reverse.

The **shape** of that class narrowed with the record. It used to include an
article the barrier could not place in a per-file durable bitmap — no file-local
ordinal, or a target reporting zero articles — and both of those questions went
with `SyncTarget.FileLocalOrdinal` and `ArticleCount`, because a run carries
`FirstArtIdx` and `LastArtIdx` directly and there is no bitmap to index. The
rule is unchanged; the sites it applies to are fewer.

### 6. The record is authoritative, gated on one `stat` — S4 INVERTED

**Read this section even if you know the old contract, because it says the
opposite of what it used to.** S4 used to read: *the stored record is never
authoritative, and where it disagrees with a recomputation from the bytes the
recomputation is correct by definition.* That is no longer true. **The record
is authoritative.** There is no recomputation left to lose to — `Resumer.recompute`
is deleted — and a reader who assumes one still wins will misread every
paragraph below.

What makes trusting it sound is *when* its content is written. The barrier is
the only thing that puts content there, and it does so only after an `fsync` it
performed, over articles a completed `Drain` reported, so nothing in the store
can assert bytes that were never written. (Several paths *delete* rows; none
can make one claim anything — see below.) That was not true of the record this replaced, which was appended
at decode with no ordering against the write at all (R2).

But trust needs a floor. If the partial file were deleted or replaced between
runs, a record believed absolutely would report most articles complete and
re-fetch only the remainder, producing a file with holes exactly where the
"done" articles were (#362). So there is one check per file, and **no reads**:

```
stat(path).size >= max(offset+length) over the file's runs
```

- **Satisfied** → every run is adopted whole. A file that is merely *longer* is
  the ordinary pre-allocated case.
- **Missing file, or shorter than the runs claim** → that file's runs are
  **DELETED** and it is downloaded again. This is the resumer's entire mutation
  budget.

#### S7 is NARROWED to size, and `ModTimeNs` is deleted — deliberately

S7's validity stamp used to be the pair `(size, mtime)`. It is now **size
alone**. This is a contract amendment with a reason, not a rewording, and it is
stated here so a reader who knows the pair can tell a decision from an
oversight.

The reason is what the **response** to a mismatch costs, not what the stamp
detects. A mismatch used to fall through to `recompute()`: the file was
re-read, the records were corrected, and the stamp cost **one read**. With
`recompute` deleted the only response left is discard-and-refetch, so the same
stamp would cost **the whole file**.

That inverts the guard's economics, because the two halves fail differently. An
mtime moves without a byte moving — a restore from backup, a copy that does not
preserve timestamps, a tool that touches the file — and each of those would now
trigger a full re-download of a file that is entirely intact. A size shortfall
cannot happen that way: it means bytes the record claims are genuinely not
there.

So `ModTimeNs` is gone from `SyncTarget.Stat` (now `Stat(fileIdx int32) (size
int64, err error)`), from `ResumeResult`, and from the barrier and resumer
plumbing that carried it.

**What this gives up:** in-place corruption that preserves the file's length is
no longer detected at startup. par2 detects and repairs it at completion, which
is the same answer §4 gives for an overlap and the same one the *"a bad article
costs only its own bytes"* rule gives generally.

#### The barrier is the only thing that puts CONTENT into the record

Under Standing Rule 2 this is the strongest result in the change, and it is
worth stating as an invariant rather than as a consequence.

The record used to have **two** writers. `Resumer.writeBack` committed "a
resume's own answer as the file's Class B record" — a recomputed result written
back over the row it disproved, and a missing file written back as the empty
result it produced. It was defensible (the startup sweep completes before the
downloader can dispatch, so no barrier is running), but it was a second writer,
and it needed a no-merge rule and a `(0,0)` sentinel stamp to work.

`writeBack` is deleted. `durability.Barrier` is the only thing that **inserts
or amends the content** of a `durable_runs` row, from `Run` and from
`FinalizeFile`, both inside the transaction that precedes the ack.
`durability.Resumer` can only delete, and only when the file on disk
contradicts the record. That asymmetry is what makes the record trustworthy
without reading a byte of it back.

**State the bound on CONTENT, not on the table.** Deletion is performed from
**five places outside the barrier's own merge**, and an earlier wording of this
section said "and nothing else writes it", which its own document then
contradicted where `SQLiteStore.Prune` appears in the memory budget:

| Deleter | When |
|---|---|
| `durability.Resumer` | a file shorter than its runs claim, or missing (§6) |
| `RunStore.DeleteJob` | a job leaving the queue, **or a retry re-parsing a manifest that changed shape** — `app.dropJobDurability` is reached from both |
| `queue.SQLiteStore.removeCorrupt` | a job whose manifest is gone, so nothing can interpret the indices |
| `queue.SQLiteStore.pruneDurabilityRows` | the crash-window backstop on every queue save |
| `history.Repository.delete` | a history entry going away for good |

A sixth deletes and is deliberately excluded from that count: `Commit`'s own
`deleteRows` removes exactly the rows it just read, inside the merge's
read-modify-write and inside the same transaction as the insert that replaces
them. It is part of writing content rather than a separate deleter, which is
why the count is stated as *outside the barrier's own merge* rather than bare.

None of the five can make the record *assert* anything — a delete only ever
removes a claim, which is S3's safe direction. So the content bound is the whole
of what §6's trust argument needs, and unlike the wider claim it is true.

This is the fifth `only`/`sole`/`nothing else` overclaim found on this branch,
and it survived four sweeps because it is phrased differently in every file.
**Enumerate before asserting one, and grep the CONCEPT rather than the wording
you happen to have used here.**

### 7. Absence of evidence is absence

S3. An article no surviving run covers is Outstanding. A missing file, a file
shorter than its runs claim, an article that was never recorded — all resolve
the same way, and none of them is an error.

This is why the startup sweep is **authoritative** rather than additive, which is
the fix for #362. `Store.RestoreJobProgress` derives `done` from the same runs
before any of this happens, so on the ordinary path the two agree. They diverge
in exactly one case, and it is the case that matters: the sweep stats each file
and deletes the runs of one that is too short, so it hands back a *smaller* set
than the restore installed. With only an additive entry point the earlier belief
always won, so a truncated or deleted partial finished as a complete file with a
zero-filled hole in it and no warning.

There are consequently two seeding entry points on `Queue`, and **they must not
be merged**:

| Entry point | Caller | Contract |
|---|---|---|
| `ReplaceFromRuns` | `Application.resumeAllJobs` (startup sweep) | **authoritative over the files it is passed** — sets *and clears* for those, and leaves a file absent from the slice entirely alone. The only caller that has just stat'ed the files and deleted the runs one of them contradicts. |
| `SeedFromRuns` | `Application.reevaluateStall` phase 3 | **additive** — only ever sets. Replaying an ack whose fsync already landed; it has stat'ed nothing. |

The union of the two contracts is either #362 (a stale bit outliving the check
that disproved it) or a stall recovery that throws away live acks.
`TestSeedFromRuns_StaysAdditive` and
`TestSeedFromCommittedRuns_DoesNotClearAnAckThisProcessMade` are the guards,
and they are the only tests in the repository that redden when the two are
merged.

The file indices are carried separately from the runs, and that is structural
rather than convenience: a file whose runs were **all** discarded contributes no
run at all, and would otherwise be indistinguishable from a file the sweep never
looked at.

`ReplaceFromRuns` never clears a permanently failed article: its bytes were
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
flight per job. `Drain` is **destructive**, so two concurrent barriers over one
file split its articles between them: one gets what the writer was holding and
the other gets none. Each then acks only its own half while both believe they
checkpointed the file, and whichever calls `Confirm` releases the reports the
other never saw — so those articles are neither acked nor re-reported, and only
a restart recovers them.

The lock is **per job**, not global: a barrier is a few dozen fsyncs, and one
job's slow mount must not park every other job's checkpoint. `FinalizeFile` takes
it too — it is a barrier by another name, same drain, same
`RunStore.Commit`.

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

R6 names five triggers, and the reload adds a sixth that R6 does not name. Five
of the six are implemented; the unimplemented one is listed so its absence is
visible rather than assumed:

| Trigger | Implementation | Bound |
|---|---|---|
| Time | `runCheckpoint`'s ticker → `checkpointAll` | `downloads.checkpoint_interval`, default **30s** (`constants.DefaultCheckpointInterval`) |
| Volume | `noteJobBytes` → `barrierKick` → `checkpointJob` | `downloads.checkpoint_bytes`, default **64 MiB** (`constants.DefaultCheckpointBytes`) |
| File completion | `Application.handleFileComplete` → `finalizeCompletedFile` → `Barrier.FinalizeFile` | per file |
| Clean shutdown | `Application.shutdownCheckpoint` → `checkpointAllShare` | `shutdownCheckpointTimeout` (10s) for the **whole sweep**, divided evenly among the jobs it visits |
| Downloader reload | `Application.ReloadDownloader` → `checkpointAllShare` | `reloadCheckpointTimeout` (10s), same division. See below — this is the one trigger whose *result* is consumed. |
| Pause | **not implemented as a trigger.** No code path runs a barrier on pause; a paused job simply stops writing, and its buffered bytes wait for the next interval tick or for shutdown. R6 names it and nothing satisfies it. | — |

### The reload trigger is the one whose coverage is load-bearing

Every other trigger may fail a job silently: the bytes stay on disk, the
articles stay Outstanding, and the next barrier picks them up. The reload
trigger is different, because `ReloadDownloader` follows it with
`Queue.ClearAllEmitted` — and clearing an Emitted bit hands the article back to
a downloader that is about to be pointed at a **different server set**.

An article the assembler had written but no barrier had acked would then be
re-fetched; if the new set cannot serve it, it is marked permanently failed
while its bytes sit on disk, and the inflated `failedBytes` can reach
`RepairNoCapacity` / `RepairBeyondCapacity`, both `Hopeless()`, aborting a job
whose file was never damaged. The disagreement is permanent — `markNotDone`
refuses a permanently failed article, and a restart re-applies the persisted
row. This was #417.

So `checkpointAllShare` returns the jobs it could **not** protect, and
`ClearAllEmitted` takes that set and withholds their Emitted bits.
`checkpointJob`'s bool answers "does this job hold written-but-unacked articles
that clearing Emitted would strand?" — which is not the same question as "did a
barrier run": a job with no open files ran none and is still safe, while a job
whose `OpenFiles` call *errored* may hold megabytes and is not.

Two consequences worth stating, because both are surprising:

- **The skip withholds the Emitted clear and nothing else.** `ClearAllEmitted`
  also un-fails articles the old downloader's teardown marked failed, and those
  two act on disjoint articles — `markFailed` clears `emitted` as it sets
  `failed`. Skipping both would leave a teardown failure permanent, trading one
  strand for another on the same inflated figure.
- **The resulting stall is only partly self-clearing.** A later barrier releases
  an Emitted bit when its article's bytes reach disk, but an article whose data
  died with the old downloader is never acked and needs a restart. The reload
  logs a warning naming the affected jobs for that reason.

The checkpoint remains **best-effort in coverage** — a budget expiry or a
storage fault still leaves a job unacked. What changed is that the caller now
knows which jobs those are instead of clearing them regardless.

The two bounds answer different failure shapes and neither subsumes the other.
The time bound is what limits rework on a slow link, where 30 seconds is a few
articles; the byte bound is what limits it on a fast one, where 30 seconds can be
a gigabyte. The barrier fires on whichever arrives first.

**Neither can be disabled.** `checkpointSettings` substitutes the default for a
zero or negative value. A barrier is the only thing that acks a downloaded
article *while the job is running*, so with checkpoints off a job makes no
visible progress and holds every article Outstanding until it stops.

**And with this design, the work IS re-fetched.** An earlier version of this
paragraph said it was not, and it was right about the shape it described: a
per-article record written at decode time named each region, so a resume could
re-read the file and recover bytes no barrier had covered. That record is gone.
A run exists only after the fsync that made it durable, so with no barrier there
is no record, and a restart returns every article of the job to Outstanding.
"Off" is not a slow startup; it is throwing the download away on every restart.
This is the same cost R3 prices for a crash, unbounded instead of bounded to one
checkpoint window.

Three details that have each been got wrong once:

- **The byte accumulator is retired by the run that EARNS it, and by nothing
  else.** The barrier reads its window before it runs and subtracts exactly that
  figure once the run succeeds, so an article written while the barrier is in
  flight belongs to the *next* window and survives the settle.

  Subtracting on success replaced a read-and-clear before the run plus a
  put-back on failure. That pair got the arithmetic right and the *window*
  wrong: between the clear and the put-back the job had no entry at all, so
  `jobsAtRisk` could not name it — and `jobsAtRisk` is what a reload consults
  when it cannot list open jobs, which is the wedged-mount case where a
  background barrier is most likely to be in flight and about to fail. The
  reload cleared that job's Emitted bits and #417 reproduced through a narrower
  door. Retiring only on success closes it by construction: there is no interval
  in which written-but-unacked bytes are invisible.
- **A dropped kick is not a lost kick.** `barrierKick` is a non-blocking send;
  the accumulator is not reset by `noteJobBytes`, so the next article re-raises
  it and the interval tick covers the job regardless.
- **`lastBarrier` stamps only a barrier that returned nil, and only one that
  actually ran.** "The barrier ran and failed" and "no barrier ran at all" are
  different facts. Folding the second into the first's nil-error case is how a
  job on a dead mount came to report a fresh stamp every 30 seconds — the exact
  inversion of what R26 asks that figure to distinguish. A job with no sync
  target likewise never reaches the settle, so its accumulator stands: zeroing
  it would report zero pending bytes beside a stale timestamp, two figures
  agreeing that nothing is at risk at the moment when everything is.

**`bytes_durable` and `bytes_pending` are not in the same unit**, and R26 asks
only that the rework window be *visible*, not that it be commensurable with the
durable total. `bytes_durable` comes from the job's progress —
`expected - failed - remaining` over NZB-declared, yEnc-**encoded** sizes, the
same unit as `size`/`sizeleft` beside it. `bytes_pending` accumulates
`len(data)` per accepted article: **decoded** bytes, the ones on disk, because
B1's volume bound measures rework at risk. Neither can move to the other's
unit. Reading `bytes_durable` from a sum over the durability record's lengths —
a decoded figure — is the substitution `docs/queue-lifecycle.md`
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
                    │     ├─ Barrier.FinalizeFile   (drain, sync, trim,
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
drained, synced, truncated, committed the runs and acked the articles, so
acting on the redundant second fsync's fault would race the completion it is
part of, and on a permanent errno would carry a 100%-complete, fully acked job
into history as failed.

That is **not** every entry path. `finalizeCompletedFile`'s defer also runs
after `app.barrier == nil`, after a nil sync target, and after the
assembler-stopped and not-in-`open` early returns; and `retryFinalize` reaches
it on a job whose runs were committed but never acked. On all of those the
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
   whose filename was never resolved is **skipped**, contributing neither a file
   index nor a run — no process ever opened a path for it, so there is nothing
   to have proved absent.
2. `durability.Resumer.Resume` — one `stat`, adopt-or-discard, per §6 above.
3. Collect the file's index, and the runs that survived the gate.

Then `Queue.ReplaceFromRuns` installs the finding. It is authoritative
**over the files it is given, and only those**: for each file named there an
article no surviving run covers goes back to Outstanding, and the job's derived
figures are recomputed so its reported health matches its per-article state.

`ResumeResult.Restart` needs no case of its own downstream: a discarded file
comes back with no runs at all, which already says "nothing here is recorded".
The file index is what says the sweep looked.

**The sweep does not read a single byte of any partial file.** That is the S4
inversion in operation, and it is the property a refactor is most likely to
break by accident — see §6.

### The resume deletes; it never writes

**There is no resume write-back.** `Resumer.writeBack` and `Resumer.recompute`
are both deleted, and the `Resumer` is no longer a second writer of the
durability record. See §6, which states the content-writer invariant; this section
records what the resume may do instead, and why the machinery the write-back
needed is gone with it.

The resume's whole mutation budget is `RunStore.DeleteFile`, and it is reached
in exactly two cases:

- **The file is missing.** Absence is the strongest disproof a resume can
  hold — not one article's bytes are on disk.
- **The file is shorter than `max(offset+length)` over its runs.** Bytes the
  record claims are genuinely not there.

Left standing, either row set survives the file: the assembler recreates and
pre-allocates the file, and the next start's gate compares against a file of
zeros that passes a check the real file would have failed. Deleting is what
stops the resurrection, and it is scoped to **one file** because a resume proves
nothing about the job's other files — it stat'ed one path.

Note what deletion is not: it is not a claim that the record was *wrong when
written*. The barrier wrote it after an fsync it performed. The file changed
underneath it, which is precisely what the gate exists to notice.

**A deletion is persisted before `Resume` returns**, and that is load-bearing
rather than tidy. Article resolution is derived from the runs on every
re-hydration, so a correction that lived only in memory would be undone by the
next eviction and re-promotion — which the sweep reaches without any
concurrency, since it calls `Stall` on a job whose other file faulted and
`Stall` pauses the job, evicting the manifest.

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

**Running only at startup is complete.** A job admitted later has no runs to
seed from, and a job's runs cannot change while it is not running — only a
barrier writes them, and a barrier runs only for a job with open files. So a job
promoted hours after startup is still correctly seeded by the sweep that ran
before it was promoted.

**A fault does not discard the files already resumed.** `resumeJobFiles` returns
what it gathered *before* the fault, `resumeAllJobs` seeds it, and only then
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
  Skipping it let #362 survive in that branch — nothing stats the file, so runs
  the file on disk no longer supports are never discarded, the restore derives
  Done from them again on the next start, and the file finalized over a hole. It
  also made `stallLost`'s own "restart gonzbd to resume this job from its
  recorded runs" unable to work.
- Not residency either — `JobPhase.IsResident` is also true for
  `PhaseProcessing`, and in those phases something other than the assembler owns
  the job's files: par2 repairs a file **in place**, unpack reads it, the move
  relocates it out of the download directory entirely. The property the sweep
  needs is *the assembler is the only writer of these files*.

A swept job that is **not resident** — every paused one — is hydrated for the
duration and evicted again, so residency is unchanged from outside.
`Application.resumeAllJobs` takes a hydrated clone through `SnapshotJob` to read
the manifest, and `Queue.ReplaceFromRuns` hydrates the live job itself to
apply the correction. Startup is when this is cheapest and safest: nothing else
holds a manifest and no article is being dispatched.

### The sweep also finishes a finalize a crash interrupted

`Application.completeStrandedFiles` runs per job, after `ReplaceFromRuns` and
only when the sweep raised no fault. For each file that is `FetchAlways`, has
**every** article resolved, and is **not** `Complete`, it trims the file to the
bound its runs imply (`durability.TrimToRuns`) and then hands it to
`Application.completeFinalizedFile`.

That state is what a crash between the barrier's commit and the following queue
save leaves behind, and accepted limitation 6 has the full argument for why
nothing else recovers it. Three things about the shape are load-bearing:

- **The trim earns the flag.** `Complete` means the finalize *ran*, and the
  truncate is the part of it the article bits cannot witness — `Barrier.Run`
  acks without truncating, only `FinalizeFile` truncates. So this pass does the
  trim rather than deriving the flag, and a trim that fails withholds the flag.
- **After `ReplaceFromRuns`, necessarily.** That call installs the Done bits
  this reads, and it clears `Complete` itself on any file whose bits it cleared.
  It is also after §3.4's gate, so a file found short has already had its runs
  discarded and cannot be trimmed to a bound derived from rows that are gone.
- **Not on the fault path.** The truncate is the only irreversible act in the
  whole sweep, and a job that raised a storage fault is about to be stalled. A
  file left stranded is no worse off than before this pass existed.

`Barrier.FinalizeFile` is not re-run here and cannot be: its first act is
`Truncator.Drain`, which answers `ErrFileNotOpen` and takes the early exit,
because nothing reopens a file during the sweep. What the pass needs is only the
trim — no drain (a fresh process has an empty write cache), no commit (no new
articles), no ack (the sweep has already re-set the bits). `boundOver` remains
the single owner of the bound rule; `TrimToRuns` and `FinalizeFile` differ in
how they *apply* it, not in what it is.

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
durable runs (§4), never from a high-water mark the assembler maintained — there
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
`write_cursor` column unusable. The durability question is answered elsewhere
entirely, by `durable_runs`, which the assembler neither reads nor writes.

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
    claim: the next `Drain` reports them, and the barrier records the run
    naming its CRC at that offset and acks it. Letting a later article overwrite
    the range makes that record unverifiable, and failing the incumbent as well
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
  that two segments claim one byte **offset** without asserting the post is
  malformed, because a redundant posting and a server-mangled `=ypart begin=`
  produce the same observation and yEnc checksums the payload, never the header.

  **This detects an exact shared start offset only, and it is one of two
  sources.** Two articles whose ranges overlap without sharing a start offset
  are invisible here — `acceptedAt` is keyed on the offset — and the later one
  overwrites the earlier's bytes. The durability layer catches that case
  instead, after both writes have landed, by comparing `Σ length` over the
  file's runs against the file's size: a sum above the size means two durable
  articles describe the same bytes (§4, which also records the blind spot in
  that comparison). It reports through
  `durability.PostAnomaly` on the barrier's return, and the app routes it to the
  same `job.Warning`, at most once per `(jobID, fileIdx)`. The latch is in
  memory, so that bound is per process: a restart raises each finding once
  more, which is what a user who restarted to fix something would expect.

  The two are not redundant, though the reason is narrower than "their cases
  are disjoint". Within one process they are: the assembler resolves its
  collision's loser permanently failed, so it never earns a durable bit and the
  walk never reaches it. Across a **restart** `acceptedAt` is empty, so two
  articles at the same offset can both be written and both become durable, and
  the barrier does then see an exact-offset pair — still not a double report,
  because the assembler is blind in exactly that window. What the barrier alone
  can see, in any window, is a range overlap sharing no start offset. Neither
  prevents the write; see #387 for what detection here does not cover.

  That exact-offset pair reaches the user by a **third** path, not by the sum
  above. `RunStore.Commit` must discard one of the two — the primary key admits
  one row per offset — and returns the discard as a `durability.Collision`,
  which the barrier renders as a `PostAnomaly` naming both articles and the
  contested offset. It has to come from the commit: the dropped row contributes
  nothing to `Σ length`, and once the commit lands the survivor is
  indistinguishable from a row that never had a rival, so no later pass over
  the stored rows can re-derive it. The report says the post is malformed and
  that par2 will repair the file; it does **not** say the file is corrupt,
  because this layer cannot tell the in-episode case (which completes *short*)
  from the cross-episode one (which completes *wrong*).
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
| **CancelJob** | `JobID=""`, `FileIdx=fileIdxCancelJob` (-1), `MessageID=jobID`, `disposition` | closes all open files for the job and *deletes* them under `DeleteFiles` or leaves them on disk under `KeepFiles` (draining the cache first, so a kept file holds every byte that arrived — written, not fsynced, so a crash can still lose them); tombstones the job in `cancelledJobs` and discards cached articles under **both**; closes `ackCh` |
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
Counting it claims nothing — a rejected article is never written, so no run
covers it and no run-derived truncate bound reaches past it. This is what a
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
| Durability rows | `durable_runs` and `failed_articles`, deleted per job by `deleteJobDurability`. `removeCorrupt` and `history.Repository.delete` delete rows too — see §6's *The barrier is the only thing that puts CONTENT into the record* for the full five, and do not read this row as an enumeration. Neither table has a foreign key to the queue, so nothing removes them implicitly. `SQLiteStore.Prune` is the backstop: on every queue save it deletes rows whose job is in neither `jobs` nor history-as-`Failed`, which catches a crash in the window between a job leaving the queue and its rows being deleted. The `Failed` exception is load-bearing -- a retry bounds `FinalizeFile`'s truncate with those rows. |

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
- **CancelJob** — closes open files and tombstones the job so subsequent
  articles are discarded. The `ackCh` synchronisation lets the caller delete the
  job directory the moment `CancelJob` returns.

  Whether the files themselves are deleted is the caller's `FileDisposition`.
  `DeleteFiles` unlinks each as it closes; `KeepFiles` drains the write cache
  into it and leaves it. `KeepFiles` does **not** `Sync`: nothing reads a
  removed job's files, so the fsync would only stall ingest for every other
  job on the single worker goroutine. The kept bytes are therefore
  page-cache-durable, not platter-durable, and no `durable_runs` record covers
  them. The tombstone is set either way — it gates admission
  for the whole job, including files never opened, and `openTargetFile` makes
  no queue-membership check — so keeping a job's bytes never makes its
  articles admissible again. What `KeepFiles` leaves is a partial: preallocated
  to the expected size with holes, and (since its caller has also removed the
  job) with no manifest or `durable_runs` record left to interpret it.
- **Shutdown (`Stop`)** — closes `stopCh`, waits for in-flight senders, drains
  remaining channel items, flushes the write cache, and closes all open files.
  Partial files are closed without firing `OnFileComplete`.

## Accepted limitations

These are known, deliberate, and **not** claims about correctness. They are
recorded here so the next reader does not mistake them for design.

1. **The startup sweep skips non-resident jobs.** `ReplaceFromRuns` needs a
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
   (`seedFromCommittedRuns`) logs and returns on failure, while phase 4 still
   delivers the completion. The result is a file marked `Complete` with some of
   its articles still Outstanding — `IsComplete` is file-based
   (`internal/queue/job.go`), so the two do not have to agree. The cost is wrong
   figures and a wasted re-fetch, not corruption or a short file. Recorded and
   unfixed.

4. **A file with a hole reports no whole-file CRC.** The claim exists exactly
   when a file holds ONE run, starting at offset 0, covering every article of
   the file (§4). A permanently failed article leaves a hole and is accounted
   for by no run, so both conditions fail and no whole-file value exists to
   record — `FileProgress.AssembledCRC32` stays zero, which is the documented
   "unavailable" value (#349), so `par2.VerifyCRCs` reads `NoCRC` and
   `par2NeedsRecovery` conservatively returns true for that file. This is the
   correct answer rather than a gap: a partial CRC recorded as the file's
   would report corruption for a file that is merely incomplete.

   Together those conditions are `prefixWalk.consumedAll` restated over runs,
   which is what carries #387's guarantee across from the walk this design
   deleted. Neither the row count nor the coverage check is that guarantee on
   its own — the row count misses the exact-offset duplicate, where one of the
   pair is dropped and a single row survives. §4 has both worked examples.

5. **`Σ length` cannot see a hole and an equal-sized overlap together.** A bound
   on the overlap check rather than a defect in it; §4 states it, its two
   bounding arguments, and what is actually lost.

6. **A crash between the barrier's commit and the following queue save strands
   a completed file, and the next start repairs it rather than the window being
   closed.** The two facts about a finished file survive a crash by *different*
   mechanisms, and only one of them is transactional:

   - **Article resolution** survives through `durable_runs`. The barrier writes
     those rows in the transaction that precedes the ack, and the startup sweep
     replays them through `SeedFromRuns`/`ReplaceFromRuns`, which re-sets the
     Done bits. The queue save plays no part in it.
   - **`Complete`** survives only through `job_files.complete`, written by the
     *next* queue save. Nothing in the durability record witnesses it.

   A crash between them leaves a file with every article resolved, no
   `Complete` flag, and nothing able to re-complete it: completion fires from
   `partsWritten == TotalParts` inside the assembler, that counter moves only
   when the assembler is handed an article to write, and on the next start
   every one of the file's articles is Done so none is dispatched. The job is
   then not dispatchable, not complete and not failed, and stays that way
   across restarts.

   **This is pre-existing and was merely made observable.** The shape this
   design replaced committed the same claim inside the same finalize, with the
   queue save equally following it; the window is neither new nor widened.

   **Do not close this by deriving `Complete` on load from the article bits.**
   It is the obvious fix — `JobProgress.recompute` already derives `Pending`,
   `BytesDownloaded` and `FailedBytes`, and Standing Design Rule 2 says a
   recomputable value should not also be stored — and it is wrong here, because
   `Complete` does not mean "every article resolved". It means **the finalize
   ran**: drained, fsynced, **truncated to the durable bound**, handle closed.
   Files are pre-allocated, so an un-finalized file carries trailing zeros, and
   `Barrier.Run` acks articles *without* truncating — only `FinalizeFile`
   truncates. So the bits under-determine the flag, and a derived `Complete`
   would send untrimmed files into post-processing, which QuickCheck reads as a
   missing file (§3.5) and works to reconstruct. Within this window the truncate
   has in fact already run — `FinalizeFile` truncates before it commits and acks
   — but nothing in the bits distinguishes that file from one whose finalize has
   not started.

   **The repair is `Application.completeStrandedFiles`**, described under
   *Which jobs the sweep covers*. `FinalizeFile` is not re-run — its first act
   is `Truncator.Drain`, which answers `ErrFileNotOpen` and takes the early
   exit — but the pass needs only one step of it, *trim this path to the bound
   its runs imply and fsync it*, and everything after that is
   `Application.completeFinalizedFile`, which was **already** split out for the
   in-process form of this same interruption: a stall between "bytes correct"
   and "file marked complete", resumed from exactly that boundary by
   `reevaluateStall`. The crash case is the same interruption with the process
   gone in between.

   **The window itself is still open, and that is why this stays a limitation.**
   The repair runs at the *next start*, so a crash still leaves a wedged file
   until then. That is indistinguishable from atomicity in practice — the wedge
   only manifests across a restart and the repair also runs at restart — but it
   is recovery, not prevention, and a reader must not take the entry above as a
   claim that the two writes are one.

   Two further residuals, both narrow:

   - A trim that fails leaves the file stranded exactly as before, because the
     trim is what earns the flag. It is logged, not stalled: the file is no
     worse off than before the pass existed, and stalling a job over an optional
     repair would turn a recoverable wedge into a paused job.
   - The pass is skipped for a job whose sweep raised a storage fault. The
     truncate is the only irreversible act in the sweep and the device has
     already refused one read.

   **The alternative considered and not taken** was folding `Complete` into the
   barrier's own transaction, so it lands with the runs and a restart
   reconstructs it from the same authority. That would close the window
   atomically rather than by repair, but it needs a schema change and makes the
   barrier write state `internal/queue` owns — the line `durable_runs`' own doc
   draws when it says `failed_articles` is written solely by the queue.

7. **An exact-offset collision is PREVENTED only within one open-file episode;
   across a boundary it is detected and reported after the fact.**
   `FileWriter.acceptedAt` maps each byte offset to the article that owns it,
   and a second article claiming an owned offset is refused and resolved
   permanently failed — which works because that article is not yet `Done`, and
   `markFailed` early-returns on one that is. The file then completes *short*.
   That map lives on the `FileWriter`, so it is forgotten when the file closes:
   a **restart**, or a **retry** of a failed job whose file was incomplete,
   reopens the file with an empty map, and the later write overwrites the
   earlier. The file then completes *wrong*.

   **The bound is that both outcomes are diagnosed and repairable.** Across the
   boundary `RunStore.Commit` must discard one of the two rows, returns it as a
   `durability.Collision`, and the barrier raises a `PostAnomaly` naming both
   articles and the contested offset (§4). The whole-file CRC is withheld either
   way, because the record cannot cover the discarded article's index. So par2
   runs in both cases; what differs is a short file versus a wrong one, and a
   failed-byte figure that is correct versus one that omits the loser's bytes.

   **Do not try to close this by rehydrating `acceptedAt` from `durable_runs`.**
   It cannot be done: a `Run` is a *merged* span carrying `FirstArtIdx`,
   `LastArtIdx`, `Offset` and `Length`, and merging destroys the per-article
   boundaries — a row saying "articles 0–199 occupy bytes [0,20000)" cannot say
   where article 137 begins. Deriving the boundaries by walking the manifest's
   article lengths assumes articles lie contiguously in `ArtIdx` order, which is
   exactly what a malformed post violates; the derivation would be correct for
   every file that does not need it. Persisting ownership separately means
   re-introducing a per-article record, which is what this design removed and
   for the reason that two per-article records with independent writers could
   disagree (#389, #421).

   **What could work, if it is ever worth it**, is a weaker question the merged
   record *can* answer: for an incoming article at offset `O` with index `A`,
   against a stored run `R`, treat `O ∈ R`'s byte span with `A` outside
   `[R.FirstArtIdx, R.LastArtIdx]` as a collision — two different articles
   claiming the same bytes — while `A` inside that span stays the ordinary R12
   redelivery. It needs no schema change and fits the injected-callback seam
   `openTargetFile` already uses for `FileInfo`. It costs a blocking `ForFile`
   read on the assembler's write path at each file open, requires rewriting the
   #342 note at `assembler.go` that currently reads as a blanket prohibition on
   seeding the open path, and still misses an article durable but not yet
   committed when the episode ended.

   Not scheduled, because after #387's fix both outcomes reach par2 with a
   warning naming the file. **The case that would change that is a post with no
   par2**, where "repairable" is false and short-versus-wrong is the difference
   between a hole and an unusable file — `AGENTS.md` Standing Rule 3's case that
   needs the bound most. How common those are in practice is the open question.

8. **The crash suite does not test fsync-to-platter.** See below.

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
  `DurableProof`, and the SQLite `RunStore` behind `durable_runs`.
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
- Authoritative startup sweep (`resumeAllJobs` → `Queue.ReplaceFromRuns`) and
  the additive stall-recovery replay (`SeedFromRuns`).
- Repair for a finalize a crash interrupted (`completeStrandedFiles` →
  `durability.TrimToRuns` → `completeFinalizedFile`), which trims the file to
  its runs' bound and then completes it. See *Accepted limitations* #6 for the
  window it repairs and the residuals it leaves.
- Storage-fault stall/fail with a surfaced, actionable reason and interval-based
  re-evaluation.
- `durable_runs` and `failed_articles` tables (migrations `002`/`003`), which
  replaced `article_facts`, `file_extents` and `job_files.articles_done`;
  `job_files.max_written` and `write_cursor` removed earlier.
- The whole-file CRC threaded to `Queue.SetFileCRC32FromRuns` by
  `Application.recordAssembledCRC`, for a file that collapses to one run
  accounting for every one of its articles.
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
