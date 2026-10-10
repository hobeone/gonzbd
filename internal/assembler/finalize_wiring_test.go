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
	// arrives, so the file carries a hole: the first run ends at 100 while the
	// highest end offset the record holds reaches 300. If the bound stopped at
	// the hole, this file would come back 100 bytes long.
	for _, art := range []struct {
		idx int32
		off int64
	}{{0, 0}, {2, 200}} {
		if err := writeArticle(t.Context(), a, WriteRequest{
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

	runs := durability.NewStore(db, "history.db")
	ack := &noopAcker{}
	b := durability.NewBarrier(runs, ack, noopStall{}, slog.New(slog.DiscardHandler))

	tgt := a.SyncTargetFor("job1")
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
			"offset the record holds. 100 would mean the bound stopped at the hole "+
			"and article 2's bytes were destroyed; 8192 would mean no truncate was "+
			"delivered and pre-allocation's zeros survive as par2 damage", st.Size())
	}
	if len(ack.acked) == 0 {
		t.Error("FinalizeFile acked nothing; the articles it just fsynced stay Outstanding forever")
	}

	// The recorded runs must describe the file the truncate left behind. A
	// hole means two rows, and the top one must reach the file's new end — a
	// record claiming more than the file holds is exactly what the next
	// resume's size gate discards the whole file for.
	stored, err := runs.ForFile(ctx, "job1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("ForFile returned %d runs, want 2 — one either side of article 1's hole", len(stored))
	}
	if end := stored[1].Offset + stored[1].Length; end != 300 {
		t.Errorf("the top run ends at %d, want 300 — the record and the trimmed file "+
			"must agree, or the next resume discards this file and re-downloads it", end)
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

// TestCompletedFileIsTrimmedAndClosedBeforeItIsReported pins finalizeFile's
// order against a real file: the last part arrives, the file is fsynced,
// trimmed from its preallocated size to the end of its last written byte and
// closed, and only then reported complete.
func TestCompletedFileIsTrimmedAndClosedBeforeItIsReported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job1_0.dat")
	// TotalParts 1: the single article below completes the file. ExpectedSize
	// preallocates past the written extent, which is what the trim removes.
	files := map[string]FileInfo{"job1:0": {Path: path, TotalParts: 1, ExpectedSize: 4096}}

	sizeAtReport := make(chan int64, 1)
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(string, int) {
		st, err := os.Stat(path)
		if err != nil {
			t.Errorf("stat at report: %v", err)
			sizeAtReport <- -1
			return
		}
		sizeAtReport <- st.Size()
	}
	a := startAssembler(t, opts)

	if err := writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, MessageID: "a0",
		Offset: 0, Data: make([]byte, 100),
	}); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}
	select {
	case got := <-sizeAtReport:
		if got != 100 {
			t.Errorf("file is %d bytes when reported complete, want 100 — the "+
				"report came before the trim, so a caller recording it complete "+
				"would record pre-allocation's trailing zeros", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the file never completed; the fixture is not exercising finalizeFile")
	}
	if got := a.SyncTargetFor("job1").Files(); len(got) != 0 {
		t.Errorf("Files() = %v after completion, want none — the handle leaks for "+
			"the rest of the job", got)
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

	tgt := a.SyncTargetFor("job1")
	if got := tgt.Path(0); got != path {
		t.Errorf("Path(0) = %q, want %q", got, path)
	}
	// A file the resolver does not know about degrades to "" rather than
	// failing: Path is diagnostic, and nothing may branch on it.
	if got := tgt.Path(99); got != "" {
		t.Errorf("Path(99) = %q for an unregistered file, want \"\"", got)
	}
}
