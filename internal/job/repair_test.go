package job_test

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

func TestJob_RepairState_UnknownWhenNoProgress(t *testing.T) {
	t.Parallel()

	j := job.New("job-1", "Test Job", job.Policy{})
	if got := j.RepairState(); got != job.RepairUnknown {
		t.Errorf("RepairState() = %v, want %v", got, job.RepairUnknown)
	}
}

// TestRepairStateFrom_EveryBranch walks the five verdicts the derivation can
// return.
//
// internal/api's queue_repair_health_test.go asserted that buildSlot asks for
// the verdict rather than rebuilding it, and deferred the verdict's own
// branches to a test in the deleted internal/queue. That test went with the
// package and nothing replaced it, so until now no test called
// RepairStateFrom at all — `git grep -lnw 'RepairStateFrom' -- '*_test.go'`
// found no files.
//
// The distinction between the two zero-capacity verdicts is the one worth
// pinning. Both mean "no recovery bytes", but RepairUnknown says par2 files
// exist and their capacity is not yet known, while RepairNoCapacity says none
// exist and none is coming. Only the second is terminal — Hopeless() is true
// for it and false for the first — so collapsing them would have the
// dispatcher's Early Health Gate abandon a job whose par2 had simply not been
// assessed yet.
func TestRepairStateFrom_EveryBranch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		contentFailed     int64
		recoveryBytes     int64
		hasPar2Files      bool
		want              job.RepairState
		wantHopeless      bool
		whyItMattersIfNot string
	}{
		{
			name: "no damage is intact whatever the capacity",
			// hasPar2Files is false and recoveryBytes zero, which would be
			// RepairNoCapacity on any other row: the damage test comes first
			// precisely so an undamaged job is never condemned for lacking
			// recovery it does not need.
			contentFailed: 0, recoveryBytes: 0, hasPar2Files: false,
			want: job.RepairIntact, wantHopeless: false,
		},
		{
			name:          "damage with par2 present but no capacity yet is unknown",
			contentFailed: 100, recoveryBytes: 0, hasPar2Files: true,
			want: job.RepairUnknown, wantHopeless: false,
		},
		{
			name:          "damage with no par2 at all has no capacity",
			contentFailed: 100, recoveryBytes: 0, hasPar2Files: false,
			want: job.RepairNoCapacity, wantHopeless: true,
		},
		{
			name:          "damage exceeding capacity is beyond repair",
			contentFailed: 101, recoveryBytes: 100, hasPar2Files: true,
			want: job.RepairBeyondCapacity, wantHopeless: true,
		},
		{
			name:          "damage within capacity is repairable",
			contentFailed: 100, recoveryBytes: 100, hasPar2Files: true,
			want: job.RepairPossible, wantHopeless: false,
		},
		{
			// The boundary is > rather than >=, so damage exactly equal to
			// capacity is repairable. par2 recovers one lost block per
			// recovery block, so equality is sufficient, not marginal.
			name:          "damage exactly equal to capacity is still repairable",
			contentFailed: 100, recoveryBytes: 100, hasPar2Files: false,
			want: job.RepairPossible, wantHopeless: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := job.RepairStateFrom(tc.contentFailed, tc.recoveryBytes, tc.hasPar2Files)
			if got != tc.want {
				t.Errorf("RepairStateFrom(%d, %d, %v) = %v, want %v",
					tc.contentFailed, tc.recoveryBytes, tc.hasPar2Files, got, tc.want)
			}
			if got.Hopeless() != tc.wantHopeless {
				t.Errorf("%v.Hopeless() = %v, want %v — the Early Health Gate acts on this",
					got, got.Hopeless(), tc.wantHopeless)
			}
		})
	}
}
