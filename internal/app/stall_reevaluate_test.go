package app

import (
	"errors"
	"slices"
	"strings"
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
	t.Parallel()
	application, job, release := newWedgedApp(t)
	ctx := t.Context()

	application.handleFileComplete(ctx, FileComplete{JobID: job.ID(), FileIdx: 0})

	// Ground the fixture: the stall really happened, and it really left the
	// completion unfinished. Without this the recovery assertions below could
	// be satisfied by a file that was never stalled in the first place.
	if row, ok := application.dispatcher.Row(job.ID()); !ok || row.Status() != constants.StatusPaused {
		t.Fatalf("status = %v before re-evaluation, want Paused; the fixture did not stall", row.Status())
	}
	if job.Progress().FileComplete(0) {
		t.Fatal("the file was already marked complete before re-evaluation; there is nothing left to recover")
	}
	if got := application.StallReason(job.ID()).Reason; got == "" {
		t.Fatal("no stall reason was recorded, so the re-evaluation has nothing to re-evaluate (R27)")
	}

	// The operator fixes the mount.
	release()
	application.reevaluateStalls(ctx)

	row, ok := application.dispatcher.Row(job.ID())
	if !ok {
		t.Fatal("job left the queue")
	}
	if !job.Progress().FileComplete(0) {
		t.Error("the file is still not marked complete after the condition cleared; the " +
			"finalize was never retried, so the job cannot proceed to DirectUnpack, job " +
			"finalization or post-processing — the stall is not self-clearing (L2)")
	}
	if !job.Progress().ArticleDone(0) {
		t.Error("the file's article is still Outstanding; the retried finalize never drained " +
			"and acked it, so this run's bytes would be re-fetched after a restart")
	}
	if row.Status() == constants.StatusPaused {
		t.Errorf("status = %v, want the job resumed once its file was finalized", row.Status())
	}
	if got := application.StallReason(job.ID()).Reason; got != "" {
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
// The sentinel matters as much as the refusal: it is what keeps the caller
// from re-classifying this as a storage fault, which is A1 running backwards.
func TestRetryFinalize_RefusesAFileWhoseHandleIsGone(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID(), 0, 0)

	// File 1 does not exist in this fixture, so no handle is open for it.
	err := application.retryFinalize(t.Context(), job.ID(), 1)
	if err == nil {
		t.Fatal("retryFinalize reported success for a file no handle is open for; the " +
			"caller marks it complete and post-processing consumes an untrimmed file")
	}
	if !errors.Is(err, errFinalizeUnrecoverable) {
		t.Errorf("err = %v, want it to wrap errFinalizeUnrecoverable — the caller cannot "+
			"otherwise tell it apart from a mount that may yet come back, and re-routes it "+
			"as a retryable storage fault", err)
	}
}

// TestReevaluateStall_KeepsTheActionableReasonForALostFile pins I5: the reason
// the operator is left with must name the action that recovers the job.
//
// The reason used to be built as a *storagefault.Fault and then handed to
// routeFinalizeFailure, which re-classified it — producing "Stalled: storage
// retryable fault on finalize \"\"" with an empty path and the restart
// instruction gone. A2 asks for an ACTIONABLE reason; that one tells the
// operator to wait for a condition that will never clear.
func TestReevaluateStall_KeepsTheActionableReasonForALostFile(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID(), 0, 0)
	application.Stall(job.ID(), &storagefault.Fault{
		Op: "sync", Path: "/data/x.bin", Err: syscall.EIO,
	})
	// File 1 has no handle, so the retry can never finalize it.
	application.notePendingFinalize(job.ID(), 1)

	application.reevaluateStall(t.Context(), job.ID())

	reason := application.StallReason(job.ID()).Reason
	if !strings.Contains(reason, "restart") {
		t.Errorf("reason = %q, want it to name the restart that recovers the job", reason)
	}
	if strings.Contains(reason, "retryable storage fault") || strings.Contains(reason, "storage retryable") {
		t.Errorf("reason = %q, was re-classified as a storage fault — the operator is told to "+
			"wait for a condition that cannot clear", reason)
	}
	row, ok := application.dispatcher.Row(job.ID())
	if !ok {
		t.Fatal("job not in queue")
	}
	if row.Header.Warning != reason {
		t.Errorf("queue warning = %q, want it to match the surfaced reason %q", row.Header.Warning, reason)
	}
	if row.Status() != constants.StatusPaused {
		t.Errorf("status = %v, want the job still Paused — a file that cannot be trimmed would "+
			"otherwise be marked complete and fed to post-processing", row.Status())
	}
}

// TestReevaluateStall_DoesNotResumeAJobWhoseFinalizeStillFails pins I6.
//
// A resumed job dispatches articles into the device that has just refused
// them, and re-evaluation happens every interval for as long as the condition
// lasts. Resuming before the retry has landed therefore turns a stall into a
// repeating re-download against a dead mount.
func TestReevaluateStall_DoesNotResumeAJobWhoseFinalizeStillFails(t *testing.T) {
	t.Parallel()
	application, job, _ := newWedgedApp(t)

	application.handleFileComplete(t.Context(), FileComplete{JobID: job.ID(), FileIdx: 0})
	if row, ok := application.dispatcher.Row(job.ID()); !ok || row.Status() != constants.StatusPaused {
		t.Fatalf("status = %v before re-evaluation, want Paused; the fixture did not stall", row.Status())
	}

	// The wedge is NOT released: the condition still holds.
	application.reevaluateStall(t.Context(), job.ID())

	if row, ok := application.dispatcher.Row(job.ID()); !ok || row.Status() != constants.StatusPaused {
		t.Errorf("status = %v after a re-evaluation that could not finalize the file, want "+
			"Paused — the job dispatches articles into the device that just refused them, "+
			"every interval, for as long as the condition lasts", row.Status())
	}
	if got := application.StallReason(job.ID()).Reason; got == "" {
		t.Error("the reason was dropped while the job is still parked (R27)")
	}
}

// TestReevaluateStall_RetriesEveryInterruptedFinalizeInOnePass pins the fd
// bound's other half.
//
// Returning at the first failing file left the rest of a job's interrupted
// finalizes untried until the following interval — one per 30 seconds, each
// holding a file handle in the meantime — so a job with several completed
// files on a broken mount took minutes to clear after the mount came back, and
// held every handle throughout.
//
// Both files here are unrecoverable, which makes the difference visible as
// state rather than as timing: under a first-failure return only file 1 would
// be classified.
func TestReevaluateStall_RetriesEveryInterruptedFinalizeInOnePass(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID(), 0, 0)
	application.Stall(job.ID(), &storagefault.Fault{Op: "sync", Path: "/data/x.bin", Err: syscall.EIO})
	// Neither file has an open handle, so each retry classifies it in turn.
	application.notePendingFinalize(job.ID(), 1)
	application.notePendingFinalize(job.ID(), 2)

	application.reevaluateStall(t.Context(), job.ID())

	files := application.recoveryFiles(job.ID())
	if got := files[1]; got != finalizeLost {
		t.Errorf("file 1 state = %v, want finalizeLost", got)
	}
	if got := files[2]; got != finalizeLost {
		t.Errorf("file 2 state = %v, want finalizeLost — the pass stopped at the first file, so "+
			"every other interrupted finalize waits another interval and holds its handle "+
			"until then", got)
	}
}

// TestRetryFinalize_RefusesAJobWithNoReadableManifest pins the second
// success-lookalike, the one retryFinalize's open-handle check does not cover.
//
// finalizeCompletedFile answers nil for a nil sync target. On a first attempt
// that is safe, because MarkFileComplete refuses the completion for the same
// reason. On a RETRY the completion is queued behind this call and delivered
// on a LATER cycle, by which time the job can be resident again — so the file
// would be recorded finalizeDone without ever having been trimmed, and shipped
// with pre-allocation's trailing zeros for par2 to read as damage.
//
// The handle is deliberately still open here, so the earlier guard cannot be
// what produces the refusal.
func TestRetryFinalize_RefusesAJobWithNoReadableManifest(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID(), 0, 0)
	if err := application.dispatcher.Remove(t.Context(), job.ID()); err != nil {
		t.Fatal(err)
	}
	open, err := application.assembler.OpenFiles(t.Context(), job.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(open, 0) {
		t.Fatal("the handle is already gone, so the open-file guard would produce the refusal " +
			"and this test would assert nothing about the sync target")
	}

	if err := application.retryFinalize(t.Context(), job.ID(), 0); err == nil {
		t.Fatal("retryFinalize reported success for a job whose manifest cannot be read; the " +
			"file is recorded finalized and its completion ships an untrimmed file on a " +
			"later cycle")
	}
}

// TestReevaluateStall_ForgetsADepartedJobWithWorkOutstanding pins the guard
// that keeps a removed job from being re-evaluated forever.
//
// Phase 1 returns early while anything is still blocked, so without this check
// a departed job with a pending finalize never reaches the Dispatcher.ResumeJob that
// used to notice it was gone — it would be retried on every interval for the
// life of the process, re-logging its routed fault each time.
func TestReevaluateStall_ForgetsADepartedJobWithWorkOutstanding(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID(), 0, 0)
	application.Stall(job.ID(), &storagefault.Fault{Op: "sync", Path: "/data/x.bin", Err: syscall.EIO})
	application.notePendingFinalize(job.ID(), 0)
	if err := application.dispatcher.Remove(t.Context(), job.ID()); err != nil {
		t.Fatal(err)
	}

	application.reevaluateStall(t.Context(), job.ID())

	if got := application.stalledJobIDs(); len(got) != 0 {
		t.Errorf("stalledJobIDs = %v, want empty — a job that has left the queue has nothing "+
			"to recover, and re-evaluating it every interval is churn that can never succeed", got)
	}
}
