package assembler

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
)

// fsyncReleaseRecorder records both callbacks that could return a file's
// articles to Outstanding, so a test can require that a failed fsync releases
// them through OnFileUntrusted, once, and through nothing else.
type fsyncReleaseRecorder struct {
	untrusted []fileKey
	unwritten []int32
}

func (r *fsyncReleaseRecorder) install(a *Assembler) {
	a.opts.OnFileUntrusted = func(jobID string, fileIdx int) {
		r.untrusted = append(r.untrusted, fileKey{jobID: jobID, fileIdx: fileIdx})
	}
	a.opts.OnArticlesUnwritten = func(_ string, _ int, arts []int32) {
		r.unwritten = append(r.unwritten, arts...)
	}
}

// check requires that want, and only want, was untrusted, once, and that no
// article was released one by one alongside it.
func (r *fsyncReleaseRecorder) check(t *testing.T, want fileKey) {
	t.Helper()
	if len(r.untrusted) != 1 || r.untrusted[0] != want {
		t.Errorf("untrusted %v, want exactly [%v] — the file whose fsync failed, once", r.untrusted, want)
	}
	if len(r.unwritten) != 0 {
		t.Errorf("OnArticlesUnwritten = %v after a failed fsync, want none: the "+
			"untrust of the file is what returns its written articles to Outstanding", r.unwritten)
	}
}

// writeHelperArticle writes article artIdx through the production success
// path, so the file holds a written article for its fsync to fail under.
func writeHelperArticle(t *testing.T, a *Assembler, f *openFile, artIdx int32) {
	t.Helper()
	a.handleSuccessArticle(f, WriteRequest{
		JobID: f.w.key.jobID, FileIdx: f.w.key.fileIdx, ArtIdx: artIdx, MessageID: "m",
		Offset: int64(artIdx) * 4, Data: []byte("abcd"),
	})
	if f.w.parts() != 1 {
		t.Fatalf("fixture: parts = %d after writing article %d, want 1", f.w.parts(), artIdx)
	}
}

// TestCloseJobHandles_UntrustsAFileWhoseCloseTimeSyncFailed pins that the
// close-handles arm hands a file whose fsync failed to OnFileUntrusted, once,
// and that this is the whole of the release. That fsync consumed the error, so
// the verification fsync at the next start would return 0 and the file's rows
// would be trusted.
func TestCloseJobHandles_UntrustsAFileWhoseCloseTimeSyncFailed(t *testing.T) {
	a := newHelperAssembler()
	var r fsyncReleaseRecorder
	r.install(a)

	dir := t.TempDir()
	bad := newHelperFile(t, dir, "bad.dat", 0)
	writeHelperArticle(t, a, bad, 0)
	bad.w.syncFile = func() error { return syscall.EIO }
	good := newHelperFile(t, dir, "good.dat", 0)
	good.w.key.fileIdx = 1
	writeHelperArticle(t, a, good, 0)
	open := map[fileKey]*openFile{
		{jobID: "job", fileIdx: 0}: bad,
		{jobID: "job", fileIdx: 1}: good,
	}

	ack := make(chan error, 1)
	a.dispatchRequest(
		WriteRequest{FileIdx: fileIdxCloseHandles, MessageID: "job", ackCh: ack},
		open, map[fileKey]struct{}{})
	if err := <-ack; !errors.Is(err, syscall.EIO) {
		t.Errorf("close-handles ack = %v, want the fsync's EIO", err)
	}

	r.check(t, fileKey{jobID: "job", fileIdx: 0})
}

// TestSyncAndCloseAll_UntrustsAFileWhoseSyncFailed is the worker-exit half:
// a clean shutdown's fsync that fails untrusts its file, synchronously, before
// the recorder's final flush, and releases nothing else.
func TestSyncAndCloseAll_UntrustsAFileWhoseSyncFailed(t *testing.T) {
	a := newHelperAssembler()
	var r fsyncReleaseRecorder
	r.install(a)

	dir := t.TempDir()
	bad := newHelperFile(t, dir, "bad.dat", 0)
	bad.w.key.fileIdx = 3
	writeHelperArticle(t, a, bad, 0)
	bad.w.syncFile = func() error { return syscall.EIO }
	good := newHelperFile(t, dir, "good.dat", 0)
	good.w.key.fileIdx = 4
	writeHelperArticle(t, a, good, 0)
	a.syncAndCloseAll(map[fileKey]*openFile{
		{jobID: "job", fileIdx: 3}: bad,
		{jobID: "job", fileIdx: 4}: good,
	})

	r.check(t, fileKey{jobID: "job", fileIdx: 3})
}

// TestQuiesce_ReturnsAfterEverythingQueuedAheadOfIt pins Quiesce's one
// promise: every article enqueued before it has been written and its
// OnArticleWritten has run by the time it returns.
func TestQuiesce_ReturnsAfterEverythingQueuedAheadOfIt(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	const n = 64
	registerFile(t, dir, files, "job1", 0, n+1) // never completes
	var written atomic.Int32
	opts := makeOpts(dir, files)
	opts.OnArticleWritten = func(string, int, int32, int64, int64, uint32) { written.Add(1) }
	a := startAssembler(t, opts)

	for i := range n {
		if err := writeArticle(t.Context(), a, WriteRequest{
			JobID: "job1", FileIdx: 0, ArtIdx: int32(i), MessageID: "m",
			Offset: int64(i) * 8, Data: make([]byte, 8),
		}); err != nil {
			t.Fatalf("writeArticle %d: %v", i, err)
		}
	}
	if err := a.Quiesce(t.Context()); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
	if got := written.Load(); got != n {
		t.Errorf("Quiesce returned with %d of %d articles written — a reload "+
			"clearing Emitted now would clear bits for articles about to be written", got, n)
	}
}

// TestOptionsSyncFile_ReachesTheCompletionFsync pins the seam: Options.SyncFile
// replaces the fsync a completing file's finish runs, so an injected device
// error untrusts the file instead of completing it.
func TestOptionsSyncFile_ReachesTheCompletionFsync(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 1)
	opts := makeOpts(dir, files)
	opts.SyncFile = func(*os.File) error { return syscall.EIO }
	opts.OnArticlesUnwritten = func(string, int, []int32) {}
	var untrusted, completed atomic.Int32
	opts.OnFileUntrusted = func(string, int) { untrusted.Add(1) }
	opts.OnFileComplete = func(string, int) { completed.Add(1) }
	a := startAssembler(t, opts)

	if err := writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, MessageID: "m", Offset: 0, Data: make([]byte, 8),
	}); err != nil {
		t.Fatalf("writeArticle: %v", err)
	}
	if err := a.Quiesce(t.Context()); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
	if untrusted.Load() != 1 || completed.Load() != 0 {
		t.Errorf("untrusted=%d completed=%d, want 1 and 0: the injected fsync error did not reach finish",
			untrusted.Load(), completed.Load())
	}
}

// TestQuiesce_RefusesWhenItCannotWait pins the guards ahead of the wait: a
// cancelled context, an assembler never started, and one already stopped each
// return at once with their own error.
func TestQuiesce_RefusesWhenItCannotWait(t *testing.T) {
	t.Parallel()
	opts := makeOpts(t.TempDir(), map[string]FileInfo{})

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := New(opts, nil).Quiesce(cancelled); !errors.Is(err, context.Canceled) {
		t.Errorf("Quiesce(cancelled) = %v, want context.Canceled", err)
	}
	if err := New(opts, nil).Quiesce(t.Context()); !errors.Is(err, ErrNotStarted) {
		t.Errorf("Quiesce before Start = %v, want ErrNotStarted", err)
	}
	a := New(opts, nil)
	if err := a.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := a.Quiesce(t.Context()); !errors.Is(err, ErrStopped) {
		t.Errorf("Quiesce after Stop = %v, want ErrStopped", err)
	}
}
