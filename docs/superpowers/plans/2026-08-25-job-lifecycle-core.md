# Job Lifecycle Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `internal/job` — the lifecycle vocabulary (`State`, `Activity`, `Outcome`, `Policy`, `WaitReason`), the transition machine, the `Attempt` record, the `Job` state holder with its own lock, and the total `ToSABnzbd` translation — as new, exhaustively tested code that nothing yet imports.

**Architecture:** Seven states on one axis with `Activity` and write-once `Outcome` beside it. `Assessing` is the only branching state. One irreversible edge separates the Correctness zone (`Fetching`/`Assessing`/`Repairing`) from Production (`Extracting`/`Finalizing`); the machine forbids the reverse. The state machine lives on the current `Attempt`, not on the `Job`, so a retry appends a verdict rather than mutating one.

**Tech Stack:** Go 1.27.0. Standard library only — no testify, no new dependencies. Table-driven tests with `t.Run` subtests.

**Spec:** `docs/superpowers/specs/2026-08-25-job-lifecycle-design.md`

## Global Constraints

- **Go 1.27.0** (toolchain 1.27.0). Module path `github.com/hobeone/gonzbd`.
- **No new external dependencies.** Standard library only for everything in this plan. Adding a dependency requires escalation per AGENTS.md § Decision Protocol.
- **No migration, no compatibility path.** Per Standing Design Rule 1 and the explicit direction for this work: nothing here owes anything to state an earlier build wrote. Do not add guards for old data, do not preserve old shapes, do not write adapters between old and new types.
- **`ToSABnzbd` is not a migration shim.** It is the permanent API translation layer from §12 of the spec. It is the one place `constants.Status` may appear, and it is write-only — nothing reads a `constants.Status` back into the machine.
- **After editing any `.go` file:** `goimports -w <file>`, then `go fix ./...`, then `go build ./...`.
- **Quality gates before every commit:** `go vet ./...`, `go test -race ./internal/job/`, `golangci-lint run ./internal/job/`.
- **Red-green is observed, not reasoned.** Every "run the test and watch it fail" step means actually running it and reading the failure. Use `-count=1` on every mutation check — Go caches passing results and a cached `ok` is not an observation.
- **Commit messages:** Conventional Commits. Scope is `job`. End with `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.

## Scope

**In:** the `internal/job` package — enums, transition table, `WaitState`, `Policy`, `Attempt`, `Job`, `StateView`, `ToSABnzbd`.

**Out, and deferred to the next plan** (each named so nobody wonders whether it was forgotten):

- `Lease`, `Manifest`, `JobProgress` — `Lease` grants a `Manifest`, which lives in `internal/queue` today and must move in the same change as the queue rewrite.
- The `Assessor` and its `Verdict` — goes in `internal/par2` (D5), and needs the `Policy` type this plan produces.
- `Queue`, the two resource pools, lease issuance, the `Checkpointer`.
- Deleting `constants.Status`'s internal use, `queue/status.go`, `JobPhase`, `ActiveSet`, `par2NeedsRecovery`, the `quickcheck` stage, `resumeAllJobs`, `shouldSkipForPP`, `Job.PostProc`.

Nothing in this plan imports `internal/queue`, and nothing in `internal/queue` imports this package yet. The package compiles and tests green on its own from Task 1 onward.

## File Structure

All under `internal/job/`. One file per concept, because these are the types every later plan reads, and a reader looking for "what are the states" should not have to scroll past the translation table.

| File | Responsibility |
|---|---|
| `doc.go` | Package doc: the three axes, the boundary, the single-decider property |
| `state.go` | `State` enum, `AllStates()`, `String()` |
| `transition.go` | `legalEdges`, `CanTransition`, `ErrIllegalTransition`, the boundary assertion |
| `wait.go` | `WaitReason`, `WaitState`, `StateView` |
| `activity.go` | `Activity` enum, `AllActivities()`, `String()` |
| `outcome.go` | `Outcome` enum, `AllOutcomes()`, `String()`, `IsTerminal()` |
| `policy.go` | `Policy`, `PolicyFromPP` |
| `attempt.go` | `Attempt`, its open/close rules |
| `job.go` | `Job` — identity, `policy`, `attempts`, `sync.RWMutex`, all mutators |
| `sabnzbd.go` | `ToSABnzbd` — the only file importing `internal/constants` |

---

### Task 1: The `State` enum

**Files:**
- Create: `internal/job/state.go`
- Test: `internal/job/state_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type State uint8`; constants `Waiting`, `Fetching`, `Assessing`, `Repairing`, `Extracting`, `Finalizing`, `Finished`; `func AllStates() []State`; `func (s State) String() string`.

- [ ] **Step 1: Write the failing test**

Create `internal/job/state_test.go`:

```go
package job

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestState_String(t *testing.T) {
	for _, tc := range []struct {
		s    State
		want string
	}{
		{Waiting, "Waiting"},
		{Fetching, "Fetching"},
		{Assessing, "Assessing"},
		{Repairing, "Repairing"},
		{Extracting, "Extracting"},
		{Finalizing, "Finalizing"},
		{Finished, "Finished"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.s.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestState_StringUnknown(t *testing.T) {
	if got := State(200).String(); got != "State(200)" {
		t.Errorf("String() = %q, want %q", got, "State(200)")
	}
}

// TestAllStates_Exhaustive parses state.go and fails if a declared State
// constant is missing from AllStates(). state.go is the single source of
// truth; AllStates only has to agree with it. Mirrors the same guard
// internal/constants/status_exhaustive_test.go applies to Status — a
// hand-written list is otherwise a second copy of the enum that goes stale
// silently.
func TestAllStates_Exhaustive(t *testing.T) {
	declared := stateConstantsFromSource(t, "state.go")
	if len(declared) == 0 {
		t.Fatal("parsed no State constants from state.go; the parser below no longer matches the file's shape, so this test would pass vacuously")
	}

	listed := make(map[State]bool, len(AllStates()))
	for _, s := range AllStates() {
		listed[s] = true
	}
	for name, value := range declared {
		if !listed[value] {
			t.Errorf("%s is declared in state.go but missing from AllStates(); add it there and give it edges in transition.go", name)
		}
	}
	if len(AllStates()) != len(declared) {
		t.Errorf("AllStates() has %d entries, state.go declares %d; the list has a duplicate or an entry that is no longer declared",
			len(AllStates()), len(declared))
	}
}

// stateConstantsFromSource returns every `Name State = iota`-style constant
// declared in filename, keyed by name, with its resolved value.
func stateConstantsFromSource(t *testing.T, filename string) map[string]State {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}

	found := make(map[string]State)
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		var idx State
		var isStateBlock bool
		for i, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if i == 0 {
				ident, ok := vs.Type.(*ast.Ident)
				isStateBlock = ok && ident.Name == "State"
			}
			if !isStateBlock {
				break
			}
			for _, name := range vs.Names {
				if name.Name == "_" {
					idx++
					continue
				}
				found[name.Name] = idx
				idx++
			}
		}
	}
	return found
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -count=1 ./internal/job/ -run TestState -v`
Expected: FAIL — the package does not exist yet (`no Go files in .../internal/job`).

- [ ] **Step 3: Write minimal implementation**

Create `internal/job/state.go`:

```go
package job

import "fmt"

// State is the position of a job's current attempt in the lifecycle. It
// answers where the job is and what may happen next; what is executing right
// now is Activity, and how the attempt ended is Outcome. Keeping the three
// apart is what collapses the transition table from a fan-out into a graph —
// see docs/superpowers/specs/2026-08-25-job-lifecycle-design.md §3.
//
// The field lives on the current Attempt, not on the Job (§3.1).
type State uint8

const (
	// Waiting holds no lease and no compute slot. It knows where it is going
	// (WaitState.Next) and why it is held (WaitState.Reason); it never
	// decides anything itself.
	Waiting State = iota
	// Fetching is downloading articles. Holds a lease.
	Fetching
	// Assessing decides whether the bytes are correct. Holds a lease and a
	// compute slot. It is the only branching state in the machine.
	Assessing
	// Repairing runs par2 repair. Holds a lease and a compute slot.
	Repairing
	// Extracting decompresses archives. Holds a compute slot. First state
	// past the irreversible boundary.
	Extracting
	// Finalizing renames, cleans, moves and runs the user script. Holds a
	// compute slot.
	Finalizing
	// Finished is terminal. The attempt's Outcome is assigned on the edge
	// into it and never revised.
	Finished
)

// AllStates returns every declared State. TestAllStates_Exhaustive fails if
// this disagrees with the const block above, so a new state cannot be added
// without appearing here.
func AllStates() []State {
	return []State{
		Waiting,
		Fetching,
		Assessing,
		Repairing,
		Extracting,
		Finalizing,
		Finished,
	}
}

func (s State) String() string {
	switch s {
	case Waiting:
		return "Waiting"
	case Fetching:
		return "Fetching"
	case Assessing:
		return "Assessing"
	case Repairing:
		return "Repairing"
	case Extracting:
		return "Extracting"
	case Finalizing:
		return "Finalizing"
	case Finished:
		return "Finished"
	default:
		return fmt.Sprintf("State(%d)", uint8(s))
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `goimports -w internal/job/state.go && go build ./... && go test -count=1 ./internal/job/ -v`
Expected: PASS — `TestState_String`, `TestState_StringUnknown`, `TestAllStates_Exhaustive`.

- [ ] **Step 5: Verify the exhaustiveness test actually discriminates**

This is the observed red check from AGENTS.md. A parser-based test that silently matches nothing passes vacuously, which is the exact failure its own `t.Fatal` guard exists for — prove the guard works.

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/job/state.go "$SCRATCH/state.bak.go"
# Remove Finalizing from AllStates() only — leave the const block alone.
sed -i '/^\t\tFinalizing,$/d' internal/job/state.go
go test -count=1 ./internal/job/ -run TestAllStates_Exhaustive
# MUST fail with: "Finalizing is declared in state.go but missing from AllStates()"
cp "$SCRATCH/state.bak.go" internal/job/state.go
go test -count=1 ./internal/job/ -run TestAllStates_Exhaustive
```

Record the observed failure message in the commit body.

- [ ] **Step 6: Commit**

```bash
git add internal/job/state.go internal/job/state_test.go
git commit -m "feat(job): add the State enum for the new lifecycle

Seven states on one axis. Queued is deliberately absent — a newly added
job is Waiting{Next: Fetching, Reason: NoLease}, which is the same
situation as any other job blocked on capacity (D1).

AllStates is guarded by a source-parsing exhaustiveness test on the same
pattern internal/constants uses, because a hand-written list is otherwise
a second copy of the enum that goes stale in silence. Observed red:
removing Finalizing from AllStates fails with \"Finalizing is declared in
state.go but missing from AllStates()\".

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 2: The transition machine

**Files:**
- Create: `internal/job/transition.go`
- Test: `internal/job/transition_test.go`

**Interfaces:**
- Consumes: `State`, `AllStates()` from Task 1.
- Produces: `var ErrIllegalTransition error`; `func CanTransition(from, to State) bool`; `func IsCorrectness(s State) bool`; `func IsProduction(s State) bool`; `func illegalTransition(from, to State) error`.

- [ ] **Step 1: Write the failing test**

Create `internal/job/transition_test.go`:

```go
package job

import (
	"errors"
	"testing"
)

func TestCanTransition(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to State
		want     bool
	}{
		{"promote", Waiting, Fetching, true},
		{"fetch done", Fetching, Assessing, true},
		{"needs more blocks", Assessing, Fetching, true},
		{"repairable", Assessing, Repairing, true},
		{"re-verify after repair", Repairing, Assessing, true},
		{"cross the boundary", Assessing, Extracting, true},
		{"produce", Extracting, Finalizing, true},
		{"done", Finalizing, Finished, true},
		{"unrecoverable", Assessing, Finished, true},
		{"pause mid-fetch", Fetching, Waiting, true},
		{"pause mid-extract", Extracting, Waiting, true},
		{"resume into extracting", Waiting, Extracting, true},
		{"cancel while waiting", Waiting, Finished, true},

		{"self is legal and idempotent", Fetching, Fetching, true},

		{"no reverse across the boundary", Extracting, Assessing, false},
		{"no reverse across the boundary, far", Finalizing, Fetching, false},
		{"no skipping assessment", Fetching, Extracting, false},
		{"no repair without a verdict", Fetching, Repairing, false},
		{"finished is terminal", Finished, Waiting, false},
		{"finished is terminal, to fetching", Finished, Fetching, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanTransition(tc.from, tc.to); got != tc.want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

// TestBoundaryIsOneWay is the machine-level pin on the central invariant of
// the design: a job crosses from Correctness to Production exactly once and
// never returns. It is driven by AllStates() rather than a literal list, so a
// state added later is checked without anyone remembering to add it here.
func TestBoundaryIsOneWay(t *testing.T) {
	for _, from := range AllStates() {
		if !IsProduction(from) {
			continue
		}
		for _, to := range AllStates() {
			if !IsCorrectness(to) {
				continue
			}
			if CanTransition(from, to) {
				t.Errorf("%s -> %s is legal, but that crosses back from Production into Correctness; "+
					"the boundary must be one-way (spec §4)", from, to)
			}
		}
	}
}

// TestOnlyAssessingBranchesWithinCorrectness pins the single-decider
// property. Within the Correctness zone, only Assessing may have more than
// one non-Waiting, non-Finished successor — every other state does work and
// returns to the hub.
func TestOnlyAssessingBranchesWithinCorrectness(t *testing.T) {
	for _, from := range AllStates() {
		if !IsCorrectness(from) || from == Assessing {
			continue
		}
		var successors []State
		for _, to := range AllStates() {
			if to == from || to == Waiting || to == Finished {
				continue
			}
			if CanTransition(from, to) {
				successors = append(successors, to)
			}
		}
		if len(successors) > 1 {
			t.Errorf("%s has %d work successors %v; only Assessing may branch (spec §5)",
				from, len(successors), successors)
		}
	}
}

func TestIllegalTransitionError(t *testing.T) {
	err := illegalTransition(Extracting, Fetching)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("errors.Is(err, ErrIllegalTransition) = false, want true")
	}
	want := "job: illegal state transition: Extracting → Fetching"
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestZoneClassification(t *testing.T) {
	for _, tc := range []struct {
		s                       State
		correctness, production bool
	}{
		{Waiting, false, false},
		{Fetching, true, false},
		{Assessing, true, false},
		{Repairing, true, false},
		{Extracting, false, true},
		{Finalizing, false, true},
		{Finished, false, false},
	} {
		t.Run(tc.s.String(), func(t *testing.T) {
			if got := IsCorrectness(tc.s); got != tc.correctness {
				t.Errorf("IsCorrectness(%s) = %v, want %v", tc.s, got, tc.correctness)
			}
			if got := IsProduction(tc.s); got != tc.production {
				t.Errorf("IsProduction(%s) = %v, want %v", tc.s, got, tc.production)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -count=1 ./internal/job/ -run 'TestCanTransition|TestBoundary|TestOnlyAssessing|TestIllegal|TestZone' -v`
Expected: FAIL — `undefined: CanTransition`, `undefined: IsProduction`, and so on.

- [ ] **Step 3: Write minimal implementation**

Create `internal/job/transition.go`:

```go
package job

import (
	"errors"
	"fmt"
	"slices"
)

// ErrIllegalTransition is returned when a requested state change is not an
// edge in the machine below.
var ErrIllegalTransition = errors.New("job: illegal state transition")

// legalEdges is the lifecycle as a directed graph. 22 edges, and every one is
// reachable — there are no fan-out blocks, because "still producing, doing
// something else now" is an Activity write rather than a transition.
//
// Three shapes account for all of it:
//
//   - The work spine (8 edges): Waiting→Fetching, Fetching→Assessing,
//     Assessing→{Fetching, Repairing, Extracting, Finished},
//     Repairing→Assessing, Extracting→Finalizing, Finalizing→Finished.
//   - Pause (6 edges): every non-terminal state may enter Waiting, and
//     Waiting may re-enter any state that can be a WaitState.Next.
//   - Cancel (6 edges): every non-terminal state may reach Finished.
//
// A self-transition is always legal and is treated as an idempotent no-op by
// CanTransition, so callers need not special-case it.
//
// The one edge the graph must NOT contain is any return from Production to
// Correctness. TestBoundaryIsOneWay enumerates AllStates() and fails if one
// appears, rather than trusting this comment.
var legalEdges = map[State][]State{
	Waiting:    {Fetching, Assessing, Repairing, Extracting, Finalizing, Finished},
	Fetching:   {Assessing, Waiting, Finished},
	Assessing:  {Fetching, Repairing, Extracting, Waiting, Finished},
	Repairing:  {Assessing, Waiting, Finished},
	Extracting: {Finalizing, Waiting, Finished},
	Finalizing: {Waiting, Finished},
	Finished:   {},
}

// CanTransition reports whether a job may move from → to. Self transitions
// are always legal.
func CanTransition(from, to State) bool {
	if from == to {
		return true
	}
	return slices.Contains(legalEdges[from], to)
}

// IsCorrectness reports whether s is in the reversible zone — the states whose
// goal is having the correct bytes, and which touch nothing outside the job's
// own working directory.
func IsCorrectness(s State) bool {
	return s == Fetching || s == Assessing || s == Repairing
}

// IsProduction reports whether s is past the irreversible boundary — the
// states that delete archives, move files and run user scripts.
func IsProduction(s State) bool {
	return s == Extracting || s == Finalizing
}

func illegalTransition(from, to State) error {
	return fmt.Errorf("%w: %s → %s", ErrIllegalTransition, from, to)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `goimports -w internal/job/transition.go && go build ./... && go test -count=1 ./internal/job/ -v`
Expected: PASS, all subtests.

- [ ] **Step 5: Verify the boundary test discriminates**

The boundary test is the most valuable assertion in the package. Prove it fails when the invariant is broken.

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/job/transition.go "$SCRATCH/transition.bak.go"
# Add an illegal return edge from Production back into Correctness.
sed -i 's/\tExtracting: {Finalizing, Waiting, Finished},/\tExtracting: {Finalizing, Waiting, Finished, Assessing},/' internal/job/transition.go
grep -n 'Extracting: {' internal/job/transition.go   # confirm the mutation landed where intended
go test -count=1 ./internal/job/ -run TestBoundaryIsOneWay
# MUST fail with: "Extracting -> Assessing is legal, but that crosses back ..."
cp "$SCRATCH/transition.bak.go" internal/job/transition.go
go test -count=1 ./internal/job/ -run TestBoundaryIsOneWay
```

- [ ] **Step 6: Commit**

```bash
git add internal/job/transition.go internal/job/transition_test.go
git commit -m "feat(job): add the transition machine and the boundary assertion

22 edges: an 8-edge work spine, 6 pause edges, 6 cancel edges, and two
self-loops folded into CanTransition. Every edge is reachable — the old
model's 66 came from a fan-out that enumerated which activity might come
next, and Activity now carries that.

TestBoundaryIsOneWay enumerates AllStates() rather than a literal list
and fails if any Production state can reach a Correctness one, so the
central invariant is pinned by a test instead of by a comment. Observed
red: adding Assessing to Extracting's successors fails with \"Extracting
-> Assessing is legal, but that crosses back from Production into
Correctness\".

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 3: `WaitReason`, `WaitState`, `StateView`

**Files:**
- Create: `internal/job/wait.go`
- Test: `internal/job/wait_test.go`

**Interfaces:**
- Consumes: `State` from Task 1.
- Produces: `type WaitReason uint8`; constants `NoLease`, `NoComputeSlot`, `UserPaused`, `GlobalPause`; `func AllWaitReasons() []WaitReason`; `func (r WaitReason) String() string`; `func (r WaitReason) IsPause() bool`; `type StateView struct{...}`.

`StateView` is the immutable read shape every consumer outside this package uses. It is produced by `Job.State()` in Task 8 and consumed by `ToSABnzbd` in Task 9.

- [ ] **Step 1: Write the failing test**

Create `internal/job/wait_test.go`:

```go
package job

import "testing"

func TestWaitReason_String(t *testing.T) {
	for _, tc := range []struct {
		r    WaitReason
		want string
	}{
		{NoLease, "NoLease"},
		{NoComputeSlot, "NoComputeSlot"},
		{UserPaused, "UserPaused"},
		{GlobalPause, "GlobalPause"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.r.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
	if got := WaitReason(9).String(); got != "WaitReason(9)" {
		t.Errorf("String() = %q, want %q", got, "WaitReason(9)")
	}
}

// TestWaitReason_IsPause pins the split that ToSABnzbd depends on: a job held
// for capacity renders as Queued, a job held by a pause renders as Paused.
func TestWaitReason_IsPause(t *testing.T) {
	for _, tc := range []struct {
		r    WaitReason
		want bool
	}{
		{NoLease, false},
		{NoComputeSlot, false},
		{UserPaused, true},
		{GlobalPause, true},
	} {
		t.Run(tc.r.String(), func(t *testing.T) {
			if got := tc.r.IsPause(); got != tc.want {
				t.Errorf("IsPause() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAllWaitReasons_CoveredByIsPause fails if a reason is added without
// IsPause being taught about it. A new reason defaults to false there, which
// would silently render a paused job as Queued; this makes that a test
// failure rather than a UI bug.
func TestAllWaitReasons_CoveredByIsPause(t *testing.T) {
	for _, r := range AllWaitReasons() {
		if r.String() == "WaitReason("+itoa(uint8(r))+")" {
			t.Errorf("WaitReason(%d) is in AllWaitReasons() but has no String() arm", r)
		}
	}
	if len(AllWaitReasons()) != 4 {
		t.Errorf("AllWaitReasons() has %d entries, expected 4; a new reason needs a String() arm, "+
			"an IsPause() decision, and a row in TestWaitReason_IsPause", len(AllWaitReasons()))
	}
}

func TestStateView_ZeroValueIsWaitingForALease(t *testing.T) {
	var v StateView
	if v.State != Waiting || v.Next != Waiting || v.Reason != NoLease {
		t.Errorf("zero StateView = %+v; want State=Waiting Next=Waiting Reason=NoLease", v)
	}
}
```

Add the tiny helper the exhaustiveness test uses, in the same file:

```go
func itoa(v uint8) string {
	if v == 0 {
		return "0"
	}
	var buf [3]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -count=1 ./internal/job/ -run 'TestWait|TestStateView' -v`
Expected: FAIL — `undefined: NoLease`, `undefined: StateView`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/job/wait.go`:

```go
package job

import "fmt"

// WaitReason is why a job is held at a state boundary. Waiting for a lease,
// waiting for a compute slot, and being paused are the same situation — the
// job is at a known boundary, holds nothing, decides nothing, and is blocked
// on permission — so they are one state with a reason rather than three
// states (spec §8.2).
type WaitReason uint8

const (
	// NoLease: no acquisition lease is available. This is also the reason a
	// job that has never run is waiting.
	NoLease WaitReason = iota
	// NoComputeSlot: no compute slot is available.
	NoComputeSlot
	// UserPaused: this job was paused.
	UserPaused
	// GlobalPause: the whole queue is paused.
	GlobalPause
)

// AllWaitReasons returns every declared reason.
func AllWaitReasons() []WaitReason {
	return []WaitReason{NoLease, NoComputeSlot, UserPaused, GlobalPause}
}

func (r WaitReason) String() string {
	switch r {
	case NoLease:
		return "NoLease"
	case NoComputeSlot:
		return "NoComputeSlot"
	case UserPaused:
		return "UserPaused"
	case GlobalPause:
		return "GlobalPause"
	default:
		return fmt.Sprintf("WaitReason(%d)", uint8(r))
	}
}

// IsPause distinguishes "held because a person or the queue said stop" from
// "held because capacity is full". Only the former renders as Paused to the
// API; capacity waits render as Queued (spec §12).
func (r WaitReason) IsPause() bool {
	return r == UserPaused || r == GlobalPause
}

// StateView is the immutable read shape of a job's current attempt. It is what
// Job.State() returns and the only thing consumers outside this package see —
// no consumer holds a job lock, and no consumer reaches a mutable field.
//
// Next and Reason are meaningful only when State is Waiting. Activity is
// ActNone unless work is executing. Outcome is OutcomePending until the
// attempt reaches Finished.
//
// The zero value is a job that has never run: Waiting for a lease.
type StateView struct {
	State    State
	Next     State
	Reason   WaitReason
	Activity Activity
	Outcome  Outcome
	// Assessed reports whether this attempt has already been through
	// Assessing. It exists so ToSABnzbd can tell a first-pass download from a
	// re-entry that is fetching recovery volumes — which is exactly what
	// upstream's "Fetching" status means (spec §12).
	Assessed bool
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `goimports -w internal/job/wait.go && go build ./... && go test -count=1 ./internal/job/ -v`
Expected: PASS. `TestStateView_ZeroValueIsWaitingForALease` requires `Waiting`, `NoLease` and `ActNone`/`OutcomePending` to all be the zero value of their types — Tasks 4 and 5 must preserve that.

Note: this task will not compile until Tasks 4 and 5 define `Activity` and `Outcome`. Implement Tasks 4 and 5 before running Step 4, then return here. If you are executing strictly in order, write `wait.go` without the `Activity` and `Outcome` fields, add them in Task 5's step 3, and run this test then.

- [ ] **Step 5: Commit**

```bash
git add internal/job/wait.go internal/job/wait_test.go
git commit -m "feat(job): add WaitReason, and StateView as the sole read shape

Waiting for a lease, waiting for a compute slot, and being paused are one
situation with three reasons rather than three states (spec 8.2). Waiting
carries its already-decided Next, so it adds no branching node and the
single-decider property survives.

StateView is what every consumer outside this package sees. Its zero
value is a job that has never run, which requires Waiting, NoLease,
ActNone and OutcomePending to each be their type's zero — pinned by
TestStateView_ZeroValueIsWaitingForALease.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 4: The `Activity` enum

**Files:**
- Create: `internal/job/activity.go`
- Test: `internal/job/activity_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Activity uint8`; constants `ActNone`, `ActCRCCheck`, `ActPar2Verify`, `ActPar2Repair`, `ActVolumeRecovery`, `ActUnpack`, `ActDeobfuscate`, `ActCleanup`, `ActMove`, `ActScript`; `func AllActivities() []Activity`; `func (a Activity) String() string`.

- [ ] **Step 1: Write the failing test**

Create `internal/job/activity_test.go`:

```go
package job

import "testing"

func TestActivity_String(t *testing.T) {
	for _, tc := range []struct {
		a    Activity
		want string
	}{
		{ActNone, "None"},
		{ActCRCCheck, "CRCCheck"},
		{ActPar2Verify, "Par2Verify"},
		{ActPar2Repair, "Par2Repair"},
		{ActVolumeRecovery, "VolumeRecovery"},
		{ActUnpack, "Unpack"},
		{ActDeobfuscate, "Deobfuscate"},
		{ActCleanup, "Cleanup"},
		{ActMove, "Move"},
		{ActScript, "Script"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.a.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
	if got := Activity(77).String(); got != "Activity(77)" {
		t.Errorf("String() = %q, want %q", got, "Activity(77)")
	}
}

func TestActivity_NoneIsZero(t *testing.T) {
	var a Activity
	if a != ActNone {
		t.Errorf("zero Activity = %v, want ActNone; StateView's zero value depends on this", a)
	}
}

func TestAllActivities_EveryEntryHasAStringArm(t *testing.T) {
	for _, a := range AllActivities() {
		if got := a.String(); got == "Activity("+itoa(uint8(a))+")" {
			t.Errorf("Activity(%d) is in AllActivities() but falls to the default String() arm", a)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -count=1 ./internal/job/ -run TestActivity -v`
Expected: FAIL — `undefined: ActNone`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/job/activity.go`:

```go
package job

import "fmt"

// Activity is what is executing right now. It is NOT a state: nothing
// branches on it, no transition consults it, and it is written by whichever
// component is doing the work. It exists for the UI, the API and the log.
//
// This is where the old model's post-processing stages went. They were states
// there, which is why its transition table needed a near-complete subgraph
// over them — the model had no way to say "still producing, doing something
// else now" (spec §1.1).
type Activity uint8

const (
	// ActNone means no work is executing. The zero value, so a StateView for
	// a job that has never run reports it without anyone assigning it.
	ActNone Activity = iota
	// ActCRCCheck is the cheap verification path — file CRCs against par2
	// headers, no par2 process. Runs inside Assessing.
	ActCRCCheck
	// ActPar2Verify is the full verification path. Runs inside Assessing.
	ActPar2Verify
	// ActPar2Repair runs inside Repairing.
	ActPar2Repair
	// ActVolumeRecovery renames obfuscated RAR volumes so sets are
	// detectable. Runs inside Extracting.
	ActVolumeRecovery
	// ActUnpack runs inside Extracting.
	ActUnpack
	// ActDeobfuscate restores clean filenames. Runs inside Finalizing.
	ActDeobfuscate
	// ActCleanup removes samples, par2 files, unwanted extensions and
	// sidecar directories. Runs inside Finalizing.
	ActCleanup
	// ActMove relocates files to the final directory. Runs inside Finalizing.
	ActMove
	// ActScript runs the user post-processing script, inside Finalizing.
	ActScript
)

// AllActivities returns every declared activity.
func AllActivities() []Activity {
	return []Activity{
		ActNone, ActCRCCheck, ActPar2Verify, ActPar2Repair, ActVolumeRecovery,
		ActUnpack, ActDeobfuscate, ActCleanup, ActMove, ActScript,
	}
}

func (a Activity) String() string {
	switch a {
	case ActNone:
		return "None"
	case ActCRCCheck:
		return "CRCCheck"
	case ActPar2Verify:
		return "Par2Verify"
	case ActPar2Repair:
		return "Par2Repair"
	case ActVolumeRecovery:
		return "VolumeRecovery"
	case ActUnpack:
		return "Unpack"
	case ActDeobfuscate:
		return "Deobfuscate"
	case ActCleanup:
		return "Cleanup"
	case ActMove:
		return "Move"
	case ActScript:
		return "Script"
	default:
		return fmt.Sprintf("Activity(%d)", uint8(a))
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `goimports -w internal/job/activity.go && go build ./... && go test -count=1 ./internal/job/ -run TestActivity -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/job/activity.go internal/job/activity_test.go
git commit -m "feat(job): add the Activity enum

What is executing right now, on its own axis. Nothing branches on it and
no transition consults it — this is where the old model's post-processing
stages go, and moving them off the state axis is what removes the
near-complete subgraph the old transition table needed over them.

ActNone is the zero value so a StateView for a job that has never run
reports it without an assignment.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 5: The `Outcome` enum

**Files:**
- Create: `internal/job/outcome.go`
- Test: `internal/job/outcome_test.go`
- Modify: `internal/job/wait.go` — add the `Activity` and `Outcome` fields to `StateView` if Task 3 deferred them.

**Interfaces:**
- Consumes: nothing.
- Produces: `type Outcome uint8`; constants `OutcomePending`, `OutcomeOK`, `OutcomeFailed`, `OutcomeUnrecoverable`, `OutcomeCancelled`; `func AllOutcomes() []Outcome`; `func (o Outcome) String() string`; `func (o Outcome) IsSettled() bool`.

- [ ] **Step 1: Write the failing test**

Create `internal/job/outcome_test.go`:

```go
package job

import "testing"

func TestOutcome_String(t *testing.T) {
	for _, tc := range []struct {
		o    Outcome
		want string
	}{
		{OutcomePending, "Pending"},
		{OutcomeOK, "OK"},
		{OutcomeFailed, "Failed"},
		{OutcomeUnrecoverable, "Unrecoverable"},
		{OutcomeCancelled, "Cancelled"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.o.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
	if got := Outcome(42).String(); got != "Outcome(42)" {
		t.Errorf("String() = %q, want %q", got, "Outcome(42)")
	}
}

// TestOutcome_PendingIsZeroAndNotSettled pins the property the write-once
// rule rests on: an attempt that has not reached Finished carries the zero
// Outcome, and the zero Outcome is not a verdict.
func TestOutcome_PendingIsZeroAndNotSettled(t *testing.T) {
	var o Outcome
	if o != OutcomePending {
		t.Fatalf("zero Outcome = %v, want OutcomePending", o)
	}
	if o.IsSettled() {
		t.Error("OutcomePending.IsSettled() = true, want false; a pending attempt has no verdict yet")
	}
}

func TestOutcome_IsSettled(t *testing.T) {
	for _, tc := range []struct {
		o    Outcome
		want bool
	}{
		{OutcomePending, false},
		{OutcomeOK, true},
		{OutcomeFailed, true},
		{OutcomeUnrecoverable, true},
		{OutcomeCancelled, true},
	} {
		t.Run(tc.o.String(), func(t *testing.T) {
			if got := tc.o.IsSettled(); got != tc.want {
				t.Errorf("IsSettled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAllOutcomes_EveryEntryHasAStringArm(t *testing.T) {
	for _, o := range AllOutcomes() {
		if got := o.String(); got == "Outcome("+itoa(uint8(o))+")" {
			t.Errorf("Outcome(%d) is in AllOutcomes() but falls to the default String() arm", o)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -count=1 ./internal/job/ -run TestOutcome -v`
Expected: FAIL — `undefined: OutcomePending`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/job/outcome.go`:

```go
package job

import "fmt"

// Outcome is an attempt's verdict. It is write-once: assigned only on the
// edge into Finished, and never revised.
//
// The old model made "did this job fail?" a question whose answer could
// change, because Failed → Queued was a legal edge. Retry therefore had to
// reconstruct "failed, retry me" from per-article bits. Here a retry appends
// a new Attempt with its own Outcome instead, so a verdict is superseded
// rather than mutated (spec §3.1).
type Outcome uint8

const (
	// OutcomePending: the attempt has not reached Finished. The zero value,
	// so an in-flight attempt carries it without an assignment.
	OutcomePending Outcome = iota
	// OutcomeOK: the job produced its files.
	OutcomeOK
	// OutcomeFailed: production ran and something in it failed.
	OutcomeFailed
	// OutcomeUnrecoverable: the verdict was Unrecoverable, so the job never
	// crossed the boundary. Its files are still in the working directory and
	// it is still retryable (D3) — which is the whole reason this is a
	// distinct outcome from OutcomeFailed rather than folded into it.
	OutcomeUnrecoverable
	// OutcomeCancelled: a person stopped it.
	OutcomeCancelled
)

// AllOutcomes returns every declared outcome.
func AllOutcomes() []Outcome {
	return []Outcome{
		OutcomePending, OutcomeOK, OutcomeFailed,
		OutcomeUnrecoverable, OutcomeCancelled,
	}
}

func (o Outcome) String() string {
	switch o {
	case OutcomePending:
		return "Pending"
	case OutcomeOK:
		return "OK"
	case OutcomeFailed:
		return "Failed"
	case OutcomeUnrecoverable:
		return "Unrecoverable"
	case OutcomeCancelled:
		return "Cancelled"
	default:
		return fmt.Sprintf("Outcome(%d)", uint8(o))
	}
}

// IsSettled reports whether a verdict has been reached. Every value except
// OutcomePending is settled, and a settled outcome may never be reassigned.
func (o Outcome) IsSettled() bool {
	return o != OutcomePending
}
```

- [ ] **Step 4: Ensure `StateView` carries both new fields**

If Task 3 deferred them, add to `StateView` in `internal/job/wait.go`:

```go
	Activity Activity
	Outcome  Outcome
```

- [ ] **Step 5: Run the whole package**

Run: `goimports -w internal/job/ && go build ./... && go test -count=1 ./internal/job/ -v`
Expected: PASS, including `TestStateView_ZeroValueIsWaitingForALease` from Task 3.

- [ ] **Step 6: Commit**

```bash
git add internal/job/outcome.go internal/job/outcome_test.go internal/job/wait.go
git commit -m "feat(job): add the write-once Outcome enum

Assigned only on the edge into Finished and never revised. The old model
made \"did this job fail?\" mutable by having Failed -> Queued as a legal
edge, so retry had to reconstruct the distinction from per-article bits.

Unrecoverable is deliberately distinct from Failed rather than folded
into it: an unrecoverable job never crossed the boundary, so its files
are intact and it is still retryable (D3). Collapsing the two would lose
exactly the information that makes retry worth offering.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 6: `Policy`

**Files:**
- Create: `internal/job/policy.go`
- Test: `internal/job/policy_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Policy struct{ Verify, Repair, Unpack, Delete bool }`; `func PolicyFromPP(pp int) Policy`; `func (p Policy) String() string`.

- [ ] **Step 1: Write the failing test**

Create `internal/job/policy_test.go`:

```go
package job

import "testing"

func TestPolicyFromPP(t *testing.T) {
	for _, tc := range []struct {
		name string
		pp   int
		want Policy
	}{
		{"pp0 download only", 0, Policy{}},
		{"pp1 repair", 1, Policy{Verify: true, Repair: true}},
		{"pp2 repair and unpack", 2, Policy{Verify: true, Repair: true, Unpack: true}},
		{"pp3 everything", 3, Policy{Verify: true, Repair: true, Unpack: true, Delete: true}},

		// PP is an external integer and arrives from config, an API query
		// parameter and an NZB meta tag. Out-of-range values clamp rather
		// than producing a policy nobody designed.
		{"negative clamps to pp0", -1, Policy{}},
		{"above range clamps to pp3", 9, Policy{Verify: true, Repair: true, Unpack: true, Delete: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PolicyFromPP(tc.pp); got != tc.want {
				t.Errorf("PolicyFromPP(%d) = %+v, want %+v", tc.pp, got, tc.want)
			}
		})
	}
}

// TestPolicy_ZeroIsDownloadOnly pins that the zero Policy is PP=0 rather than
// "everything on". A job constructed without an explicit policy must do the
// least destructive thing, not the most.
func TestPolicy_ZeroIsDownloadOnly(t *testing.T) {
	var p Policy
	if p != PolicyFromPP(0) {
		t.Errorf("zero Policy = %+v, want PolicyFromPP(0) = %+v", p, PolicyFromPP(0))
	}
	if p.Unpack || p.Delete {
		t.Error("zero Policy enables a destructive step; it must default to download-only")
	}
}

func TestPolicy_String(t *testing.T) {
	for _, tc := range []struct {
		p    Policy
		want string
	}{
		{Policy{}, "Policy(download-only)"},
		{PolicyFromPP(1), "Policy(verify,repair)"},
		{PolicyFromPP(3), "Policy(verify,repair,unpack,delete)"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.p.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -count=1 ./internal/job/ -run TestPolicy -v`
Expected: FAIL — `undefined: Policy`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/job/policy.go`:

```go
package job

import "strings"

// Policy is what a job is permitted to do, resolved once at ingestion from
// SABnzbd's PP level plus the job's category.
//
// PP 0-3 is a cumulative integer mask inherited from upstream, and it is the
// same KIND of thing as upstream's status strings: external vocabulary,
// translated at the boundary and never stored internally. The integer does
// not exist past App (D4).
//
// Every state runs at every policy. At Verify: false the Assessor returns
// Complete without doing work and the job crosses the boundary immediately.
// Gating STATES on the level instead would mean skipping Assessing at PP=0 —
// which removes the only state that decides and leaves nothing to authorize
// the crossing, forcing a second decider back into the design.
//
// The zero value is download-only, so a job built without an explicit policy
// does the least destructive thing.
type Policy struct {
	// Verify permits the Assessor to reach a real verdict. When false it
	// returns Complete unconditionally.
	Verify bool
	// Repair permits entering Repairing.
	Repair bool
	// Unpack permits archive extraction inside Extracting.
	Unpack bool
	// Delete permits removing archives and sidecar files after extraction.
	Delete bool
}

// PolicyFromPP resolves an upstream PP level to a Policy. Out-of-range input
// clamps: PP arrives from config, from an API query parameter and from an NZB
// meta tag, none of which we control, and clamping is preferable to
// synthesising a combination nobody designed.
func PolicyFromPP(pp int) Policy {
	pp = min(max(pp, 0), 3)
	return Policy{
		Verify: pp >= 1,
		Repair: pp >= 1,
		Unpack: pp >= 2,
		Delete: pp >= 3,
	}
}

func (p Policy) String() string {
	var on []string
	if p.Verify {
		on = append(on, "verify")
	}
	if p.Repair {
		on = append(on, "repair")
	}
	if p.Unpack {
		on = append(on, "unpack")
	}
	if p.Delete {
		on = append(on, "delete")
	}
	if len(on) == 0 {
		return "Policy(download-only)"
	}
	return "Policy(" + strings.Join(on, ",") + ")"
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `goimports -w internal/job/policy.go && go fix ./internal/job/ && go build ./... && go test -count=1 ./internal/job/ -run TestPolicy -v`
Expected: PASS. `go fix` should leave the `min`/`max` builtins alone — they are already the modern form.

- [ ] **Step 5: Commit**

```bash
git add internal/job/policy.go internal/job/policy_test.go
git commit -m "feat(job): add Policy, replacing the PP integer internally

PP 0-3 gets the same treatment as upstream's status strings: translated
at the boundary, never stored internally (D4). The integer does not exist
past App.

Every state runs at every policy. Gating states on the level would mean
skipping Assessing at PP=0, which removes the only state that decides and
leaves nothing to authorize the boundary crossing — a second decider
would have to be reintroduced, which is the thing this design exists to
avoid.

The zero value is download-only so a job built without an explicit policy
does the least destructive thing.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 7: `Attempt`

**Files:**
- Create: `internal/job/attempt.go`
- Test: `internal/job/attempt_test.go`

**Interfaces:**
- Consumes: `State`, `Activity`, `Outcome`, `WaitReason`, `StateView`, `CanTransition`, `illegalTransition`.
- Produces: `type Attempt struct{...}` (all fields unexported); `func newAttempt(now time.Time) Attempt`; `func (a *Attempt) view() StateView`; `func (a *Attempt) transition(to State, now time.Time) error`; `func (a *Attempt) hold(next State, r WaitReason) error`; `func (a *Attempt) setActivity(x Activity)`; `func (a *Attempt) finish(o Outcome, now time.Time) error`; `func (a *Attempt) isOpen() bool`.

All lowercase: `Attempt` is mutated only by `Job`, which lives in the same package. Nothing outside this package can construct or change one.

- [ ] **Step 1: Write the failing test**

Create `internal/job/attempt_test.go`:

```go
package job

import (
	"errors"
	"testing"
	"time"
)

func testClock() time.Time { return time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC) }

func TestNewAttempt_StartsFetching(t *testing.T) {
	a := newAttempt(testClock())
	v := a.view()
	if v.State != Fetching {
		t.Errorf("State = %v, want Fetching; an attempt opens when a lease is issued", v.State)
	}
	if v.Outcome != OutcomePending {
		t.Errorf("Outcome = %v, want OutcomePending", v.Outcome)
	}
	if !a.isOpen() {
		t.Error("isOpen() = false, want true for a fresh attempt")
	}
	if !a.started.Equal(testClock()) {
		t.Errorf("started = %v, want %v", a.started, testClock())
	}
}

func TestAttempt_TransitionRejectsIllegalEdge(t *testing.T) {
	a := newAttempt(testClock())
	// Fetching -> Extracting skips assessment.
	err := a.transition(Extracting, testClock())
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("transition(Extracting) error = %v, want ErrIllegalTransition", err)
	}
	if got := a.view().State; got != Fetching {
		t.Errorf("State = %v after a rejected transition, want Fetching unchanged", got)
	}
}

// TestAttempt_AssessedLatches pins the flag ToSABnzbd uses to tell a
// first-pass download from a re-entry fetching recovery volumes.
func TestAttempt_AssessedLatches(t *testing.T) {
	a := newAttempt(testClock())
	if a.view().Assessed {
		t.Fatal("Assessed = true on a fresh attempt, want false")
	}
	mustTransition(t, &a, Assessing)
	if !a.view().Assessed {
		t.Error("Assessed = false after entering Assessing, want true")
	}
	mustTransition(t, &a, Fetching)
	if !a.view().Assessed {
		t.Error("Assessed = false after leaving Assessing, want true; the flag latches for the attempt")
	}
}

// TestAttempt_ActivityClearsOnTransition pins that Activity never survives a
// state change. A stale activity would render as "repairing" while the job is
// extracting, which is worse than showing nothing.
func TestAttempt_ActivityClearsOnTransition(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	a.setActivity(ActPar2Verify)
	if got := a.view().Activity; got != ActPar2Verify {
		t.Fatalf("Activity = %v, want ActPar2Verify", got)
	}
	mustTransition(t, &a, Extracting)
	if got := a.view().Activity; got != ActNone {
		t.Errorf("Activity = %v after a transition, want ActNone", got)
	}
}

func TestAttempt_HoldRecordsNextAndReason(t *testing.T) {
	a := newAttempt(testClock())
	if err := a.hold(Assessing, NoComputeSlot); err != nil {
		t.Fatalf("hold: %v", err)
	}
	v := a.view()
	if v.State != Waiting || v.Next != Assessing || v.Reason != NoComputeSlot {
		t.Errorf("view = %+v; want State=Waiting Next=Assessing Reason=NoComputeSlot", v)
	}
}

func TestAttempt_HoldRejectsAnIllegalDestination(t *testing.T) {
	a := newAttempt(testClock())
	// Fetching cannot resume into Repairing, so it must not be able to wait
	// for it either — otherwise the hold defers an illegal edge instead of
	// rejecting it.
	if err := a.hold(Repairing, NoComputeSlot); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("hold(Repairing) error = %v, want ErrIllegalTransition", err)
	}
	if got := a.view().State; got != Fetching {
		t.Errorf("State = %v after a rejected hold, want Fetching unchanged", got)
	}
}

func TestAttempt_FinishIsWriteOnce(t *testing.T) {
	a := newAttempt(testClock())
	later := testClock().Add(time.Minute)
	if err := a.finish(OutcomeCancelled, later); err != nil {
		t.Fatalf("finish: %v", err)
	}
	v := a.view()
	if v.State != Finished || v.Outcome != OutcomeCancelled {
		t.Fatalf("view = %+v; want State=Finished Outcome=Cancelled", v)
	}
	if a.isOpen() {
		t.Error("isOpen() = true after finish, want false")
	}
	if !a.ended.Equal(later) {
		t.Errorf("ended = %v, want %v", a.ended, later)
	}

	err := a.finish(OutcomeOK, later.Add(time.Minute))
	if !errors.Is(err, ErrOutcomeAlreadySet) {
		t.Fatalf("second finish error = %v, want ErrOutcomeAlreadySet", err)
	}
	if got := a.view().Outcome; got != OutcomeCancelled {
		t.Errorf("Outcome = %v after a rejected second finish, want Cancelled unchanged", got)
	}
}

func TestAttempt_FinishRejectsPending(t *testing.T) {
	a := newAttempt(testClock())
	if err := a.finish(OutcomePending, testClock()); err == nil {
		t.Error("finish(OutcomePending) = nil, want an error; Pending is not a verdict")
	}
	if a.view().State == Finished {
		t.Error("attempt reached Finished with a Pending outcome")
	}
}

func mustTransition(t *testing.T, a *Attempt, to State) {
	t.Helper()
	if err := a.transition(to, testClock()); err != nil {
		t.Fatalf("transition(%v): %v", to, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -count=1 ./internal/job/ -run TestAttempt -v`
Expected: FAIL — `undefined: newAttempt`, `undefined: ErrOutcomeAlreadySet`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/job/attempt.go`:

```go
package job

import (
	"errors"
	"fmt"
	"time"
)

// ErrOutcomeAlreadySet is returned when a settled attempt is finished again.
// The write-once rule is enforced here rather than by convention, because a
// second assignment is exactly the mutation the design exists to prevent.
var ErrOutcomeAlreadySet = errors.New("job: attempt outcome already set")

// Attempt is one run of a job through the machine. The state machine lives
// here, not on Job: a job has a LIST of attempts, each carrying its own
// write-once Outcome, so a retry appends a verdict rather than revising one
// (spec §3.1).
//
// An attempt opens when a lease is first issued and no attempt is open, and
// closes when it reaches Finished. Pause and resume inside an attempt do not
// end it — the lease is surrendered and later re-taken, and the attempt
// persists across that.
//
// Every field is unexported and every mutator is package-private. Job is the
// only caller, and it holds its own lock across each of these; Attempt does
// no locking of its own.
type Attempt struct {
	state    State
	next     State
	reason   WaitReason
	activity Activity
	outcome  Outcome
	assessed bool
	started  time.Time
	ended    time.Time
}

// newAttempt opens an attempt in Fetching. There is no arm for opening in any
// other state: an attempt begins when a lease is issued, and a lease is what
// Fetching requires.
func newAttempt(now time.Time) Attempt {
	return Attempt{state: Fetching, started: now}
}

func (a *Attempt) isOpen() bool { return a.outcome == OutcomePending }

func (a *Attempt) view() StateView {
	return StateView{
		State:    a.state,
		Next:     a.next,
		Reason:   a.reason,
		Activity: a.activity,
		Outcome:  a.outcome,
		Assessed: a.assessed,
	}
}

// transition moves the attempt to `to`, rejecting any edge the machine does
// not contain. Activity is cleared, because it describes the state being left:
// carrying it forward would render a job as "repairing" while it extracts.
func (a *Attempt) transition(to State, now time.Time) error {
	if !CanTransition(a.state, to) {
		return illegalTransition(a.state, to)
	}
	a.state = to
	a.activity = ActNone
	a.next = Waiting
	a.reason = NoLease
	if to == Assessing {
		// Latches for the life of the attempt. ToSABnzbd reads it to tell a
		// first-pass download from a re-entry fetching recovery volumes,
		// which is what upstream's "Fetching" status means.
		a.assessed = true
	}
	if to == Finished && a.outcome == OutcomePending {
		a.ended = now
	}
	return nil
}

// hold parks the attempt at a boundary. next is where it will resume, and is
// validated against the machine now rather than at resume time — a hold that
// defers an illegal edge is an illegal edge with a delay.
func (a *Attempt) hold(next State, r WaitReason) error {
	if !CanTransition(a.state, Waiting) {
		return illegalTransition(a.state, Waiting)
	}
	if !CanTransition(Waiting, next) {
		return illegalTransition(Waiting, next)
	}
	a.state = Waiting
	a.next = next
	a.reason = r
	a.activity = ActNone
	return nil
}

// setActivity records what is executing. It is deliberately unvalidated
// against state: nothing branches on Activity, so a mismatch is a display bug
// rather than a correctness one, and a validation table here would be a second
// place to update whenever a stage moves.
func (a *Attempt) setActivity(x Activity) { a.activity = x }

// finish assigns the verdict and closes the attempt. Write-once.
func (a *Attempt) finish(o Outcome, now time.Time) error {
	if !o.IsSettled() {
		return fmt.Errorf("job: cannot finish an attempt with outcome %s", o)
	}
	if a.outcome.IsSettled() {
		return fmt.Errorf("%w: %s, refusing to overwrite with %s", ErrOutcomeAlreadySet, a.outcome, o)
	}
	if !CanTransition(a.state, Finished) {
		return illegalTransition(a.state, Finished)
	}
	a.state = Finished
	a.activity = ActNone
	a.outcome = o
	a.ended = now
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `goimports -w internal/job/attempt.go && go build ./... && go test -count=1 ./internal/job/ -run TestAttempt -v`
Expected: PASS.

- [ ] **Step 5: Verify write-once actually discriminates**

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/job/attempt.go "$SCRATCH/attempt.bak.go"
# Neuter the write-once guard.
sed -i 's/\tif a.outcome.IsSettled() {/\tif false {/' internal/job/attempt.go
grep -n 'if false {' internal/job/attempt.go   # confirm the mutation landed
go test -count=1 ./internal/job/ -run TestAttempt_FinishIsWriteOnce
# MUST fail with: "second finish error = <nil>, want ErrOutcomeAlreadySet"
cp "$SCRATCH/attempt.bak.go" internal/job/attempt.go
go test -count=1 ./internal/job/ -run TestAttempt_FinishIsWriteOnce
```

- [ ] **Step 6: Commit**

```bash
git add internal/job/attempt.go internal/job/attempt_test.go
git commit -m "feat(job): add Attempt, which carries the state machine

The machine lives on the attempt rather than on the job, so a retry
appends a verdict instead of revising one (D2). An attempt opens when a
lease is first issued and closes at Finished; pause and resume inside it
do not end it, so a pause cycle is not counted as a retry.

Two guards are enforced rather than documented. Write-once rejects a
second finish, and hold validates its resume destination now rather than
at resume time — a hold that defers an illegal edge is an illegal edge
with a delay. Observed red: neutering the write-once check fails with
\"second finish error = <nil>, want ErrOutcomeAlreadySet\".

Activity clears on every transition. Carrying it forward would render a
job as repairing while it extracts, which is worse than showing nothing.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 8: `Job`

**Files:**
- Create: `internal/job/job.go`
- Test: `internal/job/job_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1-7.
- Produces: `type Job struct{...}`; `func New(id, name string, p Policy) *Job`; `func (j *Job) ID() string`; `func (j *Job) Name() string`; `func (j *Job) Policy() Policy`; `func (j *Job) State() StateView`; `func (j *Job) HasRun() bool`; `func (j *Job) Attempts() int`; `func (j *Job) BeginAttempt(now time.Time) error`; `func (j *Job) Transition(to State, now time.Time) error`; `func (j *Job) Hold(next State, r WaitReason) error`; `func (j *Job) SetActivity(x Activity) error`; `func (j *Job) Finish(o Outcome, now time.Time) error`; `var ErrNoOpenAttempt error`.

- [ ] **Step 1: Write the failing test**

Create `internal/job/job_test.go`:

```go
package job

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func newTestJob(t *testing.T) *Job {
	t.Helper()
	return New("abc123", "Test.Job", PolicyFromPP(3))
}

// TestJob_NeverRunReportsWaitingForALease pins D1: there is no Queued state,
// and a job that has never run has no attempt record at all.
func TestJob_NeverRunReportsWaitingForALease(t *testing.T) {
	j := newTestJob(t)
	if j.HasRun() {
		t.Error("HasRun() = true on a fresh job, want false")
	}
	if got := j.Attempts(); got != 0 {
		t.Errorf("Attempts() = %d, want 0", got)
	}
	v := j.State()
	if v.State != Waiting || v.Next != Fetching || v.Reason != NoLease {
		t.Errorf("State() = %+v; want State=Waiting Next=Fetching Reason=NoLease", v)
	}
}

func TestJob_BeginAttemptOpensOne(t *testing.T) {
	j := newTestJob(t)
	if err := j.BeginAttempt(testClock()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	if !j.HasRun() || j.Attempts() != 1 {
		t.Errorf("HasRun()=%v Attempts()=%d, want true and 1", j.HasRun(), j.Attempts())
	}
	if got := j.State().State; got != Fetching {
		t.Errorf("State = %v, want Fetching", got)
	}
}

// TestJob_BeginAttemptIsIdempotentWhileOneIsOpen pins the rule that a lease
// re-issued after a pause does not count as a new attempt.
func TestJob_BeginAttemptIsIdempotentWhileOneIsOpen(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)
	if err := j.Hold(Fetching, UserPaused); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := j.BeginAttempt(testClock().Add(time.Hour)); err != nil {
		t.Fatalf("second BeginAttempt: %v", err)
	}
	if got := j.Attempts(); got != 1 {
		t.Errorf("Attempts() = %d after a pause/resume cycle, want 1; "+
			"an attempt closes only at Finished", got)
	}
}

// TestJob_RetryAppendsAnAttempt is D2's core property: the previous verdict
// survives, and the new attempt starts pending.
func TestJob_RetryAppendsAnAttempt(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)
	if err := j.Finish(OutcomeUnrecoverable, testClock()); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got := j.State().Outcome; got != OutcomeUnrecoverable {
		t.Fatalf("Outcome = %v, want Unrecoverable", got)
	}

	if err := j.BeginAttempt(testClock().Add(time.Hour)); err != nil {
		t.Fatalf("retry BeginAttempt: %v", err)
	}
	if got := j.Attempts(); got != 2 {
		t.Errorf("Attempts() = %d, want 2", got)
	}
	v := j.State()
	if v.State != Fetching || v.Outcome != OutcomePending {
		t.Errorf("State() = %+v; want a fresh Fetching attempt with a pending outcome", v)
	}
	if got := j.AttemptAt(0).Outcome; got != OutcomeUnrecoverable {
		t.Errorf("first attempt's Outcome = %v, want Unrecoverable preserved", got)
	}
}

func TestJob_MutatorsRequireAnOpenAttempt(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Job) error
	}{
		{"Transition", func(j *Job) error { return j.Transition(Assessing, testClock()) }},
		{"Hold", func(j *Job) error { return j.Hold(Fetching, UserPaused) }},
		{"SetActivity", func(j *Job) error { return j.SetActivity(ActUnpack) }},
		{"Finish", func(j *Job) error { return j.Finish(OutcomeOK, testClock()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := newTestJob(t)
			if err := tc.call(j); !errors.Is(err, ErrNoOpenAttempt) {
				t.Errorf("%s on a never-run job = %v, want ErrNoOpenAttempt", tc.name, err)
			}
		})
	}
}

func TestJob_FinishedJobHasNoOpenAttempt(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)
	if err := j.Finish(OutcomeOK, testClock()); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := j.Transition(Fetching, testClock()); !errors.Is(err, ErrNoOpenAttempt) {
		t.Errorf("Transition after Finish = %v, want ErrNoOpenAttempt", err)
	}
}

// TestJob_ConcurrentReadsAndWrites is the race-detector pin on Job owning its
// own lock. It asserts no outcome beyond "this does not race" — correctness
// of the transitions is covered above.
func TestJob_ConcurrentReadsAndWrites(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 200 {
				_ = j.State()
				_ = j.HasRun()
				_ = j.Attempts()
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			for range 200 {
				_ = j.SetActivity(ActPar2Verify)
				_ = j.SetActivity(ActNone)
			}
		})
	}
	wg.Wait()
}

func mustBegin(t *testing.T, j *Job) {
	t.Helper()
	if err := j.BeginAttempt(testClock()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -count=1 ./internal/job/ -run TestJob -v`
Expected: FAIL — `undefined: New`, `undefined: ErrNoOpenAttempt`, `undefined: AttemptAt`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/job/job.go`:

```go
package job

import (
	"errors"
	"sync"
	"time"
)

// ErrNoOpenAttempt is returned by every mutator when the job has no attempt
// in flight — either it has never run, or its last attempt is settled. The
// caller's fix is BeginAttempt, which is the only door into the machine.
var ErrNoOpenAttempt = errors.New("job: no open attempt")

// Job owns its state. Every field is unexported and guarded by mu; there is
// no path to a Job's state that does not go through a method here.
//
// The lock ordering rule for the whole system is that a Job method never
// calls a Queue method — Queue is strictly the caller, Job strictly the
// callee, so the order is always Queue.mu then Job.mu and cannot invert. That
// is why this file imports nothing from the rest of the daemon.
//
// Job does no I/O. It exposes State() and the attempt accessors; a
// Checkpointer reads those and is the sole writer to the database.
type Job struct {
	mu sync.RWMutex

	id     string
	name   string
	policy Policy

	// attempts is the machine. The current attempt is the last element, and
	// an empty slice means the job has never run — which is what HasRun
	// reports and what makes "never started" exact rather than a predicate
	// over byte counters (D1).
	//
	// Deliberately unbounded (D7). The growth case is one job an automation
	// tool retries on a schedule; each Attempt is a handful of words, and the
	// two remedies if it ever bites are a cap here or a sweep alongside
	// history retention. Not worth a policy before there is evidence.
	attempts []Attempt
}

// New builds a job that has never run. It has no attempt record, because
// nothing has happened to it yet.
func New(id, name string, p Policy) *Job {
	return &Job{id: id, name: name, policy: p}
}

func (j *Job) ID() string { return j.id }

func (j *Job) Name() string { return j.name }

func (j *Job) Policy() Policy { return j.policy }

// State returns the current attempt's view. For a job that has never run the
// answer is a constant rather than a special case: it is waiting for a lease,
// which is exactly what is true.
func (j *Job) State() StateView {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if len(j.attempts) == 0 {
		return StateView{State: Waiting, Next: Fetching, Reason: NoLease}
	}
	return j.attempts[len(j.attempts)-1].view()
}

// HasRun reports whether this job has ever held a lease. Exact, where any
// predicate over bytes or durable runs would conflate "did not start" with
// "started and got nowhere".
func (j *Job) HasRun() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return len(j.attempts) > 0
}

// Attempts returns how many times this job has run.
func (j *Job) Attempts() int {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return len(j.attempts)
}

// AttemptAt returns a view of the i'th attempt. Panics on an out-of-range
// index, matching slice semantics — callers bound i with Attempts().
func (j *Job) AttemptAt(i int) StateView {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.attempts[i].view()
}

// BeginAttempt opens an attempt if none is open, and is a no-op if one
// already is. That is the rule that stops a pause/resume cycle — which
// surrenders and later re-takes a lease — from being counted as a retry.
func (j *Job) BeginAttempt(now time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if a := j.currentLocked(); a != nil && a.isOpen() {
		return nil
	}
	j.attempts = append(j.attempts, newAttempt(now))
	return nil
}

func (j *Job) Transition(to State, now time.Time) error {
	return j.withOpenAttempt(func(a *Attempt) error { return a.transition(to, now) })
}

func (j *Job) Hold(next State, r WaitReason) error {
	return j.withOpenAttempt(func(a *Attempt) error { return a.hold(next, r) })
}

func (j *Job) SetActivity(x Activity) error {
	return j.withOpenAttempt(func(a *Attempt) error { a.setActivity(x); return nil })
}

func (j *Job) Finish(o Outcome, now time.Time) error {
	return j.withOpenAttempt(func(a *Attempt) error { return a.finish(o, now) })
}

// withOpenAttempt is the single door every mutator goes through: take the
// write lock, resolve the open attempt or fail, apply. One door rather than
// four copies of the same preamble, so "must there be an open attempt?" has
// one answer that cannot drift between mutators.
func (j *Job) withOpenAttempt(fn func(*Attempt) error) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	a := j.currentLocked()
	if a == nil || !a.isOpen() {
		return ErrNoOpenAttempt
	}
	return fn(a)
}

// currentLocked returns a pointer to the last attempt, or nil if there are
// none. Must hold mu.
func (j *Job) currentLocked() *Attempt {
	if len(j.attempts) == 0 {
		return nil
	}
	return &j.attempts[len(j.attempts)-1]
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `goimports -w internal/job/job.go && go build ./... && go test -count=1 -race ./internal/job/ -v`
Expected: PASS, including `TestJob_ConcurrentReadsAndWrites` under `-race`.

- [ ] **Step 5: Verify the attempt-boundary rule discriminates**

The most valuable assertion here is that a pause cycle does not open a second attempt. Prove it.

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/job/job.go "$SCRATCH/job.bak.go"
# Make BeginAttempt unconditionally append, as it would if the open check were forgotten.
sed -i 's/\tif a := j.currentLocked(); a != nil \&\& a.isOpen() {/\tif false {/' internal/job/job.go
grep -n 'if false {' internal/job/job.go   # confirm the mutation landed
go test -count=1 ./internal/job/ -run TestJob_BeginAttemptIsIdempotentWhileOneIsOpen
# MUST fail with: "Attempts() = 2 after a pause/resume cycle, want 1"
cp "$SCRATCH/job.bak.go" internal/job/job.go
go test -count=1 ./internal/job/ -run TestJob_BeginAttemptIsIdempotentWhileOneIsOpen
```

- [ ] **Step 6: Commit**

```bash
git add internal/job/job.go internal/job/job_test.go
git commit -m "feat(job): add Job, which owns its state behind its own lock

Every field is unexported and guarded by mu, and every mutator goes
through one door (withOpenAttempt) so \"must there be an open attempt?\"
has a single answer that cannot drift between mutators.

Job imports nothing from the rest of the daemon, which is how the lock
ordering rule is enforced structurally rather than by review: a Job
method cannot call a Queue method because it cannot see one.

BeginAttempt is a no-op while an attempt is open, which is what stops a
pause/resume cycle from counting as a retry. Observed red: making it
append unconditionally fails with \"Attempts() = 2 after a pause/resume
cycle, want 1\".

A job that has never run has no attempt record; State() answers with a
constant rather than a special case, and HasRun is exact where any
predicate over byte counters would conflate did-not-start with
started-and-got-nowhere.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 9: `ToSABnzbd`

**Files:**
- Create: `internal/job/sabnzbd.go`
- Test: `internal/job/sabnzbd_test.go`

**Interfaces:**
- Consumes: `StateView` and every enum; `constants.Status` from `internal/constants`.
- Produces: `func ToSABnzbd(v StateView) constants.Status`.

This is the only file in the package that may import `internal/constants`, and the translation is one-way: nothing reads a `constants.Status` back in.

- [ ] **Step 1: Write the failing test**

Create `internal/job/sabnzbd_test.go`:

```go
package job

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

func TestToSABnzbd(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    StateView
		want constants.Status
	}{
		{"never run", StateView{State: Waiting, Next: Fetching, Reason: NoLease}, constants.StatusQueued},
		{"waiting for a compute slot", StateView{State: Waiting, Next: Assessing, Reason: NoComputeSlot}, constants.StatusQueued},
		{"user paused", StateView{State: Waiting, Next: Fetching, Reason: UserPaused}, constants.StatusPaused},
		{"globally paused", StateView{State: Waiting, Next: Extracting, Reason: GlobalPause}, constants.StatusPaused},

		{"first-pass download", StateView{State: Fetching}, constants.StatusDownloading},
		{"fetching recovery volumes", StateView{State: Fetching, Assessed: true}, constants.StatusFetching},

		{"cheap verification", StateView{State: Assessing, Activity: ActCRCCheck}, constants.StatusQuickCheck},
		{"full verification", StateView{State: Assessing, Activity: ActPar2Verify}, constants.StatusVerifying},
		{"assessing, no activity yet", StateView{State: Assessing}, constants.StatusVerifying},

		{"repairing", StateView{State: Repairing, Activity: ActPar2Repair}, constants.StatusRepairing},
		{"extracting", StateView{State: Extracting, Activity: ActUnpack}, constants.StatusExtracting},
		{"volume recovery is still extracting", StateView{State: Extracting, Activity: ActVolumeRecovery}, constants.StatusExtracting},

		{"finalizing, moving", StateView{State: Finalizing, Activity: ActMove}, constants.StatusMoving},
		{"finalizing, script", StateView{State: Finalizing, Activity: ActScript}, constants.StatusRunning},
		{"finalizing, cleanup", StateView{State: Finalizing, Activity: ActCleanup}, constants.StatusMoving},

		{"completed", StateView{State: Finished, Outcome: OutcomeOK}, constants.StatusCompleted},
		{"failed", StateView{State: Finished, Outcome: OutcomeFailed}, constants.StatusFailed},
		{"unrecoverable renders as failed", StateView{State: Finished, Outcome: OutcomeUnrecoverable}, constants.StatusFailed},
		{"cancelled renders as deleted", StateView{State: Finished, Outcome: OutcomeCancelled}, constants.StatusDeleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToSABnzbd(tc.v); got != tc.want {
				t.Errorf("ToSABnzbd(%+v) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}

// TestToSABnzbd_IsTotal walks the whole product space of the axes and fails
// if any combination yields the empty status. The shim is a boundary the API
// depends on, and an unhandled combination there is a blank status string in
// a third-party client rather than a crash — which is exactly the kind of
// failure that ships unnoticed.
func TestToSABnzbd_IsTotal(t *testing.T) {
	for _, s := range AllStates() {
		for _, a := range AllActivities() {
			for _, o := range AllOutcomes() {
				for _, r := range AllWaitReasons() {
					for _, assessed := range []bool{false, true} {
						v := StateView{State: s, Activity: a, Outcome: o, Reason: r, Assessed: assessed}
						if got := ToSABnzbd(v); got == "" {
							t.Errorf("ToSABnzbd(%+v) returned the empty status", v)
						}
					}
				}
			}
		}
	}
}

// TestToSABnzbd_EmitsOnlyDeclaredStatuses guards against a typo producing a
// status string no client knows. Every output must be a declared constant.
func TestToSABnzbd_EmitsOnlyDeclaredStatuses(t *testing.T) {
	declared := make(map[constants.Status]bool)
	for _, s := range constants.AllStatuses() {
		declared[s] = true
	}
	for _, s := range AllStates() {
		for _, a := range AllActivities() {
			for _, o := range AllOutcomes() {
				for _, r := range AllWaitReasons() {
					got := ToSABnzbd(StateView{State: s, Activity: a, Outcome: o, Reason: r})
					if !declared[got] {
						t.Errorf("ToSABnzbd emitted %q, which is not in constants.AllStatuses()", got)
					}
				}
			}
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -count=1 ./internal/job/ -run TestToSABnzbd -v`
Expected: FAIL — `undefined: ToSABnzbd`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/job/sabnzbd.go`:

```go
package job

import "github.com/hobeone/gonzbd/internal/constants"

// ToSABnzbd translates our internal machine into the legacy status vocabulary
// the /api?mode=queue contract exposes to third-party clients.
//
// This is the ONLY file in the package that imports internal/constants, and
// the translation is one-way: nothing reads a constants.Status back into the
// machine. That is the whole point of having a shim rather than storing the
// upstream vocabulary — see spec §12.
//
// It is total by construction. Every arm has a fallback, and
// TestToSABnzbd_IsTotal walks the product space of every axis to prove no
// combination yields an empty string, because an unhandled combination shows
// up as a blank status in somebody's Sonarr rather than as a crash here.
//
// Four upstream statuses that no code path of ours assigns are OUTPUTS here
// and nothing more: Grabbing and Checking are unreachable (nothing in this
// design corresponds to them), Propagating is reserved, and Fetching finally
// means what upstream documents it to mean — downloading extra par2 files for
// repair, which is exactly our Assessing → Fetching re-entry.
func ToSABnzbd(v StateView) constants.Status {
	switch v.State {
	case Waiting:
		if v.Reason.IsPause() {
			return constants.StatusPaused
		}
		return constants.StatusQueued

	case Fetching:
		// A re-entry after a verdict is fetching recovery volumes. Anything
		// before the first assessment is an ordinary download.
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

	case Finished:
		return finishedStatus(v.Outcome)
	}
	// Unreachable while AllStates() and this switch agree, which
	// TestToSABnzbd_IsTotal enforces. Queued is the least misleading answer
	// for a state we do not recognise: it claims nothing is happening.
	return constants.StatusQueued
}

func finishedStatus(o Outcome) constants.Status {
	switch o {
	case OutcomeOK:
		return constants.StatusCompleted
	case OutcomeCancelled:
		return constants.StatusDeleted
	case OutcomeFailed, OutcomeUnrecoverable:
		return constants.StatusFailed
	default:
		// A Finished attempt with a pending outcome is a bug elsewhere, not
		// something to render inventively. Failed is the safe direction: it
		// never reports success for a job whose verdict we cannot read.
		return constants.StatusFailed
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `goimports -w internal/job/sabnzbd.go && go build ./... && go test -count=1 ./internal/job/ -run TestToSABnzbd -v`
Expected: PASS, all three tests.

- [ ] **Step 5: Verify totality actually discriminates**

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/job/sabnzbd.go "$SCRATCH/sabnzbd.bak.go"
# Make one arm return the empty status.
sed -i 's/\t\treturn constants.StatusRepairing/\t\treturn ""/' internal/job/sabnzbd.go
grep -n 'return ""' internal/job/sabnzbd.go   # confirm the mutation landed
go test -count=1 ./internal/job/ -run TestToSABnzbd_IsTotal
# MUST fail with: "ToSABnzbd({State:Repairing ...}) returned the empty status"
cp "$SCRATCH/sabnzbd.bak.go" internal/job/sabnzbd.go
go test -count=1 ./internal/job/ -run TestToSABnzbd_IsTotal
```

- [ ] **Step 6: Commit**

```bash
git add internal/job/sabnzbd.go internal/job/sabnzbd_test.go
git commit -m "feat(job): add the total ToSABnzbd translation

The one file in the package that imports internal/constants, and the
translation is one-way — nothing reads a Status back into the machine.

Totality is tested rather than asserted: TestToSABnzbd_IsTotal walks the
product of every axis and fails on an empty result, because an unhandled
combination surfaces as a blank status string in a third-party client
rather than as a crash here. A second test pins that every output is a
declared constant, so a typo cannot invent a status no client knows.
Observed red: returning \"\" from the Repairing arm fails with
\"ToSABnzbd({State:Repairing ...}) returned the empty status\".

Fetching now means what upstream documents it to mean — downloading
extra par2 files for repair — which is our Assessing to Fetching
re-entry, distinguished by the attempt's latched Assessed flag.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 10: Package documentation and the full-package gate run

**Files:**
- Create: `internal/job/doc.go`
- Test: none new — this task runs the repository's whole-repo gates against the new package.

**Interfaces:**
- Consumes: everything.
- Produces: no new symbols.

- [ ] **Step 1: Write the package doc**

Create `internal/job/doc.go`:

```go
// Package job is the lifecycle machine for a download.
//
// # Three axes
//
// State is where a job's current attempt is and what may happen next.
// Activity is what is executing right now and nothing branches on it.
// Outcome is the attempt's verdict, assigned once on the edge into Finished
// and never revised. Keeping them apart is what collapses the transition
// table: "still producing, doing something else now" is an Activity write
// rather than a state change.
//
// # The boundary
//
// Fetching, Assessing and Repairing form the Correctness zone: reversible,
// idempotent, touching nothing outside the job's own working directory.
// Extracting and Finalizing form Production: forward-only and destructive —
// they delete archives, move files and run user scripts.
//
// A job crosses from Correctness to Production exactly once and never
// returns. TestBoundaryIsOneWay enumerates AllStates() and fails if any edge
// violates that, so the invariant is pinned by a test rather than by this
// sentence.
//
// That one property defines four others: pause granularity, cancel
// semantics, the acquisition lease's lifetime, and which failures are
// recoverable.
//
// # One decider
//
// Assessing is the only branching state. Everything else does work and
// returns, so every path through a job is Fetching → Assessing → one of four
// destinations, and the test surface is the verdict function rather than the
// graph. TestOnlyAssessingBranchesWithinCorrectness pins it.
//
// # Attempts
//
// The machine lives on the current Attempt, not on the Job. A Job holds a
// list of attempts, each with its own write-once Outcome, so a retry appends
// a verdict instead of revising one. An attempt opens when a lease is first
// issued and no attempt is open, and closes when it reaches Finished — pause
// and resume inside it do not end it.
//
// A Job with no attempts has never run. That is what HasRun reports, and it
// is exact where any predicate over byte counters would conflate "did not
// start" with "started and got nowhere".
//
// # What this package does not do
//
// No I/O, no locking beyond its own Job.mu, and no import of any other
// package in this repository except internal/constants — which appears in
// sabnzbd.go alone, for the one-way translation to the legacy API
// vocabulary.
//
// A Job method never calls a Queue method. That is the system-wide lock
// ordering rule, and it holds structurally here rather than by review: this
// package cannot see a Queue.
//
// The design this implements is docs/superpowers/specs/2026-08-25-job-lifecycle-design.md.
package job
```

- [ ] **Step 2: Run the full package suite with the race detector**

Run: `go test -count=1 -race ./internal/job/ -v`
Expected: PASS, every test from Tasks 1-9.

- [ ] **Step 3: Run the repository's quality gates**

```bash
goimports -w internal/job/
go fix ./...
go vet ./...
golangci-lint run ./internal/job/
go run ./scripts/check_dup_comments
go run ./scripts/check_review_banner
```

Expected: all clean. Note that `check_dup_comments` and `check_review_banner` are whole-repository, not diff-scoped, so they can report a finding in a file this plan never touched — diagnose before assuming this work caused it.

- [ ] **Step 4: Run the diff-scoped gates**

```bash
go run ./scripts/check_coverage
go run ./scripts/check_test_alignment
go run ./scripts/check_lock_io
```

Expected: clean. `check_lock_io` examines a locked span plus one level of call-graph descent; `Job`'s mutators call only `Attempt` methods, which do no I/O, so a clean run here is meaningful rather than vacuous. If `check_coverage` reports a function that looks untouched, commit first and re-run — `gitscope` unions the committed and working-tree diffs and can misattribute hunks while changes are uncommitted (issue #280).

- [ ] **Step 5: Verify the package is genuinely standalone**

```bash
go list -deps ./internal/job/ | grep 'hobeone/gonzbd'
```
Expected: exactly one line, `github.com/hobeone/gonzbd/internal/constants`. Any other repository package in that list means something leaked in and the lock-ordering rule is no longer structural.

```bash
grep -rn 'internal/job' --include='*.go' . | grep -v '^./internal/job/'
```
Expected: no output. Nothing imports this package yet, which is correct — wiring is the next plan.

- [ ] **Step 6: Commit**

```bash
git add internal/job/doc.go
git commit -m "docs(job): add the package doc for the lifecycle machine

States the three axes, the reversibility boundary, the single-decider
property and the attempt model, each naming the test that pins it rather
than asking the reader to trust the prose.

Also records what the package deliberately cannot do: no I/O, and no
import of any repository package except internal/constants in sabnzbd.go.
That is what makes the system-wide lock ordering rule structural — a Job
method cannot call a Queue method because this package cannot see a
Queue. go list -deps is the check.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## What this plan does not deliver, and what comes next

At the end of Task 10 the tree builds, every gate is green, and **nothing imports `internal/job`**. That is the intended end state: the vocabulary and machine exist and are exhaustively tested, and the old model is still running the daemon untouched.

The next plan is the swap, and it is where the deletion happens. It must be written *after* this one lands, because its tasks reference the exact signatures above:

1. Move `Manifest` and `JobProgress` into `internal/job`; add `Lease` granting a `Manifest` and a `*durability.Barrier`.
2. Add `Verdict` and `Assess` to `internal/par2` (D5), taking value types only — `[]AssembledFile`, `[]Set`, `job.Policy`, and a speculation-evidence value.
3. Build the new `Queue`: ordered index, two pools, lease issuance, reorder per D9.
4. Add the `Checkpointer` as sole database writer; make `StorageBarrier` reconcile against the disk at construction.
5. **The swap**: rewire `app`, `downloader` and `postproc`; delete `queue/status.go`, `JobPhase`, `ActiveSet`, `PromoteNext`, `evictJobLocked`, `SetStatus`/`SetStatusIf`, `SetPostProcStarted`, `Queue.Retry`, `par2NeedsRecovery`, `maybeReleaseRecoveryVolumes`, the `quickcheck` stage, `NeedRequeue`/`RequeueBlocksNeeded`, `resumeAllJobs`, `shouldSkipForPP` and `Job.PostProc`.

Dispatch inversion (§10) and DirectUnpack speculation (§11) are a third plan after that.

## Self-review

**Spec coverage for this plan's scope.** §3's three axes → Tasks 1, 4, 5. §3.1 attempts and the open/close rule → Tasks 7, 8. §3.2 `Policy` → Task 6. §4's boundary → Task 2's `TestBoundaryIsOneWay`. §5's single decider → Task 2's `TestOnlyAssessingBranchesWithinCorrectness`. §8.2's unified `Waiting` → Task 3. §12's translation → Task 9. D1 → Task 8. D2 → Tasks 7, 8. D3 → `OutcomeUnrecoverable` in Task 5. D4 → Task 6. D7 → the `attempts` field comment in Task 8.

Out of scope and deferred with a named owner above: §6 `Lease`, §7 the wider ownership map, §8.1 the pools, §9 dispatch, §10 restart, §11 speculation, D5, D6, D8, D9.

**Type consistency.** `StateView` is defined once in Task 3 and consumed unchanged in Tasks 7, 8 and 9. `Attempt`'s mutators are lowercase throughout and reached only via `Job.withOpenAttempt`. `illegalTransition` is defined in Task 2 and used in Task 7. `AllStates`/`AllActivities`/`AllOutcomes`/`AllWaitReasons` all exist before Task 9's totality test consumes them.

**Every code block is meant to compile as written.** If one does not, that is a plan defect — report it rather than inventing a fix, because a wrong guess here becomes the shape every later plan is written against.
