# The persisted fetch policy must agree with the derived one (#329)

## The invariant

> Before a job becomes schedulable, its `job_files` rows agree with the fetch
> policy its construction derived.

Nothing a previous attempt wrote, and no seed default, survives into a live
job's policy.

## How it is violated today, on both entry points

`FetchPolicy` is derived at construction: `BuildIngestJob` sets a par2 recovery
volume to `FetchIfNeeded` when on-demand par2 is on, and leaves it `FetchAlways`
when it is off. Both paths that construct a job then disagree with that
derivation in the database.

**The retry path** (what #329 was filed about) writes the value three times:

| Order | Site | Writes |
|---|---|---|
| 1 | `BuildIngestJob` → `SetFileFetchPolicy` | the policy derived from live config |
| 2 | `RestoreFileMeta` | **unconditionally** overwrites it with the failed attempt's value |
| 3 | `ResetForRetry` | partially repairs step 2, for `FetchNever` only |

Step 2 is the defect, and its shape is visible on the page: `filename`,
`complete` and `crc` are each written under a guard; `Fetch` is the one bare
assignment.

**The ingest path** disagrees from the moment of creation, unconditionally:

- `seedJobFiles` hardcodes the column — `VALUES (?, ?, 0, 0, '', 0)`, and `0` is
  `FetchAlways`. This is the defect: the row is wrong from the instant it
  exists, and stays wrong. `AddJob` calling `dispatcher.Add` before the seed is
  **not** part of it — a row that does not exist yet is simply not restored.
- `checkpointer.Mark` fires only on download or ack progress, so a job that has
  not downloaded anything is never flushed and the row stays wrong indefinitely.

**Either disagreement becomes a live defect through the same door.** A failed
job deliberately keeps its `job_files` rows —
`shouldDeleteDurability := entry.Status != string(constants.StatusFailed)` in
`internal/app/job_finalizer.go` gates the only `DELETE FROM job_files`. And
`Hydrate` calls `restoreJobFiles` unconditionally on both of its branches, so
every eviction-then-hydration re-applies the row over live memory. Eviction and
re-hydration are routine dispatcher behaviour, not an edge case.

So:

- **Ingest**: an on-demand-par2 job evicted and re-hydrated before its first
  article completes has its recovery volumes flipped to `FetchAlways` and
  downloaded. The feature is silently defeated, for every such job.
- **Retry, on-demand par2 off**: the retained `FetchNever` is re-applied and the
  volume is never downloaded although the user disabled the feature.
- **Retry, on-demand par2 on**: a damage-released `FetchAlways` survives, so a
  ruling made against contents the retry is about to change is re-used.

## Premise

Issue #329 as filed is rejected: its central claim is that two retry routes
disagree, and `Dispatcher.Retry` has no production caller, so only one route is
user-reachable. Its stated blocker — that closing it "needs new state" — is also
false: `Manifest.FileIsPar2Recovery` already exists, and the opt-in is available
inside `BuildIngestJob` itself. No new persisted state, no new `FetchPolicy`
value, and so no migration. That matters concretely: `fetch_policy` carries
`CHECK (fetch_policy BETWEEN 0 AND 2)` on both `job_files` and
`history_job_files`. The audit comment on #329 carries the evidence.

The ingest-path violation was found by the plan-review loop, not by the issue.
It is the same invariant, it is unconditional rather than conditional, and it
reaches more jobs — which is why this plan is written around the invariant
rather than around the retry path.

## Decisions

- **Ownership lives in the type, not the call site.** `RestoreFileMeta` loses
  its `fetch` parameter; residency gets a dedicated `RestoreFetchPolicy`.
  Restoring a policy becomes something a caller asks for by name, so the retry
  path cannot inherit one by omission.
- **`ResetForRetry`'s fetch branch is deleted**, not kept as a safety net.
- **A recovery volume that already completed keeps `Complete` and takes the
  re-derived policy.** `Complete` + `FetchIfNeeded` is permitted, not
  special-cased.
- **The seed authors the initial value; the checkpointer is the only mutator.**
  `seedJobFiles` writes each file's derived `fetch_policy` instead of a
  hardcoded `0`.

  An earlier draft rejected this as making the seed "a second writer of a value
  the checkpointer owns". That conflated two different things. `complete`,
  `filename` and `assembled_crc32` are genuine placeholders — unknown at
  creation, and legitimately corrected by `SaveBatch` as download progresses.
  `fetch_policy` is not in that category: it is fully determined at
  construction, by `BuildIngestJob`, before `AddJob` is ever called. It does
  change later — `undeferRecovery` and `DiscardDeferredPar2` write it when the
  par2 verdict lands — but every one of those mutations happens **after** the
  job is schedulable and is persisted by the checkpointer. So the seed authors
  the initial value, exactly as it already authors `job_id` and `file_index`,
  and the checkpointer remains the sole mutator. That is the Rule 2 argument,
  and it holds; "the value never changes again" does not, and must not appear in
  a commit body.

## Why no non-recovery file is affected

`FetchIfNeeded` enters a fresh job only through `SetFileFetchPolicy`, whose sole
production caller is `internal/app/ingest.go` under
`jf.Deferred == isRecovery && onDemandPar2`. The other production writers of
`.Fetch` are each gated on a policy a non-recovery file never holds:
`undeferRecovery` and `DiscardDeferredPar2` on `FetchIfNeeded`, `ResetForRetry`
on `FetchNever`. `RestoreFileMeta` is the unguarded one this plan removes, and
`newJobProgressSized` writes the `FetchAlways` zero value.

Basis: `git grep -nE '\.Fetch\s*=[^=]' -- '*.go' | grep -v _test.go` returns 7
assignments — the six above plus `internal/app/app.go`'s
`retainedFile.Fetch`, which Task 1 deletes. (A `\.Fetch\s*=` pattern without the
`[^=]` also matches `==` comparisons and returns 12 lines; the narrower command
is the one that means what this paragraph says.)

So a non-recovery file is `FetchAlways` for its whole life, and dropping the
restore is a no-op for it. No `FileIsPar2Recovery` test is needed at the call
site.

## Tasks

Tasks 1 and 2 have a stated ordering constraint; see Task 2.

### Task 1 — `RestoreFileMeta` stops carrying the fetch policy

- Remove the `fetch FetchPolicy` parameter from `Job.RestoreFileMeta`.
- Add `Job.RestoreFetchPolicy(fileIdx int, p FetchPolicy) error`, with the same
  bounds and residency guards, as the single door for restoring a persisted
  policy.
- `internal/app/residency.go` calls both, adjacently. Hydrating a resident job
  is the one case where the persisted value *is* the current truth.
- `internal/app/app.go`'s `RetryHistoryJob` calls `RestoreFileMeta` only.
- Update the one test caller in `internal/job/content_test.go`.
- Retire the retained policy at the query layer: `retainedFile.Fetch` has
  exactly one write and one read in the package, and Task 1 deletes the read, so
  drop the field, and drop `fetch_policy` from `historyFileProgress`'s `SELECT`
  and `Scan`. `retainedMatchesManifest` does not consult it.

**Test:** residency hydration still restores a non-default policy; the retry
path leaves the ingest-derived policy in place.

**The residency test must hydrate over *default* progress**, building a fresh
`job.Job`, inserting rows, and calling the restore directly — the pattern
`residency_hydration_test.go` already uses. `Job.Evict` nils only the manifest
and leaves `JobProgress` intact, so a test that evicts and re-hydrates a live
job already has the right policy in memory, and neutering the
`RestoreFetchPolicy` call would change nothing there. Cases (d) and (e) are
therefore not substitutes for this test. Every existing test
that inserts `job_files` rows writes `FetchAlways`, which is why none of them
catches a dropped restore today.

### Task 2 — delete `ResetForRetry`'s fetch branch

**Must land at or after Task 1.** Alone it is a regression: with
`RestoreFileMeta` still writing the retained value and nothing repairing it, the
configuration-off case becomes `FetchNever` — permanently skipped — where today
it is merely held. Once Task 1 has landed this task is **behaviour-neutral on
the retry path**, because nothing there can be `FetchNever` any more. The commit
body must say that rather than claim a fix: it removes the third writer so the
ownership is real rather than repaired.

**Test:** a retained `FetchNever` does not become `FetchIfNeeded`. This is an
`internal/job` unit test on `ResetForRetry` directly — after Task 1 no app-level
retry test can reach the branch.

### Task 3 — the row agrees on both construction paths

The two paths differ in kind — one creates rows, the other inherits them — so
each is corrected where its value originates. That is one mechanism per path,
not two mechanisms for one job.

**3a — ingest: seed the derived value.** `seedJobFiles` takes the job's
progress (or the per-file policies) and writes each file's real `fetch_policy`
in place of the hardcoded `0`, using the same `Progress.FileFetchPolicy(i)`
accessor `SaveBatch` already uses. The row is then never wrong, rather than
wrong-and-later-corrected.

No reordering of `AddJob` is required, and none should be done. The window
between `dispatcher.Add` and `seedJobFiles` is harmless: with no rows yet,
`restoreJobFiles`' query returns nothing, its loop body never executes, and
live memory is untouched. Moving the seed would change error handling on the
highest-frequency mutating path in the system for no gain.

**3b — retry: seed any missing rows, then flush, immediately before
`dispatcher.Add`.**

**The position is exact, and both ends matter.** The triple —
`seedJobFiles`, `app.checkpointer.Mark(j)`, `app.checkpointer.Flush(...)` —
goes as the last thing before `dispatcher.Add`: *after* `ResetForRetry` and
after the barrier and assembler forget calls, not merely after the restore loop.
`ResetForRetry` clears `Complete` on every file whose failed articles it reset,
and `SaveBatch` persists `complete` for every file of a marked job. Flushing
before `ResetForRetry` would therefore write the **pre-reset** `complete = 1`,
leave the job clean, and let an eviction hydrate files as complete that the
retry had just un-completed. The flush must capture post-reset progress, not
merely land before `Add`.

`seedJobFiles` is `INSERT … ON CONFLICT(job_id, file_index) DO NOTHING`
— pinned by `TestSeedJobFiles_IsIdempotent` — so calling it here only adds
missing rows and cannot disturb a retained one. That is why it is safe
**without** a preceding `DELETE`; the rejected alternative below is the
delete-and-reseed variant, which is a different thing.

It is insurance rather than a fix for a demonstrated case on this branch. The
retained path cannot reach a missing row: `retainedMatchesManifest` requires
`len(retained) == m.NumFiles()` and a per-file article-count match, so a
re-parsed manifest that grew is rejected before the restore loop runs. The
reachable cases are both already deferred below — the `!progressApplied`
branch, and a job whose original seed transaction failed outright. The call is
one statement and `DO NOTHING` makes it unconditional, so it is cheaper to make
the invariant hold on every branch than to argue which branch reaches it.

The ordering is the point: before `Add` the dispatcher does not hold the job and
no eviction is possible; after `Add` the tick loop can evict and re-hydrate
concurrently, which is the window this closes.

**A `Flush` error aborts the retry** rather than being discarded, leaving the
history entry intact and the job unqueued — matching `dropJobDurability`'s
existing rule that a cleanup failure aborts a retry. A discarded error silently
reopens the door.

`Flush` writes every dirty job, not only this one, and returns one error for the
batch. So an unrelated in-flight job's checkpoint failure will abort this retry.
That is defensible — a failed `SaveBatch` means the database is not writable, and
queueing a job whose progress cannot be persisted is worse than refusing the
retry — but it is a new error coupling and the commit body should say so rather
than leave it implied.

Pass `context.Background()`, so a client disconnect mid-request does not abort a
retry that has already mutated state. There are four synchronous `Flush` call
sites; the three that pass `context.Background()` are `saveQueueIfDirty`, the
shutdown flush and `enqueuePostProc`. The fourth, the startup resume sweep,
passes `ctx` — so the precedent to follow is those three specifically, not
"the existing sites".

On the retry path the whole-map cost is acceptable: retry is user-initiated and
rare, and `enqueuePostProc` already calls the same whole-map `Flush`
synchronously. It would **not** have been
acceptable on `AddJob`, where a bulk import would pay a full write of every
active job's per-file progress once per job added — which is a second reason
3a seeds rather than flushes.

Considered and rejected: deleting and re-seeding `job_files` on retry.
`seedJobFiles` writes empty results for the progress columns by design, so an
eviction between the re-seed and the flush would restore a job with its
`Complete` and CRC wiped.

### Task 4 — regression tests

Model on `internal/app/retry_from_nzb_test.go`. **Three** fixture additions:

- an NZB variant carrying a par2 recovery subject (`.volNNN+MM.par2`);
- seeded `history_job_files` rows — the table `historyFileProgress` reads;
- seeded **`job_files`** rows carrying the failed attempt's `fetch_policy`.

The third is not optional and case (d) depends on it. After 3a the seed writes
the derived policy, so a test whose `job_files` table is empty has 3b's seed
create correct rows — and (d) would then pass with `Mark`/`Flush` deleted,
making Task 5's mutation report SURVIVED. A stale value can only reach the row
by being left there by the failed attempt, which is what `job_finalizer.go`
preserves for a job in `Failed`.

**Cases (d) and (e) must live in `package app`, not `package app_test`.** Both
need `app.residency`, an unexported field, to drive `Evict` then `Hydrate` for a
job that has downloaded nothing. `residency_test.go` and
`residency_hydration_test.go` are already `package app` and construct the
residency directly, so the pattern exists. That also settles what the first
draft carried as an open question: the eviction **is** drivable, so case (e) is
written as stated rather than falling back to a weaker seam-level assertion.

- **(a) Configuration is honoured.** On-demand par2 off, a retained `FetchNever`
  from an attempt made while it was on. The retried volume must be
  `FetchAlways`.
- **(b) A prior ruling does not survive.** On-demand par2 on, a retained
  `FetchAlways` from a damage release. The retried volume must be
  `FetchIfNeeded`.
- **(c) A completed volume keeps its bytes.** A retained `Complete` recovery
  volume stays `Complete` while taking the re-derived policy.
- **(d) The retry survives an eviction.** Retry, evict, re-hydrate, assert the
  policy is still the re-derived one. Without Task 3 this fails; none of
  (a)-(c) would notice, because they assert at retry time.
- **(e) A fresh job survives an eviction.** Add an on-demand-par2 job, evict and
  re-hydrate it before any article completes, assert its recovery volumes are
  still `FetchIfNeeded`. This is the ingest-path defect, and it fails today.

(a) and (b) pin end-to-end behaviour rather than the retained value, since after
Task 1 the column is not read at all. Say so where they are written.

### Task 5 — mutation specs

**Two spec files.** A spec carries exactly one `pkg`, and a mutation block
cannot carry its own `run`.

A spec's `pkg` decides which package's tests run (`go test <pkg>`), so **a
mutation must live in the spec whose `pkg` contains the test that kills it**,
regardless of which file the mutation edits. `file` is repo-root-relative and
only constrained to stay inside the repo, so the two are independent.

`internal/app/testdata/retry_fetch_owner.spec` — one `run` regex covering all
four:

- neuter the `RestoreFetchPolicy` call in `restoreJobFiles` — killed by Task 1's
  residency test. This is the task's main risk and no test today would catch it;
- neuter the `Mark`/`Flush` on the retry path — killed by (d);
- restore `seedJobFiles`' hardcoded `0` in place of the derived policy — killed
  by (e). Anchor the **argument expression passed to `ExecContext`**, not the
  `VALUES` literal: changing the placeholder count produces a SQLite
  argument-count error, which is red for a reason that proves nothing;
- **re-introduce the retry-path restore**, by appending
  `_ = j.RestoreFetchPolicy(f.FileIndex, job.FetchNever)` after the
  `RestoreFileMeta` call in `RetryHistoryJob` — killed by both (a) and (b).

  That last one is anchored in `internal/app/app.go` even though Task 1 deletes
  code from three non-contiguous places there. A mutation does **not** have to
  restore deleted code verbatim; it has to reconstruct the *defect*. Appending
  one line after a surviving call is a single contiguous anchor, it compiles,
  and it expresses the regression — the retry path applying a policy it did not
  derive — at the site where the regression would actually occur.

`internal/job/testdata/fetch_policy_owner.spec`:

- restore `ResetForRetry`'s `FetchNever` branch — killed by Task 2's unit test.
  This cannot be pinned from `internal/app`: after Task 1 nothing on the retry
  path is ever `FetchNever`, so restoring the branch is a no-op there and the
  mutation would survive while proving nothing.

  Anchor on the `if anyReset { … }` block, which is unique in the file.
  `j.progress.recompute(j.manifest)` appears three times in `content.go` and
  would fail `checkAnchor` with an ANCHOR error rather than running.

## Comment and doc sweep

Falsified by Task 2 and to be corrected in the same commit:

- `internal/app/app.go`, on `outcomeUnknown` — "ResetForRetry downgrades
  FetchNever to FetchIfNeeded anyway, so a retry behaves the same under either
  policy." **The conclusion survives and the reason changes**: after this change
  a retry inherits neither policy, so it behaves the same for a stronger reason.
  The `outcomeUnknown` decision — hold rather than discard, to keep an honest
  "held" label — is unaffected. Rewrite the reasoning, do not reopen the
  decision.
- `internal/app/ondemand_par2_test.go` — repeats the same claim.
- `internal/postproc/filelist.go` — the "nothing but a recovery volume is ever
  moved off `FetchAlways`" citation names `Job.ResetForRetry` as a writer.
- `docs/ARCHITECTURE.md` — "Only `ResetForRetry` leaves it, downgrading to
  `FetchIfNeeded` …". False after Task 2.
- `docs/ARCHITECTURE.md` — the third copy of the `app.go` sentence, verbatim.
  Two of these three copies were missed by the first draft of this list, which
  is the paraphrase failure AGENTS.md's step-4 section describes.
- `docs/post-processing-contract.md` — "`ResetForRetry` is the only path back …
  `FetchAlways` and `FetchIfNeeded` files are untouched by a retry." Both halves
  falsified.

Three further sites — the first needs a decision, the other two are falsified by
Task 3a and must be edited:

- `internal/history/migrations/001_initial.sql` — "The fetch_policy CHECK is the
  only guard that value has — neither `SetFileFetchPolicy` nor
  `RestoreFileMeta` range-checks it." Task 1 takes `fetch` off
  `RestoreFileMeta`, so the sentence names a parameter that no longer exists.
  Its *substance* survives: `RestoreFetchPolicy` does not range-check either, so
  the CHECK is still the only guard. Only the name is stale.

  **Correct the sentence in place.** An earlier draft of this plan ruled the
  file untouchable, generalising AGENTS.md's rule about applied migrations
  without checking this repository's practice. `git log` on that file shows the
  immediately preceding commit is a comment-only correction to it
  (`b27717ba`, "correct sixteen claims the adversarial review falsified"), and
  two commits before that collapsed the 002-007 chain into it. Under Standing
  Rule 1 there is no deployed state a stale comment could mislead, and adding a
  migration purely to carry a comment correction would be the worse outcome.
  Also give `RestoreFetchPolicy` a doc comment saying it is the unchecked door
  the CHECK guards.
- `internal/app/app.go`, `seedJobFiles`' doc comment — it describes the rows as
  holding "empty RESULTS that `SaveBatch` fills in". After 3a that is no longer
  true of `fetch_policy`, which the seed now authors outright. The
  one-transaction reasoning below it survives untouched; only the description of
  what is written needs correcting.
- `internal/app/seed_job_files_test.go` — six call sites take the old signature,
  and `TestSeedJobFiles_OneRowPerFile` asserts a non-zero policy is a **failure**
  ("seeded with results already set"), repeating the same "empty results" claim
  in its own doc comment. That assertion inverts under 3a and must become "the
  derived policy". This file is also why 3b's `ON CONFLICT … DO NOTHING`
  reliance is already test-backed.

Deliberately **not** swept, each concerning `par2ReleaseReason`, `par2Recovered`
or the `done`/`failed` bits rather than fetch policy:
`internal/job/progress.go`, `internal/postproc/filelist.go`'s second site,
`docs/post-processing-contract.md`'s "only clearer" sentence,
`docs/durability-contract.md`. `docs/reviews/*` are frozen records and exempt.

AGENTS.md requires reading `docs/ARCHITECTURE.md` and the relevant contract
section in full at the end of a change that alters an enforced invariant, rather
than grepping them. That applies here.

## Impact list

| Caller | Contract it relies on |
|---|---|
| `internal/app/residency.go` | Must keep restoring the persisted policy. A missed `RestoreFetchPolicy` silently reverts every hydrated job to `FetchAlways`. Task 1's main risk, pinned by a mutation. |
| `internal/app/app.go` (`RetryHistoryJob`) | Must NOT restore it, and must flush before `Add`. |
| `internal/app/app.go` (`AddJob`) | Unchanged in shape. Only `seedJobFiles`' signature and its one call site change. |
| `seedJobFiles` | Gains the job's per-file policies. One production caller today; 3b adds a second, both in `internal/app`. |
| `internal/app/seed_job_files_test.go` | Six call sites take the old signature, and one assertion inverts: a non-zero seeded policy stops being a failure. |
| `job_files.fetch_policy` (the row) | Must agree with the derived value before any eviction can read it. |
| `internal/app/job_finalizer.go` | Relies on failed jobs keeping their rows. Task 3 updates rows, never deletes them. |
| `internal/checkpoint` | Gains one synchronous flush call, on the retry path only. The three existing `Flush(context.Background())` sites are `saveQueueIfDirty`, the shutdown flush, and `enqueuePostProc`. |
| `internal/job/content_test.go` | Mechanical signature update. |

## Implementation guards

- `RestoreFetchPolicy` carries the same bounds check and `ErrNotResident` guard
  as `RestoreFileMeta`; a second door must not be a weaker door.
- The residency path's two calls stay adjacent, so a reader sees that hydration
  restores both halves.
- The `Flush` error is returned, never discarded.

## Resolved during plan review

Settled from source, so not carried as inconclusive:

- **`retainedMatchesManifest` accepts a seeded fixture.** It compares only the
  row count against `NumFiles()`, each `FileIndex` against its position, and each
  `ArticleCount` against the manifest's file range.
- **`Complete` + `FetchIfNeeded` is coherent, so the ruling stands.**
  `recompute` never writes `Complete`; `sizeFigures` excludes any
  non-`FetchAlways` file from **both** the expected and remaining figures; and
  `IsComplete` skips it.
- **`SaveBatch` writes `fetch_policy`**, so Task 3's mechanism reaches the
  column.
- **No deadlock in Task 3b.** `RetryHistoryJob` holds no job or app lock, and
  `Flush` serialises against the ticker on its own mutex — contention, not a
  cycle.
- **A missing row is harmless, so the ingest path needs no reordering.**
  `restoreJobFiles` iterates the rows its query returns and does nothing else;
  with no rows it makes no `RestoreFileMeta` call and leaves live memory alone.
  So the gap between `dispatcher.Add` and `seedJobFiles` cannot corrupt a
  policy, and the fix belongs in what the seed writes rather than in when it
  runs.

## Inconclusive / Deferred items

- inconclusive — whether `DeferredRecoveryIndices` includes an already-complete
  file, and whether `UndeferRecoveryVolumes` on one is a no-op.
  probe: read both, then assert no re-dispatch for a complete undeferred file.
  expected branches:
    - if it is a no-op → no action.
    - if it re-dispatches a complete file → a guard is owed here, since the
      completed-volume ruling creates the state that reaches it.

- deferred — `AddJob` returns a non-nil error from `seedJobFiles` **after** the
  job is already schedulable, so its three production callers
  (`internal/api/queue.go`, `cmd/gonzbd/adapters.go`, `cmd/gonzbd/main.go`),
  which all treat an error as "not added", are wrong today.
  reason: a genuine defect in `AddJob`'s error contract, but it is not this
  invariant and fixing it means reordering the highest-frequency mutating path
  in the system. Deliberately not smuggled into a fetch-policy change.
  resolution-point: its own issue. The reorder that fixes it is one statement.

- deferred — `DiscardDeferredPar2` and `UndeferRecoveryVolumes` are not followed
  by a `checkpointer.Mark`, so a post-verdict policy change relies on the job
  still being dirty from an earlier `Mark`.
  reason: a fourth instance of the same invariant, but after the job is
  schedulable rather than before, so outside this plan's stated scope. Named
  here so the plan's "sole writer" claim is not read wider than it is.
  resolution-point: its own issue; the probe is whether the 5 s ticker firing
  between `MarkFileComplete` and the verdict leaves the change unflushed.

- deferred — `history_job_files.fetch_policy` becomes write-only once Task 1
  drops it from `historyFileProgress`'s `SELECT`; `job_finalizer.go` still
  writes it.
  reason: dropping a column needs a migration, which is a Decision Protocol
  escalation and is not this change. Keeping it is defensible — it is the
  archived record of what the attempt ran with.
  resolution-point: whichever change next revisits the history schema; HEAD
  already removed seven such columns, so the precedent exists.

- deferred — `RetryHistoryJob`'s shape-changed path (`!progressApplied`) leaves
  `job_files` un-reseeded, so rows above the new file count survive stale.
  reason: pre-existing, and its fetch-policy consequence is a `FetchAlways`
  default — over-fetching, not permanent skipping.
  resolution-point: whichever issue owns retry's row lifecycle; this plan's
  claim is scoped to the retained-progress path.

- deferred — the unreachable `Dispatcher.Retry`.
  reason: whether queue-side retry should exist is a product question.
  resolution-point: its own issue, if queue-side retry is ever wanted.

- deferred — the missing par2 summary line after a retry.
  reason: cosmetic, and it may fall out of these changes.
  resolution-point: re-check once Tasks 1-3 are in; file separately if it
  survives.
