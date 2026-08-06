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

Remaining bytes is derived, not stored. It sums the undownloaded part of every
file that is neither complete nor deferred, from `FileProgress` alone, so it
holds at any residency and needs no adjustment when a file's state changes.
`FileProgress.Bytes` exists to make that derivation independent of the
evictable manifest.

External packages read five manifest-derived scalars — `TotalBytes`,
`NumFiles`, `NumArticles`, `Par2Bytes`, `Par2Files`. These are computed once at
`Add` and never change, so they live in the always-resident tier rather than
behind a fallible handle. This generalizes the same rule remaining bytes
already follows above: a reporting path must never need a manifest.

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

`JobProgress` must be sized without loading manifests. That started as needing
only the article count per file — `job_files.articles_done` recovers it to
within 8, so `job_files` carries an explicit `article_count` column — and
startup stays O(1) in manifest size rather than decompressing every manifest at
boot.

Deriving remaining bytes rather than maintaining them widened what a
non-resident job must reconstruct. `Store.ArticleCountsByJob` now returns a
whole `FileMeta` per file — article count, `bytes`, `bytes_downloaded`,
`failed_bytes`, `complete`, `deferred` — because the derivation reads all of
them and must produce the same figure at either residency. `failed_bytes` is
the second schema change this design has needed: a permanently failed article
resolves without ever being downloaded, so it leaves no trace in
`bytes_downloaded`, and without its own column a restarted job would report its
bytes as still outstanding.

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

**A job's file set is fixed at `Add`.** The manifest blob and the `job_files`
rows are both written once, from the same `[]JobFile`, and neither changes
shape again for the life of the job. `file_index` is therefore stable for the
job's entire life: nothing after `Add` removes a file, so nothing after `Add`
renumbers one.

`DiscardDeferredPar2` is the only operation that changes a job's download
intent after `Add`, and it does not touch the file set to do it. A recovery
volume that is never downloaded — because on-demand par2 verification came
back clean — keeps its `job_files` row exactly where it was; only its
`fetch_policy` column moves, from `FetchIfNeeded` to `FetchNever`, through the
ordinary per-file write in `SQLiteStore.updateTx`. That write is unconditional
for a resident job. For a non-resident job it is not: `updateTx` gates its
whole `job_files` loop on `job.Manifest()` succeeding, so a discard on an
evicted job updates `JobProgress` in memory immediately but leaves the
persisted `fetch_policy` at its prior value until the job is promoted back to
resident and a checkpoint runs.

This removed a whole tier of machinery that used to exist to survive the
opposite design: `Store.ReplaceManifest`, which rewrote the manifest blob and
every `job_files` row together whenever a discard shrank the file set;
`Job.manifestRowsStale` and `Job.fileSetGen`, which tracked whether the
persisted rows still matched a file set that could change out from under
them; and `Queue.reconcileJobFiles`, which retried a `ReplaceManifest` a prior
checkpoint had failed to land. All of it existed because dropping a file
renumbered every `file_index` after it — the root of #294, #308, #310, #315
and #317. None of it has a reachable caller once the file set cannot change,
so all of it is gone.

**What remains is on-disk corruption, not a torn write.** `ErrManifestStale`
and `describesSameJobAs` still guard the one thing they always guarded: a
`Manifest`/`JobProgress` size mismatch would panic inside `recompute` on a
background goroutine with no `recover`, so it is reported instead of
tolerated. Their cause changed, not their purpose. Before this task the
mismatch had an ordinary cause — a `ReplaceManifest` whose blob write and
transaction could not be made atomic, so a crash between them left the file on
disk describing the pre-discard job. No write path this process performs can
produce that any more: the manifest blob is written once, before `Add` opens
its transaction, so a crash between them leaves an orphan manifest and no job
row rather than a disagreeing pair. A mismatch that reaches the guard now is a
truncated or damaged manifest blob, or a `job_files` set altered out of band —
still worth detecting, still better reported than panicked on.

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
