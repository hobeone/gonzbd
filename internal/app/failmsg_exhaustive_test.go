package app

import (
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/queue"
)

// TestFailMsgForJob_WordsEveryHopelessState is the tripwire for adding a state
// to queue.RepairState without teaching this package what to say about it.
//
// The hazard is structural rather than hypothetical. Two separate opt-in lists
// decide a job's fate: RepairState.Hopeless() decides whether the dispatcher
// stops work, and failMsgForJob's switch decides what the user is told. They
// are not derived from each other. A new hopeless state added to the first and
// not the second aborts the job with an empty reason, because OnJobHopeless
// passes this function's result into maybeFinalize with no fallback string.
//
// Every abort path in this package therefore has to produce text for every
// state Hopeless() covers. This walks queue.AllRepairStates rather than a
// hand-written list so a new state cannot slip past by being forgotten here
// too — that list is itself pinned against the const block by
// queue.TestAllRepairStates_Exhaustive.
func TestFailMsgForJob_WordsEveryHopelessState(t *testing.T) {
	t.Parallel()

	// One fixture per state, each reaching it through the real derivation
	// rather than by constructing the state directly — a fixture that cannot
	// produce its state is itself a finding.
	fixtures := map[queue.RepairState][]failMsgFile{
		queue.RepairIntact: {
			{subject: "movie.part01.rar", bytes: 1000},
			{subject: "movie.par2", bytes: 50},
		},
		queue.RepairPossible: {
			{subject: "movie.part01.rar", bytes: 1000},
			{subject: "movie.part02.rar", bytes: 200},
			{subject: "movie.vol01+02.par2", bytes: 300},
		},
		queue.RepairUnknown: {
			{subject: "movie.part01.rar", bytes: 1000},
			{subject: "movie.par2", bytes: 50},
		},
		queue.RepairNoCapacity: {
			{subject: "movie.part01.rar", bytes: 1000},
			{subject: "movie.part02.rar", bytes: 50},
		},
		queue.RepairBeyondCapacity: {
			{subject: "movie.part01.rar", bytes: 1000},
			{subject: "movie.part02.rar", bytes: 400},
			{subject: "movie.vol01+02.par2", bytes: 300},
		},
	}
	// The index of the file whose article fails, per state.
	failIdx := map[queue.RepairState]int{
		queue.RepairIntact:         1, // the par2 index itself
		queue.RepairPossible:       1,
		queue.RepairUnknown:        0, // content, against an unrecognized par2
		queue.RepairNoCapacity:     1,
		queue.RepairBeyondCapacity: 1,
	}

	for _, state := range queue.AllRepairStates() {
		files, ok := fixtures[state]
		if !ok {
			t.Errorf("no fixture for RepairState %q — add one here and decide what failMsgForJob "+
				"should say about it. If it is hopeless and this is skipped, the dispatcher will "+
				"abort jobs with an empty reason.", state)
			continue
		}

		t.Run(string(state), func(t *testing.T) {
			job := buildFailMsgJob(t, files, failIdx[state])

			if got := job.RepairState(); got != state {
				t.Fatalf("fixture guard: reached %q, want %q — this row is testing the wrong state", got, state)
			}

			msg := failMsgForJob(job)
			if state.Hopeless() {
				if msg == "" {
					t.Errorf("failMsgForJob() = \"\" for hopeless state %q; the dispatcher stops the "+
						"job and the user is told nothing", state)
				}
				if !strings.Contains(msg, "beyond repair") {
					t.Errorf("failMsgForJob() = %q for %q, want a message naming the verdict", msg, state)
				}
				return
			}
			if msg != "" {
				t.Errorf("failMsgForJob() = %q for non-hopeless state %q, want \"\" — this aborts a "+
					"job the dispatcher is still working on", msg, state)
			}
		})
	}
}
