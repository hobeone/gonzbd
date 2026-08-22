# Single Durability Record — Design

**Issue:** #423. Supersedes #389 and #421. Carries #422 as an independent commit.

**Status:** Direction settled. This document is the argument the implementation plan
is written against.

## 1. The problem

Every downloaded article is described twice, by two writers, at two times.

| | Class A `ArticleFact` | Class B `FileExtent` |
|---|---|---|
| Written by | `pipeline.handleSuccessResult`, at decode | `Barrier`, at the checkpoint |
| Ordered against the write | **not at all** (R2) | strictly after the `fsync` (S1) |
| Asserts | *if* the bytes are present, they hash to `CRC32` | the bytes **are** present |
| Store | `article_facts` | `file_extents` |

Because the writers are independent, the two records can disagree, and every
disagreement is a defect rather than a discrepancy:

- **#389** — `pipeline.go` appends the fact before `assembler.WriteArticle` is
  called. An article the assembler then rejects or displaces holds a record with
  no durable bit. `recordedExtent` counts it in `missing` and lets it set `high`,
  and `FinalizeFile` promotes that to the truncate bound.
- **#421** — the same append site. `offsetOutOfRange` rejects *after* the append,
  so a bogus `=ypart begin=` offset is recorded permanently. `INSERT OR IGNORE`
  then makes the one mechanism that would correct it — re-fetching the article —
  the exact mechanism that is ignored.

`FinalizeFile` already carries two guards for this (`missing > 0` swaps to the
recorded bound, `unrecorded > 0` declines to truncate at all). Neither removes
the class; both choose a least-bad bound once it has occurred. Adding a third is
a third enforcement point for one invariant, which is the smell Standing Design
Rule 2 names.

## 2. The change

`WrittenArticle` is already `ArticleFact` minus the CRC, and its own doc states
the relationship:

> It is a *Written* article, not a Durable one — the barrier is what closes that
> gap, and until its fsync returns nil this struct asserts nothing about stable
> storage (S1, S2).

The change is to close that gap **with the record**:

1. `WrittenArticle` gains `CRC32`. The decoder's CRC travels
   decoder → `WriteRequest` → `FileWriter` → `Drain`.
2. The barrier writes the record in the transaction that already commits the
   extent. `pipeline.appendArticleFacts` and its call site are deleted.
3. `INSERT OR IGNORE` becomes last-write-wins.
4. The queue's `done` bit stops being persisted and is rebuilt from the records
   at load. `failed` stays persisted.

`buildExtent` at `internal/durability/barrier.go:383-387` currently carries the
comment that is exactly negated by this change:

> The barrier does NOT write ArticleFacts. Class A is appended by the writer when
> the article is decoded, with no ordering against the data (R2) — that
> independence is what lets Class A be committed without a barrier at all.
> Writing facts here would make them barrier-ordered and quietly destroy the
> property.

That comment marks the seam. The record write replaces it.

## 3. Decisions

### 3.1 Last-write-wins replaces R1 immutability

**Ruled: adopt.** This is a second writer to an append-only store, which the
Decision Protocol makes an escalation; it was escalated and ruled on.

R1 guarded a record written *before* the write, which could describe bytes that
were never written — "whether from a buggy path or a hostile server". A record
written after a completed `fsync` can only describe bytes the assembler accepted
through `offsetOutOfRange` and actually wrote, because `Drain` reports only
articles "whose bytes the target handed to the OS without error". A redelivery
cannot make the record assert anything the write did not.

So the threat R1 defends against is **unreachable** rather than rare, and the
security carve-out in `AGENTS.md` does not apply: the value cannot do anything
the write did not already do. The carve-out's own test is what the value could
*do* — this one reaches a truncate bound and a CRC, not a protocol, a path, a
query, or a command line.

It is also what makes the re-fetch path self-healing. A bogus offset recorded on
a first attempt is replaced by the correct one when the article is re-fetched.
Under `INSERT OR IGNORE` that repair is impossible by construction, which is the
whole of #421.

**The hazard it closes.** `resetForReload` clears `done` and `failed` for every
failed article on every process start and every reload
(`internal/queue/progress.go:767-778`), and an article that is both failed and
durable is reachable because `internal/durability/resume.go:373` sets the durable
bit purely on a CRC match, consulting nothing about failure. Under
`INSERT OR IGNORE` a re-fetch of such an article at a different offset would keep
the stale row — which, once the row *is* the durability claim, asserts bytes at
an offset they are not at. Last-write-wins makes that row correct instead.

### 3.2 Two types, not one

`WrittenArticle` and the persisted record have identical fields after this
change, which invites merging them. **They stay separate**, and the barrier is
the only converter.

The distinction is the one the whole design turns on: a `WrittenArticle` reached
the OS, a record survived an `fsync`. Collapsing them into one type makes it
expressible to pass a `Drain` result where a durable record is expected, which is
the same class of mistake `DurableProof` exists to make unrepresentable. Two
types with identical fields and one converter is not the "two constructors"
smell — the smell is two paths populating *one* type, which is what
`newManifest`/`UnmarshalJSON` did.

`ArticleFact` is renamed to `DurableArticle`. The old name was accurate while the
claim was conditional; it is not now. The rename is mechanical and forces every
call site to be re-read, which is wanted given the semantic change.

### 3.3 The queue keeps the push; only the persisted copy goes

**Ruled: keep `AckDurable` and `DurableProof`.**

`done` is already set only by `Queue.AckDurable`, which takes a `DurableProof`
whose fields are unexported with no exported constructor, so nothing outside a
completed barrier can mint one (`internal/queue/workset.go:39-76`,
`internal/durability/proof.go:14-26`). What changes is only that the bitmap stops
being serialized into `jobProgressJSON` and is rebuilt from the records at load.

Deleting the ack entirely and having the queue read the records was considered
and rejected: `CountUnfinishedArticles` sits on the dispatch path, so the bitmap
must remain an in-memory cache either way, and something must still invalidate it
when the barrier commits. That something is `AckDurable` under another name, and
the inversion would additionally reverse the queue-to-durability dependency
direction for no gain.

**`done` is not a durability flag.** `markFailed` sets `done` *and* `failed`,
deliberately, so a permanently-failed article is excluded from
`CountUnfinishedArticles` and the file still finalizes. The derivation on load is
therefore:

```
unfinished(art)  ==  !hasRecord(art) && !failed(art)
```

not `done == hasRecord`. Getting this backwards leaves every failed article
outstanding forever and no file ever completes.

`BytesDownloaded` stays persisted. It is derivable by summing record lengths, and
Standing Rule 2 arguably reaches it, but it feeds the byte-bound checkpoint
trigger and widening the change into progress accounting buys nothing this issue
is about.

### 3.4 The assembler's part accounting does not move

An earlier framing of this issue named moving `partsWritten`, `seenDone` and
`seenFailed` to the barrier as the largest piece of work. **It is withdrawn.**

`partsWritten` triggers *file* completion, which triggers the barrier
(`app.go:1243` — `handleFileComplete` calls `finalizeCompletedFile`). Moving it
into the barrier would make the barrier both the trigger and the consumer of the
same event. Article completion is already post-`fsync`; file completion is a
different event and stays in `FileWriter`.

A rejected or permanently-failed article counts toward `TotalParts` by design
(`internal/assembler/assembler.go:1418-1430`) — declining to count it means
`partsWritten` never reaches `TotalParts`, `OnFileComplete` never fires, and the
job sits at 100% with zero outstanding articles across restarts. That stays true
and is untouched.

## 4. Invariants

| Rule | Before | After |
|---|---|---|
| R1 — Class A immutable | `INSERT OR IGNORE`; a second delivery must not redescribe | **Deleted.** Last-write-wins; the newest `fsync` is the newest truth about disk |
| R2 — no ordering against the write | Class A may be committed before, during, after, or when the write never happens | **Deleted.** The record exists only after a completed `fsync` |
| Class A asserts | nothing about presence | presence, and the CRC of what is present |
| S1, S2 | Class B strictly after the `fsync` | unchanged, and now the only claim there is |
| S4 — recomputation wins over the stored record | the durable bitmap is rebuilt from Class A plus a disk read | unchanged; the record is still what `Resumer` reads |
| S5 — no second copy to drift | two records, reconciled by two guards | one record; the guards become unreachable |
| S6 — truncate only shrinks | unchanged | unchanged |
| R3 — losing a suffix costs a re-fetch | applies to Class A | applies to the merged record, and is now the *routine* cost after an unclean shutdown rather than a rare one |

### What this costs

Class A is currently the crash-recovery mechanism for Class B. `Resumer.recompute`
(`internal/durability/resume.go:280-296`) rebuilds the durable bitmap from the
records plus a disk read; Class B is written back, never read as authority. A
record appended only after the extent commit is unavailable in the
`fsync` → commit window, so bytes that reached disk in that window are no longer
**provable** on resume and are re-fetched instead of trusted.

The contract already prices this: Class A's own row reads *"Losing a suffix costs
a re-fetch (R3)"*, and the append already runs on a `context.WithoutCancel` copy
with a 5s timeout precisely because *"a fact lost to that race makes a resume
unable to prove bytes that are on disk."* This change makes a rare race into an
ordinary one.

The window is bounded by the checkpoint cadence — 30s or 64 MiB, whichever comes
first (`internal/constants/limits.go`) — and three events shrink it further: file
completion, a clean shutdown, and the byte-bound kick. Nothing widens it; a
dropped `barrierKick` does not reset the accumulator. **A paused job runs no
barrier at all**, so its buffered bytes wait for the tick.

So: at most one checkpoint's worth of re-download per job per unclean shutdown,
in exchange for a record that cannot contradict itself. A re-fetch is loud,
bounded, and recoverable by a mechanism the system already has. The defects it
replaces are silent.

## 5. Why the storage-fault objection does not apply

Appending at accept time was rejected for #389 on the grounds that a SQLite write
on the assembler's worker goroutine sits where `jobSyncTarget.submit` reads
"worker did not answer in 5s" as evidence about storage — so ordinary write
contention could mint a storage fault and park a healthy job (A1).

That objection does not reach this design. `b.exts.Commit` runs on the barrier's
own goroutine (`internal/durability/barrier.go:272` and `:769`); it is not a
`syncOp`. Only `Files`, `Drain`, `Sync`, `Stat`, `Truncate`, `Confirm` and
`Close` cross into the assembler worker under `barrierOpTimeout`. An existing
off-worker write gains a payload; no new write lands on the worker.

## 6. Ordering inside the barrier

`Barrier.Run`'s phase 4 carries a constraint the record write must respect:

> Phase 4 — commit Class B atomically, then and only then ack. Nothing between
> these two statements may fail, and nothing may be inserted between them: the
> commit is what makes the proof true after a crash.

The record write therefore goes **inside** the commit, not beside it: one
transaction writing both the records for this cycle's drained articles and the
extents built from them. A separate `Append` call before or after `Commit` would
be a second failure point between the fsync and the ack, which is what that
comment forbids.

This has a consequence for `buildExtent`. It computes `verifiedPrefix` over
`facts` loaded from the store, and today this cycle's articles are already in
that set because they were appended at decode. After the change they are not, so
`buildExtent` must walk the union of the stored records and this cycle's drained
articles. That merge is the real work in this file.

## 7. What does not change

- File completion, `TotalParts`, and the rejected-article counting rule.
- Progress reporting cadence, which is already checkpoint-driven.
- DirectUnpack, which is already fed by `completeFinalizedFile` *after*
  `finalizeCompletedFile` has run the barrier, drained, synced and truncated.
- The truncate itself. `FileInfo.ExpectedSize` is the NZB's declared *encoded*
  byte count and yEnc inflates ~2%, so every file is pre-allocated oversized and
  trimmed on completion; QuickCheck gates on an exact size match, so an
  untruncated file also loses the par2 bypass. The bound stays exact-or-short and
  never long, because every article with bytes on disk is drained, fsynced and
  recorded in one transaction.

## 8. #422, carried separately

`RetryHistoryJob` deletes the durability rows the retry needs. `history.Repository.Delete`
drops both tables unconditionally (`internal/history/repository.go:358-363`), and
`RetryHistoryJob` calls it at `internal/app/app.go:1840` — nine lines *after*
`queue.Add` at `:1831`, so records the re-enqueued job appends in that window die
too.

This is **not** an instance of the two-record class and is not fixed by anything
above: with one table the same loop drops that one identically, and the retry's
progress comes from `history_job_files` via `RestoreRetryProgress`
(`internal/queue/sqlite_store.go:1030-1075`), never from the records. It is
carried here by explicit scope decision, as its own commit, because it is
severity:high live data loss on the recovery path this design's failure mode
depends on.

## 9. Open questions

Carried into the plan's discovery contract rather than settled here.

1. **Where the CRC lives between the write and the drain.** `FileWriter` must
   retain one `uint32` per written-but-undrained article. Whether that fits an
   existing per-article structure or needs a new map is not established.
2. **Coalesced-run partial writes.** `flushRun` fails every article in a run on
   any write error, and `WriteAt` can return `ErrShortWrite` having written
   leading bytes. Those bytes are on disk with no record. Expected to be harmless
   because they sit above every good record and the bound is a `max`, but not
   proven.
3. **`retryFinalize` against an evicted job** (`internal/app/stall.go:484-532`) —
   no manifest and no `SyncTarget` exist there. The record write must have an
   answer for that path.
4. **Rebuilding the bitmap at load** — one query per job at startup, over
   `article_facts`. Cost against a 20k-article job is not measured.
5. **`jobProgressJSON`'s comment is already stale** — it says the done bit "is set
   only once the bytes have reached WriteAt (#355)", which contradicts
   `statusinfo.go:200-207`. It must be swept, and the sweep must not simply
   inherit the wrong claim.
