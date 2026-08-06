# Job size figures: content and recovery

Design for the reporting half of #318. Decides what a job's advertised size
means, which in turn decides what the tombstone work has to change.

## Problem

A job's size is one number, `TotalBytes`, that sums every file including par2
recovery volumes the job may never download. `DiscardDeferredPar2` exists to
correct that: once the CRC oracle proves the volumes unnecessary it removes them
from the manifest so the totals stop counting them.

Removing a non-final file renumbers every `file_index` after it, and `job_files`
rows are keyed by that index. That renumber is the root of #294, #308, #310 and
#317. Accounting is the only purpose removal has ever been recorded as serving,
and the only purpose it demonstrably serves.

So the size question is upstream of the renumber. Fix what a size means and the
removal becomes unnecessary.

## Decision

A job advertises two immutable figures instead of one filtered total:

- `content_bytes` — every file that is not a par2 recovery volume
- `recovery_bytes` — the recovery volumes, with `recovery_files` alongside

Both are computed once, at `Add`, from the manifest, and never recomputed.
Neither changes for the life of the job.

The split key is `isPar2Recovery`, not `isPar2File`. The par2 **index** is always
downloaded and therefore counts as content. Today's `par2Bytes` uses
`isPar2File` and so includes the index, which is the conflation this corrects.

No stored figure answers "what will we download". That is derived where needed,
from the two figures plus per-file state.

### Why immutable figures rather than a filtered total

A filtered total has to answer "does this include volumes we will not fetch",
and the answer depends on when it is read. Worse, the filtering state
(`Deferred`, and later `Discarded`) lives on `FileProgress` while the byte counts
live on the `Manifest`, so a filtered total is a property of the pair. The
manifest is evictable and the progress is not, which is how previous framings
arrived at a figure that meant different things depending on residency.

Two immutable figures have no such coupling. They are constants derived from the
file set, they cannot drift, and no call site has to decide what they mean.

## Derived remaining

`remainingBytes` is inherently mutable and is not one of the two figures. It
becomes derived rather than maintained:

```
remaining = sum over files where !Complete && !Deferred && !Discarded
            of (Bytes - BytesDownloaded - FailedBytes)
```

`Discarded` does not exist until step 3 of the sequencing below. Step 1's
derivation excludes `Complete` and `Deferred` only; the third term is added when
the flag lands, and needs no other change to the derivation.

`FailedBytes` is subtracted per file because the counter this replaces meant
unresolved bytes, not un-downloaded ones: a failed article was never counted as
downloaded, but it also stopped being "remaining" in any useful sense. Each
file's contribution is clamped at zero, since a file whose failed and
downloaded bytes together reach its size has nothing further to contribute.

`FileProgress` gains `Bytes` and `FailedBytes` fields. `Bytes` is restored from
the `job_files.bytes` column that already exists; `FailedBytes` is new, backed
by the `job_files.failed_bytes` column added in migration
`008_add_job_files_failed_bytes.sql`. Together they let the derivation run from
progress alone at any residency — one implementation, no residency split.

This deletes the `remainingBytes` field, its per-article decrements in
`markDone` and `markFailed`, and `DiscardDeferredPar2`'s
`remainingBytes -= discardedBytes` fixup. Deferring, un-deferring or discarding a
volume needs no adjustment; the next read reflects it.

Cost is an O(files) walk on reporting reads in place of an O(1) field read. Files
number in the hundreds; articles, which are untouched by this, number in the tens
of thousands.

## Derived expectation

Remaining is not enough on its own. Every consumer that turns it into a
percentage or a downloaded total pairs it with a size, and the identity

```
downloaded = size - failed - remaining
```

closes only when `size` and `remaining` share an exclusion set. Remaining
excludes `Complete` and `Deferred`; the size it is paired with must exclude
`Deferred` alone, because a completed file is still part of what the job set
out to fetch.

```
expected = sum over files where !Deferred && !Discarded of Bytes
```

This is the figure line 140 calls the job's advertised expectation. It is
derived on the same walk and the same predicate as remaining, so the two
cannot drift.

`Job.TotalBytes()` remains the immutable whole-manifest total and keeps its
logging and post-processing display callers. The consumers that must move to
the expectation are the ones that combine a size with remaining or with
failed bytes: the queue API's percentage and size-left, the history record's
downloaded figure, and the total-failure check.

**Correction to step 1's original claim.** Step 1 was specified as invisible
to users. It is not, on its own: excluding deferred volumes from remaining
while leaving the paired size whole made a freshly added on-demand-par2 job
report non-zero progress, and made history over-report downloaded bytes for
a job finalized before its volumes were discarded. The expectation above is
what makes step 1 invisible as intended, so it belongs in step 1 rather than
step 2.

## Components

| Component | Change |
|---|---|
| `Manifest` | `contentBytes`, `recoveryBytes`, `recoveryFiles` computed in `newManifest` via `isPar2Recovery` |
| `Job` | promoted scalars replacing `totalBytes` / `par2Bytes` / `par2Files` |
| `FileProgress` | gains `Bytes int64` and `FailedBytes int64`; the latter backed by `job_files.failed_bytes` (migration `008_add_job_files_failed_bytes.sql`) |
| `jobs` table | migration replacing `par2_bytes` / `par2_files` with `content_bytes` / `recovery_bytes` / `recovery_files` |
| queue API | `par2_bytes` / `par2_files` become `recovery_bytes` / `recovery_files`; `total_bytes` is replaced by the two figures |
| Svelte UI | reads the new fields; its repairability heuristic becomes more accurate as a consequence |

## Data flow

**Add.** `newManifest` computes the figures, the scalar sync promotes them to the
job, `addTx` writes the `jobs` columns and the `job_files` rows.

**Boot.** `SQLiteStore.Get` reads the figures from the `jobs` row and per-file
`Bytes` from `job_files`. Neither needs a manifest.

**Runtime.** Remaining derives on read.

## Error handling

Immutable figures have no staleness failure mode, and a derived value cannot
drift from what it describes, so most of the failure surface this replaces does
not exist here.

The one real risk is the derivation's exclusion set being wrong. That surfaces as
an incorrect percentage, which is visible, rather than as a spliced row, which is
not.

## Testing

- `content_bytes + recovery_bytes` equals the previous `total_bytes` for a fixture, so the split loses no bytes
- the par2 index lands in content, not recovery, pinning the `isPar2File` / `isPar2Recovery` correction
- derived remaining matches the previously maintained value across a simulated download
- remaining excludes deferred files, and changes when a file is un-deferred with no fixup call
- remaining is identical for the same job resident and non-resident

The last is the check every previous framing of this work failed, and it is the
one that pins the property the design exists to guarantee.

## Sequencing

1. `FileProgress.Bytes` and derived remaining. Deletes the seed, the decrements and the discard fixup. Adds the derived expectation below, without which this step *does* change reported figures — see the correction under Derived expectation.
2. `content_bytes` / `recovery_bytes` / `recovery_files`, with the migration, the API change and the UI change.
3. The `Discarded` flag, excluded from `DeferredRecoveryIndices` so a late failure cannot resurrect volumes already ruled unnecessary.
4. `DiscardDeferredPar2` marks instead of rebuilding; the article-bitset copy loop, its panic guard, and the row rewrite in `ReplaceManifest` are deleted.

Mechanics lead because every bug in this family comes from maintained state
drifting from what it describes. Removing the maintained figure first means the
later steps have nothing to keep in sync, and step 1 is invisible to users, so a
wrong derivation costs a wrong percentage rather than a wrong repair decision.

## Effect on #318

Step 3 of that issue — converting accounting call sites — largely disappears.
There is no ambiguous total left to classify, so no audit has to decide what a
number means at each site. The `Discarded` flag survives, justified by state
rather than accounting: the resurrection hazard, and the reporting #322 needs.

## Consequences accepted

- A job's advertised expectation moves as par2 decisions are made: it excludes recovery until damage forces an un-defer, and includes it after. This was decided deliberately.
- The API changes shape. There is no backwards-compatibility requirement — landing this work requires a full reset and reinstall — so this is a UI update, not a data migration concern.
- The UI's repairability check currently compares failed bytes against `par2_bytes`, which includes the index and so overstates repair capacity. Moving it to `recovery_bytes` corrects it. This is a behaviour change arriving as a consequence rather than as its own decision.
