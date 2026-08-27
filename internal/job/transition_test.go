package job

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestCanTransition(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to State
		want     bool
	}{
		{"fetch done", Fetching, Assessing, true},
		{"needs more blocks", Assessing, Fetching, true},
		{"repairable", Assessing, Repairing, true},
		{"re-verify after repair", Repairing, Assessing, true},
		{"cross the boundary", Assessing, Extracting, true},
		{"produce", Extracting, Finalizing, true},

		{"self is not a legal edge", Fetching, Fetching, false},

		{"no reverse across the boundary", Extracting, Assessing, false},
		{"no reverse across the boundary, far", Finalizing, Fetching, false},
		{"no skipping assessment", Fetching, Extracting, false},
		{"no repair without a verdict", Fetching, Repairing, false},
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
// one successor — every other state does work and returns to
// the hub.
func TestOnlyAssessingBranchesWithinCorrectness(t *testing.T) {
	for _, from := range AllStates() {
		if !IsCorrectness(from) || from == Assessing {
			continue
		}
		var successors []State
		for _, to := range AllStates() {
			if to == from {
				continue
			}
			if CanTransition(from, to) {
				successors = append(successors, to)
			}
		}
		if len(successors) != 1 {
			t.Errorf("%s has %d work successors %v; every non-Assessing Correctness state must have exactly one (spec §5)",
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
		{Fetching, true, false},
		{Assessing, true, false},
		{Repairing, true, false},
		{Extracting, false, true},
		{Finalizing, false, true},
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

// TestLegalEdgesIsTheWorkSpine asserts the graph's exact contents. The previous
// partition test classified edges into cancel/pause/resume/spine buckets; with
// Waiting and the settled state gone there is one bucket, so a partition
// rule would be a tautology. A literal is honest at this size and fails loudly
// when an edge moves.
func TestLegalEdgesIsTheWorkSpine(t *testing.T) {
	want := map[State][]edge{
		Fetching:   {{Assessing, byTransition}},
		Assessing:  {{Fetching, byTransition}, {Repairing, byTransition}, {Extracting, byCross}},
		Repairing:  {{Assessing, byTransition}},
		Extracting: {{Finalizing, byTransition}},
		Finalizing: {},
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

	// Exactly one edge crosses the boundary, and it is the one marked byCross.
	//
	// Both halves matter, and they are checked against each OTHER rather than
	// each against the literal above. The door is now what routes: transition
	// refuses an edge it may not take and cross refuses one it may not take,
	// neither consulting a zone predicate. So a byCross marking that drifted
	// away from the zone geometry would silently move the boundary while every
	// other test still passed. Deriving one from the zone predicates and one
	// from the table, then requiring they name the SAME edge, is what makes
	// that drift loud.
	var byZone, byMark []string
	for from, tos := range legalEdges {
		for _, e := range tos {
			if IsCorrectness(from) && IsProduction(e.to) {
				byZone = append(byZone, from.String()+"->"+e.to.String())
			}
			if e.door == byCross {
				byMark = append(byMark, from.String()+"->"+e.to.String())
			}
		}
	}
	slices.Sort(byZone)
	slices.Sort(byMark)
	if len(byZone) != 1 {
		t.Errorf("edges crossing zones = %v, want exactly one (Assessing->Extracting)", byZone)
	}
	if !slices.Equal(byZone, byMark) {
		t.Errorf("edges marked byCross = %v, but edges that actually cross the zones = %v; "+
			"the door is what routes now, so a marking that disagrees with the geometry "+
			"moves the boundary without any other test noticing", byMark, byZone)
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

// TestEdgeFrom is the lookup both moving doors now depend on, so its failure
// modes are theirs. Three of them matter: a missing edge must be reported as
// missing rather than as a zero edge (which would name StateUnset as a legal
// destination); a source with no outgoing edges must not panic; and the door
// must come back attached, since that is the whole reason the table changed
// shape.
func TestEdgeFrom(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to State
		wantOK   bool
		wantDoor door
	}{
		{"ordinary work edge", Fetching, Assessing, true, byTransition},
		{"the boundary edge", Assessing, Extracting, true, byCross},
		{"backward within Correctness", Assessing, Fetching, true, byTransition},
		{"inside Production", Extracting, Finalizing, true, byTransition},
		{"non-edge across the zones", Fetching, Extracting, false, 0},
		{"non-edge within a zone", Fetching, Repairing, false, 0},
		{"reverse of the boundary edge", Extracting, Assessing, false, 0},
		{"out of a source with no edges", Finalizing, Fetching, false, 0},
		{"the sentinel is never a destination", Assessing, StateUnset, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, ok := edgeFrom(tc.from, tc.to)
			if ok != tc.wantOK {
				t.Fatalf("edgeFrom(%v, %v) ok = %v, want %v", tc.from, tc.to, ok, tc.wantOK)
			}
			if !ok {
				if e != (edge{}) {
					t.Errorf("a missing edge returned %+v, want the zero edge; a caller reading .to "+
						"would be told %v is a legal destination", e, e.to)
				}
				return
			}
			if e.to != tc.to {
				t.Errorf("edgeFrom(%v, %v).to = %v, want %v", tc.from, tc.to, e.to, tc.to)
			}
			if e.door != tc.wantDoor {
				t.Errorf("edgeFrom(%v, %v).door = %v, want %v — the door is what routes, so this "+
					"decides which of transition and cross may take the edge", tc.from, tc.to, e.door, tc.wantDoor)
			}
		})
	}
}

// TestWrongDoor pins that cross's refusal of an edge belonging to transition
// is an ErrIllegalTransition naming both states — a caller that matched only
// on the sentinel would otherwise be told nothing about which move it asked
// for, and this is the arm that replaced three separate messages.
func TestWrongDoor(t *testing.T) {
	err := wrongDoor(Assessing, Fetching)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("wrongDoor = %v, want it to wrap ErrIllegalTransition", err)
	}
	for _, want := range []string{"Assessing", "Fetching", "transition takes it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("wrongDoor = %q, want it to contain %q", err, want)
		}
	}
}

// TestWrongDoorErrorsAreMatchable pins that both wrong-door refusals carry a
// sentinel a caller can match on, and that both also match the general
// ErrIllegalTransition.
//
// The two are counterparts — one for each direction — and wrongDoor's own
// comment says so, but only one of them was matchable. ErrCrossRequired was a
// bare errors.New, so a caller written as
//
//	if errors.Is(err, ErrIllegalTransition) { ... }
//
// handled every refused state change EXCEPT the boundary edge, which is the
// one most worth handling. The asymmetry was invisible from either site: each
// error reads correctly on its own, and only holding both up together shows
// that one wraps and the other does not.
func TestWrongDoorErrorsAreMatchable(t *testing.T) {
	t.Run("transition onto the cross edge", func(t *testing.T) {
		a := newAttempt(testClock())
		mustTransition(t, &a, Assessing)
		err := a.transition(Extracting)
		if !errors.Is(err, ErrCrossRequired) {
			t.Errorf("transition(Extracting) = %v, want ErrCrossRequired", err)
		}
		if !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("transition(Extracting) = %v, want it to also match ErrIllegalTransition — "+
				"a caller handling refused state changes generally must not have to know "+
				"the boundary edge is a special case", err)
		}
	})
	t.Run("cross onto a transition edge", func(t *testing.T) {
		a := newAttempt(testClock())
		mustTransition(t, &a, Assessing)
		err := a.cross(Repairing)
		if !errors.Is(err, ErrTransitionRequired) {
			t.Errorf("cross(Repairing) = %v, want ErrTransitionRequired", err)
		}
		if !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("cross(Repairing) = %v, want it to also match ErrIllegalTransition", err)
		}
	})
	// The two sentinels must stay distinct, or matching one would silently
	// catch the other and "which door do I call" stops being answerable.
	t.Run("the two do not match each other", func(t *testing.T) {
		if errors.Is(ErrCrossRequired, ErrTransitionRequired) || errors.Is(ErrTransitionRequired, ErrCrossRequired) {
			t.Error("ErrCrossRequired and ErrTransitionRequired match each other; each names the door to call instead, so they must be distinguishable")
		}
	})
}
