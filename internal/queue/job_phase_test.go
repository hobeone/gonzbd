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

// The phase of every status the queue can actually put a job into is a
// deliberate choice, not a fall-through. Grabbing and Checking are excluded:
// they are SABnzbd-compatibility vocabulary the API reports (see
// stageFromStatus) and have no entry in the transition table, so no job ever
// holds them.
func TestJobPhase_EveryReachableStatusIsMappedDeliberately(t *testing.T) {
	want := map[constants.Status]JobPhase{
		constants.StatusIdle:        PhasePending,
		constants.StatusQueued:      PhasePending,
		constants.StatusPropagating: PhasePending,
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
	for status, wantPhase := range want {
		j := &Job{Status: status}
		if got := j.Phase(); got != wantPhase {
			t.Errorf("Phase() for %s = %v, want %v", status, got, wantPhase)
		}
	}
}
