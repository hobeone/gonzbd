# Download Durability & Article Cache — Design

**Status:** design, not yet implemented.
**Scope:** the article → disk → ack loop, and restart reconstruction.
**Deliberately implementation-independent.** This document describes the
problem to be solved and the rules any solution must obey. It does not
describe `internal/assembler` as it stands, and it is not a refactoring plan
for it.

**Relationship to the existing contracts.** `docs/assembler-storage-contract.md`
and `docs/queue-lifecycle.md` remain authoritative: they are the contract the
current code is held to, and a disagreement between them and the code is still a
bug in the code. This document is a *design proposal* and carries no authority
over the implementation until it is accepted and implemented. On acceptance,
those two contracts must be revised to match — and until that revision lands,
they win. Nothing here should be cited as a reason the current code is wrong.

## Why this document exists

Nine open issues (#306, #311, #337, #344, #349, #353, #355, #356, #357) and
five of the last eight merged fixes (#305, #315, #341, #343, #350) are the
same two defects wearing different clothes.

**Shape 1 — a fact is recorded before the thing it asserts becomes true.**

- #355 — an article is acked `Done` when *accepted into a memory cache*, not
  when its bytes land.
- #356 — a CRC part is recorded at accept, not at write.
- #350 / #342 — `maxWritten` describes *this run's* extent, then truncates a
  file that is the product of *every* run.
- #349 — per-article CRCs from one run are combined and labelled "whole-file".

**Shape 2 — the same fact is maintained in two places and they drift.**

- #306 — `bytes_downloaded` is loaded from SQLite, then overwritten by a
  recompute.
- #337 — `total_bytes` is stored while sibling figures are derived.
- #305 / #315 — file-set state is persisted by an index another path
  renumbers.
- The whole of `docs/queue-lifecycle.md` exists because residency was a second,
  parallel axis to phase.

These recur one at a time because the codebase has **no single definition of
"resolved" for an article**. Today it is implied by the conjunction of four
independent facts: a bit in `JobProgress.done`, a row in `job_files`, bytes in
the OS page cache, and bytes on the platter. Each fix pins one pair of those
into agreement; the next bug is the next pair.

## Scope

**In scope.** The path from a decoded article to bytes on stable storage to an
ack the queue can rely on, plus everything a fresh process needs to reconstruct
what work is outstanding. This includes the write cache, coalescing,
pre-allocation, extents, per-file CRC, the ack contract between assembler and
queue, and the queue's persisted progress.

**Out of scope, and a client of this subsystem rather than part of it:**
post-processing — par2 verify/repair, unpack, user scripts, history. Its
restartability is a different problem class (idempotency of long-running
external processes) and needs its own spec.

**Compatibility.** Greenfield. No migration obligation for existing on-disk
state; the project targets fresh installations and is pre-1.0. The design is
not required to preserve the current SQLite schema, manifest format, or the
meaning of any existing column.

---

## 1. The core problem

> A download is a long-running computation over unreliable inputs, unreliable
> storage, and an unreliable process, whose output must never be claimed more
> complete than it is.
>
> The system must, at any moment and after any termination, answer one question
> correctly from stable storage alone: **which articles still need to be
> fetched?** It must answer conservatively — a wrong "still needed" costs
> bandwidth; a wrong "already have it" costs a corrupt file that no repair
> stage was told to repair.
>
> Every other concern in this subsystem — the write cache, coalescing,
> pre-allocation, batched acks, extents, CRC accumulation — exists to make that
> answer cheap to maintain. None of them may make it wrong.

The asymmetry in the second paragraph is the entire design. Articles are
content-addressed and re-fetchable indefinitely, so **over-fetching is a cost
and over-claiming is a defect**. The system never needs exactly-once semantics.
It needs at-least-once delivery with an idempotent apply, and an absolute
prohibition on optimism.

### Non-goals

Stated so they stop leaking back in:

- Minimising re-downloads to zero.
- Preserving in-memory state across a crash.
- Recovering an article whose bytes were lost — re-fetch it.
- Making the write cache durable in any form.

### Failure modes in scope

| Mode | What survives | Why it matters here |
|---|---|---|
| Process crash / `SIGKILL` | page cache | in-memory cache and unflushed acks are lost |
| OS crash / power loss | only fsynced data | makes metadata-ahead-of-data a corruption bug, not a theoretical one |
| Disk full / I/O error mid-write | — | needs a defined subject and disposition; today it is logged and swallowed |
| Corrupt / truncated / externally-modified partial file | — | no ordering discipline can help; the metadata was correct when written and the world changed |

---

## 2. Vocabulary

### An article's states

| State | Means | Survives |
|---|---|---|
| **Outstanding** | no evidence its bytes are on stable storage | — (the safe default) |
| **InFlight** | dispatched to a connection this run | nothing |
| **Decoded** | bytes in memory; `{offset, length, CRC}` now known | nothing |
| **Written** | handed to the OS without error | process crash |
| **Durable** | covered by a *completed* `fsync` | power loss |
| **Resolved** | will never be fetched again — either Durable, or permanently failed on every eligible server | — |

Only **Resolved** removes an article from the work set. Only **Durable**
justifies a download-success ack. The gap between *Written* and *Durable* is
the rework window. The gap between *Decoded* and *Durable* is where #355 and
#356 live.

### The three classes of fact

This is the load-bearing part of the design. It governs what may be persisted
and what ordering, if any, the persistence needs.

**Class A — immutable discovered facts.** `article → {offset, length, CRC32}`;
`file → {name, declared size}`. Learned once, when the article is decoded, and
never changed thereafter. Append-only. Losing a suffix is harmless: it costs a
re-fetch, never a wrong answer.

An article's decoded byte range is *not knowable until the article is fetched* —
the NZB carries only a segment number and an encoded byte count, while the real
`begin`/`end` offsets live in the `=ypart` header inside the article body.
(`offsetInRange` exists today precisely because that header is attacker-controlled.)
So the article → byte-range map is itself downloaded data, discovered
incrementally, and must be durable to reason about a file's contents. It is
also immutable once learned, which is what puts it in Class A.

**Class B — derivation cache.** The durable article bitmap, the verified
extent, the gapless prefix and its CRC, `bytes_downloaded`, health percentages.
Every one is recomputable from Class A plus the file's actual bytes. They exist
purely for speed. **None is ever authoritative.**

**Class C — transient.** InFlight, emitted, Decoded, write-cache contents,
pending ack batches. Never persisted, by rule. On restart they reconstruct as
*unknown*, which resolves to Outstanding.

### Why the taxonomy is what makes ordering safe

Metadata and data have a dangerous relationship today because the metadata
*asserts* things about the data — "this file is 4 GB long", "these articles are
done" — so committing it early is a lie.

A Class A fact asserts nothing of the kind. It says only: *if the bytes at
`[offset, offset+length)` are present, they hash to `CRC`.* That statement is
true the instant the article is decoded and stays true forever, whether the
write succeeded, failed, or was never attempted.

So Class A can be committed at any time, in any order, with no barrier. The
barrier is needed **only** for Class B — and Class B is a cache, so a barrier
failure degrades performance rather than correctness. The ordering problem is
not solved so much as moved somewhere that getting it wrong is survivable.

This also explains why the existing `writeCursor` became unusable as a
durability anchor (#353): it is a Class B value that was promoted to authority,
then had a Class C responsibility bolted on (skip past holes so coalescing keeps
working, #311). One variable serving as cache, authority, and scheduling hint.
The three roles pull in different directions, and it broke in all three.

---

## 3. Invariants

Each is stated so a violation is identifiable, and tagged with the issues it
retires. An invariant that retires nothing is not earning its place.

### Safety — never over-claim

- **S1 · No claim without an fsync.** No persisted or externally observable
  fact may assert that a byte range is present on stable storage unless a
  completed `fsync` covers it. *Corollary:* any Class B commit describing data
  is ordered strictly after the fsync of that data. → *#350, #342.*
- **S2 · Acceptance is not durability.** Entry into a buffer, channel, cache,
  or batch is never evidence about disk. An article may be acked as downloaded
  only when **Durable**; a CRC part may be recorded only when **Durable**.
  → *#355, #356.*
- **S3 · Absence of evidence is absence.** Any article whose state cannot be
  established from stable storage is **Outstanding**. There is no "probably
  done". This is what makes emitted-is-transient correct and why Class C may
  never be persisted.
- **S4 · Derived values are never authoritative.** Where a Class B value and a
  recomputation from Class A + file bytes disagree, the recomputation is correct
  **by definition** and the cache is discarded. No Class B value may be the sole
  basis for a destructive action. → *#349, #306, #337.*
- **S5 · Exactly one authoritative representation per fact.** No fact is stored
  in two places. If a value is derivable it is Class B and labelled so. Two
  writers of one fact is a design defect, not a synchronisation problem.
  → *#306, #337, #305, #315.*
- **S6 · Metadata may shrink a file, never grow it.** *(corollary of S1)* Any
  operation deriving a file length from persisted state may only truncate.
  Growing appends zeros, which asserts content that exists nowhere. → *#350.*
- **S7 · Adoption requires validation.** A Class B cache for a partial file may
  be adopted only after a validity check against the file *as it exists now*.
  On failure it is discarded and recomputed, or the file restarts. → *replaces
  the stat-and-skip heuristic in `finalizeFile`.*

### Attribution — a failure belongs to a subject

- **A1 · A storage fault is never recorded as an article fault, nor the
  reverse.** `ENOSPC` / `EIO` / `ETIMEDOUT` resolve against *storage*: the job
  stalls with a surfaced reason, the article stays Outstanding. A missing,
  corrupt, or removed article resolves against *the article*: permanently
  failed, counted as damage. Health statistics reflect article-subject outcomes
  only.
- **A2 · Every failure has a subject and a disposition.** No path may
  log-and-continue. This is the general form of the "logged and skipped"
  behaviour in #344 and #355.

### Liveness — nothing is lost or stuck

- **L1 · Exactly one resolution per article per run.** Every path that consumes
  an article emits exactly one of: a Durable ack, a permanent-failure ack, or an
  explicit return-to-Outstanding. Dropping is prohibited. → *#357, #344.*
- **L2 · Every article terminates.** Every Outstanding article eventually
  becomes Resolved, or the job enters a stalled state with a surfaced,
  actionable reason. Indefinite silent non-progress is a defect, not a wait.
- **L3 · Restart does not lose ground.** A Durable article returns to
  Outstanding only as the result of a failed validation (S7). Restart costs
  bounded rework, never unbounded regression. → *the shape behind #316.*

### Bounds — every cost is stated

- **B1 · Bounded rework.** After power loss, bytes to re-fetch per job ≤ the
  checkpoint bound. Default **30 s or 64 MB per job, whichever comes first**.
  Configurable, and tested by an actual barrier-then-kill test.
- **B2 · Bounded memory.** Memory held for in-flight and cached article data is
  bounded by configuration, independent of job size, file size, and job count.
- **B3 · Bounded fast-path startup.** Restart is O(incomplete files) on the fast
  path — stat only, no manifest decompression, no file reads. Recomputation is
  O(bytes) but fires only on validation failure, is per-file, and is
  interruptible and resumable.
- **B4 · Bounded blocking on wedged storage.** Every storage syscall on the
  critical path is timeout-bounded. A wedged mount stalls the job (A1); it never
  stalls the process.

### Structure — enforced, not merely asserted

- **X1 · Single writer per file.** Exactly one component owns a file's handle
  and its Class B state.
- **X2 · The durability boundary is crossed in exactly one place.** The
  Written → Durable → Resolved transition happens in one code path — the
  barrier. No other path may ack.
- **X3 · Enforce by construction wherever the compiler can reach.**

X2 and X3 determine whether this design works, and they come from the project's
own recorded experience rather than from general principle.
`docs/queue-lifecycle.md` documents that `TestActiveSet_ResidencyProperty`
asserted exactly the right property — `phase ∈ {Active, Processing} ⟺ manifest
!= nil` — and **passed through all eight residency bugs**, because it walked the
happy path while every real failure happened on hydration failure, concurrent
access, or an operation invoked on a non-resident job. That is a documented case
of a correct invariant, correctly tested, preventing nothing.

The lesson generalises. S2 is unenforceable as a rule, because acking is
currently reachable from `handleSuccessArticle`, `handleLateDuplicate`, `flush`,
`finalizeFile`, and the two control-message paths. Six places that must each
independently remember not to ack early — which is why the same bug keeps being
refiled. X2 makes it one place. X3 says go further where possible: if the ack
function's argument is a token type only the barrier can construct, "ack before
fsync" stops being a rule someone must follow and becomes code that does not
compile.

### Deliberately not an invariant

There is no invariant requiring that a completed file's whole-file CRC be
reportable. S4 permits honest unavailability. Making it an invariant would force
a durable prefix CRC onto the barrier's critical path for a benefit that appears
only on resumed jobs, so it is R24 — a requirement with a stated cost — instead.

---

## 4. Requirements

**MUST** = invariant-derived. **SHOULD** = quality goal with a stated cost.

### Class A — the article fact log

- **R1** The system MUST persist, for every article it has decoded, an
  immutable `{file, article index, decoded offset, decoded length, CRC32}`.
  Append-only; never updated or deleted while the job lives.
- **R2** Class A records MAY be committed at any time relative to file writes,
  with no ordering constraint against the data, because they assert nothing
  about presence.
- **R3** Loss of any suffix of Class A MUST degrade to re-fetch only, never to
  incorrect state.
- **R4** Class A MUST be bounded and evictable: ~16 B/article, ≤ ~2 MB for a
  60 GB job, and the design MUST NOT require all records resident for all jobs.
  It inherits the memory budget in `docs/queue-lifecycle.md`.

### The checkpoint barrier

- **R5** A per-job barrier MUST: drain buffered writes → issue all pending
  writes → `fsync` every open file of the job → **and only then** commit the
  job's Class B cache in one atomic transaction.
- **R6** It MUST run on the lesser of a time bound and a byte bound, and
  additionally on file completion, job pause, and clean shutdown.
- **R7** A barrier failure MUST ack nothing, MUST leave the prior committed
  cache intact, and MUST route the fault through A1.
- **R8** Barrier cost MUST NOT scale with job size — it fsyncs *open* files,
  not all files.

### Resolution and acks

- **R9** Download-success acks MUST be emitted only by the barrier, and only
  for articles whose bytes were written before the fsync that barrier completed.
- **R10** Permanent-failure acks MAY be emitted outside the barrier. They assert
  nothing about disk, and losing one is safe — restart re-attempts an article
  that will fail again.
- **R11** Every article the assembler consumes MUST produce exactly one
  resolution within the run, and "no resolution" MUST be made unrepresentable
  rather than merely tested.
- **R12** Duplicate delivery of an article MUST be idempotent. At-least-once
  delivery is the contract; the apply absorbs it.

### Resume

- **R13** On opening an incomplete file, the system MUST run a cheap validity
  check — file identity, size, mtime against the committed cache — before
  adopting any Class B value.
- **R14** On validation failure it MUST recompute the done-set by reading the
  file and verifying each Class A region against its CRC. Where Class A is
  insufficient to cover the file, it MUST restart the file rather than guess.
- **R15** Recomputation MUST be interruptible and MUST NOT block unrelated
  jobs.

  **"Resumable" was dropped from this requirement.** Recomputation fires only
  when the stat fast path fails, which is the rare case; it is O(bytes) against
  local disk rather than the network; and a discarded partial costs a re-read,
  never a wrong answer. Making it resumable needs a persisted verified-through
  offset — a new Class B field, committed per chunk during the recompute, that
  can disagree with the file. That is the exact class of independently
  maintained state this design exists to remove, spent to avoid re-reading a
  file. The requirement was over-specified.
- **R16** Restart MUST reconstruct the outstanding work set without
  decompressing manifests — fast path O(incomplete files).
- **R17** Any article not provably Durable after resume MUST be Outstanding.

### Storage faults

- **R18** Write and sync failures MUST be classified by subject and
  retryability: *retryable-storage* (`ENOSPC`, `EDQUOT`, `ETIMEDOUT`, `EIO`,
  `ESTALE`, a missing target directory, a wedged mount) and
  *permanent-storage* (`EROFS`, `EACCES`/`EPERM`, and anything that cannot
  clear without a configuration change). Anything unrecognised is retryable.
  Storage never produces an article-subject fault.

  **`EIO` and a missing target directory are retryable, reversing this
  requirement's first draft.** That draft contradicted A1, which listed `EIO`
  among the faults that stall; A1 governs. Retryable does not mean retry
  forever — it means the job stalls with a surfaced reason (R19) — and both
  conditions are routinely transient on network-backed and removable volumes.
  Failing the job instead discards every byte already downloaded, for a
  condition a user often fixes in seconds. A dying disk still reaches the
  operator, as a stall rather than a job failure.
- **R19** Retryable-storage → the job stalls, the reason is surfaced, articles
  stay Outstanding, and the condition is re-evaluated on an interval and on user
  action.
- **R20** Permanent-storage → the job fails with that reason. No article is
  marked failed.
- **R21** No storage fault may alter the health percentage or the failed-byte
  count.
- **R22** Every storage syscall on the critical path is timeout-bounded, with at
  most one probe in flight per mount.

### Whole-file integrity

- **R23** A completed file MUST report a whole-file CRC only when the
  contributing parts provably tile `[0, final length)` with no gap and no
  overlap. Otherwise it MUST report **unavailable**, and unavailable MUST be
  distinguishable from a CRC of zero.
- **R24** *(SHOULD)* A resumed file SHOULD be able to report a real whole-file
  CRC, derived from Class A over the verified extent, so QuickCheck can
  short-circuit a resumed job. This work MUST be off the barrier's critical
  path.

  **`HasPrefixCRC` means "this is a verified whole-file CRC", not "the CRC over
  `[0, VerifiedTo)` is valid for that range".** It is set only when the
  verification run consumed every recorded fact *and* reached the file's end.
  The looser reading preserves more information but has no consumer — R24's
  only stated use is QuickCheck short-circuiting, which needs a whole-file
  value — and it reintroduces the misuse that #349 was: a partial-extent CRC
  mistaken for a whole-file one. Where the strict condition does not hold, the
  honest answer is unavailable, which R23 already blesses.
- **R25** Truncation to final size MUST only shrink, and MUST be based on the
  verified extent, never on the run's high-water mark.

### Observability and configuration

- **R26** A job MUST be able to report at any time: bytes durable, bytes
  written-but-not-durable, articles outstanding, time of last successful
  barrier, and stall reason.
- **R27** A stalled job MUST surface a reason the user can act on.
- **R28** Runtime detection of an invariant violation MUST fail loudly,
  consistent with the project's existing fail-loud stance — never degrade
  silently.
- **R29** Configurable: checkpoint interval, checkpoint bytes, write-cache
  bytes, resume-recompute policy.
- **R30** Defaults MUST be safe untuned.

### Verification obligations

- **R31** B1 MUST be verified by a test that kills the process between barriers
  and asserts the re-fetch set falls within the bound — not by reasoning.
- **R32** Crash-consistency tests MUST cover all four failure modes, including a
  page-cache-dropping harness for simulated power loss.

  **Superseded in part — a page-cache drop does not simulate power loss.**
  `POSIX_FADV_DONTNEED` invalidates clean pages and skips dirty ones, and
  `/proc/sys/vm/drop_caches` skips them too, so no unprivileged call discards
  unfsynced data; the harness that this obligation asks for cannot exist without
  root. What `test/crash` does instead is stated in `docs/TESTING.md` §3a: the
  SIGKILL is real and destroys the assembler's in-process write cache, which is
  the process-boundary half of S1/S2, and the `fadvise(DONTNEED)` forces
  already-written-back ranges to be re-read from the block device rather than
  from cache. The page-cache half — that an fsync'd byte reached the platter —
  needs a device-mapper `log-writes` or `flakey` target under root and is
  **untested**.
- **R33** External modification MUST be tested: truncate, delete, append, and
  mtime-only touch. All four are covered in `test/crash/external_test.go`;
  truncate and delete currently FAIL, against a real defect (#362; see
  `docs/TESTING.md` §3a).
- **R34** Per `AGENTS.md`, every invariant MUST have a test **observed** to fail
  against a mutation that violates it, with the failure message recorded.

---

## 5. Architecture

Seven units, each with one job, each independently testable.

| Unit | Owns | Depends on | Never does |
|---|---|---|---|
| **FactLog** (Class A) | append-only `{article → offset, len, CRC}` | storage | assert presence; mutate a record |
| **FileWriter** | one file's handle; its write cache, coalescing, pre-allocation | FaultClassifier | ack; decide durability; be observed mid-write |
| **Barrier** | fsync; the Class B commit; **all** success acks | FileWriter, FactLog, ExtentStore | write article data; classify faults |
| **ExtentStore** (Class B) | committed cache: durable bitmap, verified extent, prefix CRC, validity stamp | storage | be read without a validity check |
| **Resumer** | validity check; recomputation from bytes | FactLog, ExtentStore | adopt an unvalidated cache |
| **FaultClassifier** | error → `{subject, retryability}` | — | know about articles or jobs |
| **WorkSet** | derives outstanding articles for dispatch | ExtentStore, FactLog | hold authority; be persisted |

**Steady state.** decode → FactLog append (unordered, no barrier) → FileWriter
buffers/writes → …every 30 s or 64 MB… → **Barrier**: drain → write → fsync →
commit ExtentStore → emit acks → WorkSet shrinks.

**Resume.** load ExtentStore → Resumer validity check → *pass:* adopt, WorkSet =
complement of the durable bitmap · *fail:* read file, verify against FactLog,
rebuild bitmap, recompute prefix CRC → WorkSet.

### Making X3 concrete

`Barrier` is the only unit that can construct the token `ack()` requires as an
argument. Put the token type in the barrier's package with an unexported field
and no exported constructor, and every path that can ack today —
`handleSuccessArticle`, `handleLateDuplicate`, `flush`, `finalizeFile`, and the
two control messages — physically cannot call `ack()`. Not "must not". Cannot.

`FaultClassifier` does not know what an article is, which makes A1 true by
ignorance rather than by discipline: it has no vocabulary in which to blame an
article.

### The honest costs

1. **`FileWriter` gets strictly dumber and the barrier takes the hard job.**
   Today's assembler decides durability, acks, CRCs, extents, and truncation
   inline. Splitting that out means the write path loses the ability to make any
   externally visible decision — the point of the design, but an inversion of
   the current one rather than a refactor of it.
2. **`WorkSet` derived and never persisted** is the strictest reading of S5, and
   it means dispatch recomputes a complement on every restart. That is cheap — a
   bitmap complement — but there is no longer any such thing as a saved queue
   position, only saved facts you re-derive one from.
3. **Deriving is not free.** It moves cost somewhere honest. The project has
   already paid this: deriving remaining-bytes is exactly why `FileProgress.Bytes`
   and the `failed_bytes` column had to be added (`docs/queue-lifecycle.md`).
   Expect the same shape here — each derived value needs its inputs to be
   present at every residency.

---

## 6. Decisions recorded

Choices made during design that were selected rather than derived, listed so
they are easy to revisit.

| Decision | Chosen | Alternatives considered |
|---|---|---|
| Scope boundary | article → disk → ack + restart reconstruction | assembler-only; whole job lifecycle including post-processing |
| Rework after power loss | bounded window, explicitly specified and tested | unbounded but conservative; near-zero via per-article durable record |
| Resume trust | cheap check always, full re-read only on disagreement | always re-read; trust metadata and treat external modification as user error |
| Durable per-file record | durable article bitmap + separately tracked verified prefix | gapless resolved-prefix only; bitmap only, no CRC anchor |
| Write failure policy | never mark the article failed; stall the job, retain as Outstanding | mark failed and let par2 repair; treat as fatal to the job |
| Compatibility | fully greenfield, no migration obligation | redesign + migrate; additive only |
| Architecture | reconstruction discipline (Class A/B/C) + checkpoint barrier | barrier alone; barrier + full per-article state machine |
| B1 default | 30 s or 64 MB per job | *not otherwise explored; a starting point to be tuned against measurement* |
| CRC on resume | requirement (R24), not invariant | invariant, at the cost of a durable prefix CRC on the barrier path |

## 7. Open questions

- **B1's default** is asserted, not measured. It should be validated against
  real fsync cost on the target filesystems (ext4, NFS, SMB) before it is
  treated as settled.
- **Class A's storage medium** is unspecified here on purpose — SQLite rows, a
  per-job append-only file, or an extension of the existing manifest are all
  compatible with R1–R4. The choice is an implementation decision constrained by
  R4's memory budget and B3's startup bound.
- **Recomputation triggers beyond validation failure.** Whether a periodic or
  user-invoked full verification is worth offering is not decided.
- **Interaction with DirectUnpack.** DirectUnpack consumes whole completed
  volumes, so it depends only on file completion, which this design does not
  change. That should be confirmed rather than assumed before implementation.
