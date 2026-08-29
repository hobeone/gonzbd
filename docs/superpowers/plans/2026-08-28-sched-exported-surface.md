# Half B2.1 — `internal/sched`'s Exported Surface Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `internal/sched` the four doors a Half B2 dispatcher needs — `Settle`, `Park`, `Pause`/`Resume`/`Paused`, and `Render` — so that a caller outside the package can return a lease, park a yielded worker, drive global pause, and build a `job.RenderView` without tearing.

**Architecture:** Every new door is a thin lock-taking wrapper over logic that already exists unexported. The one genuinely new piece of logic is `cancelInterrupts`, which names the interrupt-versus-gate split that `finishCancel` already tests inline, so that the settle path and the cancel path read one predicate instead of two copies of it. Nothing outside `internal/sched` changes; the package is still imported by nothing when this lands.

**Tech Stack:** Go 1.27.0, stdlib only. No new dependencies.

**Spec:** [`docs/superpowers/specs/2026-08-28-sched-exported-surface-design.md`](../specs/2026-08-28-sched-exported-surface-design.md) (committed copy of [issue #450](https://github.com/hobeone/gonzbd/issues/450)), which amends [`docs/superpowers/specs/2026-08-26-lifecycle-intents-design.md`](../specs/2026-08-26-lifecycle-intents-design.md). Read both. Unqualified §/D-I references are to the 2026-08-26 parent spec.

## Global Constraints

- **Module:** `github.com/hobeone/gonzbd`. Go **1.27.0**.
- **`internal/sched` depends on `internal/job` and nothing else.** No store, no registry, no residency, no new imports beyond the stdlib (D-B5).
- **Lock order is `Queue.mu` before `Job.mu`** (prior spec §7.1). Every exported door takes `q.mu` first, then calls into `*job.Job`. Never the reverse.
- **`q.reclaim` is the sole pool-A reclaimer; `q.releaseFor` is the sole pool-B releaser** (§3.9, D-I13). No new code may call `q.leases.reclaim` or `q.slots.release` directly.
- **`grantFor` is the sole acquirer** for both pools. This plan adds no acquisition.
- **Standing Design Rule 2 — state has one owner.** Do not add a second writer of any derived field, or a second enforcement point for one invariant. `cancelInterrupts` exists specifically to avoid one.
- **Standing Design Rule 4 — enumerate before asserting.** Any comment saying *only*, *sole*, *never*, *always*, or *the one place* must state the command you ran and its result. `go run ./scripts/check_citations` executes backticked `grep`/`git grep` claims that state a count.
- **Nothing imports `internal/sched`.** Do not wire anything up. Do not touch `internal/queue`, `internal/api`, `internal/app`, or `internal/downloader`.
- **After editing any `.go` file:** `goimports -w <file>`, then `go build ./...`.
- **Per-commit gates:** `go vet ./...`, `go test -race -count=1 ./...`, `golangci-lint run ./...`, and the whole-repo gates `go run ./scripts/check_dup_comments`, `go run ./scripts/check_review_banner`, `go run ./scripts/check_citations`.
- **`-count=1` is mandatory on every red check.** A cached `ok` is not an observation.
- **Never `git stash`, never `git checkout -- <path>`.** Restore from your own `cp` backup.
- **Conventional Commits**, scope `sched`. Footer `Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>`.

---

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `internal/sched/settle.go` | **create** | `cancelInterrupts`, `settleLocked`, `Settle`, `errCancelReserved`. The settlement path and the predicate that decides how a cancelled job ends. |
| `internal/sched/settle_test.go` | **create** | Tests for all of the above. |
| `internal/sched/cancel.go` | modify | `finishCancel`'s tail is replaced by a `settleLocked` call; its gate arm reads `cancelInterrupts`. `Cancel` is unchanged. |
| `internal/sched/advance.go` | modify | `park` → `Park`, doc comment rewritten. `parkLocked` unchanged. |
| `internal/sched/render.go` | **create** | `Render` — the single read door. |
| `internal/sched/render_test.go` | **create** | Tests for `Render`. |
| `internal/sched/queue.go` | modify | `Pause`, `Resume`, `Paused`. Door-enumeration comments updated. |
| `internal/sched/doc.go` | modify | The three discharged B2 obligations removed; what remains stated. |
| `internal/sched/scenario_test.go` | modify | `renderStatus` helper reimplemented over `q.Render`; scenario 5.13 re-pointed at `Settle`. |
| `internal/sched/advance_test.go`, `queue_test.go`, `cancel_test.go` | modify | `q.park(` → `q.Park(` at call sites. |

---

## Task 1: `cancelInterrupts` and `settleLocked`

Pure refactor. **No behaviour changes, so there is no red check in this task** — the existing suite must stay green from start to finish, and that is the deliverable's proof.

**Files:**
- Create: `internal/sched/settle.go`
- Modify: `internal/sched/cancel.go` (replace `finishCancel`'s body from its `q.running` arm to the end)

**Interfaces:**
- Consumes: `q.now()`, `q.releaseFor(j, job.StateUnset)`, `q.reclaim(l)`, `q.running(id, s)`, `job.IsProduction(s)`, `job.Snapshot`.
- Produces, for Tasks 2 and 6:
  - `func cancelInterrupts(s job.State) bool`
  - `func (q *Queue) settleLocked(j *job.Job, o job.Outcome, s job.Snapshot) error` — caller must already hold `q.mu` and must pass the snapshot it already read.

- [ ] **Step 1: Create `internal/sched/settle.go`**

```go
package sched

import (
	"github.com/hobeone/gonzbd/internal/job"
)

// cancelInterrupts reports whether Cancel stops this job's work rather than
// gating it. Prior spec §8.4 makes cancel an interrupt before the irreversible
// boundary and a gate after it, and this is that split, named once.
//
// It exists because two places need it and they must not disagree:
// finishCancel picks its arm on it, and settleLocked decides on it whether a
// cancelled job's outcome is OUR artifact or the worker's own. Written inline
// at both sites it would be one invariant in two files with nothing linking
// them — correct today only because the two copies happen to agree, and
// silently divergent the moment either moves. `grep -n 'cancelInterrupts('
// internal/sched/*.go | grep -v _test.go` finds three lines: this definition
// and its two readers.
//
// The predicate is the complement of IsProduction rather than a list of
// states, because that is what §8.4's boundary is. Note IsProduction exists to
// answer a DIFFERENT question — whether a job may return to Fetching (§4.1's
// one-way boundary) — and cancel borrows it. The two happen to want the same
// line today; if §8.4's line ever moves (making Extracting interruptible, say),
// this function is the one place that changes.
func cancelInterrupts(s job.State) bool { return !job.IsProduction(s) }

// settleLocked is the SOLE settle path: it is the only code in this package
// that calls j.Finish. `grep -n 'j\.Finish(\|\.Finish(' internal/sched/*.go |
// grep -v _test.go` finds one line, the call below. The caller must already
// hold q.mu, and must pass the snapshot it already read rather than letting
// this take a second one — two reads would reintroduce the tear job.Snapshot
// closes, at the one site where the intent may have just changed underneath.
//
// The order of the four steps is not arbitrary and each was fixed once during
// Half B1 review:
//
//  1. Apply the cancel latch, BEFORE Finish, because Finish writes the outcome
//     and there is no second chance to correct it.
//  2. Finish, and return on its error WITHOUT releasing anything — a refused
//     Finish means the attempt was not settled, so the job still occupies its
//     position and needs what that position requires. Releasing here would
//     strand it resourceless while still running.
//  3. Release the compute slot BEFORE the reclaim, because reclaim can fail
//     its identity audit and an earlier order returned through that failure
//     with the slot still held — turning one audit error into a permanent
//     pool-B leak.
//  4. Reclaim the lease.
func (q *Queue) settleLocked(j *job.Job, o job.Outcome, s job.Snapshot) error {
	// A cancelled job that was INTERRUPTED settles Cancelled whatever its
	// worker reported: we aborted it, so the worker's error is our own
	// artifact and recording it would be false. A cancelled job that was
	// GATED settles what actually happened — D-I11's running-Finalizing job
	// completes as OutcomeOK, because the files moved and the script ran.
	// Intent survives on the settled job either way, for the UI to read.
	if s.Intent == job.IntentCancel && cancelInterrupts(s.State.State) {
		o = job.OutcomeCancelled
	}
	l, err := j.Finish(o, q.now())
	if err != nil {
		return err
	}
	q.releaseFor(j, job.StateUnset)
	return q.reclaim(l)
}
```

- [ ] **Step 2: `goimports` and build**

```bash
goimports -w internal/sched/settle.go && go build ./...
```

- [ ] **Step 3: Rewire `finishCancel` in `internal/sched/cancel.go`**

Replace everything from the `if q.running(...)` line to the end of the function with:

```go
	if q.running(j.ID(), s) {
		// A worker owns this job's resources and is using them. Neither arm
		// may seize a lease or slot out from under it.
		if !cancelInterrupts(s.State.State) {
			return nil // gate: let it reach the end; D-I11 lets it complete OK
		}
		q.work.Abort(j.ID()) // interrupt: settled on the tick after it yields
		return nil
	}
	// The outcome passed here is what settleLocked will produce anyway, since
	// the latch is set and this arm is only reached pre-boundary. Passing it
	// explicitly keeps settleLocked's signature honest — the outcome is the
	// caller's to state — rather than having one caller rely on the override.
	return q.settleLocked(j, job.OutcomeCancelled, s)
```

Leave `Cancel`, and `finishCancel`'s `StateUnset` and settled arms, exactly as they are.

- [ ] **Step 4: Drop the now-unused `errors` import from `cancel.go`**

`errors.Join` was the only use. `goimports -w internal/sched/cancel.go` removes it.

- [ ] **Step 5: Confirm no test asserted on the joined error**

```bash
grep -n 'errors.Join' internal/sched/*_test.go
```

Expected: no output. Every existing assertion uses `errors.Is`, which is unaffected by dropping the join (the join wrapped a `nil` and a real error, so `errors.Is` already matched the real one).

- [ ] **Step 6: Full package suite must be GREEN, unchanged**

```bash
go test -race -count=1 ./internal/sched/
```

Expected: PASS. Every existing test, including `TestFinishCancel_PropagatesAForeignLeaseReclaimError` (`cancel_test.go:367`) and `TestBothPoolsAreAccountedAtEveryExit`, passes without modification. **If any test needs editing, stop** — this task changed behaviour and was not supposed to.

- [ ] **Step 7: Gates and commit**

```bash
go vet ./... && golangci-lint run ./... && go run ./scripts/check_citations
git add internal/sched/settle.go internal/sched/cancel.go
git commit -m "refactor(sched): name the cancel interrupt/gate split, give settle one owner

finishCancel tested job.IsProduction inline to pick its arm, and the outcome
override Settle needs is the complement of the same test. Written at both
sites that is one invariant in two files with nothing linking them, correct
only while the copies agree. cancelInterrupts names it once.

settleLocked becomes the sole caller of j.Finish in this package, carrying the
four-step ordering whose steps were each fixed separately during B1 review.

No behaviour change: the full internal/sched suite passes unmodified.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

## Task 2: `Settle`

**Files:**
- Modify: `internal/sched/settle.go` (append)
- Create: `internal/sched/settle_test.go`
- Modify: `internal/sched/scenario_test.go` (re-point scenario 5.13)

**Interfaces:**
- Consumes: `cancelInterrupts`, `settleLocked` (Task 1); `stubWorkers`, `testClock()` from `queue_test.go`.
- Produces, for Task 6: `func (q *Queue) Settle(j *job.Job, o job.Outcome) error`, `var errCancelReserved`.

- [ ] **Step 1: Write the failing tests in `internal/sched/settle_test.go`**

```go
package sched

import (
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// newSettleQueue builds a Queue with room for one of each resource and a job
// already open at Fetching holding a lease — the shape a worker settles from.
func newSettleQueue(t *testing.T) (*Queue, *job.Job) {
	t.Helper()
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	if err := q.Advance(j); err != nil { // branch 1: BeginAttempt
		t.Fatalf("Advance (begin): %v", err)
	}
	if err := q.Advance(j); err != nil { // branch 2: grant the Fetching lease
		t.Fatalf("Advance (grant): %v", err)
	}
	if !j.Snapshot().HoldsLease {
		t.Fatalf("setup: want a lease at Fetching, snapshot=%+v", j.Snapshot())
	}
	return q, j
}

// TestSettle_ReturnsBothPools pins the door's whole reason for existing: three
// exported job doors yield a *job.Lease and before this one, nothing exported
// could take it back.
func TestSettle_ReturnsBothPools(t *testing.T) {
	q, j := newSettleQueue(t)
	if err := q.Settle(j, job.OutcomeFailed); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := q.leases.outstanding(); got != 0 {
		t.Errorf("leases outstanding = %d, want 0 — a settled job needs none", got)
	}
	if got := q.slots.outstanding(); got != 0 {
		t.Errorf("slots outstanding = %d, want 0", got)
	}
	if got := j.Snapshot().State.Outcome; got != job.OutcomeFailed {
		t.Errorf("Outcome = %v, want Failed", got)
	}
}

// TestSettle_RefusesCancelledOutcome pins that only the latch authorises
// Cancelled. A caller minting it directly would settle a job that renders as
// Deleted while still carrying IntentRun, so q.Retry would reopen it — the
// resurrection D-I14's note says must have no path.
func TestSettle_RefusesCancelledOutcome(t *testing.T) {
	q, j := newSettleQueue(t)
	if err := q.Settle(j, job.OutcomeCancelled); !errors.Is(err, errCancelReserved) {
		t.Fatalf("Settle(Cancelled) = %v, want errCancelReserved", err)
	}
	if j.Snapshot().State.Outcome.IsSettled() {
		t.Error("refused Settle must not have settled the attempt")
	}
	if got := q.leases.outstanding(); got != 1 {
		t.Errorf("leases outstanding = %d, want 1 — a refused Settle releases nothing", got)
	}
}

// TestSettle_InterruptedCancelOverridesTheOutcome pins the case the door was
// designed around: Cancel on a running pre-boundary job calls Abort and
// returns without settling, so the worker comes back with an I/O error and the
// dispatcher reports Failed. Recording Failed would be false — we caused it.
func TestSettle_InterruptedCancelOverridesTheOutcome(t *testing.T) {
	q, j := newSettleQueue(t)
	if err := j.SetIntent(job.IntentCancel); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if err := q.Settle(j, job.OutcomeFailed); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := j.Snapshot().State.Outcome; got != job.OutcomeCancelled {
		t.Errorf("Outcome = %v, want Cancelled — Fetching is pre-boundary, so the "+
			"cancel interrupted this worker and its error is our artifact", got)
	}
}

// TestSettle_GatedCancelPreservesTheOutcome pins D-I11 from the other side. A
// running Finalizing job that is cancelled is GATED, not interrupted: it moves
// the files and runs the user script, then settles OK. Overriding to Cancelled
// would make the record contradict the disk.
func TestSettle_GatedCancelPreservesTheOutcome(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	driveToFinalizing(t, q, j)
	if err := j.SetIntent(job.IntentCancel); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if err := q.Settle(j, job.OutcomeOK); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := j.Snapshot().State.Outcome; got != job.OutcomeOK {
		t.Errorf("Outcome = %v, want OK — D-I11: the files moved and the script ran, "+
			"so recording Cancelled here would be false", got)
	}
	if got := j.Intent(); got != job.IntentCancel {
		t.Errorf("Intent = %v, want IntentCancel — it survives to tell a consumer "+
			"the request came too late", got)
	}
}

// TestSettle_RefusedFinishReleasesNothing pins step 2 of settleLocked's
// ordering. A job with no open attempt cannot be settled, and the failure must
// not take resources from the position it still occupies.
func TestSettle_RefusedFinishReleasesNothing(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("never-run", "n", job.Policy{})
	if err := q.Settle(j, job.OutcomeFailed); err == nil {
		t.Fatal("Settle on a job that never ran = nil, want an error from Finish")
	}
}
```

Add this helper to `settle_test.go` as well — it drives a job to a running `Finalizing`, which several tasks need:

```go
// driveToFinalizing walks a job along the work spine to a RUNNING Finalizing:
// Fetching → Assessing → cross to Extracting → Finalizing. It asserts at the
// end rather than trusting the walk, because a silent early exit would make
// every test built on it vacuous.
func driveToFinalizing(t *testing.T, q *Queue, j *job.Job) {
	t.Helper()
	if err := q.Advance(j); err != nil { // BeginAttempt
		t.Fatalf("Advance (begin): %v", err)
	}
	if err := q.Advance(j); err != nil { // grant the Fetching lease
		t.Fatalf("Advance (grant): %v", err)
	}
	if err := j.SetNext(job.Assessing); err != nil {
		t.Fatalf("SetNext(Assessing): %v", err)
	}
	if err := q.Advance(j); err != nil { // move to Assessing, take a slot
		t.Fatalf("Advance (to Assessing): %v", err)
	}
	if err := j.SetNext(job.Extracting); err != nil {
		t.Fatalf("SetNext(Extracting): %v", err)
	}
	if err := q.Advance(j); err != nil { // Cross, reclaim the lease, grant a slot
		t.Fatalf("Advance (cross): %v", err)
	}
	if err := j.SetNext(job.Finalizing); err != nil {
		t.Fatalf("SetNext(Finalizing): %v", err)
	}
	if err := q.Advance(j); err != nil { // move to Finalizing
		t.Fatalf("Advance (to Finalizing): %v", err)
	}
	s := j.Snapshot()
	if s.State.State != job.Finalizing {
		t.Fatalf("drive ended at %v, want Finalizing", s.State.State)
	}
	if !q.running(j.ID(), s) {
		t.Fatalf("drive ended not running: snapshot=%+v slots=%d", s, q.slots.outstanding())
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test -count=1 ./internal/sched/ -run 'TestSettle_'
```

Expected: FAIL to **compile**, with `undefined: q.Settle` and `undefined: errCancelReserved`. A compile failure is acceptable *here* because the symbols do not exist yet; it is not acceptable as evidence for a behavioural pin (see Step 6).

- [ ] **Step 3: Append `Settle` to `internal/sched/settle.go`**

```go
// errCancelReserved is Settle's refusal of an outcome only the cancel latch
// may produce.
var errCancelReserved = errors.New("sched: Settle: OutcomeCancelled is reserved for Cancel")

// Settle is the door a dispatcher calls when its worker has returned
// terminally. It is the counterpart of the three exported job doors that YIELD
// a lease — Cross, Finish and Surrender — none of which had an exported
// reclaimer before this: q.reclaim is unexported and is the sole reclaimer
// (§3.9, D-I13), so a dispatcher calling j.Finish itself would drop the lease
// and lose a pool-A slot permanently and silently.
//
// PRECONDITION, which this package cannot check: the caller's worker for j has
// returned and will not touch the job's lease, slot, manifest or barrier
// again. There is deliberately no q.running guard here, and its absence is not
// an oversight. running() is IsOpen && Next == StateUnset && holds(), and for
// a worker that has just finished normally at Fetching every conjunct is still
// TRUE — so a !running guard would reject exactly the call this door exists to
// serve. Cancel and Retry guard because a USER initiates them and does not
// know whether a worker is mid-article; here the caller IS the worker's owner,
// which is the evidence.
//
// It refuses OutcomeCancelled. Cancel is final for a Job (D-I14) and it is
// final because Cancel latches SetIntent(IntentCancel) before settling. A
// caller reaching this door with Cancelled would skip the latch, leaving a job
// that renders as Deleted (§4.4) while still carrying IntentRun — so q.Retry
// would see an ordinary settled attempt and reopen it. The refusal returns
// before q.mu is taken, because it is a pure check on an argument.
func (q *Queue) Settle(j *job.Job, o job.Outcome) error {
	if o == job.OutcomeCancelled {
		return errCancelReserved
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.settleLocked(j, o, j.Snapshot())
}
```

- [ ] **Step 4: `goimports`, build, run the tests**

```bash
goimports -w internal/sched/settle.go && go build ./... && go test -race -count=1 ./internal/sched/ -run 'TestSettle_' -v
```

Expected: all five PASS.

- [ ] **Step 5: Re-point scenario 5.13 at `Settle`**

`TestScenario_5_13_CancellingARunningFinalizingJob` currently settles by calling `j.Finish(job.OutcomeOK, testClock())` directly, because no `Settle` door existed. **That is now the wrong path** — production will route through `Settle`, and the test would keep passing while `Settle` produced the opposite outcome. In `internal/sched/scenario_test.go`, replace the direct settle with:

```go
	// The move and the user script complete; the worker settles through the
	// door a dispatcher uses, NOT j.Finish directly. Routed here deliberately:
	// settleLocked applies the cancel latch, and this scenario is the one that
	// pins the latch NOT firing (D-I11, Finalizing is post-boundary and so
	// gated rather than interrupted). Asserting against j.Finish would leave
	// the production path unpinned.
	if err := q.Settle(j, job.OutcomeOK); err != nil {
		t.Fatalf("Settle: %v", err)
	}
```

Delete the `l, err := j.Finish(...)` / `q.reclaim(l)` pair it replaces — `settleLocked` reclaims. Leave every assertion below it unchanged.

- [ ] **Step 6: Observed red check — prove the D-I11 carve-out is pinned**

The claim to prove is that `cancelInterrupts` in `settleLocked` is load-bearing. Neuter it, do not delete it:

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/sched/settle.go "$SCRATCH/settle.bak.go"
# Make the override unconditional, exactly as the pre-review draft had it.
sed -i 's/if s.Intent == job.IntentCancel \&\& cancelInterrupts(s.State.State) {/if s.Intent == job.IntentCancel {/' internal/sched/settle.go
grep -n 'if s.Intent == job.IntentCancel {' internal/sched/settle.go   # confirm the mutation landed
go build ./... && go test -count=1 ./internal/sched/ -run 'TestSettle_GatedCancelPreservesTheOutcome|TestScenario_5_13'
cp "$SCRATCH/settle.bak.go" internal/sched/settle.go
go build ./... && go test -count=1 ./internal/sched/ -run 'TestSettle_GatedCancelPreservesTheOutcome|TestScenario_5_13'
```

Expected on the mutated run: **both tests FAIL**, reporting `Outcome = Cancelled, want OK`. Expected after restore: both PASS. **Record the exact failure message in the commit body.** If the mutated run passes, the tests do not discriminate — stop and fix them before proceeding.

- [ ] **Step 7: Gates and commit**

```bash
go vet ./... && go test -race -count=1 ./... && golangci-lint run ./... && go run ./scripts/check_citations
git add internal/sched/settle.go internal/sched/settle_test.go internal/sched/scenario_test.go
git commit -m "feat(sched): add Settle, the door that returns a worker-yielded lease

Cross, Finish and Surrender each yield a *job.Lease and q.reclaim is
unexported, so a dispatcher settling a job had no way to give one back
without leaking a pool-A slot permanently and silently.

Settle refuses OutcomeCancelled (only the latch authorises it) and honours
the latch through cancelInterrupts: an interrupted worker's error is our
artifact and settles Cancelled, a gated one's is its own and stands. D-I11's
running-Finalizing job still completes as OK.

Scenario 5.13 is re-pointed from j.Finish at Settle. It settled directly
because no door existed, which would have left the production path unpinned
while the test stayed green.

Red check: making the override unconditional fails
TestSettle_GatedCancelPreservesTheOutcome and TestScenario_5_13 with
<paste the observed message>.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

## Task 3: `Park`

**Files:**
- Modify: `internal/sched/advance.go` (rename `park` → `Park`, rewrite its doc comment)
- Modify: `internal/sched/advance_test.go`, `internal/sched/queue_test.go`, `internal/sched/cancel_test.go` (call sites)
- Create test: append to `internal/sched/advance_test.go`

**Interfaces:**
- Consumes: `parkLocked` (unchanged), `driveToFinalizing` (Task 2).
- Produces, for Task 6: `func (q *Queue) Park(j *job.Job) error`.

- [ ] **Step 1: Write the failing test — append to `internal/sched/advance_test.go`**

```go
// TestPark_IsTotalOverEveryShape pins that the exported door is
// unconditional and safe on every job a caller can hand it. An earlier draft
// of the RFC proposed refusing a non-gated job with an errNotGated sentinel,
// by analogy to Retry's errNotSettled. That analogy is wrong: gatedBy reads
// Intent and q.paused and consults NOTHING about worker liveness, so a gated
// job whose worker is still mid-article passes such a check and is stripped
// anyway, while legitimate non-gate returns — teardown, shutdown, a dead
// connection — are refused. It would protect against nothing and forbid
// something real.
//
// Totality is structural, not accidental: slotPool.release is a map delete,
// Surrender returns nil when nothing is held, and reclaim no-ops on nil (the
// last introduced by §3.9 for the paused-then-crossing case).
func TestPark_IsTotalOverEveryShape(t *testing.T) {
	t.Run("never run", func(t *testing.T) {
		q := New(2, 2, testClock, &stubWorkers{})
		j := job.New("a", "n", job.Policy{})
		if err := q.Park(j); err != nil {
			t.Fatalf("Park on a never-run job: %v", err)
		}
	})

	t.Run("already parked", func(t *testing.T) {
		q := New(2, 2, testClock, &stubWorkers{})
		j := job.New("b", "n", job.Policy{})
		if err := q.Advance(j); err != nil {
			t.Fatalf("Advance (begin): %v", err)
		}
		if err := q.Advance(j); err != nil {
			t.Fatalf("Advance (grant): %v", err)
		}
		if err := q.Park(j); err != nil {
			t.Fatalf("first Park: %v", err)
		}
		if err := q.Park(j); err != nil {
			t.Fatalf("second Park: %v — the door must be idempotent", err)
		}
		if got := q.leases.outstanding(); got != 0 {
			t.Errorf("leases outstanding = %d, want 0", got)
		}
	})

	t.Run("settled", func(t *testing.T) {
		q := New(2, 2, testClock, &stubWorkers{})
		j := job.New("c", "n", job.Policy{})
		if err := q.Advance(j); err != nil {
			t.Fatalf("Advance (begin): %v", err)
		}
		if err := q.Advance(j); err != nil {
			t.Fatalf("Advance (grant): %v", err)
		}
		if err := q.Settle(j, job.OutcomeFailed); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		if err := q.Park(j); err != nil {
			t.Fatalf("Park on a settled job: %v", err)
		}
	})

	t.Run("work ended, next set", func(t *testing.T) {
		q := New(2, 2, testClock, &stubWorkers{})
		j := job.New("d", "n", job.Policy{})
		if err := q.Advance(j); err != nil {
			t.Fatalf("Advance (begin): %v", err)
		}
		if err := q.Advance(j); err != nil {
			t.Fatalf("Advance (grant): %v", err)
		}
		if err := j.SetNext(job.Assessing); err != nil {
			t.Fatalf("SetNext: %v", err)
		}
		if err := q.Park(j); err != nil {
			t.Fatalf("Park with next set: %v", err)
		}
		if got := j.Snapshot().State.Next; got != job.Assessing {
			t.Errorf("Next = %v, want Assessing — Park releases resources, it does "+
				"not discard the verdict the finished work recorded", got)
		}
		if got := q.leases.outstanding(); got != 0 {
			t.Errorf("leases outstanding = %d, want 0", got)
		}
	})
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
go test -count=1 ./internal/sched/ -run TestPark_IsTotalOverEveryShape
```

Expected: FAIL to compile — `q.Park undefined (type *Queue has no field or method Park, but does have method park)`.

- [ ] **Step 3: Rename and rewrite the doc comment in `internal/sched/advance.go`**

Replace `park`'s entire doc comment and signature with:

```go
// Park is the door a dispatcher calls when its worker has stopped without
// finishing the state's work — the `yielded` report spec §3.6 names, whose
// handoff is "a worker that stops on a gate without ending its work reports
// yielded, and the dispatcher calls q.park(j)".
//
// PRECONDITION, which this package cannot check: the caller's worker for j has
// returned and will not touch the job's lease, slot, manifest or barrier
// again. running() stays TRUE for a worker that has yielded and not yet been
// parked — that is precisely why this door exists — so the fact is the
// caller's to guarantee, exactly as Workers.Abort's non-blocking requirement
// is.
//
// It is UNCONDITIONAL and takes no view on why the worker stopped. A gate is
// the common reason but not the only one: teardown, shutdown, and a connection
// that died all end a worker without ending the work, and all want this door.
// Gating it on gatedBy would refuse those while protecting nothing, since
// gatedBy reads Intent and q.paused and consults nothing about worker
// liveness.
//
// Advance cannot do this job. Its branch 2 tests q.holds BEFORE q.gatedBy and
// returns early for a job that still holds, because the Queue cannot tell
// "holding and working" from "holding and yielded" and stripping a live
// worker is the worse failure. So a gated job holding a lease mid-state is
// never parked by any number of Advance ticks; only this door releases it.
//
// It takes q.mu and delegates to parkLocked, which mutates both pools with no
// lock of its own. It returns an error because reclaim carries an identity
// audit that fails on a lease this pool did not issue or already took back —
// a real condition whose only other outlet would be silence. (§10's revision
// history records park's signature being narrowed to return nothing, on the
// grounds that the error was always nil; the audit added in Half B1 makes that
// no longer true.)
func (q *Queue) Park(j *job.Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.parkLocked(j)
}
```

- [ ] **Step 4: Update `parkLocked`'s citation comment**

`parkLocked`'s doc comment states `` `grep -n 'q\.parkLocked(' advance.go` finds exactly three lines: park's own delegation, and Advance's branch 2 and branch 3 gated arms``. The count is unchanged but the name is not. Change "park's own delegation" to "Park's own delegation", and re-run the citation to confirm three:

```bash
grep -n 'q\.parkLocked(' internal/sched/advance.go
```

Expected: exactly 3 lines. If the count differs, correct the comment to the observed number — do not leave a stale count.

- [ ] **Step 5: Update the two door-enumeration comments that name `park`**

Both `internal/sched/queue.go` (the `Queue.mu` lockers list) and `internal/sched/pool.go` (the "four production doors" list) name `park` in prose. Both lists are now **five** doors, because `Settle` (Task 2) also takes `q.mu`. Re-run the enumeration and write the observed result:

```bash
grep -n 'q\.mu\.Lock' internal/sched/*.go | grep -v _test.go
```

Update both comments to name `Cancel`, `Park`, `Retry`, `Advance` and `Settle`, and to state the observed count. **This is Standing Design Rule 4** — the enumeration is a command, not a recollection.

- [ ] **Step 6: Update the call sites in the test files**

```bash
sed -i 's/q\.park(/q.Park(/g' internal/sched/advance_test.go internal/sched/queue_test.go internal/sched/cancel_test.go
grep -rn 'q\.park(' internal/sched/ || echo "no lowercase call sites remain"
```

- [ ] **Step 7: `goimports`, build, run the whole package**

```bash
goimports -w internal/sched/advance.go && go build ./... && go test -race -count=1 ./internal/sched/
```

Expected: PASS, including the new `TestPark_IsTotalOverEveryShape`.

- [ ] **Step 8: Gates and commit**

```bash
go vet ./... && golangci-lint run ./... && go run ./scripts/check_citations && go run ./scripts/check_dup_comments
git add internal/sched/
git commit -m "feat(sched): export park as Park, unconditional, with its precondition stated

Advance branch 2 tests q.holds before q.gatedBy and returns early for a job
that still holds, so a gated job holding a lease mid-state is never parked by
any number of ticks. Only this door releases it, and it was unexported.

Unconditional deliberately: gatedBy consults nothing about worker liveness, so
gating this door would refuse legitimate teardown and dead-connection yields
while protecting against nothing. The precondition is stated instead, as
Workers.Abort's already is.

Not renamed to Yield: 'yields' already means handing back a lease throughout
this subsystem, so Queue.Yield would name the door that RECEIVES what
Cross/Finish/Surrender yield.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

## Task 4: `Pause`, `Resume`, `Paused`

**Files:**
- Modify: `internal/sched/queue.go` (append the three methods after `now()`)
- Modify: `internal/sched/queue_test.go` (append tests)

**Interfaces:**
- Consumes: `q.gatedBy`, `q.waitReason`.
- Produces, for Task 5 and B2.3: `func (q *Queue) Pause()`, `func (q *Queue) Resume()`, `func (q *Queue) Paused() bool`.

- [ ] **Step 1: Write the failing tests — append to `internal/sched/queue_test.go`**

```go
// TestPause_MakesGlobalPauseReachable pins the gap this closes. gatedBy reads
// q.paused, and before these doors nothing in the package could write it — so
// job.GlobalPause existed as a WaitReason that no production code path could
// ever produce.
func TestPause_MakesGlobalPauseReachable(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance (begin): %v", err)
	}
	s := j.Snapshot()
	if _, gated := q.gatedBy(s); gated {
		t.Fatal("a fresh Queue must not be gated")
	}

	q.Pause()
	if !q.Paused() {
		t.Error("Paused() = false after Pause()")
	}
	r, gated := q.gatedBy(j.Snapshot())
	if !gated || r != job.GlobalPause {
		t.Errorf("gatedBy = (%v, %v), want (GlobalPause, true)", r, gated)
	}

	q.Resume()
	if q.Paused() {
		t.Error("Paused() = true after Resume()")
	}
	if _, gated := q.gatedBy(j.Snapshot()); gated {
		t.Error("gatedBy still gated after Resume()")
	}
}

// TestPause_DoesNotTakeResourcesFromAHoldingJob pins the contract Decision 3
// puts on B2's dispatcher. §8.3: gating never interrupts work. Pause sets a
// flag; it cannot sweep, because the Queue holds no jobs to sweep. A holding
// job keeps everything until its worker returns and the dispatcher Parks it.
func TestPause_DoesNotTakeResourcesFromAHoldingJob(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance (begin): %v", err)
	}
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance (grant): %v", err)
	}

	q.Pause()
	if err := q.Advance(j); err != nil { // a tick under global pause
		t.Fatalf("Advance under pause: %v", err)
	}
	if !j.Snapshot().HoldsLease {
		t.Error("Pause took the lease from a holding job — §8.3: gating never " +
			"interrupts work; only Park releases it")
	}

	if err := q.Park(j); err != nil { // the dispatcher's half of the contract
		t.Fatalf("Park: %v", err)
	}
	if got := q.leases.outstanding(); got != 0 {
		t.Errorf("leases outstanding after Park = %d, want 0", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
go test -count=1 ./internal/sched/ -run 'TestPause_'
```

Expected: FAIL to compile — `q.Pause undefined`, `q.Paused undefined`, `q.Resume undefined`.

- [ ] **Step 3: Append the three methods to `internal/sched/queue.go`, directly after `func (q *Queue) now()`**

```go
// Pause and Resume write the queue-wide gate that gatedBy reads, and Paused
// reads it back for a renderer. Two verbs rather than SetPaused(bool), so no
// call site reads as a blind boolean.
//
// Pause sets the flag and NOTHING else, and that is a contract on the caller
// rather than an omission. The Queue holds no jobs — its state is two pools
// and a flag, with no registry and no way to enumerate what is resident (D-B5)
// — so it structurally cannot sweep. And it must not: §8.3 says gating never
// interrupts work, and Workers.Abort belongs to Cancel alone. After Pause, a
// dispatcher AWAITS its workers and calls Park per job: a Fetching worker
// checks Paused at an article boundary and reports yielded (§3.6), while a
// worker in any other state runs its stage to the end and sets next, after
// which Advance's branch 3 gates and parks it unaided.
//
// No notification channel is needed, for the reason §3.6 gives about the
// mirror case: "Resume needs no notification. SetIntent(IntentRun) writes a
// flag; the scheduling loop calls advance on its ordinary cadence and picks it
// up." Pause is symmetric — the next tick observes it through gatedBy.
func (q *Queue) Pause() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.paused = true
}

// Resume clears the queue-wide gate. See Pause.
func (q *Queue) Resume() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.paused = false
}

// Paused reports the queue-wide gate. It exists for two callers: a renderer
// filling the top-level paused field of /api?mode=queue, and a Fetching worker
// deciding at an article boundary whether to yield — the one state that gates
// per-article rather than per-stage (§3.6).
func (q *Queue) Paused() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.paused
}
```

- [ ] **Step 4: `goimports`, build, run**

```bash
goimports -w internal/sched/queue.go && go build ./... && go test -race -count=1 ./internal/sched/ -run 'TestPause_' -v
```

Expected: both PASS.

- [ ] **Step 5: Observed red check — prove `TestPause_MakesGlobalPauseReachable` discriminates**

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/sched/queue.go "$SCRATCH/queue.bak.go"
# Neuter Pause so it no longer writes the flag.
sed -i '/^func (q \*Queue) Pause() {/,/^}/ s/q.paused = true/_ = q.paused/' internal/sched/queue.go
sed -n '/^func (q \*Queue) Pause() {/,/^}/p' internal/sched/queue.go   # confirm the mutation landed
go build ./... && go test -count=1 ./internal/sched/ -run TestPause_MakesGlobalPauseReachable
cp "$SCRATCH/queue.bak.go" internal/sched/queue.go
go build ./... && go test -count=1 ./internal/sched/ -run TestPause_MakesGlobalPauseReachable
```

Expected on the mutated run: **FAIL**, reporting `Paused() = false after Pause()`. Expected after restore: PASS. Record the message.

- [ ] **Step 6: Gates and commit**

```bash
go vet ./... && go test -race -count=1 ./... && golangci-lint run ./... && go run ./scripts/check_citations
git add internal/sched/queue.go internal/sched/queue_test.go
git commit -m "feat(sched): add Pause, Resume and Paused

gatedBy read q.paused and nothing could write it, so job.GlobalPause was a
WaitReason no production path could produce.

Pause sets the flag and nothing else. The Queue holds no jobs, so it cannot
sweep, and §8.3 says it must not: gating never interrupts work. The sweep is
a dispatcher contract, stated on Pause. Paused exists for the renderer and for
a Fetching worker's article-boundary check.

Red check: neutering Pause's write fails TestPause_MakesGlobalPauseReachable
with <paste the observed message>.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

## Task 5: `Render`

**Files:**
- Create: `internal/sched/render.go`
- Create: `internal/sched/render_test.go`
- Modify: `internal/sched/scenario_test.go` (reimplement the `renderStatus` helper over `Render`)

**Interfaces:**
- Consumes: `q.running`, `q.waitReason` (both unexported, and stay so); `job.RenderView` (`internal/job/render.go:14`).
- Produces, for B2.3/B2.4: `func (q *Queue) Render(j *job.Job) job.RenderView`.

- [ ] **Step 1: Write the failing tests in `internal/sched/render_test.go`**

```go
package sched

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestRender_FillsEveryFieldFromOneSnapshot pins that Render is the seam
// job.RenderView's own doc comment describes: "Nothing in this package can
// answer that ... so this type is the seam ... Half B fills them for real."
func TestRender_FillsEveryFieldFromOneSnapshot(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance (begin): %v", err)
	}
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance (grant): %v", err)
	}

	v := q.Render(j)
	if v.State != job.Fetching {
		t.Errorf("State = %v, want Fetching", v.State)
	}
	if !v.Running {
		t.Error("Running = false, want true — the job holds its Fetching lease " +
			"and its work has not ended")
	}
	if v.Intent != job.IntentRun {
		t.Errorf("Intent = %v, want IntentRun", v.Intent)
	}
}

// TestRender_DistinguishesRunningFromReadyToAdvance pins why one door replaces
// two exported predicates. waitReason returns (0, false) for THREE different
// configurations — settled, running, and work-ended-holding-everything — so a
// caller given only the reason cannot fill RenderView.Running. Both rows below
// have no wait reason and differ only in Running.
func TestRender_DistinguishesRunningFromReadyToAdvance(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance (begin): %v", err)
	}
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance (grant): %v", err)
	}

	running := q.Render(j)
	if !running.Running || running.Reason != 0 {
		t.Fatalf("mid-work: Running=%v Reason=%v, want true and no reason",
			running.Running, running.Reason)
	}

	// Work ends. The job still holds its lease, and Fetching's successor
	// Assessing needs one too, so it waits on nothing — but it is no longer
	// running, because its work has ended.
	if err := j.SetNext(job.Assessing); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	ended := q.Render(j)
	if ended.Running {
		t.Error("work ended: Running = true, want false — a job waiting to move " +
			"is not running (§3.4)")
	}
	if ended.Reason != 0 {
		t.Errorf("work ended: Reason = %v, want none — it holds what Assessing "+
			"needs and is simply awaiting a tick", ended.Reason)
	}
	if ended.Next != job.Assessing {
		t.Errorf("work ended: Next = %v, want Assessing", ended.Next)
	}
}

// TestRender_ReportsTheGateReason pins that gates reach the view.
func TestRender_ReportsTheGateReason(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance (begin): %v", err)
	}
	if err := j.SetIntent(job.IntentPause); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	v := q.Render(j)
	if v.Reason != job.UserPaused {
		t.Errorf("Reason = %v, want UserPaused", v.Reason)
	}
	if v.Intent != job.IntentPause {
		t.Errorf("Intent = %v, want IntentPause", v.Intent)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
go test -count=1 ./internal/sched/ -run 'TestRender_'
```

Expected: FAIL to compile — `q.Render undefined`.

- [ ] **Step 3: Create `internal/sched/render.go`**

```go
package sched

import (
	"github.com/hobeone/gonzbd/internal/job"
)

// Render is this package's SOLE read door: it builds the job.RenderView that
// job.ToSABnzbd consumes. `grep -n '^func (q \*Queue) [A-Z]' internal/sched/*.go
// | grep -v _test.go` finds six exported methods — Advance, Cancel, Park,
// Pause/Resume/Paused and Retry write or gate; this one alone reads.
//
// It is one method rather than exported Running and WaitReason predicates, for
// two reasons that each rule the pair out on their own.
//
// First, waitReason's (0, false) is three-ways ambiguous: it means "no reason"
// when the attempt is settled, when the job is running, AND when work has
// ended and the job already holds what Next requires. A caller handed only
// that cannot decide RenderView.Running, so an exported WaitReason without an
// exported Running would not actually let anyone build a view.
//
// Second, two exported predicates are two lock acquisitions. A renderer
// calling q.Running(j) then q.WaitReason(j) takes q.mu twice and j.Snapshot()
// twice, and a transition landing between them yields a view that was true at
// no instant — the tear job.Snapshot exists to remove (see its comment on why
// IsOpen lives on Snapshot rather than Job), reintroduced one layer up. One
// snapshot under one lock cannot tear.
//
// It takes the *job.Job rather than a caller-supplied Snapshot so the snapshot
// and the queue predicates come from the same instant by construction; a
// caller cannot hand in a stale one. Lock order is prior spec §7.1's:
// Queue.mu here, then Job.mu inside Snapshot.
func (q *Queue) Render(j *job.Job) job.RenderView {
	q.mu.Lock()
	defer q.mu.Unlock()
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

- [ ] **Step 4: Reimplement the `renderStatus` test helper over `Render`**

`internal/sched/scenario_test.go`'s `renderStatus` currently duplicates the production seam by hand — it is the strongest evidence this door was owed. Replace its body:

```go
// renderStatus renders j's status through the same seam production does:
// q.Render composes the view under one lock, and job.ToSABnzbd turns it into
// the legacy vocabulary. Scenario tests assert on this rather than on
// State/Outcome directly wherever the spec's trace names a rendered status,
// because ToSABnzbd is what a client actually sees.
//
// This was a hand-rolled copy of Render's body until Render existed. Routing
// it here is what keeps every scenario test pinning the PRODUCTION composition
// rather than a parallel one that could drift from it silently.
func renderStatus(q *Queue, j *job.Job) constants.Status {
	return job.ToSABnzbd(q.Render(j))
}
```

- [ ] **Step 5: `goimports`, build, run the whole package**

```bash
goimports -w internal/sched/render.go internal/sched/scenario_test.go && go build ./... && go test -race -count=1 ./internal/sched/
```

Expected: PASS. Every scenario test still passes — `renderStatus` composes exactly what it composed before, now through one lock instead of three snapshots.

- [ ] **Step 6: Observed red check — prove `Running` is not a constant**

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/sched/render.go "$SCRATCH/render.bak.go"
sed -i 's/Running:   q.running(j.ID(), s),/Running:   true,/' internal/sched/render.go
grep -n 'Running:   true,' internal/sched/render.go   # confirm the mutation landed
go build ./... && go test -count=1 ./internal/sched/ -run 'TestRender_DistinguishesRunningFromReadyToAdvance'
cp "$SCRATCH/render.bak.go" internal/sched/render.go
go build ./... && go test -count=1 ./internal/sched/ -run 'TestRender_'
```

Expected on the mutated run: **FAIL**, reporting `work ended: Running = true, want false`. Expected after restore: PASS. Record the message.

- [ ] **Step 7: Gates and commit**

```bash
go vet ./... && go test -race -count=1 ./... && golangci-lint run ./... && go run ./scripts/check_citations && go run ./scripts/check_dup_comments
git add internal/sched/render.go internal/sched/render_test.go internal/sched/scenario_test.go
git commit -m "feat(sched): add Render, the single read door

job.RenderView's doc comment designates it the seam and says Half B fills it.
Nothing could: waitReason, running, holds and gatedBy are all unexported.

One door rather than exported predicates. waitReason's (0, false) is three-ways
ambiguous — settled, running, or work-ended-and-holding — so an exported
WaitReason alone cannot decide Running; and two predicates are two lock
acquisitions, reintroducing the tear job.Snapshot exists to remove.

scenario_test.go's renderStatus helper was a hand-rolled copy of this body and
now routes through it, so scenarios pin the production composition.

Red check: pinning Running to true fails
TestRender_DistinguishesRunningFromReadyToAdvance with <paste the message>.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

## Task 6: Discharge the `doc.go` obligations and sweep the claims

**Files:**
- Modify: `internal/sched/doc.go`
- Modify: `internal/sched/advance.go` (one comment — see Step 3)

**Interfaces:**
- Consumes: everything from Tasks 1–5.
- Produces: nothing. This is the claim sweep AGENTS.md step 4 requires, done once at the end so it does not go stale.

- [ ] **Step 1: Rewrite `internal/sched/doc.go`'s obligations section**

All three listed obligations are discharged. Replace from `// # Known obligations left for Half B2` to the end of the comment block with:

```go
// # What this package exports, and what B2 still owes it
//
// Six doors. Advance, Cancel, Retry, Settle and Park write or gate; Render
// reads. `grep -n '^func (q \*Queue) [A-Z]' internal/sched/*.go | grep -v
// _test.go` finds seven lines — those six plus Pause, Resume and Paused
// counting as three, so nine in total. Acquisition happens only in grantFor;
// return happens only through reclaim and releaseFor, which Settle, Park and
// Cancel all route through.
//
// Half B2 still owes this package two things, neither of which it can supply
// for itself:
//
//   - A discard path for a cancelled job that never ran. finishCancel returns
//     nil for one, because Outcome lives on the Attempt and there is none. The
//     job therefore survives, and Render reports it as not running with a
//     NoLease reason — which job.ToSABnzbd turns into StatusQueued. A job the
//     user deleted renders as queued, forever. Closing it needs residency and
//     a store, which D-B5 keeps out of this package: B2's dispatcher must
//     evict StateUnset && IntentCancel from the active set and the store.
//
//     Note this also bounds gatedBy's stated reason for ignoring IntentCancel
//     ("advance handles it first, so no cancel value reaches the render
//     path"): true for every job that has run, false for one that has not.
//
//   - A Workers implementation whose Abort neither blocks nor takes a lock a
//     caller could hold across a call into Queue. See the Workers interface.
package sched
```

- [ ] **Step 2: Verify the enumeration in Step 1 before trusting it**

```bash
grep -n '^func (q \*Queue) [A-Z]' internal/sched/*.go | grep -v _test.go
```

Expected: 9 lines — `Advance`, `Cancel`, `Park`, `Paused`, `Pause`, `Render`, `Resume`, `Retry`, `Settle`. **If the count differs, write the observed number**, not the one above. Rule 4: the enumeration is a command, not a recollection.

- [ ] **Step 3: Correct `Advance`'s unreachability claim**

`Advance`'s `Transition`-error branch releases the slot `grantFor` acquired for a destination the job never reached. Investigation during planning established that **this branch is unreachable through `Advance`'s own preconditions**, and that `Settle` makes it *less* reachable rather than more:

- `setNext` validates `CanTransition(a.state, n)` (`attempt.go:222`), so `next` can never name an illegal edge.
- `transition` refuses only `StateUnset`, `to != a.next`, a non-edge, or a `byCross` edge (`attempt.go:249-284`). Branch 3 passes `s.State.Next`, which `setNext` already validated and which `setNext` refuses to replace with a different value.
- `Advance` returns early on a settled attempt before branch 3, so the attempt is always open there.
- The one remaining route was a concurrent `j.Finish` from outside `q.mu`. `Settle` takes `q.mu`, so once workers settle through it that race is closed.

Add this as a comment on the branch, replacing nothing that is already there — the existing comment explains *why* the release is correct, and this states what is known about *reaching* it:

```go
		// Unreachable through Advance's own preconditions, and deliberately
		// kept. setNext validates CanTransition before recording next
		// (attempt.go:222) and refuses to replace a recorded next with a
		// different one, transition's four refusals are then all excluded, and
		// Advance's settled early-return guarantees an open attempt here. The
		// one route left was a concurrent j.Finish from outside q.mu, which
		// Settle closes by taking q.mu. It is NOT tested, because
		// constructing it would mean manufacturing a race this package's lock
		// discipline forbids — and it is not deleted, because the reasoning
		// above spans two packages and would have to be re-derived by anyone
		// who widens transition's admissibility.
```

- [ ] **Step 4: Sweep the repository for claims this branch falsified**

Per AGENTS.md step 4, grep from the repository root for the literals, not the concepts:

```bash
git grep -n 'q\.park(' -- '*.go' '*.md'
git grep -n 'park is unexported'
git grep -n 'no public door'
git grep -n 'has no setter'
git grep -n 'B2 needs an exported'
```

Expected: no hits outside the design docs' historical record. Any hit in `internal/` or `AGENTS.md` is a stale claim — rewrite it to what is now true, and **do not replace a narrowed claim with a broader universal** (AGENTS.md's "narrowing a referent must not broaden a scope").

- [ ] **Step 5: Re-read the two contract docs in full**

`docs/ARCHITECTURE.md` and `docs/queue-lifecycle.md` describe layer responsibilities in prose that shares no token with any identifier this plan changed, so grep cannot find drift in them. Read both end to end and correct anything this branch made false. This is two files and a few minutes, and it is the only pass that catches a sentence wrong without containing a keyword.

- [ ] **Step 6: Full gates, whole repository**

```bash
go fix ./... && goimports -w . && go build ./... && go vet ./...
go test -race -count=1 ./...
golangci-lint run ./...
go run ./scripts/check_dup_comments
go run ./scripts/check_review_banner
go run ./scripts/check_citations
./scripts/run_tests.sh
```

Expected: all pass. `check_citations` must report `0 wrong`; the agree/unverified counts will have moved because this branch adds citations.

- [ ] **Step 7: Commit**

```bash
git add internal/sched/doc.go internal/sched/advance.go
git commit -m "docs(sched): discharge the three B2 door obligations, sweep the claims

doc.go listed three obligations — no exported lease return, park unexported,
q.paused with no setter — all now closed. Replaced with what the package
exports and the two things B2 still owes it: a discard path for a cancelled
never-run job (which today renders as StatusQueued forever), and a Workers
implementation whose Abort does not block.

Advance's Transition-error branch is annotated as unreachable through
Advance's own preconditions, with the enumeration that establishes it. Settle
closes the last route by taking q.mu, so it is less reachable than before,
not more.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

## Self-Review

**1. Spec coverage.**

| Spec decision | Task |
|---|---|
| D-B1 `Settle` + `settleLocked` + `cancelInterrupts` | 1, 2 |
| D-B2 `Park`, unconditional, doc comment rewritten, no rename | 3 |
| D-B3 `Pause`/`Resume`/`Paused`, flag only | 4 |
| D-B4 `Render`, single lock span; no exported predicates, no pool telemetry | 5 |
| D-B5 no registry, store, or new dependency | Global Constraints; enforced by adding no imports |
| D-B6 B2 lands as four PRs, B2.1 first | This plan is B2.1 |
| Re-point scenario 5.13 at `Settle` | 2, Step 5 |
| Never-run cancel renders `StatusQueued` | 6, recorded as a B2.3 obligation |
| `RenderAll` | **Deliberately out of scope** — D-B4 lands it in B2.4 with its consumer |
| §4.7 settled-reorder row | **Out of scope** — a spec edit with no code, carried to B2.3 |

**2. Placeholder scan.** No "TBD", no "add appropriate error handling", no "similar to Task N". Every code step carries the code. The three `<paste the observed message>` markers in commit bodies are instructions to record a *measured* value, which AGENTS.md requires — not placeholders for design decisions.

**3. Type consistency.** `settleLocked(j *job.Job, o job.Outcome, s job.Snapshot) error` is defined in Task 1 and called with that arity in Task 1 Step 3 and Task 2 Step 3. `cancelInterrupts(s job.State) bool` is called with `s.State.State` (a `job.State`) at both sites. `driveToFinalizing(t, q, j)` is defined in Task 2 and used in Task 2 only. `Render` returns `job.RenderView` and `job.ToSABnzbd` takes one — matching `internal/job/sabnzbd.go:45`.

**4. One correction carried into the plan.** Issue #450's second comment says `Settle` "makes the failed-`Transition` slot release reachable from a test for the first time." That is **wrong**, and Task 6 Step 3 states the correction: `Advance` returns early on a settled attempt before branch 3, so a settled attempt never reaches the `Transition` call. `Settle` taking `q.mu` closes the only remaining route rather than opening one. A follow-up comment on #450 should say so.
