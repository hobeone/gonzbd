package app

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

func testFault(op string) *storagefault.Fault {
	return &storagefault.Fault{Op: op, Path: "/data/x.bin", Err: syscall.ENOSPC}
}

// TestNoteStall_ReplacesTheReasonAndKeepsTheRecoverySet pins the one thing a
// second fault on an already-parked job must not do.
//
// The reason is replaced, because the newest condition is the one the user has
// to act on. The interrupted finalizes are NOT, because that map is the only
// record those files exist at all — the assembler reports a file complete
// exactly once — and dropping it re-opens concern 8 through a second fault.
func TestNoteStall_ReplacesTheReasonAndKeepsTheRecoverySet(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)

	application.noteStall("job-1", testFault("write"))
	application.notePendingFinalize("job-1", 3)
	application.noteStall("job-1", &storagefault.Fault{Op: "sync", Path: "/data/y.bin", Err: syscall.EIO})

	got := application.StallReason("job-1").Reason
	if got == "" {
		t.Fatal("no reason recorded at all")
	}
	if want := "sync"; !strings.Contains(got, want) {
		t.Errorf("reason = %q, want the newer %q fault — the user is shown a condition they "+
			"have already cleared", got, want)
	}
	files := application.recoveryFiles("job-1")
	if _, ok := files[3]; !ok {
		t.Errorf("recovery set = %v after a second fault, want file 3 still in it — nothing "+
			"else remembers that file, so the stall stops being self-clearing", files)
	}
}

// TestStallRecord_TracksRecoveryPerFileAndForgetsTheJobWhenEmpty pins the
// bookkeeping the re-evaluation walks: a file advances through its states, is
// dropped only when its completion has landed, and the job stops being stalled
// exactly when nothing is left.
func TestStallRecord_TracksRecoveryPerFileAndForgetsTheJobWhenEmpty(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)

	application.noteStall("job-1", testFault("write"))
	application.notePendingFinalize("job-1", 0)
	application.notePendingFinalize("job-1", 1)

	application.setFinalizeState("job-1", 0, finalizeDone)
	// A second note for a file already in flight must not reset its state, or
	// a finalize that succeeded is run again and the file is trimmed twice.
	application.notePendingFinalize("job-1", 0)
	if got := application.recoveryFiles("job-1")[0]; got != finalizeDone {
		t.Errorf("file 0 state = %v after a repeat note, want finalizeDone", got)
	}

	application.completeFinalizeRecovery("job-1", 0)
	if _, ok := application.recoveryFiles("job-1")[0]; ok {
		t.Error("file 0 is still in the recovery set after its completion landed; it would be finalized again")
	}
	if got := application.stalledJobIDs(); len(got) != 1 {
		t.Errorf("stalledJobIDs = %v, want the job still parked while file 1 is outstanding", got)
	}

	application.completeFinalizeRecovery("job-1", 1)
	if got := application.stalledJobIDs(); len(got) != 0 {
		t.Errorf("stalledJobIDs = %v, want empty — the job has nothing left to recover and "+
			"would otherwise be re-evaluated forever", got)
	}
	if got := application.StallReason("job-1").Reason; got != "" {
		t.Errorf("reason = %q for a job with nothing left to recover, want empty", got)
	}
}

// TestClearStall_ForgetsAJobWithOutstandingRecoveryWork pins the difference
// between clearStall and completeFinalizeRecovery. Fail uses the first, because
// a permanently faulted job leaves the queue and must not be re-evaluated
// however much recovery work it still had.
func TestClearStall_ForgetsAJobWithOutstandingRecoveryWork(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)
	application.noteStall("job-1", testFault("write"))
	application.notePendingFinalize("job-1", 0)

	application.clearStall("job-1")

	if got := application.stalledJobIDs(); len(got) != 0 {
		t.Errorf("stalledJobIDs = %v, want empty — a job on its way to history would be "+
			"resumed by the next re-evaluation", got)
	}
}

// TestStallReason_ReportsNothingForAJobThatIsNotParked pins the polarity the
// API depends on: an empty reason means "not stalled", so a job that never
// stalled must not produce one.
func TestStallReason_ReportsNothingForAJobThatIsNotParked(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)
	if got := application.StallReason("nobody"); got.Reason != "" || !got.Since.IsZero() {
		t.Errorf("StallReason = %+v for an unknown job, want the zero value", got)
	}
}

// TestNoteBarrierRun_StampsOnlyASuccessfulBarrier pins R26's last-barrier
// figure. It exists to tell a job that is checkpointing normally from one whose
// barriers have been failing since the mount went away, and forgetting it with
// the rest of a departed job's state is what keeps the map from growing for the
// life of the process.
func TestNoteBarrierRun_StampsOnlyASuccessfulBarrier(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)

	before := time.Now()
	application.noteBarrierRun("job-1")
	got := application.JobDurability("job-1").LastBarrier
	if got.Before(before) {
		t.Errorf("LastBarrier = %v, want at or after %v", got, before)
	}

	application.forgetJobBarrierState("job-1")
	if got := application.JobDurability("job-1").LastBarrier; !got.IsZero() {
		t.Errorf("LastBarrier = %v after the job departed, want the zero time — the map "+
			"would grow one entry per job ever downloaded", got)
	}
}

// TestReevaluateStall_LeavesAJobWhoseFileCanNoLongerBeFinalized pins the
// terminal state. Once a completed file's handle is gone, no retry in this
// process can trim it, and resuming the job anyway would let the completion
// through with pre-allocation's trailing zeros intact.
func TestReevaluateStall_LeavesAJobWhoseFileCanNoLongerBeFinalized(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID, 0, 0)

	application.noteStall(job.ID, testFault("finalize"))
	if err := application.queue.Pause(job.ID); err != nil {
		t.Fatal(err)
	}
	application.notePendingFinalize(job.ID, 0)
	application.setFinalizeState(job.ID, 0, finalizeLost)

	application.reevaluateStall(t.Context(), job.ID)

	if snap := application.queue.SnapshotJob(job.ID); snap.Status != constants.StatusPaused {
		t.Errorf("status = %v, want the job still Paused — a file that cannot be trimmed "+
			"would otherwise be marked complete and fed to post-processing", snap.Status)
	}
	if got := application.StallReason(job.ID).Reason; got == "" {
		t.Error("the reason was dropped; the job is parked with nothing the user can act on")
	}
}

// TestReevaluateStall_ForgetsAJobThatHasLeftTheQueue pins the disposition for a
// job removed while parked. Queue.Resume reports it as not found, and keeping
// the record would re-evaluate a job that no longer exists on every interval,
// forever.
func TestReevaluateStall_ForgetsAJobThatHasLeftTheQueue(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)
	application.noteStall("gone", testFault("write"))

	application.reevaluateStall(t.Context(), "gone")

	if got := application.stalledJobIDs(); len(got) != 0 {
		t.Errorf("stalledJobIDs = %v, want empty for a job the queue does not have", got)
	}
}

// TestReevaluateStalls_CoversEveryParkedJob pins that the sweep is a sweep. A
// loop that stopped at the first job would leave every other stalled download
// parked behind one wedged mount.
func TestReevaluateStalls_CoversEveryParkedJob(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)
	application.noteStall("gone-a", testFault("write"))
	application.noteStall("gone-b", testFault("write"))

	application.reevaluateStalls(t.Context())

	if got := application.stalledJobIDs(); len(got) != 0 {
		t.Errorf("stalledJobIDs = %v, want empty — a sweep that stopped at the first job "+
			"leaves the rest parked behind it", got)
	}
}

// TestReevaluateStalls_StopsOnACancelledContext pins that shutdown is not
// blocked by a sweep over jobs whose mounts are dead. Each re-evaluation can
// cost a barrier timeout per file, so a sweep that ignored cancellation would
// hold the checkpoint loop past the shutdown deadline.
func TestReevaluateStalls_StopsOnACancelledContext(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)
	application.noteStall("gone-a", testFault("write"))
	application.noteStall("gone-b", testFault("write"))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	application.reevaluateStalls(ctx)

	if got := application.stalledJobIDs(); len(got) != 2 {
		t.Errorf("stalledJobIDs = %v, want both still parked — the sweep ran anyway on a "+
			"cancelled context", got)
	}
}

// TestReevaluateStalls_KickIsDeliveredOnceAndNeverBlocks pins the property the
// API handler depends on. ReevaluateStalls is called from an HTTP request and
// a re-evaluation does barrier I/O against a mount that may be wedged, so the
// call must return immediately however many times it is made.
func TestReevaluateStalls_KickIsDeliveredOnceAndNeverBlocks(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			application.ReevaluateStalls()
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ReevaluateStalls blocked; an HTTP handler would hang behind the checkpoint loop")
	}
	if n := len(application.stallKick); n != 1 {
		t.Errorf("queued kicks = %d, want 1 — a re-evaluation already queued does the same "+
			"work, so a second is not a lost request", n)
	}
}

// TestNotePendingFinalize_RecordsAFileForAJobNotYetOnTheStalledList pins the
// order-independence the barrier's own routing needs. Barrier.routeFault
// stalls the job and then returns the fault, so routeFinalizeFailure sees an
// already-routed error — but a fault classified outside the barrier arrives
// here first, and a file dropped because no record existed yet is a file
// nothing will ever finalize.
func TestNotePendingFinalize_RecordsAFileForAJobNotYetOnTheStalledList(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)

	application.notePendingFinalize("job-1", 2)

	if _, ok := application.recoveryFiles("job-1")[2]; !ok {
		t.Error("the file was dropped because the job had no stall record yet; nothing else " +
			"remembers a completed file whose finalize failed")
	}
	if got := application.stalledJobIDs(); len(got) != 1 {
		t.Errorf("stalledJobIDs = %v, want the job parked so the file is ever retried", got)
	}
}

// TestStallLost_ReplacesTheReasonWithTheOneActionLeft pins A2's disposition for
// a file no retry in this process can finalize. Leaving the storage reason in
// place tells the user to fix a mount they have already fixed, while the job
// waits for something that will never happen.
func TestStallLost_ReplacesTheReasonWithTheOneActionLeft(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID, 0, 0)
	application.noteStall(job.ID, testFault("sync"))

	application.stallLost(job.ID, 0)

	got := application.StallReason(job.ID).Reason
	if !strings.Contains(got, "restart") {
		t.Errorf("reason = %q, want it to name the restart that recovers the job — the "+
			"previous reason points at a mount the user has already fixed", got)
	}
	if snap := application.queue.SnapshotJob(job.ID); !strings.Contains(snap.Warning, "restart") {
		t.Errorf("queue warning = %q, want the same reason; the listing is where the user looks", snap.Warning)
	}
}

// TestRetryFinalize_ReportsRatherThanAssumesWhenItCannotAsk pins the
// distinction #350 turned on: "there is nothing to finalize" and "I could not
// find out" must not be the same answer. A stopped assembler cannot say
// whether the handle is still open, and treating that as success ships an
// untrimmed file.
func TestRetryFinalize_ReportsRatherThanAssumesWhenItCannotAsk(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID, 0, 0)
	application.noteStall(job.ID, testFault("sync"))
	application.notePendingFinalize(job.ID, 0)
	if err := application.assembler.Stop(); err != nil {
		t.Fatal(err)
	}

	err := application.retryFinalize(t.Context(), job.ID, 0)
	if err == nil {
		t.Fatal("retryFinalize reported success against a stopped assembler")
	}
	got, ok := application.recoveryFiles(job.ID)[0]
	if !ok {
		t.Fatal("the file left the recovery set; the assertion below cannot distinguish that " +
			"from the state it means to check")
	}
	if got != finalizePending {
		t.Errorf("file state = %v, want finalizePending — the file was written off because "+
			"the assembler could not be ASKED, and a restart would then be demanded for a "+
			"file that is probably fine", got)
	}
}

// TestRetryFinalize_IsInertWithoutABarrier pins the process with no history
// database, where nothing acks and nothing finalizes. The retry must not
// report success there either, or the stall clears and the file ships.
func TestRetryFinalize_IsInertWithoutABarrier(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	application.barrier = nil
	application.noteStall(job.ID, testFault("sync"))
	application.notePendingFinalize(job.ID, 0)

	if err := application.retryFinalize(t.Context(), job.ID, 0); err == nil {
		t.Fatal("retryFinalize reported success with no barrier in the process")
	}
	if got := application.recoveryFiles(job.ID)[0]; got != finalizeLost {
		t.Errorf("file state = %v, want finalizeLost — no barrier will ever appear in this "+
			"process, so retrying every interval is churn that cannot succeed", got)
	}
}

// TestReevaluateStall_KeepsAFileWhoseCompletionTheQueueRefused pins the second
// half of the recovery, which needs the job RESIDENT where the finalize did
// not. Queue.Resume only makes a job eligible for promotion; the active set may
// have no room. Dropping the entry then loses the completion for good.
func TestReevaluateStall_KeepsAFileWhoseCompletionTheQueueRefused(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	application.noteStall(job.ID, testFault("sync"))
	if err := application.queue.Pause(job.ID); err != nil {
		t.Fatal(err)
	}
	// File 99 does not exist, so MarkFileComplete refuses it — standing in for
	// any queue-side refusal without needing a full active set.
	application.notePendingFinalize(job.ID, 99)
	application.setFinalizeState(job.ID, 99, finalizeDone)

	application.reevaluateStall(t.Context(), job.ID)

	if _, ok := application.recoveryFiles(job.ID)[99]; !ok {
		t.Error("the entry was dropped although the queue refused the completion; the file " +
			"is never marked complete and the job never finishes")
	}
}

// TestCompleteFinalizedFile_MarksTheFileAndReportsARefusal pins the two
// outcomes the retry branches on. A completion the queue accepted must show up
// as file state, and one it refused must come back as an error rather than as
// a silent nil (#261) — the retry has no other way to tell them apart.
func TestCompleteFinalizedFile_MarksTheFileAndReportsARefusal(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)

	if err := application.completeFinalizedFile(t.Context(), FileComplete{JobID: job.ID, FileIdx: 0}); err != nil {
		t.Fatalf("completeFinalizedFile: %v", err)
	}
	if !application.queue.SnapshotJob(job.ID).Progress().FileComplete(0) {
		t.Error("the file was not marked complete, so DirectUnpack, job finalization and " +
			"post-processing never see it")
	}

	if err := application.completeFinalizedFile(t.Context(), FileComplete{JobID: job.ID, FileIdx: 99}); err == nil {
		t.Error("a completion the queue refused was reported as success; the retry drops the " +
			"entry and the file is never marked complete")
	}
}
