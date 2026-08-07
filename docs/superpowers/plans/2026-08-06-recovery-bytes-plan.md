# Recovery bytes: re-key the par2 figures off the index

Implementation plan for #318 step 2, the last unlanded step of the job
size-figures design. Steps 1, 3 and 4 landed as #328 and #331.

**Goal:** a job's advertised repair capacity counts recovery volumes only.
Today it also counts the par2 index, which is always downloaded and
contributes nothing to repair.

**Closes:** #325, #326. **Parent:** #318.

## Two PRs, in this order

#325 and #326 have **no dependency** on the re-key: #326 touches only
`TotalBytes`/`ExpectedBytes`, neither of which is renamed, and #325 builds on
the `held`/`skipped` per-file states that already shipped in #331. Bundling
them behind a schema migration and a wire-format rename would block two small
fixes on the riskiest change in the set.

- **PR A — the two independent fixes.** Commits A1 (#326) and A2 (#325).
  No migration, no wire change, no gate change. Ships first.
- **PR B — the re-key.** One commit. Rebases on PR A, because both touch
  `QueueRow.svelte` (A2 adds the subtotal; B renames the fields it reads).

PR B is a single commit by necessity: a cross-package method rename is atomic.
`internal/api/queue.go:379` calls `j.Par2Bytes()` and `internal/queue/job.go:219`
calls `m.Par2Bytes()`, so deleting either accessor breaks packages a later
commit would fix, against the project rule that every commit passes
`go build ./... && go test ./...`. The gates' must-move-together constraint
points the same way.

## Correction: the index-has-no-recovery-blocks premise is false

Everything below that says a par2 index "carries no recovery blocks" overstates
what can be known. The claim was checked after the plan was written and does
not hold in general.

- The PAR2 specification says a file carrying recovery slices **should** be
  named with a `.volNNN+MM` segment. It does not require it, does not forbid
  recovery slices in a plainly-named `.par2`, and does not define an index file
  at all — that is a convention of the tooling.
- `par2` reads packets, not names. A recovery volume renamed to drop its `.vol`
  segment still repairs, verified locally.
- SABnzbd, the reference implementation, **opens each base `.par2` and counts
  recovery packets byte by byte** (`sabnzbd/par2file.py`, `analyse_par2`),
  precisely because the filename cannot tell you.
- par2cmdline never produces such a file — 10 configurations from 1 KB sources
  to 100% redundancy all emitted a separate index with zero `RecvSlic` packets.
  So the common case holds; the guarantee does not.

What changes as a result: the figures are **recognized** capacity, not total
capacity, and zero means "nothing matched the convention" rather than "nothing
can repair this job". Both abort gates now withhold the beyond-repair verdict
when a job carries par2 files but none were recognized as volumes. The
accounting, API, persistence and UI work is unaffected.

## What research changed about the design

`docs/superpowers/specs/2026-08-05-job-size-figures-design.md` § Components
is wrong in three places. The plan follows the findings; the design doc is
corrected as part of the work.

| Design says | Finding |
|---|---|
| queue API: "`total_bytes` is replaced by the two figures" | `queueSlot` has no `total_bytes`. Size reaches clients as `bytes`/`size`/`mb`, fed from `p.ExpectedBytes()`. Nothing to replace. |
| migration adds `content_bytes`/`recovery_bytes`/`recovery_files` | `content_bytes` has no consumer and is exactly `totalBytes - recoveryBytes` from two scalars already present. Adding it fails the dead-on-arrival check. Two columns, not three. |
| (silent) | Two repair-capacity gates and two release gates read this figure. The design enumerates none of them. |

`par2_bytes`/`par2_files` are not wire fields anywhere in the reference Python
tree — the only occurrence is a local variable in `deobfuscate_filenames.py`,
not a response key — so the rename breaks no third-party client. The
SABnzbd-compatible names (`bytes`, `size`, `mb`, `sizeleft`, `mbleft`) are
untouched.

## Global constraints

- The partition key for the *capacity* figure is `manifestFile.isPar2Recovery`,
  never `isPar2File`.
- **Amended during implementation — "content" names two different quantities,
  and they partition on different keys.** As originally written this bullet
  said `content` means "not a recovery volume" and therefore **includes the
  par2 index**. That holds for the manifest's byte partition, where the index
  is downloaded like any other file. It does *not* hold for the numerator of
  the repair comparison: `JobProgress.ContentFailedBytes()` tests `!IsPar2` and
  excludes the index, because a failed par2 file of either kind is lost
  capacity rather than damage needing repair. Do not carry one sense of the
  word into the other.
- **Amended during implementation — there are three consumers, not two.** As
  originally written this bullet said *both* repair-capacity gates read one
  quantity and must change together. `failMsgForJob` and the dispatcher's
  Early Health Gate are two; the queue listing's `buildSlot` is the third, and
  it was missed because the UI re-derives the comparison from raw fields rather
  than calling the accessors a reference search would find. It shipped with the
  denominator moved and the numerator not, which is exactly the interim
  disagreement this bullet exists to forbid — the defect shape #326 documents.
- No `//nocover:` on branching code, no dummy tests, no weakened gates.
- Never modify an existing migration file.

## Derivation (retraceable)

`totalBytes` is the sum of `f.bytes` over every file. Define the predicate
`P(f)` as `f.isPar2Recovery`. It is a total boolean, so the sets `{f : P(f)}`
and `{f : not P(f)}` are disjoint and exhaustive, giving
`contentBytes + recoveryBytes = totalBytes` — a sum over a partition equals
the sum over the whole. Overflow does not break it: `int64` addition is
associative and commutative mod 2^64, so re-partitioning the same addends
yields the same wrapped total.

Index placement: `job.go:673` sets `isRecovery := isPar2 && isRecoveryVolume(subject)`.
For `movie.par2` that is `true && false = false`, so the index lands in
content. That is the correction, not an accident.

Two corollaries, both worth pinning:

- Only recovery volumes are ever non-`FetchAlways` (`job.go:686`), so
  `contentBytes <= ExpectedBytes() <= contentBytes + recoveryBytes` always,
  with the lower bound tight iff no volume has been released.
- `recoveryVolumePattern` requires a literal `.par2` (`par2name.go:11`) and
  `NewJob` computes `isRecovery := isPar2 && …`, so `IsPar2Recovery` is a
  **subset** of `isPar2File` on every construction path. Therefore
  `RecoveryBytes <= Par2Bytes` always: the denominator never grows.

### What the byte comparison can and cannot prove

Grounding this matters, because the plan improves a proxy and should not be
read as making it exact.

Par2 repair is decided by **block counts**, never bytes:
`RepairPossible() = UsableParityShardCount >= UnusableDataShardCount`
(`par2engine/par2/decoder.go`), and the cmdline path parses "You need N more
recovery blocks" (`internal/par2/par2.go:196`). Block counts are not derivable
at Add time — the NZB carries only subjects and poster-claimed sizes, and
`internal/par2/parser.go:200-203` deliberately stops scanning before recovery
packets on files over 10 MiB, so `ParsedSet.RecoveryBlocks` is unreliable by
design and has no production consumer.

Bytes are therefore the only signal available at dispatch time. The comparison
is sound in **one direction only**:

- `failedBytes > recoveryBytes` **does** imply unrepairable. Slices are
  uniform and `recoveryBytes >= nSlices * sliceSize`, so exceeding it means
  more damaged blocks than available parity blocks. This is the direction both
  abort gates use, and removing the index — bytes with zero blocks — makes the
  bound tighter while keeping it sound. That is the correction.
- `failedBytes <= recoveryBytes` implies **nothing**. Scattered damage
  destroys more blocks than its byte count suggests (each failed article kills
  `ceil(bytes/sliceSize)+1` blocks), and critical packets are replicated into
  every volume, inflating `recoveryBytes` above its slice payload.

`QueueRow.svelte:340` renders exactly the unsound direction
(`failed_bytes <= par2_bytes` → "Repairable"). This change makes that verdict
less optimistic; it does not make it correct. See 1e.

### Blast radius, stated precisely

The re-key makes the repair-capacity denominator **weakly** smaller, not
strictly. It is neutral for the two commonest job shapes — a job with no par2
at all (both figures zero; it already aborts on the first failed byte), and a
job with volumes but no separate index. Observable behaviour changes for
exactly one shape: the **index-only job** (a par2 index present, zero recovery
volumes), which flips from "exceeds repair capacity" to "no par2 files
available" and now trips the dispatcher's hopeless gate on first failure.

The index measures 12-37% of total par2 bytes across the repository's real
par2 fixtures, so the denominator shrinks by a meaningful fraction. That
fraction sizes the *fixture*, not the population: it changes an actual
outcome only for the index-only shape, because every other shape either has
no index to remove or fails on the same side of the comparison either way.
The change is best described as weakly growing the abort set, never strictly.

---

## PR B: the re-key

One commit. See "Two PRs, in this order" for why it cannot be subdivided.

**Files:** `internal/queue/manifest.go`, `internal/queue/job.go`,
`internal/queue/snapshot.go`, `internal/queue/sqlite_store.go`,
`internal/queue/queue.go`,
`internal/downloader/dispatch.go`, `internal/app/app.go`,
`internal/api/queue.go`, `ui/src/lib/types.ts`,
`ui/src/lib/components/QueueRow.svelte`, `ui/src/lib/components/QueueRow.test.ts`,
`test/integration/contract_test.go`, `internal/downloader/dispatch_test.go`,
`internal/queue/job_queue_helpers_test.go`,
`internal/queue/clone_completeness_test.go`,
`internal/queue/residency_race_test.go`, `internal/queue/snapshot_test.go`,
`docs/superpowers/specs/2026-08-05-job-size-figures-design.md`;
create `internal/history/migrations/010_replace_jobs_par2_scalars_with_recovery.sql`,
`internal/queue/manifest_recovery_test.go`,
`internal/history/migration_010_test.go`.

### 1a. Manifest

Replace `par2Bytes int64` / `par2Files int` with `recoveryBytes int64` /
`recoveryFiles int`, computed from `f.IsPar2Recovery`, not `isPar2File`.
Delete `Par2Bytes()`/`Par2Files()`; add:

```go
// RecoveryBytes returns the summed size of the job's par2 recovery
// volumes, excluding the par2 index — the index is always downloaded and
// carries no recovery blocks.
//
// This is the best pre-download proxy for repair capacity, not a statement
// of it. Repairability is decided by block counts
// (UsableParityShardCount >= UnusableDataShardCount), which nothing can
// know before the volumes are fetched and parsed. See the plan's
// "What the byte comparison can and cannot prove".
func (m *Manifest) RecoveryBytes() int64 { return m.recoveryBytes }

// RecoveryFiles returns the count of par2 recovery volumes, excluding the
// index. See RecoveryBytes.
func (m *Manifest) RecoveryFiles() int { return m.recoveryFiles }
```

Do **not** add `ContentBytes()`. Nothing branches on it; tests assert
`TotalBytes() - RecoveryBytes()` directly.

Drop the persisted copy: remove `Par2Bytes`/`Par2Files` from `manifestJSON`
and recompute in `UnmarshalJSON` via a helper shared with `newManifest`:

```go
// recoveryFigures sums the recovery volumes in files. Shared by
// newManifest and UnmarshalJSON so the two cannot disagree.
func recoveryFigures(files []manifestFile) (bytes int64, count int) {
	for _, f := range files {
		if f.isPar2Recovery {
			bytes += f.bytes
			count++
		}
	}
	return bytes, count
}
```

Exact, because `is_par2_recovery` is already persisted per file
(`manifestJSONFile.IsPar2Recovery`, `manifest.go:187`). The figures were only
persisted to preserve a staleness `DiscardDeferredPar2` used to leave, and
#331 removed the rebuild that created it.

Dropping the two JSON keys is a **soft landing**, not a break: `encoding/json`
ignores keys absent from the target struct, and an older manifest still loads
correctly because the figures are recomputed from per-file data it already
carries. Say so in the doc rather than leaving it looking riskier than it is.

**Prose that must change or become false:**

- `newManifest` doc still describes `DiscardDeferredPar2` rebuilding and
  overwriting the pair. Both claims false since #331.
- `dropMessageIDIndex` doc still describes "DiscardDeferredPar2's equivalent
  … builds an entirely fresh Manifest".
- `manifestJSON` doc still describes "the deliberate staleness
  DiscardDeferredPar2 leaves" and "since #294 the discard rewrites this file".
- `isPar2File`'s doc claims it is "shared by newManifest and NewJob so the
  classification heuristic can't drift". After 1a `newManifest` no longer calls
  it; `job.go:669` is the only caller. Rewrite.
- `Job.Par2Bytes`/`Par2Files` accessor docs (`job.go:288-297`) and the
  five-scalar list in `Manifest()`'s doc (`job.go:327-328`).
- `queueFile.State`'s doc (`api/queue.go:166`) lists four states and omits
  `held`/`skipped` — already stale, and PR A commit A2 depends on those two.
- `sqlite_store_test.go:982-991`, the doc on
  `TestSQLiteStore_GetNonResidentScalarsFromJobFiles`, restates the same
  "cannot be derived from job_files" rationale being retracted in
  `setAggregateScalarsFromFiles` and migration 010. Retract it in all three
  places or the contradiction just moves.
- `failmsg_test.go:16-17` ("a `.par2` suffix is what makes it count toward
  Par2Bytes/Par2Files") and
  `docs/superpowers/specs/2026-08-06-par2-fetch-policy-design.md:170`
  ("the carried-over `par2Bytes`/`par2Files`").

### 1b. Job scalars and persistence

Rename the promoted pair to `recoveryBytes`/`recoveryFiles`; accessors become
`RecoveryBytes()`/`RecoveryFiles()`; `setPar2ScalarsFromStore` becomes
`setRecoveryScalarsFromStore`. `setScalarsFromManifest` (`job.go:219`) reads
the new manifest accessors — the read-before-lock shape is unchanged, so
there is no critical-section regression. `sqlite_store.go:243,259,323,417`
follow the column rename.

**Rewrite `setAggregateScalarsFromFiles`'s doc.** It currently explains the
columns exist because `is_par2_recovery` aggregation would undercount. After
this change that is exactly backwards — the aggregate is now the *correct*
value. State the real reason: the `job_files` aggregate in `Get`
(`sqlite_store.go:420-430`) **fails soft**, leaving zero on error, while the
pair comes unconditionally from the jobs row (`:416-418`). A zero recovery
figure is a wrong repairability verdict, not an absent one — `QueueRow.svelte:339`
renders it as "No repair data". The jobs row is the infallible tier
(`docs/queue-lifecycle.md:63-69`).

### 1c. Migration 010

Both halves, per-statement `StatementBegin`/`End` in 009's style:

```sql
-- +goose Up
-- +goose StatementBegin
ALTER TABLE jobs ADD COLUMN recovery_bytes INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE jobs ADD COLUMN recovery_files INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE jobs SET
  recovery_bytes = (SELECT COALESCE(SUM(bytes), 0) FROM job_files
                    WHERE job_files.job_id = jobs.id AND job_files.is_par2_recovery = 1),
  recovery_files = (SELECT COUNT(*) FROM job_files
                    WHERE job_files.job_id = jobs.id AND job_files.is_par2_recovery = 1);
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE jobs DROP COLUMN par2_bytes;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE jobs DROP COLUMN par2_files;
-- +goose StatementEnd
```

Down restores `par2_bytes`/`par2_files`, back-fills from the recovery
aggregate, then drops the new columns.

Three things the migration's comment must record:

- The backfill is **exact where applicable, not total**. `addTx` writes
  `job_files` only when a manifest is in hand (`sqlite_store.go:266-269`), so a
  job with no rows lands on 0 — which renders "No repair data".
- The Down is **lossy**: par2 index bytes cannot be recovered from
  `job_files`, so a down-migrated row undercounts by the index. (009's Down is
  also lossy — it collapses `fetch_policy` 0/1/2 into `deferred` 0/1 — but
  documents nothing. This improves on 009 rather than matching it.)
- `005_add_jobs_par2_scalars.sql:6-10` asserts these columns "cannot be derived
  from job_files". That is false after this change. 005 must not be edited, so
  010's comment retracts it explicitly.

#318 states no backfill is required because landing this needs a full reset.
It is included anyway: four lines, exact, and the alternative is a zero that
reads as a wrong verdict rather than a missing number.

### 1d. All four control-flow gates

1. `app.go:1712-1720` `failMsgForJob` — `job.RecoveryBytes()`/`RecoveryFiles()`;
   the message's "(%d par2 files)" becomes recovery volumes.
2. `queue.UnfinishedArticle.Par2Bytes` → `RecoveryBytes`, populated at
   `queue.go:1399` from `m.RecoveryBytes()`.
3. `dispatch.go:72` Early Health Gate — `a.FailedBytes > a.RecoveryBytes`.
   Reached through a struct field, so no reference search on the method
   surfaces it. Also update the struct comment at `dispatch.go:26` and the log
   string at `dispatch.go:150`, both of which name `par2Bytes`.

   This removes the index from the dispatcher's denominator; it does **not**
   make that denominator a true statement of fetchable capacity. `RecoveryBytes`
   is manifest-derived and immutable, so for an on-demand-par2 job it still
   counts volumes that may never be downloaded — and the two held states are
   **not** alike:

   - `FetchIfNeeded` is genuinely reachable: a damage signal calls
     `undeferRecoveryLocked` and the volume becomes `FetchAlways`.
   - `FetchNever` is **terminal within a run**. `undeferRecoveryLocked`
     (`queue.go:1914-1930`) skips any file whose `Fetch != FetchIfNeeded`, so
     nothing promotes it back. Only `ResetForRetry` (`job.go:849-850`)
     downgrades it, on a whole-job retry.

   So for a job whose volumes were ruled unnecessary, the gate counts capacity
   that is unreachable this run. The error is in the lenient direction — the
   job is *less* likely to be declared hopeless — so it is safe, but it is a
   known imprecision, not a property to describe as correct. Do not write the
   "a damage signal un-defers them" claim into `dispatch.go:26`; it is false
   for `FetchNever`.
4. `queue.go:1727` and `:1778` — `Par2Files() > 0` → `RecoveryFiles() > 0`;
   log keys `par2_bytes` → `recovery_bytes`.

Gate 4 is a **guard tightening with no observable behaviour change**, and the
plan claims nothing more. `DeferredRecoveryIndices` returns only
`FetchIfNeeded` files (`progress.go:440-448`), which only recovery volumes ever
are, so for an index-only job the slice is already empty and
`undeferRecoveryLocked` already returns false with no side effects. Do not
write a test asserting a behaviour change here; it would be a change-detector.

Why gates 1 and 3 cannot be split across commits: `app.go:1518`'s
`OnJobHopeless` passes `failMsgForJob(snap)` straight into `maybeFinalize`
with **no fallback string**, unlike `app.go:257-267`. If the dispatcher gate
fires on a quantity `failMsgForJob` disagrees with, the job finalizes with an
*empty* reason — not merely an inconsistent one.

### 1e. API and UI field rename

`queueSlot.Par2Bytes`/`Par2Files` (`api/queue.go:126-127`) →
`RecoveryBytes`/`RecoveryFiles`, JSON `recovery_bytes`/`recovery_files`;
`buildSlot:379-380` reads the renamed accessors; `contract_test.go:86` field
list follows. `ui/src/lib/types.ts:24-25`, `QueueRow.svelte:339-340,510-512`
and `QueueRow.test.ts:50-51` rename with them — the UI ships in this commit
because a UI still reading `par2_bytes` against a server sending
`recovery_bytes` is a broken working tree, even though its own tests would
stay self-consistent and green.

`QueueRow.svelte:339-340`'s repairability heuristic gets **less optimistic**,
not correct: it compares failed bytes against recovery-only capacity instead
of capacity inflated by the index. Its `<=` test is the unsound direction (see
"What the byte comparison can and cannot prove"), so it remains a heuristic.
Keep the logic and change the field — but consider relabelling "Repairable" to
"Possibly repairable", since the string currently asserts more than the
comparison supports. Flag that as a question for review rather than deciding
it here; it is a copy change with its own opinions attached.

Also update the prose that names the old pair: `docs/reviews/lane5-frontend.md:192`
(slot key set), `docs/queue-lifecycle.md:65` and `:317` (the five-scalar
always-resident tier), and `scripts/nntpfaultproxy/README.md:108`.
`docs/sabnzbd_spec.md` does not document these fields — verified, no spec
change needed.

### Tests for PR B

- manifest: a fixture with content + par2 index + two recovery volumes —
  `RecoveryBytes()` equals the two volumes only, `RecoveryFiles() == 2`, and
  `TotalBytes() - RecoveryBytes()` equals content plus index. Pins the
  index-in-content correction.
- manifest: marshal/unmarshal round-trip reproduces both figures; a manifest
  with zero recovery volumes reports `0, 0` rather than counting the index.
- job: resident and non-resident reads agree for the same job — already
  covered end-to-end by `sqlite_store_test.go:992-1060` and
  `queue_nonresident_test.go`, both of which this commit renames. Verify they
  still pin the property after the rename rather than adding a third copy.
- migration, in `internal/history/migration_010_test.go` (**package history**,
  not the queue package): build a goose provider over the package-level
  `embedMigrations` (`db.go:27`) exactly as `Open` does, `UpTo` 009, seed a job
  with an index and two volumes, `UpTo` 010, assert the backfill produced the
  recovery-only figure, then `Down` and assert the columns are restored. This
  placement is deliberate: `Open` runs `provider.Up` straight to head with no
  exported hook, so a queue-package test cannot drive goose to a version, and
  adding one would be a cross-package interface change.
- `failmsg_test.go`: an **index-only** fixture (index, zero volumes) now takes
  the "no par2 files available" branch instead of "exceeds repair capacity".
  No such fixture exists today. This pins the message reclassification.
- dispatcher: the same index-only job trips the hopeless gate on first failure.
- **the split-gate fixture** — this is the one that pins PR B being one commit,
  and it must be a different shape from the two above. An index-only job
  cannot expose the empty-reason hazard: `failMsgForJob` returns non-empty
  both before the change (`failed > par2Bytes`) and after
  (`recoveryBytes == 0 && failed > 0`), so a test built on it passes on
  unpatched code and on a half-applied change alike.

  The hazard needs `recoveryBytes < failedBytes <= par2Bytes`. Use content
  1000 B + index 50 B + one recovery volume 100 B, failing 120 B:

  | | `par2Bytes = 150` (old) | `recoveryBytes = 100` (new) |
  |---|---|---|
  | `dispatch.go:72` | `120 > 150` false → not hopeless | `120 > 100` true → **hopeless** |
  | `failMsgForJob` | `120 <= 150` → **`""`** | `120 > 100` → "exceeds repair capacity" |

  Split the commit and the top-right/bottom-left diagonal is what runs:
  the dispatcher declares the job hopeless and `app.go:1518` finalizes it with
  an empty reason. Assert that a job in this state yields a non-empty failure
  message *and* is marked hopeless. That assertion is red on unpatched code
  and on either gate moving alone.
- `recoveryFigures` gets a direct test. It is a new unexported helper in a
  touched file, so `check_test_alignment` will require one, and reaching it
  only through `RecoveryBytes()` will not satisfy the gate.

### Fixture deltas — the exact sites, verified

Any fixture whose file set contains a **bare `.par2` index** sees its figures
drop by the index's bytes and one file. A repo-wide sweep finds exactly three
such fixtures. Do not go hunting beyond them, and do not bulk-edit expected
values to match new output.

| Site | Today | After | Assertion style |
|---|---|---|---|
| `internal/queue/ondemand_par2_test.go:17` (`movie.par2`, 50 B) | 550 B / 2 files | 500 B / 1 file | relative (unchanged-across-discard, `:322,:360-364,:389-393`) — keeps passing |
| `internal/queue/persistence_test.go:549` (`set.par2`, 50 B) | 750 B / 3 files | 700 B / 2 files | relative (marshal/unmarshal round-trip, `:642-646`) — keeps passing |
| `internal/downloader/dispatch_test.go:389` (`test.par2`, 100 B) | 100 B | **0 B** | absolute threshold — **silently degrades** |

The first two are safe: they compare a value against itself across an
operation, so the change of value does not change the verdict.

**`dispatch_test.go` is the dangerous one.**
`TestBuildDispatchPlan_HopelessJobNotDispatched` builds an index-only job
(`test.bin` 1000 B + `test.par2` 100 B), fails 1000 bytes, and asserts the job
is hopeless. Today that pins `1000 > 100`. Afterwards it pins `1000 > 0` — it
still passes, but it has stopped testing a threshold and now only tests that
any failure with no recovery data is hopeless. Its comment at `:385-386` ("the
100-byte par2 file gives Par2Bytes=100 from real NZB classification") becomes
false.

Fix it deliberately: give the fixture a real recovery volume so it pins a
genuine threshold again, and add a separate index-only case asserting the
zero-capacity path. One test must not cover both, or the threshold stops being
pinned the moment the classification changes again.

Two files that look like they belong on this list do **not**:
`job_scalars_test.go` and `sqlite_store_test.go` both use `.vol01+02.par2`
subjects, so they contain no bare index and have zero delta. Their fixture
guards that would fail on a zero (`job_scalars_test.go:209`,
`sqlite_store_test.go:1027`, `queue_nonresident_test.go:72`) all stay
satisfied.

---

## PR A, commit A1: one denominator for the stage log (#326)

**Files:** `internal/postproc/filelist.go`; test in `internal/postproc/`.

`filelist.go:70` divides by `m.TotalBytes()` while the history record divides
by `p.ExpectedBytes()` (`internal/app/history_helper.go:70`). Move line 70 onto
`p.ExpectedBytes()` (`p` is already in scope at line 22) so the two share a
walk and a predicate.

`heldBytes` is **replaced, not deleted**. It is read at `:55` (branch
selector), `:57` (`TotalBytes()-heldBytes`), `:58` (`saved %s`) and `:81` (the
#322 "verified clean … saved %s" line). Deleting the accumulation would leave
three dangling references and drop a user-facing figure. Assign it from
`m.TotalBytes() - p.ExpectedBytes()` instead — the same quantity, derived once
rather than hand-rolled.

One caveat to record in a comment rather than discover later: the two
expressions are not the same *predicate*. Today's loop sums recovery files
with `Fetch != FetchAlways`; `TotalBytes() - ExpectedBytes()` sums **all**
files with `Fetch != FetchAlways` (`sizeFigures`, `progress.go:324-341`). They
coincide only because nothing but a recovery volume is ever moved off
`FetchAlways` (`job.go:757-765`) — a property of the callers, not of the type.
`heldVols` stays recovery-only and is printed next to `heldBytes` at `:80-81`,
so if that invariant ever breaks, the count and the bytes on one line start
describing different sets.

Keep the loop at `:36-47`: `heldVols`/`recoveryVols` are counts with no
`JobProgress` accessor.

The substitution also widens the zero-denominator case: `ExpectedBytes()` can
be 0 while `TotalBytes()` is positive (every file non-`FetchAlways`), where
the old denominator could not. Nothing can divide by it today, because the
enclosing `p.FailedBytes() > 0` branch cannot fire when nothing was
dispatched — but that is an inherited invariant, not a check. Say so in a
comment at the division.

**Test:** a job with held volumes and real failures agrees between the stage
log and the history record. Note the two are not directly comparable as
printed — `downloadCompleteness` (`internal/app/history_helper.go:19-26`)
stores an `int64`-truncated *success* percentage while `filelist.go:70`
formats a `%.1f` *failure* percentage — so the test must compare the derived
quantities and account for the truncation, not the two rendered strings.

---

## PR A, commit A2: held-back subtotal in the drawer (#325)

**Files:** `ui/src/lib/components/QueueRow.svelte`,
`ui/src/lib/components/QueueRow.test.ts`.

#325 is **additive, not corrective**: the drawer computes no total today —
nothing sums `f.bytes`; the header shows only `files.length` (`:682`). So
there is no wrong number to fix, only an absent reconciliation.

Add a `$derived` partitioning `files` on `state` (`held`/`skipped` versus the
rest) and render the held-back subtotal in the Files header, so the drawer
reconciles against the row's size. The per-file `state` field already carries
what this needs (`api/queue.go:250-256` emits `held` and `skipped`), which is
the per-file flag #325 argued for as its Option 1 — this delivers Option 1's
data with Option 2's presentation.

New test required; no existing test covers a total.

---

## Impact list

| Site | Receiver | Kind |
|---|---|---|
| `internal/api/queue.go:126-127,379-380` | Job | wire shape + display |
| `internal/app/app.go:1712,1715,1716,1719,1724` | Job | **control flow** — `:1724` is the branch whose verdict actually changes for an index-only job |
| `internal/app/app.go:1518` | — | consumes the message with no fallback |
| `internal/queue/sqlite_store.go:243,259,323,417` | Job | persistence |
| `internal/queue/job.go:219` | Manifest | pair sync under one lock |
| `internal/queue/snapshot.go:169-170` | Job | `cloneJob` copies the pair onto every snapshot |
| `internal/queue/queue.go:1399` | Manifest | populates `UnfinishedArticle` |
| `internal/queue/queue.go:1720,1776` | Manifest | reads feeding the log |
| `internal/queue/queue.go:1741,1792` | — | the `q.log.Warn` calls carrying the `par2_bytes` key |
| `internal/queue/queue.go:1727,1778` | Manifest | **control flow** (release gate) |
| `internal/downloader/dispatch.go:26,72,150` | struct field | **control flow**, invisible to reference search |
| `internal/queue/manifest.go:72` | — | `isPar2File` caller, removed by 1a |
| `internal/queue/job.go:669` | — | `isPar2File`'s only remaining caller |

Test files touching the pair: `queue_nonresident_test.go`,
`clone_completeness_test.go`, `job_queue_helpers_test.go`, `job_scalars_test.go`,
`ondemand_par2_test.go`, `residency_race_test.go`, `snapshot_test.go`,
`sqlite_store_test.go`, `persistence_test.go`, `ingest_test.go`,
`failmsg_test.go`.

## Quality gates

`go fix ./...`, `goimports -w .`, `go vet ./...`, `go test -race ./...`,
`golangci-lint run ./...`, `./scripts/run_tests.sh`, and — required by PR B —
`go test -tags=integration ./test/integration/...`, since `contract_test.go`
is behind the `integration` tag and the default suite would pass while the
wire contract broke.

Budget for `check_coverage` on two whole functions, not two lines. PR B
touches `failMsgForJob` and PR A commit A1 touches one line inside
`buildDownloadFileList` (~90 lines, many branches); the gate measures the
whole enclosing function against 80%. Per AGENTS.md's gate-semantics table
that is the gate working as designed, not a misfire — but it is real work,
and the fix is a genuine test, never a `//nocover:`.

## Rollback and observability

Neither is free here, and #318's "a full reset is required" stance covers both
only by implication. Name them:

- **Binary rollback after migration 010 is a hard boot failure**, not a
  degradation: the previous build `SELECT`s `par2_bytes`/`par2_files`, which no
  longer exist. Rolling back means restoring a database backup, not just
  redeploying. The lossy Down recovers the schema but undercounts every row by
  its index bytes.
- **The log-key rename is a silent break for log-based tooling.** `par2_bytes`
  becomes `recovery_bytes` at `queue.go:1741`, `:1792` and `dispatch.go:150`.
  There is no deprecation window; anything grepping those keys goes quiet
  rather than erroring.

## A user-visible consequence worth naming

The design spec accepts that "a job's advertised expectation moves as par2
decisions are made". PR A commit A2 is what puts that on screen. `slot.bytes` is
`ExpectedBytes()`, so when damage releases the volumes the row's total
**grows** while the drawer's held-back subtotal drops to zero, in the same
refresh — and the percentage goes **backwards** mid-download. Third-party
clients polling `mb`/`mbleft` see it too.

This is pre-existing from #328, not introduced here, but A2 renders it
next to a subtotal that invites the comparison. Either add a test for the
release transition or state the behaviour in the commit message; do not let it
arrive unremarked.

## Inconclusive / Deferred items

- deferred — #329, the two retry routes disagreeing about a damage-released
  volume.
  reason: needs new manifest state to carry the on-demand opt-in; neither
  created nor removed by this work.
  resolution-point: after this lands, when the opt-in's home is decided.

- deferred — #330's two parked items (app.go's self-contradiction about
  `ErrNotFound`; the absent guard on migration 009's CHECK).
  reason: independent of the figures.
  resolution-point: #330.

- deferred — #306, `RestoreRetryProgress`'s dead `BytesDownloaded` assignment
  and its rationale citing the deleted `RemainingBytesByJob`.
  reason: adjacent but distinct; half-fixed by #328.
  resolution-point: #306.

- deferred — `manifestJSON.TotalBytes` stays persisted while the recovery pair
  becomes derived, though `totalBytes` is equally derivable from the file list.
  reason: out of scope; deriving it too is a separate simplification with its
  own round-trip risk.
  resolution-point: file after this lands if the asymmetry proves confusing.

- inconclusive — whether `isRecoveryVolume` misclassifies an exotic index named
  `name.vol000+00.par2` (zero recovery blocks) as a recovery volume.
  probe: search real-world par2 creator output for a zero-block volume naming
  an index.
  expected branches:
    - no such naming in practice → continue per plan; the classification is
      pre-existing and unchanged by this work
    - such naming exists → file separately against `isRecoveryVolume`; it does
      not block this plan, which only re-keys onto an existing predicate
