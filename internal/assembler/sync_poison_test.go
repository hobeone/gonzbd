package assembler

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

// newWrittenFileWriter returns a FileWriter holding one successfully written
// article (index 0), not yet covered by any Sync.
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
	w.admitAccepted(0)
	if err := w.Accept(articleID{msgID: "m1", artIdx: 0}, 0, []byte("AAAA")); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	return w
}

// TestSync_FailedSyncRollsBackEveryUnsyncedArticle pins #760 on FileWriter:
// once Sync fails (e.g. EIO), every article written since the last successful
// Sync is rolled back into poisoned with its part and seenDone entry cleared,
// while keeping the range it wrote.
func TestSync_FailedSyncRollsBackEveryUnsyncedArticle(t *testing.T) {
	w := newWrittenFileWriter(t)
	w.admitAccepted(1)
	if err := w.Accept(articleID{msgID: "m2", artIdx: 1}, 4, []byte("BBBB")); err != nil {
		t.Fatalf("Accept m2: %v", err)
	}
	if w.parts() != 2 {
		t.Fatalf("precondition: parts = %d, want 2", w.parts())
	}

	// First Sync fails with EIO; a retry Sync returns nil (Linux errseq
	// behaviour), which is why the failure has to be acted on at once.
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

	if len(w.unsynced) != 0 {
		t.Errorf("unsynced after failed Sync = %v, want empty", w.unsynced)
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

	// The retry Sync returns nil and has nothing left to roll back.
	if err := w.Sync(); err != nil {
		t.Fatalf("retry Sync: %v", err)
	}
	if rolled := w.takePoisoned(); len(rolled) != 0 {
		t.Errorf("retry Sync poisoned %v, want nothing", rolled)
	}
}

// TestSync_SuccessCoversTheArticlesWrittenBeforeIt pins that a later failed
// Sync rolls back only what it had not been covered by an earlier successful
// one: a successful fsync made those bytes durable, so a later failure says
// nothing about them.
func TestSync_SuccessCoversTheArticlesWrittenBeforeIt(t *testing.T) {
	w := newWrittenFileWriter(t)
	w.syncFile = func() error { return nil }
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	w.admitAccepted(1)
	if err := w.Accept(articleID{msgID: "m2", artIdx: 1}, 4, []byte("BBBB")); err != nil {
		t.Fatalf("Accept m2: %v", err)
	}
	w.syncFile = func() error { return syscall.EIO }
	if err := w.Sync(); err == nil {
		t.Fatal("second Sync returned nil, want error")
	}

	if rolled := w.takePoisoned(); !slices.Equal(rolled, []int32{1}) {
		t.Errorf("takePoisoned = %v, want [1]: article 0 was covered by the first, successful Sync", rolled)
	}
	if w.parts() != 1 {
		t.Errorf("parts = %d, want 1: the covered article keeps its part", w.parts())
	}
}

func TestFileWriter_PoisonSyncAndRollbackSyncedArticle(t *testing.T) {
	w := newWrittenFileWriter(t)
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
	if len(w.unsynced) != 0 {
		t.Errorf("poisonSync left unsynced=%v, want empty", w.unsynced)
	}

	var unwritten []int32
	a := newHelperAssembler()
	a.opts.OnArticlesUnwritten = func(_ string, _ int, arts []int32) {
		unwritten = append(unwritten, arts...)
	}
	a.releasePoisoned(&openFile{w: w, info: FileInfo{TotalParts: 1}})
	if !slices.Equal(unwritten, []int32{0, 1}) {
		t.Errorf("releasePoisoned unwritten = %v, want [0 1]", unwritten)
	}
	if got := w.takePoisoned(); len(got) != 0 {
		t.Errorf("releasePoisoned left %v poisoned; each set must be routed exactly once", got)
	}
}

func TestDrainAndClose_FailedSyncRoutesRolledBackArticles(t *testing.T) {
	a := newHelperAssembler()
	var unwritten []int32
	a.opts.OnArticlesUnwritten = func(_ string, _ int, arts []int32) {
		unwritten = append(unwritten, arts...)
	}

	// CloseJobHandles and the worker-exit drain call drainAndClose with
	// nothing after it, so drainAndClose must route the poisoned set itself
	// before Close throws the writer away.
	key := fileKey{jobID: "job1", fileIdx: 0}
	g := newHelperFile(t, t.TempDir(), "close-handles-sync-fail.dat", 0)
	g.w.key = key
	g.w.admitAccepted(8)
	if err := g.w.Accept(articleID{msgID: "m8", artIdx: 8}, 0, []byte("abcd")); err != nil {
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
