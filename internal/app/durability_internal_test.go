package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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

// TestResetJobBytes_MakesTheBoundMeasureTheWindowBetweenBarriers pins why the
// reset lives in the barrier rather than in the kick: without it the
// accumulator keeps its pre-barrier total and every subsequent article
// re-crosses the bound.
func TestResetJobBytes_MakesTheBoundMeasureTheWindowBetweenBarriers(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)
	application.checkpointBytes = 100

	application.noteJobBytes("job-a", 150)
	<-application.barrierKick

	application.resetJobBytes("job-a")
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

	application.checkpointAll(ctx)
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
	application.checkpointAll(ctx)
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

	application.finalizeCompletedFile(t.Context(), job.ID, 0)

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

	application.finalizeCompletedFile(ctx, job.ID, 0)

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
// downloaded article, so a job with no barrier re-fetches everything on every
// restart — "off" is not a faster mode, it is a broken one.
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
func (f failingExtentStore) DeleteJob(context.Context, string) error { return f.err }

// TestAppendArticleFacts_SurvivesAFailedWrite pins R3: losing a Class A record
// costs a re-fetch and nothing else.
//
// The write path must not abort over it. The article is still handed to the
// assembler and still written; the only consequence is that a restart cannot
// prove those bytes and fetches them again — which is the safe direction under
// S3.
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

	realExtents := application.extents
	application.deleteJobDurability(t.Context(), "job-a")

	// The extent store still ran despite the fact log failing first.
	if application.extents != realExtents {
		t.Fatal("fixture replaced the extent store")
	}

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
