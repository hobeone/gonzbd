# Lifecycle Intents — Half A Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rebuild `internal/job`'s state model per the Lifecycle Intents design — remove `Waiting`, add `Intent` and `StateUnset`, make `next` an end-of-work marker, and give the irreversible boundary its own door.

**Architecture:** Seven tasks, ordered so **every commit compiles and every test passes**. Tasks 1–4 are additive; task 5 is the single atomic model change that removes `Waiting`; tasks 6–7 add the boundary door and rebuild the tests that reason over the whole state space. The additive-first ordering exists specifically so task 5 does not also have to rewrite `ToSABnzbd` — task 4 moves that off `StateView.Reason` in advance.

**Tech Stack:** Go 1.27.0, standard library only. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-08-26-lifecycle-intents-design.md` (revision 7). Read §3 and §4 before starting; read §5's thirteen scenarios before task 7.

## Global Constraints

- **Go 1.27.0** (toolchain 1.27.0). No new external dependencies — this package imports only the standard library plus `internal/constants`.
- **`internal/job` imports nothing else from this repository except `internal/constants`**, and only `sabnzbd.go` may import that among non-test sources. `TestOnlyOneNonTestFileImportsConstants` enforces it.
- **This is Half A. There is no `Queue`.** Do not create one, do not stub one, and do not add a package under `internal/queue`. Anything the spec assigns to Half B (`gatedBy`, `waitReason`, `grantFor`, `advance`, `Cancel`, `park`, `discard`, `abortWorker`, `releaseSlot`, `nextMove`) is **out of scope**.
- **Nothing imports `internal/job` yet.** Verify with `git grep -ln 'gonzbd/internal/job' -- '*.go' | grep -v '^internal/job/'` — it must return nothing. That is what makes this package safe to change wholesale.
- **No backwards compatibility** (AGENTS.md Standing Design Rule 1). Nothing persists this package's types yet. Delete removed concepts outright; do not leave shims, aliases, or deprecation comments.
- **State has one owner** (Rule 2). Every field named in the spec's §4.5 ownership table has exactly one writer. Where the plan adds a writer-enumeration test, that test is the enforcement point, not the comment.
- **Enumerate before asserting** (Rule 4). Any comment saying *only*, *sole*, *never*, *always* must be backed by a command you actually ran, cited inline. **Run every command before you write it into a comment** — a citation that errors is worse than none. `git grep` takes pathspecs, not `--include`.
- **After editing any `.go` file:** `goimports -w <file>`, then `go build ./...`. Do **not** run `go fix ./...` — it is repo-wide and rewrites unrelated packages.
- **Per-commit gates:** `go vet ./internal/job/`, `go test -race -count=1 ./internal/job/`, `golangci-lint run ./internal/job/`. All must pass before each commit.
- **Red-green is observed, not reasoned** (AGENTS.md). Every "run it and watch it fail" step means run it and read the failure. `-count=1` is mandatory; a cached `ok` is not an observation.
- **Never `git stash`.** The stash stack is shared with other sessions.

---

## File Structure

| File | Change | Responsibility after this plan |
|---|---|---|
| `internal/job/state.go` | modify | `State` enum with `StateUnset` zero; six real states |
| `internal/job/intent.go` | **create** | `Intent` enum, `AllIntents`, `String`, `IsLatched` |
| `internal/job/lease.go` | **create** | `Lease` type — the admission token |
| `internal/job/render.go` | **create** | `RenderView` — the composed read shape Half B fills |
| `internal/job/wait.go` | modify | `WaitReason` + `StateView` (loses `Reason`) |
| `internal/job/transition.go` | modify | six-edge spine; no `from == to` |
| `internal/job/attempt.go` | modify | `setNext`, `cross`, `finish` yielding the lease; `hold`/`setReason` deleted |
| `internal/job/job.go` | modify | `Intent`, `SetIntent`, `SetNext`, `Cross`, lease handling; `pending`/`Hold`/`SetWaitReason` deleted |
| `internal/job/sabnzbd.go` | modify | `ToSABnzbd(RenderView)` |
| `internal/job/doc.go` | modify | package prose swept against the new model |

Test files track their subjects. `internal/job/reachability_test.go` is rewritten wholesale in task 7.

---

## Task 1: `StateUnset` — an invalid zero

**Files:**
- Modify: `internal/job/state.go`
- Test: `internal/job/state_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `StateUnset State` (the zero value). `AllStates() []State` continues to return only real states and now **excludes** `StateUnset`.

Spec §3.2. `Waiting` is currently `iota` = 0. Removing it in task 5 would make `Fetching` the zero, so a zero `StateView` would read as an active download — Rule 2's "type with a valid zero" smell. Adding the sentinel first means task 5 never creates that window.

- [ ] **Step 1: Add the sentinel to the const block**

In `internal/job/state.go`, insert above `Waiting`:

```go
	// StateUnset is not a state. It is the zero value, and exists so that a
	// zero StateView cannot be mistaken for a job in a real state: without
	// it, removing Waiting would promote Fetching to zero and an
	// uninitialized view would read as an active download.
	//
	// No door accepts it as a destination — `grep -n 'StateUnset'
	// internal/job/transition.go` returns nothing, so it is neither a key
	// nor a value in legalEdges — and AllStates() does not list it, which
	// TestAllStates_Exhaustive asserts by name rather than by count.
	//
	// Job.State() does NOT yet return it for a job with no attempt; that
	// case is still Waiting{Next: Fetching}, constructed at job.go:120.
	StateUnset State = iota
	// Waiting holds no lease and no compute slot. It knows where it is going
	// (StateView.Next) and why it is held (StateView.Reason); it never
	// decides anything itself.
	Waiting
```

Delete the `State = iota` from the old `Waiting` line — it moves to `StateUnset`.

- [ ] **Step 2: Add its String arm**

In `func (s State) String()`, add as the first case:

```go
	case StateUnset:
		return "StateUnset"
```

- [ ] **Step 3: Run `TestAllStates_Exhaustive` and watch it fail**

```bash
go test -count=1 -run TestAllStates_Exhaustive ./internal/job/
```

Expected: FAIL. It reports `StateUnset is declared in state.go but missing from AllStates()` **and** the length mismatch. Both halves fire — that is the contradiction the spec calls out, and it is why the fix is a named exception rather than relaxing either check.

- [ ] **Step 4: Assert the exception by name**

Replace the body of `TestAllStates_Exhaustive` in `internal/job/state_test.go`:

```go
func TestAllStates_Exhaustive(t *testing.T) {
	declared := stateConstantsFromSource(t, "state.go")
	if len(declared) == 0 {
		t.Fatal("parsed no State constants from state.go; the parser below no longer matches the file's shape, so this test would pass vacuously")
	}

	// StateUnset is declared and deliberately unlisted: it is a sentinel zero,
	// not a state, and AllStates() drives exhaustive walks that must not visit
	// it. Naming it here rather than subtracting one from a count is what makes
	// a SECOND sentinel fail — someone adding one has to come and write it down.
	const sentinel = "StateUnset"
	if _, ok := declared[sentinel]; !ok {
		t.Fatalf("%s is no longer declared in state.go; if the sentinel was removed, delete this exception rather than leaving it asserting nothing", sentinel)
	}

	listed := make(map[State]bool, len(AllStates()))
	for _, s := range AllStates() {
		listed[s] = true
	}
	for name, value := range declared {
		if name == sentinel {
			if listed[value] {
				t.Errorf("%s is listed in AllStates(); the sentinel must not be walked as a real state", name)
			}
			continue
		}
		if !listed[value] {
			t.Errorf("%s is declared in state.go but missing from AllStates(); add it there and give it edges in transition.go", name)
		}
	}
	if want := len(declared) - 1; len(AllStates()) != want {
		t.Errorf("AllStates() has %d entries, state.go declares %d real states plus %s; the list has a duplicate or an entry that is no longer declared",
			len(AllStates()), want, sentinel)
	}
}
```

- [ ] **Step 5: Add a test pinning the zero value**

Append to `internal/job/state_test.go`:

```go
// TestStateUnset_IsTheZeroValue pins the property the sentinel exists for: a
// zero State — and therefore a zero StateView — is not a real state. Without
// it, removing Waiting in a later change silently promotes Fetching to zero
// and an unstarted job reads as an active download.
func TestStateUnset_IsTheZeroValue(t *testing.T) {
	var zero State
	if zero != StateUnset {
		t.Errorf("zero State = %v, want StateUnset", zero)
	}
	for _, s := range AllStates() {
		if s == StateUnset {
			t.Errorf("AllStates() contains StateUnset; it is a sentinel, not a state")
		}
	}
}
```

- [ ] **Step 6: Run the gates**

```bash
goimports -w internal/job/ && go build ./... && go vet ./internal/job/
go test -race -count=1 ./internal/job/ && golangci-lint run ./internal/job/
```

Expected: PASS. Existing tests are unaffected — `Waiting` still exists and every enum value shifted by one, which nothing persists or compares numerically.

- [ ] **Step 7: Commit**

```bash
git add internal/job/state.go internal/job/state_test.go
git commit -m "feat(job): add StateUnset as an invalid zero State

Removing Waiting would otherwise promote Fetching to the zero value, so a
zero StateView would read as an active download - Standing Design Rule 2's
\"type with a valid zero\" smell. Adding the sentinel first means that
window never exists.

TestAllStates_Exhaustive asserts the exception by NAME rather than
subtracting one from a count, so a second sentinel still fails and whoever
adds one has to write it down."
```

---

## Task 2: `Intent` — the fourth axis

**Files:**
- Create: `internal/job/intent.go`, `internal/job/intent_test.go`
- Modify: `internal/job/job.go`
- Test: `internal/job/job_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `Intent` (`IntentRun`, `IntentPause`, `IntentCancel`), `AllIntents() []Intent`, `func (i Intent) String() string`, `func (j *Job) Intent() Intent`, `func (j *Job) SetIntent(i Intent) error`, `var ErrIntentLatched = errors.New(...)`.

Spec §3.1. `Intent` is what a person has asked of this job, independent of where the job is or what it is doing — a fourth orthogonal axis alongside `State`, `Activity` and `Outcome`.

- [ ] **Step 1: Write the failing tests**

Create `internal/job/intent_test.go`:

```go
package job

import (
	"errors"
	"testing"
)

func TestIntent_RunIsZero(t *testing.T) {
	var zero Intent
	if zero != IntentRun {
		t.Errorf("zero Intent = %v, want IntentRun", zero)
	}
}

func TestAllIntents_EveryEntryHasAStringArm(t *testing.T) {
	all := AllIntents()
	if len(all) == 0 {
		t.Fatal("AllIntents() is empty")
	}
	for _, i := range all {
		if got := i.String(); got == "" || got[0] == 'I' && got == "Intent(" {
			t.Errorf("Intent(%d).String() = %q, which is not a declared arm", uint8(i), got)
		}
	}
	if got := Intent(200).String(); got != "Intent(200)" {
		t.Errorf("Intent(200).String() = %q, want the fallback form", got)
	}
}

// TestJob_IntentDefaultsToRun pins that a fresh job is not gated by anything
// it never asked for.
func TestJob_IntentDefaultsToRun(t *testing.T) {
	j := newTestJob(t)
	if got := j.Intent(); got != IntentRun {
		t.Errorf("Intent() on a fresh job = %v, want IntentRun", got)
	}
}

// TestJob_IntentRunAndPauseAreReversible pins §3.1: only cancel latches.
func TestJob_IntentRunAndPauseAreReversible(t *testing.T) {
	j := newTestJob(t)
	for _, want := range []Intent{IntentPause, IntentRun, IntentPause, IntentRun} {
		if err := j.SetIntent(want); err != nil {
			t.Fatalf("SetIntent(%v): %v", want, err)
		}
		if got := j.Intent(); got != want {
			t.Errorf("Intent() = %v, want %v", got, want)
		}
	}
}

// TestJob_IntentCancelLatches pins the one restriction §3.1 places on
// transitions: cancel is final for this Job. Prior spec D8 makes a full redo a
// re-added NZB starting a NEW Job, so clearing the latch would let a job the
// user deleted come back through a path that never re-asked them.
func TestJob_IntentCancelLatches(t *testing.T) {
	j := newTestJob(t)
	if err := j.SetIntent(IntentCancel); err != nil {
		t.Fatalf("SetIntent(IntentCancel): %v", err)
	}
	for _, try := range []Intent{IntentRun, IntentPause} {
		if err := j.SetIntent(try); !errors.Is(err, ErrIntentLatched) {
			t.Errorf("SetIntent(%v) after cancel, error = %v, want ErrIntentLatched", try, err)
		}
		if got := j.Intent(); got != IntentCancel {
			t.Fatalf("Intent() = %v after a refused SetIntent; the refusal must not have partially applied", got)
		}
	}
	// Re-asserting cancel is a no-op, not an error: an idempotent cancel from a
	// retrying caller is not a mistake to report.
	if err := j.SetIntent(IntentCancel); err != nil {
		t.Errorf("SetIntent(IntentCancel) twice = %v, want nil", err)
	}
}

// TestJob_SetIntentIsLegalInEveryState pins §3.1's correction to revision 3,
// which refused SetIntent on a settled attempt by analogy with wait reasons. An
// intent is not a wait reason: a settled job may be retried, and the intent it
// carries governs what happens when it is. Refusing left a job that was paused
// and then failed neither unpausable nor usefully retriable.
func TestJob_SetIntentIsLegalInEveryState(t *testing.T) {
	j := newTestJob(t)
	if err := j.SetIntent(IntentPause); err != nil {
		t.Fatalf("SetIntent on a never-run job: %v", err)
	}
	mustBegin(t, j)
	if err := j.SetIntent(IntentRun); err != nil {
		t.Fatalf("SetIntent on an open attempt: %v", err)
	}
	if err := j.Finish(OutcomeFailed, testClock()); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := j.SetIntent(IntentPause); err != nil {
		t.Fatalf("SetIntent on a SETTLED attempt: %v — a settled job may be retried, and its intent governs that retry", err)
	}
}
```

> **Note for the implementer:** `j.Finish` returns only `error` at this point in
> the plan. Task 6 changes it to `(*Lease, error)` and will update this call.

- [ ] **Step 2: Run them and watch them fail**

```bash
go test -count=1 -run 'TestIntent|TestAllIntents|TestJob_Intent|TestJob_SetIntent' ./internal/job/
```

Expected: FAIL to compile — `undefined: Intent`, `undefined: AllIntents`, `undefined: ErrIntentLatched`.

- [ ] **Step 3: Create `internal/job/intent.go`**

```go
package job

import "fmt"

// Intent is what a person has asked of this job, independent of where the job
// is (State), what it is executing (Activity), or how an attempt ended
// (Outcome). It is the fourth orthogonal axis — see
// docs/superpowers/specs/2026-08-26-lifecycle-intents-design.md §3.1.
//
// It exists because pause is a GATE, not an interrupt (prior spec §8.3): work
// in flight runs to the end of its state and the job then stops. The request
// and the boundary that consumes it are separated in time, and the request
// needs somewhere to live in between. Before this axis there was nowhere — a
// pause could only be recorded once the job had ALREADY stopped, which is the
// wrong way round and made "finishing repair, then pausing" unrepresentable.
//
// Intent lives on the Job, not the Attempt: a paused job that is retried stays
// paused, because the pause was a statement about the job rather than about
// one run of it.
type Intent uint8

const (
	// IntentRun is the default: nothing has been asked, so the job runs when
	// the scheduler can serve it.
	IntentRun Intent = iota
	// IntentPause means stop at the next gate. Freely reversible.
	IntentPause
	// IntentCancel means stop and settle. It LATCHES — see SetIntent.
	IntentCancel
)

// AllIntents returns every declared Intent. TestAllIntents_EveryEntryHasAStringArm
// fails if one lacks a String arm.
func AllIntents() []Intent { return []Intent{IntentRun, IntentPause, IntentCancel} }

// IsLatched reports whether this intent, once set, refuses to be replaced.
func (i Intent) IsLatched() bool { return i == IntentCancel }

func (i Intent) String() string {
	switch i {
	case IntentRun:
		return "IntentRun"
	case IntentPause:
		return "IntentPause"
	case IntentCancel:
		return "IntentCancel"
	default:
		return fmt.Sprintf("Intent(%d)", uint8(i))
	}
}
```

- [ ] **Step 4: Add the field, accessor and mutator to `internal/job/job.go`**

Add to the `Job` struct, guarded by `mu`, below `attempts`:

```go
	// intent is what a person has asked of this job. Guarded by mu. Sole
	// writer: SetIntent — enforced by
	// TestIntentWrites_MatchTheEnumerationStatedInProse (task 7), not by this
	// comment.
	intent Intent
```

Add the sentinel near the other package errors:

```go
// ErrIntentLatched is returned by SetIntent when the job has already been
// cancelled. Cancel is final for a Job because of where it leads, not
// because of what it renders as: the intent is not consulted by rendering
// at all (`git grep -n 'Intent' internal/job/sabnzbd.go` exits 1). What
// reaches the user as StatusDeleted is the settled verdict OutcomeCancelled,
// mapped at sabnzbd.go:100 — the sole arm returning it.
//
// The latch is one-way because prior spec D8 makes a full redo a re-added
// NZB starting a NEW Job rather than a new attempt on this one. Clearing the
// latch would let a job the user deleted come back through a path that never
// re-asked them.
var ErrIntentLatched = errors.New("job: intent is latched; this job is cancelled")
```

Add the methods:

```go
// Intent reports what has been asked of this job.
func (j *Job) Intent() Intent {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.intent
}

// SetIntent records what is being asked of this job. Legal in every state,
// including once the current attempt is settled: a settled job may be retried,
// and the intent it carries governs what happens when it is.
//
// Refuses only when the job is already cancelled, and only for a DIFFERENT
// intent — re-asserting cancel is an idempotent no-op rather than an error,
// since a retrying caller repeating itself is not a mistake to report.
func (j *Job) SetIntent(i Intent) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.intent.IsLatched() && i != j.intent {
		return fmt.Errorf("%w: cannot replace %s with %s", ErrIntentLatched, j.intent, i)
	}
	j.intent = i
	return nil
}
```

Add `"fmt"` to `job.go`'s imports.

- [ ] **Step 5: Run the tests and watch them pass**

```bash
goimports -w internal/job/ && go test -race -count=1 -run 'TestIntent|TestAllIntents|TestJob_Intent|TestJob_SetIntent' ./internal/job/
```

Expected: PASS.

- [ ] **Step 6: Prove the latch test discriminates**

The latch is the one behavioural rule here, so pin it by mutation:

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/job/job.go "$SCRATCH/job.bak.go"
sed -i 's|if j.intent.IsLatched() \&\& i != j.intent {|if false \&\& j.intent.IsLatched() \&\& i != j.intent {|' internal/job/job.go
grep -n 'if false && j.intent.IsLatched' internal/job/job.go   # confirm it landed
go test -count=1 -run TestJob_IntentCancelLatches ./internal/job/   # MUST fail
cp "$SCRATCH/job.bak.go" internal/job/job.go
git diff --stat internal/job/job.go   # MUST be empty
```

Record the observed failure message in the commit body.

- [ ] **Step 7: Run the gates and commit**

```bash
go build ./... && go vet ./internal/job/ && go test -race -count=1 ./internal/job/ && golangci-lint run ./internal/job/
git add internal/job/intent.go internal/job/intent_test.go internal/job/job.go internal/job/job_test.go
git commit -m "feat(job): add Intent as a fourth orthogonal axis on Job

Pause is a gate, not an interrupt (prior spec §8.3): work in flight runs to
the end of its state and the job then stops. The request and the boundary
that consumes it are separated in time, and until now the request had
nowhere to live in between - it could only be recorded once the job had
already stopped, which made \"finishing repair, then pausing\" unrepresentable.

Intent lives on the Job, not the Attempt: a paused job that is retried stays
paused. Only IntentCancel latches, and re-asserting it is an idempotent
no-op rather than an error.

SetIntent is legal in every state including a settled attempt - an intent is
not a wait reason, and a settled job may be retried."
```

---

## Task 3: `Lease`, and one releaser for it

**Files:**
- Create: `internal/job/lease.go`, `internal/job/lease_test.go`
- Modify: `internal/job/job.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Lease struct{...}`, `func (j *Job) HoldsLease() bool`, `func (j *Job) Grant(l *Lease) error`, `func (j *Job) Surrender() *Lease`, `func (j *Job) surrenderLocked() *Lease`, `var ErrAlreadyLeased = errors.New(...)`.

Spec §3.9 and prior spec §6. The `Lease` is the admission token: *"Three things with one lifetime are one object."* Half A defines the type and the Job's ownership of it; Half B fills it with a `Manifest` and `StorageBarrier` and hands it out from pool A.

**Why the type is defined here rather than stubbed:** §6's argument is that the lease, manifest and barrier are one object *because they share a lifetime*. An opaque placeholder would be a second representation of the same thing, which is the Rule 2 violation §6 exists to retire. The struct is defined with the fields it will carry, commented as unpopulated until Half B.

**The critical constraint:** `Job.mu` is a `sync.RWMutex` and Go mutexes are **not reentrant**. `withOpenAttempt` takes `j.mu.Lock()` and holds it across its callback, so any door reached through it that then called the exported `Surrender()` would deadlock the job permanently — no error, no timeout. Hence `surrenderLocked`.

- [ ] **Step 1: Write the failing tests**

Create `internal/job/lease_test.go`:

```go
package job

import (
	"errors"
	"sync"
	"testing"
)

func TestJob_GrantAndSurrender(t *testing.T) {
	j := newTestJob(t)
	if j.HoldsLease() {
		t.Fatal("a fresh job holds a lease")
	}
	l := &Lease{}
	if err := j.Grant(l); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if !j.HoldsLease() {
		t.Error("HoldsLease() is false after Grant")
	}
	if got := j.Surrender(); got != l {
		t.Errorf("Surrender() = %p, want the granted lease %p", got, l)
	}
	if j.HoldsLease() {
		t.Error("HoldsLease() is true after Surrender")
	}
}

// TestJob_SurrenderIsNilWhenNothingHeld pins the property §3.9 depends on: a
// job may legitimately reach the crossing holding no lease, having been paused
// at Assessing{next: Extracting} and resumed. Surrender must report that rather
// than assert, so the Queue's sole reclaimer can no-op on nil.
func TestJob_SurrenderIsNilWhenNothingHeld(t *testing.T) {
	j := newTestJob(t)
	if got := j.Surrender(); got != nil {
		t.Errorf("Surrender() with nothing held = %p, want nil", got)
	}
	if got := j.Surrender(); got != nil {
		t.Errorf("Surrender() twice = %p, want nil", got)
	}
}

func TestJob_GrantRefusesASecondLease(t *testing.T) {
	j := newTestJob(t)
	if err := j.Grant(&Lease{}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := j.Grant(&Lease{}); !errors.Is(err, ErrAlreadyLeased) {
		t.Errorf("second Grant, error = %v, want ErrAlreadyLeased", err)
	}
}

func TestJob_GrantRefusesNil(t *testing.T) {
	j := newTestJob(t)
	if err := j.Grant(nil); err == nil {
		t.Error("Grant(nil) = nil; a nil lease is indistinguishable from holding none")
	}
}

// TestJob_LeaseIsRaceFree runs the accessors concurrently under -race. The
// lease field is guarded by the same mutex as the lifecycle fields and must not
// be readable without it.
func TestJob_LeaseIsRaceFree(t *testing.T) {
	j := newTestJob(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 200 {
				if err := j.Grant(&Lease{}); err == nil {
					_ = j.Surrender()
				}
				_ = j.HoldsLease()
			}
		})
	}
	wg.Wait()
}
```

- [ ] **Step 2: Run them and watch them fail**

```bash
go test -count=1 -run 'TestJob_Grant|TestJob_Surrender|TestJob_Lease' ./internal/job/
```

Expected: FAIL to compile — `undefined: Lease`, `undefined: ErrAlreadyLeased`.

- [ ] **Step 3: Create `internal/job/lease.go`**

```go
package job

// Lease is the admission token for the correctness loop. Prior spec §6:
// pool-A capacity, the resident Manifest and the StorageBarrier have exactly
// one lifetime between them — from a job beginning to fetch until it crosses
// the irreversible boundary — and "three things with one lifetime are one
// object".
//
// Holding one is what makes a job RUNNABLE in Fetching, Assessing and
// Repairing; it is surrendered at the crossing, and Extracting and Finalizing
// hold only a compute slot. That is why running-ness is a question about what
// a job HOLDS rather than about pool occupancy — see the design's §3.4.
//
// The manifest and barrier fields arrive in Half B, together with the Queue
// that issues these. They are named here rather than left out because §6's
// argument is that the three share a lifetime and are therefore one object;
// an opaque placeholder now would be a second representation of the same
// thing, which is the ownership violation §6 exists to retire.
type Lease struct {
	// manifest *Manifest        // Half B
	// barrier  *StorageBarrier  // Half B
}
```

- [ ] **Step 4: Add the field and the three methods to `internal/job/job.go`**

Add to the `Job` struct, guarded by `mu`:

```go
	// lease is the admission token this job currently holds, or nil. Guarded
	// by mu. Granted by Grant; released by surrenderLocked, which is the sole
	// writer of nil into this field. At this commit Surrender is its only
	// caller — Cross does not yet exist in this package (it is task 6) and
	// Finish does not yet touch lease at all (its signature changes to yield
	// one, also task 6); both are wired through surrenderLocked then.
	lease *Lease
```

Add the sentinel:

```go
// ErrAlreadyLeased is returned by Grant when the job already holds a lease.
// Pool-A capacity is reserved across the whole correctness loop (prior spec
// §8.1), so a job re-entering Fetching from Assessing still holds the one it
// was given; a second grant would mean the Queue had issued capacity twice.
var ErrAlreadyLeased = errors.New("job: already holds a lease")
```

Add the methods:

```go
// HoldsLease reports whether this job currently holds its admission token.
// This is half of what makes a job "running" — see the design's §3.4.
func (j *Job) HoldsLease() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.lease != nil
}

// Grant hands this job an admission token. Refuses nil, which would be
// indistinguishable from holding none, and refuses a second lease.
func (j *Job) Grant(l *Lease) error {
	if l == nil {
		return fmt.Errorf("job: Grant(nil): a nil lease is indistinguishable from holding none")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.lease != nil {
		return ErrAlreadyLeased
	}
	j.lease = l
	return nil
}

// Surrender yields the lease, or nil if none is held. Callers that already
// hold j.mu must use surrenderLocked instead — see its comment.
func (j *Job) Surrender() *Lease {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.surrenderLocked()
}

// surrenderLocked is the sole releaser of j.lease. Must hold mu.
//
// It exists because j.mu is a sync.RWMutex and Go mutexes are NOT reentrant.
// The doors that will end a job's need for a lease — Cross and Finish — will
// reach the attempt through withOpenAttempt, which takes j.mu.Lock() and
// holds it across its callback. A door calling the exported Surrender() from
// there would take j.mu a second time and deadlock the job permanently, with
// no error and no timeout. Routing releases through this helper keeps one
// releaser without reacquiring anything.
//
// Not yet true at this commit: Cross does not exist in this package (task 6
// adds it), and Finish does not call this method — Finish's signature
// changes to yield the lease, also in task 6. Today Surrender is this
// method's only caller.
func (j *Job) surrenderLocked() *Lease {
	l := j.lease
	j.lease = nil
	return l
}
```

- [ ] **Step 5: Run the tests and watch them pass**

```bash
goimports -w internal/job/ && go test -race -count=1 -run 'TestJob_Grant|TestJob_Surrender|TestJob_Lease' ./internal/job/
```

Expected: PASS, including under `-race`.

- [ ] **Step 6: Run the gates and commit**

```bash
go build ./... && go vet ./internal/job/ && go test -race -count=1 ./internal/job/ && golangci-lint run ./internal/job/
git add internal/job/lease.go internal/job/lease_test.go internal/job/job.go
git commit -m "feat(job): add the Lease type and one releaser for it

Prior spec §6: pool-A capacity, the resident Manifest and the StorageBarrier
share exactly one lifetime, and three things with one lifetime are one
object. Half A defines the type and the Job's ownership of it; Half B fills
it and issues it from pool A.

surrenderLocked exists because j.mu is a sync.RWMutex and Go mutexes are not
reentrant. The doors that will end a job's need for a lease - Cross and
Finish, both task 6 - will reach the attempt through withOpenAttempt, which
holds j.mu across its callback - a door calling the exported Surrender()
from there would take the lock a second time and deadlock the job
permanently, with no error and no timeout."
```

---

## Task 4: `RenderView`, and `ToSABnzbd` keyed on running-ness

**Files:**
- Create: `internal/job/render.go`
- Modify: `internal/job/sabnzbd.go`, `internal/job/sabnzbd_test.go`, `internal/job/job_test.go`

**Interfaces:**
- Consumes: `StateView` (unchanged so far), `WaitReason`, `Intent` (task 2).
- Produces: `type RenderView struct { StateView; Running bool; Reason WaitReason; Intent Intent }`, `func ToSABnzbd(v RenderView) constants.Status`.

Spec §4.4. This task exists **before** the model change so that task 5 does not also have to rewrite the status shim. `ToSABnzbd` currently reads `v.State == Waiting` and `v.Reason.IsPause()`; neither survives task 5, and moving it onto `RenderView` now decouples the two changes.

**This answers the question §4.8 left to this plan.** `ToSABnzbd` is *not* deleted and reintroduced in Half B. It takes a composed view that Half A can construct directly in tests, so the whole §4.4 table stays covered here; Half B's only job is to fill the three composed fields from the Queue.

- [ ] **Step 1: Create `internal/job/render.go`**

```go
package job

// RenderView is a job's state as a CONSUMER sees it: the attempt's own view,
// plus the three facts only the Queue can supply.
//
// Running-ness and the wait reason are DERIVED, never stored (design §3.4,
// D-I4). A job is running when its attempt is open, it holds everything its
// current state requires, and that state's work has not ended. Nothing in this
// package can answer that — it depends on pool-B slots and on a queue-wide
// pause flag that live in the Queue — so this type is the seam.
//
// Half A constructs these directly in tests, which is what keeps §4.4's whole
// table covered before a Queue exists. Half B fills them for real.
type RenderView struct {
	StateView

	// Running is the design's running(j): attempt open, holds what the
	// current state requires, and next unset.
	Running bool
	// Reason is why it is not running, and is meaningless when Running is
	// true. Derived by the Queue from intent, its own pause flag, and what
	// the job holds.
	Reason WaitReason
	// Intent is the Job's, carried here so a consumer can render "finishing
	// repair, then pausing" — a running job with IntentPause shows its state,
	// not Paused.
	Intent Intent
}
```

- [ ] **Step 2: Rewrite the failing test table**

Replace `TestToSABnzbd` in `internal/job/sabnzbd_test.go`:

```go
func TestToSABnzbd(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    RenderView
		want constants.Status
	}{
		{"never run", RenderView{StateView: StateView{State: StateUnset}}, constants.StatusQueued},
		{"never run, paused", RenderView{StateView: StateView{State: StateUnset}, Reason: UserPaused, Intent: IntentPause}, constants.StatusPaused},

		{"waiting for a lease", RenderView{StateView: StateView{State: Fetching}, Reason: NoLease}, constants.StatusQueued},
		{"waiting for a compute slot", RenderView{StateView: StateView{State: Fetching}, Reason: NoComputeSlot}, constants.StatusQueued},
		{"user paused", RenderView{StateView: StateView{State: Fetching}, Reason: UserPaused, Intent: IntentPause}, constants.StatusPaused},
		{"globally paused", RenderView{StateView: StateView{State: Extracting}, Reason: GlobalPause, Intent: IntentRun}, constants.StatusPaused},

		{"first-pass download", RenderView{StateView: StateView{State: Fetching}, Running: true}, constants.StatusDownloading},
		{"fetching recovery volumes", RenderView{StateView: StateView{State: Fetching, Assessed: true}, Running: true}, constants.StatusFetching},

		{"cheap verification", RenderView{StateView: StateView{State: Assessing, Activity: ActCRCCheck}, Running: true}, constants.StatusQuickCheck},
		{"full verification", RenderView{StateView: StateView{State: Assessing, Activity: ActPar2Verify}, Running: true}, constants.StatusVerifying},
		{"assessing, no activity yet", RenderView{StateView: StateView{State: Assessing}, Running: true}, constants.StatusVerifying},

		{"repairing", RenderView{StateView: StateView{State: Repairing, Activity: ActPar2Repair}, Running: true}, constants.StatusRepairing},
		{"extracting", RenderView{StateView: StateView{State: Extracting, Activity: ActUnpack}, Running: true}, constants.StatusExtracting},
		{"volume recovery is still extracting", RenderView{StateView: StateView{State: Extracting, Activity: ActVolumeRecovery}, Running: true}, constants.StatusExtracting},

		{"finalizing, moving", RenderView{StateView: StateView{State: Finalizing, Activity: ActMove}, Running: true}, constants.StatusMoving},
		{"finalizing, script", RenderView{StateView: StateView{State: Finalizing, Activity: ActScript}, Running: true}, constants.StatusRunning},
		{"finalizing, cleanup", RenderView{StateView: StateView{State: Finalizing, Activity: ActCleanup}, Running: true}, constants.StatusMoving},

		// A RUNNING job with IntentPause renders as its state, not Paused. It
		// is still repairing; the pause takes effect at the next gate. This is
		// the whole point of the axis — see design §1.1.
		{"running, pause requested", RenderView{StateView: StateView{State: Repairing, Activity: ActPar2Repair}, Running: true, Intent: IntentPause}, constants.StatusRepairing},
		{"running, cancel requested", RenderView{StateView: StateView{State: Extracting, Activity: ActUnpack}, Running: true, Intent: IntentCancel}, constants.StatusExtracting},

		{"completed", RenderView{StateView: StateView{State: Finished, Outcome: OutcomeOK}}, constants.StatusCompleted},
		{"failed", RenderView{StateView: StateView{State: Finished, Outcome: OutcomeFailed}}, constants.StatusFailed},
		{"unrecoverable renders as failed", RenderView{StateView: StateView{State: Finished, Outcome: OutcomeUnrecoverable}}, constants.StatusFailed},
		{"cancelled renders as deleted", RenderView{StateView: StateView{State: Finished, Outcome: OutcomeCancelled}}, constants.StatusDeleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToSABnzbd(tc.v); got != tc.want {
				t.Errorf("ToSABnzbd(%+v) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 3: Run it and watch it fail**

```bash
go test -count=1 -run TestToSABnzbd ./internal/job/
```

Expected: FAIL to compile — `undefined: RenderView` is resolved by step 1, so the real failure is `cannot use tc.v (variable of type RenderView) as StateView value`.

- [ ] **Step 4: Rewrite `ToSABnzbd`**

Replace the function in `internal/job/sabnzbd.go`. Keep the existing doc comment's paragraphs about one-way translation and the four never-produced statuses; replace the arms:

```go
func ToSABnzbd(v RenderView) constants.Status {
	if v.State == Finished {
		return finishedStatus(v.Outcome)
	}
	if !v.Running {
		// Keyed on the wait REASON, not on Intent. Under a queue-wide pause
		// every job still carries IntentRun, so keying on IntentPause alone
		// would render the whole queue as Queued — a live API regression
		// against TestJob_PausedRendersAsStatusPaused's globally-paused
		// subtest. WaitReason.IsPause() covers UserPaused and GlobalPause
		// both, so routing through it cannot omit one.
		if v.Reason.IsPause() {
			return constants.StatusPaused
		}
		return constants.StatusQueued
	}

	// Running. Intent is deliberately NOT consulted: a job with a pause
	// requested is still repairing until it reaches a gate, and reporting it
	// as Paused is what design §1.1 exists to prevent. Surfacing "finishing
	// repair, then pausing" is the UI reading RenderView.Intent alongside this
	// status.
	switch v.State {
	case Fetching:
		if v.Assessed {
			return constants.StatusFetching
		}
		return constants.StatusDownloading
	case Assessing:
		if v.Activity == ActCRCCheck {
			return constants.StatusQuickCheck
		}
		return constants.StatusVerifying
	case Repairing:
		return constants.StatusRepairing
	case Extracting:
		return constants.StatusExtracting
	case Finalizing:
		if v.Activity == ActScript {
			return constants.StatusRunning
		}
		return constants.StatusMoving
	default:
		// StateUnset with Running true is not constructible by the Queue — a
		// job with no attempt holds nothing — but ToSABnzbd is total by
		// construction and a blank status in somebody's Sonarr is worse than
		// a wrong-but-declared one.
		return constants.StatusQueued
	}
}
```

- [ ] **Step 5: Update the three product-space tests**

In `internal/job/sabnzbd_test.go`, each of `TestToSABnzbd_IsTotal`, `TestToSABnzbd_EmitsOnlyDeclaredStatuses` and `TestToSABnzbd_NeverEmitsUnproducedStatuses` walks a product space. Extend each loop nest with `Running` and `Intent`, and keep `Reason` — **the `Reason` axis is not optional**: §4.4's `Paused` row keys on `IsPause()` precisely so queue-wide pause is covered, and a walk over `Intent` alone would pass while `GlobalPause` rendered `Queued`.

Replace the inner construction in all three with:

```go
	for _, s := range append(states, StateUnset) {
		for _, a := range activities {
			for _, o := range outcomes {
				for _, r := range reasons {
					for _, in := range AllIntents() {
						for _, assessed := range []bool{false, true} {
							for _, running := range []bool{false, true} {
								v := RenderView{
									StateView: StateView{State: s, Activity: a, Outcome: o, Assessed: assessed},
									Running:   running, Reason: r, Intent: in,
								}
								got := ToSABnzbd(v)
								// Then each test's own assertion on `got`:
								//   IsTotal                    -> got == "" is a failure
								//   EmitsOnlyDeclaredStatuses  -> !declared[got] is a failure
								//   NeverEmitsUnproducedStatuses -> unproduced[got] is a failure
								_ = got
							}
						}
					}
				}
			}
		}
	}
```

Note `append(states, StateUnset)`: `AllStates()` excludes the sentinel by design (task 1), and the shim must be total over it too.

- [ ] **Step 6: Add the queue-wide-pause regression pin**

Append to `internal/job/sabnzbd_test.go`:

```go
// TestToSABnzbd_GlobalPauseRendersAsPaused is the regression pin for the one
// finding in this area that reached a shipped revision of the design: a table
// keyed on Intent renders a globally-paused queue as Queued, because each job
// still carries IntentRun. Keyed on WaitReason.IsPause() it cannot.
func TestToSABnzbd_GlobalPauseRendersAsPaused(t *testing.T) {
	for _, s := range AllStates() {
		if s == Finished {
			continue // settled jobs are not waiting for anything
		}
		v := RenderView{StateView: StateView{State: s}, Running: false, Reason: GlobalPause, Intent: IntentRun}
		if got := ToSABnzbd(v); got != constants.StatusPaused {
			t.Errorf("ToSABnzbd(%+v) = %q, want StatusPaused; a queue-wide pause leaves every job at IntentRun, "+
				"so a table keyed on Intent would report the whole queue as Queued", v, got)
		}
	}
}
```

- [ ] **Step 7: Update `TestJob_PausedRendersAsStatusPaused`**

That test in `job_test.go` calls `ToSABnzbd(j.State())`, which no longer compiles. It exercises `SetWaitReason`, which task 5 deletes. **Delete the test here** and rely on `TestToSABnzbd_GlobalPauseRendersAsPaused` plus the table, which cover the same property against the type that now decides it. Note the deletion and its replacement in the commit body — this is a covered property changing hands, not one being dropped.

- [ ] **Step 8: Prove the global-pause pin discriminates**

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/job/sabnzbd.go "$SCRATCH/sabnzbd.bak.go"
sed -i 's|if v.Reason.IsPause() {|if v.Intent == IntentPause {|' internal/job/sabnzbd.go
grep -n 'if v.Intent == IntentPause' internal/job/sabnzbd.go
go test -count=1 -run TestToSABnzbd_GlobalPauseRendersAsPaused ./internal/job/   # MUST fail
cp "$SCRATCH/sabnzbd.bak.go" internal/job/sabnzbd.go
git diff --stat internal/job/sabnzbd.go   # MUST be empty
```

This is the exact mutation that shipped in a design revision. Record the failure message.

- [ ] **Step 9: Run the gates and commit**

```bash
goimports -w internal/job/ && go build ./... && go vet ./internal/job/
go test -race -count=1 ./internal/job/ && golangci-lint run ./internal/job/
git add internal/job/render.go internal/job/sabnzbd.go internal/job/sabnzbd_test.go internal/job/job_test.go
git commit -m "feat(job): key ToSABnzbd on running-ness via a composed RenderView

Running-ness and the wait reason are derived, never stored (design D-I4), and
neither is answerable inside this package - both depend on pool-B slots and a
queue-wide pause flag that live in the Queue. RenderView is that seam. Half A
constructs it directly in tests, which keeps §4.4's whole table covered before
a Queue exists; Half B only has to fill three fields.

The Paused row keys on WaitReason.IsPause(), not on Intent. Under a
queue-wide pause every job still carries IntentRun, so an Intent-keyed table
renders the entire queue as Queued - a live API regression, and one that
reached a shipped revision of the design.

A running job with IntentPause renders as its state, not Paused: it is still
repairing until it reaches a gate.

TestJob_PausedRendersAsStatusPaused is deleted; it drove the property through
SetWaitReason, which the next commit removes. The property moves to
TestToSABnzbd_GlobalPauseRendersAsPaused and the table, against the type that
now decides it."
```

---

## Task 5: Remove `Waiting`; `next` becomes the end-of-work marker

**Files:**
- Modify: `internal/job/state.go`, `internal/job/wait.go`, `internal/job/transition.go`, `internal/job/attempt.go`, `internal/job/job.go`
- Test: `internal/job/state_test.go`, `internal/job/wait_test.go`, `internal/job/transition_test.go`, `internal/job/attempt_test.go`, `internal/job/job_test.go`

**Interfaces:**
- Consumes: `StateUnset` (task 1), `Intent` (task 2).
- Produces: `func (j *Job) SetNext(n State) error`, `var ErrNextAlreadySet`, `var ErrNotAssessing`. **Removed:** `Waiting`, `Attempt.hold`, `Attempt.setReason`, `Job.Hold`, `Job.SetWaitReason`, `Job.pending`, `Attempt.reason`, `StateView.Reason`, `ErrHoldRequired`, `ErrNotWaiting`, `CanTransition`'s `from == to` arm.

Spec §3.2, §3.3, §4.1, §4.2. **This is one logical change and therefore one commit** — removing `Waiting` breaks `state.go`, `transition.go`, `attempt.go`, `job.go` and `wait.go` simultaneously, so there is no compiling intermediate. AGENTS.md's "one logical change per commit" is satisfied; "each commit builds" is satisfied at the end of this task, not inside it.

**The rule being implemented, verbatim from §3.3:**

> `next` is set ⟺ the current state's work has **ENDED** and the job continues to another work state. Its value is where it continues to. When work ends and the job does *not* continue, `finish` settles the attempt instead, and `next` stays unset.

*Ended*, not *succeeded*. A `Fetching` that exhausts every server has ended, and `Assessing` decides what that means. `Finalizing` is not an exception — its work ends and it continues nowhere, so it settles.

- [ ] **Step 1: Write the failing tests for `SetNext`**

Append to `internal/job/attempt_test.go`:

```go
// TestAttempt_SetNextRecordsTheDestination pins the marker's basic contract.
func TestAttempt_SetNextRecordsTheDestination(t *testing.T) {
	a := newAttempt(testClock())
	if a.next != StateUnset {
		t.Fatalf("a fresh attempt has next = %v, want StateUnset (its work has not ended)", a.next)
	}
	if err := a.setNext(Assessing); err != nil {
		t.Fatalf("setNext(Assessing) from Fetching: %v", err)
	}
	if a.next != Assessing {
		t.Errorf("next = %v, want Assessing", a.next)
	}
}

// TestAttempt_SetNextIsWriteOncePerVisit is defect 3's pin, carried into the
// door that replaced hold. Without it a verdict of Repairing could be
// overwritten with Extracting and the job would cross the boundary SKIPPING
// REPAIR. Re-declaring the same value is a no-op, so a caller retrying is not
// punished.
func TestAttempt_SetNextIsWriteOncePerVisit(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, a, Assessing)
	if err := a.setNext(Repairing); err != nil {
		t.Fatalf("setNext(Repairing): %v", err)
	}
	if err := a.setNext(Extracting); !errors.Is(err, ErrNextAlreadySet) {
		t.Fatalf("setNext(Extracting) over a Repairing verdict, error = %v, want ErrNextAlreadySet", err)
	}
	if a.next != Repairing {
		t.Fatalf("next = %v after a refused setNext; the refusal must not have partially applied", a.next)
	}
	if err := a.setNext(Repairing); err != nil {
		t.Errorf("setNext(Repairing) twice = %v, want nil (idempotent re-assertion)", err)
	}
}

// TestAttempt_SetNextRejectsANonEdge pins that a destination the current state
// could not reach directly cannot be recorded either.
func TestAttempt_SetNextRejectsANonEdge(t *testing.T) {
	a := newAttempt(testClock()) // Fetching
	if err := a.setNext(Finalizing); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("setNext(Finalizing) from Fetching, error = %v, want ErrIllegalTransition", err)
	}
	if err := a.setNext(StateUnset); err == nil {
		t.Error("setNext(StateUnset) = nil; the sentinel is not a destination")
	}
}

// TestAttempt_TransitionClearsNext pins §3.3 rule 3: the move consumes the
// marker, so an attempt that never re-enters Assessing cannot carry a stale
// verdict for the rest of its life.
func TestAttempt_TransitionClearsNext(t *testing.T) {
	a := newAttempt(testClock())
	if err := a.setNext(Assessing); err != nil {
		t.Fatalf("setNext: %v", err)
	}
	if err := a.transition(Assessing); err != nil {
		t.Fatalf("transition(Assessing): %v", err)
	}
	if a.next != StateUnset {
		t.Errorf("next = %v after the move was taken, want StateUnset", a.next)
	}
}

// TestAttempt_TransitionRequiresNextWhenSet pins the single-decider property.
// From Assessing, legalEdges permits Fetching, Repairing and Extracting; once a
// verdict is recorded, nothing else may choose. This is transition's to == next
// check, whose ONLY remaining purpose this is now that Waiting is gone.
func TestAttempt_TransitionRequiresNextWhenSet(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, a, Assessing)
	if err := a.setNext(Repairing); err != nil {
		t.Fatalf("setNext(Repairing): %v", err)
	}
	if err := a.transition(Extracting); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("transition(Extracting) against a Repairing verdict, error = %v, want ErrIllegalTransition; "+
			"Assessing is the only decider and its verdict must not be bypassable", err)
	}
	if err := a.transition(Repairing); err != nil {
		t.Errorf("transition(Repairing) matching the verdict: %v", err)
	}
}

// TestAttempt_TransitionAcceptsAnyLegalEdgeWhenNextIsUnset pins the other half:
// with no verdict recorded, the edge map alone decides.
func TestAttempt_TransitionAcceptsAnyLegalEdgeWhenNextIsUnset(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, a, Assessing)
	if a.next != StateUnset {
		t.Fatalf("next = %v, want StateUnset", a.next)
	}
	if err := a.transition(Repairing); err != nil {
		t.Errorf("transition(Repairing) with no verdict recorded: %v", err)
	}
}
```

Add the helper if `attempt_test.go` lacks one:

```go
func mustTransition(t *testing.T, a *Attempt, to State) {
	t.Helper()
	if err := a.transition(to); err != nil {
		t.Fatalf("transition(%v): %v", to, err)
	}
}
```

- [ ] **Step 2: Write the failing tests for the removals**

Append to `internal/job/job_test.go`:

```go
// TestJob_NeverRunReportsStateUnset replaces
// TestJob_NeverRunReportsWaitingForALease. A job with no attempt is not at a
// state; StateUnset says exactly that, where the old Waiting{Next: Fetching}
// claimed a position the job had not reached.
func TestJob_NeverRunReportsStateUnset(t *testing.T) {
	j := newTestJob(t)
	v := j.State()
	if v.State != StateUnset {
		t.Errorf("State() on a never-run job = %v, want StateUnset", v.State)
	}
	if v.Next != StateUnset {
		t.Errorf("Next = %v on a never-run job, want StateUnset: nothing has ended, so nothing is pending", v.Next)
	}
	if j.HasRun() {
		t.Error("HasRun() is true for a job with no attempt")
	}
}

// TestJob_SetNextRequiresAnOpenAttempt pins that the marker cannot be written
// before there is an attempt to carry it.
func TestJob_SetNextRequiresAnOpenAttempt(t *testing.T) {
	j := newTestJob(t)
	if err := j.SetNext(Assessing); !errors.Is(err, ErrNoOpenAttempt) {
		t.Errorf("SetNext on a never-run job, error = %v, want ErrNoOpenAttempt", err)
	}
}
```

Replace `TestStateView_ZeroValueIsWaitingForALease` in `internal/job/wait_test.go`:

```go
// TestStateView_ZeroValueIsUnset pins that a zero view names no state. It is
// the reason StateUnset exists: without it the zero would be Fetching, and an
// unstarted job would read as an active download.
func TestStateView_ZeroValueIsUnset(t *testing.T) {
	var v StateView
	if v.State != StateUnset {
		t.Errorf("zero StateView.State = %v, want StateUnset", v.State)
	}
	if v.Next != StateUnset {
		t.Errorf("zero StateView.Next = %v, want StateUnset", v.Next)
	}
}
```

- [ ] **Step 3: Run them and watch them fail**

```bash
go test -count=1 -run 'TestAttempt_SetNext|TestAttempt_Transition|TestJob_NeverRun|TestJob_SetNext|TestStateView_Zero' ./internal/job/
```

Expected: FAIL to compile — `undefined: ErrNextAlreadySet`, `a.setNext undefined`, `j.SetNext undefined`.

- [ ] **Step 4: Narrow the state enum**

In `internal/job/state.go`, delete the `Waiting` constant and its `String` arm, and remove it from `AllStates()`. The block becomes `StateUnset, Fetching, Assessing, Repairing, Extracting, Finalizing, Finished`; `AllStates()` returns the six real states.

- [ ] **Step 5: Narrow `legalEdges` and drop the self-edge**

Replace the map and `CanTransition` in `internal/job/transition.go`:

```go
// legalEdges is the lifecycle as a directed graph: the six-edge WORK SPINE, and
// nothing else. Pause and resume are not edges — a job that is not running is
// still at the state it last occupied (design §3.2) — and cancellation is not
// an edge either, because finish is its own door and never consults this map
// (`sed -n '/func (a \*Attempt) finish/,/^}/p' internal/job/attempt.go | grep
// CanTransition` returns nothing).
//
// Exactly one of these crosses the irreversible boundary: Assessing →
// Extracting. That is why Cross owns one EDGE rather than a state class.
var legalEdges = map[State][]State{
	Fetching:   {Assessing},
	Assessing:  {Fetching, Repairing, Extracting},
	Repairing:  {Assessing},
	Extracting: {Finalizing},
	Finalizing: {},
	Finished:   {},
}

// CanTransition reports whether a job may move from → to.
//
// Self-transitions are NOT legal. The previous from == to early return existed
// partly to keep hold's Finalizing case reachable, and hold is gone; no door
// requests one now, and leaving it would permit a legal no-op that clears
// Activity and nothing else.
func CanTransition(from, to State) bool {
	return slices.Contains(legalEdges[from], to)
}
```

- [ ] **Step 6: Rewrite `Attempt`**

In `internal/job/attempt.go`:

1. **Delete** the `reason` field, `hold`, `setReason`, `ErrHoldRequired` and `ErrNotWaiting`.
2. **Add** the sentinels:

```go
// ErrNextAlreadySet is returned by setNext when a different destination is
// already recorded. This is defect 3's guard, carried into the door that
// replaced hold: without it a verdict of Repairing could be overwritten with
// Extracting and the job would cross the boundary skipping repair.
var ErrNextAlreadySet = errors.New("job: next is already set to a different destination")
```

3. **Add** `setNext`:

```go
// setNext records that this state's work has ENDED and where the job continues
// to. It is the marker Waiting used to carry, and the fact it carries is not
// derivable: "has this download finished?" is about the world, not the graph.
//
// Ended, not succeeded — a Fetching that exhausts every server has ended, and
// Assessing decides what that means. Work that ends without continuing settles
// via finish instead and leaves next unset, which is why Finalizing never sets
// it and is not an exception to the rule.
//
// Three guards, each closing a specific hole:
//   - the destination must be a legal edge from the current state;
//   - the sentinel is not a destination;
//   - write-once per visit: a DIFFERENT destination is refused, the same one
//     is a no-op. transition clears next when it takes the move, so re-entering
//     a state permits a fresh verdict.
func (a *Attempt) setNext(n State) error {
	if n == StateUnset {
		return fmt.Errorf("%w: StateUnset is not a destination", ErrIllegalTransition)
	}
	if !CanTransition(a.state, n) {
		return illegalTransition(a.state, n)
	}
	if a.next != StateUnset && a.next != n {
		return fmt.Errorf("%w: %s is recorded, refusing to replace it with %s", ErrNextAlreadySet, a.next, n)
	}
	a.next = n
	return nil
}
```

4. **Rewrite** `transition`:

```go
// transition moves the attempt to `to`. Activity is cleared, because it
// describes the state being left. next is cleared, because the move consumes it.
//
// When next is SET, to must equal it. That check no longer guards the boundary
// — with Waiting gone, legalEdges does that alone — and its sole remaining
// purpose is enforcing that once a state's work has decided where to go,
// nothing else may choose. From Assessing, legalEdges permits Fetching,
// Repairing and Extracting, so without this check a caller could ignore a
// NeedsRepair verdict and cross into Production having skipped repair.
//
// When next is UNSET the edge map alone decides, which is the ordinary
// forward move of a state that has just started.
func (a *Attempt) transition(to State) error {
	if to == Finished {
		return ErrFinishRequired
	}
	if to == StateUnset {
		return fmt.Errorf("%w: StateUnset is not a destination", ErrIllegalTransition)
	}
	if a.next != StateUnset && to != a.next {
		return illegalTransition(a.state, to)
	}
	if !CanTransition(a.state, to) {
		return illegalTransition(a.state, to)
	}
	a.state = to
	a.activity = ActNone
	a.next = StateUnset
	if to == Assessing {
		a.assessed = true
	}
	if IsProduction(to) {
		a.crossed = true
	}
	return nil
}
```

5. **Update** `finish` to drop `a.next = Waiting; a.reason = NoLease` and instead clear `a.next = StateUnset`. Leave the rest, including the `a.crossed` guard, untouched.
6. **Update** `view()` to drop `Reason`.

- [ ] **Step 7: Rewrite `Job`**

In `internal/job/job.go`:

1. **Delete** the `pending` field, `Hold`, `SetWaitReason`, and `ErrBoundaryConsumed`'s reference to `Waiting` if any.
2. **Rewrite** `New` to drop `pending: NoLease`.
3. **Rewrite** `State()`:

```go
// State returns the current attempt's view, or a StateUnset view for a job that
// has never run. A job with no attempt is not AT a state — the old model
// answered Waiting{Next: Fetching}, which claimed a position the job had not
// reached. HasRun() distinguishes the two cases for a caller that needs to.
func (j *Job) State() StateView {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if a := j.currentLocked(); a != nil {
		return a.view()
	}
	return StateView{}
}
```

4. **Add** the door:

```go
// SetNext records that the open attempt's current state has finished its work,
// and where it continues to. See Attempt.setNext.
func (j *Job) SetNext(n State) error {
	return j.withOpenAttempt(func(a *Attempt) error { return a.setNext(n) })
}
```

- [ ] **Step 8: Drop `Reason` from `StateView`**

In `internal/job/wait.go`, delete the `Reason` field and update the doc comment: `Next` is meaningful when set and means the current state's work has ended; the wait reason now lives on `RenderView` (task 4), because it is derived by the Queue rather than stored.

**Sweep the whole `StateView` doc comment, not only the sentence about `Reason`.** Two other claims in that block are falsified by this task and neither contains the word `Reason`:

- "Next and Reason are meaningful only when State is Waiting" — `Waiting` no longer exists, and `Next`'s meaning is now "this state's work has ended", which is the redefinition this very step makes.
- The zero-value paragraph names `job.go:120` and says the never-run case is `Waiting{Next: Fetching}`. Step 7 of this task changes that line to return `StateUnset`, so the paragraph must lose the exception it describes — the zero value and the never-run shape become the same thing.

The same forward reference exists in `state.go`'s `StateUnset` block ("Job.State() does NOT yet return it for a job with no attempt"). That sentence was true when task 1 wrote it and is falsified here; delete it and say the sentinel IS the never-run shape.

Keep `WaitReason`, `AllWaitReasons` and `IsPause` — `RenderView` uses them.

- [ ] **Step 9: Delete the tests of deleted behaviour**

Remove these outright. They test doors that no longer exist; do not adapt them.

| File | Delete |
|---|---|
| `attempt_test.go` | `TestAttempt_HoldRecordsNextAndReason`, `TestAttempt_HoldRejectsAfterFinish`, `TestAttempt_HoldRejectsAnIllegalDestination`, `TestAttempt_HoldRejectsResumeIntoFinished`, `TestAttempt_HoldRejectsResumeIntoWaiting`, `TestAttempt_HoldRejectsWhenAlreadyWaiting`, `TestAttempt_SetReasonRejectsWhenNotWaiting`, `TestAttempt_SetReasonUpdatesReasonOnly`, `TestAttempt_TransitionFromWaitingRequiresNext`, `TestAttempt_TransitionRejectsWaiting`, `TestAttempt_BoundaryHoldsAcrossAHold`, `TestAttempt_FinishRejectsUnrecoverableAfterCrossingThenHold`, `TestAttempt_FinishClearsNextAndReason` (replaced by `TestAttempt_TransitionClearsNext` plus a `finish` assertion) |
| `job_test.go` | `TestJob_SetWaitReasonOnNeverRunJob`, `TestJob_SetWaitReasonOnParkedAttempt`, `TestJob_SetWaitReasonRejectsMidWork`, `TestJob_SetWaitReasonRejectsSettledJob`, `TestJob_SetWaitReasonCannotChangeNext`, `TestJob_BeginAttemptAfterSetWaitReasonOnNeverRunJob`, `TestJob_NeverRunReportsWaitingForALease`, `TestJob_TransitionSurfacesHoldAndFinishDoors` (rewrite to cover `ErrFinishRequired` only) |
| `transition_test.go` | `TestEdgeCountsMatchTheStatedPartition` — the partition collapses to one bucket. Replace with a direct assertion (step 10). |
| `wait_test.go` | `TestStateView_ZeroValueIsWaitingForALease` (replaced in step 2) |

- [ ] **Step 10: Replace the edge-count test**

In `internal/job/transition_test.go`:

```go
// TestLegalEdgesIsTheWorkSpine asserts the graph's exact contents. The previous
// partition test classified edges into cancel/pause/resume/spine buckets; with
// Waiting and the -> Finished edges gone there is one bucket, so a partition
// rule would be a tautology. A literal is honest at this size and fails loudly
// when an edge moves.
func TestLegalEdgesIsTheWorkSpine(t *testing.T) {
	want := map[State][]State{
		Fetching:   {Assessing},
		Assessing:  {Fetching, Repairing, Extracting},
		Repairing:  {Assessing},
		Extracting: {Finalizing},
		Finalizing: {},
		Finished:   {},
	}
	if len(legalEdges) != len(want) {
		t.Fatalf("legalEdges has %d sources, want %d", len(legalEdges), len(want))
	}
	for from, wantTo := range want {
		if !slices.Equal(legalEdges[from], wantTo) {
			t.Errorf("legalEdges[%v] = %v, want %v", from, legalEdges[from], wantTo)
		}
	}
	var n int
	for _, to := range legalEdges {
		n += len(to)
	}
	if n != 6 {
		t.Errorf("legalEdges has %d edges, want 6 (the work spine)", n)
	}

	// Exactly one edge crosses the boundary. Cross owns that ONE edge, which is
	// only proportionate if there is only one.
	var crossings int
	for from, tos := range legalEdges {
		for _, to := range tos {
			if IsCorrectness(from) && IsProduction(to) {
				crossings++
			}
		}
	}
	if crossings != 1 {
		t.Errorf("legalEdges has %d Correctness->Production edges, want exactly 1 (Assessing->Extracting)", crossings)
	}
}

// TestCanTransition_NoSelfEdges pins the removal of the from == to arm.
func TestCanTransition_NoSelfEdges(t *testing.T) {
	for _, s := range AllStates() {
		if CanTransition(s, s) {
			t.Errorf("CanTransition(%v, %v) is true; self-transitions are not edges and no door requests one", s, s)
		}
	}
}
```

- [ ] **Step 11: Fix the remaining compile errors and run everything**

```bash
goimports -w internal/job/ && go build ./... && go vet ./internal/job/
go test -race -count=1 ./internal/job/
```

`internal/job/reachability_test.go` will not compile: its action set names `Hold` and its `configKey` reads `a.reason`, both of which this task deletes. Make the **minimal** edit to restore green — delete the `Hold` action, and drop `a.reason` from the key — and leave everything else alone. Task 7 rebuilds the oracle and adds the new actions; splitting it that way keeps this commit's diff to the model change and gives task 7 a working test to start from rather than a commented-out one.

After that edit:

```bash
go test -race -count=1 ./internal/job/
```

Expected: PASS, with the walk reaching fewer configurations than before — `Waiting` and its eleven edges are gone. If it drops below the `configs < 50` floor, investigate rather than lowering the floor.

- [ ] **Step 12: Prove the write-once guard discriminates**

This is defect 3's pin and the one behavioural guard this task adds:

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/job/attempt.go "$SCRATCH/attempt.bak.go"
sed -i 's|if a.next != StateUnset \&\& a.next != n {|if false \&\& a.next != StateUnset \&\& a.next != n {|' internal/job/attempt.go
grep -n 'if false && a.next != StateUnset' internal/job/attempt.go
go test -count=1 -run TestAttempt_SetNextIsWriteOncePerVisit ./internal/job/   # MUST fail
cp "$SCRATCH/attempt.bak.go" internal/job/attempt.go
git diff --stat internal/job/attempt.go   # MUST be empty
```

Then the same for `transition`'s `to == next` check against `TestAttempt_TransitionRequiresNextWhenSet`. **Revert each separately** — one being pinned says nothing about the other. Record both failure messages.

- [ ] **Step 13: Run the gates and commit**

```bash
golangci-lint run ./internal/job/
git add internal/job/
git commit -m "feat(job)!: remove Waiting; next becomes the end-of-work marker

Waiting was carrying two facts: where the job goes next, and whether the
current state's work has ended. The first is usually derivable from the edge
map. The second never is - \"has this download finished?\" is a fact about the
world, not about the graph - and that is what next now carries:

  next is set <=> the current state's work has ENDED and the job continues
  to another work state.

Ended, not succeeded. A Fetching that exhausts every server has ended, and
Assessing decides what that means. Finalizing is not an exception: its work
ends, it continues nowhere, so it settles via finish and leaves next unset.

A job that is not running is still at the state it last occupied, with
Activity ActNone - which is what that axis already means everywhere else.

Deleted: Waiting, hold, setReason, Job.Hold, Job.SetWaitReason, Job.pending,
Attempt.reason, StateView.Reason, ErrHoldRequired, ErrNotWaiting, and
CanTransition's from == to arm. legalEdges narrows from 22 edges to the
six-edge work spine; exactly one crosses the boundary.

Defects 1 and 2 from #439 become unrepresentable - there is no Waiting to
transition into, and no Waiting node to launder Extracting -> Fetching
through. Defect 3 stays GUARDED, not unrepresentable: deleting hold removes
one door for re-declaring a destination and setNext is a new one, closed by
write-once-per-visit.

BREAKING CHANGE: State loses Waiting and StateView loses Reason. Nothing
imports this package yet (git grep -ln 'gonzbd/internal/job' -- '*.go' |
grep -v '^internal/job/' returns nothing), so there is no caller to migrate."
```

---

## Task 6: `Cross` — a door for the one boundary edge

**Files:**
- Modify: `internal/job/attempt.go`, `internal/job/job.go`
- Test: `internal/job/attempt_test.go`, `internal/job/job_test.go`

> **Do not change `BeginAttempt`.** Spec §3.5 and D-I12 discuss it at length,
> because revision 3 changed it to `BeginAttempt(l *Lease, now)` and revision 4
> reverted that. The signature on `main` is already
> `func (j *Job) BeginAttempt(now time.Time) error` — verified with
> `grep -n 'func (j \*Job) BeginAttempt' internal/job/job.go`. D-I12 is
> therefore satisfied by **doing nothing**. Adding the parameter would
> reintroduce the retry deadlock and the orphaned-lease paths that decision
> exists to prevent.

**Interfaces:**
- Consumes: `Lease`, `surrenderLocked` (task 3); `setNext`, narrowed `legalEdges` (task 5). `BeginAttempt` unchanged.
- Produces: `func (j *Job) Cross(to State) (*Lease, error)`, `func (j *Job) Finish(o Outcome, now time.Time) (*Lease, error)` — **signature change**. `transition` now refuses `IsCorrectness(from) && IsProduction(to)`.

Spec §3.5 and §3.9. There is exactly one Correctness→Production edge in the spine (task 5, step 10 asserts it): `Assessing → Extracting`. Doing it as two calls reproduces defect 5's shape — forget the surrender and a pool-A slot leaks **permanently and silently**; do them in the other order and the job sits in `Assessing` holding no lease that `Assessing` requires.

`Finish` gains the same signature for the same reason: §3.9's table shows revision 3 leaking a lease on **every** pre-boundary failure, every `Unrecoverable` verdict and every cancel, because `Finish` yielded nothing and `advance` skips settled attempts.

- [ ] **Step 1: Write the failing tests**

Append to `internal/job/attempt_test.go`:

```go
// TestJob_CrossYieldsTheLeaseAtomically pins §3.5: state, crossed, next and the
// lease all move in ONE call. As two calls this is defect 5's shape — forgetting
// the surrender leaks a pool-A slot permanently and silently.
func TestJob_CrossYieldsTheLeaseAtomically(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)
	l := &Lease{}
	if err := j.Grant(l); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := j.Transition(Assessing); err != nil {
		t.Fatalf("Transition(Assessing): %v", err)
	}
	if err := j.SetNext(Extracting); err != nil {
		t.Fatalf("SetNext(Extracting): %v", err)
	}

	got, err := j.Cross(Extracting)
	if err != nil {
		t.Fatalf("Cross(Extracting): %v", err)
	}
	if got != l {
		t.Errorf("Cross yielded %p, want the held lease %p", got, l)
	}
	if j.HoldsLease() {
		t.Error("the job still holds a lease after crossing; Extracting holds only a compute slot")
	}
	v := j.State()
	if v.State != Extracting {
		t.Errorf("State = %v, want Extracting", v.State)
	}
	if v.Next != StateUnset {
		t.Errorf("Next = %v after the move was taken, want StateUnset", v.Next)
	}
}

// TestJob_CrossEnforcesTheVerdict pins that Cross is not a hole in the
// single-decider property transition's to == next check protects. Without it a
// caller could cross from anywhere, to anywhere in Production, ignoring the
// verdict Assessing recorded.
func TestJob_CrossEnforcesTheVerdict(t *testing.T) {
	t.Run("refuses a destination the verdict did not name", func(t *testing.T) {
		j := newTestJob(t)
		mustBegin(t, j)
		mustJobTransition(t, j, Assessing)
		if err := j.SetNext(Repairing); err != nil {
			t.Fatalf("SetNext(Repairing): %v", err)
		}
		if _, err := j.Cross(Extracting); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("Cross(Extracting) against a Repairing verdict, error = %v, want ErrIllegalTransition", err)
		}
	})
	t.Run("refuses from a state that is not Assessing", func(t *testing.T) {
		j := newTestJob(t)
		mustBegin(t, j) // Fetching
		if _, err := j.Cross(Extracting); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("Cross(Extracting) from Fetching, error = %v, want ErrIllegalTransition", err)
		}
	})
	t.Run("refuses a non-Production destination", func(t *testing.T) {
		j := newTestJob(t)
		mustBegin(t, j)
		mustJobTransition(t, j, Assessing)
		if _, err := j.Cross(Repairing); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("Cross(Repairing), error = %v, want ErrIllegalTransition; Cross owns the boundary edge only", err)
		}
	})
}

// TestJob_CrossYieldsNilWhenNoLeaseIsHeld pins the case §3.9 calls out: a job
// may legitimately reach the crossing holding nothing, having been paused at
// Assessing{next: Extracting} and resumed. Cross must report that rather than
// assert, so the Queue's sole reclaimer can no-op on nil.
func TestJob_CrossYieldsNilWhenNoLeaseIsHeld(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)
	mustJobTransition(t, j, Assessing)
	if err := j.SetNext(Extracting); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	got, err := j.Cross(Extracting)
	if err != nil {
		t.Fatalf("Cross with no lease held: %v", err)
	}
	if got != nil {
		t.Errorf("Cross yielded %p, want nil", got)
	}
	if j.State().State != Extracting {
		t.Error("the crossing did not happen; holding no lease must not block it")
	}
}

// TestJob_TransitionRefusesTheBoundaryEdge pins that Cross is the SOLE door.
func TestJob_TransitionRefusesTheBoundaryEdge(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)
	mustJobTransition(t, j, Assessing)
	if err := j.SetNext(Extracting); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if err := j.Transition(Extracting); !errors.Is(err, ErrCrossRequired) {
		t.Errorf("Transition(Extracting), error = %v, want ErrCrossRequired", err)
	}
	if j.State().State != Assessing {
		t.Error("the refused Transition moved the attempt anyway")
	}
}

// TestJob_FinishYieldsTheLease pins §3.9's largest leak: revision 3's Finish
// yielded nothing, so every pre-boundary failure, every Unrecoverable verdict
// and every cancel lost a pool-A slot until restart.
func TestJob_FinishYieldsTheLease(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)
	l := &Lease{}
	if err := j.Grant(l); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	got, err := j.Finish(OutcomeFailed, testClock())
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got != l {
		t.Errorf("Finish yielded %p, want the held lease %p", got, l)
	}
	if j.HoldsLease() {
		t.Error("the job still holds a lease after settling")
	}
}

// TestJob_CrossAndFinishDoNotDeadlock is the pin for the reason surrenderLocked
// exists. withOpenAttempt takes j.mu and holds it across its callback, and
// sync.RWMutex is not reentrant: a door calling the exported Surrender() from
// there would hang the job permanently, with no error and no timeout. A
// deadlocked test does not fail, it hangs, so this runs under a watchdog.
func TestJob_CrossAndFinishDoNotDeadlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Job) error
	}{
		{"Finish", func(j *Job) error { _, err := j.Finish(OutcomeOK, testClock()); return err }},
		{"Cross", func(j *Job) error {
			mustJobTransition(t, j, Assessing)
			if err := j.SetNext(Extracting); err != nil {
				return err
			}
			_, err := j.Cross(Extracting)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := newTestJob(t)
			mustBegin(t, j)
			if err := j.Grant(&Lease{}); err != nil {
				t.Fatalf("Grant: %v", err)
			}
			done := make(chan error, 1)
			go func() { done <- tc.run(j) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("%s: %v", tc.name, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s did not return within 5s — it is almost certainly taking j.mu twice; "+
					"the doors must call surrenderLocked, not the exported Surrender", tc.name)
			}
		})
	}
}
```

Add the helper to `job_test.go` if absent:

```go
func mustJobTransition(t *testing.T, j *Job, to State) {
	t.Helper()
	if err := j.Transition(to); err != nil {
		t.Fatalf("Transition(%v): %v", to, err)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

```bash
go test -count=1 -run 'TestJob_Cross|TestJob_Finish|TestJob_Transition' ./internal/job/
```

Expected: FAIL to compile — `j.Cross undefined`, `ErrCrossRequired` undefined, and `Finish` returning one value where two are assigned.

- [ ] **Step 3: Add `ErrCrossRequired` and `cross` to `internal/job/attempt.go`**

```go
// ErrCrossRequired is returned when transition is asked to take the one
// Correctness -> Production edge. Cross is the sole door across the
// irreversible boundary, because entering Production and surrendering the
// lease must happen together — see Job.Cross.
var ErrCrossRequired = errors.New("job: transition cannot cross the boundary; call Cross instead")

// cross moves the attempt across the irreversible boundary. Must hold j.mu;
// the lease is released by the CALLER through surrenderLocked, which is why
// this returns nothing about it.
//
// It validates exactly what transition would have, and both checks matter:
// without them Cross would be a hole in the single-decider property that
// transition's to == next check exists to protect — a caller could cross from
// anywhere, to anywhere in Production, ignoring the verdict Assessing recorded.
func (a *Attempt) cross(to State) error {
	if !IsProduction(to) {
		return fmt.Errorf("%w: %s is not a Production state; cross owns the boundary edge only", ErrIllegalTransition, to)
	}
	if a.state != Assessing {
		return fmt.Errorf("%w: cross is legal only from Assessing, not %s", ErrIllegalTransition, a.state)
	}
	if a.next != to {
		return fmt.Errorf("%w: %s is recorded; cross cannot take %s instead", ErrIllegalTransition, a.next, to)
	}
	if !CanTransition(a.state, to) {
		return illegalTransition(a.state, to)
	}
	a.state = to
	a.activity = ActNone
	a.next = StateUnset
	a.crossed = true
	return nil
}
```

In `transition`, add above the `CanTransition` check:

```go
	if IsCorrectness(a.state) && IsProduction(to) {
		return ErrCrossRequired
	}
```

Remove the `if IsProduction(to) { a.crossed = true }` block from `transition` — `cross` is now the sole writer of `crossed`.

- [ ] **Step 4: Add `Job.Cross` and change `Job.Finish`**

In `internal/job/job.go`:

```go
// Cross is the sole door across the irreversible boundary. It sets state,
// latches crossed, clears next and yields the lease in ONE call — there is no
// way to do one without the others.
//
// The lease may be nil: a job can legitimately reach the crossing holding
// none, having been paused at Assessing{next: Extracting} and resumed. It does
// not need one to cross, because crossing is where it would have given the
// lease up anyway. The Queue's reclaimer no-ops on nil rather than each caller
// testing for it.
//
// Not routed through withOpenAttempt, because it must surrender under the same
// lock it mutates the attempt under — see surrenderLocked for why calling the
// exported Surrender from a locked door deadlocks.
func (j *Job) Cross(to State) (*Lease, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	a := j.currentLocked()
	if a == nil || !a.isOpen() {
		return nil, ErrNoOpenAttempt
	}
	if err := a.cross(to); err != nil {
		return nil, err
	}
	return j.surrenderLocked(), nil
}

// Finish assigns the verdict, closes the open attempt, and yields the lease if
// one is held.
//
// It yields the lease because every settling path ends the job's need for it:
// a pre-boundary failure, an Unrecoverable verdict from Assessing, and a
// cancel all reach Finished without passing through Cross. An earlier design
// returned only an error, and leaked a pool-A slot on all three.
func (j *Job) Finish(o Outcome, now time.Time) (*Lease, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	a := j.currentLocked()
	if a == nil || !a.isOpen() {
		return nil, ErrNoOpenAttempt
	}
	if err := a.finish(o, now); err != nil {
		return nil, err
	}
	return j.surrenderLocked(), nil
}
```

- [ ] **Step 5: Update every `Finish` call site in tests**

`grep -rn '\.Finish(' internal/job/*_test.go` and change each to `_, err := j.Finish(...)` or `if _, err := j.Finish(...); err != nil`. Do not leave any bare `j.Finish(...)` — `golangci-lint` will flag the unused return.

- [ ] **Step 6: Run everything**

```bash
goimports -w internal/job/ && go build ./... && go vet ./internal/job/
go test -race -count=1 -timeout 60s ./internal/job/
```

Expected: PASS. The `-timeout 60s` is deliberate — if `surrenderLocked` was wired wrong the deadlock test hangs, and a short timeout turns that into a readable failure instead of a stalled run.

- [ ] **Step 7: Prove the deadlock test discriminates**

This is the only test in the plan that catches a hang rather than a wrong value, so it must be shown to work:

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/job/job.go "$SCRATCH/job.bak.go"
sed -i 's|return j.surrenderLocked(), nil|return j.Surrender(), nil|g' internal/job/job.go
grep -c 'return j.Surrender(), nil' internal/job/job.go   # expect 2
go test -count=1 -timeout 30s -run TestJob_CrossAndFinishDoNotDeadlock ./internal/job/   # MUST fail on the watchdog
cp "$SCRATCH/job.bak.go" internal/job/job.go
git diff --stat internal/job/job.go   # MUST be empty
```

Expected failure: the 5s watchdog fires for both subtests. Record it.

- [ ] **Step 8: Run the gates and commit**

```bash
golangci-lint run ./internal/job/
git add internal/job/
git commit -m "feat(job)!: give the boundary its own door, and make settling yield the lease

There is exactly one Correctness -> Production edge in the six-edge spine:
Assessing -> Extracting. Doing it as two calls reproduces defect 5's shape -
forget the surrender and a pool-A slot leaks permanently and SILENTLY; do
them in the other order and the job sits in Assessing holding no lease that
Assessing requires. Cross does both in one call and is now the sole writer
of crossed; transition refuses the edge.

Cross validates what transition would have - Assessing, and to == next -
because otherwise it would be a hole in exactly the single-decider property
transition's check protects: a caller could cross from anywhere to anywhere
in Production, ignoring the verdict.

Finish now yields the lease too. Every settling path ends the job's need for
it - a pre-boundary failure, an Unrecoverable verdict, a cancel - and none
passes through Cross. Returning only an error leaked a slot on all three.

Both call surrenderLocked, not Surrender: withOpenAttempt holds j.mu across
its callback and sync.RWMutex is not reentrant, so the exported form would
hang the job permanently with no error and no timeout.

BREAKING CHANGE: Job.Finish returns (*Lease, error).

Note what this does NOT prove: a caller may still drop the *Lease either
door returns, and no test in this package can see that. Cross makes the leak
one call site, not unrepresentable."
```

---

## Task 7: Rebuild the whole-space tests, and sweep the prose

**Files:**
- Modify: `internal/job/reachability_test.go` (rewritten), `internal/job/outcome_writer_enumeration_test.go`, `internal/job/attempts_writer_enumeration_test.go`, `internal/job/doc.go`, `internal/job/transition_test.go`
- Create: `internal/job/writer_enumeration_test.go`

**Interfaces:**
- Consumes: everything from tasks 1–6.
- Produces: no new production symbols. This task is the enforcement layer.

Spec §6. Three jobs: give the reachability walk an **independent oracle**, extend the writer enumerations to the fields this plan added, and sweep the package prose against the new model.

### 7a. The reachability walk needs a new oracle

`reachability_test.go` currently judges with `IsCorrectness`, and its own doc comment states the condition under which that stops being legitimate:

> *"if a door ever starts branching on `IsCorrectness`, this test needs a different oracle."*

**Task 6 creates that condition.** `transition` refuses `IsCorrectness(from) && IsProduction(to)` and `cross` is selected by the same predicate. Judging those doors with the predicate they decide by means a wrong predicate agrees with itself — the exact failure the comment was written to prevent.

- [ ] **Step 1: Replace the oracle with a literal set**

In `internal/job/reachability_test.go`, add above `checkReachableConfig`:

```go
// correctnessStates is the reachability walk's ORACLE, written as a literal
// rather than derived from IsCorrectness. That is not stylistic. The doors this
// test judges now branch on IsCorrectness themselves — transition refuses
// IsCorrectness(from) && IsProduction(to), and cross is selected by it — so
// judging them with the same predicate would let a wrong predicate agree with
// itself. The test's previous doc comment named this exact condition as the
// point at which it would need a different oracle, and this design created it.
//
// A literal cannot drift silently the way a shared predicate can. The guard
// below is what makes ADDING a state fail loudly here rather than quietly
// widening the oracle to a set nobody re-classified.
var correctnessStates = map[State]bool{
	Fetching:  true,
	Assessing: true,
	Repairing: true,
}

// TestReachabilityOracleClassifiesEveryState fails when AllStates() grows a
// member correctnessStates does not name. Without it, a new state defaults to
// "not Correctness" and the boundary walk silently stops checking it.
func TestReachabilityOracleClassifiesEveryState(t *testing.T) {
	productionStates := map[State]bool{Extracting: true, Finalizing: true}
	for _, s := range AllStates() {
		if s == Finished {
			continue
		}
		if !correctnessStates[s] && !productionStates[s] {
			t.Errorf("%s is in AllStates() but classified by neither the oracle's Correctness set "+
				"nor its Production set; classify it deliberately rather than letting it default", s)
		}
	}
}
```

Then in `checkReachableConfig`, replace `IsCorrectness(v.State)` with `correctnessStates[v.State]`, and update the doc comment's independence paragraph to cite the literal instead of the grep.

- [ ] **Step 2: Update the action set and the config key**

`Hold` is gone; `SetIntent`, `SetNext` and `Cross` are new. In `allActions()`:

- **Remove** the `Hold(s)` action.
- **Add** `SetNext(s)` for each `s` in `AllStates()`.
- **Add** `Cross(s)` for each `s` in `AllStates()` — the walk must be able to attempt an illegal crossing, not only a legal one.
- **Add** `SetIntent(i)` for each `i` in `AllIntents()`, replacing the single `SetWaitReason` action.
- **Change** `BeginAttempt` to the lease-free signature, and add a `Grant` action so leased and unleased configurations are both reachable.

In `configKey`, replace `a.reason` with `j.intent` and `j.lease != nil`:

```go
	key := fmt.Sprintf("n=%d/intent=%v/leased=%v", len(j.attempts), j.intent, j.lease != nil)
	for i := range j.attempts {
		a := &j.attempts[i]
		key += fmt.Sprintf("|%v,%v,%v,%v,%v,%v",
			a.state, a.next, a.activity, a.outcome, a.assessed, a.crossed)
	}
```

Omitting a field here silently prunes real configurations, so this list must cover everything a door reads.

- [ ] **Step 3: Run the walk and check it still reaches a real space**

```bash
go test -race -count=1 -run 'TestBoundaryIsUnreachableByAnyPath|TestReachabilityOracle' -v ./internal/job/
```

Expected: PASS, with the `t.Logf` reporting **at least several hundred** distinct configurations. If the count collapsed, the `configs < 50` floor fires — investigate rather than lowering it.

- [ ] **Step 4: Re-verify the walk by mutation**

The walk is only worth its runtime if it still discriminates. Revert `BeginAttempt`'s crossed refusal and confirm red:

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/job/job.go "$SCRATCH/job.bak.go"
sed -i 's|if a != nil \&\& a.crossed {|if false \&\& a != nil \&\& a.crossed {|' internal/job/job.go
grep -n 'if false && a != nil && a.crossed' internal/job/job.go
go test -count=1 -run TestBoundaryIsUnreachableByAnyPath ./internal/job/   # MUST fail
cp "$SCRATCH/job.bak.go" internal/job/job.go
git diff --stat internal/job/job.go   # MUST be empty
```

Then repeat for `transition`'s `to != a.next` check. Record both messages.

### 7b. Extend the writer enumerations

- [ ] **Step 5: Add enumerations for the three new single-writer fields**

Create `internal/job/writer_enumeration_test.go`.

Two near-identical scanners already exist — `scanOutcomeWriters`
(`outcome_writer_enumeration_test.go:79`) and `scanAttemptsWriters`
(`attempts_writer_enumeration_test.go:51`). Adding four more copies would be
six. **Factor one `scanWriters(t *testing.T, field string) []string`** into the
new file, and rewrite both existing tests to call it. It parses the package's
non-test sources with `go/parser` and collects the enclosing function name for
every assignment to the named field, handling selector assignments, keyed
**and unkeyed** composite literals, compound assignment, inc/dec, and
package-level `var` declarations — the existing scanners already handle all of
these, and dropping a case would silently narrow what the enumeration sees.

```go
// TestCrossedWrites_MatchTheEnumerationStatedInProse asserts cross is the sole
// writer of a.crossed. Before this design, transition wrote it; the boundary
// door now owns it, and the whole argument for Cross existing is that entering
// Production and surrendering the lease cannot be separated. A second writer
// would separate them again.
func TestCrossedWrites_MatchTheEnumerationStatedInProse(t *testing.T) {
	assertSoleWriters(t, "crossed", []string{"cross"})
}

// TestIntentWrites_MatchTheEnumerationStatedInProse asserts SetIntent is the
// sole writer of j.intent, which is what makes the cancel latch a property
// rather than a convention.
func TestIntentWrites_MatchTheEnumerationStatedInProse(t *testing.T) {
	assertSoleWriters(t, "intent", []string{"SetIntent"})
}

// TestNextWrites_MatchTheEnumerationStatedInProse asserts the three writers of
// a.next and no others: setNext records the marker, transition and cross each
// clear it when they take the move. A fourth writer is how a stale verdict
// survives a state change.
func TestNextWrites_MatchTheEnumerationStatedInProse(t *testing.T) {
	assertSoleWriters(t, "next", []string{"setNext", "transition", "cross", "finish"})
}

// TestLeaseWrites_MatchTheEnumerationStatedInProse asserts Grant and
// surrenderLocked are the only writers of j.lease. surrenderLocked being the
// sole RELEASER is what lets Cross and Finish yield without reacquiring j.mu.
func TestLeaseWrites_MatchTheEnumerationStatedInProse(t *testing.T) {
	assertSoleWriters(t, "lease", []string{"Grant", "surrenderLocked"})
}
```

Factor `assertSoleWriters(t *testing.T, field string, want []string)` out of the existing scanner so all four share one implementation. **Guard against a vacuous pass**: if the scan parses zero files or finds zero writers, `t.Fatal` — an empty result reads identically to "nobody writes it".

> **Verify the `finish` entry before committing.** The list above includes
> `finish` for `next` because task 5 step 6 has it clear the field. Run the test;
> if `finish` is not in the observed set, remove it from the want list rather
> than adding a write to satisfy the test.

- [ ] **Step 6: Run them**

```bash
go test -race -count=1 -run 'Writes_MatchTheEnumeration' ./internal/job/
```

Expected: PASS. Each failure message must print the observed set and the wanted set, so a drift is diagnosable without reading the scanner.

### 7c. Sweep the prose

- [ ] **Step 7: Sweep for the literals this plan falsified**

`git grep` is blind to paraphrase, and a doc restates a claim in prose sharing no token with the code. Sweep for the **literals**, from the repository root:

```bash
git grep -n 'Waiting' -- 'internal/job/*.go'
git grep -n 'SetWaitReason\|ErrHoldRequired\|ErrNotWaiting\|\.reason\|pending' -- 'internal/job/*.go'
git grep -n '22 edges\|six-edge\|from == to' -- 'internal/job/*.go' 'docs/*.md'
git grep -n 'internal/job' -- 'docs/ARCHITECTURE.md' 'AGENTS.md'
```

Every surviving hit is either a deliberate historical reference (in a comment explaining *why* something was removed) or a stale claim to fix. There is no third category.

- [ ] **Step 8: Re-read `doc.go` and `docs/ARCHITECTURE.md` in full**

Not grep — read. A sentence can be wrong without containing any of your keywords: `docs/ARCHITECTURE.md` once said *"All message-IDs are validated before use"* and survived a sweep for `validateMessageID`, because it never named the function it described.

`doc.go` currently describes a seven-state machine with `Waiting` as the wait node. Rewrite its state-machine paragraphs against §3 of the spec: four axes, six states, `next` as the end-of-work marker, four doors.

- [ ] **Step 9: Check every comment this plan wrote**

Run `pr-review-toolkit:comment-analyzer` over the cumulative diff — `git diff main...HEAD`. It reads the comments you changed; step 7's grep finds the ones you didn't. They cover different things.

Then re-read each comment against `git diff --cached` as a reader who has not seen this plan. **Sweep against the diff the commit will land as, not the diff that motivated the edit** — that specific failure has shipped three times in this design's own history.

- [ ] **Step 10: Full gates and commit**

```bash
goimports -w internal/job/ && go build ./... && go vet ./...
go test -race -count=1 -timeout 120s ./...
golangci-lint run ./...
go run ./scripts/check_dup_comments && go run ./scripts/check_review_banner
```

`golangci-lint` and the whole-repo test run may report **pre-existing** findings in packages this plan never touched. Diagnose before assuming you caused them: `git stash` is forbidden, so compare against `main` by reading the reported paths against `git diff --name-only main...HEAD`.

```bash
git add internal/job/ && git commit -m "test(job): rebuild the whole-space tests on an independent oracle

The reachability walk judged with IsCorrectness, and its own doc comment
recorded the condition under which that stops being legitimate: \"if a door
ever starts branching on IsCorrectness, this test needs a different oracle.\"
This design created that condition - transition refuses IsCorrectness(from)
&& IsProduction(to), and cross is selected by it - so the oracle is now a
literal set that shares no code with the doors, plus a guard that fails
loudly when AllStates() grows a member nobody classified.

The action set drops Hold and gains SetIntent, SetNext, Cross and Grant, so
leased and unleased configurations are both reachable; the config key gains
intent and lease-heldness, because a field omitted there silently prunes
real configurations.

Writer enumerations added for crossed (cross), intent (SetIntent), next and
lease (Grant, surrenderLocked). Where a population is enumerable by a
machine, the test is the enforcement point and the comment is only a
pointer to it.

Package prose swept against the new model."
```

---

## Imports the new test code needs

`goimports -w internal/job/` resolves these, but they are listed so a compile
error reads as expected rather than as a mistake:

| File | Adds |
|---|---|
| `intent_test.go` | `errors`, `testing` |
| `lease_test.go` | `errors`, `sync`, `testing` |
| `attempt_test.go` | `errors` (if absent), `time` — task 6's deadlock watchdog uses `time.After` |
| `transition_test.go` | `slices` — task 5's `TestLegalEdgesIsTheWorkSpine` uses `slices.Equal` |
| `job.go` | `fmt` — `SetIntent` and `Grant` format their errors |

## Spec sections this plan does NOT implement

Named so their absence reads as deliberate rather than as a gap:

| Spec section | Why not here |
|---|---|
| §3.6 `advance`, §3.7 `Cancel`/`finishCancel` | Half B — they are `Queue` methods and there is no `Queue` |
| §3.8 restart | The persistence contract binds the Checkpointer, which is Half B. Half A's job is to make the fields it names exist and have one writer each, which tasks 1–6 do. |
| §4.7 reorder | Half B — reorder is a `Queue` operation |
| §6.1, §6.2, §6.3 | They test `blockedBy`, `waitReason` purity and `nextMove`, all Half B |
| §5's thirteen scenarios | Most drive `advance` and belong to Half B. The four that are expressible against the doors alone — 5.3's repair loop and crossing, 5.4's two restart variants, 5.8's failed fetch returning the lease — are covered by tasks 5–7's tests. **Half B's plan must carry the rest**, and should say so in its own scope section. |

## Definition of done

- [ ] All seven tasks committed, each building and testing green on its own.
- [ ] `go build ./... && go vet ./... && go test -race -count=1 ./... && golangci-lint run ./...` clean.
- [ ] `go run ./scripts/check_dup_comments` and `go run ./scripts/check_review_banner` clean.
- [ ] `git grep -n 'Waiting' -- 'internal/job/*.go'` returns only deliberate historical references inside comments that explain the removal.
- [ ] `git grep -ln 'gonzbd/internal/job' -- '*.go' | grep -v '^internal/job/'` still returns nothing — Half A must land before anything imports the package.
- [ ] Every mutation check in the plan was **run and observed red**, with the message recorded in its commit body.

## Out of scope — Half B

Do not build these. They are the swap plan's item 3, and the spec assigns them there: `gatedBy`, `waitReason`, `grantFor`, `advance`, `nextMove`, `Cancel`, `finishCancel`, `park`, `discard`, `abortWorker`, `releaseSlot`, the pools, and filling `RenderView`'s three composed fields. Half A leaves the package unable to answer "is this job running" — that is expected and stated in the spec's §4.8, and it is unobservable because nothing imports the package.
