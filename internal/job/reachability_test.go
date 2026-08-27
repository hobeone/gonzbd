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
// oracle, and task 6's ErrCrossRequired guard created it by making transition
// branch on IsCorrectness.
//
// That caller is GONE: change 02 moved the door into the edge table, so
// transition resolves edgeFrom(a.state, to) and checks e.door instead. `git
// grep -n 'Is[C]orrectness(' -- 'internal/job/*.go' ':!internal/job/*_test.go'`
// now returns one line, the definition alone. The literal oracle stays
// regardless — it was adopted because a shared predicate CAN agree with
// itself, and reverting it every time the last caller happens to disappear
// would make the test's independence a matter of luck. What follows describes
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

// productionStates is the other half of the oracle, and is package-level for
// the same reason correctnessStates is: checkReachableConfig judges "has this
// job been in Production" with it, replaying the sequence rather than reading
// crossed(), so the code's own answer to "has this crossed" is not both the
// thing under test and the thing testing it.
var productionStates = map[State]bool{
	Extracting: true,
	Finalizing: true,
}

// TestReachabilityOracleClassifiesEveryState fails when AllStates() grows a
// member that NEITHER correctnessStates nor productionStates names — naming it
// in either one satisfies the test, because either is a deliberate
// classification. Without this, a new state is silently non-Correctness to
// checkReachableConfig and silently non-Production to the walk's
// everProduction accumulator, so the boundary walk stops checking it while
// still reporting PASS. There is no terminal member to skip: settling is an
// Outcome fact, so every State belongs to one zone or the other.
func TestReachabilityOracleClassifiesEveryState(t *testing.T) {
	for _, s := range AllStates() {
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
// Three invariants are asserted at every reachable configuration:
//
//  0. A job that reached Production during the sequence reports crossed().
//     This is the assertion change 03 owes. crossed() was a latch — a bool
//     nothing could clear — and is now IsProduction(a.state), which is only
//     equivalent because no edge runs from Production back to Correctness.
//     Asserting it against REPLAYED history rather than against the predicate
//     itself is what keeps that from being a claim the code checks about
//     itself.
//  1. Once ANY attempt of this job has crossed, the current state is not in
//     the oracle's Correctness set. (The central claim; both escapes above
//     violated it.)
//  2. Once any attempt has crossed, finish(OutcomeUnrecoverable) is refused.
//     D3 defines that verdict as "never crossed the boundary". This needed a
//     latch for as long as finish overwrote a.state on the very call that
//     settled the attempt, erasing the crossing a state-based guard would have
//     read — the defect a second external reviewer found in finish, back when
//     the erasing state was Waiting. finish no longer overwrites a.state at
//     all, so the position survives settling and the state-based guard is
//     correct. That is precisely what made the latch deletable.
//
// A third was drafted and deleted rather than shipped: "a settled attempt is
// never also in a Correctness state". It was unsatisfiable when drafted,
// because a settled attempt was in a terminal state that the Correctness set
// did not name, so the conjunction could never fire — it would have sat here
// asserting nothing, which is worse than its absence, since a reader counting
// assertions would credit it. It is now worse than unsatisfiable: it is FALSE.
// An attempt that settles OutcomeFailed from Fetching is settled AND in a
// Correctness state, which is exactly the shape TestToSABnzbd's settled rows
// now exercise.
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

	// The oracle for "this job has been in Production" is REPLAYED HISTORY
	// judged by the literal productionStates map — deliberately not
	// crossed(). Reading it would repeat, one level deeper, the mistake the
	// correctnessStates literal exists to avoid, and change 03 made the repeat
	// WORSE rather than better. While crossed was a latch written only by
	// cross, a latch-based oracle could at least see every escape that went
	// through Cross. Now that crossed() is IsProduction(a.state), it reports
	// on where the job is standing RIGHT NOW — so an escape that reaches
	// Extracting and then returns to Fetching, which is the exact shape this
	// test exists to catch, leaves crossed() false. This function would return
	// early and both assertions below would run on nothing: on precisely the
	// configurations that constitute the escape.
	//
	// That is not hypothetical. Deleting transition's ErrCrossRequired guard
	// (attempt.go) opens exactly that route, and against a latch-based oracle
	// this test walked 1999 configurations, reported the same 76 crossed, and
	// PASSED, while
	//   BeginAttempt → Transition(Assessing) → Transition(Extracting)
	//   → Transition(Finalizing) → Finish(OK) → BeginAttempt
	// left a job in Fetching having been in Production — escape #2's shape.
	// Only the two single-edge tests went red, which is the same class of
	// test that missed both prior escapes.
	//
	// Replaying costs O(depth) per configuration and the walk runs in ~20ms,
	// so the price of an independent oracle is not worth optimising away.
	base := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	probe := New("probe", "probe", Policy{})
	var everProduction bool
	for i, a := range seq {
		a.apply(probe, base.Add(time.Duration(i)*time.Second))
		if productionStates[probe.State().State] {
			everProduction = true
		}
	}
	if !everProduction {
		return false
	}

	v := j.State()

	// (0) crossed() must agree with what actually happened.
	//
	// This is the assertion change 03 owes. crossed() used to be a latch,
	// independent of the graph; it is now IsProduction(a.state), which agrees
	// with history ONLY because no edge runs from Production back to
	// Correctness. BeginAttempt's reopen refusal reads it, so if the
	// derivation and the history could ever disagree, a job that had written
	// files could be reopened.
	//
	// everProduction is accumulated by REPLAYING the sequence through the real
	// doors and testing each position against the literal productionStates
	// map. It is not read back from crossed(), so this compares two
	// independently-derived answers rather than one answer with itself — the
	// same reason assertion (1) below replays rather than reading the latch.
	if !j.currentCrossedForTest() {
		t.Errorf("reachable configuration reached Production during the sequence but reports "+
			"crossed() == false: %s\nthe job is now in %s. crossed() is derived from the "+
			"position, so this means the position no longer records a crossing that happened — "+
			"BeginAttempt would reopen a job that has already written files", trace(seq), v.State)
	}

	// (1) the central claim: crossed once, never back in a Correctness state
	// as classified by the literal oracle above, not by IsCorrectness itself.
	if correctnessStates[v.State] {
		t.Errorf("reachable configuration violates the one-way boundary: %s\n"+
			"this job reached a Production state during the sequence above, yet it is now in %s, "+
			"which is a Correctness state (spec §4). Note the premise is a REPLAYED Production "+
			"visit, not crossed() — a route that entered Production and left again reads "+
			"crossed() == false and violates this just as surely", trace(seq), v.State)
	}

	// (2) Unrecoverable must stay refused past the boundary, including after
	// the attempt has settled. Probed on a REPLAYED job so the attempt is not
	// consumed for the walk's other successors.
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
	// Cross no action in this walk can reach a Production state, so
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
			a.state, a.next, a.activity, a.outcome, a.assessed, a.crossed())
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
