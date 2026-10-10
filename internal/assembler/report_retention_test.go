package assembler

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	_ "modernc.org/sqlite"
)

// newWrittenFileWriter returns a FileWriter holding one successfully written
// article, ready to be drained.
func newWrittenFileWriter(t *testing.T) *FileWriter {
	t.Helper()
	path := filepath.Join(t.TempDir(), "movie.bin")
	fh, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fh.Close() })

	key := fileKey{jobID: "job1", fileIdx: 0}
	w := newFileWriter(fh, path, key)
	if err := w.Accept(articleID{msgID: "m1", artIdx: 0}, 0, []byte("AAAA"), 0); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	return w
}

// TestDrainReport_SurvivesAFailureAfterTheSync pins the retention against the
// window it was supposed to cover.
//
// Sync discarded the report the moment the fsync returned, but the barrier's
// order is drain-all, sync-all, build, then Commit and AckDurable. Every
// failure AFTER the fsync — a Stat timeout, a failed store commit, an
// AckDurable answering job.ErrNotResident — therefore lost the report exactly as
// it did before the retention existed. The reported field's own doc claimed
// coverage of "the sync, the run commit, the truncate"; only the first was
// real.
//
// Losing it is not losing the bytes, but it is losing the file. Those articles
// keep their bytes on disk and are never acked and never named by a run,
// and a redelivery is dropped by handleSuccessArticle's seenDone check with no
// write and no partsWritten increment — so the file cannot complete for the
// life of the handle.
func TestDrainReport_SurvivesAFailureAfterTheSync(t *testing.T) {
	w := newWrittenFileWriter(t)

	first, err := w.Drain()
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// Grounding: without a first report there is nothing to lose and the
	// assertion below would hold for the wrong reason.
	if len(first) != 1 {
		t.Fatalf("first Drain returned %d articles, want 1", len(first))
	}

	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// The barrier's commit or ack fails here. Nothing tells the writer, which
	// is the whole point: the writer cannot know, so it must keep the report
	// until something confirms the cycle landed.
	second, err := w.Drain()
	if err != nil {
		t.Fatalf("second Drain: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("second Drain returned %d articles, want the report re-delivered. "+
			"The article's bytes are on disk but it is never acked and never earns a "+
			"durable bit, and a redelivery is dropped as a duplicate, so the file can "+
			"never complete for the life of this handle", len(second))
	}
	if second[0].ArtIdx != first[0].ArtIdx {
		t.Errorf("re-reported article %d, want %d", second[0].ArtIdx, first[0].ArtIdx)
	}
}

// TestDrainReport_IsReleasedOnceTheCycleIsConfirmed pins the other side, which
// is what keeps the retained set bounded: a confirmed cycle must drop its
// report, or every later Drain re-reports every article ever written to the
// file and the barrier's per-cycle commit grows without bound.
func TestDrainReport_IsReleasedOnceTheCycleIsConfirmed(t *testing.T) {
	w := newWrittenFileWriter(t)

	if _, err := w.Drain(); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	w.Confirm()

	after, err := w.Drain()
	if err != nil {
		t.Fatalf("Drain after Confirm: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("Drain re-reported %d articles after the cycle was confirmed; the "+
			"retained set never shrinks and every checkpoint re-does the whole file",
			len(after))
	}
}

// TestJobSyncTargetConfirm_SwallowsAStoppedAssembler pins that a Confirm which
// cannot reach the worker is not an error anyone has to handle.
//
// It records work that already succeeded — the runs are committed and the
// articles are acked — so there is no recovery a caller could perform. What a
// missed Confirm actually costs is one redundant re-report on the next Drain,
// which R12 requires the apply to absorb. Returning an error here would invent
// a failure path for a cycle that has already landed.
func TestJobSyncTargetConfirm_SwallowsAStoppedAssembler(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 1)

	a := startAssembler(t, makeOpts(dir, files))
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// A stopped assembler answers ErrAssemblerStopped to every submit. The
	// requirement is that this returns quietly rather than panicking or
	// blocking — it has no error to return by design.
	tgt := a.SyncTargetFor("job1")
	tgt.Confirm(t.Context(), 0)
}

// TestDrainReport_FailedSyncPoisonsReportAndRollsBackArticles pins #760 on
// FileWriter: once Sync fails (e.g. EIO), the retained drain report (and any
// article written between Drain and Sync) is released as failed rather than
// re-drained on the next Drain, and the affected articles are rolled back into
// poisoned with their parts and seenDone entries cleared, while keeping the
// ranges they wrote.
func TestDrainReport_FailedSyncPoisonsReportAndRollsBackArticles(t *testing.T) {
	w := newWrittenFileWriter(t)
	w.admitAccepted(0)

	first, err := w.Drain()
	if err != nil {
		t.Fatalf("first Drain: %v", err)
	}
	if len(first) != 1 || first[0].ArtIdx != 0 {
		t.Fatalf("first Drain = %+v, want [artIdx 0]", first)
	}

	// An article written between Drain and Sync also sits in the page cache
	// when fsync fails and must be rolled back alongside w.reported.
	w.admitAccepted(1)
	if err := w.Accept(articleID{msgID: "m2", artIdx: 1}, 4, []byte("BBBB"), 0); err != nil {
		t.Fatalf("Accept m2: %v", err)
	}
	if w.parts() != 2 {
		t.Fatalf("precondition: parts = %d, want 2", w.parts())
	}

	// First Sync fails with EIO; retry Sync returns nil (Linux errseq behavior).
	syncCalls := 0
	w.syncFile = func() error {
		syncCalls++
		if syncCalls == 1 {
			return os.ErrInvalid
		}
		return nil
	}
	if err := w.Sync(); err == nil {
		t.Fatal("first Sync returned nil, want error")
	}

	if got := w.unconfirmed(); len(got) != 0 {
		t.Errorf("unconfirmed after failed Sync = %+v, want empty (report poisoned)", got)
	}
	if got := w.writtenSoFar(); len(got) != 0 {
		t.Errorf("writtenSoFar after failed Sync = %+v, want empty", got)
	}
	if w.parts() != 0 {
		t.Errorf("parts after failed Sync = %d, want 0 (parts rolled back)", w.parts())
	}
	if rolled := w.takePoisoned(); !slices.Equal(rolled, []int32{0, 1}) {
		t.Errorf("takePoisoned = %v, want [0 1] rolled back to Outstanding", rolled)
	}
	// The rolled-back article keeps the range it wrote: its redelivery is
	// accepted (same artIdx) and a rival is refused.
	if owner, owned := w.owned.ownerOf(Range{0, 4}, articleID{artIdx: 9}); !owned || owner.artIdx != 0 {
		t.Errorf("ownerOf([0,4)) = (%+v, %v) after failed Sync, want article 0 still owning it", owner, owned)
	}
	if _, owned := w.owned.ownerOf(Range{0, 4}, articleID{artIdx: 0}); owned {
		t.Error("the rolled-back article's own redelivery was refused after a failed Sync")
	}

	// Retry cycle: Drain + Sync where Sync now returns nil must report nothing.
	retryDrained, err := w.Drain()
	if err != nil {
		t.Fatalf("retry Drain: %v", err)
	}
	if len(retryDrained) != 0 {
		t.Errorf("retry Drain returned %+v, want empty (failed Sync must not re-drain)", retryDrained)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("retry Sync: %v", err)
	}
}

func runSyncOpWithCompleted(t *testing.T, a *Assembler, open map[fileKey]*openFile, completed map[fileKey]struct{}, op syncOp) syncReply {
	t.Helper()
	op.reply = make(chan syncReply, 1)
	a.handleSyncOp(&op, open, completed)
	select {
	case r := <-op.reply:
		return r
	default:
		t.Fatal("handleSyncOp did not reply")
		return syncReply{}
	}
}

func TestFileWriter_PoisonSyncAndRollbackSyncedArticle(t *testing.T) {
	w := newWrittenFileWriter(t)
	w.admitAccepted(0)
	if _, err := w.Drain(); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// Calling rollbackSyncedArticle twice for the same artIdx must append to
	// w.poisoned only once.
	w.rollbackSyncedArticle(0)
	w.rollbackSyncedArticle(0)
	if got := len(w.poisoned); got != 1 {
		t.Fatalf("len(w.poisoned) = %d after duplicate rollbackSyncedArticle, want 1", got)
	}
	// A different article is still rolled back alongside it.
	w.admitAccepted(1)
	w.rollbackSyncedArticle(1)
	if !slices.Equal(w.poisoned, []int32{0, 1}) {
		t.Fatalf("w.poisoned = %v, want [0 1]", w.poisoned)
	}
	w.poisonSync()
	if len(w.reported) != 0 || len(w.written) != 0 {
		t.Errorf("poisonSync left reported=%d written=%d, want 0/0", len(w.reported), len(w.written))
	}

	var unwritten []int32
	a := newHelperAssembler()
	a.opts.OnArticlesUnwritten = func(_ string, _ int, arts []int32) {
		unwritten = append(unwritten, arts...)
	}
	key := fileKey{jobID: "job1", fileIdx: 0}
	completed := map[fileKey]struct{}{key: {}}
	f := &openFile{w: w, info: FileInfo{TotalParts: 1}}
	a.releaseSyncRollback(f, key, completed)
	if !slices.Equal(unwritten, []int32{0, 1}) {
		t.Errorf("releaseSyncRollback unwritten = %v, want [0 1]", unwritten)
	}
	if _, stillDone := completed[key]; stillDone {
		t.Error("releaseSyncRollback left completed[key] set when parts < TotalParts")
	}
	if !f.rolledBack {
		t.Error("releaseSyncRollback did not set f.rolledBack when lifting completed[key]")
	}

	// When parts() >= TotalParts (or TotalParts == 0), releaseSyncRollback must
	// preserve completed[key] and leave f.rolledBack false.
	w.admitAccepted(0)
	completed[key] = struct{}{}
	f.rolledBack = false
	a.releaseSyncRollback(f, key, completed)
	if _, stillDone := completed[key]; !stillDone {
		t.Error("releaseSyncRollback deleted completed[key] when parts() >= TotalParts")
	}
	if f.rolledBack {
		t.Error("releaseSyncRollback set f.rolledBack when parts() >= TotalParts")
	}

	w.fail(articleID{artIdx: 0})
	f.info.TotalParts = 0
	a.releaseSyncRollback(f, key, completed)
	if _, stillDone := completed[key]; !stillDone {
		t.Error("releaseSyncRollback deleted completed[key] when TotalParts == 0")
	}
}

func TestDrainAndClose_FailedSyncRoutesRolledBackArticles(t *testing.T) {
	a := newHelperAssembler()
	var unwritten []int32
	a.opts.OnArticlesUnwritten = func(_ string, _ int, arts []int32) {
		unwritten = append(unwritten, arts...)
	}

	key := fileKey{jobID: "job1", fileIdx: 0}
	f := newHelperFile(t, t.TempDir(), "close-sync-fail.dat", 0)
	f.info.TotalParts = 1
	open := map[fileKey]*openFile{key: f}
	completed := map[fileKey]struct{}{key: {}}
	f.w.admitAccepted(7)
	if err := f.w.Accept(articleID{msgID: "m7", artIdx: 7}, 0, []byte("abcd"), 0); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	f.w.syncFile = func() error { return syscall.EIO }

	rClose := runSyncOpWithCompleted(t, a, open, completed, syncOp{kind: opClose, jobID: "job1", fileIdx: 0})
	if !errors.Is(rClose.err, syscall.EIO) {
		t.Fatalf("opClose = %v, want EIO", rClose.err)
	}
	if len(unwritten) != 1 || unwritten[0] != 7 {
		t.Errorf("OnArticlesUnwritten = %v, want [7] after failed close-time Sync", unwritten)
	}
	if _, stillDone := completed[key]; stillDone {
		t.Error("completed[key] still set after opClose failed Sync rolled back parts below TotalParts")
	}

	// CloseJobHandles and the worker-exit drain call drainAndClose with no
	// releaseSyncRollback after it, so drainAndClose must route the poisoned
	// set itself before Close throws the writer away.
	unwritten = nil
	g := newHelperFile(t, t.TempDir(), "close-handles-sync-fail.dat", 0)
	g.w.key = key
	g.w.admitAccepted(8)
	if err := g.w.Accept(articleID{msgID: "m8", artIdx: 8}, 0, []byte("abcd"), 0); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	g.w.syncFile = func() error { return syscall.EIO }
	if err := a.drainAndClose(g); !errors.Is(err, syscall.EIO) {
		t.Fatalf("drainAndClose = %v, want EIO", err)
	}
	if !slices.Equal(unwritten, []int32{8}) {
		t.Errorf("OnArticlesUnwritten = %v, want [8] from drainAndClose's failed Sync", unwritten)
	}
}
