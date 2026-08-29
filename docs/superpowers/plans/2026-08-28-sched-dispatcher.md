# Half B2.3 — `internal/dispatch` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `internal/dispatch` — the loop that drives `internal/sched`, owns the job registry and manifest residency, observes worker exits, and composes a queue listing.

**Architecture:** A single ticker goroutine walks a registry in queue order calling `sched.Queue.Advance` on each job; a size-1 buffered channel lets callers kick it early without becoming a second owner of liveness. The dispatcher launches every worker and observes every exit, calling `Settle` on terminal completion and `Park` on every other exit — the one fact the tick cannot compute for itself. Manifest residency is derived from pool membership rather than separately bounded.

**Tech Stack:** Go 1.27.0. Standard library only — `sync`, `time`, `context`, `testing`. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-08-28-sched-dispatcher-design.md` (decisions D-B7 … D-B14). Read it before Task 1; it carries the argument this plan executes.

## Global Constraints

- **Module path** `github.com/hobeone/gonzbd`. Go 1.27.0.
- **`internal/dispatch` imports `internal/sched`, `internal/job`, and the standard library only.** It must not import `internal/queue`, `internal/downloader`, or `internal/postproc`. The dependency points from B2 to B1, never the reverse.
- **`dispatch.mu` is NEVER held across a call into `sched`** (D-B9). Lock order: `dispatch.mu` → (released) → `Queue.mu` → `Job.mu`.
- **No new exported door on `sched` beyond `RenderAll`** (Task 1). Adding one silently falsifies the enumerations in five files.
- **After editing any `.go` file:** `goimports -w <file>`, then `go build ./...`. Do **not** run `go fix ./...` or `goimports -w .` repo-wide — it touches files outside the task and has caused unrelated-file churn on this branch before.
- **Every behavioural test must be verified by observed mutation** — neuter the change, watch the named test fail *for the right reason*, restore, watch it pass. `go test -count=1` is mandatory; a cached `ok` is not an observation, and a build failure is not an observation of a behavioural pin.
- **Never `git stash`** — the stash stack is shared with other sessions. Never `git checkout -- <path>` — it discards unrelated uncommitted edits. To revert for a mutation check, copy the file to a scratch dir first and copy it back.
- **Quality gates before every commit:** `go vet ./...`, `go test -race -count=1 ./internal/...`, `golangci-lint run ./...`.
- **Whole-repo gates before the final commit:** `go run ./scripts/check_dup_comments`, `go run ./scripts/check_review_banner`, `go run ./scripts/check_citations`.
- **Diff-scoped gates:** `check_coverage` (80% whole-function on any function containing a changed line), `check_test_alignment` (every unexported helper in a *touched file*), `check_lock_io`. A failure on code you did not write is working as intended — fix it with a real test, never a dummy reference or a `//nocover:` on branching code.
- **Comments that quantify over a population** (*only*, *sole*, *never*, *always*, *the one place*) must state the enumeration actually performed, with the command. `check_citations` executes any backticked `grep`/`git grep` that states a count.
- **Conventional Commits.** Scope is the package: `feat(dispatch)`, `refactor(sched)`. Footer `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/sched/render.go` (modify) | Extract `renderLocked`; add `RenderAll`. |
| `internal/sched/lock_enumeration_test.go` (modify) | Extend to assert which doors reach a `*job.Job` method — Rule 4's "write the test, not the sentence". |
| `internal/dispatch/doc.go` (create) | Package doc: what it owns, what it deliberately does not. |
| `internal/dispatch/registry.go` (create) | `Header`, `Row`, the registry map + order, `Add`, `List`, `remove`. |
| `internal/dispatch/dispatch.go` (create) | `Dispatcher`, `New`, `Start`, `Stop`, the ticker goroutine and the kick. |
| `internal/dispatch/tick.go` (create) | One tick: the walk, `Advance`, residency reconciliation, D-B12 eviction, worker launch. |
| `internal/dispatch/worker.go` (create) | `Finished`, `Yielded`, the launch path and its intent re-verify. |
| `internal/dispatch/ports.go` (create) | The interfaces `dispatch` defines for others to implement: `Residency`, `Store`, `Runner`. |
| `internal/dispatch/fakes_test.go` (create) | In-memory `Residency`, `Store`, `Runner`, `Workers` for tests. |

**Why `Residency` is an interface that names no manifest type.** `Manifest` lives in `internal/queue` and does not migrate until B2.4, and `dispatch` has no business parsing one anyway. It decides *when* a job should be resident (D-B8) and delegates *what to load*:

```go
type Residency interface {
	Hydrate(ctx context.Context, id string) error
	Evict(id string)
}
```

This keeps D-B8 implementable and testable now, and is the correct boundary regardless: residency is a scheduling decision, manifest contents are data the scheduler never reads.

---

## Task 1: `renderLocked` and `RenderAll` in `internal/sched`

**Files:**
- Modify: `internal/sched/render.go`
- Modify: `internal/sched/lock_enumeration_test.go`
- Modify (comments only): `internal/sched/queue.go`, `internal/sched/doc.go`, `internal/sched/pool.go`, `internal/job/job.go`
- Test: `internal/sched/render_test.go`

**Interfaces:**
- Consumes: `job.RenderView`, `job.Snapshot`, existing unexported `q.waitReason`, `q.running`.
- Produces: `func (q *Queue) RenderAll(js []*job.Job) []job.RenderView` — one `Queue.mu` acquisition for the whole slice. Returns a slice of the same length and order as `js`; a nil or empty input returns an empty non-nil slice.

- [ ] **Step 1: Write the failing test for one-lock-per-listing**

Add to `internal/sched/render_test.go`. This needs two goroutines on two *different* jobs: a single-goroutine test exercises these lines constantly and constrains the lock not at all.

```go
func TestRenderAll_TakesTheQueueLockOnce(t *testing.T) {
	q := New(2, 2, testClock, &stubWorkers{})
	a := job.New("a", "n", job.Policy{})
	b := job.New("b", "n", job.Policy{})
	for _, j := range []*job.Job{a, b} {
		if err := j.BeginAttempt(q.now()); err != nil {
			t.Fatalf("BeginAttempt: %v", err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 200 {
			_ = q.Advance(b)
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			got := q.RenderAll([]*job.Job{a, b})
			if len(got) != 2 {
				t.Errorf("RenderAll returned %d rows, want 2", len(got))
			}
		}
	}()
	wg.Wait()
}

func TestRenderAll_MatchesRenderPerJob(t *testing.T) {
	q := New(2, 2, testClock, &stubWorkers{})
	a := job.New("a", "n", job.Policy{})
	b := job.New("b", "n", job.Policy{})
	if err := a.BeginAttempt(q.now()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}

	all := q.RenderAll([]*job.Job{a, b})
	if len(all) != 2 {
		t.Fatalf("RenderAll returned %d rows, want 2", len(all))
	}
	for i, j := range []*job.Job{a, b} {
		if want := q.Render(j); all[i] != want {
			t.Errorf("row %d = %+v, want %+v — RenderAll and Render must compute the same view", i, all[i], want)
		}
	}
}

func TestRenderAll_EmptyInputReturnsEmptyNonNil(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	got := q.RenderAll(nil)
	if got == nil {
		t.Fatal("RenderAll(nil) = nil, want an empty non-nil slice")
	}
	if len(got) != 0 {
		t.Fatalf("RenderAll(nil) has %d rows, want 0", len(got))
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 -race ./internal/sched/ -run TestRenderAll`
Expected: FAIL — `q.RenderAll undefined (type *Queue has no field or method RenderAll)`.

This is a compile failure, which proves only that the method is absent. The behavioural pin comes from the mutation in Step 6.

- [ ] **Step 3: Extract `renderLocked` and add `RenderAll`**

Replace `Render`'s body in `internal/sched/render.go`, keeping the existing doc comment on `Render` and adding new ones. `renderLocked` is the sole computer of a `RenderView`; both doors call it.

```go
func (q *Queue) Render(j *job.Job) job.RenderView {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.renderLocked(j)
}

// RenderAll composes one view per job in js, in the same order, under a SINGLE
// q.mu acquisition. A loop over Render would take the lock once per job, and a
// transition landing between two of them yields a listing that was true at no
// instant — job 3 rendered Downloading and job 300 Queued when nothing ever
// held both. That is the same tear Render's own comment rejects, one layer up.
//
// It shares renderLocked with Render rather than duplicating the composition:
// there is one function that computes a RenderView, and two doors that differ
// only in how many jobs they lock around.
//
// The returned slice is always non-nil, so a caller may range over it without
// a nil check.
func (q *Queue) RenderAll(js []*job.Job) []job.RenderView {
	out := make([]job.RenderView, 0, len(js))
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range js {
		out = append(out, q.renderLocked(j))
	}
	return out
}

// renderLocked is the sole computer of a job.RenderView. The caller must hold
// q.mu. Lock order is prior spec §7.1's: q.mu already held here, then Job.mu
// inside Snapshot.
func (q *Queue) renderLocked(j *job.Job) job.RenderView {
	s := j.Snapshot()
	reason, _ := q.waitReason(j.ID(), s)
	return job.RenderView{
		StateView: s.State,
		Running:   q.running(j.ID(), s),
		Reason:    reason,
		Intent:    s.Intent,
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -count=1 -race ./internal/sched/ -run TestRenderAll`
Expected: PASS.

- [ ] **Step 5: Update the enumerations in all five files**

A tenth exported door is a tenth `q.mu` locker. Five non-test files state the count of nine in prose. Find them:

```bash
grep -rn 'nine' internal/sched/queue.go internal/sched/doc.go internal/sched/render.go internal/sched/pool.go internal/job/job.go
```

Update each to **ten**, adding `RenderAll` to every name list. Two need more than a number:

- `internal/sched/render.go` — its own doc comment embeds `` `grep -n '^func (q \*Queue) [A-Z]' internal/sched/*.go | grep -v _test.go` `` and says it "finds nine exported methods". Re-run that exact command and write the number it actually returns.
- `internal/job/job.go:135-142` — states a *derived* claim: "of those nine, six reach a `*job.Job` method call". `RenderAll` calls `Snapshot`, so this becomes **seven of ten**. `job.go:154` records that the locker test enforces names only, not this split — Step 6 fixes that.

- [ ] **Step 6: Convert the six-of-nine prose claim into a test**

Rule 4: where a population is enumerable by a machine, write the test instead of the sentence. Extend `internal/sched/lock_enumeration_test.go`, which already AST-parses the package's non-test sources.

```go
// TestQueueDoorsReachingJob_MatchTheEnumerationStatedInProse pins the
// seven-of-ten split that internal/job/job.go states in prose. That comment
// was explicitly "a reviewed property, not a machine-checked one", and adding
// RenderAll moved it from six-of-nine to seven-of-ten — a change no gate in
// this repository would have caught, since
// TestQueueMuLockers_MatchTheEnumerationStatedInProse asserts locker NAMES
// only.
func TestQueueDoorsReachingJob_MatchTheEnumerationStatedInProse(t *testing.T) {
	want := map[string]bool{
		"Cancel": true, "Park": true, "Retry": true, "Advance": true,
		"Settle": true, "Render": true, "RenderAll": true,
		"Pause": false, "Resume": false, "Paused": false,
	}
	got := doorsReachingJobMethod(t) // set of exported doors whose body reaches a *job.Job method
	for name, expected := range want {
		if got[name] != expected {
			t.Errorf("door %s reaches *job.Job = %v, want %v — update internal/job/job.go's prose split", name, got[name], expected)
		}
	}
	if len(got) != len(want) {
		t.Errorf("found %d exported doors, prose enumerates %d", len(got), len(want))
	}
}
```

Implement `doorsReachingJobMethod` beside the existing AST helper in that file, reusing its parser setup. It walks each exported `func (q *Queue) Name` body — including calls into unexported helpers in the same package — and reports whether any reaches a method on a `*job.Job` value.

- [ ] **Step 7: Verify the mutation — RenderAll must actually take the lock**

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/sched/render.go "$SCRATCH/render.bak.go"
# Remove the q.mu.Lock()/defer q.mu.Unlock() pair from RenderAll only.
go test -count=1 -race ./internal/sched/ -run TestRenderAll_TakesTheQueueLockOnce
# MUST fail with a DATA RACE on q.leases or q.slots.
cp "$SCRATCH/render.bak.go" internal/sched/render.go
go test -count=1 -race ./internal/sched/ -run TestRenderAll
```

Record the observed failure output in the commit body. A red-green claim without the message it produced is an assertion, not evidence.

- [ ] **Step 8: Run the gates and commit**

```bash
goimports -w internal/sched/render.go internal/sched/lock_enumeration_test.go
go build ./... && go vet ./...
go test -race -count=1 ./internal/sched/ ./internal/job/
golangci-lint run ./internal/sched/... ./internal/job/...
go run ./scripts/check_citations
git add internal/sched/ internal/job/job.go
git commit -m "feat(sched): add RenderAll, one lock per listing"
```

---

## Task 2: the registry — `Header`, `Row`, `Add`, `List`

**Files:**
- Create: `internal/dispatch/doc.go`, `internal/dispatch/registry.go`
- Test: `internal/dispatch/registry_test.go`

**Interfaces:**
- Consumes: `sched.Queue.RenderAll` (Task 1), `job.New`, `job.RenderView`.
- Produces:
  - `type Header struct { Name, Category string; Priority int; Bytes int64 }`
  - `type Row struct { ID string; Header Header; View job.RenderView }`
  - `func (d *Dispatcher) Add(j *job.Job, h Header) error`
  - `func (d *Dispatcher) List() []Row`
  - unexported `func (d *Dispatcher) snapshotOrder() []*job.Job`

- [ ] **Step 1: Write the failing tests**

`internal/dispatch/registry_test.go`:

```go
func TestAdd_RejectsADuplicateID(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{Name: "n"}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := d.Add(job.New("j1", "other", job.Policy{}), Header{Name: "other"}); err == nil {
		t.Fatal("second Add with the same ID returned nil, want an error — the registry is keyed by ID and a silent overwrite would strand the first job's resources")
	}
}

func TestList_PreservesInsertionOrder(t *testing.T) {
	d := newTestDispatcher(t)
	for _, id := range []string{"c", "a", "b"} {
		if err := d.Add(job.New(id, id, job.Policy{}), Header{Name: id}); err != nil {
			t.Fatalf("Add(%s): %v", id, err)
		}
	}
	got := d.List()
	want := []string{"c", "a", "b"}
	if len(got) != len(want) {
		t.Fatalf("List returned %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("row %d is %s, want %s — queue order is the priority policy and List must not reorder it", i, got[i].ID, want[i])
		}
	}
}

func TestList_CarriesTheHeaderAndTheView(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	h := Header{Name: "movie", Category: "tv", Priority: 2, Bytes: 4096}
	if err := d.Add(j, h); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got := d.List()
	if len(got) != 1 {
		t.Fatalf("List returned %d rows, want 1", len(got))
	}
	if got[0].Header != h {
		t.Errorf("Header = %+v, want %+v", got[0].Header, h)
	}
	if got[0].View != d.q.Render(j) {
		t.Errorf("View = %+v, want %+v", got[0].View, d.q.Render(j))
	}
}

func TestList_EmptyRegistryReturnsEmptyNonNil(t *testing.T) {
	d := newTestDispatcher(t)
	got := d.List()
	if got == nil {
		t.Fatal("List() = nil on an empty registry, want an empty non-nil slice")
	}
	if len(got) != 0 {
		t.Fatalf("List() has %d rows, want 0", len(got))
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -count=1 ./internal/dispatch/`
Expected: FAIL — the package does not exist.

- [ ] **Step 3: Write `doc.go`**

```go
// Package dispatch drives internal/sched. It owns the three things sched
// deliberately does not have (D-B5): the job registry in queue order, manifest
// residency, and the loop.
//
// The loop is the owner of liveness (D-B7). A ticker walks the registry and
// calls sched.Queue.Advance on each job; the kick is a latency optimisation
// that must remain deletable without changing what the system computes. The
// package this replaces made promotion event-driven, which put nine call sites
// in charge of one condition — `grep -c 'q\.PromoteNext(' internal/queue/queue.go`
// returns 9 — where forgetting one yields a job that is eligible, unblocked and
// never starts, silently.
//
// The dispatcher also launches every worker and observes every exit (D-B14).
// That is not bookkeeping: the Queue cannot distinguish "holding and working"
// from "holding and yielded", so without an exit path a cancelled job's worker
// is aborted, surrenders nothing, and finishCancel re-aborts it every tick
// forever while the job never settles.
//
// It imports internal/sched, internal/job and the standard library. It must not
// import internal/queue, internal/downloader or internal/postproc: the
// dependency points from B2 to B1, never the reverse.
//
// # What this package does not have yet
//
// Nothing imports it. B2.4 repoints production onto it and deletes
// internal/queue. Row carries a Header supplied at Add rather than byte and
// article progress, because internal/job.Job holds only id, name and policy —
// the progress tier is still in internal/queue until B2.4.
package dispatch
```

- [ ] **Step 4: Write `registry.go`**

```go
package dispatch

import (
	"fmt"

	"github.com/hobeone/gonzbd/internal/job"
)

// Header is the display metadata a listing needs that internal/job.Job does
// not carry. job.Job holds id, name and policy only; category, priority and
// the total byte figure live in internal/queue until B2.4 migrates them, so
// the caller supplies them at Add.
type Header struct {
	Name     string
	Category string
	Priority int
	Bytes    int64
}

// Row is one line of a queue listing: the scheduling view sched computes,
// beside the header the caller supplied.
type Row struct {
	ID     string
	Header Header
	View   job.RenderView
}

// entry is the registry's record for one job.
type entry struct {
	j *job.Job
	h Header
}

// Add registers a job in queue order and wakes the tick.
//
// A duplicate ID is an error rather than an overwrite: the registry is the only
// route by which a job's resources are returned, so replacing an entry would
// strand whatever the displaced job held, with nothing left to release it.
func (d *Dispatcher) Add(j *job.Job, h Header) error {
	d.mu.Lock()
	if _, dup := d.byID[j.ID()]; dup {
		d.mu.Unlock()
		return fmt.Errorf("dispatch: Add: job %q is already registered", j.ID())
	}
	d.byID[j.ID()] = &entry{j: j, h: h}
	d.order = append(d.order, j.ID())
	d.mu.Unlock()

	d.kick()
	return nil
}

// snapshotOrder copies the registry in queue order. The copy exists so the tick
// can release d.mu before calling into sched: D-B9 forbids holding d.mu across
// such a call, because Workers.Abort runs inside Queue.mu and an Abort that
// took d.mu would deadlock ABBA against a concurrent Cancel.
func (d *Dispatcher) snapshotOrder() []*job.Job {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*job.Job, 0, len(d.order))
	for _, id := range d.order {
		out = append(out, d.byID[id].j)
	}
	return out
}

// List composes the queue listing. It takes Queue.mu exactly once, through
// RenderAll, so every row is from one instant.
func (d *Dispatcher) List() []Row {
	d.mu.Lock()
	ids := make([]string, len(d.order))
	copy(ids, d.order)
	js := make([]*job.Job, 0, len(ids))
	hs := make([]Header, 0, len(ids))
	for _, id := range ids {
		e := d.byID[id]
		js = append(js, e.j)
		hs = append(hs, e.h)
	}
	d.mu.Unlock()

	views := d.q.RenderAll(js)
	out := make([]Row, 0, len(ids))
	for i, id := range ids {
		out = append(out, Row{ID: id, Header: hs[i], View: views[i]})
	}
	return out
}
```

- [ ] **Step 5: Add the minimal `Dispatcher` and `newTestDispatcher` so this compiles**

In `internal/dispatch/dispatch.go`, the fields Task 3 will build on:

```go
package dispatch

import (
	"sync"

	"github.com/hobeone/gonzbd/internal/sched"
)

// Dispatcher drives sched.Queue. It owns the Queue outright (D-B13): a caller
// reaching sched.Cancel directly would latch the cancel intent and skip the
// eviction Cancel below performs, reintroducing the defect where a deleted job
// renders as queued forever.
type Dispatcher struct {
	mu    sync.Mutex
	byID  map[string]*entry
	order []string

	// resident, launched and written are the dispatcher's own bookkeeping,
	// all guarded by mu. None may be held across a call into sched or into
	// Residency.Hydrate — take mu, read or write one map, release.
	resident map[string]bool
	launched map[string]bool
	written  map[string]Persisted

	q      *sched.Queue
	wake   chan struct{}
	log    *slog.Logger
}

// lookup returns the registered job for an ID.
func (d *Dispatcher) lookup(id string) (*job.Job, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.byID[id]
	if !ok {
		return nil, false
	}
	return e.j, true
}

// The three log helpers exist so the tick has exactly one shape for "this job
// failed, keep walking". A tick must never abandon the rest of the queue
// because one job errored — that would let a single bad job stall every other,
// which is the blast radius Standing Design Rule 3 bounds for articles and the
// same argument applies here.
func (d *Dispatcher) logAdvanceError(id string, err error) {
	d.log.Error("advance failed", "job_id", id, "err", err)
}

func (d *Dispatcher) logResidencyError(id string, err error) {
	d.log.Error("residency reconcile failed", "job_id", id, "err", err)
}

func (d *Dispatcher) logStoreError(id string, err error) {
	d.log.Error("store write failed", "job_id", id, "err", err)
}

// kick wakes the ticker without blocking. The channel is buffered to 1, so a
// burst of Adds collapses into one wakeup and a full buffer means a wakeup is
// already pending.
func (d *Dispatcher) kick() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}
```

And in `internal/dispatch/fakes_test.go`:

```go
package dispatch

import (
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/sched"
)

type stubWorkers struct{ aborted []string }

func (s *stubWorkers) Abort(jobID string) { s.aborted = append(s.aborted, jobID) }

func testClock() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// testOpts is how every test in this package varies the dispatcher it builds.
// One constructor with options, rather than a family of
// newTestDispatcherWithX helpers: later tasks need to vary four things
// independently, and the combinatorial helper set is the thing that rots.
type testOpts struct {
	leaseCap, slotCap int
	res               Residency
	store             Store
	runner            Runner
	workers           sched.Workers
}

func newTestDispatcher(t *testing.T, mods ...func(*testOpts)) *Dispatcher {
	t.Helper()
	o := testOpts{
		leaseCap: 2, slotCap: 2,
		res:     &fakeResidency{},
		store:   &fakeStore{},
		runner:  &fakeRunner{},
		workers: &stubWorkers{},
	}
	for _, m := range mods {
		m(&o)
	}
	return &Dispatcher{
		byID:      map[string]*entry{},
		resident:  map[string]bool{},
		launched:  map[string]bool{},
		written:   map[string]Persisted{},
		q:         sched.New(o.leaseCap, o.slotCap, testClock, o.workers),
		wake:      make(chan struct{}, 1),
		res:       o.res,
		store:     o.store,
		runner:    o.runner,
		tickEvery: time.Hour, // long, so only explicit d.tick calls advance anything
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		log:       slog.New(slog.DiscardHandler),
	}
}

func withCaps(lease, slot int) func(*testOpts) {
	return func(o *testOpts) { o.leaseCap, o.slotCap = lease, slot }
}
func withResidency(r Residency) func(*testOpts) { return func(o *testOpts) { o.res = r } }
func withStore(s Store) func(*testOpts)         { return func(o *testOpts) { o.store = s } }
func withRunner(r Runner) func(*testOpts)       { return func(o *testOpts) { o.runner = r } }
func withWorkers(w sched.Workers) func(*testOpts) { return func(o *testOpts) { o.workers = w } }
```

**Every later task calls this one constructor**, varying it with the `with*` options above. No task defines a constructor of its own.

**The tick interval is an hour on purpose.** Every test drives the loop by calling `d.tick(ctx)` directly, so no test depends on wall-clock timing. A test that waited for a real ticker would be the flaky kind this repository already has trouble with.

- [ ] **Step 6: Run to verify they pass**

Run: `go test -count=1 -race ./internal/dispatch/`
Expected: PASS.

- [ ] **Step 7: Verify the mutation — `List` must not reorder**

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/dispatch/registry.go "$SCRATCH/registry.bak.go"
# In List, sort ids before building the rows.
go test -count=1 ./internal/dispatch/ -run TestList_PreservesInsertionOrder
# MUST fail: row 0 is a, want c
cp "$SCRATCH/registry.bak.go" internal/dispatch/registry.go
go test -count=1 ./internal/dispatch/ -run TestList
```

- [ ] **Step 8: Gates and commit**

```bash
goimports -w internal/dispatch/
go build ./... && go vet ./...
go test -race -count=1 ./internal/dispatch/
golangci-lint run ./internal/dispatch/...
git add internal/dispatch/
git commit -m "feat(dispatch): add the registry, Header, Row and List"
```

---

## Task 3: the tick — `New`, `Start`, `Stop`, and the walk

**Files:**
- Modify: `internal/dispatch/dispatch.go`
- Create: `internal/dispatch/tick.go`, `internal/dispatch/ports.go`
- Test: `internal/dispatch/tick_test.go`

**Interfaces:**
- Consumes: `d.snapshotOrder()`, `d.kick()` (Task 2); `sched.Queue.Advance`.
- Produces:
  - `func New(leaseCap, slotCap int, tick time.Duration, clock func() time.Time, w sched.Workers, r Residency, s Store) *Dispatcher`
  - `func (d *Dispatcher) Start(ctx context.Context) error`
  - `func (d *Dispatcher) Stop() error`
  - unexported `func (d *Dispatcher) tick(ctx context.Context)`
  - `internal/dispatch/ports.go` declaring `Residency` and `Store` (the `Store` methods are stubbed in Task 7; declare the interface here so `New` has its parameter).

- [ ] **Step 1: Write the failing tests**

```go
func TestTick_PromotesWithoutAKick(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Drain the kick Add queued, so only the tick itself can promote.
	select {
	case <-d.wake:
	default:
	}

	d.tick(context.Background())

	if !j.HasRun() {
		t.Fatal("job never started — the ticker alone must be sufficient for liveness; if this needs a kick, the kick has become a second owner (D-B7)")
	}
}

func TestTick_WalksInQueueOrder(t *testing.T) {
	// One lease, two jobs: the first in queue order must win it.
	d := newTestDispatcher(t, withCaps(1, 1))
	first := job.New("first", "n", job.Policy{})
	second := job.New("second", "n", job.Policy{})
	for _, j := range []*job.Job{first, second} {
		if err := d.Add(j, Header{}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	d.tick(context.Background())
	d.tick(context.Background())

	if !d.q.Render(first).Running {
		t.Error("first is not running — the head of the queue must win the only lease")
	}
	if d.q.Render(second).Running {
		t.Error("second is running — it must wait behind first for the only lease")
	}
}

func TestStart_IsNotBlockingAndStopIsIdempotent(t *testing.T) {
	d := newTestDispatcher(t)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := d.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := d.Stop(); err != nil {
		t.Fatalf("second Stop returned %v, want nil — Stop must be idempotent", err)
	}
}

func TestStart_TwiceIsAnError(t *testing.T) {
	d := newTestDispatcher(t)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })
	if err := d.Start(context.Background()); err == nil {
		t.Fatal("second Start returned nil, want an error — a second ticker breaks D-B7's single-goroutine premise, and two concurrent walks would need locking that this design does not have")
	}
}

func TestStop_ParksHoldersRatherThanSettlingThem(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background()) // begin, then grant
	if !d.q.Render(j).Running {
		t.Fatal("setup: job is not running")
	}

	if err := d.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := d.q.Render(j).Outcome; got.IsSettled() {
		t.Errorf("Outcome = %v after Stop, want unsettled — a shutdown is not an outcome, and recording one would contradict what is on disk", got)
	}
	if j.HoldsLease() {
		t.Error("job still holds its lease after Stop — Stop must park every holder so the pools are returned")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -count=1 ./internal/dispatch/ -run 'TestTick|TestStart|TestStop'`
Expected: FAIL — `d.tick undefined`, `d.Start undefined`, `d.Stop undefined`.

- [ ] **Step 3: Write `ports.go`**

```go
package dispatch

import (
	"context"

	"github.com/hobeone/gonzbd/internal/job"
)

// Residency is how the dispatcher makes a job's manifest available and takes it
// away again. It names no manifest type on purpose: Manifest lives in
// internal/queue until B2.4, and the dispatcher never reads its contents. It
// decides WHEN a job should be resident (D-B8) and delegates WHAT to load.
//
// Hydrate may block on disk I/O. The dispatcher calls it with no lock held.
type Residency interface {
	Hydrate(ctx context.Context, id string) error
	Evict(id string)
}

// Store is the persistence the dispatcher needs, and no more (D-B11): read the
// whole queue once at Start, and write a job's four axes when they move.
// internal/dispatch defines it; B2.2 implements it against SQLite.
type Store interface {
	Load(ctx context.Context) ([]Persisted, error)
	Save(ctx context.Context, p Persisted) error
	Delete(ctx context.Context, id string) error
}

// Persisted is one job's durable state: identity, header, and the four axes.
// `crossed` is deliberately absent — it is derived from State via
// Attempt.crossed, and storing it would create a second source of truth that
// could disagree with State after a restore.
type Persisted struct {
	ID     string
	Header Header
	State  job.StateView
	Intent job.Intent
}
```

- [ ] **Step 4: Write `New`, `Start`, `Stop` in `dispatch.go`**

Add to the struct: `res Residency`, `store Store`, `tickEvery time.Duration`, `stop chan struct{}`, `done chan struct{}`, `started bool`.

```go
// New builds a Dispatcher and the sched.Queue it owns.
//
// It panics on a nil Residency, Store or Workers for the same reason sched.New
// panics on a nil Workers: these are construction-time programmer errors, not
// state an earlier build wrote, so Standing Design Rule 1's guard-removal
// argument does not apply. Failing here beats a nil dereference on the ticker
// goroutine with no construction frame left to explain it.
func New(leaseCap, slotCap int, tickEvery time.Duration, clock func() time.Time, w sched.Workers, r Residency, s Store) *Dispatcher {
	if r == nil {
		panic("dispatch: New: Residency must not be nil")
	}
	if s == nil {
		panic("dispatch: New: Store must not be nil")
	}
	if tickEvery <= 0 {
		panic("dispatch: New: tick interval must be positive")
	}
	return &Dispatcher{
		byID:      map[string]*entry{},
		q:         sched.New(leaseCap, slotCap, clock, w),
		wake:      make(chan struct{}, 1),
		res:       r,
		store:     s,
		tickEvery: tickEvery,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// Start registers everything the store holds, then launches the ticker and
// returns. It does not block.
//
// A second Start is an error rather than a no-op: it would create a second
// ticker goroutine, and two concurrent walks would need locking between them
// that D-B7's single-goroutine design deliberately does not have.
func (d *Dispatcher) Start(ctx context.Context) error {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return errors.New("dispatch: Start: already started")
	}
	d.started = true
	d.mu.Unlock()

	if err := d.restore(ctx); err != nil {
		return fmt.Errorf("dispatch: Start: %w", err)
	}

	go d.run(ctx)
	return nil
}

func (d *Dispatcher) run(ctx context.Context) {
	defer close(d.done)
	t := time.NewTicker(d.tickEvery)
	defer t.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			d.tick(ctx)
		case <-d.wake:
			d.tick(ctx)
		}
	}
}

// Stop halts the ticker, waits for the in-flight tick, and parks every job that
// still holds resources.
//
// It parks rather than settles. A shutdown is not an outcome: recording one
// would claim a verdict about work that simply stopped, and D-I11's reasoning
// applies — an Outcome must not contradict what is on disk. Park is safe for
// every shape because B2.1 made it unconditional and total.
func (d *Dispatcher) Stop() error {
	d.mu.Lock()
	if !d.started {
		d.mu.Unlock()
		return nil
	}
	d.started = false
	d.mu.Unlock()

	close(d.stop)
	<-d.done

	var firstErr error
	for _, j := range d.snapshotOrder() {
		if err := d.q.Park(j); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("dispatch: Stop: park %s: %w", j.ID(), err)
		}
		d.res.Evict(j.ID())
	}
	return firstErr
}
```

- [ ] **Step 5: Write `tick.go` — the walk**

Residency, worker launch and D-B12 eviction arrive in Tasks 4-6; this is the walk alone.

```go
package dispatch

import "context"

// tick is one pass over the registry. It is called only from run, so it never
// overlaps itself and needs no locking against another tick.
//
// It copies the registry under d.mu and releases it before the first Advance:
// D-B9 forbids holding d.mu across a call into sched, because Workers.Abort
// runs inside Queue.mu and an Abort implementation that took d.mu would
// deadlock ABBA against a concurrent Cancel.
func (d *Dispatcher) tick(ctx context.Context) {
	for _, j := range d.snapshotOrder() {
		if err := d.q.Advance(j); err != nil {
			d.logAdvanceError(j.ID(), err)
		}
	}
}
```

Add `logAdvanceError` as a method on `Dispatcher` taking a `*slog.Logger` field set in `New`. Use `slog`, matching the rest of the repository.

- [ ] **Step 6: Add a temporary `restore` stub so `Start` compiles**

Task 7 implements it. The stub must be honest — it reads the store and ignores nothing silently:

```go
// restore is implemented in Task 7. Until then it reads the store and reports
// any error, so Start's error path is real rather than dead code.
func (d *Dispatcher) restore(ctx context.Context) error {
	_, err := d.store.Load(ctx)
	return err
}
```

- [ ] **Step 7: Run to verify they pass**

Run: `go test -count=1 -race ./internal/dispatch/`
Expected: PASS.

- [ ] **Step 8: Verify the mutation — the ticker must not depend on the kick**

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/dispatch/registry.go "$SCRATCH/registry.bak.go"
# In Add, delete the d.kick() call.
go test -count=1 ./internal/dispatch/ -run TestTick_PromotesWithoutAKick
# MUST still PASS — that is the point: the ticker alone is sufficient.
cp "$SCRATCH/registry.bak.go" internal/dispatch/registry.go
```

Then the converse, which is the pin that matters:

```bash
cp internal/dispatch/tick.go "$SCRATCH/tick.bak.go"
# In tick, replace the loop body with a no-op.
go test -count=1 ./internal/dispatch/ -run TestTick_PromotesWithoutAKick
# MUST fail: "job never started"
cp "$SCRATCH/tick.bak.go" internal/dispatch/tick.go
```

- [ ] **Step 9: Gates and commit**

```bash
goimports -w internal/dispatch/
go build ./... && go vet ./... && go test -race -count=1 ./internal/dispatch/
golangci-lint run ./internal/dispatch/...
git add internal/dispatch/
git commit -m "feat(dispatch): add the ticker, Start/Stop, and the registry walk"
```

---

## Task 4: residency derived from pool membership (D-B8)

**Files:**
- Modify: `internal/dispatch/tick.go`
- Test: `internal/dispatch/residency_test.go`

**Interfaces:**
- Consumes: `Residency` (Task 3), `sched.Queue.Render`, `sched.Queue.Settle`.
- Produces: unexported `func (d *Dispatcher) reconcileResidency(ctx context.Context, j *job.Job) error`, called from `tick` after `Advance`.

- [ ] **Step 1: Write the failing tests**

```go
func TestResidency_HydratesWhenAJobAcquiresResources(t *testing.T) {
	res := &fakeResidency{}
	d := newTestDispatcher(t, withResidency(res))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	d.tick(context.Background())
	d.tick(context.Background())

	if !res.resident("j1") {
		t.Fatal("job holds resources but was never hydrated — residency is derived from pool membership (D-B8)")
	}
}

func TestResidency_EvictsWhenAJobReleasesResources(t *testing.T) {
	res := &fakeResidency{}
	d := newTestDispatcher(t, withResidency(res))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background())
	if !res.resident("j1") {
		t.Fatal("setup: job was never hydrated")
	}

	if err := d.q.Settle(j, job.OutcomeFailed); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	d.tick(context.Background())

	if res.resident("j1") {
		t.Error("job holds nothing but is still resident — a settled job's manifest must be evicted or a long queue accumulates them")
	}
}

func TestResidency_HydrationFailureSettlesFailedAndReturnsBothPools(t *testing.T) {
	res := &fakeResidency{failOn: map[string]error{"j1": errors.New("manifest unreadable")}}
	d := newTestDispatcher(t, withResidency(res))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	d.tick(context.Background())
	d.tick(context.Background())

	v := d.q.Render(j)
	if v.Outcome != job.OutcomeFailed {
		t.Errorf("Outcome = %v, want Failed — a job whose manifest cannot be read cannot run and must not hold resources forever", v.Outcome)
	}
	if j.HoldsLease() {
		t.Error("lease still held after a hydration failure — settling must return both pools")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -count=1 ./internal/dispatch/ -run TestResidency`
Expected: FAIL — `d.reconcileResidency undefined` and `fakeResidency` undefined.

- [ ] **Step 3: Add `fakeResidency` to `fakes_test.go`**

```go
type fakeResidency struct {
	mu     sync.Mutex
	live   map[string]bool
	failOn map[string]error
}

func (f *fakeResidency) Hydrate(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOn[id]; err != nil {
		return err
	}
	if f.live == nil {
		f.live = map[string]bool{}
	}
	f.live[id] = true
	return nil
}

func (f *fakeResidency) Evict(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, id)
}

func (f *fakeResidency) resident(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live[id]
}
```

- [ ] **Step 4: Implement `reconcileResidency` and call it from `tick`**

```go
// reconcileResidency brings a job's manifest residency in line with what it
// holds (D-B8: manifestResident(j) <=> q.holds(j)).
//
// The invariant is stated at TICK BOUNDARIES, not instantaneously. grantFor
// runs inside Advance under Queue.mu, so a job acquires resources before this
// function can hydrate it, and Hydrate does disk I/O that must not run under
// any lock. A job therefore holds a lease with no manifest for the length of
// one read. Nothing consumes that window: Task 5's launch path runs after this
// function, and a failed read settles the job here.
func (d *Dispatcher) reconcileResidency(ctx context.Context, j *job.Job) error {
	holds := d.q.Render(j).Running || d.holdsAnything(j)
	switch {
	case holds && !d.isResident(j.ID()):
		if err := d.res.Hydrate(ctx, j.ID()); err != nil {
			// The job cannot run without its manifest, and it is holding
			// resources it can never use. Settling returns both pools; leaving
			// it would strand them, because no later tick reaches a different
			// branch for it.
			if serr := d.q.Settle(j, job.OutcomeFailed); serr != nil {
				return fmt.Errorf("hydrate %s: %w (and settle failed: %v)", j.ID(), err, serr)
			}
			d.markNotResident(j.ID())
			return fmt.Errorf("hydrate %s: %w", j.ID(), err)
		}
		d.markResident(j.ID())
	case !holds && d.isResident(j.ID()):
		d.res.Evict(j.ID())
		d.markNotResident(j.ID())
	}
	return nil
}
```

Add `resident map[string]bool` to `Dispatcher`, guarded by `d.mu`, with `isResident`, `markResident` and `markNotResident` taking and releasing `d.mu` around a single map operation each — never held across the `Hydrate` call.

Implement `holdsAnything(j *job.Job) bool` as `j.HoldsLease() || d.q.Render(j).Running`. **Escalate before adding anything to `sched` here:** if a `holds`-shaped predicate is genuinely needed, that is a new exported door on `sched` and falsifies the ten-door enumeration again, which is a decision for the plan's author, not the implementer.

Call it from `tick`, after `Advance`:

```go
for _, j := range d.snapshotOrder() {
	if err := d.q.Advance(j); err != nil {
		d.logAdvanceError(j.ID(), err)
		continue
	}
	if err := d.reconcileResidency(ctx, j); err != nil {
		d.logResidencyError(j.ID(), err)
	}
}
```

- [ ] **Step 5: Run to verify they pass**

Run: `go test -count=1 -race ./internal/dispatch/`
Expected: PASS.

- [ ] **Step 6: Verify the mutation — the hydration failure path**

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/dispatch/tick.go "$SCRATCH/tick.bak.go"
# In reconcileResidency, replace the Settle call in the Hydrate error arm with a bare return.
go test -count=1 ./internal/dispatch/ -run TestResidency_HydrationFailureSettlesFailedAndReturnsBothPools
# MUST fail: Outcome = Pending, want Failed
cp "$SCRATCH/tick.bak.go" internal/dispatch/tick.go
```

- [ ] **Step 7: Gates and commit**

```bash
goimports -w internal/dispatch/
go build ./... && go vet ./... && go test -race -count=1 ./internal/dispatch/
golangci-lint run ./internal/dispatch/...
go run ./scripts/check_lock_io
git add internal/dispatch/
git commit -m "feat(dispatch): derive manifest residency from pool membership"
```

---

## Task 5: the worker lifecycle (D-B14)

**Files:**
- Create: `internal/dispatch/worker.go`
- Modify: `internal/dispatch/tick.go`, `internal/dispatch/ports.go`
- Test: `internal/dispatch/worker_test.go`

**Interfaces:**
- Consumes: `reconcileResidency` (Task 4), `sched.Queue.Settle`, `sched.Queue.Park`, `sched.Queue.Render`.
- Produces:
  - `type Runner interface { Run(ctx context.Context, id string, state job.State) }` in `ports.go` — how the dispatcher starts work. `Run` must not block the caller.
  - `func (d *Dispatcher) Finished(j *job.Job, o job.Outcome) error`
  - `func (d *Dispatcher) Yielded(j *job.Job) error`
  - unexported `func (d *Dispatcher) launch(ctx context.Context, j *job.Job)`

- [ ] **Step 1: Write the failing tests**

The first is the defect this task exists to prevent, and it is the most important test in the plan.

```go
func TestCancelledWorker_SettlesRatherThanReAbortingForever(t *testing.T) {
	w := &stubWorkers{}
	d := newTestDispatcher(t, withWorkers(w))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background())
	if !d.q.Render(j).Running {
		t.Fatal("setup: job is not running")
	}

	if err := d.Cancel(j.ID()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if len(w.aborted) != 1 {
		t.Fatalf("Abort called %d times, want 1", len(w.aborted))
	}

	// The worker notices the abort and exits without finishing.
	if err := d.Yielded(j); err != nil {
		t.Fatalf("Yielded: %v", err)
	}
	d.tick(context.Background())

	if got := d.q.Render(j).Outcome; got != job.OutcomeCancelled {
		t.Errorf("Outcome = %v, want Cancelled — without an exit path the job holds its lease, running() stays true, and finishCancel re-Aborts every tick forever", got)
	}
	if len(w.aborted) != 1 {
		t.Errorf("Abort called %d times in total, want 1 — a second call means the job never settled and the loop is live", len(w.aborted))
	}
}

func TestYielded_UnderPauseReturnsTheLease(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background())
	if !j.HoldsLease() {
		t.Fatal("setup: job holds no lease")
	}

	d.Pause()
	if err := d.Yielded(j); err != nil {
		t.Fatalf("Yielded: %v", err)
	}

	if j.HoldsLease() {
		t.Error("lease still held after a pause yield — Advance branch 2 returns early while holds() is true, so only the dispatcher's exit path can return it")
	}
}

func TestFinished_RefusesCancelledAsAnOutcome(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background())

	if err := d.Finished(j, job.OutcomeCancelled); err == nil {
		t.Fatal("Finished accepted OutcomeCancelled, want an error — only the cancel latch may produce it, and a worker reporting it would let any exit masquerade as a user deletion")
	}
}

func TestLaunch_SkippedWhenIntentTurnedToCancelDuringHydration(t *testing.T) {
	runner := &fakeRunner{}
	res := &fakeResidency{}
	d := newTestDispatcher(t, withResidency(res), withRunner(runner))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Cancel lands while the manifest read is in flight.
	res.onHydrate = func(string) { _ = d.Cancel(j.ID()) }

	d.tick(context.Background())
	d.tick(context.Background())

	if runner.started(j.ID()) {
		t.Error("worker launched for a job cancelled during hydration — the launch path must re-read the snapshot after the unlocked read")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -count=1 ./internal/dispatch/ -run 'TestCancelledWorker|TestYielded|TestFinished|TestLaunch'`
Expected: FAIL — `d.Finished`, `d.Yielded`, `d.Cancel`, `d.Pause` undefined.

- [ ] **Step 3: Add `Runner` to `ports.go` and `fakeRunner` to `fakes_test.go`**

```go
// Runner starts the work for one job at one state. It must return promptly —
// the dispatcher calls it from the tick goroutine, and a Runner that blocks
// stalls every other job's advance.
//
// The runner reports completion by calling Dispatcher.Finished, and any other
// exit by calling Dispatcher.Yielded. Not calling either strands the job's
// resources: the Queue cannot tell "holding and working" from "holding and
// yielded", so nothing else can return them.
type Runner interface {
	Run(ctx context.Context, id string, state job.State)
}
```

```go
type fakeRunner struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (f *fakeRunner) Run(_ context.Context, id string, _ job.State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	f.seen[id] = true
}

func (f *fakeRunner) started(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[id]
}
```

Add an `onHydrate func(string)` field to **`fakeResidency`** (not `fakeRunner`), invoked at the top of `Hydrate` before it records residency, so a test can interleave a `Cancel` with the unlocked read:

```go
func (f *fakeResidency) Hydrate(_ context.Context, id string) error {
	if f.onHydrate != nil {
		f.onHydrate(id) // called WITHOUT f.mu: the hook re-enters the Dispatcher
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// ... failOn check and f.live[id] = true, as in Task 4
}
```

The hook must run outside `f.mu`. It calls back into `Dispatcher.Cancel`, and holding a fake's lock across a re-entry is how a test deadlocks against itself.

- [ ] **Step 4: Write `worker.go`**

```go
package dispatch

import (
	"context"
	"fmt"

	"github.com/hobeone/gonzbd/internal/job"
)

// Finished records a worker's terminal completion.
//
// It rejects OutcomeCancelled before touching the Queue. sched.Settle refuses
// it too, but failing here names the caller: only the cancel latch may produce
// Cancelled, and a worker allowed to report it could make any exit look like a
// user deletion.
func (d *Dispatcher) Finished(j *job.Job, o job.Outcome) error {
	if o == job.OutcomeCancelled {
		return fmt.Errorf("dispatch: Finished(%s): OutcomeCancelled is reserved for the cancel latch", j.ID())
	}
	if err := d.q.Settle(j, o); err != nil {
		return fmt.Errorf("dispatch: Finished(%s): %w", j.ID(), err)
	}
	d.kick()
	return nil
}

// Yielded records a worker exiting without finishing its state's work: a pause
// yield at an article boundary, an Abort, a shutdown, a dead connection.
//
// It parks unconditionally. Park is total — slot release is a map delete,
// Surrender returns nil when nothing is held, and reclaim no-ops on nil — so
// the dispatcher never has to decide whether a yielding worker still holds
// something. That totality is what makes one door correct for every exit shape.
//
// This is the input the tick cannot compute. Advance branch 2 returns early
// while holds() is true, because the Queue cannot distinguish a working holder
// from a yielded one, and stripping a live worker is the worse failure. Only
// the dispatcher knows which it is.
func (d *Dispatcher) Yielded(j *job.Job) error {
	if err := d.q.Park(j); err != nil {
		return fmt.Errorf("dispatch: Yielded(%s): %w", j.ID(), err)
	}
	d.kick()
	return nil
}

// launch starts a worker if the job is runnable and still wanted.
//
// It re-reads the snapshot rather than trusting the one the tick took: between
// Advance granting resources and this call, the manifest read ran unlocked
// (D-B8) and a concurrent Cancel may have latched IntentCancel. Launching
// anyway is not a correctness failure — the next tick aborts it — but it starts
// work the user already cancelled and pays a further tick to stop it.
func (d *Dispatcher) launch(ctx context.Context, j *job.Job) {
	v := d.q.Render(j)
	if !v.Running || v.Intent != job.IntentRun {
		return
	}
	if d.claimLaunched(j.ID()) {
		d.runner.Run(ctx, j.ID(), v.State)
	}
}
```

**The field is `runner`, not `run`.** `run` is already the ticker goroutine's method name (`func (d *Dispatcher) run(ctx context.Context)`), and Go allows no type to have a field and a method with the same name — the package would not compile.

`claimLaunched(id) bool` sets a `launched map[string]bool` under `d.mu` and reports whether this call was the one that set it, so a job already being worked is not started twice by a later tick. `Finished` and `Yielded` clear it.

- [ ] **Step 5: Add the `Cancel`, `Retry`, `Pause`, `Resume`, `Paused` doors (D-B13)**

In `dispatch.go`. `Cancel` is the one with behaviour beyond delegation — Task 6 adds its eviction; here it delegates and kicks.

```go
// Cancel latches the intent through sched and wakes the tick.
//
// It exists on Dispatcher rather than leaving callers to reach sched.Cancel
// because D-B12's eviction has no other home: sched has no registry to remove a
// never-run job from. A second route to the latch would skip it.
func (d *Dispatcher) Cancel(id string) error {
	j, ok := d.lookup(id)
	if !ok {
		return fmt.Errorf("dispatch: Cancel: no job %q", id)
	}
	if err := d.q.Cancel(j); err != nil {
		return fmt.Errorf("dispatch: Cancel(%s): %w", id, err)
	}
	d.kick()
	return nil
}

func (d *Dispatcher) Pause()       { d.q.Pause(); d.kick() }
func (d *Dispatcher) Resume()      { d.q.Resume(); d.kick() }
func (d *Dispatcher) Paused() bool { return d.q.Paused() }
```

`Retry(id)` follows `Cancel`'s shape, delegating to `d.q.Retry`.

- [ ] **Step 6: Call `launch` from `tick`, after residency**

```go
if err := d.reconcileResidency(ctx, j); err != nil {
	d.logResidencyError(j.ID(), err)
	continue
}
d.launch(ctx, j)
```

- [ ] **Step 7: Run to verify they pass**

Run: `go test -count=1 -race ./internal/dispatch/`
Expected: PASS.

- [ ] **Step 8: Verify the mutation — the abort loop**

This is the plan's most important mutation. It reproduces the defect the spec's first draft shipped.

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/dispatch/worker.go "$SCRATCH/worker.bak.go"
# In Yielded, replace the d.q.Park(j) call with `_ = j` and return nil.
go test -count=1 ./internal/dispatch/ -run TestCancelledWorker_SettlesRatherThanReAbortingForever
# MUST fail with BOTH:
#   Outcome = Pending, want Cancelled
#   Abort called 2 times in total, want 1
cp "$SCRATCH/worker.bak.go" internal/dispatch/worker.go
go test -count=1 -race ./internal/dispatch/
```

Record both messages in the commit body.

- [ ] **Step 9: Gates and commit**

```bash
goimports -w internal/dispatch/
go build ./... && go vet ./... && go test -race -count=1 ./internal/dispatch/
golangci-lint run ./internal/dispatch/...
git add internal/dispatch/
git commit -m "feat(dispatch): own the worker lifecycle"
```

---

## Task 6: evict a cancelled never-run job (D-B12)

**Files:**
- Modify: `internal/dispatch/tick.go`, `internal/dispatch/registry.go`
- Test: `internal/dispatch/cancel_test.go`

**Interfaces:**
- Consumes: `Store.Delete` (Task 3's `ports.go`), the registry (Task 2).
- Produces: unexported `func (d *Dispatcher) evictCancelledNeverRun(ctx context.Context, j *job.Job) bool` and `func (d *Dispatcher) remove(id string)`.

- [ ] **Step 1: Write the failing tests**

```go
func TestCancelledNeverRunJob_IsRemovedFromTheListing(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := d.Cancel(j.ID()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	d.tick(context.Background())

	if got := d.List(); len(got) != 0 {
		t.Errorf("List has %d rows after cancelling a never-run job, want 0 — finishCancel cannot settle one (Outcome lives on the Attempt and there is none), so without eviction it renders as StatusQueued forever", len(got))
	}
}

func TestCancelledNeverRunJob_IsDeletedFromTheStore(t *testing.T) {
	st := &fakeStore{}
	d := newTestDispatcher(t, withStore(st))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := d.Cancel(j.ID()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	d.tick(context.Background())

	if !st.deleted("j1") {
		t.Error("job removed from the registry but not the store — it would come back at the next Start")
	}
}

func TestCancelledRunningJob_IsNotEvicted(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background())
	if !d.q.Render(j).Running {
		t.Fatal("setup: job is not running")
	}

	if err := d.Cancel(j.ID()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	d.tick(context.Background())

	if got := d.List(); len(got) != 1 {
		t.Errorf("List has %d rows, want 1 — a job that HAS run must settle as Cancelled and stay visible; only the never-run case is evicted", len(got))
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -count=1 ./internal/dispatch/ -run TestCancelled`
Expected: FAIL — `TestCancelledNeverRunJob_IsRemovedFromTheListing` finds 1 row, want 0.

- [ ] **Step 3: Add `fakeStore` to `fakes_test.go`**

```go
type fakeStore struct {
	mu   sync.Mutex
	rows map[string]Persisted
	gone map[string]bool
}

func (f *fakeStore) Load(context.Context) ([]Persisted, error) { return nil, nil }

func (f *fakeStore) Save(_ context.Context, p Persisted) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows == nil {
		f.rows = map[string]Persisted{}
	}
	f.rows[p.ID] = p
	return nil
}

func (f *fakeStore) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone == nil {
		f.gone = map[string]bool{}
	}
	f.gone[id] = true
	delete(f.rows, id)
	return nil
}

func (f *fakeStore) deleted(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gone[id]
}
```

- [ ] **Step 4: Implement the eviction and call it from `tick`**

```go
// evictCancelledNeverRunLocked removes a job the user deleted before it ever
// started, and reports whether it did.
//
// finishCancel returns nil for such a job because Outcome lives on the Attempt
// and there is none, so Finish would return ErrNoOpenAttempt. The job therefore
// survives; gatedBy ignores IntentCancel, waitReason returns NoLease,
// NoLease.IsPause() is false, and job.ToSABnzbd renders StatusQueued. A job the
// user deleted looks queued, forever.
//
// It runs after Advance, which routes IntentCancel to finishCancel before every
// other branch, so this cannot race a settle. It frees no pools:
// TestRequirements_StateUnsetRequiresNothing pins that StateUnset requires
// neither a lease nor a slot.
func (d *Dispatcher) evictCancelledNeverRun(ctx context.Context, j *job.Job) bool {
	v := d.q.Render(j)
	if v.State != job.StateUnset || v.Intent != job.IntentCancel {
		return false
	}
	if err := d.store.Delete(ctx, j.ID()); err != nil {
		// Leave it registered: dropping it here and failing to delete it would
		// resurrect it at the next Start, which is worse than one more tick.
		d.logStoreError(j.ID(), err)
		return false
	}
	d.remove(j.ID())
	return true
}
```

`remove(id)` deletes from `d.byID` and `d.order` under `d.mu`, in `registry.go`.

In `tick`, before `reconcileResidency`:

```go
if d.evictCancelledNeverRun(ctx, j) {
	continue
}
```

- [ ] **Step 5: Run to verify they pass**

Run: `go test -count=1 -race ./internal/dispatch/`
Expected: PASS.

- [ ] **Step 6: Verify the mutation**

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/dispatch/tick.go "$SCRATCH/tick.bak.go"
# In tick, delete the evictCancelledNeverRun call.
go test -count=1 ./internal/dispatch/ -run TestCancelledNeverRunJob
# MUST fail: List has 1 rows after cancelling a never-run job, want 0
cp "$SCRATCH/tick.bak.go" internal/dispatch/tick.go
```

- [ ] **Step 7: Gates and commit**

```bash
goimports -w internal/dispatch/
go build ./... && go vet ./... && go test -race -count=1 ./internal/dispatch/
golangci-lint run ./internal/dispatch/...
git add internal/dispatch/
git commit -m "feat(dispatch): evict a cancelled never-run job"
```

---

## Task 7: the store — `restore` at `Start`, and persistence on change

**Files:**
- Modify: `internal/dispatch/dispatch.go`, `internal/dispatch/tick.go`
- Test: `internal/dispatch/store_test.go`

**Interfaces:**
- Consumes: `Store`, `Persisted` (Task 3).
- Produces: `func (d *Dispatcher) restore(ctx context.Context) error` (replacing Task 3's stub), and `persistIfChanged` called from `tick`.

- [ ] **Step 1: Write the failing tests**

```go
func TestRestore_RegistersEveryStoredJobInOrder(t *testing.T) {
	st := &fakeStore{}
	st.seed([]Persisted{
		{ID: "c", Header: Header{Name: "c"}},
		{ID: "a", Header: Header{Name: "a"}},
	})
	d := newTestDispatcher(t, withStore(st))

	if err := d.restore(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}

	got := d.List()
	want := []string{"c", "a"}
	if len(got) != len(want) {
		t.Fatalf("List has %d rows after restore, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("row %d is %s, want %s — restore must preserve stored queue order", i, got[i].ID, want[i])
		}
	}
}

func TestRestore_JobsComeBackHoldingNothing(t *testing.T) {
	st := &fakeStore{}
	st.seed([]Persisted{{
		ID:     "j1",
		Header: Header{Name: "j1"},
		State:  job.StateView{State: job.Repairing},
	}})
	d := newTestDispatcher(t, withStore(st))

	if err := d.restore(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}

	j, ok := d.lookup("j1")
	if !ok {
		t.Fatal("job was not registered")
	}
	if j.HoldsLease() {
		t.Error("restored job holds a lease — the pools are process-local and there is nothing to reclaim, so the first tick must re-acquire through grantFor")
	}
}

func TestRestore_StoreErrorFailsStart(t *testing.T) {
	st := &fakeStore{loadErr: errors.New("database is locked")}
	d := newTestDispatcher(t, withStore(st))

	if err := d.Start(context.Background()); err == nil {
		t.Fatal("Start returned nil after a store failure, want an error — starting with a partial queue would silently drop jobs")
	}
}

func TestPersist_WritesWhenTheAxesMove(t *testing.T) {
	st := &fakeStore{}
	d := newTestDispatcher(t, withStore(st))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	d.tick(context.Background()) // BeginAttempt moves State off StateUnset

	if got, ok := st.row("j1"); !ok || got.State.State == job.StateUnset {
		t.Errorf("stored State = %+v (present=%v), want a started state — a restart would otherwise re-run the job from the beginning", got.State, ok)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -count=1 ./internal/dispatch/ -run 'TestRestore|TestPersist'`
Expected: FAIL — `st.seed`, `st.row`, `d.lookup` undefined; `restore` registers nothing.

- [ ] **Step 3: Extend `fakeStore` with `seed`, `row` and `loadErr`**

```go
func (f *fakeStore) seed(ps []Persisted) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = nil
	if f.rows == nil {
		f.rows = map[string]Persisted{}
	}
	for _, p := range ps {
		f.rows[p.ID] = p
		f.order = append(f.order, p.ID)
	}
}

func (f *fakeStore) row(id string) (Persisted, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.rows[id]
	return p, ok
}
```

Change `Load` to return the seeded rows in `f.order`, or `f.loadErr` when set.

- [ ] **Step 4: Implement `restore`**

```go
// restore registers everything the store holds, before the first tick.
//
// Every job comes back holding nothing, whatever state it was persisted at.
// The pools are process-local: there is no lease or slot from a previous
// process to reclaim, so a job persisted mid-Repairing is simply a job at
// Repairing that holds nothing, and branch 2 of Advance grants it resources on
// the first tick. That is the same path a paused job resumes through — §5.4's
// restart scenarios and §5.12's restored post-boundary job both rely on it.
//
// Standing Design Rule 1 applies directly: rows an earlier build wrote may be
// assumed to satisfy the invariants this design introduces, so there is no
// migration, no dual-read, and no "old jobs behave differently" branch.
func (d *Dispatcher) restore(ctx context.Context) error {
	rows, err := d.store.Load(ctx)
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}
	for _, p := range rows {
		j := job.Restore(p.ID, p.Header.Name, job.Policy{}, p.State, p.Intent)
		if err := d.Add(j, p.Header); err != nil {
			return fmt.Errorf("register %s: %w", p.ID, err)
		}
	}
	return nil
}
```

**`job.Restore` does not exist yet.** `internal/job` exports `New(id, name string, p Policy) *Job` only, which always starts a job with no attempts. Rebuilding a job at a persisted state needs a constructor — and adding a **second constructor** for `job.Job` is an owner-model decision AGENTS.md requires escalating (Standing Design Rule 2 names "two constructors for one type" as its first smell, with `newManifest`/`UnmarshalJSON` as the worked example that had already diverged).

**Stop and escalate before writing it.** Present the options: a `Restore` constructor in `internal/job` that shares one initialisation path with `New`; a replay approach that calls `BeginAttempt`/`Transition` forward to the persisted state; or deferring restore-of-state to B2.2 and having B2.3 restore identity and header only. Do not choose unilaterally.

- [ ] **Step 5: Implement `persistIfChanged` and call it from `tick`**

```go
// persistIfChanged writes a job's four axes when they have moved since the last
// write. The comparison is against the last value written, held in memory, so a
// quiet tick over a large queue costs no store traffic.
func (d *Dispatcher) persistIfChanged(ctx context.Context, j *job.Job, h Header) {
	v := d.q.Render(j)
	p := Persisted{ID: j.ID(), Header: h, State: v.StateView, Intent: v.Intent}
	if last, ok := d.lastWritten(j.ID()); ok && last == p {
		return
	}
	if err := d.store.Save(ctx, p); err != nil {
		d.logStoreError(j.ID(), err)
		return
	}
	d.markWritten(p)
}
```

Call it at the end of each job's turn in `tick`, after `launch`.

- [ ] **Step 6: Run to verify they pass**

Run: `go test -count=1 -race ./internal/dispatch/`
Expected: PASS.

- [ ] **Step 7: Verify the mutation**

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/dispatch/dispatch.go "$SCRATCH/dispatch.bak.go"
# In Start, discard restore's error: `_ = d.restore(ctx)`.
go test -count=1 ./internal/dispatch/ -run TestRestore_StoreErrorFailsStart
# MUST fail: Start returned nil after a store failure, want an error
cp "$SCRATCH/dispatch.bak.go" internal/dispatch/dispatch.go
```

- [ ] **Step 8: Whole-repo gates and final commit**

```bash
goimports -w internal/dispatch/
go build ./... && go vet ./...
go test -race -count=1 ./...
golangci-lint run ./...
go run ./scripts/check_dup_comments
go run ./scripts/check_review_banner
go run ./scripts/check_citations
git add internal/dispatch/
git commit -m "feat(dispatch): restore from the store at Start and persist on change"
```

- [ ] **Step 9: Sweep the claims this branch falsified**

Run from the repository root, not over the files you touched:

```bash
git grep -n 'nine'
git grep -n 'imported by nothing'
git grep -n 'Half B2'
git grep -n 'the dispatcher'
```

Then **read `docs/ARCHITECTURE.md` and `docs/queue-lifecycle.md` in full.** Grep is blind to paraphrase: a doc restates a claim in prose and shares no token with the code. `internal/sched/doc.go` in particular says "Nothing imports this package yet, by design" and lists two things "Half B2 still owes this package" — both are now false and neither contains the word `dispatch`.

---

## Notes for the executor

**Two escalations are built into this plan.** Neither is a blocker to be worked around:

1. **Task 4, Step 4** — if residency needs a `holds`-shaped predicate from `sched`, that is a new exported door and re-falsifies the ten-door enumeration.
2. **Task 7, Step 4** — `job.Restore` is a second constructor for `job.Job`, which AGENTS.md requires escalating before writing.

**The most valuable test in this plan is `TestCancelledWorker_SettlesRatherThanReAbortingForever`** (Task 5). It pins a defect the design's own first draft shipped: the claim that the tick discharged cancel's settle "for free". It does not. Without `Yielded`, an aborted worker surrenders nothing, `running()` stays true, and `finishCancel` re-aborts every tick forever while the job never settles and holds pool-A capacity for the life of the process. If that test is ever weakened, the defect returns silently.
