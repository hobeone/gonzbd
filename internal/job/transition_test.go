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

		{"self is a legal edge", Fetching, Fetching, true},

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

// TestEdgeCountsMatchTheStatedPartition classifies every edge in legalEdges
// by the partition rule stated in the doc comment above legalEdges — Cancel
// (target Finished), Pause (target Waiting), Resume (source Waiting, target
// not Finished), Work spine (everything else) — and pins the bucket sizes and
// their total. This is what keeps that comment from drifting silently again:
// a hand-counted breakdown in prose contradicted itself (claimed 22, summed
// to 20, and double-counted two edges) and nothing caught it until review.
func TestEdgeCountsMatchTheStatedPartition(t *testing.T) {
	var cancel, pause, resume, spine int
	total := 0
	for from, tos := range legalEdges {
		for _, to := range tos {
			total++
			switch {
			case to == Finished:
				cancel++
			case to == Waiting:
				pause++
			case from == Waiting:
				resume++
			default:
				spine++
			}
		}
	}

	wantCancel, wantPause, wantResume, wantSpine := 6, 5, 5, 6
	if cancel != wantCancel {
		t.Errorf("cancel edges (target Finished) = %d, want %d", cancel, wantCancel)
	}
	if pause != wantPause {
		t.Errorf("pause edges (target Waiting) = %d, want %d", pause, wantPause)
	}
	if resume != wantResume {
		t.Errorf("resume edges (source Waiting, target not Finished) = %d, want %d", resume, wantResume)
	}
	if spine != wantSpine {
		t.Errorf("work spine edges = %d, want %d", spine, wantSpine)
	}

	wantTotal := wantCancel + wantPause + wantResume + wantSpine
	if total != wantTotal {
		t.Errorf("total edges = %d, want %d", total, wantTotal)
	}
}
