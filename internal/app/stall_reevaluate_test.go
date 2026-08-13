package app

import (
	"errors"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestReevaluateStall_FinishesTheFinalizeTheStallInterrupted is the pin for
// concern 8, and it is the reason R19's re-evaluation cannot be a flag check.
//
// The assembler reports a file complete exactly once. When the finalize that
// follows fails, the file's parts have all been delivered and tombstoned, so
// nothing re-triggers one — the job stayed parked for the rest of the process
// even after the operator fixed the mount. Indefinite non-progress with a
// reason the user has already acted on is an L2 defect, not a wait.
//
// The assertions are on WORK DONE, not on state cleared. A re-evaluation that
// merely unpaused the job would satisfy "not paused" and "no warning" while
// leaving the file untrimmed, unmarked and its last drain unacked — which is
// the exact failure the stall existed to prevent, now shipped silently. So the
// file being marked complete and its article being acked durable come first;
// the unpause is asserted afterwards, as a consequence.
func TestReevaluateStall_FinishesTheFinalizeTheStallInterrupted(t *testing.T) {
	application, job, release := newWedgedApp(t)
	ctx := t.Context()

	application.handleFileComplete(ctx, FileComplete{JobID: job.ID, FileIdx: 0})

	// Ground the fixture: the stall really happened, and it really left the
	// completion unfinished. Without this the recovery assertions below could
	// be satisfied by a file that was never stalled in the first place.
	if snap := application.queue.SnapshotJob(job.ID); snap.Status != constants.StatusPaused {
		t.Fatalf("status = %v before re-evaluation, want Paused; the fixture did not stall", snap.Status)
	}
	if application.queue.SnapshotJob(job.ID).Progress().FileComplete(0) {
		t.Fatal("the file was already marked complete before re-evaluation; there is nothing left to recover")
	}
	if got := application.StallReason(job.ID).Reason; got == "" {
		t.Fatal("no stall reason was recorded, so the re-evaluation has nothing to re-evaluate (R27)")
	}

	// The operator fixes the mount.
	release()
	application.reevaluateStalls(ctx)

	snap := application.queue.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("job left the queue")
	}
	if !snap.Progress().FileComplete(0) {
		t.Error("the file is still not marked complete after the condition cleared; the " +
			"finalize was never retried, so the job cannot proceed to DirectUnpack, job " +
			"finalization or post-processing — the stall is not self-clearing (L2)")
	}
	if !snap.Progress().ArticleDone(0) {
		t.Error("the file's article is still Outstanding; the retried finalize never drained " +
			"and acked it, so this run's bytes would be re-fetched after a restart")
	}
	if snap.Status == constants.StatusPaused {
		t.Errorf("status = %v, want the job resumed once its file was finalized", snap.Status)
	}
	if got := application.StallReason(job.ID).Reason; got != "" {
		t.Errorf("stall reason = %q after recovery, want it cleared — the queue would keep "+
			"showing a condition that no longer holds", got)
	}
}

// TestReevaluateStall_ResumesAJobWithNothingToRetry pins the plain R19 case: a
// storage fault raised somewhere other than a file finalize leaves nothing to
// re-run, and re-evaluation is then just "unpause and find out".
//
// It is a separate test rather than a subtest of the one above because the two
// have opposite failure modes. That one fails if the re-evaluation resumes too
// early; this one fails if it never resumes at all.
func TestReevaluateStall_ResumesAJobWithNothingToRetry(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)

	application.Stall(job.ID, &storagefault.Fault{Op: "write", Path: "/data/x.bin", Err: syscall.ENOSPC})
	if snap := application.queue.SnapshotJob(job.ID); snap.Status != constants.StatusPaused {
		t.Fatalf("status = %v after Stall, want Paused", snap.Status)
	}

	application.reevaluateStalls(t.Context())

	snap := application.queue.SnapshotJob(job.ID)
	if snap.Status == constants.StatusPaused {
		t.Errorf("status = %v, want the job off Paused — a stall that is never re-evaluated "+
			"has no path back to running except a restart (R19)", snap.Status)
	}
	if got := application.StallReason(job.ID).Reason; got != "" {
		t.Errorf("stall reason = %q, want it cleared once the job was resumed", got)
	}
}

// TestRetryFinalize_RefusesAFileWhoseHandleIsGone pins the one case the retry
// must NOT treat as success.
//
// finalizeCompletedFile answers nil both when it finalized the file and when
// there was legitimately nothing to finalize. On a first attempt that is
// correct — a file nothing holds open is one nothing downstream acts on
// either. On a retry the file's completion is queued behind the call, so
// reporting success marks complete a file that was never trimmed and ships
// pre-allocation's trailing zeros to par2 as damage.
//
// The job therefore stays parked, with a reason naming the one action left.
func TestRetryFinalize_RefusesAFileWhoseHandleIsGone(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID, 0, 0)

	// File 1 does not exist in this fixture, so no handle is open for it.
	err := application.retryFinalize(t.Context(), job.ID, 1)
	if err == nil {
		t.Fatal("retryFinalize reported success for a file no handle is open for; the " +
			"caller marks it complete and post-processing consumes an untrimmed file")
	}
	if !errors.Is(err, ErrNotFinalized) {
		t.Errorf("err = %v, want it to wrap ErrNotFinalized", err)
	}
	reason := application.StallReason(job.ID).Reason
	if reason == "" {
		t.Error("no reason was surfaced; the job is parked with nothing the user can act on (R27, A2)")
	}
	if snap := application.queue.SnapshotJob(job.ID); snap.Warning == "" {
		t.Error("the queue carries no warning, so the stall is invisible in the listing")
	}
}
