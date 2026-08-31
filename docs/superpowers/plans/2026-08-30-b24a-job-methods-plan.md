# B2.4a — move the per-job methods onto `*Job` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move 18 single-job method bodies off `*Queue` onto `*Job`, leaving `Queue` as lookup
plus queue-level bookkeeping, with no caller change and no behaviour change.

**Architecture:** Every one of these methods has the same shape today — take `q.mu`, resolve an
ID to a `*Job`, do work that touches only that job, then set `q.dirty` / notify / persist. The
first and last parts are queue concerns; the middle is not, and it is what has to live on `Job`
before any caller can hold a `*Job` and operate on it directly. This half performs that
extraction and nothing else: `q.mu` remains the only lock, and every moved method stays
unexported and is called only from a `Queue` wrapper that holds it.

**Tech Stack:** Go 1.27.0, `internal/queue`, no new dependencies.

**Spec:** RFC #456 (`gh issue view 456`), D2 row **B2.4a**; roadmap in
`docs/superpowers/specs/2026-08-28-sched-exported-surface-design.md`; tier model in
`docs/queue-lifecycle.md`.

## Global Constraints

- **No caller outside `internal/queue` changes.** `Queue`'s exported surface keeps every method,
  signature, error and doc comment it has today. A diff touching any file outside
  `internal/queue/` is out of scope for this half.
- **No behaviour change.** This is a pure extraction. Every existing test must pass unmodified;
  a test that needs editing to stay green is a signal the extraction was not faithful, not a
  test to fix.
- **No lock change.** `q.mu` stays the sole guard on job content. Moved methods are unexported
  and carry a `Must be called with q.mu held` line. See "What this half deliberately does not
  do" below.
- `//nocover:` markers are not an option; new unexported helpers on `Job` are covered by the
  existing tests that drive their `Queue` wrappers, which is what `check_test_alignment` wants.
- Every quality gate in AGENTS.md § Quality Gates, plus the three whole-repo gates. `go test
  -race ./...` is separate from `run_tests.sh` and both are required.

---

## Why this half exists, and what it deliberately does not do

RFC #456's D2 table describes B2.4a as "move the ~21 lookup-and-delegate methods from `Queue`
onto `Job`; **make `Job` self-locking**". Those are two changes with very different risk, and
this plan does only the first.

**The move is mechanical and locally verifiable.** Each method's per-job body is lifted verbatim
onto `Job`; the wrapper keeps the lock, the lookup and the bookkeeping. Nothing about
concurrency changes, so every existing test is a regression test for the extraction.

**Making `Job` self-locking is a concurrency redesign** and is not mechanical. Today `q.mu` is a
single global lock that guards, at once: the queue's collections (`jobs`, `byID`, `promoting`,
`paused`), and *every job's mutable content* — `Job.Status`, `JobProgress`'s fields, and the
`Manifest` a job points at. `Job.residencyMu` exists but guards only the manifest/progress
**pointer** fields and the manifest-derived scalars, explicitly not their contents
(`internal/queue/job.go`, `residencyMu`'s doc comment). Giving `Job` a lock over its content
therefore has to answer questions this half does not raise:

- **Cross-job atomicity is lost.** `ClearAllEmitted`, `ForEachUnfinishedArticle`,
  `TotalRemainingBytes` and the four `Has*`/`ExistsBy*` predicates read many jobs' content under
  one `q.mu`. With per-job locks they become non-atomic across jobs, and whether each of them
  cares needs an answer per method, not in general.
- **`failedGen` / `failedPersistMu` are ordered against `q.mu`.** `queue.go`'s comment on
  `failedPersistMu` spells out an INSERT-versus-DELETE race that the current nesting closes; a
  third lock changes that argument's premises.
- **Lock order gains an edge.** The tree documents `q.mu` → `residencyMu` today. A content lock
  is a third node, and `ForEachUnfinishedArticle` already hoists work specifically to avoid
  taking `residencyMu` inside a `q.mu` walk.

Splitting them costs one extra PR and buys a red-green check that is actually possible: with the
move landed and behaviour unchanged, the locking PR's diff is *only* the locking, and a race the
race detector finds is unambiguously attributable to it. Landing both at once produces a diff in
which every hunk is both.

**Sequencing consequence:** RFC #456's B2.4b (repoint progress-only callers onto `*Job`) needs
the exported, self-locking methods, so it depends on the locking half, not on this one. The D2
table should be amended to read **B2.4a (move)** → **B2.4a₂ (self-locking)** → **B2.4b**.

---

## The transformation, stated once

Every method in the inventory below has this shape:

```go
func (q *Queue) SetWarning(jobID, warning string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	job.Warning = warning          // <- the part that moves
	q.dirty.Store(true)
	return nil
}
```

and becomes:

```go
// setWarning records a human-readable warning on the job.
// Must be called with q.mu held for writing.
func (j *Job) setWarning(warning string) {
	j.Warning = warning
}

func (q *Queue) SetWarning(jobID, warning string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	job.setWarning(warning)
	q.dirty.Store(true)
	return nil
}
```

**Three rules that decide where each line goes.** They are what make this mechanical rather than
a judgement call per method:

1. **Lock, lookup, `q.dirty`, `q.notifyLocked()` and `q.store.Update` stay on the wrapper.** They
   are queue-level bookkeeping and persistence, not job state. A moved method that flips
   `q.dirty` would need a back-pointer, which is the thing this half exists to remove.
2. **Anything reading queue-level config stays on the wrapper.** Only `SetName` is affected: it
   sanitizes through `q.sOpts`. The wrapper sanitizes, the `Job` method takes the finished name.
3. **A method that changes what the wrapper does after it returns returns that fact.** The
   pattern already exists — `undeferRecoveryLocked` returns `changed` and the caller decides
   whether to mark dirty and notify. Keep it; do not have `Job` methods signal by side effect.

---

## File Structure

| File | Responsibility after this half |
|---|---|
| `internal/queue/job.go` | Gains `Job.resident()` and the moved progress-tier methods. Already 1,024 lines; the manifest-tier methods go elsewhere to avoid pushing it past `queue.go`'s size. |
| `internal/queue/job_articles.go` | **New.** The moved manifest-tier methods — the ones that resolve a file index or article index against a resident `Manifest`. Kept together because they share the residency precondition and the gate test keys on it. |
| `internal/queue/workset.go` | Two of its four methods shrink to wrappers (Task 3); the other two stay whole, with reasons in the out-of-scope table. |
| `internal/queue/queue.go` | Loses ~250 lines of method bodies; keeps every exported signature, its locking, and its bookkeeping. |
| `internal/queue/manifest_gate_test.go` | Extended to walk `*Job` methods as well as `*Queue` methods (Task 1). Without this the gate goes blind the moment Task 3 runs. |

---

## The inventory

**Eighteen method bodies move.** Grouped by which tier the body touches, because that decides
whether the residency gate applies. Two further methods are listed as *not* moving, so a
reviewer does not go looking for a missing hunk.

### Progress-tier — body touches `job.progress` or plain `Job` fields only (9 move)

| `*Queue` method | Line | Becomes | Wrapper keeps |
|---|---|---|---|
| `SetPP` | queue.go:1047 | `(*Job).setPP` | dirty |
| `SetName` | queue.go:1065 | `(*Job).setName` | dirty, `q.sOpts` sanitize |
| `SetScript` | queue.go:1082 | `(*Job).setScript` | dirty |
| `SetWarning` | queue.go:1899 | `(*Job).setWarning` | dirty |
| `SetPar2ReleaseReason` | queue.go:1870 | `(*Job).setPar2ReleaseReason` | dirty |
| `RecordDownload` | queue.go:1412 | `(*Job).recordDownload` | dirty |
| `MarkJobStarted` | queue.go:1392 | `(*Job).markStartedOnce` | dirty, `q.store.Update` |
| `MarkDownloadFinished` | queue.go:1370 | `(*Job).markDownloadFinishedOnce` | dirty, `q.store.Update` |
| `DiscardDeferredPar2` | queue.go:1951 | `(*Job).discardDeferredPar2` | dirty (conditional on returned `changed`) |

**Two name choices in that table are forced, not stylistic.** `Job` already has an exported
`MarkDownloadFinished` (`job.go:888`) whose semantics differ from the queue's — it assigns
unconditionally, where `Queue.MarkDownloadFinished` applies "first finish wins" via an `IsZero`
test. Naming the moved body `markDownloadFinished` would put two methods with different
semantics one capital letter apart. `markDownloadFinishedOnce` names the condition instead, and
returns `bool` so the wrapper knows whether to persist. `markStartedOnce` matches for the same
reason.

> **Rule 2 finding, recorded and deferred.** `git grep -n 'downloadFinished =' -- '*.go'
> ':!*_test.go'` returns 6 lines. Split by what each does:
>
> | Class | Sites | First-wins? |
> |---|---|---|
> | Applies a transition | `queue.go:1381` (`Queue.MarkDownloadFinished`), `queue.go:1360` (`SetPostProcStarted`) | yes, both `IsZero`-guarded |
> | Applies a transition | `job.go:890` (`Job.MarkDownloadFinished`) | **no** |
> | Resets | `job.go:849` (`ResetForRetry`) | n/a |
> | Restores persisted state | `sqlite_store.go:571`, `progress.go:919` | n/a |
>
> Restores and the reset are a different class and are not the finding. **Of the three writers
> that apply the finish transition, two enforce first-wins and one does not** — a condition with
> more than one writer that disagree, which is Standing Design Rule 2's second smell. It
> predates this plan, and `Job.MarkDownloadFinished`'s callers are all `_test.go` today, which
> is why it has stayed invisible. **File it as an issue; do not fix it here**, because giving
> that method the first-wins test is a behaviour change and this half asserts it makes none.

### Manifest-tier — body dereferences `job.manifest`, so residency gates it (7 move)

| `*Queue` method | Line | Becomes | Wrapper keeps |
|---|---|---|---|
| `CountUnfinishedArticles` | queue.go:348 | `(*Job).countUnfinishedArticles` | RLock, lookup |
| `MarkArticleEmittedByIdx` | queue.go:1551 | `(*Job).markArticleEmittedByIdx` | — |
| `ClearArticleEmittedByIdx` | queue.go:1576 | `(*Job).clearArticleEmittedByIdx` | `q.notifyLocked()` |
| `MarkFileComplete` | queue.go:2000 | `(*Job).markFileComplete` | dirty |
| `SetFileFilename` | queue.go:2016 | `(*Job).setFileFilename` | dirty |
| `SetFileCRC32FromRuns` | queue.go:2084 | `(*Job).setFileCRC32FromRuns` | dirty |
| `UndeferRecoveryVolumes` | queue.go:1853 | delegates to the moved helper below | dirty, notify |
| `undeferRecoveryLocked` | queue.go:1980 | `(*Job).undeferRecovery` — already returns `changed`; drop its two `q.` lines | dirty, notify |
| `AckDurable` | workset.go:48 | `(*Job).ackDurable` returning `(invalid, nArt int, err error)` | dirty, and the `q.log.Warn` — which already runs **after** the unlock, so it stays put unchanged |
| `SeedFromRuns` | workset.go:248 | `(*Job).seedFromRuns` | dirty |

### Listed to record a decision, not work (2)

| Method | Why nothing moves |
|---|---|
| `CheckEarlyAbort` (queue.go:1830) | Body is already a single delegation to `job.IsEarlyAbort()`. The wrapper keeps only the residency check. |
| `GetJobStatus` (queue.go:270) | Body is `return job.Status` — a header-tier field read, not progress or manifest. Extracting it would produce a method whose only content is the field access it wraps, and it is deleted outright by B2.4d. |

### Explicitly out of scope, with reasons

| Method | Why not |
|---|---|
| `SetStatus`, `SetStatusIf`, `SetPostProcStarted` | Their bodies call `hydrateResidentLocked`, `evictJobLocked` and `PromoteNext` — residency and promotion are queue work, not job work. Their per-job core is already factored as `setStatusLocked`. All three are rewritten by B2.4d anyway. |
| `AckPermanentFailure` (`workset.go:127`) | The one method that participates in the `failedGen`/`failedPersistMu` protocol, which is queue-wide by construction — `failedPersistMu`'s comment states one counter for the whole queue is deliberate. Extracting its core means deciding what the generation check means per job, which is design work, not a move. |
| `ReplaceFromRuns` (`workset.go:343`) | Calls `hydrateJobLocked` and `evictJobLocked` — it hydrates a non-resident job to do its work and may evict afterwards. Residency transitions are queue work. |
| `Add`, `Remove`, `Retry`, `Pause`, `Resume`, `Reorder`, `PromoteNext`, `SetPriority`, `SetCategory` | Mutate queue order, membership or the active set. These are the scheduling tier and are `internal/dispatch`'s in B2.4c. |
| `ClearAllEmitted`, `ForEachUnfinishedArticle`, `TotalRemainingBytes`, `HasDownloadableJobs`, `HasDownloadingJobs`, `HasPostProcJobs`, `ExistsByName`, `ExistsByMD5` | Iterate every job. Their per-job step could move, but the cross-job atomicity question belongs to B2.4a₂, and moving them now would answer it silently. |

---

## Task 1: Re-home the residency gate onto `Job`, and make the gate test see it

This task is first and is not optional ordering. `TestManifestAccessIsGated` walks only methods
whose receiver is `*Queue` (`isQueueMethod`, `manifest_gate_test.go:96`). The moment Task 3
moves a manifest-touching body onto `*Job`, that method leaves the gate's field of view — and
the test still passes, because its vacuity guards only check that *some* `*Queue` method is
still covered. The protection would evaporate silently, which is exactly the failure mode #261
created the gate for.

**Files:**
- Modify: `internal/queue/job.go` (add `resident`)
- Modify: `internal/queue/queue.go:328-346` (`residentJob` delegates)
- Modify: `internal/queue/manifest_gate_test.go:38-120`

**Interfaces:**
- Produces: `func (j *Job) resident() error` — returns `ErrJobNotResident` (no ID in the message;
  the caller has it) when `j.manifest == nil || j.progress == nil`, else nil. Tasks 3 and 4
  consume it.
- Produces: `manifestGateExempt` **re-keyed from `"Method"` to `"Receiver.Method"`**, and six
  new `Job.*` entries. See Step 3a — this is the part that makes Task 1 land green rather than
  red.

- [ ] **Step 1: Write the failing gate-coverage test**

Add to `manifest_gate_test.go`. It asserts the walk covers `*Job`, using a fixture the walk must
find — not a hand-maintained list, which is what went stale twice before.

```go
// TestManifestGateCoversJobMethods pins that the gate's AST walk sees *Job
// methods, not only *Queue ones. B2.4a moves the manifest-tier bodies onto
// *Job; a walk that still matched only *Queue would keep passing while
// covering none of them.
func TestManifestGateCoversJobMethods(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	src := `package queue
func (j *Job) ungatedProbe() int { return j.manifest.NumFiles() }`
	file, err := parser.ParseFile(fset, "probe.go", src, 0)
	if err != nil {
		t.Fatalf("parse probe: %v", err)
	}
	fn := file.Decls[0].(*ast.FuncDecl)
	if !isGatedReceiver(fn) {
		t.Fatal("isGatedReceiver rejected a *Job method; the gate cannot see the tier B2.4a moves onto Job")
	}
	touches, gated := manifestUse(fn.Body)
	if !touches || gated {
		t.Fatalf("manifestUse(probe) = (%v, %v), want (true, false)", touches, gated)
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

```bash
go test -count=1 ./internal/queue/ -run TestManifestGateCoversJobMethods
```

Expected: a compile failure on `isGatedReceiver` (undefined). Then, after renaming
`isQueueMethod` → `isGatedReceiver` with its body unchanged, re-run and expect the assertion to
fail: `isGatedReceiver rejected a *Job method`. **Record the second message**, not the compile
error — AGENTS.md § Step 2 in practice: a compile error does not demonstrate the test
discriminates.

- [ ] **Step 3: Widen the receiver check and the gate predicate**

```go
// isGatedReceiver reports whether fn is a method on *Queue or *Job. Both are
// in scope: the manifest tier is moving from the first to the second (B2.4a),
// and a walk that matched only *Queue would go blind exactly as it moved.
func isGatedReceiver(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && (id.Name == "Queue" || id.Name == "Job")
}
```

In `manifestUse`, count `resident` as a gate call alongside `residentJob`, so a `*Job` method
that checks its own residency is not reported. Update the failure message to name the receiver
rather than hardcoding `(*Queue).`.

- [ ] **Step 3a: Re-key the exemption map and exempt the six `*Job` methods that already exist**

**Widening the receiver check turns the gate red immediately, before a single body has moved.**
Six methods on `*Job` dereference `j.manifest` today without any residency call, and the widened
walk sees all of them:

```
$ awk '/^func \(j \*Job\)/{...}' internal/queue/job.go   # bodies matching /\.manifest/
Manifest  setResidency  setHydrateFailure  ResetForRetry  MarshalJSON  UnmarshalJSON
```

Each is legitimately exempt, and for a reason that is not "we did not get to it":

```go
// Keyed "Receiver.Method", not "Method". Two receivers are now walked, and a
// bare method name would let an exemption written for one silently cover a
// same-named method on the other — the aliasing this map's own stale-entry
// check exists to prevent.
var manifestGateExempt = map[string]string{
	// ... the existing entries, re-keyed with a "Queue." prefix ...
	"Job.Manifest":          "is the fallible accessor the tier model requires; reporting absence is its whole job",
	"Job.setResidency":      "assigns the manifest; establishes residency rather than relying on it",
	"Job.setHydrateFailure": "records why hydration failed; runs precisely when there is no manifest",
	"Job.ResetForRetry":     "re-derives progress from the manifest it is handed; a caller that got here has one",
	"Job.MarshalJSON":       "serialises whatever tier is present; a non-resident job marshals without one by design",
	"Job.UnmarshalJSON":     "constructs the manifest; cannot require one to be present",
}
```

**Verify each of those six reasons against the method rather than copying this table.** A reason
that turns out to be wrong is worse than no exemption: this map's polarity means an entry
permanently re-permits the next method that takes that name.

Then confirm the gate is green *before* any body moves:

```bash
go test -count=1 ./internal/queue/ -run TestManifestAccessIsGated
```

A red result here means one of the six is not actually exempt-worthy, or a seventh exists that
the `awk` scan missed — investigate rather than adding an entry to make it pass.

- [ ] **Step 4: Add `Job.resident` and delegate `residentJob` to it**

```go
// resident reports whether this job's evictable tier is in memory, returning
// ErrJobNotResident when it is not. It is the manifest gate in its Job-level
// form: every *Job method that dereferences j.manifest calls it first, so a
// non-resident job is reported rather than silently skipped (#261).
//
// The progress == nil clause is defence in depth, not a reachable state — see
// Queue.residentJob's comment, which carries the full argument for both
// clauses and is the one place it is written down.
//
// Must be called with q.mu held; it reads the pointer fields residencyMu also
// guards, and every caller reaches it through a Queue method that holds q.mu.
func (j *Job) resident() error {
	if j.manifest == nil || j.progress == nil {
		return ErrJobNotResident
	}
	return nil
}
```

`Queue.residentJob` keeps its own doc comment and its `%w: %s` wrapping, and its body becomes
the lookup plus `job.resident()`. It stays the gate for `*Queue` methods; nothing that calls it
changes.

- [ ] **Step 5: Run the gates and commit**

```bash
go test -count=1 -race ./internal/queue/
go vet ./... && golangci-lint run ./internal/queue/...
git add internal/queue/job.go internal/queue/queue.go internal/queue/manifest_gate_test.go
git commit -m "refactor(queue): give the manifest gate a Job-level form and a Job-aware walk"
```

---

## Task 2: Move the nine progress-tier bodies

**Files:**
- Modify: `internal/queue/job.go` (add the ten methods)
- Modify: `internal/queue/queue.go` (nine bodies shrink to wrappers)

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces: `(*Job).setPP`, `.setName`, `.setScript`, `.setWarning`, `.setPar2ReleaseReason`,
  `.recordDownload`, `.markStartedOnce`, `.markDownloadFinishedOnce`, `.discardDeferredPar2` —
  all unexported, all documented `Must be called with q.mu held`. Signatures mirror the
  wrapper's minus the ID. `discardDeferredPar2`, `markStartedOnce` and
  `markDownloadFinishedOnce` return `bool` so the wrapper knows whether to mark dirty and
  persist.

- [ ] **Step 1: Move one method and confirm the suite still passes**

Start with `SetWarning`, exactly as shown in "The transformation, stated once". Run:

```bash
go test -count=1 ./internal/queue/ -run TestSetWarning
```

Expected: PASS, unchanged. **There is no red step here and that is correct** — this is an
extraction, not a fix, so the discriminating check is the mutation check in Step 3, not a test
that fails first. (AGENTS.md's red-green rule governs bug fixes; see also the memory on
refactor-shaped changes needing a partial rather than whole-file revert.)

- [ ] **Step 2: Move the remaining eight**

Two need more than a lift:

```go
// setName records the display name. The caller sanitizes: CleanupName and
// SanitizeFolderName read Queue.sOpts, which is queue-level configuration
// and does not belong on a Job.
func (j *Job) setName(name string) { j.Name = name }
```

```go
// discardDeferredPar2 marks every on-demand recovery volume as never-fetch,
// and reports whether anything changed so the caller can decide about
// q.dirty. Progress-tier only: it walks progress.files by index and reads no
// manifest, so it needs no residency gate.
func (j *Job) discardDeferredPar2() bool {
	changed := false
	for fi := range len(j.progress.files) {
		if j.progress.files[fi].Fetch == FetchIfNeeded {
			j.progress.files[fi].Fetch = FetchNever
			changed = true
		}
	}
	return changed
}
```

- [ ] **Step 3: Prove the extraction is load-bearing, per method**

An extraction that compiles is not evidence the body arrived intact. For each moved method,
neuter one line of the *moved* body and confirm a test fails:

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/queue/job.go "$SCRATCH/job.bak.go"
# neuter one line inside ONE moved method — e.g. make setWarning assign "" —
# anchoring on text unique to that method, not on a shape repeated elsewhere
go test -count=1 ./internal/queue/ -run TestSetWarning   # MUST fail
cp "$SCRATCH/job.bak.go" internal/queue/job.go
```

`-count=1` is mandatory: a cached `ok` replays the pre-mutation pass and reads as "the test does
not discriminate", which is the opposite of the truth. Record each observed message. Nine
methods, nine separate mutations — one passing says nothing about the others.

**`markDownloadFinishedOnce` needs its mutation aimed at the `IsZero` test specifically**, not
at the assignment. Deleting the assignment fails loudly; deleting the first-wins condition is
the silent half, and it is the exact semantic that distinguishes this method from the existing
exported `Job.MarkDownloadFinished` two lines away in the same file.

**If a method has no test that fails when its body is neutered, that is a coverage gap this
task surfaced, and the fix is a real test.** Do not paper over it.

- [ ] **Step 4: Gates and commit**

```bash
goimports -w internal/queue/ && go fix ./... && go build ./...
go test -count=1 -race ./internal/queue/ && go vet ./...
golangci-lint run ./internal/queue/...
git add internal/queue/job.go internal/queue/queue.go
git commit -m "refactor(queue): move the progress-tier per-job bodies onto Job"
```

---

## Task 3: Move the nine manifest-tier bodies (7 from queue.go, 2 from workset.go)

**Files:**
- Create: `internal/queue/job_articles.go`
- Modify: `internal/queue/queue.go`
- Modify: `internal/queue/workset.go` (`AckDurable`, `SeedFromRuns` shrink to wrappers)

**Interfaces:**
- Consumes: `(*Job).resident` from Task 1.
- Produces: `(*Job).countUnfinishedArticles`, `.markArticleEmittedByIdx`,
  `.clearArticleEmittedByIdx`, `.markFileComplete`, `.setFileFilename`, `.setFileCRC32FromRuns`,
  `.undeferRecovery`, `.ackDurable`, `.seedFromRuns`. All unexported; each calls `j.resident()`
  first and returns its error.
- `ackDurable` returns `(invalid, nArt int, err error)`. The counts go back to the wrapper
  because the `q.log.Warn` that consumes them runs **after** `q.mu.Unlock()` today
  (`workset.go`, under the `--- No lock held below this line ---` marker) and must keep doing
  so — moving the log onto `Job` would either put I/O under the lock or hand `Job` a logger it
  has no other use for.

- [ ] **Step 1: Write the gate-compliance pin before moving anything**

```go
// TestJobArticleMethodsGateOnResidency pins that the moved manifest-tier
// methods report non-residency rather than acting on a nil manifest. The AST
// gate proves they CALL resident(); this proves they RETURN its error, which
// the AST cannot see.
func TestJobArticleMethodsGateOnResidency(t *testing.T) {
	t.Parallel()
	j := &Job{ID: "j1", progress: &JobProgress{}} // manifest deliberately nil
	if err := j.markFileComplete(0); !errors.Is(err, ErrJobNotResident) {
		t.Errorf("markFileComplete on a non-resident job = %v, want ErrJobNotResident", err)
	}
	if _, err := j.countUnfinishedArticles(0); !errors.Is(err, ErrJobNotResident) {
		t.Errorf("countUnfinishedArticles on a non-resident job = %v, want ErrJobNotResident", err)
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

```bash
go test -count=1 ./internal/queue/ -run TestJobArticleMethodsGateOnResidency
```

Expected: compile failure (methods undefined). That is the expected state for a test written
ahead of the move; the discriminating check for this task is Step 4's mutation, which removes
the `resident()` call and must turn this test red while the build still succeeds.

- [ ] **Step 3: Create `job_articles.go` and move the nine bodies**

Each follows this shape — the residency check that was `q.residentJob(jobID)` becomes
`j.resident()`, and the wrapper's lookup drops to `q.byID`:

```go
// markFileComplete marks the file at fileIdx as fully assembled.
// Returns ErrJobNotResident if the manifest is not in memory, and an
// out-of-range error for a genuine bad index — kept distinct, because
// reporting a de-hydrated job as a caller bug misdiagnoses it.
//
// Must be called with q.mu held for writing.
func (j *Job) markFileComplete(fileIdx int) error {
	if err := j.resident(); err != nil {
		return err
	}
	// ... body lifted verbatim from Queue.MarkFileComplete ...
}
```

**The wrapper must not blanket-wrap what the `Job` method returns.** These methods return two
error shapes, and only one of them wants the ID appended:

| Shape produced today | By | Wanted after the split |
|---|---|---|
| `"queue: job not resident: j1"` | `residentJob`'s `fmt.Errorf("%w: %s", …)` | wrapper appends the ID |
| `"queue: fileIdx 9 out of range for job j1"` | the method body, already carrying the ID | **passed through untouched** |

A blanket `fmt.Errorf("%w: %s", err, jobID)` yields
`"queue: fileIdx 9 out of range for job j1: j1"`. So the `Job` method formats its own index
errors from `j.ID`, and the wrapper wraps **only** the residency sentinel:

```go
func (q *Queue) MarkFileComplete(jobID string, fileIdx int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	if err := job.markFileComplete(fileIdx); err != nil {
		if errors.Is(err, ErrJobNotResident) {
			return fmt.Errorf("%w: %s", err, jobID)
		}
		return err // already names the job; see the table above
	}
	q.dirty.Store(true)
	return nil
}
```

`RecordDownload` is the one wrapper that must **not** gain the `%w: %s` shape: it returns a bare
`ErrNotFound` today (`queue.go:1417`), not a wrapped one, and matching the neighbours would be a
behaviour change smuggled in as tidying.

- [ ] **Step 3a: Pin the error strings before changing them**

Three current messages are the contract. Assert on them so a regression is a test failure rather
than a reader's discovery:

```go
func TestMovedMethodsPreserveErrorStrings(t *testing.T) {
	t.Parallel()
	// ... resident job with a 1-file manifest, id "j1" ...
	if err := q.MarkFileComplete("j1", 9); err == nil ||
		err.Error() != "queue: fileIdx 9 out of range for job j1" {
		t.Errorf("MarkFileComplete out-of-range = %v, want the un-suffixed message", err)
	}
	if err := q.RecordDownload("nope", "srv", 1); !errors.Is(err, ErrNotFound) ||
		err.Error() != ErrNotFound.Error() {
		t.Errorf("RecordDownload miss = %q, want the bare sentinel", err)
	}
}
```

Run it against the **unmoved** code first: it must pass before Step 3 and after it. A pin that
only passes afterwards is describing the new behaviour, not preserving the old.

`undeferRecovery` keeps its `changed` return and loses only the `q.dirty.Store(true)` and
`q.notifyLocked()` lines, which move to `Queue.UndeferRecoveryVolumes`.

- [ ] **Step 4: Prove the residency gate is live in the moved code**

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/queue/job_articles.go "$SCRATCH/ja.bak.go"
# delete ONLY the `if err := j.resident(); err != nil { return err }` block
# from markFileComplete — anchor on text unique to that method
go test -count=1 ./internal/queue/ -run 'TestJobArticleMethodsGateOnResidency|TestManifestAccessIsGated'
# MUST fail: the pin on the returned error, AND the AST gate
cp "$SCRATCH/ja.bak.go" internal/queue/job_articles.go
```

Both must go red. The AST gate going red is the evidence Task 1 actually re-homed the walk — if
only the runtime pin fails, `isGatedReceiver` is not seeing `job_articles.go` and Task 1 is
incomplete.

- [ ] **Step 5: Prune `manifestGateExempt` if the move emptied an entry**

The list errors on *stale* entries as well as missing gates. `ForEachUnfinishedArticle`,
`ClearAllEmitted`, `Retry`, `PromoteNext`, `SnapshotJob`, `Add`, `hydrateJobLocked`,
`hydrateResidentLocked` and `residentJob` all stay on `*Queue` and keep their entries.
`undeferRecoveryLocked`'s entry — "takes an already-resolved `*Job` from a gated caller" — is
the one that changes: the method moves to `*Job` and gains its own `resident()` call, so **the
entry is deleted**, not renamed. Run the gate to confirm rather than reasoning about it:

```bash
go test -count=1 ./internal/queue/ -run TestManifestAccessIsGated
```

- [ ] **Step 6: Gates and commit**

```bash
goimports -w internal/queue/ && go build ./...
go test -count=1 -race ./internal/queue/ && go vet ./...
golangci-lint run ./internal/queue/...
git add internal/queue/job_articles.go internal/queue/queue.go internal/queue/manifest_gate_test.go
git commit -m "refactor(queue): move the manifest-tier per-job bodies onto Job"
```

---

## Task 4: The claim sweep

The change falsifies sentences in places the diff does not touch. AGENTS.md § Step 4 in
practice: grep the **literal**, from the repository root, not the concept.

**Files:** whatever the greps return. Expect `internal/queue/queue.go`, `internal/queue/job.go`,
`docs/queue-lifecycle.md`, possibly `docs/ARCHITECTURE.md`.

- [ ] **Step 1: Sweep the claims this change invalidated**

```bash
git grep -n 'every \*Queue method'
git grep -n 'residentJob'
git grep -n 'Must be called with q.mu'
git grep -n 'isQueueMethod'
git grep -n 'manifestGateExempt'
```

`Queue.residentJob`'s own doc comment is the primary casualty. It currently says "every
manifest-tier mutation routes through here" and "`TestManifestAccessIsGated` walks the package
AST and fails any **\*Queue method** that dereferences `job.manifest` without calling this".
After Task 3 both are false as written: the mutations route through `Job.resident`, and the walk
covers two receivers.

**Narrowing a referent must not broaden a scope.** The rewrite says what still holds and names
what was checked — "`residentJob` is the gate for `*Queue` methods; `Job.resident` is the gate
for `*Job` methods; `TestManifestAccessIsGated` walks both" — not "nothing bypasses the gate".

- [ ] **Step 2: Read the contract docs in full, not by grep**

`docs/queue-lifecycle.md` § Residency and § Enforcement describe where the gate lives. Read both
sections end to end; a doc restates a claim in prose and shares no token with the code, which is
precisely what the greps above cannot find.

- [ ] **Step 3: Run the whole-repo gates**

```bash
go run ./scripts/check_dup_comments     # job_articles.go's headers vs queue.go's originals
go run ./scripts/check_review_banner
git add -A && go run ./scripts/check_citations
```

`check_dup_comments` is the one most likely to fire: moving a body and its doc comment leaves
two copies of a multi-line block if the original is not fully deleted. The fix is to delete the
original, never a `//dupcomment:ok`.

`check_citations` scans **tracked** `.go` files, so run it after `git add`. It counts output
lines, so a `grep -c` in a comment always reads as one match. Any citation in the touched files
whose count moved — `residentJob`'s call sites drop as wrappers stop calling it — must be
re-run and corrected, not estimated.

- [ ] **Step 4: Full gates, then commit the sweep**

```bash
go test -race ./... && ./scripts/run_tests.sh
golangci-lint run ./...
git commit -m "docs(queue): correct the gate's stated scope after the Job-level split"
```

---

## Task 5: Verify no caller changed, and open the PR

- [ ] **Step 1: Prove the exported surface is untouched**

```bash
git diff main...HEAD --stat -- ':!internal/queue/*'
```

Expected: empty, except `docs/`. A non-empty result outside `internal/queue/` and `docs/` means
the extraction leaked and the plan's first Global Constraint is violated.

- [ ] **Step 2: Prove every commit builds independently**

```bash
for c in $(git rev-list --reverse main..HEAD); do
  git checkout -q --detach "$c" && go build ./... && go vet ./... || echo "BROKEN $c"
done
git checkout -q -
```

- [ ] **Step 3: Integration tests**

`test/integration/` is excluded from the default gates by its build tag, and this half touches
`internal/queue`, which the pipeline consumes:

```bash
go test -v -tags=integration ./test/integration/...
```

- [ ] **Step 4: PR**

Body states: what moved, what deliberately did not and why (the locking split), the observed
mutation messages from Tasks 2 and 3, and the amendment to RFC #456's D2 table.

---

## Inconclusive / Deferred items

- **Which lock `Job` gets in B2.4a₂, and what happens to cross-job atomicity.** Not decided
  here, and this half is written so it does not have to be.
  *Probe:* for each of the eight multi-job methods listed in "Explicitly out of scope", ask
  whether any caller depends on reading two jobs at one instant.
  *Expected branches:* (a) none does → per-job locks, `Queue` iterates and locks each in turn;
  (b) at least one does → that method keeps a queue-level read lock and the ordering
  `q.mu` → `job.mu` becomes load-bearing and must be documented and tested.

- **Whether `Job.resident()` should take `residencyMu`.** This plan says no: every caller
  reaches it under `q.mu`, and taking `residencyMu` inside a `q.mu` walk is the pattern
  `ForEachUnfinishedArticle` explicitly hoists work to avoid. The answer changes under
  B2.4a₂.
  *Probe:* `grep -n 'residencyMu' internal/queue/job.go` and check whether any moved method's
  new callers can reach it without `q.mu`.
  *Expected branches:* (a) none can → leave as planned; (b) some can → the method needs the
  read lock, and its doc comment's "Must be called with q.mu held" is wrong.

- **Whether `job_articles.go` is the right file boundary.** Chosen so `job.go` does not grow past
  `queue.go`. If Task 3's moved bodies come in under ~150 lines, folding them into `job.go` is
  simpler and the split is not worth a file.
  *Probe:* `wc -l internal/queue/job_articles.go` after Task 3, Step 3.
  *Expected branches:* under 150 → fold in; over → keep the file.

- **`check_test_alignment` on `queue.go`.** The gate scans every unexported helper in a *touched
  file*, not the diff, and `queue.go` is a hot file. It may surface a long-standing untested
  helper unrelated to this change.
  *Probe:* run it after Task 2.
  *Expected branches:* clean → proceed; a finding → write the real test and say in the commit
  body that it is pre-existing debt surfaced by the touch, so the scope is legible.

## Non-goals

- Making `Job` self-locking, exporting the moved methods, or changing any lock. B2.4a₂.
- Moving any caller off `Queue`. B2.4b.
- Anything to do with `constants.Status`. B2.4d.
- Renaming the package to `internal/jobstate`. B2.4e — the name is settled (RFC #456 D1), the
  move is not this half.
