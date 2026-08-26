package job

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestBoundaryIsUnreachableByAnyPath is the owner-shaped counterpart to
// TestBoundaryIsOneWay, and exists because that test cannot see the defects
// this one is built to catch.
//
// TestBoundaryIsOneWay (transition_test.go) enumerates AllStates() and
// asserts no Production->Correctness pair is a legal EDGE. That is a property
// of legalEdges. The invariant the design actually claims — a job crosses
// once and never returns — is a property of REACHABILITY, and two individually
// legal edges can compose into the transit the graph forbids directly. Both
// escapes found on this branch by external review were exactly that shape,
// and TestBoundaryIsOneWay passed throughout:
//
//   - Extracting -> hold(...) -> Waiting -> transition(Fetching). Waiting is
//     the universal resume point in legalEdges, so an unconstrained resume out
//     of it reached Correctness from Production without any single edge doing
//     so. Closed by transition's `to != a.next` check.
//   - Attempt 1 crosses into Production and finishes; BeginAttempt appends a
//     fresh Attempt, which opens in Fetching. The per-Attempt machine was
//     never violated — Job holds a LIST of them, and the boundary latch was
//     scoped one layer below the invariant. Closed by ErrBoundaryConsumed.
//
// So this test walks the configuration space by REPLAYING action sequences
// against a real *Job through the exported doors, rather than by reading the
// edge map. The oracle it judges with must not be the same predicate the code
// under test decided with, or a wrong predicate would agree with itself: the
// assertions below use IsCorrectness, which has no non-test caller at all:
// `git grep -n 'Is[C]orrectness(' -- 'internal/job/*.go'
// ':!internal/job/*_test.go'` returns exactly one line, its own definition at
// transition.go:84. It is consulted only from tests, where transition and
// finish decide with CanTransition and IsProduction instead.
//
// That citation is shaped three ways on purpose: the bracketed C stops it
// matching its own text, the trailing paren excludes the four prose mentions
// of the name in this very comment block, and the _test.go exclusion is what
// turns eleven lines a reader must filter into the one line that actually
// carries the claim. That independence is a
// property of the current tree, not something enforced; if a door ever starts
// branching on IsCorrectness, this test needs a different oracle.
//
// Two invariants are asserted at every reachable configuration. Each is a
// history property that a.state alone cannot express, which is why each needed
// a latch and why each latch's boundary is where a defect lived:
//
//  1. Once ANY attempt of this job has crossed, the current state is not a
//     Correctness state. (The central claim; both escapes above violated it.)
//  2. Once any attempt has crossed, finish(OutcomeUnrecoverable) is refused.
//     D3 defines that verdict as "never crossed the boundary", and hold
//     overwrites a.state with Waiting — so a state-based guard misses an
//     attempt that crossed and was then held. This is what the second external
//     reviewer found in finish.
//
// A third was drafted and deleted rather than shipped: "a settled attempt is
// never also in a Correctness state" reads as an invariant but is
// unsatisfiable as written — IsCorrectness is true only for Fetching,
// Assessing and Repairing, so `State == Finished && IsCorrectness(State)` can
// never fire. It would have sat here asserting nothing, which is worse than
// its absence, since a reader counting assertions would credit it.
//
// What this test does NOT cover, stated so its green is not read as wider
// than it is: it says nothing about STRANDING (an attempt reachable only by
// finish — the defect that produced ErrHoldRequired), because "no useful move
// remains" is not expressible as a predicate over one configuration; finish
// always succeeds on an open attempt, so no configuration is stranded by this
// test's definition. It also says nothing about Activity, which no door
// branches on (see setActivity).
func TestBoundaryIsUnreachableByAnyPath(t *testing.T) {
	const (
		maxDepth    = 7
		maxAttempts = 3
	)

	base := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)

	// replay rebuilds a job from scratch by applying seq in order. Replaying
	// rather than cloning is deliberate: *Job holds a sync.RWMutex and only
	// unexported fields, so a copy would either be a lock-copy vet error or a
	// hand-written duplicate of Job's own layout — a second constructor for
	// the type under test, which is the owner-model violation AGENTS.md names
	// directly. Replay costs O(depth) per node and needs no such duplicate.
	replay := func(seq []action) *Job {
		j := New("j", "job", Policy{})
		for i, a := range seq {
			a.apply(j, base.Add(time.Duration(i)*time.Second))
		}
		return j
	}

	// The frontier holds action sequences, deduplicated by the configuration
	// they produce, so the walk is bounded by the reachable config space
	// rather than by 21^maxDepth.
	seen := map[string]bool{}
	frontier := [][]action{{}}
	seen[configKey(replay(nil))] = true

	var configs int
	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		var next [][]action
		for _, seq := range frontier {
			for _, act := range allActions() {
				child := append(append([]action{}, seq...), act)
				j := replay(child)

				if len(j.attempts) > maxAttempts {
					continue
				}
				key := configKey(j)
				if seen[key] {
					continue
				}
				seen[key] = true
				configs++

				checkReachableConfig(t, j, child)
				next = append(next, child)
			}
		}
		frontier = next
	}

	// A walk that reached almost nothing would pass all three assertions
	// vacuously and read identically to a real clean run — the same failure
	// mode scanOutcomeWriters and TestToSABnzbd_IsTotal guard against. The
	// floor is deliberately far below the observed count rather than pinned
	// to it, so adding a legal state or door widens the walk without failing
	// here, while a change that collapses it to a handful of configs does.
	if configs < 50 {
		t.Fatalf("the walk reached only %d distinct configurations; that is too few for the "+
			"assertions above to have been meaningfully exercised, and a vacuous pass here "+
			"would look identical to a real one", configs)
	}
	t.Logf("walked %d distinct configurations to depth %d", configs, maxDepth)
}

// checkReachableConfig judges one reachable configuration. It reads the
// latches directly (this is an in-package test) because the invariants are
// about history, and history is exactly what StateView does not carry.
func checkReachableConfig(t *testing.T, j *Job, seq []action) {
	t.Helper()

	var everCrossed bool
	for i := range j.attempts {
		if j.attempts[i].crossed {
			everCrossed = true
			break
		}
	}
	if !everCrossed {
		return
	}

	v := j.State()

	// (1) the central claim: crossed once, never back in Correctness.
	if IsCorrectness(v.State) {
		t.Errorf("reachable configuration violates the one-way boundary: %s\n"+
			"an attempt of this job crossed into Production, yet the job is now in %s, "+
			"which is a Correctness state (spec §4)", trace(seq), v.State)
	}

	// (2) Unrecoverable must stay refused past the boundary, including across
	// a hold that has overwritten a.state with Waiting. Probed on a REPLAYED
	// job so the attempt is not consumed for the walk's other successors.
	probe := New("probe", "probe", Policy{})
	base := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	for i, a := range seq {
		a.apply(probe, base.Add(time.Duration(i)*time.Second))
	}
	err := probe.Finish(OutcomeUnrecoverable, base.Add(time.Hour))
	if err == nil {
		t.Errorf("reachable configuration accepted OutcomeUnrecoverable after crossing: %s\n"+
			"D3 defines Unrecoverable as \"never crossed the boundary\"; recording it here "+
			"would contradict BeginAttempt's own refusal to reopen this job", trace(seq))
	} else if !errors.Is(err, ErrUnrecoverableAfterBoundary) && !errors.Is(err, ErrNoOpenAttempt) {
		t.Errorf("reachable configuration refused OutcomeUnrecoverable for the wrong reason: %s\n"+
			"got %v, want ErrUnrecoverableAfterBoundary (crossed) or ErrNoOpenAttempt (already settled)",
			trace(seq), err)
	}
}

// action is one call through a public door. SetActivity is deliberately
// absent: setActivity is unvalidated and nothing branches on Activity, so
// including it would multiply the walk by 10 without reaching a configuration
// any invariant here can distinguish.
type action struct {
	name  string
	apply func(*Job, time.Time)
}

func allActions() []action {
	// 2 job-level doors + one Transition and one Hold per state + one Finish
	// per outcome. Sized rather than grown so the walk's branching factor is
	// stated in one place.
	acts := make([]action, 0, 2+2*len(AllStates())+len(AllOutcomes()))
	acts = append(acts,
		action{"BeginAttempt", func(j *Job, now time.Time) { _ = j.BeginAttempt(now) }},
		action{"SetWaitReason(GlobalPause)", func(j *Job, _ time.Time) { _ = j.SetWaitReason(GlobalPause) }},
	)
	for _, s := range AllStates() {
		acts = append(acts, action{
			name:  fmt.Sprintf("Transition(%s)", s),
			apply: func(j *Job, _ time.Time) { _ = j.Transition(s) },
		})
		acts = append(acts, action{
			name:  fmt.Sprintf("Hold(%s)", s),
			apply: func(j *Job, _ time.Time) { _ = j.Hold(s, NoComputeSlot) },
		})
	}
	for _, o := range AllOutcomes() {
		acts = append(acts, action{
			name:  fmt.Sprintf("Finish(%s)", o),
			apply: func(j *Job, now time.Time) { _ = j.Finish(o, now) },
		})
	}
	return acts
}

// configKey is the dedup identity: every field any door reads to make a
// decision, plus the attempt count. A field omitted here would collapse two
// genuinely different configurations into one and silently prune whichever
// arrived second — so this deliberately includes the latches (assessed,
// crossed, pending) that StateView does not carry.
func configKey(j *Job) string {
	j.mu.RLock()
	defer j.mu.RUnlock()

	key := fmt.Sprintf("n=%d/pending=%v", len(j.attempts), j.pending)
	for i := range j.attempts {
		a := &j.attempts[i]
		key += fmt.Sprintf("|%v,%v,%v,%v,%v,%v,%v",
			a.state, a.next, a.reason, a.activity, a.outcome, a.assessed, a.crossed)
	}
	return key
}

func trace(seq []action) string {
	if len(seq) == 0 {
		return "(the initial configuration)"
	}
	out := ""
	for i, a := range seq {
		if i > 0 {
			out += " -> "
		}
		out += a.name
	}
	return out
}
