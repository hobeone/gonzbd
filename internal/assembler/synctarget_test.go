package assembler

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// newSyncOpFixture builds a worker-side open-file map with one real file.
// The adapter must satisfy the barrier's interface; asserted at package scope
// so a signature drift is a compile error rather than a runtime surprise.
var _ durability.SyncTarget = (*jobSyncTarget)(nil)

func newSyncOpFixture(t *testing.T) (*Assembler, map[fileKey]*openFile, fileKey) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sync.dat")
	fh, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = fh.Close() })
	key := fileKey{jobID: "job1", fileIdx: 0}
	f := &openFile{
		w:    newFileWriter(fh, path, key, newWriteCache(0)),
		info: FileInfo{Path: path},
		key:  key,
	}
	return &Assembler{log: slog.Default()}, map[fileKey]*openFile{key: f}, key
}

func runSyncOp(t *testing.T, a *Assembler, open map[fileKey]*openFile, op syncOp) syncReply {
	t.Helper()
	op.reply = make(chan syncReply, 1)
	a.handleSyncOp(&op, open)
	select {
	case r := <-op.reply:
		return r
	default:
		t.Fatal("handleSyncOp did not reply; the barrier would block forever")
		return syncReply{}
	}
}

// TestHandleSyncOp_FilesReportsOnlyTheRequestedJob pins the per-job scoping
// that is the adapter's whole reason to exist. SyncTarget.Files is per-job
// while the worker's map spans every job, so a leak here would make a barrier
// for one job fsync and ack another's files.
func TestHandleSyncOp_FilesReportsOnlyTheRequestedJob(t *testing.T) {
	a, open, _ := newSyncOpFixture(t)
	other := fileKey{jobID: "job2", fileIdx: 7}
	open[other] = &openFile{info: FileInfo{Path: "other"}, key: other}

	r := runSyncOp(t, a, open, syncOp{kind: opFiles, jobID: "job1"})
	if len(r.files) != 1 || r.files[0] != 0 {
		t.Fatalf("Files for job1 = %v, want [0] — job2's file must not appear", r.files)
	}
}

// TestHandleSyncOp_UnknownFileIsAnErrorNotASilentSkip pins A2 for the
// adapter's disagreement case. A file the barrier believes open but the worker
// does not is a bookkeeping defect; skipping it silently would let the barrier
// commit an extent for a file nothing wrote.
func TestHandleSyncOp_UnknownFileIsAnErrorNotASilentSkip(t *testing.T) {
	a, open, _ := newSyncOpFixture(t)
	for _, kind := range []syncOpKind{opDrain, opSync, opStat, opTruncate} {
		r := runSyncOp(t, a, open, syncOp{kind: kind, jobID: "job1", fileIdx: 99})
		if r.err == nil {
			t.Errorf("op %v on an unopened file returned nil error", kind)
		}
	}
}

// TestHandleSyncOp_DrainSyncStatTruncate walks the four per-file operations
// against a real handle, so each one is pinned to its actual effect rather
// than to the dispatcher merely returning.
func TestHandleSyncOp_DrainSyncStatTruncate(t *testing.T) {
	a, open, key := newSyncOpFixture(t)
	f := open[key]

	if err := f.w.Accept(articleID{msgID: "a0", artIdx: 0}, 0, []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	r := runSyncOp(t, a, open, syncOp{kind: opDrain, jobID: "job1", fileIdx: 0})
	if r.err != nil || len(r.written) != 1 || r.written[0].ArtIdx != 0 {
		t.Fatalf("Drain = %+v, err=%v; want one entry for article 0", r.written, r.err)
	}
	if r := runSyncOp(t, a, open, syncOp{kind: opSync, jobID: "job1", fileIdx: 0}); r.err != nil {
		t.Fatalf("Sync: %v", r.err)
	}
	r = runSyncOp(t, a, open, syncOp{kind: opStat, jobID: "job1", fileIdx: 0})
	if r.err != nil || r.size != 10 {
		t.Fatalf("Stat size = %d, err=%v; want 10", r.size, r.err)
	}
	if r := runSyncOp(t, a, open, syncOp{kind: opTruncate, jobID: "job1", fileIdx: 0, bound: 4}); r.err != nil {
		t.Fatalf("Truncate: %v", r.err)
	}
	st, err := os.Stat(f.info.Path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 4 {
		t.Errorf("file is %d bytes after a truncate to 4, want 4", st.Size())
	}
}

// TestSyncTargetSubmit_AfterStopDoesNotBlock pins the shutdown path of submit.
// A barrier operation posted to a stopped assembler must return an error
// rather than wait for a worker that will never answer — B4's "never stalls
// the process" applied to the adapter itself.
func TestSyncTargetSubmit_AfterStopDoesNotBlock(t *testing.T) {
	a := New(Options{FileInfo: func(string, int) (FileInfo, error) { return FileInfo{}, nil }}, slog.Default())
	if err := a.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := a.Stop(); err != nil {
		t.Fatal(err)
	}
	tgt := a.SyncTargetFor("job1").(*jobSyncTarget)
	if _, err := tgt.submit(context.Background(), syncOp{kind: opFiles}); err == nil {
		t.Fatal("submit returned nil error after Stop; the barrier would block on a dead worker")
	}
}

// TestSyncTargetFor_RoundTripsThroughTheWorker is the end-to-end check that
// the control-message plumbing actually reaches the worker goroutine, rather
// than the per-op unit tests above passing while nothing is wired.
func TestSyncTargetFor_RoundTripsThroughTheWorker(t *testing.T) {
	dir := t.TempDir()
	files := map[string]FileInfo{}
	path := registerFile(t, dir, files, "job1", 0, 2)
	a := startAssembler(t, makeOpts(dir, files))

	if err := a.WriteArticle(t.Context(), WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, MessageID: "a0", Offset: 0, Data: []byte("abcd"),
	}); err != nil {
		t.Fatal(err)
	}
	tgt := a.SyncTargetFor("job1")
	if got := tgt.Files(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("Files() = %v through the worker, want [0]", got)
	}
	written, err := tgt.Drain(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 || written[0].ArtIdx != 0 {
		t.Fatalf("Drain through the worker = %v, want one entry for article 0", written)
	}
	if err := tgt.Sync(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	_ = path

}
