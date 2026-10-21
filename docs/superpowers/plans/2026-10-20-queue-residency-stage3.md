# Queue Residency Stage 3 — Delete the Startup Verifier — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** delete `verifyPausedJobs` and everything that exists only to serve it, so that a job restored paused at `Fetching` is treated like every other paused job: it reports the restore pass's estimate, is not resident, and is verified by the tick's hydration when the user resumes it.

**Architecture:** stage 2 made every restored job's progress visible before the first tick, which was the verifier's only purpose (design §4.7). Stage 3 removes the goroutine `Start` launched last, its deadline, its test hook, its ten tests and two specs, and every sentence in code and docs that describes it. Nothing is added except one pin that the deletion holds: after `Start`, a job restored paused at `Fetching` has progress, is unverified, and is not resident.

**Tech Stack:** Go, `scripts/mutate` specs, the six whole-repository doc/comment gates.

**Spec:** `docs/superpowers/specs/2026-10-20-queue-residency-design.md` §4.7, §5 (first bullet), §7 item 3. Decision log: `docs/superpowers/specs/2026-10-20-queue-residency-decisions.md` (D12 keeps `AttachContent`; D8 keeps `NewManifest([]JobFile)`; D20 keeps the header-bytes fallback — none of those is reopened here).

## Global Constraints

- Durability §3 verbatim: nothing in this stage touches how a Done bit comes to exist.
- Standing Rule 4: every "only/sole/every" written or rewritten in a comment cites its grep; the one new pin has a `scripts/mutate` spec with its mutation KILLED.
- Commit-cycle Step 4 applies per commit: the sentences a deletion falsifies are swept in the same PR, not left for stage 4 (ruling D22).
- Every commit builds and passes `go test ./...`.
- Worktree: `.claude/worktrees/queue-residency-stage3`, branch `worktree-queue-residency-stage3`, based on `origin/feat/queue-residency-stage2` (PR #1034) at 2b0d3d9c. Push refspec `HEAD:feat/queue-residency-stage3`. PR base `feat/queue-residency-stage2` until #1034 merges.

## Rulings taken at planning (recorded as D21–D23 in Task 0)

- **D21 — Stage 3's scope is the verifier alone.** Design §7 item 3 says "`verifyPausedJobs` and everything in §5 not yet removed". Re-derived from source on 2026-10-20: `SetRecoveryBytes`, `RestoreContent`, `describesSameJobAs`, `dispatch_jobs.recovery_bytes`, `manifestFile`, `manifestJSONFile` are gone; `AttachContent` stays by D12; `JobFile` and `NewManifest([]JobFile)` stay by D8 as the ingest shape. So the remaining §5 work is exactly the first bullet, plus `testdata/paused_verify_stop.spec`, which §5 does not list.
- **D22 — The prose sweep is stage 3's, not stage 4's.** Design §7 item 4 lists "limitation 1 (deleted)" and the `verifyPausedJobs` sentences as stage-4 contract work; the commit cycle requires the sentences a change falsifies to go with the change. Stage 3 deletes every sentence that describes the verifier; stage 4 becomes the holistic re-read of the four contracts for paraphrase and structure (the "one window" section, memory table, `go-standards.md`'s persistence paragraph, `test/crash` against §3).
- **D23 — A job restored paused at `Fetching` is verified on resume, not at startup.** User-visible: after a restart such a job shows the recorded-count estimate with the "progress from the download record" badge until the user resumes it, instead of its verified figure arriving a few seconds after startup. It is also no longer resident after a restart (about 1.6 MB per 20k articles, job-lifecycle §13, no longer held per paused job). Cost if wrong: the owner wanted the verified figure without a resume, and the verifier comes back as a background pass (design §9, "a background pass remains a pure addition").

## Review Focus

1. A rename of a paused job (`Dispatcher.SetName` → `LoadProgress`) is now the one path that hydrates a job without the user resuming it; `TestReevaluateStall_VerifyFaultOnAPausedJobKeepsItsReason` pins the fault case it reaches and its comment must name that path, not the verifier. Task 1.
2. `fileOwedUnwantedFailures`'s doc argued it needed no ordering against the verifier; with the verifier gone the paragraph goes, and nothing else may be rewritten into a universal ("the only startup hydration") without a grep. Task 1.
3. Two helpers in `paused_verify_test.go` are used by other test files (`pauseAtFetching` by `restore_progress_test.go`; `awaitEntered` by `postproc_cancel_test.go` and `postproc_handoff_dispatch_test.go`), and one test in it is not about the verifier (`TestFileOwedUnwantedFailures_FailedHydrationIsLoggedAndFiled`). They move; they are not deleted. Task 1.
4. `loose_record_waits.spec` loses one mutation and one test from its `run` line; the other mutations' kills must survive (`go run ./scripts/mutate` on the spec after the edit). Task 1.
5. The design cites `paused_verify_test.go` and `testdata/paused_verify.spec` as deletion targets; once deleted, `check_doc_citations` reports them. They are marked, not reworded: a design that names what to delete is correct to name it. Task 2.

---

### Task 0: Record D21–D23

**Files:** Modify `docs/superpowers/specs/2026-10-20-queue-residency-decisions.md` — append a `## 2026-10-20 — stage 3` heading and D21, D22, D23 in the existing `### D<n> — title` / **Call** / **Why** / **Cost if wrong** format, text from "Rulings taken at planning" above.

- [ ] Step 1: append; `go run ./scripts/check_doc_citations`; commit `docs: record stage-3 decisions D21–D23`. Batched with Task 1's dispatch.

### Task 1: Delete the verifier, retarget its tests, pin the deletion

**Files:**
- Modify: `internal/app/startup_reconcile.go` — delete `defaultPausedVerifyTimeout` and `verifyPausedJobs`; delete the third paragraph of `reconcileBeforeFirstTick`'s doc ("The jobs restored paused at Fetching are not loaded here…"); delete the second paragraph of `fileOwedUnwantedFailures`'s doc ("It needs no ordering against verifyPausedJobs…" through "…so a paused job never becomes owed."); drop the `time` import if it becomes unused.
- Modify: `internal/app/app.go` — delete the `pausedVerifiedHook` and `pausedVerifyTimeout` fields (~:299–307), the `app.pausedVerifyTimeout = defaultPausedVerifyTimeout` assignment in `New` (~:431), and the comment plus `app.wg.Go(func() { app.verifyPausedJobs(app.ctx) })` at the end of `Start` (~:1403–1408).
- Delete: `internal/app/paused_verify_test.go`. Before deleting, move `pauseAtFetching` and `awaitEntered` to `internal/app/loose_record_helpers_internal_test.go` (or the helper file their callers already import from; `grep -ln 'func lrAwait\|func lrWaitFor' internal/app/*_test.go` finds it), and move `TestFileOwedUnwantedFailures_FailedHydrationIsLoggedAndFiled` to `internal/app/startup_reconcile_test.go`. `releaser`, `blockingLoadHook`, `loadRecorder` and `syncEmitter` are deleted if `grep -n '<name>' internal/app/*_test.go` shows no caller outside the deleted file (on 2026-10-20 it shows none).
- Delete: `internal/app/testdata/paused_verify.spec`, `internal/app/testdata/paused_verify_stop.spec`.
- Modify: `internal/app/testdata/loose_record_waits.spec` — remove the `[no paused job is verified after startup]` mutation (it anchors in `startup_reconcile.go` on a line that no longer exists) and `TestLooseRecord_PausedJobReportsVerifiedProgress` from the `run` line.
- Modify: `internal/app/loose_record_test.go` — replace `TestLooseRecord_PausedJobReportsVerifiedProgress` with `TestLooseRecord_PausedJobReportsEstimateUntilResumed` (Step 1).
- Create: `TestStart_LeavesAJobRestoredPausedAtFetchingUnverified` in `internal/app/startup_reconcile_test.go` (Step 1) and `internal/app/testdata/no_startup_verifier.spec` (Step 5).
- Comments that name the verifier, each retargeted to what still holds (`git grep -n -E 'verifyPausedJobs|pausedVerif|PausedVerif' -- '*.go'` must return 0 lines when done):
  - `internal/app/residency.go` ~:108–111 (`Hydrate` doc): the waiter that must not report another hydration's deadline as its own is now a tick that waited on a rename's load (`Dispatcher.SetName`'s `LoadProgress`, under `setNameLoadTimeout`).
  - `internal/app/record.go` ~:631–633 (`enqueueResumedCompletion` doc): drop "and after Start from verifyPausedJobs".
  - `internal/app/verify_fault_park_test.go` ~:69–74: the case is "the job was already paused when its hydration faulted", reached through `Dispatcher.SetName`'s `LoadProgress` on a paused job.
  - `internal/dispatch/registry.go` ~:1209–1212 (`LoadProgress` doc): delete the verifier sentence; if the rewrite says `SetName` is its only caller, cite `git grep -n '\.LoadProgress(' -- '*.go' ':!*_test.go'` with the count observed.
  - `internal/app/startup_reconcile.go` `fileOwedUnwantedFailures` doc: after the paragraph deletion, the first paragraph must still read as complete.

**Interfaces:**
- Consumes: `lrEnv`/`lrApp` helpers (`newLREnv`, `env.newApp`, `a.start`, `a.registered`, `env.writeFileA`, `a.recordFileA`, `lrWaitFor`, `articleDone`, `lrArtLen`) as `paused_verify_test.go` uses them today; `Job.HasProgress()`, `Job.AllVerified()`, `Job.Resident()`, `Dispatcher.Row(id)`, `Dispatcher.ResumeJobByUser(id)`.
- Produces: nothing new in production code. Deleted symbols: `verifyPausedJobs`, `defaultPausedVerifyTimeout`, `Application.pausedVerifyTimeout`, `Application.pausedVerifiedHook`.

- [ ] **Step 1: Write the two pins** (they compile against the current tree and FAIL on it, because the verifier verifies the job):

```go
// internal/app/startup_reconcile_test.go
// TestStart_LeavesAJobRestoredPausedAtFetchingUnverified pins that no startup
// pass hydrates a paused job: after Start it carries the restore pass's seed
// and nothing more.
func TestStart_LeavesAJobRestoredPausedAtFetchingUnverified(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	j := pauseAtFetching(t, env, env.newApp(t), "paused")
	a2 := env.newApp(t)
	a2.start(t)
	j2 := a2.registered(t, j.ID())
	if !j2.HasProgress() { t.Fatal("the restored job has no progress after Start") }
	if j2.AllVerified() { t.Error("the restored paused job was verified during Start") }
	if j2.Resident() { t.Error("the restored paused job is resident after Start") }
	row, ok := a2.dispatcher.Row(j.ID())
	if !ok { t.Fatal("the job is not registered") }
	if want := row.Header.Bytes - 2*lrArtLen; row.RemainingBytes != want {
		t.Errorf("remaining = %d of %d, want %d estimated from the 2 recorded articles", row.RemainingBytes, row.Header.Bytes, want)
	}
}
```

```go
// internal/app/loose_record_test.go (replaces TestLooseRecord_PausedJobReportsVerifiedProgress)
// TestLooseRecord_PausedJobReportsEstimateUntilResumed pins the hand-off: a
// Fetching job restored paused reports the recorded-count estimate, and the
// user's resume is what verifies it.
func TestLooseRecord_PausedJobReportsEstimateUntilResumed(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	j := pauseAtFetching(t, env, env.newApp(t), "paused")
	a2 := env.newApp(t)
	a2.start(t)
	j2 := a2.registered(t, j.ID())
	if j2.AllVerified() || j2.Resident() { t.Fatal("the restored paused job was hydrated before its resume") }
	if err := a2.dispatcher.ResumeJobByUser(j.ID()); err != nil { t.Fatalf("ResumeJobByUser: %v", err) }
	lrWaitFor(t, "the resumed job to hold its lease on the verified content", func() bool {
		r, ok := a2.dispatcher.Row(j.ID())
		return ok && r.View.Holds && j2.Resident() && articleDone(j2, 1)
	})
	if !articleDone(j2, 0) { t.Error("a verified article is not Done after the resume") }
	if !j2.AllVerified() { t.Error("the resumed job is not verified after its hydration") }
	expected, remaining, _ := j2.ProgressFigures()
	if remaining >= expected { t.Errorf("remaining %d of %d after the resume", remaining, expected) }
}
```

- [ ] **Step 2: Run them → FAIL** on the unchanged tree: `go test -count=1 -run 'TestStart_LeavesAJobRestoredPausedAtFetchingUnverified|TestLooseRecord_PausedJobReportsEstimateUntilResumed' ./internal/app/` — expected: the verifier verifies the job, so `AllVerified()`/`Resident()` are true (record the message in the commit body). If the first test is flaky-green because the verifier's goroutine has not yet run, that is itself the reason the test must assert after `Start` returns and the verifier is gone; note it and proceed.
- [ ] **Step 3: Delete and retarget** per the Files list. `goimports -w`, `go fix ./...`, `go build ./...`, `go vet ./...`.
- [ ] **Step 4: Run** `go test -count=1 -race ./internal/app/ ./internal/dispatch/` → PASS; `git grep -n -E 'verifyPausedJobs|pausedVerif|PausedVerif' -- '*.go'` → 0 lines.
- [ ] **Step 5: `internal/app/testdata/no_startup_verifier.spec`** running both pins, one mutation that puts a startup hydration of paused jobs back:

```text
pkg ./internal/app/
run ^(TestStart_LeavesAJobRestoredPausedAtFetchingUnverified|TestLooseRecord_PausedJobReportsEstimateUntilResumed)$

[Start hydrates every paused job before it returns]
file internal/app/app.go
--- anchor
	app.sweepOrphans(app.ctx)
--- replace
	app.sweepOrphans(app.ctx)
	for _, row := range app.dispatcher.List() {
		if j, ok := app.dispatcher.Job(row.ID); ok && j.Intent() == job.IntentPause {
			_ = app.dispatcher.LoadProgress(app.ctx, row.ID)
		}
	}
--- end
```

  `go run ./scripts/mutate internal/app/testdata/no_startup_verifier.spec` → KILLED. If the anchor matches more than once, extend it with the preceding comment line. Then `go run ./scripts/mutate internal/app/testdata/loose_record_waits.spec` → every remaining mutation KILLED, and `go run ./scripts/mutate --check-all` → clean.
- [ ] **Step 6: Commit** `feat(app): verify a job restored paused at Fetching on its resume, not at startup` — body: what is deleted (symbols, files, specs), the observed red message from Step 2, the KILLED verdicts, and the `git grep` count of 0.

### Task 2: Sweep the prose the deletion falsified

**Files** (each site re-read in place, then rewritten to what still holds; a sentence that only described the verifier is deleted, not softened):
- `docs/ARCHITECTURE.md` ~:92 (the "After startup every job restored paused at `Fetching` is hydrated too (`verifyPausedJobs`)…" sentence) and ~:211 (the "and last launches the verification…" clause and the "For a job restored paused at `Fetching` that hydration is `verifyPausedJobs`'s…" sentence — a job restored paused is verified by the tick when the user resumes it).
- `docs/dispatch-contract.md` ~:291–300: "Three loads happen outside this rule. Two go through `LoadProgress`…" becomes two loads — `SetName`'s through `LoadProgress`, and `fileOwedUnwantedFailures`'s direct `Hydrate`.
- `docs/durability-contract.md` ~:387–392 (the `verifyPausedJobs` bullet in the "every path that attaches content" list — delete); Accepted limitation 1, second paragraph (~:1505–1535, "At startup the paused jobs are verified after the API listens…" — delete the whole paragraph; the first paragraph stays true as written); limitation 6 ~:1573–1575 (drop "or after startup the paused jobs' verifier and the `Shutdown` step that joins it (limitation 1)").
- `docs/job-lifecycle.md` ~:1384–1389 ("Startup adds one more: `verifyPausedJobs` hydrates…" and "The paused jobs are deliberately not capped" — a job restored paused holds no manifest until resumed); ~:1476–1479; ~:1539–1542; ~:1553–1556 (the list of who makes a first hydration: the tick, `fileOwedUnwantedFailures`, a rename's `LoadProgress`).
- `docs/superpowers/specs/2026-10-20-queue-residency-design.md` §5: the deleted files it names get `<!-- doccite:ok <token> — deleted by stage 3; the design names what to delete -->` markers if `check_doc_citations` reports them. The design's text is not otherwise edited.

- [ ] Step 1: `git grep -n -E 'verifyPausedJobs|pausedVerif|paused.jobs. verifier|startup verifier|verifier.*after Start' -- ':!docs/superpowers/plans/' ':!ui/dist'` — the list above plus whatever it adds; fix each.
- [ ] Step 2: read `docs/ARCHITECTURE.md` § "Compiler-Enforced Residency" and the startup-sequence step, `docs/durability-contract.md` §3 and Accepted limitations, `docs/dispatch-contract.md` § residency, and `docs/job-lifecycle.md` §11–§14 in full for paraphrase (a sentence about "a few seconds after startup", "while the API serves", "one at a time under a deadline", "stays resident until resumed" that names no symbol).
- [ ] Step 3: `go run ./scripts/check_doc_citations && go run ./scripts/check_citations && go run ./scripts/check_dup_comments && go run ./scripts/check_review_banner` → clean. The `git grep` from Step 1 → 0 lines outside `docs/superpowers/` (the design and decision log may name the verifier as the thing deleted).
- [ ] Step 4: commit `docs: a job restored paused at Fetching is verified on resume`.

### Task 3: Gates

- [ ] Step 1: `go fix ./... && goimports -l . && go vet ./... && go vet -tags=integration,uitest,crash ./...` → no output from `goimports -l`, vet clean.
- [ ] Step 2: `./scripts/run_tests.sh` → clean (race, alignment at 8, coverage, lock_io, govulncheck, every spec KILLED, integration, crash, UI, Playwright). A failure in `TestResumeAtStartup_StorageFaultStallsAndDoesNotFailArticles` is #1032 (pre-existing flake): re-run once.
- [ ] Step 3: `golangci-lint run ./...` → 0 issues; `go run ./scripts/check_test_doubles --all && go run ./scripts/mutate --check-all` → clean.
- [ ] Step 4: anything found → fix in a commit that names what it fixes; re-run the gate that found it.
