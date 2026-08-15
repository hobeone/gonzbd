package app

import (
	"context"
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
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// newDurabilityTestApp builds an Application over a real SQLite-backed queue
// and history, with one job of nFiles files each holding nArts articles, and
// the assembler started.
func newDurabilityTestApp(t *testing.T, nFiles, nArts int) (*Application, *queue.Job) {
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
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "durability-unit"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := application.queue.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return application, job
}

func fileFixtureName(f int) string { return string(rune('A'+f)) + ".bin" }
func fileFixtureArticleID(f, a int) string {
	return string(rune('A'+f)) + string(rune('0'+a)) + "@t"
}

// writeFixtureArticle hands one article of one file to the assembler, at the
// offset the fixture's uniform 100-byte articles imply.
func writeFixtureArticle(t *testing.T, application *Application, jobID string, fileIdx, globalArt int) {
	t.Helper()
	if err := application.pipeline.registerFile(jobID, fileIdx); err != nil {
		t.Fatalf("registerFile %d: %v", fileIdx, err)
	}
	if err := application.assembler.WriteArticle(t.Context(), assemblerWrite(jobID, globalArt)); err != nil {
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

// ---------- manifestArticleMap ----------

// TestManifestArticleMap_TranslatesGlobalIndicesToFileLocalOrdinals pins the
// mapping the barrier places durable bits by. A wrong ordinal marks the wrong
// article durable, which costs a silently short file on the next resume — so
// both the in-range translation and every out-of-range rejection matter.
func TestManifestArticleMap_TranslatesGlobalIndicesToFileLocalOrdinals(t *testing.T) {
	application, job := newDurabilityTestApp(t, 2, 3)
	m, err := application.queue.SnapshotJob(job.ID).Manifest()
	if err != nil {
		t.Fatal(err)
	}
	am := manifestArticleMap{m: m}

	if got := am.ArticleCount(0); got != 3 {
		t.Errorf("ArticleCount(0) = %d, want 3", got)
	}
	if got := am.ArticleCount(1); got != 3 {
		t.Errorf("ArticleCount(1) = %d, want 3", got)
	}
	// File 1 owns global articles 3..5, so its local ordinals are 0..2. An
	// implementation that returned the global index would pass a file-0 test
	// and place file 1's bits three positions too high.
	for global, wantOrd := range map[int32]int{3: 0, 4: 1, 5: 2} {
		got, ok := am.FileLocalOrdinal(1, global)
		if !ok || got != wantOrd {
			t.Errorf("FileLocalOrdinal(1, %d) = (%d, %v), want (%d, true)", global, got, ok, wantOrd)
		}
	}
	// An article belonging to another file must be rejected, not translated.
	if _, ok := am.FileLocalOrdinal(1, 0); ok {
		t.Error("FileLocalOrdinal(1, 0) reported an ordinal for an article file 0 owns")
	}
	if _, ok := am.FileLocalOrdinal(0, 5); ok {
		t.Error("FileLocalOrdinal(0, 5) reported an ordinal for an article file 1 owns")
	}
}

// TestManifestArticleMap_RejectsUnknownFilesWithoutPanicking pins the bound on
// fileIdx. queue.Manifest.FileRange indexes its slices directly and panics on
// an out-of-range file, so the guard is what keeps a bookkeeping defect from
// taking the process down.
func TestManifestArticleMap_RejectsUnknownFilesWithoutPanicking(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 2)
	m, err := application.queue.SnapshotJob(job.ID).Manifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, am := range []manifestArticleMap{{m: m}, {m: nil}} {
		for _, idx := range []int32{-1, 7} {
			if _, _, ok := am.rangeOf(idx); ok {
				t.Errorf("rangeOf(%d) reported a usable range for a file the manifest does not have", idx)
			}
			if got := am.ArticleCount(idx); got != 0 {
				t.Errorf("ArticleCount(%d) = %d, want 0", idx, got)
			}
			if _, ok := am.FileLocalOrdinal(idx, 0); ok {
				t.Errorf("FileLocalOrdinal(%d, 0) reported an ordinal", idx)
			}
		}
	}
	// The nil-manifest map must also reject a file index that WOULD be valid
	// for a real manifest, or the guard is only bounding the index.
	if got := (manifestArticleMap{m: nil}).ArticleCount(0); got != 0 {
		t.Errorf("ArticleCount(0) with no manifest = %d, want 0", got)
	}
}

// ---------- Stall / Fail ----------

// TestStall_PausesTheJobAndSurfacesTheReason pins A1's storage half and R27.
//
// A full disk resolves against storage: the job stops making requests and the
// user is told which file and which condition. No article may be touched —
// marking one failed would burn its retry budget and degrade the job's
// reported health from something the user fixes in seconds.
func TestStall_PausesTheJobAndSurfacesTheReason(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 2)

	application.Stall(job.ID, storagefault.Classify("sync", "/mnt/full/A.bin", syscall.ENOSPC))

	snap := application.queue.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("job left the queue")
	}
	if snap.Status != constants.StatusPaused {
		t.Errorf("status = %v after a retryable fault, want Paused — the job keeps "+
			"dispatching articles into a device that cannot take them", snap.Status)
	}
	if snap.Warning == "" {
		t.Fatal("no warning was surfaced; the job is paused for no visible reason (R27)")
	}
	for _, want := range []string{"/mnt/full/A.bin", "sync"} {
		if !strings.Contains(snap.Warning, want) {
			t.Errorf("warning %q does not mention %q; the user cannot act on it", snap.Warning, want)
		}
	}
	for i := range 2 {
		if snap.Progress().ArticleFailed(i) {
			t.Errorf("article %d was marked failed by a storage fault (A1, R21)", i)
		}
		if snap.Progress().ArticleDone(i) {
			t.Errorf("article %d was marked done by a storage fault", i)
		}
	}
	if snap.Progress().FailedBytes() != 0 {
		t.Errorf("failed bytes = %d after a storage fault, want 0 (R21)", snap.Progress().FailedBytes())
	}
}

// TestFail_SurfacesTheReasonAndStillFailsNoArticle pins R20 alongside A1. A
// read-only filesystem is permanent, so the job stops rather than waiting —
// but it says nothing about any article's availability, so the health figure
// must keep describing the download rather than the disk.
func TestFail_SurfacesTheReasonAndStillFailsNoArticle(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 2)

	application.Fail(job.ID, storagefault.Classify("write", "/mnt/ro/A.bin", syscall.EROFS))

	snap := application.queue.SnapshotJob(job.ID)
	if snap == nil {
		// maybeFinalize can carry the job straight out of the queue, which is
		// the intended terminal path; the article assertions below need a
		// snapshot, so the reason is checked against history instead.
		t.Skip("job left the queue for history; see TestFail on a resident job")
	}
	if !strings.Contains(snap.Warning, "/mnt/ro/A.bin") {
		t.Errorf("warning %q does not name the file (R27)", snap.Warning)
	}
	for i := range 2 {
		if snap.Progress().ArticleFailed(i) {
			t.Errorf("article %d was marked failed by a permanent storage fault (A1, R20)", i)
		}
	}
	if snap.Progress().FailedBytes() != 0 {
		t.Errorf("failed bytes = %d, want 0 (R21)", snap.Progress().FailedBytes())
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

// TestTakeJobBytes_MakesTheBoundMeasureTheWindowBetweenBarriers pins why the
// reset lives in the barrier rather than in the kick: without it the
// accumulator keeps its pre-barrier total and every subsequent article
// re-crosses the bound.
func TestTakeJobBytes_MakesTheBoundMeasureTheWindowBetweenBarriers(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)
	application.checkpointBytes = 100

	application.noteJobBytes("job-a", 150)
	<-application.barrierKick

	if got := application.takeJobBytes("job-a"); got != 150 {
		t.Errorf("takeJobBytes = %d, want 150 — the window's bytes are RETURNED as well "+
			"as cleared, and the caller puts them back if its barrier fails", got)
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

// TestSyncTargetFor_IsNilForAJobTheQueueCannotDescribe pins the guard that
// keeps a manifest-less job away from the barrier. The adapter answers
// "unknown" for every ordinal without a manifest, and a barrier over such a
// target refuses the job loudly — correct, but noise for the ordinary case of
// a job that has already left.
func TestSyncTargetFor_IsNilForAJobTheQueueCannotDescribe(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 2)

	if got := application.syncTargetFor("no-such-job"); got != nil {
		t.Error("built a sync target for a job that is not in the queue")
	}
	tgt := application.syncTargetFor(job.ID)
	if tgt == nil {
		t.Fatal("no sync target for a resident job; nothing would ever be checkpointed")
	}
	if got := tgt.ArticleCount(0); got != 2 {
		t.Errorf("target ArticleCount(0) = %d, want 2 — the manifest was not threaded through", got)
	}
}

// ---------- cadence ----------

// TestCheckpointAll_CoversEveryJobWithAnOpenFile pins the set the interval
// tick iterates. It comes from the assembler because "has an open file" is the
// assembler's fact and R8 bounds barrier cost by exactly that set; deriving it
// from job status would be a second copy of one fact, free to drift.
func TestCheckpointAll_CoversEveryJobWithAnOpenFile(t *testing.T) {
	application, job := newDurabilityTestApp(t, 2, 1)
	ctx := t.Context()

	// Only file 0 is written, so only it is open. A barrier must still reach
	// the job, and must not fail over the file that was never opened.
	writeFixtureArticle(t, application, job.ID, 0, 0)

	application.checkpointAll(ctx, shutdownCheckpointTimeout)
	if got := application.BarrierRuns(); got != 1 {
		t.Fatalf("%d barriers ran, want 1 for the one job holding an open file", got)
	}
	snap := application.queue.SnapshotJob(job.ID)
	if !snap.Progress().ArticleDone(0) {
		t.Error("the checkpoint did not ack the article it fsynced")
	}
	if snap.Progress().ArticleDone(1) {
		t.Error("an article that was never written was acked")
	}

	// With nothing open, the sweep must find no job rather than barrier every
	// job in the queue.
	if err := application.assembler.CloseJobHandles(ctx, job.ID); err != nil {
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
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID, 0, 0)
	application.barrier = nil

	application.checkpointJob(t.Context(), job.ID)
	if got := application.BarrierRuns(); got != 0 {
		t.Errorf("%d barriers ran with none wired", got)
	}
}

// TestRunCheckpoint_SavesTheQueueAfterEachCheckpoint pins the ordering the
// loop exists to impose. An ack marks articles Done in memory; until the queue
// is written a crash re-fetches them anyway, so saving before the barrier
// would persist a snapshot that is stale by construction.
func TestRunCheckpoint_SavesTheQueueAfterEachCheckpoint(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID, 0, 0)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		application.runCheckpoint(ctx, 10*time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	saved := false
	for time.Now().Before(deadline) && !saved {
		if application.BarrierRuns() > 0 && !application.queue.IsDirty() {
			saved = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if !saved {
		t.Fatalf("after %d barriers the queue was still dirty; the ack never reached disk",
			application.BarrierRuns())
	}
}

// TestSaveQueueIfDirty_SkipsACleanQueue pins the cheap half. The loop runs on
// every tick and every kick, and rewriting an unchanged queue on each would be
// a file write per checkpoint for a job that is idle.
func TestSaveQueueIfDirty_SkipsACleanQueue(t *testing.T) {
	application, _, adminDir := newLifecycleTestApp(t)
	statePath := filepath.Join(adminDir, "queue")

	application.saveQueueIfDirty()
	if application.queue.IsDirty() {
		// Nothing has made it dirty in this fixture; if that changes the
		// assertion below stops meaning anything.
		t.Fatal("the fixture's queue is dirty; this test cannot show the skip")
	}
	if _, err := os.Stat(statePath); err == nil {
		t.Error("a clean queue was written to disk")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat queue state: %v", err)
	}
}

// TestShutdownCheckpoint_IsInertWithoutABarrier pins that Shutdown's final
// pass costs nothing in the degraded mode, rather than dereferencing nil on
// the way out of every process that has no history database.
func TestShutdownCheckpoint_IsInertWithoutABarrier(t *testing.T) {
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
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID, 0, 0)

	application.shutdownCheckpoint()

	if got := application.BarrierRuns(); got != 1 {
		t.Fatalf("%d barriers ran on shutdown, want 1", got)
	}
	if application.queue.IsDirty() {
		t.Error("the shutdown checkpoint acked an article and left the queue unsaved; " +
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
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID, 0, 0)
	if err := application.assembler.Stop(); err != nil {
		t.Fatalf("assembler.Stop: %v", err)
	}

	if err := application.finalizeCompletedFile(t.Context(), job.ID, 0); err != nil {
		t.Fatalf("finalizing after an ordinary assembler stop = %v, want nil — every "+
			"completion drained during shutdown would stall its job", err)
	}

	snap := application.queue.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("job left the queue")
	}
	if snap.Status == constants.StatusPaused {
		t.Error("finalizing a file the assembler no longer holds paused the job; " +
			"every completion drained during shutdown would stall its job")
	}
	if snap.Warning != "" {
		t.Errorf("a stall reason %q was surfaced for an ordinary shutdown", snap.Warning)
	}
}

// TestFinalizeCompletedFile_TrimsAndReleasesTheHandle pins the second half of
// the assembler handoff: the barrier gets the file while it is still open, and
// the handle comes back afterwards.
func TestFinalizeCompletedFile_TrimsAndReleasesTheHandle(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	ctx := t.Context()
	writeFixtureArticle(t, application, job.ID, 0, 0)

	// The Class A fact the truncate bound is derived from. The pipeline
	// appends this in production; here the article was handed to the
	// assembler directly.
	if err := application.factLog.Append(ctx, job.ID, []durability.ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	info, err := application.pipeline.resolveFileInfo(job.ID, 0)
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

	if err := application.finalizeCompletedFile(ctx, job.ID, 0); err != nil {
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
	if !application.queue.SnapshotJob(job.ID).Progress().ArticleDone(0) {
		t.Error("finalizing the file acked nothing; its last articles stay Outstanding forever")
	}
	// The handle is back with the assembler, so post-processing's unlink does
	// not silly-rename on NFS.
	if got := application.syncTargetFor(job.ID).Files(); len(got) != 0 {
		t.Errorf("files still open after finalizing: %v", got)
	}
}

// ---------- Class A ----------

// TestAppendArticleFacts_RecordsTheDecodedRange pins the Class A write. The
// decoded offset and length come from the yEnc =ypart header inside the
// article body, so this map is itself downloaded data and nothing else can
// reconstruct it.
func TestAppendArticleFacts_RecordsTheDecodedRange(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	ctx := t.Context()

	application.pipeline.appendArticleFacts(ctx, job.ID, durability.ArticleFact{
		FileIdx: 0, ArtIdx: 0, Offset: 512, Length: 100, CRC32: 0xABCD,
	})

	got, err := application.factLog.ForFile(ctx, job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("recorded %d facts, want 1 — without Class A the completion truncate "+
			"has no bound and a resume can verify nothing", len(got))
	}
	if got[0].Offset != 512 || got[0].Length != 100 || got[0].CRC32 != 0xABCD {
		t.Errorf("fact = %+v, want offset 512, length 100, crc 0xABCD", got[0])
	}
}

// TestAppendArticleFacts_SurvivesTheCallersCancellation pins the shutdown
// case, which is not hypothetical: the daemon cancels the pipeline's context
// while an article is still being applied, and the write that follows this
// call can still land — the assembler takes the buffer, the shutdown
// checkpoint fsyncs it and acks it. On the caller's context the insert loses
// that race, so the committed extent claims an article durable that Class A
// cannot prove, and the next resume re-fetches bytes sitting on disk.
//
// Measured on the crash harness before the fix: one article in roughly one run
// in ten, always the last applied, with
// `err="durability: append fact job=... art=18: context canceled"` in the log.
//
// Dropping the cancellation cannot over-claim: a fact is true when it is
// minted and asserts nothing about presence, so the worst case is a fact for
// bytes that never reached disk, which a resume fails to verify and leaves
// Outstanding.
func TestAppendArticleFacts_SurvivesTheCallersCancellation(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if ctx.Err() == nil {
		t.Fatal("the context is not cancelled, so this asserts against the ordinary path")
	}

	application.pipeline.appendArticleFacts(ctx, job.ID, durability.ArticleFact{
		FileIdx: 0, ArtIdx: 0, Offset: 512, Length: 100, CRC32: 0xABCD,
	})

	got, err := application.factLog.ForFile(t.Context(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("recorded %d facts, want 1 — the article's Class A fact was lost to a "+
			"cancellation that did not stop its bytes reaching disk, so a resume cannot "+
			"prove them and re-fetches them", len(got))
	}
	if got[0].Offset != 512 || got[0].CRC32 != 0xABCD {
		t.Errorf("fact = %+v, want offset 512 and crc 0xABCD", got[0])
	}
}

// TestAppendArticleFacts_IsInertWithoutAFactLog pins the degraded mode: no
// history database means no Class A, and the write path must not dereference
// nil for every article it decodes.
func TestAppendArticleFacts_IsInertWithoutAFactLog(t *testing.T) {
	p := &pipeline{log: slog.New(slog.DiscardHandler)}
	p.appendArticleFacts(context.Background(), "job-a", durability.ArticleFact{ArtIdx: 1})
}

// ---------- job departure ----------

// TestDeleteJobDurability_RemovesBothClasses pins the only thing that cleans
// up. Both tables are keyed by job ID with no foreign key to the queue, so
// without this a database accumulates one set of rows per job ever downloaded.
func TestDeleteJobDurability_RemovesBothClasses(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	ctx := t.Context()

	if err := application.factLog.Append(ctx, job.ID, []durability.ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := application.factLog.Append(ctx, "other-job", []durability.ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
	}); err != nil {
		t.Fatal(err)
	}
	bm := durability.NewBitmap(1)
	bm.Set(0)
	if err := application.extents.Commit(ctx, job.ID, []durability.FileExtent{{FileIdx: 0, Durable: bm}}); err != nil {
		t.Fatal(err)
	}
	if err := application.extents.Commit(ctx, "other-job", []durability.FileExtent{{FileIdx: 0, Durable: bm}}); err != nil {
		t.Fatal(err)
	}

	application.deleteJobDurability(ctx, job.ID)

	facts, err := application.factLog.ForFile(ctx, job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 0 {
		t.Errorf("%d Class A facts survive the job's departure", len(facts))
	}
	exts, err := application.extents.Load(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(exts) != 0 {
		t.Errorf("%d Class B extents survive the job's departure", len(exts))
	}
	// Scoped to the departing job: deleting every job's rows would throw away
	// a live download's durable bits.
	otherFacts, err := application.factLog.ForFile(ctx, "other-job", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(otherFacts) != 1 {
		t.Errorf("another job's facts = %d, want 1 — the delete was not scoped", len(otherFacts))
	}
	otherExts, err := application.extents.Load(ctx, "other-job")
	if err != nil {
		t.Fatal(err)
	}
	if len(otherExts) != 1 {
		t.Errorf("another job's extents = %d, want 1 — the delete was not scoped", len(otherExts))
	}
}

// TestDeleteJobDurability_IsInertWithoutStores pins the degraded mode.
func TestDeleteJobDurability_IsInertWithoutStores(t *testing.T) {
	application := &Application{log: slog.New(slog.DiscardHandler)}
	application.deleteJobDurability(context.Background(), "job-a")
}

// ---------- settings ----------

// TestCheckpointSettings_SubstitutesDefaultsForUnsetBounds pins that neither
// bound can be switched off. A barrier is the only thing that acks a
// downloaded article while the job is running, so with checkpoints off a job
// holds every article Outstanding until it stops, and a restart must re-read
// every partial file instead of adopting the Class B cache. See
// checkpointSettings for why that is not the same as re-fetching everything.
func TestCheckpointSettings_SubstitutesDefaultsForUnsetBounds(t *testing.T) {
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

// failingFactLog is a FactLog whose every operation fails, for the paths that
// must degrade to a re-fetch rather than to a wrong answer.
type failingFactLog struct{ err error }

func (f failingFactLog) Append(context.Context, string, []durability.ArticleFact) error { return f.err }
func (f failingFactLog) ForFile(context.Context, string, int32) ([]durability.ArticleFact, error) {
	return nil, f.err
}
func (f failingFactLog) DeleteJob(context.Context, string) error { return f.err }

// failingExtentStore is an ExtentStore whose every operation fails.
type failingExtentStore struct{ err error }

func (f failingExtentStore) Commit(context.Context, string, []durability.FileExtent) error {
	return f.err
}
func (f failingExtentStore) Load(context.Context, string) ([]durability.FileExtent, error) {
	return nil, f.err
}
func (f failingExtentStore) LoadFile(context.Context, string, int32) (durability.FileExtent, bool, error) {
	return durability.FileExtent{}, false, f.err
}
func (f failingExtentStore) DeleteJob(context.Context, string) error { return f.err }

// recordingExtentStore notes whether DeleteJob was reached. The alternative —
// comparing application.extents to a copy of itself taken two lines earlier —
// is a tautology, which is what this replaces.
type recordingExtentStore struct{ deleted []string }

func (r *recordingExtentStore) Commit(context.Context, string, []durability.FileExtent) error {
	return nil
}

func (r *recordingExtentStore) Load(context.Context, string) ([]durability.FileExtent, error) {
	return nil, nil
}

func (r *recordingExtentStore) LoadFile(context.Context, string, int32) (durability.FileExtent, bool, error) {
	return durability.FileExtent{}, false, nil
}

func (r *recordingExtentStore) DeleteJob(_ context.Context, jobID string) error {
	r.deleted = append(r.deleted, jobID)
	return nil
}

// TestAppendArticleFacts_SurvivesAFailedWrite pins R2: losing a Class A record
// must not abort the write path.
//
// The article is still handed to the assembler and still written. What it
// costs is NOT "a re-fetch and nothing else" — see pipeline.appendArticleFacts
// and Barrier.FinalizeFile's unrecorded guard, since a fact the log never
// received also hides the article from the walks that bound the truncate.
// This test pins only the half stated above: the write proceeds.
func TestAppendArticleFacts_SurvivesAFailedWrite(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	application.pipeline.factLog = failingFactLog{err: errors.New("disk on fire")}

	application.pipeline.appendArticleFacts(t.Context(), job.ID, durability.ArticleFact{
		FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100,
	})

	// The real log is untouched: the failure did not half-write anything.
	got, err := application.factLog.ForFile(t.Context(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("recorded %d facts through a failing log", len(got))
	}
}

// TestStall_ReportsRatherThanPanicsOnAJobThatHasLeft pins the case a storage
// fault most easily hits: the barrier finds the fault, and by the time the
// stall reaches the queue the job has been removed. Both queue writes fail and
// neither may take the process down or abort the other.
func TestStall_ReportsRatherThanPanicsOnAJobThatHasLeft(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)

	application.Stall("no-such-job", storagefault.Classify("sync", "/x", syscall.ENOSPC))
	application.Fail("no-such-job", storagefault.Classify("write", "/x", syscall.EROFS))
}

// TestSyncTargetFor_IsNilWhenTheManifestCannotBeRead pins the non-resident
// case. A job whose manifest has been evicted has nothing open to checkpoint,
// and building a target with no ArticleMap would make the barrier refuse it
// loudly for an entirely ordinary condition.
func TestSyncTargetFor_IsNilWhenTheManifestCannotBeRead(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)

	// Remove the manifest file the queue would hydrate from, then evict the
	// job so the next Manifest() has to read it.
	adminDir := application.config.GetGeneral().AdminDir
	manifestPath := filepath.Join(adminDir, "queue", "manifests", job.ID+".json.gz")
	if err := os.Remove(manifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove manifest: %v", err)
	}
	if err := application.queue.Pause(job.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	snap := application.queue.SnapshotJob(job.ID)
	if snap == nil {
		t.Skip("the job left the queue entirely; the manifest-read branch is unreachable here")
	}
	if _, err := snap.Manifest(); err == nil {
		t.Skip("the manifest is still resident; this fixture cannot reach the read failure")
	}
	if got := application.syncTargetFor(job.ID); got != nil {
		t.Error("built a sync target for a job whose manifest cannot be read; the " +
			"barrier would refuse every file of an ordinarily non-resident job")
	}
}

// TestDeleteJobDurability_ReportsAFailedDelete pins that a failing cleanup is
// logged rather than swallowed, and that a failure in one store does not stop
// the other from being tried — leaving half a job's rows behind is worse than
// leaving all of them, because the surviving half describes a job that no
// longer exists.
func TestDeleteJobDurability_ReportsAFailedDelete(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)
	boom := errors.New("database is locked")
	application.factLog = failingFactLog{err: boom}

	rec := &recordingExtentStore{}
	application.extents = rec
	application.deleteJobDurability(t.Context(), "job-a")

	// The load-bearing assertion: the extent store still ran even though the
	// fact log failed FIRST. An early return on the fact log's error would
	// leave half a job's rows behind, which is worse than leaving all of them
	// — the surviving half describes a job that no longer exists.
	//
	// This replaces `if application.extents != realExtents`, which compared
	// the field to a copy of itself taken two lines earlier with nothing in
	// between that could change it. It could not fail.
	if !slices.Equal(rec.deleted, []string{"job-a"}) {
		t.Fatalf("extent store DeleteJob calls = %v, want [job-a]; a failing fact log "+
			"must not stop the extent store from being tried", rec.deleted)
	}

	// The mirror case: the extent store failing must not panic or abort either.
	application.factLog = nil
	application.extents = failingExtentStore{err: boom}
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
	application, job := newDurabilityTestApp(t, 1, 1)
	rec := &recordingEmitter{}
	application.emitter = rec

	application.emit(Event{Type: "queue_updated", NzoID: "direct"})
	if len(rec.events) != 1 || rec.events[0].Type != "queue_updated" || rec.events[0].NzoID != "direct" {
		t.Fatalf("emit delivered %+v, want one queue_updated for \"direct\"", rec.events)
	}

	application.Stall(job.ID, storagefault.Classify("sync", "/mnt/full/A.bin", syscall.ENOSPC))
	var sawStallUpdate bool
	for _, e := range rec.events[1:] {
		if e.Type == "queue_updated" && e.NzoID == job.ID {
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
func newWedgedApp(t *testing.T) (*Application, *queue.Job, func()) {
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
	application.assembler = assembler.New(assembler.Options{FileInfo: wedge.resolve},
		slog.New(slog.DiscardHandler))
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
	writeFixtureArticle(t, application, job.ID, 0, 0)
	// File 1's open parks the worker, so no control message can be answered.
	if err := application.pipeline.registerFile(job.ID, 1); err != nil {
		t.Fatalf("registerFile 1: %v", err)
	}
	// FileIdx 1 explicitly: assemblerWrite always targets file 0, and sending
	// this to file 0 would never reach the wedge.
	if err := application.assembler.WriteArticle(ctx, assembler.WriteRequest{
		JobID: job.ID, FileIdx: 1, ArtIdx: 1, MessageID: "b@t",
		Offset: 0, Data: make([]byte, 100),
	}); err != nil {
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
	application, job, _ := newWedgedApp(t)

	err := application.finalizeCompletedFile(t.Context(), job.ID, 0)
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
	application, job, _ := newWedgedApp(t)

	application.handleFileComplete(t.Context(), FileComplete{JobID: job.ID, FileIdx: 0})

	snap := application.queue.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("job left the queue")
	}
	if snap.Progress().FileComplete(0) {
		t.Error("the file was marked complete although it was never finalized; " +
			"DirectUnpack, job finalization and post-processing all act on that bit, " +
			"and a file still carrying pre-allocation's zeros reads as par2 damage")
	}
	if snap.Status != constants.StatusPaused {
		t.Errorf("status = %v, want Paused — the job carries on and completes with an "+
			"untrimmed file", snap.Status)
	}
	if snap.Warning == "" {
		t.Error("no reason was surfaced; the job halts and the user is told nothing (R27)")
	}
	if snap.Progress().ArticleFailed(0) || snap.Progress().FailedBytes() != 0 {
		t.Error("a storage condition was recorded as article damage (A1, R21)")
	}
}

// TestFilePathFor_NamesTheFileOrSaysNothing pins R27's input on the stall
// path. A stall reason that names no file tells a user their download halted
// without telling them which volume or which mount to look at — and both
// polarities matter, because the "" case is what a caller must not branch on.
func TestFilePathFor_NamesTheFileOrSaysNothing(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID, 0, 0)

	want, err := application.pipeline.resolveFileInfo(job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want.Path == "" {
		t.Fatal("the fixture resolved an empty path; the assertion below cannot distinguish it from the failure case")
	}
	if got := application.filePathFor(job.ID, 0); got != want.Path {
		t.Errorf("filePathFor = %q, want %q", got, want.Path)
	}
	// A file the pipeline never registered must not be answered with ANOTHER
	// file's path. This is the version of the negative case that can actually
	// fail: an earlier one asserted only `== ""`, which no implementation
	// reachable from here can violate — resolveFileInfo returns a zero
	// FileInfo on miss, so ignoring its error yields "" either way. An
	// implementation that ignored fileIdx and returned the job's first known
	// path would have passed it, and fails this.
	if got := application.filePathFor(job.ID, 7); got == want.Path {
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
	const path = "/mnt/ro/movie.rar"

	t.Run("permanent", func(t *testing.T) {
		application, job := newDurabilityTestApp(t, 1, 1)

		// Exactly what Barrier.routeFault does for a permanent fault.
		fault := storagefault.Classify("truncate", path, syscall.EROFS)
		if !fault.Permanent {
			t.Fatal("EROFS is not classified permanent; this subtest is about the other branch")
		}
		application.Fail(job.ID, fault)

		before := application.queue.SnapshotJob(job.ID)
		if before == nil {
			t.Skip("the permanent failure carried the job straight out of the queue")
		}
		if !strings.Contains(before.Warning, "Failed:") {
			t.Fatalf("Fail set warning %q; the fixture is not in the state this test is about", before.Warning)
		}

		// ...and then returns the fault as its error, MARKED as routed and
		// wrapped on the way out. The marker is what the caller reads: a bare
		// fault means unrouted, and one of those now genuinely reaches here
		// from the SyncTarget boundary.
		err := fmt.Errorf("%w: job %s file %d: %w", ErrNotFinalized, job.ID, 0,
			fmt.Errorf("%w: %w", durability.ErrFaultRouted, fault))
		application.routeFinalizeFailure(job.ID, 0, path, err)

		after := application.queue.SnapshotJob(job.ID)
		if after == nil {
			t.Fatal("the job left the queue between the two reads")
		}
		if strings.Contains(after.Warning, "Stalled:") {
			t.Errorf("warning = %q — a permanent fault was re-routed as a stall, so the "+
				"operator is told to wait out a read-only filesystem", after.Warning)
		}
		if !strings.Contains(after.Warning, "Failed:") {
			t.Errorf("warning = %q, want the permanent reason Fail set to survive", after.Warning)
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
		application.Stall(job.ID, fault)

		before := application.queue.SnapshotJob(job.ID)
		if before == nil {
			t.Fatal("job left the queue")
		}
		if !strings.Contains(before.Warning, "on sync") {
			t.Fatalf("the barrier's own reason is %q; this subtest reads the operation "+
				"name to tell a re-route from an untouched reason, so it needs one", before.Warning)
		}

		err := fmt.Errorf("%w: job %s file %d: %w", ErrNotFinalized, job.ID, 0,
			fmt.Errorf("%w: %w", durability.ErrFaultRouted, fault))
		application.routeFinalizeFailure(job.ID, 0, path, err)

		after := application.queue.SnapshotJob(job.ID)
		if after == nil {
			t.Fatal("job left the queue")
		}
		if after.Warning != before.Warning {
			t.Errorf("the reason changed from %q to %q — the fault the barrier had "+
				"already routed was routed a second time, rewrapping the operator's "+
				"reason in a layer that describes this code path rather than the fault",
				before.Warning, after.Warning)
		}
		if strings.Contains(after.Warning, "on finalize") {
			t.Errorf("warning = %q names this code path rather than the failing syscall; "+
				"the barrier reported %q and it must survive intact", after.Warning, before.Warning)
		}
		if after.Progress().ArticleFailed(0) || after.Progress().FailedBytes() != 0 {
			t.Error("a storage condition was recorded as article damage (A1, R21)")
		}
	})

	t.Run("never routed", func(t *testing.T) {
		application, job := newDurabilityTestApp(t, 1, 1)

		// An OpenFiles timeout, or a target that cannot truncate: these never
		// reach Barrier.routeFault, so nothing has surfaced a reason and this
		// path must supply one — otherwise the job halts silently.
		err := fmt.Errorf("%w: job %s file %d: cannot tell whether it is still open: %w",
			ErrNotFinalized, job.ID, 0, context.DeadlineExceeded)
		application.routeFinalizeFailure(job.ID, 0, path, err)

		snap := application.queue.SnapshotJob(job.ID)
		if snap == nil {
			t.Fatal("job left the queue")
		}
		if snap.Status != constants.StatusPaused {
			t.Errorf("status = %v, want Paused — an unrouted failure left the job running "+
				"and it completes with an untrimmed file", snap.Status)
		}
		if !strings.Contains(snap.Warning, path) {
			t.Errorf("warning = %q does not name the file; the job halts with no reason "+
				"the user can act on (R27)", snap.Warning)
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
			ErrNotFinalized, job.ID, 0, fault)
		application.routeFinalizeFailure(job.ID, 0, path, err)

		snap := application.queue.SnapshotJob(job.ID)
		if snap == nil {
			t.Fatal("job left the queue")
		}
		if snap.Status != constants.StatusPaused {
			t.Errorf("status = %v, want Paused — a fault nothing had routed was read as "+
				"already handled, so the job carried on with a file that was never trimmed",
				snap.Status)
		}
		// The fault carried no path of its own, because resolving one means
		// calling back into the component that just failed to answer. The
		// caller has it and fills it in; without that the operator is told a
		// download halted and not which file.
		if !strings.Contains(snap.Warning, path) {
			t.Errorf("warning = %q does not name the file (R27)", snap.Warning)
		}
	})

	// The other half of the boundary rule: a condition that is NOT about
	// storage must not park the job at all. A deliberate close, a stopped
	// assembler, a caller that stopped waiting — parking on one of these named
	// a disk that did not fail and offered an action that does not exist.
	t.Run("not a storage condition", func(t *testing.T) {
		application, job := newDurabilityTestApp(t, 1, 1)

		err := fmt.Errorf("%w: job %s file %d: %w", ErrNotFinalized, job.ID, 0,
			fmt.Errorf("%w: %w", durability.ErrTargetUnavailable, context.Canceled))
		application.routeFinalizeFailure(job.ID, 0, path, err)

		snap := application.queue.SnapshotJob(job.ID)
		if snap == nil {
			t.Fatal("job left the queue")
		}
		if snap.Status == constants.StatusPaused {
			t.Errorf("the job was parked for a condition that is not about storage: warning=%q",
				snap.Warning)
		}
		// Still owed, so the next re-evaluation retries it rather than the
		// file being silently left untrimmed.
		if !application.hasPendingFinalize(job.ID, 0) {
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
	application, job, _ := newWedgedApp(t)

	want, err := application.pipeline.resolveFileInfo(job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want.Path == "" {
		t.Fatal("the fixture resolved an empty path; this test cannot show the difference")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		application.handleFileComplete(t.Context(), FileComplete{JobID: job.ID, FileIdx: 0})
	}()

	// Well inside the finalize's 5s bound, and well after it has begun.
	time.Sleep(time.Second)
	application.pipeline.forgetJob(job.ID)
	if got := application.filePathFor(job.ID, 0); got != "" {
		t.Fatalf("filePathFor still returns %q after the cache was dropped; the fixture "+
			"does not reproduce the condition it is about", got)
	}

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("handleFileComplete never returned")
	}

	snap := application.queue.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("job left the queue")
	}
	if !strings.Contains(snap.Warning, want.Path) {
		t.Errorf("warning = %q does not name %q — the path was resolved after the "+
			"finalize, by which point the job's FileInfo was gone, so the operator is "+
			"told a download halted without being told which file or which mount",
			snap.Warning, want.Path)
	}
}

// TestCheckpointJob_DoesNotStampABarrierThatNeverRan pins R26's last-barrier
// figure against its own inversion.
//
// checkpointJob had two outcomes where the world has three. A job with no sync
// target ran no barrier at all, but `err` stayed nil, so control fell through
// to the success stamp — and takeJobBytes had already zeroed the window. The
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
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID, 0, 0)
	application.noteJobBytes(job.ID, 4096)

	if err := application.queue.Remove(job.ID); err != nil {
		t.Fatal(err)
	}
	if application.syncTargetFor(job.ID) != nil {
		t.Fatal("the fixture still has a sync target; the assertions below would pass against " +
			"a barrier that really ran")
	}
	// The assembler is unaware the job left, so checkpointAll still lists it.
	jobs, err := application.assembler.OpenJobIDs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(jobs, job.ID) {
		t.Fatal("the assembler no longer holds the job's file, so checkpointJob would never " +
			"be called for it and this test asserts nothing reachable")
	}

	application.checkpointJob(t.Context(), job.ID)

	got := application.JobDurability(job.ID)
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
// row and stopped — leaving article_facts and file_extents behind, keyed by
// job ID with no foreign key to jobs and with no later path to collect them.
//
// Both directions matter and they fail differently. Retaining the rows for a
// succeeded job leaks one set per crash, forever. Dropping them for a FAILED
// one is worse than a leak: the retry reuses the job ID over the same partial
// file, and those facts are what bound FinalizeFile's truncate to the whole
// file. Without them durableExtent returns the end offset of the re-fetched
// articles alone and the rest of the partial is destroyed silently, because
// every article the fact log then knows about IS durable, so neither #342
// guard fires.
func TestDropJobAlreadyInHistory_AppliesTheFailedRetentionRule(t *testing.T) {
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

			if err := application.factLog.Append(t.Context(), job.ID, []durability.ArticleFact{
				{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1},
			}); err != nil {
				t.Fatal(err)
			}
			got, err := application.factLog.ForFile(t.Context(), job.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("fixture recorded %d facts, want 1; the test would pass vacuously", len(got))
			}

			if err := application.historyRepo.Add(t.Context(), history.Entry{
				NzoID: job.ID, Name: "reconciled", Status: string(tc.status),
			}); err != nil {
				t.Fatal(err)
			}

			if !application.dropJobAlreadyInHistory(t.Context(), job.ID) {
				t.Fatal("dropJobAlreadyInHistory reported no removal for a job that is in history")
			}

			after, err := application.factLog.ForFile(t.Context(), job.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if kept := len(after) > 0; kept != tc.wantKept {
				if tc.wantKept {
					t.Error("a FAILED job's Class A facts were dropped; its retry re-fetches a " +
						"few articles, FinalizeFile trims to their end offset, and the rest of " +
						"the partial file is destroyed with no guard firing")
				} else {
					t.Error("a finished job's Class A facts survived its removal; nothing else " +
						"collects them, so they accumulate one set per crash of this kind")
				}
			}
		})
	}
}
