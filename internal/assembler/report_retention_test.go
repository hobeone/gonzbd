package assembler

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/storagefault"
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

// TestAssembler_FailedSyncReleasesArticlesToOutstandingAndAcksNothingOnRetry
// pins the end-to-end #760 contract across Assembler and handleSyncOp:
//  1. Articles are written and drained, then Sync fails with EIO.
//  2. OnArticlesUnwritten fires with the affected article indices so the queue
//     returns them to Outstanding, and a completed file's tombstone is lifted.
//  3. A retry Drain + Sync (where Sync now returns nil, matching Linux errseq)
//     reports 0 articles, so no article from the failed cycle is acked Done.
//  4. Re-delivering the Outstanding articles writes them afresh rather than
//     dropping them as duplicates.
func TestAssembler_FailedSyncReleasesArticlesToOutstandingAndAcksNothingOnRetry(t *testing.T) {
	a := &Assembler{log: slog.Default()}
	path := filepath.Join(t.TempDir(), "sync.dat")
	key := fileKey{jobID: "job1", fileIdx: 0}
	open := map[fileKey]*openFile{}
	completed := map[fileKey]struct{}{}

	syncCalls := 0
	a.opts.FileInfo = func(string, int) (FileInfo, error) {
		return FileInfo{Path: path, TotalParts: 1}, nil
	}
	a.opts.SyncFile = func() error {
		syncCalls++
		if syncCalls == 1 {
			return os.ErrInvalid
		}
		return nil
	}
	var unwritten []int32
	a.opts.OnArticlesUnwritten = func(_ string, _ int, arts []int32) {
		unwritten = append(unwritten, arts...)
	}
	var completedCalls int
	a.opts.OnFileComplete = func(_ string, _ int) {
		completedCalls++
	}

	// Write article 0; file opens via openTargetFile, reaches TotalParts (1), and is tombstoned in completed.
	a.processRequest(WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, MessageID: "m0@example",
		Offset: 0, Data: []byte("payload-0"),
	}, open, completed)
	f := open[key]
	t.Cleanup(func() {
		if f != nil {
			_ = f.w.Close()
		}
	})
	if completedCalls != 1 {
		t.Fatalf("precondition: OnFileComplete fired %d times, want 1", completedCalls)
	}

	// Cycle 1: Drain returns article 0, then Sync fails with EIO.
	rDrain1 := runSyncOpWithCompleted(t, a, open, completed, syncOp{kind: opDrain, jobID: "job1", fileIdx: 0})
	if rDrain1.err != nil || len(rDrain1.written) != 1 {
		t.Fatalf("Drain 1 = %+v, err=%v; want 1 article", rDrain1.written, rDrain1.err)
	}

	rSync1 := runSyncOpWithCompleted(t, a, open, completed, syncOp{kind: opSync, jobID: "job1", fileIdx: 0})
	if rSync1.err == nil {
		t.Fatal("Sync 1 returned nil, want error")
	}
	if len(unwritten) != 1 || unwritten[0] != 0 {
		t.Errorf("OnArticlesUnwritten = %v, want [0] (article returned to Outstanding)", unwritten)
	}
	if f.w.parts() != 0 {
		t.Errorf("parts after failed Sync = %d, want 0", f.w.parts())
	}
	if _, stillDone := completed[key]; stillDone {
		t.Error("completed[key] still set after failed Sync dropped parts below TotalParts")
	}

	// Cycle 2 (retry): Sync now returns nil (Linux errseq). Drain must return
	// 0 articles so the barrier acks nothing from the failed cycle.
	rDrain2 := runSyncOpWithCompleted(t, a, open, completed, syncOp{kind: opDrain, jobID: "job1", fileIdx: 0})
	if rDrain2.err != nil {
		t.Fatalf("Drain 2: %v", rDrain2.err)
	}
	if len(rDrain2.written) != 0 {
		t.Errorf("Drain 2 on retry returned %+v, want empty — barrier would ack bytes that never reached disk", rDrain2.written)
	}
	rSync2 := runSyncOpWithCompleted(t, a, open, completed, syncOp{kind: opSync, jobID: "job1", fileIdx: 0})
	if rSync2.err != nil {
		t.Fatalf("Sync 2: %v", rSync2.err)
	}

	// Re-delivering article 0 from Outstanding writes it afresh and completes the file again.
	a.processRequest(WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, MessageID: "m0@example",
		Offset: 0, Data: []byte("payload-0"),
	}, open, completed)
	if completedCalls != 2 {
		t.Errorf("OnFileComplete fired %d times after re-delivery, want 2", completedCalls)
	}
	rDrain3 := runSyncOpWithCompleted(t, a, open, completed, syncOp{kind: opDrain, jobID: "job1", fileIdx: 0})
	if rDrain3.err != nil || len(rDrain3.written) != 1 || rDrain3.written[0].ArtIdx != 0 {
		t.Errorf("Drain 3 after re-delivery = %+v, err=%v; want [artIdx 0]", rDrain3.written, rDrain3.err)
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

type testBarrierAcker struct {
	acked []int32
}

func (a *testBarrierAcker) AckDurable(p durability.DurableProof) error {
	a.acked = append(a.acked, p.Articles()...)
	return nil
}

type testBarrierStall struct {
	stalled []*storagefault.Fault
}

func (s *testBarrierStall) Stall(_ string, f *storagefault.Fault) { s.stalled = append(s.stalled, f) }
func (s *testBarrierStall) Fail(_ string, _ *storagefault.Fault)  {}

func openTestRunStore(t *testing.T) *durability.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "history.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE durable_runs (
		job_id TEXT NOT NULL,
		file_idx INTEGER NOT NULL,
		first_art_idx INTEGER NOT NULL,
		last_art_idx INTEGER NOT NULL,
		offset INTEGER NOT NULL,
		length INTEGER NOT NULL,
		crc32 INTEGER NOT NULL,
		PRIMARY KEY (job_id, file_idx, offset)
	)`)
	if err != nil {
		t.Fatalf("create durable_runs: %v", err)
	}
	return durability.NewStore(db, dbPath)
}

// TestBarrierWithAssembler_FailedSyncAcksNothingOnRetryAndReturnsArticlesToOutstanding
// pins the full #760 scenario across durability.Barrier and Assembler:
//   - Article 0 is written to the file.
//   - First Barrier.Run drains article 0, then Sync fails with EIO.
//   - Retry Barrier.Run runs on the same open handle with Sync returning nil.
//   - Asserts no article from the failed cycle is acked Done or committed to
//     durable_runs on the retry, and article 0 is returned to Outstanding via
//     OnArticlesUnwritten.
func TestBarrierWithAssembler_FailedSyncAcksNothingOnRetryAndReturnsArticlesToOutstanding(t *testing.T) {
	a, open, key := newSyncOpFixture(t)
	f := open[key]
	f.info.TotalParts = 1
	completed := map[fileKey]struct{}{}
	var unwritten []int32
	a.opts.OnArticlesUnwritten = func(_ string, _ int, arts []int32) {
		unwritten = append(unwritten, arts...)
	}

	a.processRequest(WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, MessageID: "m0@example",
		Offset: 0, Data: []byte("0123456789"),
	}, open, completed)
	if err := os.Truncate(f.info.Path, 4096); err != nil {
		t.Fatalf("Truncate to 4096: %v", err)
	}

	syncCalls := 0
	f.w.syncFile = func() error {
		syncCalls++
		if syncCalls == 1 {
			return syscall.EIO
		}
		return nil
	}

	rs := openTestRunStore(t)
	ack := &testBarrierAcker{}
	stall := &testBarrierStall{}
	b := durability.NewBarrier(rs, ack, stall, slog.New(slog.DiscardHandler))
	tgt := &directSyncTarget{a: a, open: open, completed: completed, jobID: "job1"}

	// First FinalizeFile: Sync fails with EIO, poisoning the report, rolling
	// article 0 back to Outstanding, and lifting completed[key].
	if err := b.FinalizeFile(t.Context(), "job1", 0, tgt); !errors.Is(err, syscall.EIO) {
		t.Fatalf("first FinalizeFile = %v, want EIO", err)
	}
	if len(ack.acked) != 0 {
		t.Fatalf("first FinalizeFile acked %v on failed Sync, want none", ack.acked)
	}
	if len(unwritten) != 1 || unwritten[0] != 0 {
		t.Errorf("OnArticlesUnwritten after failed Sync = %v, want [0] (article returned to Outstanding)", unwritten)
	}

	// Second Barrier.Run (checkpoint retry): Sync returns nil (Linux errseq).
	// No article from the failed cycle may be acked Done or committed to durable_runs.
	if err := b.Run(t.Context(), "job1", tgt); err != nil {
		t.Fatalf("retry Barrier.Run: %v", err)
	}
	if len(ack.acked) != 0 {
		t.Errorf("retry Barrier.Run acked %v Done after failed fsync, want [] (#760)", ack.acked)
	}
	runs, err := rs.ForJob(t.Context(), "job1")
	if err != nil {
		t.Fatalf("ForJob: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("durable_runs after retry = %+v, want empty (#760)", runs)
	}

	// Retry FinalizeFile before article 0 is re-delivered: must refuse with
	// ErrFileIncomplete rather than truncating or completing an incomplete file.
	if err := b.FinalizeFile(t.Context(), "job1", 0, tgt); !errors.Is(err, durability.ErrFileIncomplete) {
		t.Fatalf("retry FinalizeFile on rolled-back file = %v, want ErrFileIncomplete", err)
	}
	if st, err := os.Stat(f.info.Path); err != nil || st.Size() != 4096 {
		t.Errorf("file size after refused FinalizeFile = %d (err %v), want 4096", st.Size(), err)
	}

	// Re-deliver article 0 to the still-open FileWriter; it reaches TotalParts
	// again and FinalizeFile now trims the file to its exact 10-byte decoded size.
	a.processRequest(WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, MessageID: "m0@example",
		Offset: 0, Data: []byte("0123456789"),
	}, open, completed)
	if err := b.FinalizeFile(t.Context(), "job1", 0, tgt); err != nil {
		t.Fatalf("FinalizeFile after re-delivery: %v", err)
	}
	if st, err := os.Stat(f.info.Path); err != nil || st.Size() != 10 {
		t.Errorf("file size after FinalizeFile = %d (err %v), want 10", st.Size(), err)
	}
	if !slices.Equal(ack.acked, []int32{0}) {
		t.Errorf("acked after re-delivery FinalizeFile = %v, want [0]", ack.acked)
	}
}

type directSyncTarget struct {
	a         *Assembler
	open      map[fileKey]*openFile
	completed map[fileKey]struct{}
	jobID     string
}

func (d *directSyncTarget) Files() []int32 {
	r := d.run(syncOp{kind: opFiles, jobID: d.jobID})
	return r.files
}

func (d *directSyncTarget) Drain(_ context.Context, fileIdx int32) ([]durability.WrittenArticle, error) {
	r := d.run(syncOp{kind: opDrain, jobID: d.jobID, fileIdx: fileIdx})
	return r.written, r.err
}

func (d *directSyncTarget) Sync(_ context.Context, fileIdx int32) error {
	r := d.run(syncOp{kind: opSync, jobID: d.jobID, fileIdx: fileIdx})
	return r.err
}

func (d *directSyncTarget) Confirm(_ context.Context, fileIdx int32) {
	_ = d.run(syncOp{kind: opConfirm, jobID: d.jobID, fileIdx: fileIdx})
}

func (d *directSyncTarget) Stat(fileIdx int32) (int64, error) {
	r := d.run(syncOp{kind: opStat, jobID: d.jobID, fileIdx: fileIdx})
	return r.size, r.err
}

func (d *directSyncTarget) Truncate(_ context.Context, fileIdx int32, bound int64) error {
	r := d.run(syncOp{kind: opTruncate, jobID: d.jobID, fileIdx: fileIdx, bound: bound})
	return r.err
}

func (d *directSyncTarget) Path(fileIdx int32) string {
	if f, ok := d.open[fileKey{jobID: d.jobID, fileIdx: int(fileIdx)}]; ok {
		return f.info.Path
	}
	return ""
}

func (d *directSyncTarget) run(op syncOp) syncReply {
	op.reply = make(chan syncReply, 1)
	d.a.handleSyncOp(&op, d.open, d.completed)
	return <-op.reply
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
