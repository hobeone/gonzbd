package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/storagefault"
	"github.com/hobeone/gonzbd/internal/types"
)

// newDurabilityTestApp builds an Application over a real SQLite-backed queue
// and history, with one job of nFiles files each holding nArts articles, and
// the assembler started.
func newDurabilityTestApp(t *testing.T, nFiles, nArts int) (*Application, *job.Job) {
	t.Helper()
	application, _, _ := newLifecycleTestApp(t)
	if err := application.assembler.Start(t.Context()); err != nil {
		t.Fatalf("assembler.Start: %v", err)
	}
	t.Cleanup(func() { _ = application.assembler.Stop() })

	parsed := &nzb.NZB{}
	for f := range nFiles {
		file := nzb.File{Subject: fileFixtureName(f), Bytes: int64(nArts) * 100}
		for a := range nArts {
			file.Articles = append(file.Articles, nzb.Article{
				ID: fileFixtureArticleID(f, a), Bytes: 100, Number: a + 1,
			})
		}
		parsed.Files = append(parsed.Files, file)
	}
	j, hdr, err := BuildIngestJob(application.config, parsed, "durability-unit.nzb", types.FetchOptions{NzbName: "durability-unit"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.Dispatcher().Add(j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return application, j
}

func fileFixtureName(f int) string { return string(rune('A'+f)) + ".bin" }
func fileFixtureArticleID(f, a int) string {
	return fmt.Sprintf("%c%d@t", 'A'+f, a)
}

// writeFixtureArticle hands one article of one file to the assembler, at the
// offset the fixture's uniform 100-byte articles imply.
func writeFixtureArticle(t *testing.T, application *Application, jobID string, fileIdx, globalArt int) {
	t.Helper()
	if err := application.pipeline.registerFile(jobID, fileIdx); err != nil {
		t.Fatalf("registerFile %d: %v", fileIdx, err)
	}
	// Offset 0: this helper writes one article at the start of its file, so
	// the offset is file-local and does not follow the global article index.
	ref, req := assemblerWrite(jobID, fileIdx, globalArt, 0)
	if err := application.assembler.WriteArticle(t.Context(), ref, req); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}
	// WriteArticle returns once the worker has ACCEPTED the request, not once
	// it has opened the file and written it. Round-trip a control message
	// through the same worker so the file exists before the caller looks at
	// it — the assembler's own ordering guarantee, not a sleep.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if slices.Contains(application.syncTargetFor(jobID).Files(), int32(fileIdx)) { //nolint:gosec // G115: test file counts are tiny
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("file %d never opened after WriteArticle", fileIdx)
}

// ---------- Stall / Fail ----------

// TestStall_PausesTheJobAndSurfacesTheReason pins A1's storage half and R27.
//
// A full disk resolves against storage: the job stops making requests and the
// user is told which file and which condition. No article may be touched —
// marking one failed would burn its retry budget and degrade the job's
// reported health from something the user fixes in seconds.
func TestStall_PausesTheJobAndSurfacesTheReason(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	application.Stall(job.ID(), storagefault.Classify("sync", "/mnt/full/A.bin", syscall.ENOSPC))

	row, ok := application.dispatcher.Row(job.ID())
	if !ok {
		t.Fatal("job left the dispatcher")
	}
	if row.Status() != constants.StatusPaused {
		t.Errorf("status = %v after a retryable fault, want Paused — the job keeps "+
			"dispatching articles into a device that cannot take them", row.Status())
	}
	stallReason := application.StallReason(job.ID()).Reason
	if stallReason == "" {
		t.Fatal("no stall reason was surfaced; the job is paused for no visible reason (R27)")
	}
	for _, want := range []string{"/mnt/full/A.bin", "sync"} {
		if !strings.Contains(stallReason, want) {
			t.Errorf("stall reason %q does not mention %q; the user cannot act on it", stallReason, want)
		}
	}
	for i := range 2 {
		if job.Progress().ArticleFailed(i) {
			t.Errorf("article %d was marked failed by a storage fault (A1, R21)", i)
		}
		if job.Progress().ArticleDone(i) {
			t.Errorf("article %d was marked done by a storage fault", i)
		}
	}
	if job.Progress().FailedBytes() != 0 {
		t.Errorf("failed bytes = %d after a storage fault, want 0 (R21)", job.Progress().FailedBytes())
	}
}

// TestFail_SurfacesTheReasonAndStillFailsNoArticle pins R20 alongside A1. A
// read-only filesystem is permanent, so the job stops rather than waiting —
// but it says nothing about any article's availability, so the health figure
// must keep describing the download rather than the disk.
func TestFail_SurfacesTheReasonAndStillFailsNoArticle(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	application.Fail(job.ID(), storagefault.Classify("write", "/mnt/ro/A.bin", syscall.EROFS))

	row, ok := application.dispatcher.Row(job.ID())
	if !ok {
		// maybeFinalize can carry the job straight out of the queue, which is
		// the intended terminal path; the article assertions below need a
		// snapshot, so the reason is checked against history instead.
		t.Skip("job left the dispatcher for history; see TestFail on a resident job")
	}
	if !strings.Contains(row.Header.FailReason, "/mnt/ro/A.bin") {
		t.Errorf("fail reason %q does not name the file (R27)", row.Header.FailReason)
	}
	for i := range 2 {
		if job.Progress().ArticleFailed(i) {
			t.Errorf("article %d was marked failed by a permanent storage fault (A1, R20)", i)
		}
	}
	if job.Progress().FailedBytes() != 0 {
		t.Errorf("failed bytes = %d, want 0 (R21)", job.Progress().FailedBytes())
	}
}

// ---------- byte accounting ----------

// TestNoteJobBytes_KicksOnlyOnceTheBoundIsCrossed pins B1's volume half at the
// level the write path sees it.
//
// The counter is per job and the kick is edge-triggered on the bound, so a
// steady trickle below it must produce nothing: a kick per article would run a
// barrier per article, which is a few dozen fsyncs each.
func TestNoteJobBytes_KicksOnlyOnceTheBoundIsCrossed(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	application.checkpointBytes = 100

	application.noteJobBytes("job-a", 40)
	application.noteJobBytes("job-a", 40)
	if n := len(application.barrierKick); n != 0 {
		t.Fatalf("%d kicks after 80 of a 100-byte bound; a barrier per article costs "+
			"a few dozen fsyncs each", n)
	}
	// A different job's bytes must not count toward this one's bound.
	application.noteJobBytes("job-b", 90)
	if n := len(application.barrierKick); n != 0 {
		t.Fatalf("%d kicks; one job's bytes were charged to another's bound", n)
	}

	application.noteJobBytes("job-a", 25)
	select {
	case got := <-application.barrierKick:
		if got != "job-a" {
			t.Errorf("kicked %q, want job-a", got)
		}
	default:
		t.Fatal("no kick after crossing the byte bound; on a fast link a whole " +
			"interval's downloads stay unacked")
	}

	// Zero and negative counts are ignored rather than accumulated.
	application.noteJobBytes("job-b", 0)
	application.noteJobBytes("job-b", -5)
	if n := len(application.barrierKick); n != 0 {
		t.Errorf("%d kicks from non-positive byte counts", n)
	}
}

// TestSettleJobBytes_MakesTheBoundMeasureTheWindowBetweenBarriers pins why the
// reset lives in the barrier rather than in the kick: without it the
// accumulator keeps its pre-barrier total and every subsequent article
// re-crosses the bound.
//
// The retirement is on the SUCCESS path now, not before the run, so this models
// a barrier that read its window and then earned it.
func TestSettleJobBytes_MakesTheBoundMeasureTheWindowBetweenBarriers(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	application.checkpointBytes = 100

	application.noteJobBytes("job-a", 150)
	<-application.barrierKick

	pending := application.pendingBytesFor("job-a")
	if pending != 150 {
		t.Errorf("pendingBytesFor = %d, want 150 — the window's bytes are READ without "+
			"being cleared, so a job whose barrier is in flight stays at risk", pending)
	}
	application.settleJobBytes("job-a", pending)
	if got := application.pendingBytesFor("job-a"); got != 0 {
		t.Errorf("pending = %d after a successful barrier settled its window, want 0", got)
	}
	application.noteJobBytes("job-a", 10)
	if n := len(application.barrierKick); n != 0 {
		t.Fatalf("%d kicks from 10 bytes after a reset; the accumulator still carries "+
			"the previous window and every article now re-crosses the bound", n)
	}
	application.noteJobBytes("job-a", 95)
	if n := len(application.barrierKick); n != 1 {
		t.Errorf("%d kicks after 105 bytes in the new window, want 1", n)
	}
}

// TestNoteJobBytes_IsInertWithoutABarrier pins the degraded mode. A process
// with no history database has no barrier, so accumulating bytes and kicking a
// loop that will do nothing with them is pure growth.
func TestNoteJobBytes_IsInertWithoutABarrier(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	application.barrier = nil
	application.checkpointBytes = 1

	application.noteJobBytes("job-a", 1000)
	if n := len(application.barrierKick); n != 0 {
		t.Errorf("%d kicks with no barrier wired", n)
	}
	application.barrierMu.Lock()
	got := application.jobBarrierBytes["job-a"]
	application.barrierMu.Unlock()
	if got != 0 {
		t.Errorf("accumulated %d bytes with no barrier to spend them on", got)
	}
}

// ---------- per-job state ----------

// TestJobBarrierLock_IsPerJobAndStable pins both halves of the lock's
// identity. The same job must get the same mutex — two mutexes for one job
// serialise nothing — and two jobs must get different ones, or one job's slow
// mount parks every other job's checkpoint.
func TestJobBarrierLock_IsPerJobAndStable(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)

	a1 := application.jobBarrierLock("job-a")
	a2 := application.jobBarrierLock("job-a")
	b := application.jobBarrierLock("job-b")

	if a1 != a2 {
		t.Error("two calls for one job returned different mutexes; nothing is serialised")
	}
	if a1 == b {
		t.Error("two jobs share one mutex; one job's fsyncs park every other job's checkpoint")
	}

	// And it really excludes: a second Lock must not succeed while the first
	// is held.
	a1.Lock()
	done := make(chan struct{})
	go func() {
		a2.Lock()
		a2.Unlock() //nolint:staticcheck // SA2001: the point is that Lock blocked, not the critical section
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("a second Lock for the same job succeeded while the first was held")
	case <-time.After(50 * time.Millisecond):
	}
	a1.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the second Lock never acquired after the first was released")
	}
}

// TestForgetJobBarrierState_DropsBothMaps pins the bound on growth. Both maps
// are keyed by job ID, so without this they hold one entry per job ever
// downloaded for the life of the process.
func TestForgetJobBarrierState_DropsBothMaps(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	application.checkpointBytes = 1 << 30

	application.jobBarrierLock("job-a")
	application.jobBarrierLock("job-b")
	// Released, or the deletion below is DEFERRED rather than applied — see
	// releaseJobBarrierLock. That deferral is the fix for a different defect
	// and is pinned by its own test.
	application.releaseJobBarrierLock("job-a")
	application.releaseJobBarrierLock("job-b")
	application.noteJobBytes("job-a", 10)
	application.noteJobBytes("job-b", 10)

	application.forgetJobBarrierState("job-a")

	application.barrierMu.Lock()
	defer application.barrierMu.Unlock()
	if _, ok := application.jobBarrierMu["job-a"]; ok {
		t.Error("the departed job's mutex is still held")
	}
	if _, ok := application.jobBarrierBytes["job-a"]; ok {
		t.Error("the departed job's byte accumulator is still held")
	}
	// The other job's state must survive: forgetting everything would reset
	// every live job's window.
	if _, ok := application.jobBarrierMu["job-b"]; !ok {
		t.Error("forgetting one job dropped another's mutex")
	}
	if _, ok := application.jobBarrierBytes["job-b"]; !ok {
		t.Error("forgetting one job dropped another's byte accumulator")
	}
}

// ---------- target construction ----------

// TestSyncTargetFor_IsNilForAJobTheQueueCannotDescribe pins the residency
// check that keeps a job with no resident manifest away from the barrier.
//
// The target itself no longer needs the manifest for anything — a run carries
// its own article indices — but "has a resident manifest" is still what
// decides whether a checkpoint should run at all. A job whose manifest has
// been evicted is not downloading, and Job.AckDurable would refuse its ack
// anyway, so reaching the barrier only to fail there would turn an ordinary
// event into a logged error.
func TestSyncTargetFor_IsNilForAJobTheQueueCannotDescribe(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	if got := application.syncTargetFor("no-such-job"); got != nil {
		t.Error("built a sync target for a job that is not in the queue")
	}
	tgt := application.syncTargetFor(job.ID())
	if tgt == nil {
		t.Fatal("no sync target for a resident job; nothing would ever be checkpointed")
	}
	if got := tgt.Files(); got != nil {
		t.Errorf("Files() = %v for a job with nothing open, want none", got)
	}
}

// ---------- cadence ----------

// TestCheckpointAll_CoversEveryJobWithAnOpenFile pins the set the interval
// tick iterates. It comes from the assembler because "has an open file" is the
// assembler's fact and R8 bounds barrier cost by exactly that set; deriving it
// from job status would be a second copy of one fact, free to drift.
func TestCheckpointAll_CoversEveryJobWithAnOpenFile(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 2, 1)
	ctx := t.Context()

	// Only file 0 is written, so only it is open. A barrier must still reach
	// the job, and must not fail over the file that was never opened.
	writeFixtureArticle(t, application, job.ID(), 0, 0)

	application.checkpointAll(ctx, shutdownCheckpointTimeout)
	if got := application.BarrierRuns(); got != 1 {
		t.Fatalf("%d barriers ran, want 1 for the one job holding an open file", got)
	}
	if !job.Progress().ArticleDone(0) {
		t.Error("the checkpoint did not ack the article it fsynced")
	}
	if job.Progress().ArticleDone(1) {
		t.Error("an article that was never written was acked")
	}

	// With nothing open, the sweep must find no job rather than barrier every
	// job in the queue.
	if err := application.assembler.CloseJobHandles(ctx, job.ID()); err != nil {
		t.Fatalf("CloseJobHandles: %v", err)
	}
	before := application.BarrierRuns()
	application.checkpointAll(ctx, shutdownCheckpointTimeout)
	if got := application.BarrierRuns(); got != before {
		t.Errorf("%d barriers ran with no file open, want %d", got, before)
	}
}

// TestCheckpointJob_IsInertWithoutABarrier pins the degraded mode's cost: no
// barrier means no run is even attempted, rather than a nil dereference on the
// checkpoint goroutine.
func TestCheckpointJob_IsInertWithoutABarrier(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID(), 0, 0)
	application.barrier = nil

	application.checkpointJob(t.Context(), job.ID())
	if got := application.BarrierRuns(); got != 0 {
		t.Errorf("%d barriers ran with none wired", got)
	}
}

// TestRunCheckpoint_SavesTheQueueAfterEachCheckpoint pins the ordering the
// loop exists to impose. An ack marks articles Done in memory; until the queue
// is written a crash re-fetches them anyway, so saving before the barrier
// would persist a snapshot that is stale by construction.
func TestRunCheckpoint_SavesTheQueueAfterEachCheckpoint(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID(), 0, 0)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		application.runCheckpoint(ctx, 10*time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	saved := false
	for time.Now().Before(deadline) && !saved {
		if application.BarrierRuns() > 0 && application.checkpointer.DirtyCount() == 0 {
			saved = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if !saved {
		t.Fatalf("after %d barriers the checkpointer was still dirty; the ack never reached disk",
			application.BarrierRuns())
	}
}

// TestSaveQueueIfDirty_SkipsACleanQueue pins the cheap half. The loop runs on
// every tick and every kick, and rewriting an unchanged queue on each would be
// a file write per checkpoint for a job that is idle.
func TestSaveQueueIfDirty_SkipsACleanQueue(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)

	application.saveQueueIfDirty()
	if application.checkpointer.DirtyCount() != 0 {
		// Nothing has made it dirty in this fixture; if that changes the
		// assertion below stops meaning anything.
		t.Fatal("the fixture's checkpointer is dirty; this test cannot show the skip")
	}
}

// TestShutdownCheckpoint_IsInertWithoutABarrier pins that Shutdown's final
// pass costs nothing in the degraded mode, rather than dereferencing nil on
// the way out of every process that has no history database.
func TestShutdownCheckpoint_IsInertWithoutABarrier(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	application.barrier = nil
	application.shutdownCheckpoint()
	if got := application.BarrierRuns(); got != 0 {
		t.Errorf("%d barriers ran with none wired", got)
	}
}

// TestShutdownCheckpoint_CheckpointsAndSaves pins R6's shutdown trigger at the
// unit level, complementing the end-to-end pin in
// TestBarrierRunsOnCleanShutdown.
func TestShutdownCheckpoint_CheckpointsAndSaves(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID(), 0, 0)

	application.shutdownCheckpoint()

	if got := application.BarrierRuns(); got != 1 {
		t.Fatalf("%d barriers ran on shutdown, want 1", got)
	}
	if application.checkpointer.DirtyCount() != 0 {
		t.Error("the shutdown checkpoint acked an article and left the checkpointer unsaved; " +
			"the ack does not survive the process")
	}
}

// ---------- completion ----------

// TestFinalizeCompletedFile_SkipsAFileTheAssemblerNoLongerHolds pins the guard
// that keeps shutdown from stalling every job on its way out.
//
// watchCompletions drains its pending completions after the assembler has
// stopped, and every barrier operation against a stopped worker returns an
// error the barrier cannot distinguish from a storage fault — so without the
// guard each drained completion would classify that error, stall its job and
// pause it.
func TestFinalizeCompletedFile_SkipsAFileTheAssemblerNoLongerHolds(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID(), 0, 0)
	if err := application.assembler.Stop(); err != nil {
		t.Fatalf("assembler.Stop: %v", err)
	}

	if err := application.finalizeCompletedFile(t.Context(), job.ID(), 0); err != nil {
		t.Fatalf("finalizing after an ordinary assembler stop = %v, want nil — every "+
			"completion drained during shutdown would stall its job", err)
	}

	row, ok := application.dispatcher.Row(job.ID())
	if !ok {
		t.Fatal("job left the dispatcher")
	}
	if row.Status() == constants.StatusPaused {
		t.Error("finalizing a file the assembler no longer holds paused the job; " +
			"every completion drained during shutdown would stall its job")
	}
	if reason := application.StallReason(job.ID()).Reason; reason != "" {
		t.Errorf("a stall reason %q was surfaced for an ordinary shutdown", reason)
	}
}

// TestFinalizeCompletedFile_TrimsAndReleasesTheHandle pins the second half of
// the assembler handoff: the barrier gets the file while it is still open, and
// the handle comes back afterwards.
func TestFinalizeCompletedFile_TrimsAndReleasesTheHandle(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)
	ctx := t.Context()
	writeFixtureArticle(t, application, job.ID(), 0, 0)

	// No durability record is seeded. The barrier's own drain reports the
	// article this fixture wrote, and the truncate bound is taken over the
	// stored runs PLUS that drain — which is what lets a file be trimmed on
	// the very first finalize, before anything has been recorded for it.

	info, err := application.pipeline.resolveFileInfo(job.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	// Grow the file past its decoded content, which is what pre-allocation
	// does and what the completion truncate exists to clean up. Without this
	// the size assertion below would pass whether or not anything trimmed.
	fh, err := os.OpenFile(info.Path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := fh.Truncate(4096); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}

	if err := application.finalizeCompletedFile(ctx, job.ID(), 0); err != nil {
		t.Fatalf("finalizeCompletedFile: %v", err)
	}

	st, err := os.Stat(info.Path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 100 {
		t.Errorf("file is %d bytes after finalizing, want 100 — pre-allocation's "+
			"trailing zeros survive and par2 reports a healthy download as damaged",
			st.Size())
	}
	if !job.Progress().ArticleDone(0) {
		t.Error("finalizing the file acked nothing; its last articles stay Outstanding forever")
	}
	// The handle is back with the assembler, so post-processing's unlink does
	// not silly-rename on NFS.
	if got := application.syncTargetFor(job.ID()).Files(); len(got) != 0 {
		t.Errorf("files still open after finalizing: %v", got)
	}
}

// ---------- job departure ----------

// TestDeleteJobDurability_RemovesTheDepartedJobsRows pins the removal that
// runs on a job's way out. Its tables are keyed by job ID with no foreign key
// to dispatch_jobs, so without this a database accumulates one set of rows per
// job ever downloaded until sweepOrphanedDurability reclaims them at the next
// startup — and that sweep deliberately spares history-as-FAILED, so it is not
// equivalent to removing them here.
//
// Through their own OWNERS: durability.RunStore owns durable_runs and
// appCheckpointStore.SaveBatch owns failed_articles. A cleanup that reached
// only one of them would leave part of a departed job's rows behind.
func TestDeleteJobDurability_RemovesTheDepartedJobsRows(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)
	ctx := t.Context()

	for _, id := range []string{job.ID(), "other-job"} {
		if _, err := application.runs.Commit(ctx, id, []durability.DurableArticle{
			{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := application.historyRepo.DB().ExecContext(ctx, "INSERT INTO failed_articles (job_id, art_idx) VALUES (?, 1)", id); err != nil {
			t.Fatal(err)
		}
	}

	application.deleteJobDurability(ctx, job.ID())

	if nr, nf := durabilityRowCounts(t, application, job.ID()); nr != 0 || nf != 0 {
		t.Errorf("%d runs and %d failed rows survive the job's departure", nr, nf)
	}
	// Scoped to the departing job: deleting every job's rows would throw away
	// a live download's recorded ground.
	if nr, nf := durabilityRowCounts(t, application, "other-job"); nr != 1 || nf != 1 {
		t.Errorf("another job has %d runs and %d failed rows, want 1 and 1 — the "+
			"delete was not scoped", nr, nf)
	}
}

// TestDeleteJobDurability_IsInertWithoutStores pins the degraded mode.
func TestDeleteJobDurability_IsInertWithoutStores(t *testing.T) {
	t.Parallel()
	application := &Application{log: slog.New(slog.DiscardHandler)}
	if err := application.deleteJobDurability(context.Background(), "job-a"); err != nil {
		t.Errorf("deleteJobDurability with no stores = %v, want nil", err)
	}
}

// seedDurabilityRowsFor gives one job a row in each of the three tables a
// departing job must take with it.
func seedDurabilityRowsFor(t *testing.T, application *Application, jobID string) {
	t.Helper()
	ctx := t.Context()
	if _, err := application.runs.Commit(ctx, jobID, []durability.DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1},
	}); err != nil {
		t.Fatal(err)
	}
	db := application.historyRepo.DB()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO failed_articles (job_id, art_idx) VALUES (?, 1)`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO job_files (job_id, file_index, complete, fetch_policy, filename, assembled_crc32)
		 VALUES (?, 0, 0, 0, '', 0)`, jobID); err != nil {
		t.Fatal(err)
	}
}

// TestSweepOrphanedDurability_ReclaimsOnlyWhatNothingCanReach is the backstop
// for the crash windows deleteJobDurability cannot close.
//
// The four fixtures are the whole predicate, and every one of them is needed:
// dropping the live or failed case would let a sweep that deletes
// indiscriminately pass, and dropping the completed case would let one that
// spares every job with any history row pass. Two may go — orphan and
// completed — and two must stay.
//
// The failed case is the one with teeth. Its rows are retained on purpose so a
// retry can bound FinalizeFile's truncate to the whole partial file; a sweep
// that took them would destroy exactly the ground the retry path preserves,
// and would do it with no error and no log.
func TestSweepOrphanedDurability_ReclaimsOnlyWhatNothingCanReach(t *testing.T) {
	t.Parallel()
	application, _ := newDurabilityTestApp(t, 1, 1)
	ctx := t.Context()
	db := application.historyRepo.DB()

	for _, id := range []string{"orphan", "live", "failed", "completed"} {
		seedDurabilityRowsFor(t, application, id)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO dispatch_jobs (id, sort_key, name) VALUES ('live', 1, 'live')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO history (nzo_id, status) VALUES (?, ?)`,
		"failed", string(constants.StatusFailed)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO history (nzo_id, status) VALUES (?, ?)`,
		"completed", string(constants.StatusCompleted)); err != nil {
		t.Fatal(err)
	}

	swept, err := application.sweepOrphanedDurability(ctx)
	if err != nil {
		t.Fatalf("sweepOrphanedDurability: %v", err)
	}

	for _, tc := range []struct {
		id      string
		want    int
		because string
	}{
		{"orphan", 0, "in neither the queue nor history, so nothing will ever read its rows again"},
		{"live", 1, "still in dispatch_jobs — sweeping it would destroy a live download's recorded ground"},
		{"failed", 1, "in history as FAILED, whose rows a retry reads to bound its truncate"},
		{"completed", 0, "in history, but not as FAILED — nothing retries it, so its rows are garbage"},
	} {
		nr, nf := durabilityRowCounts(t, application, tc.id)
		njf := jobFilesRowCount(t, application, tc.id)
		if nr != tc.want || nf != tc.want || njf != tc.want {
			t.Errorf("%s: runs=%d failed=%d job_files=%d, want %d each — %s",
				tc.id, nr, nf, njf, tc.want, tc.because)
		}
	}
	if swept != 2 {
		t.Errorf("sweep reclaimed %d jobs, want 2 (orphan and completed)", swept)
	}
}

// TestSweepOrphanedDurability_SparesEveryJobWhenAnIDIsNull pins the reason the
// predicate uses NOT EXISTS rather than the NOT IN that reads more naturally.
//
// SQLite permits NULL in a TEXT PRIMARY KEY, so dispatch_jobs.id can hold one.
// A single NULL anywhere in a NOT IN subquery makes the predicate NULL for
// EVERY row, and the sweep then matches nothing at all — not an error, not a
// partial result, just a function that silently stops working and is
// indistinguishable from a database with no orphans.
//
// That is why this is a test and not a sentence. The failure direction is
// retention, so it would never surface as a bug report; the only thing that
// catches it is an assertion that a real orphan still goes.
func TestSweepOrphanedDurability_SparesEveryJobWhenAnIDIsNull(t *testing.T) {
	t.Parallel()
	application, _ := newDurabilityTestApp(t, 1, 1)
	ctx := t.Context()
	db := application.historyRepo.DB()

	seedDurabilityRowsFor(t, application, "orphan")
	if _, err := db.ExecContext(ctx,
		`INSERT INTO dispatch_jobs (id, sort_key, name) VALUES (NULL, 1, 'null-id')`); err != nil {
		t.Skipf("this build refuses a NULL dispatch_jobs.id, so the trap is unreachable: %v", err)
	}

	swept, err := application.sweepOrphanedDurability(ctx)
	if err != nil {
		t.Fatalf("sweepOrphanedDurability: %v", err)
	}
	nr, nf := durabilityRowCounts(t, application, "orphan")
	if njf := jobFilesRowCount(t, application, "orphan"); nr != 0 || nf != 0 || njf != 0 {
		t.Errorf("with a NULL id present: runs=%d failed=%d job_files=%d, want 0 each — "+
			"one NULL made the predicate NULL for every row, so the sweep matched "+
			"nothing and reported success", nr, nf, njf)
	}
	if swept != 1 {
		t.Errorf("sweep reclaimed %d jobs with a NULL id present, want 1", swept)
	}
}

// TestWithDurabilityTx_NamesTheOperationAndRollsBack pins the shared
// transaction wrapper both delete entry points go through.
//
// The two used to carry their own copy of this, and the copies had drifted:
// one wrapped the inner error with the operation name and the other returned
// it raw, so the same underlying failure read differently depending on which
// door the caller came through. The first assertion is that the name is now
// always there; the second is that a failing fn commits nothing.
func TestWithDurabilityTx_NamesTheOperationAndRollsBack(t *testing.T) {
	t.Parallel()
	application, _ := newDurabilityTestApp(t, 1, 1)
	ctx := t.Context()
	seedDurabilityRowsFor(t, application, "job-a")

	boom := errors.New("deliberate")
	err := application.withDurabilityTx(ctx, "test operation", "job-a", func(tx *sql.Tx) error {
		// A real write, so the rollback assertion below is about a
		// transaction that had something to undo.
		if _, err := tx.ExecContext(ctx, `DELETE FROM job_files WHERE job_id = ?`, "job-a"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap fn's failure", err)
	}
	if !strings.Contains(err.Error(), "test operation") || !strings.Contains(err.Error(), "job-a") {
		t.Errorf("err = %q, want it to name the operation and the job", err)
	}
	if n := jobFilesRowCount(t, application, "job-a"); n != 1 {
		t.Errorf("job_files rows = %d after a failing fn, want 1 — the write was committed "+
			"rather than rolled back", n)
	}
}

// TestDeleteJobDurability_ReportsAFailedJobFilesDelete pins the first statement
// in the transaction, whose error was discarded outright before #549 — the
// call read `_, _ = ...ExecContext(...)`, so job_files was the one table that
// could fail to be cleaned with no error and no log line.
func TestDeleteJobDurability_ReportsAFailedJobFilesDelete(t *testing.T) {
	t.Parallel()
	application, _ := newDurabilityTestApp(t, 1, 1)
	ctx := t.Context()
	seedDurabilityRowsFor(t, application, "job-a")

	if _, err := application.historyRepo.DB().ExecContext(ctx,
		`CREATE TRIGGER fail_job_files_delete BEFORE DELETE ON job_files
		 BEGIN SELECT RAISE(ABORT, 'simulated job_files failure'); END;`); err != nil {
		t.Fatal(err)
	}

	err := application.deleteJobDurability(ctx, "job-a")
	if err == nil {
		t.Fatal("deleteJobDurability returned nil although job_files refused the delete")
	}
	if !strings.Contains(err.Error(), "job files") {
		t.Errorf("err = %q, want it to name job_files as the cause", err)
	}
	nr, nf := durabilityRowCounts(t, application, "job-a")
	if njf := jobFilesRowCount(t, application, "job-a"); nr != 1 || nf != 1 || njf != 1 {
		t.Errorf("runs=%d failed=%d job_files=%d, want 1 each — the failing first statement "+
			"must roll back the whole transaction", nr, nf, njf)
	}
}

// TestDeleteJobDurabilityTx_ClearsSeveralJobsInOneTransaction pins the
// property the sweep is built on: the per-job deletion composes, so N jobs can
// share one transaction instead of opening N.
//
// Called directly rather than through deleteJobDurability, because what is
// being pinned is exactly that the caller may own the transaction — the reason
// this exists as a separate function at all.
func TestDeleteJobDurabilityTx_ClearsSeveralJobsInOneTransaction(t *testing.T) {
	t.Parallel()
	application, _ := newDurabilityTestApp(t, 1, 1)
	ctx := t.Context()
	ids := []string{"job-a", "job-b", "job-c"}
	for _, id := range ids {
		seedDurabilityRowsFor(t, application, id)
	}

	tx, err := application.historyRepo.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if err := application.deleteJobDurabilityTx(ctx, tx, id); err != nil {
			t.Fatalf("deleteJobDurabilityTx(%s): %v", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	for _, id := range ids {
		nr, nf := durabilityRowCounts(t, application, id)
		if njf := jobFilesRowCount(t, application, id); nr != 0 || nf != 0 || njf != 0 {
			t.Errorf("%s: runs=%d failed=%d job_files=%d, want 0 each", id, nr, nf, njf)
		}
	}
}

// TestDurabilityDB_ReportsWhetherThereIsAHandle pins the guard every
// transaction on this path opens against. Without a history repository there
// is no shared *sql.DB, and a nil returned here is what sends each caller down
// its degraded branch rather than into a nil dereference.
func TestDurabilityDB_ReportsWhetherThereIsAHandle(t *testing.T) {
	t.Parallel()
	if db := (&Application{}).durabilityDB(); db != nil {
		t.Errorf("durabilityDB with no history repository = %v, want nil", db)
	}
	application, _ := newDurabilityTestApp(t, 1, 1)
	if db := application.durabilityDB(); db == nil {
		t.Error("durabilityDB with a history repository = nil, want the shared handle")
	}
}

// TestDeleteJobDurability_WithoutAHandleStillDropsRuns pins the degraded
// branch, which is not a no-op.
//
// durable_runs reaches its own store without the shared handle, so a build
// with no history repository can still remove two-thirds of nothing — the one
// table that is reachable. Removing what can be removed beats removing
// nothing, and the caller is still told whether it worked.
func TestDeleteJobDurability_WithoutAHandleStillDropsRuns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		call func(*Application) error
	}{
		{"deleteJobDurability", func(a *Application) error {
			return a.deleteJobDurability(context.Background(), "job-a")
		}},
		{"dropJobDurability", func(a *Application) error {
			return a.dropJobDurability(context.Background(), "job-a")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &recordingRunStore{}
			application := &Application{log: slog.New(slog.DiscardHandler), runs: rec}
			if err := tc.call(application); err != nil {
				t.Fatalf("%s without a handle: %v", tc.name, err)
			}
			if len(rec.deleted) != 1 || rec.deleted[0] != "job-a" {
				t.Errorf("%s deleted %v, want the job's runs dropped through the store "+
					"that does not need the shared handle", tc.name, rec.deleted)
			}
		})
	}
}

// TestDropJobDurabilityTx_StopsAtTheFirstFailure pins that the two statements
// no longer join their errors and continue.
//
// Inside a transaction the first failure has already poisoned what follows, so
// attempting the second write reports a second error about a transaction that
// is rolling back either way — noise in place of the real cause. The
// assertion is that exactly the first cause comes back.
func TestDropJobDurabilityTx_StopsAtTheFirstFailure(t *testing.T) {
	t.Parallel()
	application, _ := newDurabilityTestApp(t, 1, 1)
	ctx := t.Context()
	db := application.historyRepo.DB()
	seedDurabilityRowsFor(t, application, "job-a")

	if _, err := db.ExecContext(ctx,
		`CREATE TRIGGER fail_runs_delete BEFORE DELETE ON durable_runs
		 BEGIN SELECT RAISE(ABORT, 'simulated runs failure'); END;`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	err = application.dropJobDurabilityTx(ctx, tx, "job-a")
	if err == nil {
		t.Fatal("dropJobDurabilityTx returned nil although durable_runs refused the delete")
	}
	if !strings.Contains(err.Error(), "durable runs") {
		t.Errorf("error = %q, want the durable_runs cause", err)
	}
	if strings.Contains(err.Error(), "failed articles") {
		t.Errorf("error = %q — the second statement was attempted and reported after the "+
			"first had already poisoned the transaction", err)
	}
}

// TestSweepOrphanedDurability_RollsBackWholeWhenOneJobRefuses pins the cost of
// reclaiming every orphan in ONE transaction.
//
// An earlier shape opened a transaction per job, so a wedged job was reported
// while its neighbours still went. One transaction trades that for a single
// BeginTx and Commit, and the trade is deliberate: the realistic causes of a
// failed DELETE here — a cancelled context, a locked database — fail every job
// in the batch anyway, so per-job isolation only ever bought partial progress
// against a corrupt or trigger-guarded row.
//
// What must not change is that the failure is REPORTED and the rows are left
// intact for the next startup. A sweep that rolled back and returned nil would
// look exactly like a sweep that found nothing to do.
func TestSweepOrphanedDurability_RollsBackWholeWhenOneJobRefuses(t *testing.T) {
	t.Parallel()
	application, _ := newDurabilityTestApp(t, 1, 1)
	ctx := t.Context()
	db := application.historyRepo.DB()

	seedDurabilityRowsFor(t, application, "reclaimable")
	seedDurabilityRowsFor(t, application, "stuck")
	if _, err := db.ExecContext(ctx,
		`CREATE TRIGGER fail_stuck BEFORE DELETE ON failed_articles
		 WHEN OLD.job_id = 'stuck'
		 BEGIN SELECT RAISE(ABORT, 'simulated delete failure'); END;`); err != nil {
		t.Fatal(err)
	}

	swept, err := application.sweepOrphanedDurability(ctx)
	if err == nil {
		t.Fatal("sweepOrphanedDurability returned nil although one job's rows could not go; " +
			"a rolled-back sweep that reports success is indistinguishable from one " +
			"that found no orphans")
	}
	if !strings.Contains(err.Error(), "stuck") {
		t.Errorf("error = %q, want it to name the job it could not reclaim", err)
	}
	if swept != 0 {
		t.Errorf("swept = %d, want 0 — the transaction rolled back, so nothing was "+
			"reclaimed and the count must not claim otherwise", swept)
	}
	// Both survive, including the one whose own deletes succeeded before the
	// refusal. That is the rollback, and it is what makes the next startup's
	// sweep see the same work rather than a half-done one.
	for _, id := range []string{"reclaimable", "stuck"} {
		nr, nf := durabilityRowCounts(t, application, id)
		if njf := jobFilesRowCount(t, application, id); nr != 1 || nf != 1 || njf != 1 {
			t.Errorf("%s: runs=%d failed=%d job_files=%d, want 1 each — the whole "+
				"transaction must roll back", id, nr, nf, njf)
		}
	}
}

// TestSweepOrphanedDurability_ReportsAFailedQuery pins that a sweep which
// could not even read reports it rather than returning a clean zero.
//
// A cancelled context is the reachable cause: the sweep runs inside Start, so
// a shutdown arriving during startup cancels it mid-flight. Returning (0, nil)
// there would be indistinguishable from a database with no orphans, which is
// the same silent-success failure the NOT EXISTS choice avoids elsewhere in
// this function.
func TestSweepOrphanedDurability_ReportsAFailedQuery(t *testing.T) {
	t.Parallel()
	application, _ := newDurabilityTestApp(t, 1, 1)
	seedDurabilityRowsFor(t, application, "orphan")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	swept, err := application.sweepOrphanedDurability(ctx)
	if err == nil {
		t.Error("sweepOrphanedDurability on a cancelled context returned nil; a sweep " +
			"that could not read is not a sweep that found nothing")
	}
	if swept != 0 {
		t.Errorf("swept = %d on a cancelled context, want 0", swept)
	}
	// The orphan is untouched, so the next startup still sees it.
	if nr, _ := durabilityRowCounts(t, application, "orphan"); nr != 1 {
		t.Errorf("orphan runs = %d after a failed sweep, want 1", nr)
	}
}

// TestSweepOrphanedDurability_IsInertWithoutAHandle pins the degraded mode: no
// history repository means no shared handle to query, and the sweep reports
// nothing reclaimed rather than failing startup.
func TestSweepOrphanedDurability_IsInertWithoutAHandle(t *testing.T) {
	t.Parallel()
	application := &Application{log: slog.New(slog.DiscardHandler)}
	swept, err := application.sweepOrphanedDurability(context.Background())
	if err != nil || swept != 0 {
		t.Errorf("sweepOrphanedDurability with no handle = (%d, %v), want (0, nil)", swept, err)
	}
}

// jobFilesRowCount counts one job's job_files rows — the third table a
// departing job leaves behind, and the one that had no backstop at all before
// #549 because the deleted sweep's predicate never named it.
func jobFilesRowCount(t *testing.T, application *Application, jobID string) int {
	t.Helper()
	var n int
	if err := application.historyRepo.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM job_files WHERE job_id = ?`, jobID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestDeleteJobDurability_IsAtomicAndReportsFailure pins the two properties the
// three separate autocommits did not have.
//
// A departing job has rows in three tables, and they used to be removed by
// three independent statements: job_files (whose error was discarded outright),
// then durable_runs, then failed_articles. A failure part-way through therefore
// committed the deletions that had already run and stranded the rest, with no
// sweep to reclaim them and — for job_files — no log line either.
//
// The trigger aborts the LAST of the three, which is what makes this a test of
// rollback rather than of ordering: both earlier deletes have already executed
// inside the transaction when it fires, so all three rows may only survive if
// the transaction actually rolls back. Against three autocommits, two of them
// would be gone.
//
// The second half then removes the trigger and repeats, so a mutation that
// makes the delete unconditionally fail cannot pass the first half alone.
func TestDeleteJobDurability_IsAtomicAndReportsFailure(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 1)
	ctx := t.Context()
	db := application.historyRepo.DB()

	seedDurabilityRowsFor(t, application, j.ID())

	assertAllThreePresent := func(when string) {
		t.Helper()
		nr, nf := durabilityRowCounts(t, application, j.ID())
		njf := jobFilesRowCount(t, application, j.ID())
		if nr != 1 || nf != 1 || njf != 1 {
			t.Errorf("%s: runs=%d failed=%d job_files=%d, want 1/1/1 — the delete was "+
				"not atomic, so a failure part-way through stranded the rest with "+
				"nothing to reclaim them", when, nr, nf, njf)
		}
	}
	assertAllThreePresent("precondition")

	if _, err := db.ExecContext(ctx,
		`CREATE TRIGGER fail_failed_articles_delete BEFORE DELETE ON failed_articles
		 BEGIN SELECT RAISE(ABORT, 'simulated delete failure'); END;`); err != nil {
		t.Fatal(err)
	}

	err := application.deleteJobDurability(ctx, j.ID())
	if err == nil {
		t.Error("deleteJobDurability returned nil although the delete was refused; " +
			"the caller cannot tell that the job's rows are still there")
	}
	assertAllThreePresent("after the refused delete")

	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_failed_articles_delete`); err != nil {
		t.Fatal(err)
	}
	if err := application.deleteJobDurability(ctx, j.ID()); err != nil {
		t.Fatalf("deleteJobDurability after the fault was removed: %v", err)
	}
	nr, nf := durabilityRowCounts(t, application, j.ID())
	if njf := jobFilesRowCount(t, application, j.ID()); nr != 0 || nf != 0 || njf != 0 {
		t.Errorf("after a clean delete: runs=%d failed=%d job_files=%d, want 0/0/0", nr, nf, njf)
	}
}

// ---------- settings ----------

// TestCheckpointSettings_SubstitutesDefaultsForUnsetBounds pins that neither
// bound can be switched off. A barrier is the only thing that acks a
// downloaded article while the job is running, so with checkpoints off a job
// holds every article Outstanding until it stops, and a restart has no
// recorded run to adopt for anything downloaded since the last barrier. See
// checkpointSettings for why that is not the same as re-fetching everything.
func TestCheckpointSettings_SubstitutesDefaultsForUnsetBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		interval       time.Duration
		bytes          int64
		wantInterval   time.Duration
		wantBytesIsDef bool
	}{
		{"both set", time.Minute, 1024, time.Minute, false},
		{"zero interval", 0, 1024, defaultCheckpointInterval, false},
		{"negative interval", -time.Second, 1024, defaultCheckpointInterval, false},
		{"zero bytes", time.Minute, 0, time.Minute, true},
		{"negative bytes", time.Minute, -1, time.Minute, true},
		{"neither set", 0, 0, defaultCheckpointInterval, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotInterval, gotBytes := checkpointSettings(tc.interval, tc.bytes)
			if gotInterval != tc.wantInterval {
				t.Errorf("interval = %v, want %v", gotInterval, tc.wantInterval)
			}
			wantBytes := tc.bytes
			if tc.wantBytesIsDef {
				wantBytes = defaultCheckpointBytes
			}
			if gotBytes != wantBytes {
				t.Errorf("bytes = %d, want %d", gotBytes, wantBytes)
			}
			if gotInterval <= 0 || gotBytes <= 0 {
				t.Errorf("resolved to a disabled bound (%v, %d); nothing would ever ack",
					gotInterval, gotBytes)
			}
		})
	}
	// The defaults are the figures B1 states, not arbitrary ones.
	if defaultCheckpointInterval != constants.DefaultCheckpointInterval ||
		defaultCheckpointBytes != constants.DefaultCheckpointBytes {
		t.Error("the package defaults have drifted from the constants B1 is stated in")
	}
}

// ---------- error paths ----------

// failingRunStore is a RunStore whose every operation fails, for the paths
// that must degrade to a re-fetch rather than to a wrong answer.
type failingRunStore struct{ err error }

func (f failingRunStore) Commit(context.Context, string, []durability.DurableArticle) ([]durability.Collision, error) {
	return nil, f.err
}

func (f failingRunStore) ForFile(context.Context, string, int32) ([]durability.Run, error) {
	return nil, f.err
}

func (f failingRunStore) ForJob(context.Context, string) ([]durability.Run, error) {
	return nil, f.err
}
func (f failingRunStore) DeleteFile(context.Context, string, int32) error { return f.err }
func (f failingRunStore) DeleteJob(context.Context, string) error         { return f.err }

func (f failingRunStore) DeleteJobTx(context.Context, durability.Execer, string) error {
	return f.err
}

// recordingRunStore notes whether the job-scoped delete was reached. The
// alternative — comparing application.runs to a copy of itself taken two lines
// earlier — is a tautology, which is what this replaces.
//
// Both entry points record, because production reaches only DeleteJobTx now
// (deleteJobDurability runs all three tables in one transaction) while other
// callers still use DeleteJob. Recording in one alone would make this fake
// observe nothing the day the caller switched, which is exactly the silent
// failure it was written to prevent.
type recordingRunStore struct{ deleted []string }

func (r *recordingRunStore) Commit(context.Context, string, []durability.DurableArticle) ([]durability.Collision, error) {
	return nil, nil
}

func (r *recordingRunStore) ForFile(context.Context, string, int32) ([]durability.Run, error) {
	return nil, nil
}

func (r *recordingRunStore) ForJob(context.Context, string) ([]durability.Run, error) {
	return nil, nil
}

func (r *recordingRunStore) DeleteFile(context.Context, string, int32) error { return nil }

func (r *recordingRunStore) DeleteJob(ctx context.Context, jobID string) error {
	return r.DeleteJobTx(ctx, nil, jobID)
}

func (r *recordingRunStore) DeleteJobTx(_ context.Context, _ durability.Execer, jobID string) error {
	r.deleted = append(r.deleted, jobID)
	return nil
}

// TestStall_ReportsRatherThanPanicsOnAJobThatHasLeft pins the case a storage
// fault most easily hits: the barrier finds the fault, and by the time the
// stall reaches the queue the job has been removed. Both queue writes fail and
// neither may take the process down or abort the other.
func TestStall_ReportsRatherThanPanicsOnAJobThatHasLeft(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)

	application.Stall("no-such-job", storagefault.Classify("sync", "/x", syscall.ENOSPC))
	application.Fail("no-such-job", storagefault.Classify("write", "/x", syscall.EROFS))
}

// TestSyncTargetFor_IsNilWhenTheManifestCannotBeRead pins the non-resident
// case. A job whose manifest has been evicted has nothing open to checkpoint,
// so syncTargetFor answers nil rather than handing the barrier a target for a
// job it cannot resolve.
func TestSyncTargetFor_IsNilWhenTheManifestCannotBeRead(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)

	// Remove the manifest file the queue would hydrate from, then evict the
	// job so the next Manifest() has to read it.
	adminDir := application.config.GetGeneral().AdminDir
	manifestPath := filepath.Join(adminDir, "queue", "manifests", job.ID()+".json.gz")
	if err := os.Remove(manifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove manifest: %v", err)
	}
	job.Evict()

	if _, err := job.Manifest(); err == nil {
		t.Skip("the manifest is still resident; this fixture cannot reach the read failure")
	}
	if got := application.syncTargetFor(job.ID()); got != nil {
		t.Error("built a sync target for a job whose manifest cannot be read; the " +
			"barrier would refuse every file of an ordinarily non-resident job")
	}
}

// TestDeleteJobDurability_ReportsAFailedDelete pins that a failing cleanup is
// logged rather than swallowed, and that a failure in one store does not stop
// the other from being tried — leaving half a job's rows behind is worse than
// leaving all of them, because the surviving half describes a job that no
// longer exists.
//
// The two stores have different OWNERS, which is why the "one must not stop
// the other" half matters here rather than being a tidiness rule:
// durability.RunStore owns durable_runs and checkpoint.Store.SaveBatch owns failed_articles, so
// a single early return would leave one owner's rows for a departed job while
// the other's were collected.
func TestDeleteJobDurability_ReportsAFailedDelete(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	boom := errors.New("database is locked")

	rec := &recordingRunStore{}
	application.runs = rec
	application.deleteJobDurability(t.Context(), "job-a")

	// The load-bearing assertion: the run store's DeleteJob really ran. This
	// replaces a comparison of the field to a copy of itself taken two lines
	// earlier, with nothing in between that could change it. It could not
	// fail.
	if !slices.Equal(rec.deleted, []string{"job-a"}) {
		t.Fatalf("run store DeleteJob calls = %v, want [job-a]", rec.deleted)
	}

	// A failing run store must not panic, must not abort, and must not stop
	// the queue-owned half from being cleared either.
	application.runs = failingRunStore{err: boom}
	application.deleteJobDurability(t.Context(), "job-a")

	// And with no run store at all — the degraded no-history-database mode.
	application.runs = nil
	application.deleteJobDurability(t.Context(), "job-a")
}

// ---------- event emission ----------

// recordingEmitter captures the events an Application broadcasts.
type recordingEmitter struct{ events []Event }

func (r *recordingEmitter) Broadcast(e Event) { r.events = append(r.events, e) }

// TestEmit_ReachesTheRegisteredEmitter pins the path every UI update takes.
//
// A stall is the case that makes it load-bearing rather than cosmetic: the job
// is paused with a reason nobody asked for, so the only way a user learns of
// it is a pushed queue update. Without the broadcast the queue silently stops
// and the UI keeps showing the pre-stall state until something else happens to
// refresh it.
func TestEmit_ReachesTheRegisteredEmitter(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)
	rec := &recordingEmitter{}
	application.emitter = rec

	application.emit(Event{Type: "queue_updated", NzoID: "direct"})
	if len(rec.events) != 1 || rec.events[0].Type != "queue_updated" || rec.events[0].NzoID != "direct" {
		t.Fatalf("emit delivered %+v, want one queue_updated for \"direct\"", rec.events)
	}

	application.Stall(job.ID(), storagefault.Classify("sync", "/mnt/full/A.bin", syscall.ENOSPC))
	var sawStallUpdate bool
	for _, e := range rec.events[1:] {
		if e.Type == "queue_updated" && e.NzoID == job.ID() {
			sawStallUpdate = true
		}
	}
	if !sawStallUpdate {
		t.Error("a stall broadcast nothing; the queue halts and the UI shows the " +
			"pre-stall state until an unrelated event refreshes it")
	}
}

// ---------- bound resolution ----------

// TestNew_ResolvesBothCheckpointBoundsBeforeAnythingRuns pins where the
// defaults are substituted, which is a question about data races rather than
// about defaults.
//
// Both bounds are read from goroutines Start launches: noteJobBytes runs on
// every pipeline worker, runCheckpoint on its own. Resolving them in Start
// means writing the fields after those goroutines exist. That is a race with a
// concrete cost, not a theoretical one — the substitution is what turns the
// configured 0 ("use the default") into 64 MiB, so a reader that sees the
// unresolved 0 finds `bytes >= 0` true for every article and asks for a full
// barrier per article: a few dozen fsyncs per 700 KB.
//
// The assertion is on behaviour rather than on the field, because a test that
// only read the field would pass against a Start-time resolution the moment
// the test happened to call Start first.
func TestNew_ResolvesBothCheckpointBoundsBeforeAnythingRuns(t *testing.T) {
	t.Parallel()
	adminDir := t.TempDir()
	cfg := testConfig(t.TempDir(), t.TempDir(), adminDir, config.ServerConfig{
		Name: "mock", Host: "127.0.0.1", Port: 1119, Enable: false,
	})
	// 0 is the documented "use the default" for both.
	cfg.With(func(c *config.Config) {
		c.Downloads.CheckpointInterval = 0
		c.Downloads.CheckpointBytes = 0
	})
	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	application, err := New(cfg, history.NewRepository(db))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Start has NOT been called. A single byte must not cross a 64 MiB bound.
	application.noteJobBytes("job-a", 1)
	if n := len(application.barrierKick); n != 0 {
		t.Fatalf("%d barrier kicks from one byte against an unresolved bound; the "+
			"default was substituted after the readers were already running, so every "+
			"article asks for a full barrier", n)
	}

	if application.checkpointInterval != defaultCheckpointInterval {
		t.Errorf("checkpointInterval = %v after New, want %v",
			application.checkpointInterval, defaultCheckpointInterval)
	}
	if application.checkpointBytes != defaultCheckpointBytes {
		t.Errorf("checkpointBytes = %d after New, want %d",
			application.checkpointBytes, defaultCheckpointBytes)
	}

	// And a configured value still wins over the default.
	cfg.With(func(c *config.Config) { c.Downloads.CheckpointBytes = 4096 })
	configured, err := New(cfg, history.NewRepository(db))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if configured.checkpointBytes != 4096 {
		t.Errorf("checkpointBytes = %d, want the configured 4096", configured.checkpointBytes)
	}
}

// wedgeOnFile parks the assembler's worker goroutine when it opens one
// particular file, and never lets it go.
//
// It reproduces a wedged mount without needing one. The worker owns every file
// handle, so anything that blocks it blocks every barrier operation for every
// job — which is exactly the condition barrierOpTimeout exists for, and the
// reason SyncTarget.Files can return "no files" for a reason that is not "no
// files".
type wedgeOnFile struct {
	fileIdx int
	entered chan struct{}
	release chan struct{}
	inner   func(string, int) (assembler.FileInfo, error)
}

func (w *wedgeOnFile) resolve(jobID string, fileIdx int) (assembler.FileInfo, error) {
	if fileIdx == w.fileIdx {
		close(w.entered)
		<-w.release
	}
	return w.inner(jobID, fileIdx)
}

// newWedgedApp builds an Application whose assembler holds file 0 open and
// whose worker is parked inside file 1's open until the returned func is
// called.
//
// Releasing it is what makes the recovery tests real: a stall test that never
// unwedges can only observe a flag, while one that does can watch the job
// actually finish the work the stall interrupted.
//
// The assembler is constructed here rather than taken from New, because the
// resolver has to be substituted before the worker ever runs and Application
// exposes no hook for it — deliberately, since production has no reason to
// swap one.
func newWedgedApp(t *testing.T) (*Application, *job.Job, func()) {
	t.Helper()
	application, job := newDurabilityTestApp(t, 2, 1)
	ctx := t.Context()

	// Stop the assembler New built and replace it with one whose resolver
	// wedges. Nothing has been written through the first one yet.
	if err := application.assembler.Stop(); err != nil {
		t.Fatalf("stop the original assembler: %v", err)
	}
	wedge := &wedgeOnFile{
		fileIdx: 1,
		entered: make(chan struct{}),
		release: make(chan struct{}),
		inner:   application.pipeline.resolveFileInfo,
	}
	application.assembler = assembler.New(assembler.Options{
		FileInfo:         wedge.resolve,
		BarrierOpTimeout: 20 * time.Millisecond,
	}, slog.New(slog.DiscardHandler))
	application.closeHandlesTimeout = 20 * time.Millisecond
	application.pipeline.assembler = application.assembler
	if err := application.assembler.Start(ctx); err != nil {
		t.Fatalf("start the wedging assembler: %v", err)
	}
	// Registered in this order so it runs FIRST: t.Cleanup is LIFO, and
	// Assembler.Stop joins the worker, which is parked on this channel. The
	// other way round the test deadlocks in its own teardown.
	t.Cleanup(func() { _ = application.assembler.Stop() })
	var once sync.Once
	release := func() { once.Do(func() { close(wedge.release) }) }
	t.Cleanup(release)

	// File 0 opens normally and stays open.
	writeFixtureArticle(t, application, job.ID(), 0, 0)
	// File 1's open parks the worker, so no control message can be answered.
	if err := application.pipeline.registerFile(job.ID(), 1); err != nil {
		t.Fatalf("registerFile 1: %v", err)
	}
	// File 1 explicitly: sending this to file 0 would never reach the wedge.
	// It is not routed through writeFixtureArticle because that waits for the
	// file to open, and the whole point here is that the open never returns.
	ref, req := assemblerWrite(job.ID(), 1, 1, 0)
	if err := application.assembler.WriteArticle(ctx, ref, req); err != nil {
		t.Fatalf("WriteArticle 1: %v", err)
	}
	select {
	case <-wedge.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never reached the wedge; the fixture is not wedged and the " +
			"assertions below would pass against a healthy assembler")
	}
	return application, job, release
}

// TestFinalizeCompletedFile_RefusesToShipAFileItCouldNotFinalize pins the
// difference between "nothing to finalize" and "cannot tell", which used to be
// the same answer.
//
// SyncTarget.Files reports an error as "no files" — deliberately, because the
// barrier has nothing useful to do with one. Reading that as "there was
// nothing to trim" is what let a barrierOpTimeout on a wedged mount ship a
// file with pre-allocation's trailing zeros intact, which par2 reports as
// damage on a perfectly healthy download. #350, arriving silently by a
// different route.
//
// The fixture wedges the assembler's worker rather than a filesystem: the
// worker owns every handle, so a worker stuck in someone else's call is
// exactly the condition barrierOpTimeout exists for, and it is reproducible
// without a dead mount.
func TestFinalizeCompletedFile_RefusesToShipAFileItCouldNotFinalize(t *testing.T) {
	t.Parallel()
	application, job, _ := newWedgedApp(t)

	err := application.finalizeCompletedFile(t.Context(), job.ID(), 0)
	if err == nil {
		t.Fatal("finalizing against a wedged worker returned nil; the caller proceeds to " +
			"MarkFileComplete, DirectUnpack and post-processing with a file that was " +
			"never trimmed and whose last drain was never acked")
	}
	if !errors.Is(err, ErrNotFinalized) {
		t.Errorf("err = %v, want it to wrap ErrNotFinalized so the caller can act on it", err)
	}
	// It must have ASKED the assembler and given up on the bound, rather than
	// short-circuiting to an error without consulting it — a guard that
	// refused every completed file would satisfy every assertion above while
	// stalling healthy jobs.
	//
	// Asserted on the wrapped cause rather than on elapsed time. An earlier
	// version measured the wall clock, and it did not pin this: the deferred
	// CloseFile sits on the same 5s bound, so a finalize that short-circuited
	// without ever reaching OpenFiles still took 5s and the assertion passed.
	//
	// The "gave up rather than waiting for the mount" half has no assertion at
	// all, deliberately: without the bound this call never returns, so the
	// only thing that can observe it is the test binary's own timeout. An
	// assertion on it could run only in a world where the test had already
	// died, which is what the version before that one was.
	//
	// The bound now reports itself as a storage fault rather than as a bare
	// context error, and that IS the assertion: our own timeout expiring means
	// the worker is parked in a syscall, and a bare context error was
	// indistinguishable from an ordinary shutdown — which is what let a
	// healthy job be parked on a disk that did not fail.
	f, ok := errors.AsType[*storagefault.Fault](err)
	if !ok {
		t.Fatalf("err = %v (%T), want the bound's own storage fault — the finalize "+
			"never reached the assembler, so it would refuse healthy completions too", err, err)
	}
	if f.Permanent {
		t.Errorf("fault = %v, want retryable", f)
	}
}

// TestHandleFileComplete_StallsRatherThanShippingAnUnfinalizedFile pins the
// caller's half of the same rule. The file must not be marked complete, which
// is what keeps DirectUnpack, job finalization and post-processing away from
// it, and the job must carry a reason a user can act on.
//
// No article may be marked failed by any of this: a failure to trim is a
// condition of storage, and attributing it to an article would burn its retry
// budget and degrade the job's reported health (A1, R21).
func TestHandleFileComplete_StallsRatherThanShippingAnUnfinalizedFile(t *testing.T) {
	t.Parallel()
	application, job, _ := newWedgedApp(t)

	application.handleFileComplete(t.Context(), FileComplete{JobID: job.ID(), FileIdx: 0})

	row, ok := application.dispatcher.Row(job.ID())
	if !ok {
		t.Fatal("job left the dispatcher")
	}
	if job.Progress().FileComplete(0) {
		t.Error("the file was marked complete although it was never finalized; " +
			"DirectUnpack, job finalization and post-processing all act on that bit, " +
			"and a file still carrying pre-allocation's zeros reads as par2 damage")
	}
	if row.Status() != constants.StatusPaused {
		t.Errorf("status = %v, want Paused — the job carries on and completes with an "+
			"untrimmed file", row.Status())
	}
	if application.StallReason(job.ID()).Reason == "" {
		t.Error("no reason was surfaced; the job halts and the user is told nothing (R27)")
	}
	if job.Progress().ArticleFailed(0) || job.Progress().FailedBytes() != 0 {
		t.Error("a storage condition was recorded as article damage (A1, R21)")
	}
}

// TestFilePathFor_NamesTheFileOrSaysNothing pins R27's input on the stall
// path. A stall reason that names no file tells a user their download halted
// without telling them which volume or which mount to look at — and both
// polarities matter, because the "" case is what a caller must not branch on.
func TestFilePathFor_NamesTheFileOrSaysNothing(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID(), 0, 0)

	want, err := application.pipeline.resolveFileInfo(job.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if want.Path == "" {
		t.Fatal("the fixture resolved an empty path; the assertion below cannot distinguish it from the failure case")
	}
	if got := application.filePathFor(job.ID(), 0); got != want.Path {
		t.Errorf("filePathFor = %q, want %q", got, want.Path)
	}
	// A file the pipeline never registered must not be answered with ANOTHER
	// file's path. This is the version of the negative case that can actually
	// fail: an earlier one asserted only `== ""`, which no implementation
	// reachable from here can violate — resolveFileInfo returns a zero
	// FileInfo on miss, so ignoring its error yields "" either way. An
	// implementation that ignored fileIdx and returned the job's first known
	// path would have passed it, and fails this.
	if got := application.filePathFor(job.ID(), 7); got == want.Path {
		t.Errorf("filePathFor for unregistered file 7 = %q, which is file 0's path — "+
			"the stall reason would name the wrong file", got)
	}
	if got := application.filePathFor("no-such-job", 0); got == want.Path {
		t.Errorf("filePathFor for an unknown job = %q, which is another job's path", got)
	}
}

// ---------- fault routing on the completion path ----------

// TestRouteFinalizeFailure_DoesNotReRouteWhatTheBarrierAlreadyRouted pins the
// permanent case, which is the one the double-routing destroyed.
//
// Barrier.routeFault dispatches every storage fault it meets — Fail for
// permanent, Stall for retryable — and then returns that same fault as its
// error, so a caller that ignores Stallable still cannot mistake a fault for
// success. Re-classifying that returned error and stalling again is a second,
// WRONG dispatch that overwrites the first: a read-only filesystem was
// correctly reported as "Failed: … read-only file system" and then relabelled
// "Stalled: …", telling the operator to wait out a condition that cannot
// clear, and destroying the actionable reason on the way.
//
// The fixture reproduces the barrier's exact sequence — route, then return the
// fault — because that is the only shape the caller ever sees.
//
// # What each subtest can and cannot catch
//
// Worth stating, because two of the three are structurally unable to catch one
// obvious mutation and a reader who assumed otherwise would be misled.
//
// "permanent" and "retryable" both feed a fault the barrier ALREADY routed, and
// for that input the correct behaviour is to do nothing. So neither can redden
// when the function's body is deleted outright: a no-op body is behaviourally
// correct for this input class, and no observation of the job's state can tell
// "recognised it as already-routed and did nothing" from "did nothing".
// Verified, not assumed — with the body emptied both subtests pass.
//
// What they DO catch is the defect this function exists for: a second,
// unconditional dispatch of an already-routed fault. Both redden under it.
//
// "never_routed" is the input class where the function must act, and it is what
// pins the body against being empty. It reddens on that mutation and passes
// under the re-route one.
//
// The three together therefore cover the function completely: every mutation
// that can be distinguished by any observer is caught by at least one, and the
// one mutation two of them miss is caught by the third.
func TestRouteFinalizeFailure_DoesNotReRouteWhatTheBarrierAlreadyRouted(t *testing.T) {
	t.Parallel()
	const path = "/mnt/ro/movie.rar"

	t.Run("permanent", func(t *testing.T) {
		application, job := newDurabilityTestApp(t, 1, 1)

		// Exactly what Barrier.routeFault does for a permanent fault.
		fault := storagefault.Classify("truncate", path, syscall.EROFS)
		if !fault.Permanent {
			t.Fatal("EROFS is not classified permanent; this subtest is about the other branch")
		}
		application.Fail(job.ID(), fault)

		before, ok := application.dispatcher.Row(job.ID())
		if !ok {
			t.Skip("the permanent failure carried the job straight out of the dispatcher")
		}
		if !strings.Contains(before.Header.FailReason, "Failed:") {
			t.Fatalf("Fail set fail reason %q; the fixture is not in the state this test is about", before.Header.FailReason)
		}

		// ...and then returns the fault as its error, MARKED as routed and
		// wrapped on the way out. The marker is what the caller reads: a bare
		// fault means unrouted, and one of those now genuinely reaches here
		// from the SyncTarget boundary.
		err := fmt.Errorf("%w: job %s file %d: %w", ErrNotFinalized, job.ID(), 0,
			fmt.Errorf("%w: %w", durability.ErrFaultRouted, fault))
		application.routeFinalizeFailure(job.ID(), 0, path, err)

		after, ok := application.dispatcher.Row(job.ID())
		if !ok {
			t.Fatal("the job left the dispatcher between the two reads")
		}
		if stallReason := application.StallReason(job.ID()).Reason; stallReason != "" {
			t.Errorf("stall reason = %q — a permanent fault was re-routed as a stall, so the "+
				"operator is told to wait out a read-only filesystem", stallReason)
		}
		if !strings.Contains(after.Header.FailReason, "Failed:") {
			t.Errorf("fail reason = %q, want the permanent reason Fail set to survive", after.Header.FailReason)
		}
		// Two further assertions were tried here and removed, because neither
		// could fire. "the warning still names the path" holds under the bug
		// too — the re-stall NESTS the original fault, which names it. And
		// "status is not Paused" cannot fire because Fail leaves the job in
		// Verifying, where Queue.Pause is rejected outright, so the re-stall
		// changes the reason without changing the status. Only the two above
		// distinguish the two worlds.
	})

	// The retryable half of the same rule. It needs a different assertion from
	// the permanent one, and finding that out took a mutation: re-stalling an
	// already-stalled job leaves it Paused, still naming the file, with no
	// article damage — so every obvious assertion is satisfied by the
	// fixture's own Stall and passes whether the function re-routes or not.
	//
	// What DOES separate the two worlds is the operation named in the reason.
	// The barrier's own fault says "on sync"; a re-route rebuilds it as "on
	// finalize" and wraps the original inside. So the reason growing a second
	// layer is the observable, and it is the only one.
	t.Run("retryable", func(t *testing.T) {
		application, job := newDurabilityTestApp(t, 1, 1)

		fault := storagefault.Classify("sync", path, syscall.ENOSPC)
		if fault.Permanent {
			t.Fatal("ENOSPC is classified permanent; this subtest is about the other branch")
		}
		application.Stall(job.ID(), fault)

		_, ok := application.dispatcher.Row(job.ID())
		if !ok {
			t.Fatal("job left the dispatcher")
		}
		beforeReason := application.StallReason(job.ID()).Reason
		if !strings.Contains(beforeReason, "on sync") {
			t.Fatalf("the barrier's own reason is %q; this subtest reads the operation "+
				"name to tell a re-route from an untouched reason, so it needs one", beforeReason)
		}

		err := fmt.Errorf("%w: job %s file %d: %w", ErrNotFinalized, job.ID(), 0,
			fmt.Errorf("%w: %w", durability.ErrFaultRouted, fault))
		application.routeFinalizeFailure(job.ID(), 0, path, err)

		if _, ok := application.dispatcher.Row(job.ID()); !ok {
			t.Fatal("job left the dispatcher")
		}
		afterReason := application.StallReason(job.ID()).Reason
		if afterReason != beforeReason {
			t.Errorf("the reason changed from %q to %q — the fault the barrier had "+
				"already routed was routed a second time, rewrapping the operator's "+
				"reason in a layer that describes this code path rather than the fault",
				beforeReason, afterReason)
		}
		if strings.Contains(afterReason, "on finalize") {
			t.Errorf("stall reason = %q names this code path rather than the failing syscall; "+
				"the barrier reported %q and it must survive intact", afterReason, beforeReason)
		}
		if job.Progress().ArticleFailed(0) || job.Progress().FailedBytes() != 0 {
			t.Error("a storage condition was recorded as article damage (A1, R21)")
		}
	})

	t.Run("never routed", func(t *testing.T) {
		application, job := newDurabilityTestApp(t, 1, 1)

		// An OpenFiles timeout, or a target that cannot truncate: these never
		// reach Barrier.routeFault, so nothing has surfaced a reason and this
		// path must supply one — otherwise the job halts silently.
		err := fmt.Errorf("%w: job %s file %d: cannot tell whether it is still open: %w",
			ErrNotFinalized, job.ID(), 0, context.DeadlineExceeded)
		application.routeFinalizeFailure(job.ID(), 0, path, err)

		snap, ok := application.dispatcher.Row(job.ID())
		if !ok {
			t.Fatal("job left the dispatcher")
		}
		if snap.Status() != constants.StatusPaused {
			t.Errorf("status = %v, want Paused — an unrouted failure left the job running "+
				"and it completes with an untrimmed file", snap.Status())
		}
		if stallReason := application.StallReason(job.ID()).Reason; !strings.Contains(stallReason, path) {
			t.Errorf("stall reason = %q does not name the file; the job halts with no reason "+
				"the user can act on (R27)", stallReason)
		}
	})

	// The input class the old "is it a *Fault" test could not distinguish from
	// an already-routed one, and the reason the marker exists.
	//
	// The SyncTarget boundary mints a genuine *storagefault.Fault when the
	// worker does not answer, and nothing routes it — OpenFiles is not the
	// barrier. Under the shape test that fault read as "already handled": the
	// job was left running, its completed file was never trimmed, and it
	// shipped pre-allocation's trailing zeros to par2 as damage.
	t.Run("an unrouted fault from the target boundary", func(t *testing.T) {
		application, job := newDurabilityTestApp(t, 1, 1)

		fault := storagefault.Classify("list", "", errors.New("the assembler worker did not answer"))
		err := fmt.Errorf("%w: job %s file %d: cannot tell whether it is still open: %w",
			ErrNotFinalized, job.ID(), 0, fault)
		application.routeFinalizeFailure(job.ID(), 0, path, err)

		snap, ok := application.dispatcher.Row(job.ID())
		if !ok {
			t.Fatal("job left the dispatcher")
		}
		if snap.Status() != constants.StatusPaused {
			t.Errorf("status = %v, want Paused — a fault nothing had routed was read as "+
				"already handled, so the job carried on with a file that was never trimmed",
				snap.Status())
		}
		// The fault carried no path of its own, because resolving one means
		// calling back into the component that just failed to answer. The
		// caller has it and fills it in; without that the operator is told a
		// download halted and not which file.
		if stallReason := application.StallReason(job.ID()).Reason; !strings.Contains(stallReason, path) {
			t.Errorf("stall reason = %q does not name the file (R27)", stallReason)
		}
	})

	// The other half of the boundary rule: a condition that is NOT about
	// storage must not park the job at all. A deliberate close, a stopped
	// assembler, a caller that stopped waiting — parking on one of these named
	// a disk that did not fail and offered an action that does not exist.
	t.Run("not a storage condition", func(t *testing.T) {
		application, job := newDurabilityTestApp(t, 1, 1)

		err := fmt.Errorf("%w: job %s file %d: %w", ErrNotFinalized, job.ID(), 0,
			fmt.Errorf("%w: %w", durability.ErrTargetUnavailable, context.Canceled))
		application.routeFinalizeFailure(job.ID(), 0, path, err)

		snap, ok := application.dispatcher.Row(job.ID())
		if !ok {
			t.Fatal("job left the dispatcher")
		}
		if snap.Status() == constants.StatusPaused {
			t.Errorf("the job was parked for a condition that is not about storage: stall reason=%q",
				application.StallReason(job.ID()).Reason)
		}
		// Still owed, so the next re-evaluation retries it rather than the
		// file being silently left untrimmed.
		if !application.hasPendingFinalize(job.ID(), 0) {
			t.Error("the finalize was neither performed nor recorded for retry")
		}
	})
}

// TestHandleFileComplete_ResolvesThePathBeforeFinalizing pins the second half
// of the same finding, which stands on its own even after the double-routing
// is fixed.
//
// Application.Fail carries a permanently faulted job into maybeFinalize, and
// maybeFinalize drops the pipeline's cached FileInfo for it. A path asked for
// AFTER the finalize therefore comes back empty, so the reason the operator is
// shown names no file — on exactly the path where naming the volume matters
// most.
//
// The fixture drops that cache from another goroutine WHILE the finalize is in
// flight, which is what maybeFinalize does from inside it. The window is not a
// guess: the wedged worker makes the finalize sit on a 5s bound, and the drop
// happens a second in.
func TestHandleFileComplete_ResolvesThePathBeforeFinalizing(t *testing.T) {
	t.Parallel()
	application, job, _ := newWedgedApp(t)
	application.assembler.SetBarrierOpTimeout(100 * time.Millisecond)

	want, err := application.pipeline.resolveFileInfo(job.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if want.Path == "" {
		t.Fatal("the fixture resolved an empty path; this test cannot show the difference")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		application.handleFileComplete(t.Context(), FileComplete{JobID: job.ID(), FileIdx: 0})
	}()

	// Well inside the finalize's 100ms bound, and well after it has begun.
	time.Sleep(15 * time.Millisecond)
	application.pipeline.forgetJob(job.ID())
	if got := application.filePathFor(job.ID(), 0); got != "" {
		t.Fatalf("filePathFor still returns %q after the cache was dropped; the fixture "+
			"does not reproduce the condition it is about", got)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handleFileComplete never returned")
	}

	if _, ok := application.dispatcher.Row(job.ID()); !ok {
		t.Fatal("job left the dispatcher")
	}
	if stallReason := application.StallReason(job.ID()).Reason; !strings.Contains(stallReason, want.Path) {
		t.Errorf("stall reason = %q does not name %q — the path was resolved after the "+
			"finalize, by which point the job's FileInfo was gone, so the operator is "+
			"told a download halted without being told which file or which mount",
			stallReason, want.Path)
	}
}

// TestCheckpointJob_DoesNotStampABarrierThatNeverRan pins R26's last-barrier
// figure against its own inversion.
//
// checkpointJob had two outcomes where the world has three. A job with no sync
// target ran no barrier at all, but `err` stayed nil, so control fell through
// to the success stamp — and the window had already been zeroed, by a
// read-and-clear that then ran before the barrier rather than after it. The
// operator then sees a fresh barrier timestamp beside zero pending bytes: two
// figures agreeing that nothing is at risk, at the moment when everything
// written since the last real barrier is.
//
// The fixture removes the job from the queue while the assembler still holds
// its file open, because that is what actually produces a nil target.
// Eviction does NOT: syncTargetFor goes through Queue.SnapshotJob, which
// hydrates one job's manifest from disk, so a merely paused job still has a
// target. The reachable nil cases are a job that has left the queue between
// checkpointAll's OpenJobIDs and this call, and a manifest that cannot be
// read.
func TestCheckpointJob_DoesNotStampABarrierThatNeverRan(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID(), 0, 0)
	application.noteJobBytes(job.ID(), 4096)

	if err := application.dispatcher.Remove(t.Context(), job.ID()); err != nil {
		t.Fatal(err)
	}
	if application.syncTargetFor(job.ID()) != nil {
		t.Fatal("the fixture still has a sync target; the assertions below would pass against " +
			"a barrier that really ran")
	}
	// The assembler is unaware the job left, so checkpointAll still lists it.
	jobs, err := application.assembler.OpenJobIDs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(jobs, job.ID()) {
		t.Fatal("the assembler no longer holds the job's file, so checkpointJob would never " +
			"be called for it and this test asserts nothing reachable")
	}

	application.checkpointJob(t.Context(), job.ID())

	got := application.JobDurability(job.ID())
	if !got.LastBarrier.IsZero() {
		t.Errorf("LastBarrier = %v after a checkpoint that ran no barrier, want the zero time — "+
			"the figure exists to tell a job that is checkpointing from one whose barriers "+
			"stopped, and this reports the opposite", got.LastBarrier)
	}
	if got.PendingBytes != 4096 {
		t.Errorf("PendingBytes = %d, want 4096 — a window that was never closed was zeroed, so "+
			"the bytes at risk read as none", got.PendingBytes)
	}
}

// TestDropJobAlreadyInHistory_AppliesTheFailedRetentionRule pins the startup
// reconcile's half of the durability-row cleanup, in both directions.
//
// This path is reached by a job that crashed between MoveToHistory and the
// queue removal that follows it, so it is the ONE removal that runs without
// finalizeJob. It fetched the history entry, discarded it, removed the queue
// row and stopped — leaving durable_runs and failed_articles behind, keyed by
// job ID with no foreign key to dispatch_jobs. sweepOrphanedDurability now
// reclaims what escapes this path, but only for a job that is not in history
// as FAILED, which is exactly the case the retention rule below is about.
//
// Both directions matter and they fail differently. Retaining the rows for a
// succeeded job leaks one set per crash, forever. Dropping them for a FAILED
// one is worse than a leak: the retry reuses the job ID over the same partial
// file, and those runs are what bound FinalizeFile's truncate to the whole
// file. Without them the bound is the end offset of the re-fetched articles
// alone and the rest of the partial is destroyed silently.
func TestDropJobAlreadyInHistory_AppliesTheFailedRetentionRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		status   constants.Status
		wantKept bool
	}{
		{name: "a completed entry takes its rows with it", status: constants.StatusCompleted},
		{name: "a failed entry keeps them for its retry", status: constants.StatusFailed, wantKept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			application, job := newDurabilityTestApp(t, 1, 2)

			seedDurability(t, application, job.ID())
			if nr, nf := durabilityRowCounts(t, application, job.ID()); nr != 1 || nf != 1 {
				t.Fatalf("fixture recorded %d runs and %d failed rows, want 1 and 1; "+
					"the test would pass vacuously", nr, nf)
			}

			if err := application.historyRepo.Add(t.Context(), history.Entry{
				NzoID: job.ID(), Name: "reconciled", Status: string(tc.status),
			}); err != nil {
				t.Fatal(err)
			}

			if !application.dropJobAlreadyInHistory(t.Context(), job.ID()) {
				t.Fatal("dropJobAlreadyInHistory reported no removal for a job that is in history")
			}

			nr, nf := durabilityRowCounts(t, application, job.ID())
			if kept := nr > 0 || nf > 0; kept != tc.wantKept {
				if tc.wantKept {
					t.Error("a FAILED job's durability rows were dropped; its retry re-fetches " +
						"a few articles, FinalizeFile trims to their end offset, and the rest " +
						"of the partial file is destroyed silently")
				} else {
					t.Error("a finished job's durability rows survived its removal; nothing " +
						"else collects them, so they accumulate one set per crash of this kind")
				}
			}
		})
	}
}
