package queue

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// TestHasPar2Verdict_TracksTheReleaseReason pins the lifecycle HasPar2Verdict
// reports: false on a fresh job, true once a release reason is recorded,
// false again after ResetForRetry clears it, and false on a nil receiver.
//
// It stayed in internal/queue when progress_test.go moved to internal/job: the
// two transitions it exercises are driven by Job.setPar2ReleaseReason and
// Job.ResetForRetry, both of which are *queue.Job methods that read Queue-era
// state and did not travel with the content tier. Testing HasPar2Verdict
// against a bare JobProgress would set and read the same field and assert
// nothing.
func TestHasPar2Verdict_TracksTheReleaseReason(t *testing.T) {
	t.Parallel()

	var nilP *JobProgress
	if nilP.HasPar2Verdict() {
		t.Error("nil *JobProgress HasPar2Verdict() = true, want false")
	}

	job, err := NewJob(minimalNZB(), AddOptions{Filename: "verdict.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if job.progress.HasPar2Verdict() {
		t.Error("fresh job: HasPar2Verdict() = true, want false")
	}

	job.setPar2ReleaseReason("volume 3 damaged")
	if !job.progress.HasPar2Verdict() {
		t.Error("after setPar2ReleaseReason: HasPar2Verdict() = false, want true")
	}

	job.ResetForRetry()
	if job.progress.HasPar2Verdict() {
		t.Error("after ResetForRetry: HasPar2Verdict() = true, want false")
	}
}
