# par2: hold the volumes when nothing could be identified — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When `par2.Assess` identifies nothing on disk, stop reporting that as a clean verdict. Hold the recovery volumes rather than discarding them, record why in durable state, and stop the finalize summary claiming a check that did not happen.

**Architecture:** `app.par2Verdict` answers a two-valued question (`needsRecovery bool`), and its caller reads `false` as "verified clean → discard the volumes". Two states collapse into that `false`: *verified clean*, and *nothing could be identified*. The second is produced both by a Layout B post (par2 protects the extracted contents) and by an obfuscated single-file post damaged inside its first 16 KB, which defeats identification passes 1, 2 and 3 together. This plan splits the outcome three ways and gives the third its own handling: hold, record, report.

**Tech Stack:** Go 1.27, SQLite via `modernc.org/sqlite`, goose migrations. No new dependencies.

**Spec:** GitHub issue #491, as corrected by the premise audit
(https://github.com/hobeone/gonzbd/issues/491#issuecomment-5515631204) and the direction settled at
Gate 1 (https://github.com/hobeone/gonzbd/issues/491#issuecomment-5516222648), **as further corrected
by the plan review below**.

## What the plan review changed, and why this document was rewritten

The first draft rested on two false claims. Both are corrected here rather than patched, because each changed the task set.

1. **`par2ReleaseReason` is not persisted.** The first draft asserted it was, citing `jobProgressJSON` (`internal/queue/progress.go:983-984`). Those struct tags are real but the path is dead: their only reader is `Job.UnmarshalJSON`, whose own doc states *"There is no production caller… Tracked as dead code in #304"* (`internal/queue/job.go:1168-1171`). There is no `par2_release_reason` column in any migration, `updateTx` does not write one, and `RestoreJobProgress` does not restore one. **Task 1 now adds the column**, which is why this plan carries a migration it did not before.

2. **Holding a volume buys nothing on its own.** `undeferRecovery` has exactly two callers — `Queue.UndeferRecoveryVolumes` (`internal/queue/queue.go:1878`, sole production caller `internal/app/app.go:1538`) and the permanent-article-failure path (`internal/queue/workset.go:159`) — and `maybeReleaseRecoveryVolumes` has one call site, inside the file-complete handler (`internal/app/app.go:1401`). Nothing promotes a held volume after a job finalizes, and `ResetForRetry` downgrades `FetchNever → FetchIfNeeded` anyway (`internal/queue/job.go:1101`), so a retry is identical under either policy. **Holding is justified by reporting, not recovery:** `fileState` renders `FetchIfNeeded` as `"held"` and `FetchNever` as `"skipped"` (`internal/api/queue.go:327-329`), and `"skipped"` asserts a verdict that was not earned. Any task comment or test message claiming the hold rescues a repairable job is false and must not be written.

3. **A gate task was deleted, and the numbering gap is deliberate.** Tasks run 1, 2, 3, 4, 6, 7 — there is no Task 5. It added a term to `maybeReleaseRecoveryVolumes`'s gate to stop a held-and-decided job re-assessing after a restart. Round 3 proved that unreachable: `completeFinalizedFile`'s startup caller is gated on `strandedComplete` (`internal/app/resume_startup.go:507-521`), which requires `FetchAlways && !FileComplete`, and a verdict-reached job has every `FetchAlways` file complete while held volumes are `FetchIfNeeded`. The numbers are left as they are so the review record above still refers to the right tasks.

## Global Constraints

- **Rule 2 — state has one owner.** "A verdict was recorded" gets exactly one accessor, `JobProgress.HasPar2Verdict()`. Call sites use it; none re-spell `Par2ReleaseReason() != ""` inline.
- **Rule 4 — enumerate before asserting, and make the citation runnable.** Every comment quantifying over writers, callers or branches states the command that established it — and the command must actually produce the number claimed. `git grep -n 'par2ReleaseReason = ' -- internal/queue/` returns **four** lines, not two: the setter (`job.go:521`), the clearer (`job.go:1078`, which the pattern subsumes), the dead-JSON restore (`progress.go:1037`), and the direct assignment on the permanent-failure path (`workset.go:160`). Task 1 adds another. Any comment citing a count must use a pattern that excludes what it does not mean to count, must be re-run **after `git add`** (`check_citations` scans tracked files), and must not sit inside its own pattern's path — a comment placed in `internal/queue/` matching `par2ReleaseReason` counts itself.
- **Never modify an existing migration.** `internal/history/migrations/` holds `001_initial.sql`, `002_durable_runs.sql`, `003_drop_legacy_durability.sql`. Add `004_…`; edit none of them.
- **Rule 1 — no backwards compatibility.** A missing column value on an older row is not a case to handle; the migration's `DEFAULT ''` is the whole story.
- **Red-green is observed.** Every behavioural pin gets a `scripts/mutate` spec, run with `-count=1` semantics via the tool, and each mutation must report **KILLED**. `EXCLUDED` and `ANCHOR` are failures, not warnings.
- **No claim that holding rescues a repairable job.** See point 2 above.

---

### Task 1: Persist the par2 release reason

`par2ReleaseReason` is process-local. **Task 6 is what needs it durable.** A job that reached a verdict is left at `StatusVerifying` (`internal/queue/queue.go:1351`), which `Phase()` maps to `PhaseProcessing` (`internal/queue/job.go:681`) and `IsResident()` reports true (`:694`), so a restart re-enqueues it for post-processing at `internal/app/app.go:1082`. `buildPreambleLog` then runs with `Par2ReleaseReason() == ""`, Task 6's new case cannot fire, and the `✓ Par2: verified clean from index` line returns — the exact sentence Task 6 exists to remove.

*(An earlier draft also justified this by a gate re-opening after restart. That task was deleted: the restart route for a complete job calls `enqueuePostProc`/`maybeFinalize` directly and never re-enters `maybeReleaseRecoveryVolumes`. Task 1 survives on the ground above alone.)*

**Files:**
- Create: `internal/history/migrations/004_par2_release_reason.sql`
- Modify: `internal/queue/sqlite_store.go` (INSERT `:476`; `updateTx` `:1058`; `Get`'s `qJob` SELECT `:562` and its `Scan`)
- Modify: `internal/history/migrations_test.go` — two places: the hardcoded `[]string{"001_…","002_…","003_…"}` compared with `slices.Equal` at `:268`, **and the prose at `:254-256`** which enumerates the migration set in words ("This guarded a single `001_initial.sql` until `002_…` added … and `003_…` dropped …"). A `004_` falsifies both; fixing only the assertion leaves the comment stating a set that no longer exists.
- Modify: `internal/history/testdata/schema.golden` (`:21` — pins `CREATE TABLE jobs` verbatim; SQLite rewrites `sqlite_master.sql` on `ADD COLUMN`)
- Test: `internal/queue/sqlite_store_test.go`

**Interfaces:**
- Produces: `jobs.par2_release_reason TEXT NOT NULL DEFAULT ''`, round-tripped through `SQLiteStore.Get` (resident) **and through the non-resident bulk read** (see Step 4b).

  **The non-resident path is destructive, not merely lossy, and must be closed here.** `Queue.Load` hydrates a non-resident job with `newJobProgressSized` (`internal/queue/persistence.go:255-262`), giving it an empty reason; once the column is in `updateTx`'s `UPDATE jobs SET …`, that job's next `Update` writes `''` over the stored value. This is precisely what happened to the download stamps, and `persistence.go:246-252` records the outcome: *"Left that way they are not merely missing from the UI — the next `updateTx` encodes the zeros back over the persisted values and the real stamps are gone."* Their fix was a dedicated bulk read, `DownloadStampsByJob` (`sqlite_store.go:78-102`), pinned by `TestSQLiteStore_NonResidentJobKeepsItsDownloadStamps`.

  Promotion's wholesale replacement (`internal/queue/queue.go:859`) is a separate, pre-existing loss already documented at `queue.go:1936-1947` for the `FetchNever` mark; it is **not** closed here.
- Consumes: `JobProgress.Par2ReleaseReason()` (`internal/queue/progress.go:615`), `setPar2ReleaseReason` (`internal/queue/job.go:521`).

- [ ] **Step 1: Write the failing round-trip test**

```go
func TestSQLiteStore_Par2ReleaseReasonSurvivesAReload(t *testing.T) {
	t.Parallel()

	// Process-local was enough while the only consumer ran in the same
	// process as the verdict. It is not enough once the finalize summary
	// branches on it: a restart between the verdict and post-processing
	// would print "verified clean from index" for a job nothing verified.
}
```

Store a job with a non-empty reason, `Get` it from a fresh store, assert the reason survives.

**The fixture must use a resident status** (`StatusDownloading` or `StatusVerifying`). `Get` builds `job.progress` only inside `if isResidentStatus(job.Status)` (`sqlite_store.go:621-631`), so a job left at `NewJob`'s default `StatusQueued` gets a nil progress and `Par2ReleaseReason()` returns `""` through its nil guard — the test would never go green no matter how correct the column is.

- [ ] **Step 2: Run it and watch it fail**

Run: `go test -count=1 -run TestSQLiteStore_Par2ReleaseReasonSurvivesAReload ./internal/queue/`
Expected: FAIL — the reloaded reason is `""`.

- [ ] **Step 3: Add the migration**

Follow the existing convention: **all three current migrations wrap every statement in `-- +goose StatementBegin` / `-- +goose StatementEnd`**, including `003_drop_legacy_durability.sql`'s bare `ALTER TABLE`s, with the rationale comment block inside the wrapper.

```sql
-- +goose Up
-- +goose StatementBegin
ALTER TABLE jobs ADD COLUMN par2_release_reason TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE jobs DROP COLUMN par2_release_reason;
-- +goose StatementEnd
```

`DROP COLUMN` is proven against the pinned driver: `003_drop_legacy_durability.sql` already runs one.

The comment block states what the column means and what it does **not**: it records why the on-demand par2 verdict released or withheld the recovery volumes, and a non-empty value is the marker that a verdict was reached. It is not a repair result and nothing branches on its text.

`internal/history/db.go:192`'s `refuseUnknownSchema` needs no edit — it derives its bound from the embedded filenames (`highestEmbeddedVersion`, `:214`).

- [ ] **Step 4: Thread it through the three SQL sites**

INSERT (`:476`), `updateTx`'s `UPDATE` (`:1058` — its comment at `:1054` says the SQL lives in exactly one place, and `Update` `:1116` / `UpdateBatch` `:1122` both route through it), and `Get`'s `qJob` SELECT (`:562`) plus its `Scan` list. Assign through `setPar2ReleaseReason`, **inside `Get`'s resident branch** — that setter dereferences `j.progress` unconditionally (`job.go:521`) and would panic on a non-resident job.

- [ ] **Step 4b: Carry it on the non-resident path too, or the UPDATE erases it**

Write a second failing test first, mirroring `TestSQLiteStore_NonResidentJobKeepsItsDownloadStamps`: load a non-resident job whose stored reason is non-empty, call `Update`, reload, and assert the reason survived. It will fail by writing `''`.

The bulk read at `sqlite_store.go:78-102` already does exactly this job for the stamps — `SELECT id, download_started, download_finished FROM jobs` at `:81`. Add `par2_release_reason` to that SELECT and to `Queue.Load`'s restore loop (`persistence.go:250-262`) rather than adding a second query over the same table. Rename it to say what it now carries; `DownloadStampsByJob` would become a name that describes half its result.

- [ ] **Step 5: Run the test, the package, and the two history gates**

Run: `go test -race -count=1 ./internal/queue/ ./internal/history/`
Expected: PASS, after regenerating the golden schema with
`go test ./internal/history/ -run TestMigrations_GoldenSchema -update` — and **read that diff** rather than accepting it; `migrations_test.go:275-296` says why.

- [ ] **Step 6: Commit**

---

### Task 2: Delete the write-only requeue fields

`NeedRequeue`, `RequeueBlocksNeeded` and `RequeueReason` are written and read nowhere. `postproc.go:512` justifies them as "recorded for informational purposes (history/UI)" — nothing in `internal/history`, `internal/api` or `ui/` reads any of the three. The Phase 2 seam for block-exact fetching is `UndeferRecoveryVolumes`'s `fileIdxs` argument (`internal/queue/queue.go:1868`), which is live and documented; these fields reserve nothing it does not.

**Files:**
- Modify: `internal/postproc/stages.go` (`:169`, `:173`, `:176`), `internal/postproc/stage_repair.go` (`:262-264`, `:275-276`), `internal/postproc/postproc.go` (`:512-516`)
- Modify: `internal/postproc/stage_repair_test.go` — **five** sites, not two: `TestHandleRepairResult_NeedMoreBlocks` (`:342`) and `_InvalidPar2` (`:375`) assert the fields positively; `:333`, `:428` and `:454` assert them negatively ("NeedRequeue should be false on generic error / on generic failure / on success"). All five stop compiling when the fields go.

- [ ] **Step 1: Re-establish the population**

```bash
git grep -n 'NeedRequeue\|RequeueBlocksNeeded\|RequeueReason' -- '*.go'
```

Expected: declarations, the five writes, and the two tests. **A read site outside a test falsifies this task — stop rather than delete.**

- [ ] **Step 2: Keep the two repair tests discriminating BEFORE deleting**

`TestHandleRepairResult_NeedMoreBlocks` and `TestHandleRepairResult_InvalidPar2` each assert `ParError`, a non-nil error, *and* the doomed fields. Strip only the field assertions and the two become behaviourally identical, so nothing distinguishes `stage_repair.go:258` from `:271` any more — a future change merging the two branches would pass both tests.

Add an assertion on `err.Error()` content to each first, so each still pins the branch it was written for. Run both and watch them pass against the *unmodified* code, which is what makes them a pin rather than a rewrite.

Apply the same treatment to the three negative sites (`:333`, `:428`, `:454`) before deleting their assertions: check what each was distinguishing and give it a surviving assertion, or record in the commit body that it was distinguishing nothing.

- [ ] **Step 3: Delete the fields, the writes, and the field assertions**

The log lines stay — `"Par2 repair %q needs %d more blocks — repair not possible with current data"` is the user-visible report and is not being removed.

- [ ] **Step 4: Rewrite `postproc.go:512-516`**

It names a consumer that does not exist. Replace with what is true: insufficient blocks set `job.ParError`, which suppresses unpack (`internal/postproc/stage_unpack.go:128`), and the count is reported in the repair stage's log line rather than carried on the job.

- [ ] **Step 5: Verify and commit**

Run: `go build ./... && go test -race -count=1 ./internal/postproc/`

---

### Task 3: One owner for "a verdict was recorded"

**One caller, and the warrant is encapsulation rather than call-site count.** An earlier draft justified this by "two call sites need it"; the second was the gate task, now deleted. Task 6 is the only consumer.

The accessor still earns its place, on different ground: the reason an empty string means "no verdict" is a fact about `internal/queue`'s internals — `workset.go:160` writes a reason without a verdict, and it is safe only because that write sits inside a branch which also sets `par2Recovered` (`job_articles.go:205`). Exporting the raw string and spelling `!= ""` at `internal/postproc/filelist.go` would put that reasoning in a package that cannot see either line, and would have to restate it in a comment that no longer sits next to what it describes.

If that ground is not accepted, fold Task 3 into Task 6 and use the inline check — but then the comment explaining the `workset.go` guard goes with it, in a file two packages away from the code it is about.

**Files:**
- Modify: `internal/queue/progress.go` (beside `Par2ReleaseReason()` at `:615`)
- Test: `internal/queue/progress_test.go`

**Interfaces:**
- Produces: `func (p *JobProgress) HasPar2Verdict() bool`. Nil-safe, matching its neighbours.

- [ ] **Step 1: Write the failing test**

Assert `HasPar2Verdict()` is false on a fresh progress, true once a reason is set, false again after `ResetForRetry` clears it (`internal/queue/job.go:1078`), and false on a nil receiver.

- [ ] **Step 2: Run it and watch it fail** (undefined method)

- [ ] **Step 3: Implement**

The doc comment must state the enumeration rather than assert a single writer:

```go
// HasPar2Verdict reports whether the on-demand par2 verdict has already been
// reached for this job, using the reason string as the marker.
//
// One writer sets it without a verdict: workset.go's permanent-article-failure
// path. That is safe rather than an exception, and the branch is why — the
// assignment sits inside `if job.undeferRecovery(...)`, and undeferRecovery
// sets par2Recovered whenever it reports a change (job_articles.go:205). So
// every caller that must exclude that case already tests Par2Recovered().
//
// ResetForRetry is the only clearer (job.go:1078), which is what makes a
// retry re-derive the verdict rather than inherit it.
```

**Do not cite a writer count here.** The enumeration is real but the obvious command does not produce it — see the Global Constraint — and a comment placed in `internal/queue/` matching `par2ReleaseReason` would count itself. Name the two facts that matter (the `workset.go` writer is guarded; `ResetForRetry` is the only clearer) with their `file:line`, and let `git grep` be the reader's tool rather than a number this comment has to keep true.

- [ ] **Step 4: Run, then commit**

---

### Task 4: `par2Verdict` reports three outcomes, and the unidentifiable case holds

**Files:**
- Modify: `internal/app/app.go` (`par2Verdict` `:1563`; call site `:1509-1541`; the comment block `:1581-1599`)
- Modify: `internal/app/ondemand_par2_test.go` (`assessVerdict` `:154`, its six call sites at `:182, :195, :210, :222, :255, :279`, and the subtest at `:241`)

**Interfaces:**
- Produces: `par2Outcome` with `outcomeClean`, `outcomeRepair`, `outcomeUnknown`; a `String()`; and `allPar2Outcomes()`. `par2Verdict` returns `(par2Outcome, string)`.

- [ ] **Step 1: Mirror the established enum pattern**

`internal/postproc/stages.go:71-95` gives `QuickCheckOutcome` a `String()`, an `AllQuickCheckOutcomes()`, and `TestAllQuickCheckOutcomes_Exhaustive` that **parses the const block itself** — its comment says a hand-written list is otherwise "a second copy carrying the same defect: a value added to the const block but not here is invisible to every loop over it". `par2Outcome` is the same shape of enum; give it the same three pieces and the same parse-the-const-block test.

- [ ] **Step 2: Write the failing test**

```go
func TestPar2Verdict_NothingIdentifiedIsNotACleanVerdict(t *testing.T) {
	t.Parallel()

	// Nothing on disk matched any par2 entry. That is a Layout B post — and
	// it is ALSO an obfuscated single-file post damaged inside its first
	// 16 KB, which defeats identification passes 1, 2 and 3 together. The
	// two are indistinguishable from this value, so it cannot be reported as
	// a clean verdict.
	//
	// Holding the volumes does NOT rescue the damaged case: nothing promotes
	// a held volume after finalize (undeferRecovery has two callers, neither
	// reachable post-finalize) and ResetForRetry downgrades FetchNever to
	// FetchIfNeeded anyway. What the hold buys is an honest label — fileState
	// renders FetchIfNeeded as "held" and FetchNever as "skipped"
	// (internal/api/queue.go:327-329), and "skipped" claims a verdict that
	// was never earned.
	a := par2.Assessment{
		ID:  par2.Identification{Unaccounted: []par2.FileDesc{{FileName: "payload.mkv"}}},
		CRC: par2.CRCVerifyResult{Unverified: 1},
	}

	got, reason := par2Verdict(a, slog.New(slog.DiscardHandler))
	if got != outcomeUnknown {
		t.Fatalf("outcome = %s, want %s", got, outcomeUnknown)
	}
	if reason == "" {
		t.Error("reason is empty; nothing records why the volumes were held")
	}
}
```

- [ ] **Step 3: Run it and watch it fail**

- [ ] **Step 4: Change `par2Verdict`, then migrate its callers in the same commit**

`assessVerdict` (`:154`) forwards `par2Verdict`'s return, so the signature change breaks it at compile time. Change its return type to `(par2Outcome, string)` and update all six call sites; its two early returns (`:161`, `:165`) become `outcomeRepair`. **Do not coerce back to a bool** (`outcome != outcomeClean`) — that re-collapses the distinction this task exists to create.

The subtest at `:241` pins the old `false` for the Layout B fixture; it becomes `outcomeUnknown`, with a comment saying the expectation changed because "nothing matched" is no longer read as a clean verdict.

- [ ] **Step 5: Change the call site**

`outcomeClean` → `DiscardDeferredPar2` as today. `outcomeRepair` → `SetPar2ReleaseReason` + `UndeferRecoveryVolumes` as today. `outcomeUnknown` → `SetPar2ReleaseReason` and **neither discard nor un-defer**, then return false so the job finalizes.

- [ ] **Step 6: Run and red-check**

Run: `go test -race -count=1 ./internal/app/` then `go run ./scripts/mutate internal/app/testdata/par2_verdict.spec`.

The spec's fifth mutation anchors on `needsRecovery, reason = par2Verdict(a, app.log)` (spec `:67`), which this task rewrites — expect **ANCHOR** and re-anchor it on the surviving text. The other four anchors survive verbatim.

The `run` line matches `TestPar2Verdict_NothingIdentifiedIsNotACleanVerdict` through its unanchored `TestPar2Verdict` term, so `EXCLUDED` is not expected for it. It does **not** match `TestAllPar2Outcomes_Exhaustive`, which needs no mutation coverage — do not add a term for it, and do not describe the line as matching "both new tests".

- [ ] **Step 7: Commit**

---

### Task 6: The finalize summary stops claiming "verified clean"

`internal/postproc/filelist.go:95-102` prints `✓ Par2: verified clean from index — N recovery volume(s) skipped` whenever `heldVols > 0` (`:43`, counting `!= FetchAlways`). It switches on volume counts, not on the verdict.

**Files:**
- Modify: `internal/postproc/filelist.go`
- Test: `internal/postproc/filelist_test.go` (existing clean-path assertion at `:265`)

- [ ] **Step 1: Write the failing test**

Fixture: held volumes, `Par2Recovered() == false`, a non-empty reason. Assert the output does **not** contain `verified clean` and does contain the reason.

- [ ] **Step 2: Run it and watch it fail**

- [ ] **Step 3: Add the case ahead of the existing one**

```go
case heldVols > 0 && !p.Par2Recovered() && p.HasPar2Verdict():
```

- [ ] **Step 4: Handle the case this condition also catches**

`outcomeRepair` whose `UndeferRecoveryVolumes` failed reaches this same combination — reason set, `Par2Recovered()` never set, files still held — and verification *did* find damage there. Reporting it as "could not verify" is the same class of mislabel this task removes, one layer down.

Give the un-defer-failure path at `internal/app/app.go:1538-1541` a reason that says what happened, so the line's parenthetical carries the truth even though its headline is shared. Record in the commit body that the two remain indistinguishable by outcome, and why that is accepted: distinguishing them needs `par2Outcome` persisted, which this plan deliberately does not do.

- [ ] **Step 5: Correct the surviving comment**

`:96-100` currently reasons "Still withheld at finalize => never downloaded => verified clean". That implication is exactly what this task refutes.

- [ ] **Step 6: Run, red-check, commit**

Check `filelist_test.go:265`'s existing `✓ Par2: verified clean` assertion still passes for a genuinely clean job.

---

### Task 7: Correct the two false claims #495 shipped

Both quantify over behaviour without naming the branch they depend on.

**Files:**
- Modify: `docs/ARCHITECTURE.md` — **two bullets, three sentences**: `:102`'s "same `Assess` call" claim, `:102`'s "if nothing was identified, they are not [fetched]", and `:104`'s *"the volumes **provably cannot be spent**"* in the "Where relocation does happen" bullet
- Modify: `internal/app/app.go` (the comment block `:1581-1599`; the target sentence is at `:1598`)

**Ordering:** Task 4 already rewrites `app.go:1581-1599`. Fold this file's edit into Task 4 rather than re-touching the same prose in a later commit; keep only the `ARCHITECTURE.md` half here if that is cleaner.

- [ ] **Step 1: `docs/ARCHITECTURE.md:102`**

It states *"The QuickCheck stage consumes the same `Assess` call, so the two cannot reach different verdicts about one directory."* There are two calls — `internal/app/app.go:1481` and `internal/postproc/stage_quickcheck.go:151` — and they do reach different verdicts. Replace with what is true: both build an assessment over the same directory from the same `Manifest`+`JobProgress`-derived list, so they agree about content; they are separate calls answering different questions, and their verdicts are not required to agree.

**Two sentences in that bullet, not one.** The same paragraph also says *"if **nothing** was identified, they are not [fetched] (the Layout B case below)"*. Task 4 falsifies that too: the outcome is now `outcomeUnknown`, the volumes are **held** rather than discarded, and the case is no longer described as Layout B alone. Fix both in one edit — a sweep that corrects the sentence it came for and leaves its neighbour is the failure AGENTS.md's "sweep against the diff the commit will land as" describes, and it has shipped on this branch before.

- [ ] **Step 2: `internal/app/app.go:1598`**

*"Hash16k is what tells the two apart"* is true only for a **healthy** obfuscated release. Damage inside the first 16 KB defeats `Hash16k` and the whole-file CRC32 pass together, so it does not separate a damaged obfuscated post from Layout B — which is why the outcome is now `outcomeUnknown`. Name that branch.

- [ ] **Step 3: Sweep on the CONCEPT, not on the sentences already found**

```bash
git grep -n 'Layout B' -- '*.go' '*.md'
```

The phrase-based grep an earlier draft used here (`same Assess call\|tells the two apart\|cannot reach different verdicts`) could not reach `ARCHITECTURE.md:104`, because that copy shares none of those words — it says *"the volumes provably cannot be spent"*. Its own instruction to treat a hit elsewhere as in scope could therefore never fire. That is the paraphrase blindness AGENTS.md describes, reproduced inside the task written to fix it.

Sweep the concept the change falsifies. Expect hits in `ARCHITECTURE.md` (both bullets), `internal/app/app.go`, the `par2_verdict.spec` comments, and this plan file. Read each and decide; a hit is not automatically stale.

- [ ] **Step 4: Verify and commit**

`go run ./scripts/check_citations && go run ./scripts/check_dup_comments`

---

## Resolved before review

1. **`ResetForRetry` is the only clearer of `par2ReleaseReason`.** `git grep -n 'par2ReleaseReason = ""' -- internal/queue/` → one hit, `internal/queue/job.go:1078`.
2. **`heldVols` counts held volumes.** `internal/postproc/filelist.go:43` counts `!= queue.FetchAlways`, so both `FetchIfNeeded` and `FetchNever`.
3. **The reason reaches the user on a failed job.** `buildDownloadFileList` is called only from `buildPreambleLog` (`internal/postproc/postproc.go:319`), which runs before any stage and is unconditional on the outcome.
4. **`par2ReleaseReason` has more writers than the obvious grep suggests, and the guard is what matters.** `workset.go:160` sets it without a verdict, but only inside `if job.undeferRecovery(...)`, which sets `par2Recovered` whenever it reports a change (`job_articles.go:205`). Every caller that must exclude that case already tests `Par2Recovered()`. See the Global Constraint for why no comment should cite a count.
5. **`par2_verdict.spec`'s `run` line matches `TestPar2Verdict_NothingIdentifiedIsNotACleanVerdict`** via its unanchored `TestPar2Verdict` term, so `EXCLUDED` is not expected for it; only the fifth mutation's anchor needs re-pointing. It does **not** match `TestAllPar2Outcomes_Exhaustive`, which needs no mutation coverage — do not widen the claim to "both new tests".
6. **A restart does not re-enter `maybeReleaseRecoveryVolumes`.** `completeFinalizedFile` has three callers; the startup one (`internal/app/resume_startup.go:482`) is gated on `strandedComplete` (`:507-533`), which requires `FetchAlways && !Complete`. A verdict-reached job has every `FetchAlways` file already complete — `MarkFileComplete` (`app.go:1381`) runs before the `IsComplete()` gate (`:1400`) — and held volumes are `FetchIfNeeded`, which that predicate excludes. The restart route for a complete job is `app.go:1060-1085`, calling `enqueuePostProc`/`maybeFinalize` directly. **This is why the gate task was deleted.**
7. **`ALTER TABLE … DROP COLUMN` works against the pinned driver.** `003_drop_legacy_durability.sql` already runs one and `./internal/history/` passes.

## Inconclusive / Deferred items

1. **Whether `Par2Recovered` also needs persisting.** It is not in the schema either. Task 6's new case tests `!Par2Recovered()`, and after a restart that reads false regardless.
   - *Probe:* construct the restart case for a repaired job and check whether `filelist.go`'s `recoveryVols > 0 && p.Par2Recovered()` branch still fires.
   - *Expected branches:* it does not fire → a pre-existing reporting gap this plan does not introduce; **file it, do not widen scope**. It does → nothing owed.

2. **Where `par2Outcome`'s const block should live.** Task 4 puts it in `internal/app/app.go`. The mirror test parses a named file and assumes a stable iota position (`internal/postproc/quickcheck_exhaustive_test.go:55`).
   - *Probe:* write the exhaustiveness test against `app.go` and see whether the parser finds the block unambiguously in a 1,700-line file.
   - *Expected branches:* it does → leave it. It does not, or the assumption is fragile → move the enum to its own small file, which is what makes the parser's assumption safe.
