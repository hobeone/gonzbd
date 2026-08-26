package job

import (
	"errors"
	"slices"
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
		{"finished is terminal", Finished, Fetching, false},
		{"nothing moves into Finished", Assessing, Finished, false},
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
// one non-Finished successor — every other state does work and returns to
// the hub.
func TestOnlyAssessingBranchesWithinCorrectness(t *testing.T) {
	for _, from := range AllStates() {
		if !IsCorrectness(from) || from == Assessing {
			continue
		}
		var successors []State
		for _, to := range AllStates() {
			if to == from || to == Finished {
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
