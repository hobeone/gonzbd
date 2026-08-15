package assembler

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// oneFileMap is an ArticleMap for a single file whose articles are numbered
// from zero, which is what the fixtures below build.
type oneFileMap struct{ n int }

func (m oneFileMap) ArticleCount(int32) int { return m.n }
func (m oneFileMap) FileLocalOrdinal(_ int32, artIdx int32) (int, bool) {
	if int(artIdx) >= m.n || artIdx < 0 {
		return 0, false
	}
	return int(artIdx), true
}

type noopAcker struct{ acked []int32 }

func (a *noopAcker) AckDurable(p durability.DurableProof) error {
	a.acked = append(a.acked, p.Articles()...)
	return nil
}

type noopStall struct{}

func (noopStall) Stall(string, *storagefault.Fault) {}
func (noopStall) Fail(string, *storagefault.Fault)  {}

// TestFinalizeFileTruncatesThroughTheRealAdapter is the delivery-chain test for
// the completion truncate: durability.Barrier.FinalizeFile → the control-message
// adapter → FileWriter.Truncate → a real file on disk.
//
// Every other test on this path stubs one of its links. finalize_test.go drives
// the barrier against a stub Truncator, so it pins WHAT the bound should be but
// never delivers it; FileWriter's own S6 tests call Truncate directly, so they
// pin the writer but not who calls it. Between them sat the adapter's opTruncate
// case and the re-stat that follows the trim, with no test traversing either.
//
// Task 3 broke a truncation bound while every gate stayed green. An untested
// delivery path for the very bound this task exists to fix is the wrong gap to
// carry, so this closes it end to end against real bytes.
func TestFinalizeFileTruncatesThroughTheRealAdapter(t *testing.T) {
	ctx := context.Background()

	// A file pre-allocated well past its decoded content, which is the
	// condition the completion truncate exists to clean up.
	dir := t.TempDir()
	files := map[string]FileInfo{}
	path := filepath.Join(dir, "job1_0.dat")
	files["job1:0"] = FileInfo{Path: path, TotalParts: 1000}
	opts := makeOpts(dir, files)
	a := startAssembler(t, opts)

	// Two articles land: 0 at [0,100) and 2 at [200,300). Article 1 never
	// arrives, so the file carries a hole and the gapless prefix stops at 100
	// while the real extent reaches 300. If the truncate used VerifiedTo, this
	// file would come back 100 bytes long.
	for _, art := range []struct {
		idx int32
		off int64
	}{{0, 0}, {2, 200}} {
		if err := a.WriteArticle(t.Context(), WriteRequest{
			JobID: "job1", FileIdx: 0, ArtIdx: art.idx,
			MessageID: string(rune('a' + art.idx)), //nolint:unconvert // art.idx is int32; the rune conversion is the point
			Offset:    art.off, Data: make([]byte, 100),
		}); err != nil {
			t.Fatalf("WriteArticle %d: %v", art.idx, err)
		}
	}

	hdb, err := history.Open(t.Context(), filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = hdb.Close() })
	db := history.NewRepository(hdb).DB()

	facts := durability.NewSQLiteFactLog(db)
	if err := facts.Append(ctx, "job1", []durability.ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
		{FileIdx: 0, ArtIdx: 2, Offset: 200, Length: 100},
	}); err != nil {
		t.Fatalf("Append facts: %v", err)
	}

	ack := &noopAcker{}
	b := durability.NewBarrier(facts, durability.NewSQLiteExtentStore(db), ack, noopStall{},
		slog.New(slog.DiscardHandler))

	tgt := a.SyncTargetFor("job1", oneFileMap{n: 3})
	trunc, ok := tgt.(durability.Truncator)
	if !ok {
		t.Fatal("the per-job adapter does not implement durability.Truncator, so no barrier can trim a completed file")
	}

	// The file must be longer than the decoded content or the truncate has
	// nothing to remove and the assertion below passes vacuously.
	//
	// This is done EXPLICITLY rather than by depending on pre-allocation. The
	// previous version skipped when the filesystem had no fallocate, which
	// meant the only test traversing FinalizeFile -> adapter -> Truncate could
	// vanish from a CI run with no signal at all — and a skipped test is
	// indistinguishable from a passing one in a summary. Extending the file
	// here makes the fixture independent of filesystem capability, so the pin
	// either runs or fails loudly.
	//
	// The extension is a plain ftruncate on a second handle: the assembler
	// owns the writer's handle, and this must not race it. Nothing has been
	// written past 300 bytes, so growing the file cannot destroy content.
	if err := extendFileTo(path, 8192); err != nil {
		t.Fatalf("extend target to 8192: %v", err)
	}
	if st, err := os.Stat(path); err != nil {
		t.Fatalf("stat target: %v", err)
	} else if st.Size() <= 300 {
		t.Fatalf("target is %d bytes; the truncate has nothing to trim and the "+
			"assertion below would pass vacuously", st.Size())
	}

	if err := b.FinalizeFile(ctx, "job1", 0, trunc); err != nil {
		t.Fatalf("FinalizeFile: %v", err)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 300 {
		t.Errorf("file is %d bytes after FinalizeFile, want 300 — the highest end "+
			"offset among durable facts. 100 would mean the gapless prefix was used "+
			"and article 2's bytes were destroyed; 8192 would mean no truncate was "+
			"delivered and pre-allocation's zeros survive as par2 damage", st.Size())
	}
	if len(ack.acked) == 0 {
		t.Error("FinalizeFile acked nothing; the articles it just fsynced stay Outstanding forever")
	}

	// The committed stamp must describe the file AFTER the trim, or the next
	// resume's S7 validity check fails and discards a valid cache.
	exts, err := durability.NewSQLiteExtentStore(db).Load(ctx, "job1")
	if err != nil {
		t.Fatal(err)
	}
	if len(exts) != 1 {
		t.Fatalf("Load returned %d extents, want 1", len(exts))
	}
	if exts[0].Size != 300 {
		t.Errorf("committed Size = %d, want 300 — the stamp describes the file before "+
			"the truncate, so the next resume sees a size mismatch and throws the cache away",
			exts[0].Size)
	}
}

// extendFileTo grows path to n bytes without touching its content.
//
// A separate handle, opened and closed here, so it never races the handle the
// assembler's worker owns.
func extendFileTo(path string, n int64) error {
	fh, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = fh.Close() }()
	return fh.Truncate(n)
}

// TestCompletedFileStaysOpenForTheBarrierThenCloses pins the handoff that
// makes the completion truncate reachable at all.
//
// The assembler used to drain, fsync and CLOSE a file the moment its last part
// arrived, and only then report it complete. Every operation
// durability.Barrier.FinalizeFile performs — Drain, Sync, Truncate, Stat —
// goes through that handle, so under the old order a completed file could
// never be trimmed: it would keep pre-allocation's trailing zeros, which par2
// reports as damage on a file whose download was perfectly healthy.
//
// The order is now: tombstone and report, caller finalizes, caller calls
// CloseFile. This test walks that sequence against a real file, asserting the
// handle is still usable in between and gone afterwards.
func TestCompletedFileStaysOpenForTheBarrierThenCloses(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "job1_0.dat")
	// TotalParts 1 means the single article below completes the file, which is
	// what puts finalizeFile on the path. Every other test in this file keeps
	// the file incomplete precisely to avoid it.
	files := map[string]FileInfo{"job1:0": {Path: path, TotalParts: 1}}

	done := make(chan struct{}, 1)
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(string, int) {
		select {
		case done <- struct{}{}:
		default:
		}
	}
	a := startAssembler(t, opts)

	if err := a.WriteArticle(t.Context(), WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, MessageID: "a0",
		Offset: 0, Data: make([]byte, 100),
	}); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the file never completed; the fixture is not exercising finalizeFile")
	}

	tgt := a.SyncTargetFor("job1", oneFileMap{n: 1})
	if got := tgt.Files(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("Files() = %v after completion, want [0] — the handle was closed "+
			"before the barrier could finalize the file, so it keeps pre-allocation's "+
			"trailing zeros and its last articles are never acked", got)
	}

	if err := extendFileTo(path, 4096); err != nil {
		t.Fatalf("extend: %v", err)
	}

	hdb, err := history.Open(t.Context(), filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = hdb.Close() })
	db := history.NewRepository(hdb).DB()
	facts := durability.NewSQLiteFactLog(db)
	if err := facts.Append(ctx, "job1", []durability.ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	ack := &noopAcker{}
	b := durability.NewBarrier(facts, durability.NewSQLiteExtentStore(db), ack, noopStall{},
		slog.New(slog.DiscardHandler))

	trunc, ok := tgt.(durability.Truncator)
	if !ok {
		t.Fatal("the per-job adapter does not implement durability.Truncator")
	}
	if err := b.FinalizeFile(ctx, "job1", 0, trunc); err != nil {
		t.Fatalf("FinalizeFile on a completed file: %v", err)
	}
	if st, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if st.Size() != 100 {
		t.Errorf("file is %d bytes after finalizing a completed file, want 100", st.Size())
	}
	if len(ack.acked) != 1 || ack.acked[0] != 0 {
		t.Errorf("acked %v, want [0] — the last drain's articles are the ones a "+
			"close-at-completion would have thrown away", ack.acked)
	}

	if err := a.CloseFile(t.Context(), "job1", 0); err != nil {
		t.Fatalf("CloseFile: %v", err)
	}
	if got := tgt.Files(); len(got) != 0 {
		t.Errorf("Files() = %v after CloseFile, want none — the handle leaks for the "+
			"rest of the job", got)
	}
	// Idempotent: CancelJob, CloseJobHandles and shutdown can all get there
	// first, and the completion consumer must not turn that race into an error.
	if err := a.CloseFile(t.Context(), "job1", 0); err != nil {
		t.Errorf("second CloseFile returned %v, want nil — closing an already-closed "+
			"file is a race with cancel/shutdown, not a disagreement", err)
	}
}

// TestSyncTargetPath_ReportsTheResolvedTargetPath pins R27's input. The
// barrier stamps this onto every storage fault it routes, and an empty one
// tells a user their disk is full without saying which disk.
func TestSyncTargetPath_ReportsTheResolvedTargetPath(t *testing.T) {
	dir := t.TempDir()
	files := map[string]FileInfo{}
	path := registerFile(t, dir, files, "job1", 0, 2)
	a := New(makeOpts(dir, files), slog.New(slog.DiscardHandler))

	tgt := a.SyncTargetFor("job1", oneFileMap{n: 2})
	if got := tgt.Path(0); got != path {
		t.Errorf("Path(0) = %q, want %q", got, path)
	}
	// A file the resolver does not know about degrades to "" rather than
	// failing: Path is diagnostic, and nothing may branch on it.
	if got := tgt.Path(99); got != "" {
		t.Errorf("Path(99) = %q for an unregistered file, want \"\"", got)
	}
}
