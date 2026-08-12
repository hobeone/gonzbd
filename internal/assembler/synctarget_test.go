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
	tgt := a.SyncTargetFor("job1", nil).(*jobSyncTarget)
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
	tgt := a.SyncTargetFor("job1", oneFileMap{n: 2})
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

// TestSyncTargetFor_NilArticleMapAnswersUnknown pins the safe direction of the
// manifest dependency. The adapter has no manifest of its own, so with no
// ArticleMap it must answer "I don't know" rather than invent an ordinal: the
// barrier places durable bits by that ordinal, and a plausible-looking wrong
// answer marks the wrong article durable and costs a silently short file.
//
// A false ordinal makes Barrier.buildExtent fail loudly on the first article,
// which is the intended outcome — a barrier that cannot place an article must
// stop, not guess.
func TestSyncTargetFor_NilArticleMapAnswersUnknown(t *testing.T) {
	a := New(Options{FileInfo: func(string, int) (FileInfo, error) { return FileInfo{}, nil }}, slog.Default())
	tgt := a.SyncTargetFor("job1", nil)

	if got := tgt.ArticleCount(0); got != 0 {
		t.Errorf("ArticleCount with no ArticleMap = %d, want 0", got)
	}
	if _, ok := tgt.FileLocalOrdinal(0, 3); ok {
		t.Error("FileLocalOrdinal reported a usable ordinal with no ArticleMap; " +
			"the barrier would place a durable bit from an invented position")
	}
}

// TestSyncTargetFor_ArticleMapRejectsOutOfRangeArticles pins the false result
// on the supplied-map path too. An article that does not belong to the file is
// a bookkeeping defect, and reporting a usable ordinal for it would write the
// bit into some other article's position.
func TestSyncTargetFor_ArticleMapRejectsOutOfRangeArticles(t *testing.T) {
	a := New(Options{FileInfo: func(string, int) (FileInfo, error) { return FileInfo{}, nil }}, slog.Default())
	tgt := a.SyncTargetFor("job1", oneFileMap{n: 3})

	if got := tgt.ArticleCount(0); got != 3 {
		t.Errorf("ArticleCount = %d, want 3", got)
	}
	if ord, ok := tgt.FileLocalOrdinal(0, 2); !ok || ord != 2 {
		t.Errorf("FileLocalOrdinal(0,2) = (%d,%v), want (2,true)", ord, ok)
	}
	if _, ok := tgt.FileLocalOrdinal(0, 9); ok {
		t.Error("FileLocalOrdinal accepted article 9 for a 3-article file")
	}
}

// TestSyncTargetFiles_AfterStopReportsNoFiles pins Files' error branch. It has
// no error return, so a dead worker has to surface as an empty set — the
// barrier then fsyncs nothing and claims nothing, which is the safe answer.
func TestSyncTargetFiles_AfterStopReportsNoFiles(t *testing.T) {
	a := New(Options{FileInfo: func(string, int) (FileInfo, error) { return FileInfo{}, nil }}, slog.Default())
	if err := a.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := a.Stop(); err != nil {
		t.Fatal(err)
	}
	if got := a.SyncTargetFor("job1", oneFileMap{n: 1}).Files(); len(got) != 0 {
		t.Errorf("Files() = %v after Stop, want empty", got)
	}
}

// TestSyncTargetFor_NilArticleMapReportsNoFiles pins the inertness of a nil
// ArticleMap, which is a data-loss guard rather than a tidiness rule.
//
// Answering "unknown" to the manifest questions is only conditionally safe. A
// barrier that drains at least one article fails on the missing ordinal, but
// one whose drain is empty never reaches the lookup: it sizes the durable
// bitmap from ArticleCount == 0 and commits a zero-width bitmap over the
// stored one, erasing every durable bit the job had. The next restart reads
// nothing as durable and re-downloads everything — the ground L3 says a
// restart must not lose.
//
// Reporting no files removes the case: the barrier iterates nothing, so there
// is no extent to commit and nothing to erase.
func TestSyncTargetFor_NilArticleMapReportsNoFiles(t *testing.T) {
	dir := t.TempDir()
	files := map[string]FileInfo{}
	registerFile(t, dir, files, "job1", 0, 1000)
	a := startAssembler(t, makeOpts(dir, files))

	if err := a.WriteArticle(t.Context(), WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, MessageID: "a0", Offset: 0, Data: []byte("abcd"),
	}); err != nil {
		t.Fatal(err)
	}
	// The file IS open, so a target with a manifest sees it...
	if got := a.SyncTargetFor("job1", oneFileMap{n: 1}).Files(); len(got) != 1 {
		t.Fatalf("Files() with an ArticleMap = %v, want one file — the fixture is not exercising the guard", got)
	}
	// ...and one without a manifest must still report nothing.
	if got := a.SyncTargetFor("job1", nil).Files(); len(got) != 0 {
		t.Errorf("Files() with a nil ArticleMap = %v, want none: a barrier would size a "+
			"zero-width bitmap from ArticleCount==0 and commit it over the stored durable bits", got)
	}
}
