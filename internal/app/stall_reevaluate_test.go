package app

import (
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestReevaluateStall_ResumesAJobWithNothingToRetry pins the plain R19 case: a
// storage fault raised somewhere other than a file finalize leaves nothing to
// re-run, and re-evaluation is then just "unpause and find out".
//
// It is a separate test rather than a subtest of the one above because the two
// have opposite failure modes. That one fails if the re-evaluation resumes too
// early; this one fails if it never resumes at all.
func TestReevaluateStall_ResumesAJobWithNothingToRetry(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)

	application.Stall(job.ID(), &storagefault.Fault{Op: "write", Path: "/data/x.bin", Err: syscall.ENOSPC})
	if row, ok := application.dispatcher.Row(job.ID()); !ok || row.Status() != constants.StatusPaused {
		t.Fatalf("status = %v after Stall, want Paused", row.Status())
	}

	application.reevaluateStalls(t.Context())

	row, ok := application.dispatcher.Row(job.ID())
	if !ok || row.Status() == constants.StatusPaused {
		t.Errorf("status = %v, want the job off Paused — a stall that is never re-evaluated "+
			"has no path back to running except a restart (R19)", row.Status())
	}
	if got := application.StallReason(job.ID()).Reason; got != "" {
		t.Errorf("stall reason = %q, want it cleared once the job was resumed", got)
	}
}

// TestReevaluateStall_ForgetsADepartedJob pins the guard that keeps a removed
// job from being re-evaluated on every interval for the life of the process.
func TestReevaluateStall_ForgetsADepartedJob(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID(), 0, 0)
	application.Stall(job.ID(), &storagefault.Fault{Op: "sync", Path: "/data/x.bin", Err: syscall.EIO})
	if err := application.dispatcher.Remove(t.Context(), job.ID()); err != nil {
		t.Fatal(err)
	}

	application.reevaluateStall(job.ID())

	if got := application.stalledJobIDs(); len(got) != 0 {
		t.Errorf("stalledJobIDs = %v, want empty — a job that has left the queue has nothing "+
			"to recover, and re-evaluating it every interval is churn that can never succeed", got)
	}
}
