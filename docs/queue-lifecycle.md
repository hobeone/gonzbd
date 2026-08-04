# Queue & Job Lifecycle Contract

This document is the contract for `internal/queue`: what state a job is
guaranteed to have, which operations may fail, and which must not. Read it
before changing anything that touches job residency, the `ActiveSet`, the
promotion loop, or `Manifest`/`JobProgress` access.

`docs/ARCHITECTURE.md` describes the queue's shape. This describes its
obligations.

**This states the contract in the present tense, including parts not yet
built.** That is deliberate — it is the target the code is held to, not a
report on the code as it stands. The Status section below records exactly what
has landed. Where the two disagree, the code is wrong and the gap is a bug, not
a documentation error.

## Why this exists

The queue evicts a job's `Manifest` and `JobProgress` when the job leaves the
active set, to bound memory. That optimization is justified — see the measured
figures below — but it was landed without the structural safeguard its own
design called for. Issue #203 was explicit:

> Four phases replace the 14 statuses, with residency as a function of phase —
> Active/Processing if and only if the manifest is resident — rather than a
> parallel `inflated` axis. That structural choice is what removes the
> silent-nil defect class.

The memory work shipped; the structural choice did not. `JobPhase.IsResident()`
declares the invariant, but nothing enforces it, and residency is decided
independently in `Queue.Add`, `evictJobLocked` and `PromoteNext`. Eight issues
(#258, #260–#265, #267) and four pull requests followed, every one of them a
variation on "these two pointers can be nil and nothing made you check".

A property test for the invariant already exists —
`TestActiveSet_ResidencyProperty` asserts `phase ∈ {Active, Processing} ⟺
manifest != nil`. **It passed through all eight bugs**, because it walks one job
through the happy path of normal transitions. It never exercises a hydration
failure, concurrent access, an aggregate query spanning non-resident jobs, or an
operation invoked *on* a non-resident job — which is where every one of the
failures actually occurred. That is the evidence for enforcing this contract
with the compiler rather than with tests or review.

## The three tiers

Every mutating `JobProgress` operation takes a `*Manifest`. Not one read does.
The tiering below is not an imposed taxonomy; it is that boundary made explicit.

| Tier | Needs | May fail? |
|---|---|---|
| **Header** — remove, reorder, priority, status, ID/name lookups | neither | **Never** |
| **Progress** — all reporting, counters, completion and abort checks | `JobProgress` (always resident) | **Never** |
| **Manifest** — dispatch, article indexing, byte accounting | `Manifest` (evictable) | Yes, and must say so |

Writes need article byte counts and the file↔article mapping. Reads do not.

External packages read five manifest-derived scalars — `TotalBytes`,
`NumFiles`, `NumArticles`, `Par2Bytes`, `Par2Files`. These are computed once at
`Add` and never change, so they live in the always-resident tier rather than
behind a fallible handle. This generalizes what `lastKnownRemainingBytes`
already does as a one-off: a reporting path must never need a manifest.

The consequence is the point of the whole design: **every reporting path, and
all current external call sites, become infallible.** Only the downloader and
assembler mutation paths take the fallible handle, and those already hold the
queue lock and already return errors.

## Residency

**Always resident, from `Add` until the job leaves the queue:** header fields,
`JobProgress`, and the five manifest scalars. No code path may drop them, so no
caller checks for their absence.

**Evictable:** the `Manifest`, and only the manifest, bounded by `maxActive` as
today.

**Residency is not derived from status.** Either you hold a manifest handle or
you do not, and that is visible to the compiler. `Phase()` remains a useful
derived view — the 14 statuses cannot collapse into four, because the legacy
`/api?mode=queue` contract exposes status strings to third-party clients — but
nothing may infer residency from it.

Two properties follow, both of which retire open defects without special
handling:

- **A handle cannot be invalidated by eviction.** `Manifest` is immutable after
  parse, so a handle holding a pointer the queue has since replaced still
  describes the job correctly. No lock, no re-check, no stale read. This is
  what makes #263 structurally impossible rather than fixed.
- **Snapshots stop hydrating from disk for reporting**, so there is no window
  between cloning a job and reading a file that may have been unlinked
  meanwhile. A concurrently removed job yields a stale-but-correct read instead
  of an error, and a queue listing can no longer fail because a job completed
  during it.

### Restart

`JobProgress` must be sized without loading manifests, which requires the
article count per file. `job_files.articles_done` recovers that only to within
8, so `job_files` carries an explicit `article_count` column. This is the
design's only schema change and needs a new goose migration; startup then stays
O(1) in manifest size rather than decompressing every manifest at boot.

## Memory budget

Per article, measured against the field types:

| | per article | 20k-article job |
|---|---|---|
| `Manifest` — `articleIDs []string`, `articleBytes`, `articleNumber`, lazy index | ~80 B + map | ~3.3 MB |
| `JobProgress` — `done`/`failed`/`emitted` as `[]bool` | 3 B | ~60 KB |
| `JobProgress` — the same, as bitsets | 0.375 B | ~7.5 KB |

Progress is roughly 2% of what eviction currently reclaims. Keeping it resident
for hundreds of jobs costs tens of MB; keeping manifests resident for the same
queue approaches 1 GB. That asymmetry is the whole justification for evicting
one and not the other.

`done`/`failed`/`emitted` are stored as bitsets, not `[]bool`. Three `[]bool`
spend three bytes to hold three bits; #203 assumed bitsets throughout and the
implementation never did it.

### Terminal jobs

A failed job never leaves the queue on its own: `status.go` allows
`StatusFailed` to transition only to `StatusQueued` (retry) or `StatusDeleted`.
It parks in `byID` until the operator acts. Holding full per-article state for
every parked failure is therefore an unbounded, silent cost.

A job in a terminal phase is not dispatching, so its per-article arrays are dead
weight; only the summary counters and per-file state are read. The plan was to
drop those arrays on entry to a terminal phase and rehydrate them from the
persisted `articles_done` bitmap on retry.

**Measured, and declined.** The argument above was written when
`done`/`failed`/`emitted` were three `[]bool` and before eviction on terminal
entry was dependable. Steps 1 and 2 removed most of what it was reclaiming. For
a 20k-article job (100 files × 200):

| | per job | per article |
|---|---|---|
| `Manifest` — already evicted on terminal entry | 1,356,394 B | 67.8 B |
| The three per-article bitsets — all compaction can drop | 7,512 B | 0.376 B |

Compaction would therefore reclaim **7.5 KB per parked terminal job**: roughly
140 parked 20k-article failures per megabyte, against a manifest 181× larger
that is already gone. The cost that motivated this section is not there any
more.

`TestTerminalJobRetention_Measured` produces both figures and asserts the
ratio, so this decision can be re-derived rather than inherited. It computes
the bitset figure exactly from the backing arrays instead of sampling the
heap — at 7.5 KB the saving sits below the allocator noise from building a
1.4 MB manifest, and `-benchmem` cannot see it at all, since the bitsets are
allocated whether or not they are later dropped.

The cheap version is also unavailable. `ArticleDone(i)` returns `false` for an
out-of-range index, so simply dropping the bitsets would make a compacted job
report every article as not-done — silently, and plausibly. That is the
silent-nil class this whole contract exists to remove, so compaction would
require the full type-level treatment: a summary type with no per-article
accessors, threaded through every package that reads progress. A cross-package
API break for 7.5 KB a job is not a trade worth making.

Note that the rehydration half already exists and is exercised: `Retry` →
`hydrateJobLocked` → `RestoreJobProgress` → `decodeArticlesDone` restores both
`done` and `failed` from the persisted bitmap (the #260 widening), and
`SQLiteStore.Update`'s per-file flush is guarded on the manifest being
readable, so a terminal job's stored bitmap survives eviction untouched. If the
memory figures ever change, only the dropping and the type remain to build.

## Failing versus degrading

The fallible surface is manifest-bearing mutation, and nothing else.

**Recovery paths never hydrate.** `RemoveJob`, `queueDelete("all")` and
`Application.Start` are header-tier. Removing a job does not require reading
what is being deleted, and enumerating IDs does not require loading manifests.
A damaged job must remain removable, and one damaged manifest must never
prevent the daemon from starting.

**A hydration failure has exactly one meaning:** a job that needs its manifest
in order to download cannot load it. Fail that job — transition to `Failed`
with a warning, via the existing `prepareClaimFailureLocked` claim-failure
path. Do not fail the operation that happened to observe it, and do not fail
unrelated jobs.

**Do not distinguish concurrent removal from corruption.** That distinction was
only ever needed because reporting paths hydrated from disk. They no longer do.
In the manifest tier a vanished job makes the operation moot: `ErrNotFound`.

**A mutation that changes a job's file set must move both persisted artifacts
together.** The manifest blob and the `job_files` rows describe the same file
set, and the staleness guard pairs the blob against in-memory progress, so
writing one without the other is a defect on both counts: the pair disagrees
in this process, and the two on-disk artifacts describe different shapes.
`DiscardDeferredPar2` is the only such mutation after `Add`, and it goes
through `Store.ReplaceManifest`, which rewrites every row rather than deleting
the discarded ones — dropping a file renumbers every `file_index` after it.

The two writes cannot be made atomic across the filesystem and SQLite, so the
disagreement has to be *detected* rather than prevented, and it is detected
differently on each side of a restart (#294).

**Nothing may write `job_files` by `file_index` while the rows are known not
to describe the job's manifest.** `SQLiteStore.updateTx` — the third writer of
that table, after `addTx` and `ReplaceManifest` — takes each value from the
live manifest and never rewrites the identity columns, so under a
disagreement it splices one file's progress onto another file's row. A
`ReplaceManifest` that fails leaves exactly that disagreement, because the
discard is deliberately not rolled back.

`Job.manifestRowsStale` records it, guarded by `residencyMu` alongside
`fileSetGen`. `DiscardDeferredPar2` raises it before attempting the rewrite —
before, so a panic or partial write leaves it raised too — and lowers it on
success. `updateTx` skips its `job_files` half while it is raised, and
`Queue.saveStore` retries the wholesale rewrite each checkpoint until one
lands. This is the one self-heal in the manifest tier: everywhere else a
disagreement is reported and the job fails, but here the in-memory state is
correct and only the persisted copy is behind (#310).

`fileSetGen` is what makes this verifiable after the fact. It counts file-set
rebuilds and only `DiscardDeferredPar2` bumps it, so a rewrite that completed
outside `q.mu` can tell a second discard (which invalidates it) from ordinary
eviction and rehydration (which does not). Keying that check on the manifest
pointer instead is wrong: eviction nils the pointer and rehydration installs a
new one, so residency churn alone would leave the flag raised for the life of
the process.

**A snapshot may write `job_files` by index only while the job still has the
file set it was cloned from.** `saveStore` clones under `q.mu.RLock`, releases,
and only then writes, so a discard landing in that window renumbers the rows
the snapshot is about to address by their old indices. `manifestRowsStale`
cannot catch this alone — on the discard's success path the flag is lowered
again, and the snapshot predates its ever being raised — so
`reconcileJobFiles` compares each snapshot's `fileSetGen` against the live
job's and marks any it has outrun. That job simply skips one checkpoint; the
next clone is current.

**Across a restart the flag is gone**, and the stored row indices are the only
surviving evidence. `Store.RestoreJobProgress` range-checks every stored
`file_index` against the manifest it just read and reports `ErrManifestStale`
when a row falls outside it — the restatement the boot path previously lacked,
which is the half of #278 this pair needs. `SQLiteStore.Get` then *finishes the
interrupted write* rather than degrading: the blob is the newer artifact by
construction, so it rewrites `job_files` from it, carrying each surviving
file's progress across by **subject**. The index is what the renumber
invalidated; the subject is the identity that survives it. Only if that rewrite
itself fails does the job load non-resident, with `Queue.Load` sizing its
progress from `job_files`.

Degrading is not the fallback of choice, because it is not free: a job torn
mid-discard is at `StatusDownloading`, and a non-resident job at that status is
skipped by `ForEachUnfinishedArticle` and never selected by
`findNextQueuedCandidateLocked`. Nothing normalises the status at startup, so
it would report "Downloading" and dispatch nothing for the life of the process.

Neither guard is `describesSameJobAs`, and it cannot serve here: it compares a
live `JobProgress` against a manifest read from disk, which is the right check
on `hydrateJobLocked` and `hydrateSnapshot`, where the progress predates the
read. On the restart path the progress was just built *from* that manifest, so
it agrees by construction and the comparison can never fire (#294, #310).

## Across a restart

Every mechanism this contract relies on is scoped to one process:

| Mechanism | Enforced by | Survives a restart |
|---|---|---|
| `Job.Manifest()` returns `(*Manifest, error)` | the compiler | no |
| `Job.Progress()` infallible; no nil guard needed | the compiler | no |
| "a handle cannot be invalidated by eviction" | pointer immutability | no |
| `TestManifestAccessIsGated` | an AST walk over the package | no — it audits source, not stored state |
| `manifestRowsStale` (#315) | an in-memory field | no — deliberately not persisted |

That is not a defect in any of them. It is what "enforced by the compiler"
means: the guarantee holds for as long as the values do. But it has a
consequence worth stating once, because two bugs have now come out of leaving
it implicit (#294, #310):

> **An invariant held by a runtime handle is enforced only for that handle's
> lifetime. At a process boundary the handle is gone and only bytes remain, so
> the invariant needs a restatement that is derived from content — a shape
> comparison, a generation counter, a checksum — and a place on the load path
> that checks it.**

The failure mode is specific and worth recognising by shape. Code that pairs
two independently-persisted artifacts tends to acquire an in-memory guard
first, because that is where the disagreement is first observed. The guard
then reads as though it covers the invariant, and the load path — which
observes the same disagreement, later and with less information — is never
given one. Whether the in-memory guard would even *fire* on the load path is
the question to ask, and for `describesSameJobAs` the answer was no.

What follows for this package:

- **The pairs that must be checked on load are the manifest blob against the
  `job_files` rows.** Nothing else in the queue is split across two stores.
- **`Store.RestoreJobProgress` is where that check lives**, so every caller
  gets it by construction rather than by remembering. `TestJobFilesReadsCheckRowShape`
  fails any new reader of `job_files` that neither mentions `ErrManifestStale`
  nor records why a renumber cannot affect it. It checks that the author
  considered the disagreement, not that they handled it correctly — a grep
  standing in for a proof, which is the most an AST walk can offer here.
- **A load-path disagreement is repaired, not merely reported.** Degrading the
  job to non-resident is not a safe fallback here: `ReplaceManifest` is only
  reached from `DiscardDeferredPar2`, which runs mid-download, so the crash
  leaves the job at `StatusDownloading` — and a `Downloading` job with no
  manifest is invisible to both `ForEachUnfinishedArticle` (which skips a nil
  manifest) and `PromoteNext` (which only considers `StatusQueued`). Nothing
  normalises status at startup, so it would sit there dispatching nothing for
  the life of the process. Reporting the error instead is worse still: `List`
  drops a job whose `Get` fails, against "a damaged job must remain removable"
  above. Only reconciliation leaves the job usable.
- **Reconciliation preserves progress, or it is not worth doing.** Rebuilding
  rows from the manifest alone would zero `articles_done` for every surviving
  file and re-download a job that may have been nearly complete. If
  reconciliation itself fails there is no recovery left, and only then does the
  job load non-resident — with `hydrateErr` set, so "could not be made usable"
  stays distinguishable from routine eviction.
- **Reporting scalars are reconstructed from `job_files`, not from the
  manifest**, for exactly this reason: the rows are the artifact that survives
  with its own shape intact.

## Enforcement

- `Job.Manifest()` returns `(*Manifest, error)`. Every dependence on an
  evictable manifest is then a compile error until it is resolved: the
  compiler enumerates the work, where a hand audit does not — #267 records
  separate passes over the same code finding different subsets.
- **`Job.Progress()` stays infallible.** An earlier draft of this section
  said both accessors should stop existing. That contradicted the tier
  model directly below it: progress is always resident, so a fallible
  `Progress()` would force error handling at every reporting site for a
  condition that cannot occur, and would bury the manifest sites where
  absence is real. Only the evictable tier is fallible.
- ~~The terminal summary type has no per-article accessors.~~ Retired with
  terminal compaction; see the measurement under "Terminal jobs".
- `JobProgress` needs no nil guard anywhere, because it cannot be absent.
- `TestActiveSet_StructuralMemoryBound` asserts a count (`ActiveSet.Len() == 4`),
  not memory. With bitsets there are concrete per-article targets, so it should
  assert bytes per article. The #203 figures were measured once and then
  drifted unmeasured.
- Keep the residency property test, but extend it across the operation set
  rather than one job's transitions. Its current shape is why it caught nothing.
- `TestJobFilesReadsCheckRowShape` walks the package AST and fails any function
  that reads `job_files` without mentioning `ErrManifestStale`. Its exemption list carries the reason each reader is immune
  to a renumber — aggregates and bulk copies are, anything binding a row to a
  file by position is not — and is written to shrink, the same polarity as
  `TestManifestAccessIsGated` and for the same reason.

## Status

Staged so each step is independently landable:

1. Bitsets for `done`/`failed`/`emitted`, behind the existing accessors.
2. Promote the five manifest scalars; make `JobProgress` always resident; add
   the `article_count` column.
3. Convert every caller that reaches through the manifest only to read a
   promoted scalar, then make `Job.Manifest()` fallible and migrate what is
   left across `internal/api`, `internal/app`, `internal/postproc` and
   `cmd/`.
4. ~~Terminal compaction and its summary type.~~ Measured and declined — see
   "Terminal jobs". Steps 1 and 2 reduced the saving to 7.5 KB per parked job.
5. ~~Retire the parts of #261 this dissolves.~~ Done — see below.

Only the second half of step 3 cannot be partial, and the first half exists
to shrink it: of the call sites outside `internal/queue`, roughly half were
reading `TotalBytes`/`NumFiles`/`Par2Bytes` through a manifest they did not
otherwise need. Converting those first leaves the atomic change touching
only the sites that genuinely need per-file structure. Steps 1 and 2 are
additive.

Already landed: artifact unlinking now happens only after a job leaves `byID`
(PR #271), which removes the window in which a job in the queue could have no
manifest on disk. Steps 1, 2 and 3 are done — `Job.Manifest()` now returns
`(*Manifest, error)`, so reaching through an evictable manifest without
handling its absence is a compile error. Step 4 was measured and declined.
Step 5 is done. All five steps are therefore closed, and this document
describes the code as it stands rather than a target it is held to.

## What this dissolves

Recorded so these are not re-investigated as open questions:

- **#261** (methods returning nil while silently skipping) is resolved, and it
  split along the tier boundary exactly as predicted. Of the seventeen entries
  it listed, ten were manifest-tier and now route through `residentJob`,
  returning `ErrJobNotResident` where they used to return `nil`. The other
  seven are progress-tier and needed no gate at all — for two of them the
  guard was not merely redundant but wrong: `SetPar2ReleaseReason` demanded a
  manifest it never reads, so the reason a job's par2 volumes were released
  was silently discarded for precisely the non-resident jobs the on-demand
  par2 path acts on.

  `Job.ResetForRetry` is not on #261's list but has the same shape; it is
  documented in place, since both callers hydrate first and an error return
  would change an exported signature for an unreachable branch.

  Four of the ten were nearly missed. `MarkArticlesDone`, `MarkArticleDone`,
  `MarkArticlesFailed` and `MarkArticleFailed` write the guard inverted —
  `if job.manifest != nil && job.progress != nil { ...whole body... }` — so a
  search for `== nil` does not find them, and `MarkDownloadFinished` and
  `MarkJobStarted` hide their dead progress guard the same way. This is the
  third time a hand search over this surface has returned a different subset
  (#267 records the first two), which is why the invariant is now enforced by
  `TestManifestAccessIsGated` rather than by reading: it walks the package AST
  and fails any `*Queue` method that dereferences `job.manifest` without going
  through `residentJob`. Its exemption list is written to shrink — a new
  method is a failure until someone gates it or records why it does not need
  to be.

  The rule that falls out, and that `residentJob`'s doc comment now states:
  **gate on residency if and only if the method needs the manifest.** Adding a
  residency check to a progress-tier method is the same defect wearing
  caution's clothes — it refuses work the method is always able to do.

  Callers were the other half. `errors.Is(err, ErrJobNotResident)` is now
  something production code branches on rather than only tests: a job removed
  or evicted mid-flight is ordinary and logs at Debug, while anything else
  keeps its Warn. Two discarded errors in the on-demand par2 path
  (`_ = DiscardDeferredPar2`, `_ = SetPar2ReleaseReason`) were only harmless
  while the methods could not report the case that mattered.
- **#262** (`TotalRemainingBytes` under-reports) cannot recur: it reads
  progress, which is always resident.
- **#263** (data race on `Job.Manifest()`) cannot recur: handles hold immutable
  values.
- **#272** is to be closed rather than reworked. Its subject is what `Snapshot`
  should do when hydration fails; under this contract snapshots do not hydrate
  for reporting, so reworking it would build the thing this design deletes. Two
  pieces of it are worth keeping independently: a doc-comment fix, and the first
  direct tests for `queueList`, `buildSlot` and `modeAddLocalFile`.
