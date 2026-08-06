# Par2 fetch policy: tombstone instead of renumber

Design for steps 3 and 4 of `2026-08-05-job-size-figures-design.md`, which are
steps 2–4 of #318's own sequencing. Step 1 of the size-figures spec landed in
#328; step 2 (`content_bytes` / `recovery_bytes`) is deliberately not a
dependency of this work and remains open.

## Problem

`DiscardDeferredPar2` is the only operation that changes a job's file set after
`Add`, and it changes it by *removing* entries. Removing a non-final file
renumbers every `file_index` after it, and `job_files` rows are keyed by that
index, so each renumber invalidates every row past the gap.

That renumber is the root of #294, #308, #310, #315 and #317. Each was fixed by
adding a guard at one more writer, and each guard was correct. What no
individual fix could see is that every one of them guards the same single
caller.

Accounting was the only purpose removal ever served. #328 made the reported
figures derive from per-file state, so that purpose is gone. Removal is now
pure cost.

## Decision

A file carries a **fetch policy** — whether the job intends to download it —
and the discard sets that policy instead of deleting the file.

```go
// FetchPolicy records whether the job intends to download a file.
type FetchPolicy uint8

const (
    // FetchAlways is every content file, the par2 index, and any recovery
    // volume the job has decided to fetch after all.
    FetchAlways FetchPolicy = iota
    // FetchIfNeeded is a recovery volume held back pending the CRC verdict.
    FetchIfNeeded
    // FetchNever is a recovery volume the verdict proved unnecessary.
    FetchNever
)
```

This replaces `FileProgress.Deferred`. It does not sit alongside it.

The field is `FileProgress.Fetch`, and the accessor is
`JobProgress.FileFetchPolicy(fi int) FetchPolicy`, matching the existing
`FileComplete` / `FileBytes` accessor convention. `FileDeferred` is deleted
rather than kept as a shim — retaining it would preserve exactly the call sites
the compiler needs to surface.

### Why a tri-state rather than a second bool

Three properties follow from replacement that do not follow from addition.

**Contradictory states become unrepresentable.** `Discarded && !Deferred` and
its inverse have no meaning, and a two-bool encoding admits both.

**Two hazards are fixed by construction rather than by vigilance.**
`DeferredRecoveryIndices` feeds `undeferRecoveryLocked`, which runs on any
first-time permanent article failure while the job is not yet par2-recovered.
If discarded volumes appeared in that list, one late failure would re-activate
exactly the volumes the CRC oracle proved unnecessary — undoing the feature's
entire purpose. Separately, the release path gates on `HasDeferredPar2()`, so a
volume that stayed "deferred" after being discarded would re-run full CRC
verification on every subsequent completion event. Under the tri-state both
guards read `FetchIfNeeded` and neither can match a discarded volume.

**The audit becomes a compiler product.** #318 warns that this change has a
third class of call site, invisible to any classification of aggregates versus
per-index reads: per-index loops that need a skip they do not currently have.
`api.firstIncompleteFile` (`internal/api/queue.go:227`) is one. It walks for
the first `!Complete` file; a tombstoned volume is never `Complete`, so it
would become the job's reported current file for the whole post-processing
phase. It compiles today and would still compile after a bool-only change. The
bug is the *absence* of a check, and no care taken at the sites you do edit
surfaces a site you never open. Deleting `FileDeferred()` turns all 28
non-test call sites into compile errors, which is the only mechanism that
reliably reaches that third class.

### Why the name

`Deferred` and `Discarded` name two exceptions, which forces the ordinary case
to be expressed as the absence of a par2 concept — a plain RAR file described
as "not deferred, not discarded". Naming the field for the intent instead makes
the zero value a real state, and makes `FetchIfNeeded` a literal description of
what on-demand par2 does: the CRC verdict and subsequent damage are simply what
move a file between the three values.

## Storage

**Migration `009_replace_deferred_with_fetch_policy.sql`** adds
`fetch_policy INTEGER NOT NULL DEFAULT 0` and drops `deferred`, on **both**
tables that have the column: `job_files` (added in migration 002) and
`history_job_files` (007).

No dual-read path. #318 records that there is no backwards-compatibility
requirement — landing this work requires a full reset and reinstall.

The migration does backfill, in one `UPDATE` per table mapping `deferred = 1`
to `FetchIfNeeded`. An earlier draft of this section claimed `FetchAlways` was
the correct default for every row regardless; that holds only for rows that
were not deferred. For a held volume `FetchAlways` means *download it*, which
is the outcome the feature exists to avoid. The reset requirement makes the
difference unobservable in practice, but a migration that is correct on its own
terms rather than dependent on an out-of-band instruction costs one line per
table.

`Down` is lossy: `FetchNever` collapses to `deferred = 0`. That is acceptable
because `Down` is a development affordance and the verdict is re-derived by
re-verification.

The `Manifest` is untouched. Fetch policy is progress state; the manifest has
never had a concept of deferral, and giving it one would recreate the
residency-dependent coupling #328 removed.

## Call sites

28 non-test sites reference `Deferred`, `FileDeferred`, or the field in a
struct literal — 25 of them inside `internal/queue`, plus two in
`internal/postproc/filelist.go` and one in `internal/api/queue.go`. They sort
into four groups.

**Aggregates** — exclude any policy other than `FetchAlways`. A discarded file
is as absent from dispatch and accounting as a deferred one.

| Site | File |
|---|---|
| `sizeFigures` (expected and remaining) | `internal/queue/progress.go:278` |
| `recompute` | `internal/queue/progress.go:460` |
| `Job.IsComplete` | `internal/queue/job.go:849` |
| `Queue.ForEachUnfinishedArticle` | `internal/queue/queue.go:1369` |

**Serialization** — carry the value rather than filter on it. These are the
sites that must change shape, from a `bool`-to-`0`/`1` conversion to writing
the policy's numeric value.

| Site | File |
|---|---|
| `insertJobFilesTx` | `internal/queue/sqlite_store.go:304` |
| `updateTx` | `internal/queue/sqlite_store.go:724` |
| `RestoreJobProgress`, `RestoreRetryProgress` | `internal/queue/sqlite_store.go` |
| checkpoint and history JSON | `internal/queue/progress.go:590,621,657` |

**Policy-specific guards** — must name which policy, and all three mean
`FetchIfNeeded`:

| Site | Consequence of getting it wrong |
|---|---|
| `HasDeferredPar2` | full CRC re-verification on every completion event |
| `DeferredRecoveryIndices` | a late failure resurrects discarded volumes |
| `undeferRecoveryLocked` (`queue.go:2017`) | an un-defer re-activates a discarded volume |

`undeferRecoveryLocked`'s own guard is the second line of defence behind
`DeferredRecoveryIndices`: it currently skips any index that is not `Deferred`,
and reading that as `!= FetchIfNeeded` means a discarded volume is refused even
if it reaches the call by some other route.

**Per-index skips absent today** — `api.firstIncompleteFile` needs a skip it
has never had. This is the class described under the tri-state rationale above.
`internal/postproc/filelist.go:42` and `:181` are the #322 sites and are
policy-specific in a different direction: a volume that was never downloaded
counts as saved bandwidth whether the verdict has landed or not, so both read
`!= FetchAlways`.

## `DiscardDeferredPar2` after

The body collapses to a walk setting `FetchIfNeeded → FetchNever`, plus the
`dirty` store. Deleted: the `activeFiles` reconstruction, the `newManifest`
rebuild, the carried-over `par2Bytes`/`par2Files`, the bit-by-bit
done/failed/emitted copy into pre-sized bitsets, the `idx != NumArticles()`
panic guard, the `setResidency` and `setScalarsFromManifest` re-sync, the
`bumpFileSetGen` call, and the inline `ReplaceManifest` write with its
surrounding stale-flag handling.

No file set changes, so no index moves, so nothing needs rewriting. An ordinary
checkpoint persists the new policy through the existing `updateTx` path.

## Teardown

Every one of these exists only to contain the renumber, and becomes
unreferenced with it:

| Removed | Existed for |
|---|---|
| `Store.ReplaceManifest` — interface method and SQLite implementation | rewriting rows after a renumber |
| `Job.manifestRowsStale` and `updateTx`'s skip (`sqlite_store.go:709`) | #310, landed in #315 |
| `Job.fileSetGen`, `clearManifestRowsStaleIfGen` | a stale rewrite clearing a newer flag |
| `Queue.reconcileJobFiles` (`persistence.go:41-104`) | retrying the rewrite each checkpoint |

**`ErrManifestStale` and `describesSameJobAs` are kept.** An earlier draft of
this table listed them for removal as part of the un-atomic
blob-plus-transaction pair. They are not renumber containment: they convert a
`Manifest`/`JobProgress` size mismatch into a reported error instead of a panic
on a background goroutine with no `recover`, and that value is independent of
what caused the mismatch.

What the teardown removes is their last *reachable* cause. `Add` writes the
manifest blob before opening its transaction, so a crash between the two leaves
an orphan manifest and no job row — not a disagreeing pair — and with
`ReplaceManifest` gone no write path this process performs can produce one.
What remains is on-disk corruption: a truncated or damaged manifest blob.
That is the only thing the guard actually detects — `describesSameJobAs`
compares sizes only (`NumFiles`/`NumArticles` against
`len(progress.files)`/`done.Len()`) — so it does NOT cover `job_files` rows
altered out of band: `RestoreJobProgress` fills `progress.files` by
`file_index` without resizing it, so an out-of-band row change still passes
the size check and silently mispairs per-article state. The boot path
(`SQLiteStore.Get`) carries no guard at all, a gap tracked by #278, which is
still open. Deleting the guard would not remove the corruption case, only the
report of it. Both doc comments must be rewritten, since each currently
explains itself entirely in terms of the discard, and must not claim
detection of the out-of-band case the guard cannot actually see.

**#320 and #321 close as subsumed.** Both are defects inside this layer: #320
is a `manifestRowsStale` that has no residency-independent clear, so an evicted
job never heals; #321 is `Get` reconstructing scalars from rows already found
torn. Neither state can arise once nothing tears.

Verification obligation: `ReplaceManifest` must be confirmed to have no
remaining caller before its interface method is removed, and the removal must
be a compile-time consequence rather than a judgement — deleting the interface
method first makes any surviving caller a build failure.

## Retry

`FetchNever → FetchIfNeeded`. `FetchIfNeeded` and `FetchAlways` carry
unchanged.

The clean verdict was computed against one download's damage profile, and a
retry re-fetches the articles that failed, so the file contents the oracle
certified may differ. Re-deriving the verdict costs no downloads, and the
volumes are still not re-fetched — which is what #323 needs.

Both routes must implement it, or they disagree:

- `Job.ResetForRetry` mutates live progress in place.
- `RestoreRetryProgress` overlays `RetainedFile` after a history retry rebuilds
  the manifest from the NZB.

`RetainedFile` gains a `FetchPolicy` field. It must be carried explicitly: the
struct copies a fixed field list, and an uncarried field zero-values to
`FetchAlways`, which is #323 verbatim — a full re-download of volumes already
proven unnecessary. A test must pin the carry, not merely the downgrade.

## API and UI

`fileState` gains `"skipped"` for `FetchNever`; `"held"` remains
`FetchIfNeeded`. The distinction makes the on-demand par2 saving visible
per-file rather than only in the history summary.

UI changes are two lines: the union member in `ui/src/lib/types.ts:49` and the
colour case in `ui/src/lib/components/QueueRow.svelte:286`.

`Par2Held` (JSON `par2_held`) becomes `policy != FetchAlways` rather than
`FetchIfNeeded` alone. It drives the "par2 on-demand" badge, whose tooltip
already reads "downloaded only if repair is needed" — true of both non-default
policies. Left as `FetchIfNeeded` only, the badge would disappear at the exact
moment the feature succeeds. This is a semantic change under an unchanged JSON
key, and is called out here rather than left to be discovered.

## Documentation

`docs/queue-lifecycle.md` lines 213–260 document the containment layer as live
design — `ReplaceManifest`, the torn-write window, `manifestRowsStale`,
`fileSetGen`, and the `ErrManifestStale` hydration failure. That section
describes machinery this design deletes and must be rewritten, not merely
trimmed: CLAUDE.md directs agents to read this file before touching job
residency or `Manifest`/`JobProgress` access, so a stale section here misleads
exactly the reader told to trust it.

This also discharges the salvage obligation recorded when #317 was closed. That
issue's "Across a restart" material was left pending on this step precisely
because the restart story changes once the renumber is gone.

## Error handling

The failure surface shrinks rather than moving. Setting a policy is an
in-memory field write on resident progress followed by an ordinary checkpoint;
there is no second write to tear against, so `DiscardDeferredPar2` loses its
error return path entirely and the operation cannot partially apply.

What remains is the ordinary checkpoint failure, already handled: the policy
stays correct in memory and is rewritten on the next tick. A crash before any
checkpoint loses the discard, and the CRC verdict is simply re-derived on the
next completion event — the same self-healing the pre-verdict state already
has.

## Testing

- `DeferredRecoveryIndices` excludes `FetchNever`, and the test **fails without
  the exclusion**. #318 requires this one by name; it is the resurrection
  hazard.
- A late permanent article failure after a discard re-activates no volumes.
- `HasDeferredPar2` is false after a discard, so the release path does not
  re-run verification.
- `firstIncompleteFile` skips a `FetchNever` volume rather than reporting it as
  the job's current file.
- #322: the savings report and the "verified clean" status line fire on the
  clean path.
- #323: a retry after a discard re-fetches no recovery volume, and leaves each
  at `FetchIfNeeded`.
- `RetainedFile` carries the policy across a history retry — pinned separately
  from the downgrade, since an uncarried field fails silently at
  `FetchAlways`.
- `file_index` is stable across a discard, asserted resident and non-resident.
- Round trip: policy survives checkpoint, restart, and promotion — for a
  discard taken while the job is resident. **A discard taken while the job is
  non-resident does not survive promotion**, and the round-trip test pins that
  loss rather than asserting against it. `PromoteNext` rebuilds `JobProgress`
  with `newJobProgress` when the manifest is nil, discarding the in-memory
  `FetchNever`, and `RestoreJobProgress` then assigns the stale row's value
  back over it. This is bounded and self-correcting — the file returns to
  `FetchIfNeeded`, so `HasDeferredPar2()` is true again and the next
  verification re-discards it — at a cost of one redundant CRC pass, with no
  re-download and no data loss. Making it survive needs a
  residency-independent per-file write path, which does not exist.

## Consequences accepted

- `Job.TotalBytes()` no longer shrinks when a discard fires. Under #328 the
  reported figures derive from `ExpectedBytes()`, which excludes non-fetched
  files, so no user-visible total changes — but the immutable scalar now
  genuinely stays immutable.
- Deferred and discarded volumes keep their `job_files` rows for the life of
  the job. That is the point of the design, and it is bounded by the file
  count, which numbers in the hundreds.
- The diff is large and spans `internal/queue`, `internal/api`,
  `internal/postproc` and `ui/`, and includes a schema migration. Per the
  project's escalation rules — persistence, a `goose` migration, a public
  interface change, and a diff over three packages — review and verification
  run on Opus regardless of implementation tier.

## Sequencing

1. `FetchPolicy` replaces `Deferred`, with migration 009 and all 28 call sites.
   `DeferredRecoveryIndices` and `HasDeferredPar2` read `FetchIfNeeded` in this
   step, with the failing-without-it test. No behaviour change yet: nothing
   sets `FetchNever`.
2. `DiscardDeferredPar2` marks instead of rebuilding. Closes #322 and #323.
3. Retry downgrade on both routes, including the `RetainedFile` carry.
4. Teardown of the containment layer, and the `docs/queue-lifecycle.md`
   rewrite. Closes #320 and #321.
5. API `"skipped"` state, the `par2_held` semantic change, and the two UI
   lines.

Step 1 is the largest and is behaviour-neutral, which makes it the safest place
to absorb the call-site churn. Step 2 is small and is where the observable fix
lands. Step 4 is deletion only and is safe to review last, since a surviving
caller surfaces as a build failure rather than as a runtime state.
