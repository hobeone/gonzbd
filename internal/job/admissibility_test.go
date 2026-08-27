package job

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestAdmissibleAt_IsTotal is the exhaustiveness gate. It fails when an Outcome
// or a State is added and nobody classified the new cells — which is the
// failure mode the two ad-hoc guards had, in the form "nobody wrote the cell at
// all". A table with a hole is not an owner.
func TestAdmissibleAt_IsTotal(t *testing.T) {
	for _, o := range AllOutcomes() {
		if o == OutcomePending {
			// Not a verdict. finish rejects it before consulting the table, so
			// the table deliberately has no row — asserted, not skipped.
			if _, ok := admissibleAt[o]; ok {
				t.Errorf("admissibleAt has a row for %v; it is not a verdict and finish refuses it earlier", o)
			}
			continue
		}
		row, ok := admissibleAt[o]
		if !ok {
			t.Errorf("admissibleAt has no row for %v; every settled outcome must name the positions that admit it", o)
			continue
		}
		for _, s := range row {
			if !slices.Contains(AllStates(), s) {
				t.Errorf("admissibleAt[%v] names %v, which AllStates() does not declare", o, s)
			}
		}
	}
	for o := range admissibleAt {
		if !slices.Contains(AllOutcomes(), o) {
			t.Errorf("admissibleAt has a row for %v, which AllOutcomes() does not declare", o)
		}
	}
}

// TestAdmissibleAt_MatchesTheSpec asserts each row against the clause it comes
// from. Totality (above) proves the table has no holes; this proves the holes
// were filled with the right values. Neither is sufficient alone: a table that
// is total and wrong is exactly what shipped as two hand-written guards.
func TestAdmissibleAt_MatchesTheSpec(t *testing.T) {
	for _, tc := range []struct {
		o      Outcome
		admits []State
		why    string
	}{
		{OutcomeOK, []State{Finalizing},
			"§3.3's work-spine table: Extracting completes with SetNext(Finalizing), " +
				"and Finish(OutcomeOK) is Finalizing's row alone"},
		{OutcomeFailed, []State{Fetching, Assessing, Repairing, Extracting, Finalizing},
			"§3.3's failure table: every state either continues or settles Failed"},
		{OutcomeUnrecoverable, []State{Fetching, Assessing, Repairing},
			"D3: Unrecoverable means the job never crossed the boundary"},
		{OutcomeCancelled, []State{Fetching, Assessing, Repairing, Extracting, Finalizing},
			"§5.5 and §5.12 both settle Cancelled from Extracting when running(j) is false"},
	} {
		t.Run(tc.o.String(), func(t *testing.T) {
			for _, s := range AllStates() {
				want := slices.Contains(tc.admits, s)
				if got := admits(tc.o, s); got != want {
					t.Errorf("admits(%v, %v) = %v, want %v — %s", tc.o, s, got, want, tc.why)
				}
			}
		})
	}
}

// TestFinish_AgreesWithTheTableAtEveryCell walks Outcome × State and asserts
// finish accepts exactly the cells admissibleAt names. This is the test the
// two ad-hoc guards never had: the guard for OutcomeOK was wrong for one
// state out of five, and every test that existed either used a state where it
// happened to be right or asserted the wrong behaviour outright.
//
// It judges with the table, which is legitimate ONLY because the table is the
// thing under test in TestAdmissibleAt_IsTotal and TestAdmissibleAt_MatchesTheSpec
// above — those pin the table against AllStates()/AllOutcomes() and against
// named spec clauses, so this test is checking that the CODE follows the
// table, not that the table agrees with itself.
func TestFinish_AgreesWithTheTableAtEveryCell(t *testing.T) {
	for _, s := range AllStates() {
		for _, o := range AllOutcomes() {
			if o == OutcomePending {
				continue // refused earlier, by IsSettled; covered separately
			}
			t.Run(s.String()+"/"+o.String(), func(t *testing.T) {
				a := attemptAt(t, s)
				err := a.finish(o, testClock())
				if admits(o, s) && err != nil {
					t.Errorf("finish(%v) at %v = %v, want nil — the table admits this cell", o, s, err)
				}
				if !admits(o, s) && err == nil {
					t.Errorf("finish(%v) at %v = nil, want a refusal — the table does not admit this cell", o, s)
				}
			})
		}
	}
}

// attemptAt returns an open attempt sitting at s, driven through the real
// doors rather than constructed field-by-field, so a state this helper cannot
// reach is a state the machine cannot reach.
func attemptAt(t *testing.T, s State) Attempt {
	t.Helper()
	a := newAttempt(testClock())
	switch s {
	case Fetching:
	case Assessing:
		mustTransition(t, &a, Assessing)
	case Repairing:
		mustTransition(t, &a, Assessing)
		mustTransition(t, &a, Repairing)
	case Extracting:
		mustTransition(t, &a, Assessing)
		mustCross(t, &a, Extracting)
	case Finalizing:
		mustTransition(t, &a, Assessing)
		mustCross(t, &a, Extracting)
		mustTransition(t, &a, Finalizing)
	default:
		t.Fatalf("attemptAt has no arm for %v; add one rather than skipping the state", s)
	}
	return a
}

// TestInadmissible_CarriesTheSentinelTheRuleEarned walks every cell the table
// REFUSES and asserts the error names the right reason. The two sentinels are
// independent — neither wraps the other (attempt.go) — so a caller can tell
// "this verdict contradicts where the attempt got to" from "this verdict is
// not valid here", and collapsing them would silently widen what a caller
// matching ErrUnrecoverableAfterBoundary sees.
//
// It walks rather than spot-checks because the mapping is per-outcome: a new
// outcome added to admissibleAt with no arm in inadmissible would otherwise
// report ErrInvalidOutcome by default and nobody would have decided that.
func TestInadmissible_CarriesTheSentinelTheRuleEarned(t *testing.T) {
	var refused int
	for _, o := range AllOutcomes() {
		if o == OutcomePending {
			continue // no row; finish refuses it before the table
		}
		for _, s := range AllStates() {
			if admits(o, s) {
				continue
			}
			refused++
			err := inadmissible(o, s)
			if err == nil {
				t.Fatalf("inadmissible(%v, %v) = nil; every refused cell must name a reason", o, s)
			}
			wantUnrecoverable := o == OutcomeUnrecoverable
			if got := errors.Is(err, ErrUnrecoverableAfterBoundary); got != wantUnrecoverable {
				t.Errorf("inadmissible(%v, %v): errors.Is(ErrUnrecoverableAfterBoundary) = %v, want %v — %v",
					o, s, got, wantUnrecoverable, err)
			}
			if got := errors.Is(err, ErrInvalidOutcome); got != !wantUnrecoverable {
				t.Errorf("inadmissible(%v, %v): errors.Is(ErrInvalidOutcome) = %v, want %v — %v",
					o, s, got, !wantUnrecoverable, err)
			}
			// The non-boundary message earns its keep by naming where the
			// outcome IS admissible; without that a caller sees a refusal and
			// no route to a legal call.
			if !wantUnrecoverable {
				for _, ok := range admissibleAt[o] {
					if !strings.Contains(err.Error(), ok.String()) {
						t.Errorf("inadmissible(%v, %v) = %q; does not name %v, where %v is admissible",
							o, s, err, ok, o)
					}
				}
			}
		}
	}
	// Without this the loops above pass vacuously if the table ever admits
	// everything, which is precisely the regression that would matter.
	if refused == 0 {
		t.Fatal("no cell was refused; this test asserted nothing")
	}
}
