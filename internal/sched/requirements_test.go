package sched

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestRequirements_AreTotal fails when a State is added and nobody classified
// what it requires. The table is §3.4's, and a state missing from it silently
// requires nothing — which reads as "runnable with no resources" and is the
// most dangerous possible default.
func TestRequirements_AreTotal(t *testing.T) {
	want := map[job.State]struct{ lease, slot bool }{
		job.Fetching:   {lease: true, slot: false},
		job.Assessing:  {lease: true, slot: true},
		job.Repairing:  {lease: true, slot: true},
		job.Extracting: {lease: false, slot: true},
		job.Finalizing: {lease: false, slot: true},
	}
	for _, s := range job.AllStates() {
		w, ok := want[s]
		if !ok {
			t.Errorf("%v is declared by AllStates() but this test does not classify it; "+
				"add the row deliberately rather than letting the state require nothing", s)
			continue
		}
		if got := needsLease(s); got != w.lease {
			t.Errorf("needsLease(%v) = %v, want %v (§3.4)", s, got, w.lease)
		}
		if got := needsSlot(s); got != w.slot {
			t.Errorf("needsSlot(%v) = %v, want %v (§3.4)", s, got, w.slot)
		}
	}
	if len(want) != len(job.AllStates()) {
		t.Errorf("this test classifies %d states, AllStates() declares %d", len(want), len(job.AllStates()))
	}
}

// TestRequirements_StateUnsetRequiresNothing pins the sentinel separately. It
// is not in AllStates(), and §3.4 says a job with no attempt requires nothing —
// a gate that asks for a lease on behalf of a never-run job would hold pool-A
// capacity for a job that has not started.
func TestRequirements_StateUnsetRequiresNothing(t *testing.T) {
	if needsLease(job.StateUnset) || needsSlot(job.StateUnset) {
		t.Error("StateUnset requires a resource; a job with no attempt is not at a state (§3.4)")
	}
}
