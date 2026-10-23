---
ArtifactMetadata:
  RequestFeedback: true
  UserFacing: true
---

# Assembler Job-Closed Resolver Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **Review aid only.** Per the owner's ruling, documents under `docs/superpowers/` are not
> merged. Gap-audit anything durable into `docs/durability-contract.md` and
> `docs/post-processing-contract.md` (Task 6), then delete this file before the PR merges.

**Goal:** Remove the assembler's unbounded `cancelledJobs` tombstone set (issue #1053) by having the assembler ask the pipeline whether a job still accepts writes at the moment it would open a file, instead of remembering which jobs are closed.

**Architecture:** The assembler already calls `Options.FileInfo` (the pipeline's `resolveFileInfo`) before it creates a file. That resolver gains the job-liveness check: it refuses with a new sentinel, `assembler.ErrJobClosed`, when the dispatcher no longer holds the instance that registered the file, or when that instance has been admitted to post-processing (`postProcAdmissions.has`, the same flag the downloader reads as `HandedOff`). The worker treats the sentinel as "drop the article silently", exactly as the tombstone check does today. Both state flips already happen before the close-handles or cancel control message is sent, and the worker processes that message before any later write, so a write arriving after the close observes the flipped state on the single worker goroutine. The tombstone map, its two writers and its reader are deleted. `ForgetJob` stays, narrowed to the per-file `completed` map.

**Tech Stack:** Go (version per `go.mod`), `scripts/mutate` specs for red checks, standard gates in `AGENTS.md`.

**Spec:** This document's "Design" section. The design was agreed in chat on 2026-10-23 as a small change; there is no separate spec file.

## Design

Three guards, each with one owner:

| Guard | Where | Refuses when | Protects |
|---|---|---|---|
| Resolver | `pipeline.resolveFileInfo` (`internal/app/pipeline.go:681`) | the cached instance is not `dispatcher.Job(id)`, or `handedOff(instance)` is true | the file open on the assembler worker: the gate that replaces the tombstone |
| Register | `pipeline.registerFileAttempt`, at the cache write under `p.mu` (`pipeline.go:648-668`) | the instance is not `dispatcher.Job(id)` | the `fileInfo` map from re-growing after a late article |
| Forget | `jobFinalizer.persistAndCommit`, after a successful `dispatcher.RemoveJob` (`internal/app/job_finalizer.go:391-395`) | always runs | entries a late article added during post-processing, which the Register guard now keeps from coming back |

The cached entry records the registering instance: the `fileInfo` map value changes from `assembler.FileInfo` to a struct carrying the `*job.Job` too. The resolver compares instances, not IDs, so a stale write from attempt 1 arriving after attempt 2 registers is refused. Today it is accepted (the case `internal/app/durability.go:116` describes as #914).

What does NOT change: the retry path keeps its `ForgetJob` call (`internal/app/app.go:2881`) because `ForgetJob` still drops the per-file `completed` entries for a job whose close-handles message timed out before reaching the worker. `handleFailureResult`'s order (register, mark failed, write) is untouched. The Register guard checks currency only, not admission, so a permanent failure arriving after the hand-over is still marked on the job as it is today.

Ordering facts the design rests on, each already in the tree:

- `enqueuePostProc` admits before it sends close-handles: `internal/app/app.go:2427` and the `has` doc at `internal/app/postproc_admission.go:396`.
- `RemoveJob` withdraws the admission, then `dispatcher.Remove`, and only then `assembler.CancelJob`: `internal/app/app.go:927`, `:948`, `:1009`. A failed `Remove` returns before the cancel is sent.
- The assembler stops before the dispatcher: `stopWorkers` at `app.go:1470` precedes `joinAndStop` at `app.go:1520`, so the resolver's dispatcher lookup is valid during the worker's shutdown drain.

## Global Constraints

- Standing Design Rule 1: no guard for state an earlier build wrote. None is added.
- Standing Design Rule 2: each guard above has one owner. Do not add a second site that decides "this job is closed".
- Standing Design Rule 4: every comment that quantifies (only, never, sole) carries the command that enumerated it. `check_citations` must agree, not report unverified.
- Red check is mechanical: every pin in this plan has a `scripts/mutate` spec, and each mutation must be `KILLED` before its commit.
- Never `git stash`. Work in a worktree under `.claude/worktrees/` with `ui/dist` copied and `ui/node_modules` symlinked from the main checkout. Push with `git push -u origin HEAD:fix/1053-job-closed-resolver`.
- Commits follow Conventional Commits with the Go package as scope. Every commit builds and passes `go test ./...`.
- Attribution line on every commit: `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

## Review Focus

1. **An admitted job whose file is already open.** A write between the admission and the close-handles arm lands on the open handle, as it does today; after the arm closes it, the next write re-opens through the resolver and is refused. Pinned by Task 3's `handed off after CloseJobHandles` subtest.
2. **A stale attempt-1 write after attempt-2 registers under the same ID.** Must be refused, because the cached instance is attempt 1. Pinned by Task 2's `TestResolveFileInfo_RefusesAnEarlierInstance`.
3. **A permanent failure (FatalErr) arriving after the hand-over.** Registers and marks as today, then the FatalErr write reaches the worker and is dropped at the resolver instead of at the tombstone. Pinned by Task 3's `FatalErr` subtest.
4. **CloseJobHandles timeout.** When the control message never reaches the worker, the job's handles stay open and a late write to an open file lands. That is today's behaviour too (the tombstone was set by the same arm that never ran). No test change; the executor re-reads `internal/assembler/closehandles_fault_test.go` and confirms nothing there asserts on the tombstone.
5. **The resolver called during the shutdown drain.** `dispatcher.Job` after `dispatcher.Stop` must not panic. The order above makes this unreachable; the executor confirms by reading `app.go:1434-1530` and records the two line numbers in the PR body.

---

### Task 1: The assembler drops an article whose resolver says the job is closed

**Files:**
- Modify: `internal/assembler/assembler.go:189-200` (Options doc), `:1310-1345` (processRequest error branch), `:1453-1456` (openTargetFile)
- Test: `internal/assembler/closed_job_test.go` (create)
- Spec: `internal/assembler/testdata/closed_job.spec` (create)

**Interfaces:**
- Produces: `var ErrJobClosed = errors.New("assembler: job no longer accepts writes")`, exported from `internal/assembler`. `Options.FileInfo` may return it, wrapped or bare. The worker then releases the article's buffer, calls `req.Release`, and returns without `OnWriteFault`, `OnArticlesUnwritten`, or a file open.

- [ ] **Step 1: Write the failing test**

```go
// closed_job_test.go, package assembler
func TestOpenTargetFile_ClosedJobDropsTheArticle(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	path := registerFile(t, dir, files, "job1", 0, 1)

	var closed atomic.Bool
	opts := makeOpts(dir, files)
	inner := opts.FileInfo
	opts.FileInfo = func(jobID string, fileIdx int) (FileInfo, error) {
		if closed.Load() {
			return FileInfo{}, fmt.Errorf("resolve %s: %w", jobID, ErrJobClosed)
		}
		return inner(jobID, fileIdx)
	}
	var faults, unwritten atomic.Int32
	opts.OnWriteFault = func(string, int, *storagefault.Fault) { faults.Add(1) }
	opts.OnArticlesUnwritten = func(string, int, []int32) { unwritten.Add(1) }

	a := startAssembler(t, opts)
	closed.Store(true)
	released := make(chan struct{})
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("late"),
		Release: func() { close(released) },
	})
	if err := a.Quiesce(t.Context()); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%s) = %v, want not-exist: a closed job's article opened a file nothing will close", path, err)
	}
	if n := faults.Load(); n != 0 {
		t.Errorf("OnWriteFault fired %d times, want 0: a closed job is not a storage fault", n)
	}
	if n := unwritten.Load(); n != 0 {
		t.Errorf("OnArticlesUnwritten fired %d times, want 0: the drop is silent, as the tombstone's was", n)
	}
	select {
	case <-released:
	default:
		t.Error("the dropped article's Release was not called")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestOpenTargetFile_ClosedJobDropsTheArticle -count=1 ./internal/assembler/`
Expected: FAIL. Compile error `undefined: ErrJobClosed` first; after adding only the variable, FAIL on `OnWriteFault fired 1 times` and `OnArticlesUnwritten fired 1 times`.

- [ ] **Step 3: Implement the sentinel and the drop path**

In `assembler.go`: declare `ErrJobClosed` next to `ErrNotStarted`/`ErrStopped`. In `openTargetFile`, when the resolver's error satisfies `errors.Is(err, ErrJobClosed)`, return it as is rather than through `storagefault.Classify`. In `processRequest`'s `openTargetFile` error branch, before `noteArticlesUnwritten`: on `errors.Is(err, ErrJobClosed)`, release `req.Data` via `a.releaseBuffer` when non-nil and return. (`req.Release` already runs via the `defer` in `dispatchRequest`.) Extend the `Options.FileInfo` doc: it may return `ErrJobClosed` for a job that no longer accepts writes, and the worker then drops the article silently; any other error is a `resolve` storage fault.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -run TestOpenTargetFile_ClosedJobDropsTheArticle -count=1 -race ./internal/assembler/`
Expected: PASS

- [ ] **Step 5: Write the red-check spec and run it**

```text
pkg ./internal/assembler/
run TestOpenTargetFile_ClosedJobDropsTheArticle$

# An article whose resolver answers ErrJobClosed is dropped silently; it is
# not a storage fault and no file is opened.

[the closed-job branch in processRequest is unreachable]
file internal/assembler/assembler.go
--- anchor
		if errors.Is(err, ErrJobClosed) {
--- replace
		if false && errors.Is(err, ErrJobClosed) {
--- end
```

Adjust the anchor to the exact line written in Step 3; it must match once. Run: `go run ./scripts/mutate internal/assembler/testdata/closed_job.spec`
Expected: `KILLED` for the one mutation, exit 0.

- [ ] **Step 6: Gates and commit**

Run: `goimports -w internal/assembler && go fix ./... && go vet ./... && go test -race -count=1 ./internal/assembler/`
Expected: all pass.

```bash
git add internal/assembler/assembler.go internal/assembler/closed_job_test.go internal/assembler/testdata/closed_job.spec
git commit -m "feat(assembler): drop an article whose resolver reports the job closed" -m "..." 
```
Body records the observed red output from Step 2 and Step 5.

---

### Task 2: The pipeline's resolver and register guards

**Files:**
- Modify: `internal/app/pipeline.go:106-150` (struct), `:537-668` (registerFile / registerFileAttempt), `:681-690` (resolveFileInfo), `:705-722` (forgetFile / forgetJob)
- Modify: `internal/app/app.go:497-510` (pipeline literal)
- Modify: `internal/app/pipeline_helpers_test.go:121` (map value type)
- Test: `internal/app/pipeline_closed_test.go` (create, `package app`)
- Spec: `internal/app/testdata/pipeline_closed.spec` (create)

**Interfaces:**
- Consumes: `assembler.ErrJobClosed` from Task 1.
- Produces:
  - `type fileEntry struct { info assembler.FileInfo; j *job.Job }`; `pipeline.fileInfo` becomes `map[fileKey]fileEntry`.
  - `pipeline.handedOff func(j *job.Job) bool` (nil means never handed off; wired in `app.go` to `app.postProcAdmissions.has`).
  - `resolveFileInfo(jobID string, fileIdx int) (assembler.FileInfo, error)` unchanged signature; returns an error wrapping `assembler.ErrJobClosed` when `p.dispatcher.Job(jobID)` is not `entry.j`, or `p.handedOff != nil && p.handedOff(entry.j)`.
  - `registerFile(j *job.Job, fileIdx int) error` unchanged signature; `registerFileAttempt` returns an error wrapping `assembler.ErrJobClosed` when, at the cache write under `p.mu`, `p.dispatcher.Job(j.ID())` is not `j`. The check sits inside the locked section so the write and the check are one critical section.
  - `handleSuccessResult` and `handleFailureResult`: when `registerFile` returns an error satisfying `errors.Is(err, assembler.ErrJobClosed)`, log at Debug, release the result, and return WITHOUT `clearEmitted`, matching the `isCurrent` drop at `pipeline.go:350-356`.

- [ ] **Step 1: Write the failing tests**

```go
// pipeline_closed_test.go, package app
func TestResolveFileInfo_RefusesAJobThatLeftTheDispatcher(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	j, hdr := newBareJob(t, application, "closed-left", "md5-closed-left")
	if err := application.Dispatcher().Add(context.Background(), j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	p := newPipeline(application.Dispatcher())
	if err := p.registerFile(j, 0); err != nil {
		t.Fatalf("registerFile: %v", err)
	}
	if err := application.Dispatcher().RemoveJob(context.Background(), j); err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}
	_, err := p.resolveFileInfo(j.ID(), 0)
	if !errors.Is(err, assembler.ErrJobClosed) {
		t.Fatalf("resolveFileInfo after RemoveJob = %v, want ErrJobClosed: the assembler would open a file for a job in history", err)
	}
}

func TestResolveFileInfo_RefusesAHandedOffJob(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	j, hdr := newBareJob(t, application, "closed-handed", "md5-closed-handed")
	if err := application.Dispatcher().Add(context.Background(), j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	p := newPipeline(application.Dispatcher())
	var handed atomic.Bool
	p.handedOff = func(got *job.Job) bool { return got == j && handed.Load() }
	if err := p.registerFile(j, 0); err != nil {
		t.Fatalf("registerFile: %v", err)
	}
	if _, err := p.resolveFileInfo(j.ID(), 0); err != nil {
		t.Fatalf("resolveFileInfo before the hand-over = %v, want nil", err)
	}
	handed.Store(true)
	_, err := p.resolveFileInfo(j.ID(), 0)
	if !errors.Is(err, assembler.ErrJobClosed) {
		t.Fatalf("resolveFileInfo after the hand-over = %v, want ErrJobClosed", err)
	}
}

func TestResolveFileInfo_RefusesAnEarlierInstance(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	first, _ := retriedUnderOneID(t, application, "feedface00105301")
	// retriedUnderOneID registers first, removes it, and registers second
	// under the same ID. Register file 0 from FIRST before the swap by
	// reproducing its steps inline if the helper cannot pause; the entry
	// must carry the first instance.
	p := newPipeline(application.Dispatcher())
	p.mu.Lock()
	p.fileInfo[fileKey{jobID: first.ID(), fileIdx: 0}] = fileEntry{info: assembler.FileInfo{Dir: t.TempDir(), Name: "f0", TotalParts: 1}, j: first}
	p.mu.Unlock()
	_, err := p.resolveFileInfo(first.ID(), 0)
	if !errors.Is(err, assembler.ErrJobClosed) {
		t.Fatalf("resolveFileInfo for an earlier instance = %v, want ErrJobClosed: attempt 1's write would land in attempt 2's file", err)
	}
}

func TestRegisterFile_RefusesOnceTheJobLeftTheDispatcher(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	j, hdr := newBareJob(t, application, "closed-reg", "md5-closed-reg")
	if err := application.Dispatcher().Add(context.Background(), j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	p := newPipeline(application.Dispatcher())
	inHook, release := make(chan struct{}), make(chan struct{})
	p.countedHook = func(*job.Job) { close(inHook); <-release }
	errCh := make(chan error, 1)
	go func() { errCh <- p.registerFile(j, 0) }()
	<-inHook
	if err := application.Dispatcher().RemoveJob(context.Background(), j); err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}
	close(release)
	if err := <-errCh; !errors.Is(err, assembler.ErrJobClosed) {
		t.Fatalf("registerFile that counted the file before RemoveJob = %v, want ErrJobClosed", err)
	}
	p.mu.RLock()
	n := len(p.fileInfo)
	p.mu.RUnlock()
	if n != 0 {
		t.Errorf("fileInfo holds %d entries for a job that left the dispatcher, want 0", n)
	}
}

func TestHandleSuccessResult_DropsAClosedJobsArticleWithoutReturningIt(t *testing.T) {
	// Same shape as TestHandleSuccessResult_ReturnsTheArticleWhenTheFileCannotBeRegistered
	// (pipeline_test.go): drive handleResult with a result whose registerFile
	// will return ErrJobClosed (use the nameReadHook + RemoveJob interleaving
	// above), and assert with that test's Emitted-bit accessor that the
	// article is STILL Emitted — the handler must not clearEmitted on this path.
}
```

Note on `TestResolveFileInfo_RefusesAnEarlierInstance`: `retriedUnderOneID` lives in `pipeline_instance_test.go`; use it as is and seed the entry directly, as shown. Note on the hook: `countedHook` runs after `registerFileAttempt` has read the name and counted the file's articles from the manifest, and before the locked cache write. Blocking there, rather than at `nameReadHook`, matters: `RemoveJob` evicts the manifest, and a count that ran after the eviction would fail with a residency error before reaching the guard this test pins. The window is still the one the issue describes: `isCurrent` has passed and the write has not happened.

Note on `TestHandleSuccessResult_DropsAClosedJobsArticleWithoutReturningIt`: use the same `countedHook` interleaving, driving `p.handleResult` on a goroutine instead of `p.registerFile`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestResolveFileInfo_|TestRegisterFile_RefusesOnce|TestHandleSuccessResult_DropsAClosed' -count=1 ./internal/app/`
Expected: compile failure on `fileEntry`, `p.handedOff`; after adding only the types and field, FAIL with `want ErrJobClosed` in each resolve/register test and the Emitted assertion in the handler test.

- [ ] **Step 3: Implement**

In `pipeline.go`: add `fileEntry`, change the map type, add `handedOff`. In `registerFileAttempt`'s locked section at `:648-668`, before the cache write: `if cur, ok := p.dispatcher.Job(jobID); !ok || cur != j { return fmt.Errorf("register %s[%d]: %w", jobID, fileIdx, assembler.ErrJobClosed) }`, then store `fileEntry{info, j}`. In `resolveFileInfo`, after the map lookup and still under `RLock` (the dispatcher lookup takes the registry's own lock, not `p.mu`, so no lock-order change): the two checks from the Interfaces block, returning a wrapped `assembler.ErrJobClosed`. Update `forgetFile`/`forgetJob` for the value type (no logic change). In both result handlers, add the `errors.Is(err, assembler.ErrJobClosed)` early return described in Interfaces, placed before the existing `clearEmitted` on a register failure. In `app.go:497`, add `handedOff: app.postProcAdmissions.has,` and change `fileInfo: make(map[fileKey]fileEntry)`. Fix `pipeline_helpers_test.go:121` to assign a `fileEntry`.

The doc comment on `resolveFileInfo` states the two conditions and the ordering fact they rely on (admission precedes close-handles; `RemoveJob` precedes the cancel message), citing `enqueuePostProc` and `RemoveJob` by name, not line number.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestResolveFileInfo_|TestRegisterFile_RefusesOnce|TestHandleSuccessResult_DropsAClosed|TestIsCurrent|TestRegisterFile' -count=1 -race ./internal/app/`
Expected: PASS

- [ ] **Step 5: Write the red-check spec and run it**

```text
pkg ./internal/app/
run Test(ResolveFileInfo_RefusesAJobThatLeftTheDispatcher|ResolveFileInfo_RefusesAHandedOffJob|ResolveFileInfo_RefusesAnEarlierInstance|RegisterFile_RefusesOnceTheJobLeftTheDispatcher|HandleSuccessResult_DropsAClosedJobsArticleWithoutReturningIt)$

# The resolver is the gate that replaced the assembler's job tombstone; the
# register guard keeps a late article from re-growing fileInfo.

[the resolver ignores the dispatcher]
file internal/app/pipeline.go
--- anchor
	<exact resolver currency condition line from Step 3>
--- replace
	<same line with the condition replaced by false>
--- end

[the resolver ignores the hand-over]
file internal/app/pipeline.go
--- anchor
	<exact resolver handedOff condition line>
--- replace
	<same line with the condition replaced by false>
--- end

[registerFile ignores the dispatcher]
file internal/app/pipeline.go
--- anchor
	<exact register currency condition line>
--- replace
	<same line with the condition replaced by false>
--- end

[the handler returns a closed job's article to the pool]
file internal/app/pipeline.go
--- anchor
	<exact errors.Is(err, assembler.ErrJobClosed) line in handleSuccessResult>
--- replace
	<same line with false &&>
--- end
```

Each anchor must match exactly once (the two resolver conditions must be on separate lines so they can be mutated separately; the handler's branch in `handleFailureResult` is a second site and gets its own mutation if a test covers it, otherwise record it as checked-by-reading in the PR body). Run: `go run ./scripts/mutate internal/app/testdata/pipeline_closed.spec`
Expected: every mutation `KILLED`, exit 0.

- [ ] **Step 6: Gates and commit**

Run: `goimports -w internal/app && go fix ./... && go vet ./... && go test -race -count=1 ./internal/app/`
Expected: PASS.

```bash
git add internal/app/pipeline.go internal/app/app.go internal/app/pipeline_helpers_test.go internal/app/pipeline_closed_test.go internal/app/testdata/pipeline_closed.spec
git commit -m "fix(app): refuse file info for a job that left the dispatcher or was handed off"
```
Body: the issue number, the two ordering facts, and the red outputs.

---

### Task 3: End-to-end pin: a write after filing creates no file

This is the issue's acceptance item: a result that passed `isCurrent` before the job left the dispatcher and writes after it still creates no file, driven deterministically.

**Files:**
- Test: `internal/app/pipeline_closed_test.go` (extend)

**Interfaces:**
- Consumes: `assembler.New(assembler.Options{FileInfo: p.resolveFileInfo, ...})` with the real worker started, as `TestHandleResult_AbandonsTheArticleWhenTheJobIsGone` (`pipeline_test.go:368`) builds one; `p.handedOff`; `Assembler.CloseJobHandles`, `Assembler.Quiesce`.

- [ ] **Step 1: Write the failing test**

```go
func TestWriteArticle_AfterTheJobClosedCreatesNoFile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		close func(t *testing.T, application *Application, p *pipeline, a *assembler.Assembler, j *job.Job)
		fatal bool
	}{
		{"removed from the dispatcher", func(t *testing.T, application *Application, _ *pipeline, _ *assembler.Assembler, j *job.Job) {
			if err := application.Dispatcher().RemoveJob(context.Background(), j); err != nil { t.Fatalf("RemoveJob: %v", err) }
		}, false},
		{"handed off after CloseJobHandles", func(t *testing.T, _ *Application, p *pipeline, a *assembler.Assembler, j *job.Job) {
			p.handedOff = func(got *job.Job) bool { return got == j }
			if err := a.CloseJobHandles(t.Context(), j.ID()); err != nil { t.Fatalf("CloseJobHandles: %v", err) }
		}, false},
		{"FatalErr after the hand-over", /* same close as above */ nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Add j; build p with a real started assembler whose FileInfo is
			// p.resolveFileInfo and whose OnWriteFault counts; registerFile(j, 0)
			// so the cache holds the entry (the window the issue describes:
			// registered while resident, written after); tc.close(...);
			// a.WriteArticle(ctx, refFor(res), WriteRequest{...FatalErr set when tc.fatal});
			// a.Quiesce(ctx).
			// Assert: os.Stat(info.Path()) is not-exist; OnWriteFault count == 0.
		})
	}
}
```

The executor fills the body from `pipeline_test.go:368-387` and Task 1's test; the plan fixes the three cases and the two assertions.

- [ ] **Step 2: Run the test to verify it fails against a resolver with the guard disabled**

Run Task 2's spec with this test added to its `run` line, or temporarily neuter the resolver's currency condition by hand and restore from your own copy (never `git checkout --`). Expected: FAIL on `os.Stat ... want not-exist` for the first case and `OnWriteFault` or `not-exist` for the second.

- [ ] **Step 3: Run it to verify it passes on the real tree**

Run: `go test -run TestWriteArticle_AfterTheJobClosedCreatesNoFile -count=1 -race ./internal/app/`
Expected: PASS

- [ ] **Step 4: Add the test to the spec's run line and re-run the spec**

Run: `go run ./scripts/mutate internal/app/testdata/pipeline_closed.spec`
Expected: every mutation `KILLED`.

- [ ] **Step 5: Commit**

```bash
git add internal/app/pipeline_closed_test.go internal/app/testdata/pipeline_closed.spec
git commit -m "test(app): pin that a write after the job closed opens no file"
```

---

### Task 4: Delete the job-level tombstone from the assembler

Only after Tasks 1-3: the tree must never lack the guard.

**Files:**
- Modify: `internal/assembler/assembler.go:812-846` (ForgetJob doc), `:964`, `:982`, `:1010`, `:1040-1056` (dispatchRequest signature and doc), `:1073-1088` (cancel arm comment and write), `:1104-1116` (close-handles arm comment and write), `:1140-1156` (ForgetJob arm), `:1170-1178` (the read), `:1184-1190` (dropFileTombstones doc)
- Modify: `internal/assembler/control.go:22-25`
- Modify: `internal/app/durability.go:116-118`, `internal/app/app.go:2877-2880` (retry comment), `internal/app/app.go:2427-2432` (hand-over comment: "dropped by the assembler's whole-job tombstone")
- Delete tests: `TestForgetJob_DropsTheCancelledMark` (`forgetjob_test.go:71`), `TestCloseJobHandles_TombstonesTheWholeJob` (`closehandles_tombstone_test.go:97`), `TestCloseJobHandles_TombstonesEvenWhenTheSyncFailed` (`:26`), `TestCancelJob_KeepFilesStillTombstonesTheWholeJob` (`cancel_test.go:252`)
- Rewrite tests: `TestCancelJob_RejectsLateArticles` (`cancel_test.go:76`), `TestJobTombstone_DropsTheJobsFileTombstones` (`tombstone_sweep_test.go:21`), `TestForgetJob_DropsOnlyTheNamedJobsTombstones` (`forgetjob_test.go:25`) and any other test that passes a `cancelledJobs` map to `dispatchRequest` (`git grep -n 'dispatchRequest(' -- 'internal/assembler/*_test.go'`)
- Delete spec: `internal/assembler/testdata/closehandles_job_tombstone.spec`
- Modify specs: `internal/assembler/testdata/job_tombstone_sweep.spec` (drop the last mutation and the two deleted tests from `run`), `internal/app/testdata/retry_forget_job.spec` (drop the second mutation; keep the first)

**Interfaces:**
- Produces: `dispatchRequest(req WriteRequest, open map[fileKey]*openFile, completed map[fileKey]struct{}) int`. `ForgetJob` doc says it drops the job's per-file `completed` entries, which matter for a retry of a job whose close-handles message never reached the worker; nothing else. `Assembler.CancelJob` / `CloseJobHandles` docs no longer mention a job tombstone; they say a later article for the job is refused at the open by the resolver (`Options.FileInfo` returning `ErrJobClosed`).

- [ ] **Step 1: Rewrite `TestCancelJob_RejectsLateArticles` to express the new contract**

After `CancelJob`, flip the test's resolver to answer `ErrJobClosed` for `job1` (the same `atomic.Bool` wrapper as Task 1), then send the late articles. Keep both assertions (no completion, file does not exist). The comment says the late article is refused at the open, by the resolver, and that `TestOpenTargetFile_ClosedJobDropsTheArticle` pins the worker's half.

- [ ] **Step 2: Delete the four tombstone tests and the `cancelledJobs` assertion in `TestJobTombstone_DropsTheJobsFileTombstones`**

That test keeps its per-file sweep assertions and the bystander check; remove the `cancelledJobs` map and the `if _, ok := cancelledJobs["job"]` block.

- [ ] **Step 3: Run the package to verify it fails to compile**

Run: `go test -count=1 ./internal/assembler/`
Expected: compile errors at every remaining `cancelledJobs` use in tests, which enumerates them for Step 4.

- [ ] **Step 4: Delete the map**

Remove the declaration, the parameter, both call-site arguments, the two writes, the delete, and the read-and-drop block. Rewrite the cancel-arm and close-handles-arm comments: each arm closes the job's handles and drops its per-file tombstones; what refuses a later article for the job is the resolver, on the worker, at the open. Rewrite the `ForgetJob` doc to the narrowed meaning. Rewrite `control.go:22-25`. Update `durability.go:116-118` (a write of an earlier attempt blocked in `WriteArticle` is now refused at the open because the resolver compares instances; cite `TestResolveFileInfo_RefusesAnEarlierInstance`). Update the retry comment at `app.go:2877-2880` and the hand-over comment at `app.go:2427-2432`.

- [ ] **Step 5: Update the specs and run them**

Edit `job_tombstone_sweep.spec` and `retry_forget_job.spec` as listed; delete `closehandles_job_tombstone.spec`. Run:
```bash
go run ./scripts/mutate --check-all
go run ./scripts/mutate internal/assembler/testdata/job_tombstone_sweep.spec
go run ./scripts/mutate internal/app/testdata/retry_forget_job.spec
go run ./scripts/mutate internal/assembler/testdata/closed_job.spec
```
Expected: `--check-all` clean; every mutation `KILLED`.

- [ ] **Step 6: Gates and commit**

Run: `goimports -w . && go fix ./... && go vet ./... && go test -race -count=1 ./internal/assembler/ ./internal/app/ && go run ./scripts/check_citations && go run ./scripts/check_dup_comments`
Expected: PASS; `check_citations` reports *agree* for the `dispatchRequest` and `ackCh` citations (their counts are unchanged: two callers, four senders).

```bash
git add internal/assembler internal/app/durability.go internal/app/app.go internal/app/testdata/retry_forget_job.spec
git commit -m "refactor(assembler): remove the job-level tombstone set" -m "Closes #1053 ..."
```
Body: the enumeration of every former writer and reader of `cancelledJobs` with the grep that found them, and the measured size of the leak (16-byte IDs, about 50 bytes per entry).

---

### Task 5: The finalizer forgets the job's file info after filing

**Files:**
- Modify: `internal/app/job_finalizer.go:391-400` (after the successful `RemoveJob`)
- Test: `internal/app/job_finalizer_instance_test.go` (extend, `package app`)
- Spec: `internal/app/testdata/finalizer_forget_fileinfo.spec` (create)

**Interfaces:**
- Consumes: `pipeline.forgetJob(jobID string)` (`pipeline.go:715`), `fileEntry` from Task 2.
- Produces: nothing new. The comment at the call site says why this is the second `forgetJob` on the finalize path and why it is not a second owner: the hand-over's call at `app.go:2479` runs while the job is still current, so a late article can re-register; this one runs once it is not, so nothing can.

- [ ] **Step 1: Write the failing test**

```go
func TestPersistAndCommit_DropsTheJobsFileInfo(t *testing.T) {
	app, _, _ := newLifecycleTestApp(t)
	const id = "feedface00105302"
	j := job.New(id, "fileinfo", job.Policy{})
	if err := app.dispatcher.Add(context.Background(), j, dispatch.Header{Name: "fileinfo"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// If the fixture's app.pipeline is nil, build one as pipeline_test.go:373
	// does, with app.dispatcher, and assign it to app.pipeline.
	app.pipeline.mu.Lock()
	app.pipeline.fileInfo[fileKey{jobID: id, fileIdx: 0}] = fileEntry{info: assembler.FileInfo{Dir: t.TempDir(), Name: "f0", TotalParts: 1}, j: j}
	app.pipeline.mu.Unlock()

	ppJob := &postproc.Job{Job: j}
	if err := app.finalizer.persistAndCommit(app.log, buildHistoryEntry(ppJob), ppJob); err != nil {
		t.Fatalf("persistAndCommit: %v", err)
	}
	app.pipeline.mu.RLock()
	_, still := app.pipeline.fileInfo[fileKey{jobID: id, fileIdx: 0}]
	app.pipeline.mu.RUnlock()
	if still {
		t.Error("fileInfo still holds the filed job's entry: a late article re-added it during post-processing and nothing drops it again")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestPersistAndCommit_DropsTheJobsFileInfo -count=1 ./internal/app/`
Expected: FAIL with `fileInfo still holds the filed job's entry`.

- [ ] **Step 3: Implement**

In `persistAndCommit`, immediately after the `RemoveJob` retry block succeeds (the `err == nil` path), call `app.pipeline.forgetJob(ppJob.Job.ID())` guarded on `app.pipeline != nil`, with the comment from Interfaces.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -run TestPersistAndCommit_DropsTheJobsFileInfo -count=1 -race ./internal/app/`
Expected: PASS

- [ ] **Step 5: Spec and run**

```text
pkg ./internal/app/
run TestPersistAndCommit_DropsTheJobsFileInfo$

# Once the job has left the dispatcher nothing can re-register its files, so
# this forget is the one that bounds fileInfo.

[the finalizer leaves the job's file info]
file internal/app/job_finalizer.go
--- anchor
		app.pipeline.forgetJob(ppJob.Job.ID())
--- replace
		_ = ppJob.Job.ID()
--- end
```
Run: `go run ./scripts/mutate internal/app/testdata/finalizer_forget_fileinfo.spec`
Expected: `KILLED`.

- [ ] **Step 6: Commit**

```bash
git add internal/app/job_finalizer.go internal/app/job_finalizer_instance_test.go internal/app/testdata/finalizer_forget_fileinfo.spec
git commit -m "fix(app): drop a filed job's cached file info after it leaves the dispatcher"
```

---

### Task 6: Sweep the docs and comments the change falsified

**Files:**
- Modify: `docs/durability-contract.md:976-979` (job tombstone paragraph), `:1247-1249` (control table rows for CancelJob, CloseJobHandles, ForgetJob), `:941-942`, `:1523`
- Modify: `docs/post-processing-contract.md:146-151` (cites `TestCloseJobHandles_TombstonesTheWholeJob`, which Task 4 deleted; `check_doc_citations` will fail until this is fixed)
- Leave: `docs/go-standards.md:344` (general advice about per-file tombstones, still true for `completed`)

- [ ] **Step 1: Grep the claim, not the file**

Run from the root:
```bash
git grep -n -i 'job-level tombstone\|whole-job tombstone\|tombstones the job\|job tombstone\|cancelledJobs'
git grep -n 'TombstonesTheWholeJob\|DropsTheCancelledMark\|KeepFilesStillTombstones'
git grep -n -E 'ForgetJob.*(tombstone|clears)'
```
Every hit outside `docs/superpowers/` is rewritten or confirmed still true. Record the three commands and their final counts in the PR body.

- [ ] **Step 2: Rewrite each passage**

The durability-contract paragraph at `:976-979` becomes: a job instance whose handles were closed is not downloaded again; the downloader skips an admitted job, and an article still in flight is refused at the open because the pipeline's resolver answers `assembler.ErrJobClosed` for an instance that is admitted or no longer the dispatcher's; a retry is a new instance, so its writes resolve; `RetryHistoryJob`'s `ForgetJob` clears the per-file `completed` entries of a job whose close-handles message never reached the worker. Name `TestWriteArticle_AfterTheJobClosedCreatesNoFile` and `TestResolveFileInfo_RefusesAnEarlierInstance` as the pins. The table rows drop the "tombstones the job" clauses; the ForgetJob row says "drops the job's per-file `completed` entries". The post-processing-contract passage names `TestWriteArticle_AfterTheJobClosedCreatesNoFile` in place of the deleted test.

- [ ] **Step 3: Read `docs/durability-contract.md` § on the assembler and `docs/post-processing-contract.md` § on the hand-over in full**

Not grep: the paraphrase pass the sweep rules require. Fix any sentence that describes the assembler as remembering closed jobs.

- [ ] **Step 4: Run the whole-repository gates**

```bash
go run ./scripts/check_doc_citations
go run ./scripts/check_citations
go run ./scripts/check_dup_comments
go run ./scripts/check_review_banner
go run ./scripts/check_test_doubles --all
go run ./scripts/mutate --check-all
```
Expected: all clean.

- [ ] **Step 5: Commit**

```bash
git add docs/durability-contract.md docs/post-processing-contract.md
git commit -m "docs: describe the resolver as the gate on a closed job's late articles"
```

---

### Task 7: Full gates, comment analysis, and the PR

- [ ] **Step 1: Full gate block**

```bash
go fix ./... && goimports -w . && go vet ./... && go vet -tags=integration,uitest,crash ./... && go test -race ./... && ./scripts/run_tests.sh && golangci-lint run ./...
```
Expected: all pass. `check_coverage` and `check_test_alignment` run inside `run_tests.sh`; a finding on an untouched helper in `pipeline.go` or `assembler.go` is file-scoped debt (see `AGENTS.md` Gate Semantics) and is answered with a real test, named in the PR body.

- [ ] **Step 2: Run `pr-review-toolkit:comment-analyzer` over `git diff main...HEAD`** and triage its findings.

- [ ] **Step 3: Run `quality-lenses` in `diff` mode** and triage per item.

- [ ] **Step 4: Delete this plan file from the branch** (`git rm docs/superpowers/plans/2026-10-23-assembler-job-closed-resolver-plan.md`) once Task 6 has moved anything durable into the contracts.

- [ ] **Step 5: Push and open the PR**

```bash
git push -u origin HEAD:fix/1053-job-closed-resolver
```
PR body (via `file-pullreq`), containing: the problem as corrected by the audit (about 50 bytes per filed or cancelled job, plus the `fileInfo` re-registration leak); the design table from this plan; the Rule 4 enumerations (every former `cancelledJobs` writer and reader; every site that opens a job file, which is `openTargetFile` only: `git grep -n 'openInDir(' -- 'internal/assembler/*.go' ':!*_test.go'`); the two ordering facts with their line numbers; the five Review Focus items with the test that pins each or the reading that checked it; and the observed red output of every spec. Do not merge; wait for the user's review agent verdict on the current head.
