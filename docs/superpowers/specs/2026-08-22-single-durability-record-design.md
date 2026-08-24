# Durable Runs — Design

**Issue:** #423. Supersedes #389 and #421. Carries #422 as an independent commit.

**Status:** Direction settled. This document is the argument the implementation
plan is written against.

> **Second draft.** The first version of this document collapsed Class A and
> Class B into one record *per article*, keeping the per-article offset and CRC
> and therefore keeping the entire contiguity apparatus that consumes them —
> `verifiedPrefix`, the abutment walk, `durableAt`, `overlapAnywhere`, and a
> five-consumer union with dedup and ordering requirements. Four review rounds
> hardened that plan without ever asking whether the record needed to be
> per-article at all. It does not. The unit of recording should be the unit of
> writing: a **run** of contiguous articles that were fsynced together.

## 1. The problem

Every downloaded article is described twice, by two writers, at two times.

| | Class A `ArticleFact` | Class B `FileExtent` |
|---|---|---|
| Written by | `pipeline.handleSuccessResult`, at decode | `Barrier`, at the checkpoint |
| Ordered against the write | **not at all** (R2) | strictly after the `fsync` (S1) |
| Asserts | *if* the bytes are present, they hash to `CRC32` | the bytes **are** present |

Because the writers are independent, the records can disagree, and every
disagreement is a defect:

- **#389** — `pipeline.go` appends before `assembler.WriteArticle` is called, so
  an article the assembler rejects holds a record with no durable bit.
- **#421** — the same site. `offsetOutOfRange` rejects *after* the append, so a
  bogus `=ypart begin=` is recorded permanently, and `INSERT OR IGNORE` makes
  re-fetching — the one mechanism that would correct it — the exact mechanism
  that is ignored.

`FinalizeFile` carries two guards for this. Neither removes the class; both
choose a least-bad bound once it has occurred.

## 2. The change

**One record, written only after the `fsync` that makes its bytes durable, and
describing a contiguous run of articles rather than a single one.**

```
durable_runs(job_id, file_idx, first_art_idx, last_art_idx, offset, length, crc32)
```

One row per run of articles that abut in **both** byte offset and article index
and were made durable together. Adjacent rows **merge**: combine the CRCs, sum
the lengths, widen the index range.

A file whose articles arrive in order collapses to a single row — offset 0,
length equal to the file's true decoded size, `crc32` equal to the whole-file
CRC.

### Why a run rather than an article

`crc32util.Combine` is zlib's `crc32_combine` and is associative, so the CRC of
`A ++ B ++ C` can be built pairwise in any grouping. The whole-file CRC has
always been computed this way — `prefix.go:258` combines per-article CRCs and
never reads the file. What `verifiedPrefix` adds is only the *proof that the
pieces abut*, re-derived by walking every article on every barrier.

Recording runs moves that proof to write time, where it is a single comparison,
and makes it durable. The walk disappears rather than being made correct.

The alignment is already in the code: `flushRun` coalesces contiguous articles
into one `WriteAt` because that is the efficient way to write them. That same
run is the efficient way to record them.

### What each field is for

| Field | Purpose |
|---|---|
| `first_art_idx`, `last_art_idx` | which articles this run accounts for — the resume set is the complement |
| `offset`, `length` | the truncate bound is `max(offset+length)`; the integrity check is `Σ length` |
| `crc32` | combined over the run; when one row spans the file it *is* the whole-file CRC |

## 3. Decisions

### 3.1 Last-write-wins replaces R1 immutability

**Ruled: adopt.** A second writer to an append-only store is a Decision Protocol
escalation; it was escalated and ruled on.

R1 guarded a record written *before* the write, which could describe bytes that
were never written — "whether from a buggy path or a hostile server". A record
written after a completed `fsync` can only describe bytes the assembler accepted
through `offsetOutOfRange` and actually wrote, because `Drain` reports only
articles "whose bytes the target handed to the OS without error". A redelivery
cannot make the record assert anything the write did not.

The threat R1 defends against is **unreachable** rather than rare, so
`AGENTS.md`'s security carve-out does not apply: the value reaches a truncate
bound and a CRC, not a protocol, a path, a query, or a command line.

Merging is itself read-modify-write, so append-only was going to go regardless.

### 3.2 An out-of-range offset fails permanently and immediately

**Ruled: keep current behaviour.** It is a verdict about *content*, not about a
server. The article downloaded and decoded cleanly; its yEnc header claims an
implausible position, and another server asked for the same Message-ID will
answer the same way. Spending the whole try-list on it costs connections to
reach a conclusion already reached.

### 3.3 Overlaps are detected at completion, not refused at accept

A run is built only from articles that abut exactly, so an overlapping article
never merges into one. It is still **written**, and the overlap is caught at
completion by comparing the recorded lengths against the file:

| | Meaning |
|---|---|
| `Σ length > stat size` | **definite overlap** — articles wrote over each other |
| `Σ length == stat size` | **no evidence of overlap** — not proof of a clean tiling; a hole and an equal-sized overlap cancel here |
| `Σ length < stat size` | articles are missing or failed, which is the ordinary incomplete case |

Only the first is a positive signal, and it is enough for the case that matters:
a file which *looks* complete while holding a hole.

It is not a complete overlap detector, and the middle row says why. `Σ length`
is a sum, so an overlap of N bytes and a hole of N bytes cancel and land on
equality. The old prefix walk compared adjacent extents structurally and saw the
overlap regardless; this check cannot. Two things bound the loss. A hole means a
gap between rows, so such a file has more than one row and §3.5 withholds the
whole-file CRC on the **row count** — the #387 outcome is closed by a different
guard, structurally, not by this arithmetic. And the file is incomplete either
way, so par2 fetches recovery volumes and repairs both defects. What is lost is
a warning on a file the user is already told is incomplete.

A first draft of this section had overlapping articles **refused** at accept
time, the way an out-of-range offset is. That was over-engineered, and it was
also not buildable as described. `acceptedAt` — the existing collision index —
is `map[int64]offsetOwner` with `offsetOwner{id, written}`, keyed on **start
offset** and carrying no length. It answers "does another article already own
this exact offset", which is #383's question, and cannot answer "does
`[off, off+len)` intersect anything already written". Refusing partial overlaps
would need a second, range-shaped index of the same fact — a second source of
truth for a case the completion check already catches.

So the existing exact-offset collision handling stays exactly as it is, and
nothing new is added at accept time.

The consequence of *not* refusing is contained: an overlapped file keeps more
rows, never collapses to one, and so never publishes a whole-file CRC. That is
the correct outcome for a file whose bytes are in doubt.

### 3.4 The resume trusts the record, gated on one `stat`

**Ruled: trust the log.** No per-region CRC verification at startup.

But trust needs a floor. If the partial file were deleted or replaced between
runs, a log believed absolutely would report most articles complete and re-fetch
only the remainder, producing a file with holes where the "done" articles were.

So one check per file, no reads: **`stat(path).size >= max(offset+length)`**. A
missing file, or one shorter than the records claim, discards that file's records
and re-downloads it. A file that is merely *longer* is the ordinary
pre-allocated case.

**This inverts S4.** The contract currently says a recomputation from the bytes
is correct by definition and the stored record is never authoritative. After this
change the record *is* authoritative, and only its size is checked. That is a
deliberate trade of an in-place-corruption check for a startup that does no I/O,
and the contract must state it rather than inherit the old claim.

**It also narrows S7 to size alone, and `ModTimeNs` is deleted outright.** S7's
validity stamp is today the pair `(size, mtime)` — `synctarget.go:138-142` says
so, and `resume.go:131` is where it is checked:

```go
if ok && ext.Size == fi.Size() && ext.ModTimeNs == fi.ModTime().UnixNano() {
```

The mtime half is not merely redundant after this change; it becomes actively
harmful, and the reason is what the *response* to a mismatch costs. Today a
mismatch falls through to `recompute()` — the file is re-read and re-hashed, and
the records are corrected. The stamp costs **one read**. `recompute()` is
deleted by this section, so the only response left to a mismatch is to discard
the file's records and re-fetch. The same stamp would then cost **the whole
file**.

That inverts the guard's economics. An mtime can change without a byte changing
— a restore from backup, a copy that does not preserve timestamps, a tool that
touches the file — and each of those would trigger a full re-download of a file
that is entirely intact. A size shortfall cannot happen that way: it means bytes
the records claim are genuinely not there.

So `ModTimeNs` goes from `SyncTarget.Stat` (which becomes `Stat(fileIdx int32)
(size int64, err error)`), from `ResumeResult`, and from the barrier and resumer
plumbing that carries it. `FileWriter.Stat`'s second return value has exactly
one consumer — `assembler/synctarget.go:543`, feeding the interface method above
— so the whole vertical goes with it.

**What is given up:** in-place corruption that preserves file length is no longer
detected at startup. par2 detects and repairs it at completion, which is the same
answer §3.3 gives for an overlap and the same one the *"a bad article costs only
its own bytes"* rule gives generally. This is a **contract amendment, not a
cleanup** — S7 is a numbered rule, and it is listed among the escalations
alongside the S4 inversion rather than buried in an implementation step.

### 3.5 The whole-file CRC is a query, not a walk

It exists exactly when the file has **exactly one row**, and that row starts at
`offset == 0`. Its `crc32` is the value. There is no prefix walk, no
`VerifiedTo`, no `HasPrefixCRC` sentinel.

**The predicate is a row count, not a span.** An earlier draft of this section
said instead that some row must have `offset == 0` and `length ==
max(offset+length)` — which reads as the same rule and is not. §3.3 *writes* an
overlapping article rather than refusing it, and gives it its own row. So a file
whose articles tile `[0,1000)` into one merged row, plus a displaced article
sitting at `[450,550)` in a second row, satisfies the span form: a row does
start at 0, and its length does equal the maximum. Under that predicate the CRC
is published — combined from the *original* articles, while foreign bytes
occupy 450–550. par2 then matches a manifest whose bytes are not what is on
disk, and the recovery volumes that would have repaired it are never fetched.

That is #387, which `prefixWalk.consumedAll` exists to prevent and which this
change deletes. The row-count form is what carries #387's guarantee across: a
second row means bytes this record cannot account for, whatever its span. It is
also what §3.3's closing sentence already asserts — *"an overlapped file keeps
more rows, never collapses to one, and so never publishes a whole-file CRC"* —
so the two sections now state one rule rather than two that agree only on the
cases anyone happened to imagine.

Its consumer is `par2.VerifyCRCs`, which compares an assembled CRC against the
par2 manifest "without re-reading files from disk". What the CRC saves directly
is one read of each file during verification.

**It IS a par2 bypass, and an earlier draft of this section denied it in the
course of correcting a different error.** That draft was right that
`par2.QuickCheck` — the *function* — never consumes our value, and wrong to
conclude from this that "par2 still runs". The chain is:
`stage_quickcheck.go:136` feeds `p.FileAssembledCRC32(fi)` into
`par2.VerifyCRCsWithOptions` (`:141`); `Checked > 0` with nothing unverifiable
sets `QuickCheckClean` (`:194`); and `stage_repair.go:111-117` returns `nil` on
`QuickCheckClean`, skipping the par2 verify+repair subprocess entirely. The
same value also lets `app.par2NeedsRecovery` return false, leaving the deferred
recovery volumes unfetched. So our CRC feeds the verdict, and the verdict is a
full bypass.

The half worth keeping is the distinction, not the denial: **`par2.VerifyCRCs`
consumes our CRC; `par2.QuickCheck` does not.** The confusion is that the
post-processing *stage* shares the latter's name.

`par2.QuickCheck` is a different function — it relocates flat downloads into the
subdirectory paths a par2 manifest references — and it computes its own CRC from
disk (`tryMatchCRC32File` takes a path). It does not consume ours. It does gate
its hash16k and CRC passes on an exact size match, which is why the truncate in
§3.6 is load-bearing: an untrimmed file is never relocated, so par2 reports it
missing and works to reconstruct a file that is complete on disk.

### 3.6 Pre-allocation stays; the truncate bounds on `Σ length`

`FileInfo.ExpectedSize` is the NZB's declared **encoded** byte count and yEnc
inflates ~2%, so every file is pre-allocated above its decoded size and trimmed
on completion. Pre-allocation earns its place — it detects `ENOSPC` at the start
of a 50 GB job rather than at 90% — and the trim is what makes §3.5's size
comparison meaningful.

The bound is `max(offset+length)` over the file's rows, which for a complete
file equals `Σ length`.

## 4. Invariants

| Rule | Before | After |
|---|---|---|
| R1 — record immutable | `INSERT OR IGNORE` | **deleted** — merging is read-modify-write, and the newest `fsync` is the newest truth |
| R2 — no ordering against the write | may be committed before, during, after, or never | **deleted** — the record exists only after a completed `fsync` |
| Class A asserts | nothing about presence | presence, and the CRC of what is present |
| S1, S2 | Class B strictly after the `fsync` | unchanged, and now the only claim there is |
| S4 — recomputation beats the stored record | the bitmap is rebuilt from facts plus a disk read | **inverted** — the record is authoritative, gated on one `stat` (§3.4) |
| S5 — no second copy to drift | two records, two guards | one record; both guards and `file_extents` gone |
| **Writers to the durability record** | **two** — the barrier's commit, and `Resumer.writeBack` | **one** — the barrier. The resume only *deletes*, and only when the file on disk contradicts the record |
| S6 — truncate only shrinks | unchanged | unchanged |
| S7 — validity stamp | `(size, mtime)`, mismatch triggers a recompute | **narrowed to size** — with no recompute left, mtime's only response is a full re-fetch, and mtime moves without bytes moving (§3.4) |
| R3 — losing a suffix costs a re-fetch | applies to Class A | applies to the run record, and is now the routine cost after an unclean shutdown |

### What this deletes

`article_facts`, `file_extents`, `ArticleFact`, `FileExtent`, `verifiedPrefix`,
`prefixWalk`, `durableAt`, `durableExtent`, `recordedExtent`, `overlapAnywhere`,
both `FinalizeFile` guards, `PrefixCRC`/`HasPrefixCRC`, `VerifiedTo`,
`BytesDurable`, `ModTimeNs` and the mtime half of S7 (§3.4), `Bitmap` and the
whole of `extent.go`, `Resumer.recompute`, `Resumer.writeBack`,
`verifyRegions`, `SeedFromExtents`, `ReplaceFromResume`, the `articles_done`
blob in both `job_files` and `history_job_files`, and its independent
reimplementation in `test/crash/harness.go`.

**`SyncTarget` also loses two methods**, which is an interface change rather
than a deletion of dead code. `FileLocalOrdinal` maps a global article index to
a per-file bitmap bit and `ArticleCount` sizes that bitmap; a run carries
`first_art_idx` and `last_art_idx` directly, so neither conversion has anywhere
to happen. The slice reaches both interface declarations, `jobSyncTarget`'s
implementations, and `manifestArticleMap`, which exists only to back them.
`Truncator` embeds `SyncTarget`, so both shrink together.

Roughly **1,850 non-test lines are deleted against ~360 written.**

**The write cache is deliberately kept.** It was surveyed as a candidate — it
coalesces contiguous articles into fewer `WriteAt` calls, is not needed for run
formation, and its disabled path already exists and is exercised — and the
decision was to retain it. Recorded so it is not later mistaken for an
oversight.

### What this costs

**A checkpoint of re-download per unclean shutdown.** Bytes fsynced but not yet
recorded are re-fetched rather than trusted. The barrier cadence is 30s or
64 MiB, whichever comes first, and file completion and clean shutdown both
shrink it further; a paused job runs no barrier at all. The contract already
prices this as R3.

**No detection of in-place corruption at startup.** §3.4's trade.

**Diagnosis gets coarser.** A merged row says the file is wrong, not which
article. par2 repairs at block level and does not consume that distinction.

**An overlapped file is detected but not prevented.** Its bytes are written, the
overlap surfaces at completion as `Σ length > stat size`, and it never publishes
a whole-file CRC. par2 repairs it. Nothing refuses the write, because the index
that would have to answer "does this range intersect anything written" does not
exist and building it would duplicate a fact the completion check already
covers.

## 5. Why the storage-fault objection does not apply

Appending at accept time was rejected for #389 because a SQLite write on the
assembler's worker goroutine sits where `jobSyncTarget.submit` reads "worker did
not answer in 5s" as evidence about storage, so contention could mint a storage
fault and park a healthy job (A1).

That does not reach this design. The commit runs on the barrier's own goroutine
(`barrier.go:272`, `:769`); only `Files`, `Drain`, `Sync`, `Stat`, `Truncate`,
`Confirm` and `Close` cross into the worker under `barrierOpTimeout`.

## 6. Ordering inside the barrier

`Run`'s phase 4 carries a constraint the record write must respect:

> commit Class B atomically, then and only then ack. Nothing between these two
> statements may fail, and nothing may be inserted between them: the commit is
> what makes the proof true after a crash.

So the run rows are written **inside** that commit, in one transaction, at both
commit sites — `Run`'s at `:272` and `FinalizeFile`'s own at `:769`. A separate
call before or after would be a second failure point between the fsync and the
ack.

**The barrier does not build the runs. `Commit` takes the drained articles and
the store builds them**, inside that same transaction. The barrier's job is to
hand over what was fsynced; deciding which of those articles form a run is
derived state, and it gets one owner.

That is not tidiness — it is the only place the dedup can be correct. The
drained set overlaps the stored rows whenever an ack fails after a commit,
because `Confirm` runs only after `AckDurable` and `Drain` re-delivers (R12).
Dedup must therefore happen against **what is already stored**, and it must
happen at *article* granularity **before** grouping:

> A re-delivery of articles 5–9 arrives alongside genuinely new articles 10–12.
> Grouped first, they form one run `[5,12]` that no stored row covers, so no
> whole-run check drops it, and it is inserted beside the stored `[0,1000]`
> row. `Σ length` is then 1800 against a 1300-byte file — a permanent
> false overlap finding (§3.3) on a perfectly healthy file.

Subtracting covered `art_idx` values first leaves the run `[10,12]`, which is
the truth. A barrier that grouped before calling the store could not do this
without first querying the store for coverage — which is the store's knowledge,
reached through a second code path, at two call sites. One owner, one order:
**subtract, then sort, then group, then merge.**

## 7. #422, carried separately

`RetryHistoryJob` deletes the durability rows the retry needs.
`history.Repository.Delete` drops them unconditionally
(`internal/history/repository.go:358-363`), called at `internal/app/app.go:1858`
— nine lines *after* `queue.Add` at `:1849`, so rows the re-enqueued job writes
in that window die too.

**The function says so itself, twice, and both statements are false today.**
`app.go:1841-1842` opens with "its Class A facts survive with it (`Append` is
`INSERT OR IGNORE` …)", seventeen lines above the `Delete` that removes them.
And `:1852-1857`, justifying `Delete` over `deleteHistoryEntries`, enumerates
what goes and what stays and does not mention the durability rows at all. One
asserts they survive; the other lists what is destroyed and omits them.

This is not an instance of the two-record class and is not fixed by anything
above. It is carried here by explicit scope decision, as its own commit.

## 8. Open questions

**The plan's `Inconclusive / Deferred items` section is the authority**, and this
section deliberately does not restate it. An earlier version listed four
questions here as well; three were then resolved in the plan and this copy went
stale within the day, which is the failure mode the single-authority rule exists
to prevent.

One question remains open at the time of writing, and it gates nothing: how
often an NZB's segment `number` matches the yEnc part number in the body, which
decides how often runs actually collapse to one row rather than whether the
design holds. It cannot be answered from an NZB — an NZB carries segment numbers
and encoded byte counts, never offsets — and the only `=ypart` emitters in this
repository are its own test generators. It needs the decoder instrumented against
real traffic.
