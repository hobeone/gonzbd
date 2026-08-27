package job

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// correctnessStates is the reachability walk's ORACLE, written as a literal
// rather than derived from IsCorrectness. That is not stylistic. A door this
// test judges now branches on IsCorrectness itself — attempt.go's transition
// refuses IsCorrectness(a.state) && IsProduction(to), returning
// ErrCrossRequired so that Cross is the only door onto that edge — so
// judging that refusal with the predicate it decides by would let a wrong
// predicate agree with itself. This test's previous doc comment named
// exactly that condition as the point at which it would need a different
// oracle (IsCorrectness is defined at transition.go:44), and task 6's
// ErrCrossRequired guard created it: `git grep -n 'Is[C]orrectness(' --
// 'internal/job/*.go' ':!internal/job/*_test.go'` now returns two lines, the
// definition and that one caller, where it previously returned only the
// definition. cross itself (also attempt.go) does not call IsCorrectness —
// it hardcodes `a.state != Assessing` — so it is not a second instance of
// this problem; it is transition's refusal alone that now shares code with
// the oracle.
//
// A literal cannot drift silently the way a shared predicate can.
// TestReachabilityOracleClassifiesEveryState below is what makes ADDING a
// state fail loudly here rather than quietly widening the oracle to a set
// nobody re-classified.
var correctnessStates = map[State]bool{
	Fetching:  true,
	Assessing: true,
	Repairing: true,
}

// TestReachabilityOracleClassifiesEveryState fails when AllStates() grows a
// member correctnessStates does not name. Without it, a new state defaults
// to "not Correctness" in checkReachableConfig and the boundary walk
// silently stops checking it.
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
//     gone now (task 5), and this escape is why: it was the universal resume
//     point in legalEdges, so an unconstrained resume out of it reached
//     Correctness from Production without any single edge doing so. Closed by
//     transition's `to != a.next` check, which survives Waiting's removal.
//   - Attempt 1 crosses into Production and finishes; BeginAttempt appends a
//     fresh Attempt, which opens in Fetching. The per-Attempt machine was
//     never violated — Job holds a LIST of them, and the boundary latch was
//     scoped one layer below the invariant. Closed by ErrBoundaryConsumed.
//
// So this test walks the configuration space by REPLAYING action sequences
// against a real *Job through the exported doors, rather than by reading the
// edge map. The oracle it judges with must not be the same predicate the code
// under test decided with, or a wrong predicate would agree with itself — see
// correctnessStates above for why that oracle is now a literal rather than
// IsCorrectness itself.
//
// Two invariants are asserted at every reachable configuration. Each is a
// history property that a.state alone cannot express, which is why each needed
// a latch and why each latch's boundary is where a defect lived:
//
//  1. Once ANY attempt of this job has crossed, the current state is not in
//     the oracle's Correctness set. (The central claim; both escapes above
//     violated it.)
//  2. Once any attempt has crossed, finish(OutcomeUnrecoverable) is refused.
//     D3 defines that verdict as "never crossed the boundary", and finish
//     overwrites a.state with Finished on the very call that could settle
//     it — so a state-based guard misses an attempt that crossed and then
//     settled. This is what the second external reviewer found in finish,
//     back when the state that erased the crossing was Waiting rather than
//     Finished; the latch this test checks is what survived that rename.
//
// A third was drafted and deleted rather than shipped: "a settled attempt is
// never also in a Correctness state" reads as an invariant but is
// unsatisfiable as written — the oracle's Correctness set names only
// Fetching, Assessing and Repairing, so `State == Finished &&
// correctnessStates[State]` can never fire. It would have sat here asserting
// nothing, which is worse than its absence, since a reader counting
// assertions would credit it.
//
// What this test does NOT cover, stated so its green is not read as wider
// than it is: it says nothing about STRANDING (an attempt reachable only by
// finish — the defect that produced ErrHoldRequired, back when Hold existed),
// because "no useful move remains" is not expressible as a predicate over one
// configuration; finish always succeeds on an open attempt, so no
// configuration is stranded by this test's definition. It also says nothing
// about Activity, which no door branches on (see setActivity).
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

	var configs, crossedConfigs int
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

				if checkReachableConfig(t, j, child) {
					crossedConfigs++
				}
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

	// configs alone cannot see the defect that motivated this floor: before
	// Cross joined allActions(), a.crossed had exactly one writer, reached
	// only through Job.Cross, so everCrossed was false at every one of the
	// walk's reachable configurations and checkReachableConfig returned
	// before either assertion ran on any of them. configs was north of 50
	// throughout — the walk was exploring widely, just never crossing — so
	// the existing floor is structurally unable to detect that shape of
	// vacuity. This counts configurations where everCrossed was actually
	// true and floors THAT separately. As with the configs floor above, the
	// number is picked far below what the walk currently reaches (observed:
	// 1885 total configurations, 76 of which had crossed, at maxDepth=7 —
	// see the commit body) so that widening the walk does not make this
	// brittle, while a collapse back to zero or a handful — the exact shape
	// of the defect this guards against — fails loudly.
	if crossedConfigs < 20 {
		t.Fatalf("only %d of %d distinct configurations had ever crossed the boundary; "+
			"that is too few to show the crossed-only assertions in checkReachableConfig were "+
			"meaningfully exercised, and is exactly the shape of vacuity the plain configs floor "+
			"above cannot see — Cross not appearing in allActions() previously left crossedConfigs "+
			"at zero while configs stayed well above its own floor", crossedConfigs, configs)
	}
	t.Logf("walked %d distinct configurations to depth %d, %d of which had crossed the boundary",
		configs, maxDepth, crossedConfigs)
}

// checkReachableConfig judges one reachable configuration. It reads the
// latches directly (this is an in-package test) because the invariants are
// about history, and history is exactly what StateView does not carry. It
// reports whether this configuration had ever crossed, so the caller can
// floor how many configurations actually exercised the two assertions below
// (see the crossedConfigs floor in TestBoundaryIsUnreachableByAnyPath) — a
// configuration that has not crossed returns before reaching either one.
func checkReachableConfig(t *testing.T, j *Job, seq []action) bool {
	t.Helper()

	var everCrossed bool
	for i := range j.attempts {
		if j.attempts[i].crossed {
			everCrossed = true
			break
		}
	}
	if !everCrossed {
		return false
	}

	v := j.State()

	// (1) the central claim: crossed once, never back in a Correctness state
	// as classified by the literal oracle above, not by IsCorrectness itself.
	if correctnessStates[v.State] {
		t.Errorf("reachable configuration violates the one-way boundary: %s\n"+
			"an attempt of this job crossed into Production, yet the job is now in %s, "+
			"which is a Correctness state (spec §4)", trace(seq), v.State)
	}

	// (2) Unrecoverable must stay refused past the boundary, including across
	// a settle that has overwritten a.state with Finished. Probed on a
	// REPLAYED job so the attempt is not consumed for the walk's other
	// successors.
	probe := New("probe", "probe", Policy{})
	base := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	for i, a := range seq {
		a.apply(probe, base.Add(time.Duration(i)*time.Second))
	}
	_, err := probe.Finish(OutcomeUnrecoverable, base.Add(time.Hour))
	if err == nil {
		t.Errorf("reachable configuration accepted OutcomeUnrecoverable after crossing: %s\n"+
			"D3 defines Unrecoverable as \"never crossed the boundary\"; recording it here "+
			"would contradict BeginAttempt's own refusal to reopen this job", trace(seq))
	} else if !errors.Is(err, ErrUnrecoverableAfterBoundary) && !errors.Is(err, ErrNoOpenAttempt) {
		t.Errorf("reachable configuration refused OutcomeUnrecoverable for the wrong reason: %s\n"+
			"got %v, want ErrUnrecoverableAfterBoundary (crossed) or ErrNoOpenAttempt (already settled)",
			trace(seq), err)
	}
	return true
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
	// 1 job-level door (BeginAttempt) + 1 more (Grant) + one Transition, one
	// SetNext and one Cross per state + one Finish per outcome + one
	// SetIntent per intent. Sized rather than grown so the walk's branching
	// factor is stated in one place. Hold and SetWaitReason are gone with
	// Waiting (task 5); this task (7) adds SetNext, Cross, SetIntent and
	// Grant, which is what makes the boundary reachable at all — without
	// Cross, a.crossed has no writer this walk can reach and
	// checkReachableConfig's assertions never fire (see the crossedConfigs
	// floor above), and without SetNext, Cross can never fire either, since
	// it only succeeds from Assessing with a matching next already recorded.
	acts := make([]action, 0, 2+3*len(AllStates())+len(AllOutcomes())+len(AllIntents()))
	acts = append(acts,
		action{"BeginAttempt", func(j *Job, now time.Time) { _ = j.BeginAttempt(now) }},
		action{"Grant", func(j *Job, _ time.Time) { _ = j.Grant(&Lease{}) }},
	)
	for _, s := range AllStates() {
		acts = append(acts, action{
			name:  fmt.Sprintf("Transition(%s)", s),
			apply: func(j *Job, _ time.Time) { _ = j.Transition(s) },
		})
	}
	for _, s := range AllStates() {
		acts = append(acts, action{
			name:  fmt.Sprintf("SetNext(%s)", s),
			apply: func(j *Job, _ time.Time) { _ = j.SetNext(s) },
		})
	}
	for _, s := range AllStates() {
		// Cross(s) is offered for every state, not only Extracting and
		// Finalizing, because the walk must be able to attempt an ILLEGAL
		// crossing (e.g. Cross(Fetching)) and see it refused, not only a
		// legal one — see cross's own validation in attempt.go.
		acts = append(acts, action{
			name:  fmt.Sprintf("Cross(%s)", s),
			apply: func(j *Job, _ time.Time) { _, _ = j.Cross(s) },
		})
	}
	for _, o := range AllOutcomes() {
		acts = append(acts, action{
			name:  fmt.Sprintf("Finish(%s)", o),
			apply: func(j *Job, now time.Time) { _, _ = j.Finish(o, now) },
		})
	}
	for _, i := range AllIntents() {
		acts = append(acts, action{
			name:  fmt.Sprintf("SetIntent(%s)", i),
			apply: func(j *Job, _ time.Time) { _ = j.SetIntent(i) },
		})
	}
	return acts
}

// configKey is the dedup identity: every field any door reads to make a
// decision, plus the attempt count. A field omitted here would collapse two
// genuinely different configurations into one and silently prune whichever
// arrived second — so this deliberately includes the latches (assessed,
// crossed) that StateView does not carry, and now j.intent and
// j.lease != nil, which SetIntent's latch check and Grant/surrenderLocked
// read respectively. j.pending is gone with Waiting (task 5): a never-run
// job no longer carries a wait reason of its own.
func configKey(j *Job) string {
	j.mu.RLock()
	defer j.mu.RUnlock()

	key := fmt.Sprintf("n=%d/intent=%v/leased=%v", len(j.attempts), j.intent, j.lease != nil)
	for i := range j.attempts {
		a := &j.attempts[i]
		key += fmt.Sprintf("|%v,%v,%v,%v,%v,%v",
			a.state, a.next, a.activity, a.outcome, a.assessed, a.crossed)
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
