package assembler

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

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
	files["job1:0"] = FileInfo{Path: path, TotalParts: 1000, ExpectedSize: 8192}
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

	// Guard the fixture: pre-allocation must actually have inflated the file,
	// or the truncate below has nothing to remove and passes vacuously.
	if st, err := os.Stat(path); err == nil && st.Size() <= 300 {
		t.Skipf("filesystem did not pre-allocate (size %d); the truncate has nothing to trim", st.Size())
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
