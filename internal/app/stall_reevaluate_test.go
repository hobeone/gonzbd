package app

import (
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
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
// A job still in the queue whose manifest has been evicted can run no barrier,
// and can be resident again by the time phase 4 delivers its completion. A
// retry that reported success would record the file finalizeDone without ever
// having trimmed it, and ship pre-allocation's trailing zeros for par2 to read
// as damage. The refusal must also keep the handle, which the retry that
// follows the promotion needs.
//
// The handle is deliberately still open here, so the earlier guard cannot be
// what produces the refusal.
func TestRetryFinalize_RefusesAJobWithNoReadableManifest(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID(), 0, 0)
	job.Evict()
	open, err := application.assembler.OpenFiles(t.Context(), job.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(open, 0) {
		t.Fatal("the handle is already gone, so the open-file guard would produce the refusal " +
			"and this test would assert nothing about the sync target")
	}

	err = application.retryFinalize(t.Context(), job.ID(), 0)
	if err == nil {
		t.Fatal("retryFinalize reported success for a job whose manifest cannot be read; the " +
			"file is recorded finalized and its completion ships an untrimmed file on a " +
			"later cycle")
	}
	if open, oerr := application.assembler.OpenFiles(t.Context(), job.ID()); oerr != nil || !slices.Contains(open, 0) {
		t.Errorf("the refusal released the handle (open=%v, err=%v); the retry after the job "+
			"is promoted again finds no handle and the job needs a restart", open, oerr)
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

// TestReevaluateStall_FailedFinalizeSyncRollsBackAndRetrimsOnRedelivery pins
// the end-to-end #760 recovery path when FinalizeFile's Sync fails on a
// completed file:
//  1. FinalizeFile's Sync fails with EIO, poisoning the drain report, rolling
//     article 0 back to Outstanding, lifting completed[key], and stalling the job.
//  2. Stall recovery (reevaluateStalls) sees ErrFileIncomplete on retryFinalize,
//     keeps the open FileWriter handle in place without truncating or closing it,
//     drops the pending finalize entry, and resumes the job.
//  3. Re-delivering article 0 completes the file again on the same FileWriter,
//     and handleFileComplete runs FinalizeFile to trim the file from its
//     pre-allocated 4096 bytes down to its exact 100-byte decoded size.
func TestReevaluateStall_FailedFinalizeSyncRollsBackAndRetrimsOnRedelivery(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 1)
	ctx := t.Context()

	// Exercise dropPendingFinalize and releasePark on an unknown job (no-op branch).
	application.dropPendingFinalize("no-such-job", 0)
	application.releasePark("no-such-job")

	if err := application.assembler.Stop(); err != nil {
		t.Fatalf("stop original assembler: %v", err)
	}
	var syncCalls atomic.Int32
	completedCh := make(chan FileComplete, 4)
	application.assembler = assembler.New(assembler.Options{
		FileInfo:            application.pipeline.resolveFileInfo,
		OnWriteFault:        application.handleWriteFault,
		OnArticlesUnwritten: application.handleArticlesUnwritten,
		OnArticleRejected:   application.handleArticleRejected,
		OnFileComplete: func(jobID string, fileIdx int) {
			completedCh <- FileComplete{JobID: jobID, FileIdx: fileIdx}
		},
		SyncFile: func() error {
			if syncCalls.Add(1) == 1 {
				return syscall.EIO
			}
			return nil
		},
	}, slog.New(slog.DiscardHandler))
	application.pipeline.assembler = application.assembler
	if err := application.assembler.Start(ctx); err != nil {
		t.Fatalf("start assembler: %v", err)
	}
	t.Cleanup(func() { _ = application.assembler.Stop() })

	// Mark article 0 Emitted as the downloader does before handing it to the assembler.
	if err := j.MarkArticleEmitted(0); err != nil {
		t.Fatalf("MarkArticleEmitted: %v", err)
	}
	writeFixtureArticle(t, application, j.ID(), 0, 0)
	fc1 := <-completedCh

	info, err := application.pipeline.resolveFileInfo(j.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(info.Path, 4096); err != nil {
		t.Fatalf("extend to 4096: %v", err)
	}

	// Step 1: handleFileComplete runs FinalizeFile; Sync fails with EIO.
	application.handleFileComplete(ctx, fc1)

	if row, ok := application.dispatcher.Row(j.ID()); !ok || row.Status() != constants.StatusPaused {
		t.Fatalf("status after failed FinalizeFile Sync = %v, want Paused", row.Status())
	}
	if j.Progress().FileComplete(0) || j.Progress().ArticleDone(0) {
		t.Fatal("file or article marked complete/done after failed FinalizeFile Sync")
	}
	if j.Progress().ArticleEmitted(0) {
		t.Error("article 0 still has Emitted set after failed Sync; OnArticlesUnwritten did not return it to Outstanding")
	}

	// Step 2: Stall recovery runs. Include a second recovery entry (file 99,
	// finalizeDone) so len(files) > 0 in Phase 4 and dropPendingFinalize(0)
	// is what removes file 0 from rec.files rather than clearStall.
	application.notePendingFinalize(j.ID(), 99)
	application.setFinalizeState(j.ID(), 99, finalizeDone)
	application.reevaluateStalls(ctx)

	if application.hasPendingFinalize(j.ID(), 0) {
		t.Error("file 0 remained in pendingFinalizes after ErrFileIncomplete; dropPendingFinalize did not remove it")
	}
	application.completeFinalizeRecovery(j.ID(), 99)

	if row, ok := application.dispatcher.Row(j.ID()); !ok || row.Status() == constants.StatusPaused {
		t.Fatalf("status after reevaluateStalls = %v, want resumed", row.Status())
	}
	if rerr := application.retryFinalize(ctx, j.ID(), 0); !errors.Is(rerr, durability.ErrFileIncomplete) || errors.Is(rerr, durability.ErrFaultRouted) {
		t.Fatalf("retryFinalize on incomplete file = %v, want ErrFileIncomplete without ErrFaultRouted", rerr)
	}
	// A first-attempt handleFileComplete arriving while the file is rolledBack
	// (e.g. after a concurrent checkpoint rollback) must not stall the job or
	// record a pending finalize in routeFinalizeFailure.
	application.handleFileComplete(ctx, fc1)
	if row, ok := application.dispatcher.Row(j.ID()); !ok || row.Status() == constants.StatusPaused {
		t.Fatalf("handleFileComplete on ErrFileIncomplete re-stalled the job: status = %v", row.Status())
	}
	if application.hasPendingFinalize(j.ID(), 0) {
		t.Error("routeFinalizeFailure recorded pendingFinalize for ErrFileIncomplete")
	}
	if j.Progress().FileComplete(0) {
		t.Fatal("reevaluateStalls marked file 0 complete before rolled-back article 0 was re-fetched")
	}
	if open, err := application.assembler.OpenFiles(ctx, j.ID()); err != nil || !slices.Contains(open, 0) {
		t.Fatalf("OpenFiles after reevaluateStalls = %v (err %v), want [0] kept open for re-delivery", open, err)
	}

	// Step 3: Re-deliver article 0 to the still-open FileWriter; OnFileComplete
	// fires again and handleFileComplete trims the file to 100 bytes.
	if err := j.MarkArticleEmitted(0); err != nil {
		t.Fatalf("MarkArticleEmitted: %v", err)
	}
	writeFixtureArticle(t, application, j.ID(), 0, 0)
	fc2 := <-completedCh
	application.handleFileComplete(ctx, fc2)

	if !j.Progress().FileComplete(0) {
		t.Error("file 0 not marked complete after re-delivery FinalizeFile")
	}
	if !j.Progress().ArticleDone(0) {
		t.Error("article 0 not acked Done after re-delivery FinalizeFile")
	}
	if st, err := os.Stat(info.Path); err != nil || st.Size() != 100 {
		t.Errorf("file size after re-delivery FinalizeFile = %d (err %v), want 100", st.Size(), err)
	}
}
