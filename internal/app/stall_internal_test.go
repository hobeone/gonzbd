package app

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
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
	application.notePendingFinalize(job.ID, 0)

	application.stallLost(job.ID, 0)

	if st := application.recoveryFiles(job.ID)[0]; st != finalizeLost {
		t.Errorf("file state = %v after stallLost, want finalizeLost — the file is retried "+
			"every interval against a handle that no longer exists", st)
	}
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

	err := application.retryFinalize(t.Context(), job.ID, 0)
	if err == nil {
		t.Fatal("retryFinalize reported success with no barrier in the process")
	}
	if !errors.Is(err, errFinalizeUnrecoverable) {
		t.Errorf("err = %v, want errFinalizeUnrecoverable — no barrier will ever appear in "+
			"this process, so retrying every interval is churn that cannot succeed", err)
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

// TestRunCheckpoint_ReevaluatesStallsOnItsTicker pins the R19 seam itself.
//
// It exists because the two tests that looked like they covered this were both
// proxies that never met: the API test asserts a counter on a fake, and the app
// test calls reevaluateStalls directly. Deleting BOTH arms of the select in
// runCheckpoint left every package green — the interval half of R19 was wired
// to nothing that any test could see.
//
// This drives the real loop and asserts the job actually leaves Paused, which
// only happens if the ticker arm reaches reevaluateStalls.
func TestRunCheckpoint_ReevaluatesStallsOnItsTicker(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t, WithStallRecheckInterval(10*time.Millisecond))
	job := addStallTestJob(t, application, "ticker-job")
	application.Stall(job.ID, testFault("write"))
	if snap := application.queue.SnapshotJob(job.ID); snap.Status != constants.StatusPaused {
		t.Fatalf("status = %v after Stall, want Paused; the fixture is not parked", snap.Status)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go application.runCheckpoint(ctx, time.Hour) // checkpoint bound long: only the stall ticker may fire

	waitForResumed(t, application, job.ID,
		"the checkpoint loop's stall ticker never reached reevaluateStalls; R19's interval "+
			"half is wired to nothing and a parked job waits for a restart")
}

// TestRunCheckpoint_ReevaluatesStallsOnAKick pins the other arm — R19's "on
// user action" — through the same real loop, so that ReevaluateStalls is shown
// to reach reevaluateStalls rather than only to increment something.
func TestRunCheckpoint_ReevaluatesStallsOnAKick(t *testing.T) {
	// An interval far longer than the test, so only the kick can explain a
	// resumed job.
	application, _, _ := newLifecycleTestApp(t, WithStallRecheckInterval(time.Hour))
	job := addStallTestJob(t, application, "kick-job")
	application.Stall(job.ID, testFault("write"))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go application.runCheckpoint(ctx, time.Hour)

	application.ReevaluateStalls()

	waitForResumed(t, application, job.ID,
		"ReevaluateStalls did not reach the checkpoint loop; resuming a job from the API "+
			"leaves it parked until the interval or a restart")
}

// addStallTestJob adds a one-file job to the application's queue.
func addStallTestJob(t *testing.T, application *Application, name string) *queue.Job {
	t.Helper()
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  name + ".bin",
		Bytes:    100,
		Articles: []nzb.Article{{ID: name + "0@t", Bytes: 100, Number: 1}},
	}}}
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: name}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := application.queue.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return job
}

// waitForResumed blocks until the job is off Paused, or fails with why.
func waitForResumed(t *testing.T, application *Application, jobID, why string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snap := application.queue.SnapshotJob(jobID)
		if snap != nil && snap.Status != constants.StatusPaused {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(why)
}

// TestNoteStallReason_RecordsAReasonThatIsNotAStorageFault pins the entry
// point that exists because not every reason IS a fault.
//
// A completed file whose handle was released can never be finalized in this
// process. Expressing that through storagefault.Classify produced "Stalled:
// storage retryable fault on finalize \"\"" — an empty path, and the restart
// instruction erased. This path keeps the text the caller wrote, and creates
// the record when the job is not yet on the stalled list.
func TestNoteStallReason_RecordsAReasonThatIsNotAStorageFault(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)

	application.noteStallReason("job-1", "Stalled: restart gonzbd to recover this job")

	got := application.StallReason("job-1")
	if got.Reason != "Stalled: restart gonzbd to recover this job" {
		t.Errorf("reason = %q, want the text as written — routing it through the fault "+
			"vocabulary is what erased the actionable half", got.Reason)
	}
	if got.Since.IsZero() {
		t.Error("Since is zero, so the record was never created and the job is not on the " +
			"stalled list at all")
	}
	if ids := application.stalledJobIDs(); len(ids) != 1 {
		t.Errorf("stalledJobIDs = %v, want the job parked", ids)
	}

	// A later fault replaces the text rather than accumulating.
	application.noteStall("job-1", testFault("write"))
	if r := application.StallReason("job-1").Reason; !strings.Contains(r, "no space") {
		t.Errorf("reason = %q, want the newer fault", r)
	}
}

// TestSeedFromCommittedRuns_InstallsWhatTheRetryCouldNotAck pins phase 3 of
// the re-evaluation.
//
// A finalize retried while the job is paused records its runs and then fails
// to ack, because AckDurable needs a resident job. Without this replay those
// articles stay Outstanding on a file the assembler has already tombstoned, so
// no re-fetch can ever resolve them.
func TestSeedFromCommittedRuns_InstallsWhatTheRetryCouldNotAck(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 2)
	ctx := t.Context()
	if application.queue.SnapshotJob(job.ID).Progress().ArticleDone(0) {
		t.Fatal("the fixture already reports article 0 done; the assertion below cannot " +
			"distinguish a replay from the starting state")
	}

	if err := application.runs.Commit(ctx, job.ID, []durability.DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	application.seedFromCommittedRuns(ctx, job.ID)

	p := application.queue.SnapshotJob(job.ID).Progress()
	if !p.ArticleDone(0) {
		t.Error("article 0 is still Outstanding although a recorded run covers it; the " +
			"retry's work is thrown away and the article can never be resolved, because the " +
			"assembler has tombstoned its file")
	}
	if p.ArticleDone(1) {
		t.Error("article 1 was marked done although no run covers it — the replay is " +
			"claiming durability the barrier never established")
	}
}

// TestSeedFromCommittedRuns_IsInertWithoutARunStore pins the process with no
// history database, where there is no record to replay. The re-evaluation must
// still complete rather than panicking on a nil store.
func TestSeedFromCommittedRuns_IsInertWithoutARunStore(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	application.runs = nil

	application.seedFromCommittedRuns(t.Context(), job.ID)

	if application.queue.SnapshotJob(job.ID).Progress().ArticleDone(0) {
		t.Error("an article was marked durable with no run store to prove it")
	}
}

// TestSeedFromCommittedRuns_ReportsAJobItCannotSeed pins the disposition when
// the queue refuses the seed — a job the active set had no room to re-promote.
// The articles stay Outstanding and are re-fetched, which is the safe
// direction, and it must not abort the rest of the re-evaluation.
func TestSeedFromCommittedRuns_ReportsAJobItCannotSeed(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	ctx := t.Context()
	if err := application.runs.Commit(ctx, job.ID, []durability.DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Pause evicts the manifest, and SeedFromRuns needs a resident one.
	if err := application.queue.Pause(job.ID); err != nil {
		t.Fatal(err)
	}

	application.seedFromCommittedRuns(ctx, job.ID)

	// liveProgress rather than SnapshotJob: hydration re-derives the article
	// state from the same record this replay reads, so a hydrated clone would
	// report the bit set whether or not the seed landed.
	if liveProgress(t, application, job.ID).ArticleDone(0) {
		t.Error("a non-resident job was seeded anyway; SeedFromRuns installs bits into the " +
			"LIVE job, and a job with no resident manifest has nowhere to install them")
	}
}

// TestSeedFromCommittedRuns_ReportsAFailedLoad pins the arm where the record
// cannot be read at all. It must not be mistaken for "nothing is durable": the
// job simply re-fetches, and the re-evaluation carries on to deliver the
// completions it was in the middle of.
func TestSeedFromCommittedRuns_ReportsAFailedLoad(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	if err := application.runs.Commit(t.Context(), job.ID, []durability.DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	application.seedFromCommittedRuns(ctx, job.ID)

	if application.queue.SnapshotJob(job.ID).Progress().ArticleDone(0) {
		t.Error("an article was marked durable from a load that failed; a read error is not " +
			"evidence about what is on disk")
	}
}

// TestSetStallReasonLocked_CreatesTheRecordItNeeds pins the shared half of the
// two entry points above. Both may be the FIRST thing that parks a job — a
// fault routed by the barrier, or a file whose handle is gone — so a helper
// that only updated an existing record would silently drop the reason and
// leave the job off the stalled list, unreachable by any re-evaluation.
func TestSetStallReasonLocked_CreatesTheRecordItNeeds(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)

	application.stallMu.Lock()
	application.setStallReasonLocked("job-1", "Stalled: first")
	application.setStallReasonLocked("job-1", "Stalled: second")
	rec := application.stalls["job-1"]
	application.stallMu.Unlock()

	if rec == nil {
		t.Fatal("no record was created, so the job is not on the stalled list and no " +
			"re-evaluation will ever visit it")
	}
	if rec.reason != "Stalled: second" {
		t.Errorf("reason = %q, want the newer text", rec.reason)
	}
	if rec.files == nil {
		t.Error("the record's recovery map is nil; notePendingFinalize would panic writing to it")
	}
}

// TestSeedFromCommittedExtents_DoesNotClearAnAckThisProcessMade is the caller
// half of #362's two-contracts rule, and it is the guard on the mistake the
// fix makes newly possible.
//
// Phase 3 replays Class B after a stall recovery. It has verified NOTHING
// about any file — it is re-delivering an ack whose fsync already landed — so
// it must stay on the additive Queue.SeedFromExtents. Pointing it at
// Queue.ReplaceFromResume instead compiles, reads as a tidy-up, and silently
// destroys exactly the bits this phase exists to preserve: an ack this process
// made AFTER the last extent commit is not in that extent, so an authoritative
// replay would clear it and the article would be re-fetched on a file the
// assembler has already tombstoned.
//
// Verified as the only test in the repository that reddens on that swap: with
// phase 3 switched to ReplaceFromResume, ./internal/app and ./internal/api
// were both still green before this test existed.
func TestSeedFromCommittedRuns_DoesNotClearAnAckThisProcessMade(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 2)
	ctx := t.Context()

	// Article 1 was acked by a barrier in THIS process, after the commit
	// below was stamped. Article 0 is the one that commit covers.
	ackDoneIdx(t, application.queue, job.ID, 1)
	pre := application.queue.SnapshotJob(job.ID).Progress()
	if !pre.ArticleDone(1) {
		t.Fatal("the fixture's ack did not land, so there is no live bit for the replay " +
			"to destroy and the assertion below would hold vacuously")
	}
	if pre.ArticleDone(0) {
		t.Fatal("article 0 is already done, so the replay cannot be shown to have run at all")
	}

	if err := application.runs.Commit(ctx, job.ID, []durability.DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	application.seedFromCommittedRuns(ctx, job.ID)

	p := application.queue.SnapshotJob(job.ID).Progress()
	if !p.ArticleDone(1) {
		t.Error("article 1 lost its Done bit to a replay of a record that predates it — " +
			"phase 3 has stat'ed nothing and proved nothing, so it may not contradict an " +
			"ack that already landed; it must stay on the additive SeedFromRuns (#362)")
	}
	if !p.ArticleDone(0) {
		t.Error("article 0 is still Outstanding, so the replay installed nothing and the " +
			"assertion above holds vacuously")
	}
}
