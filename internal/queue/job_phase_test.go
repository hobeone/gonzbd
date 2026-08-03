package queue

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

// PhaseTerminal was declared, given a String() arm, and written into
// docs/queue-lifecycle.md's contract, but Phase() never returned it: the
// terminal statuses fell through to the default arm and reported
// PhasePending. A parked failure and a job waiting to be dispatched are not
// the same state, and anything that branches on the phase — the compaction
// design in the lifecycle doc among it — would have silently taken the
// pending path for every completed, failed and deleted job.
func TestJobPhase_TerminalStatusesReportTerminal(t *testing.T) {
	for _, status := range []constants.Status{
		constants.StatusCompleted,
		constants.StatusFailed,
		constants.StatusDeleted,
	} {
		j := &Job{Status: status}
		if got := j.Phase(); got != PhaseTerminal {
			t.Errorf("Phase() for %s = %v, want PhaseTerminal", status, got)
		}
	}
}

// Terminal jobs were already non-resident by virtue of landing in
// PhasePending, so naming the phase correctly must not move them onto the
// resident side and start holding manifests for parked failures.
func TestJobPhase_TerminalStaysNonResident(t *testing.T) {
	for _, status := range []constants.Status{
		constants.StatusCompleted,
		constants.StatusFailed,
		constants.StatusDeleted,
	} {
		if isResidentStatus(status) {
			t.Errorf("isResidentStatus(%s) = true, want false: a terminal job must not hold a manifest", status)
		}
	}
}

// Every status's phase is a deliberate choice, not a fall-through.
//
// The loop is driven by constants.AllStatuses(), not by the want map below.
// Iterating the map instead — as the first version of this test did — cannot
// catch the defect it exists to catch: a status missing from Phase()'s switch
// is missing from the map too, so the loop never visits it and the test
// passes while the status silently takes the default arm. The chain that
// makes this real runs one step further back: constants'
// TestAllStatuses_Exhaustive parses the const block and fails if a declared
// status is absent from AllStatuses(), so a new status cannot hide from this
// loop either.
//
// Grabbing and Checking map to PhasePending as an explicit entry rather than
// by omission. They are SABnzbd-compatibility vocabulary the API reports (see
// stageFromStatus) that no code path assigns to a job, but "unused" is not a
// reason to leave a status unconsidered.
func TestJobPhase_EveryStatusIsMappedDeliberately(t *testing.T) {
	want := map[constants.Status]JobPhase{
		constants.StatusIdle:        PhasePending,
		constants.StatusQueued:      PhasePending,
		constants.StatusPropagating: PhasePending,
		constants.StatusGrabbing:    PhasePending,
		constants.StatusChecking:    PhasePending,
		constants.StatusFetching:    PhaseActive,
		constants.StatusDownloading: PhaseActive,
		constants.StatusPaused:      PhasePaused,
		constants.StatusQuickCheck:  PhaseProcessing,
		constants.StatusVerifying:   PhaseProcessing,
		constants.StatusRepairing:   PhaseProcessing,
		constants.StatusExtracting:  PhaseProcessing,
		constants.StatusMoving:      PhaseProcessing,
		constants.StatusRunning:     PhaseProcessing,
		constants.StatusCompleted:   PhaseTerminal,
		constants.StatusFailed:      PhaseTerminal,
		constants.StatusDeleted:     PhaseTerminal,
	}

	for _, status := range constants.AllStatuses() {
		wantPhase, considered := want[status]
		if !considered {
			t.Errorf("status %s has no expected phase here; decide its phase in Job.Phase() and record it in this map rather than letting it inherit PhasePending from the default arm", status)
			continue
		}
		j := &Job{Status: status}
		if got := j.Phase(); got != wantPhase {
			t.Errorf("Phase() for %s = %v, want %v", status, got, wantPhase)
		}
	}
}
