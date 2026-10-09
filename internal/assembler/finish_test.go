package assembler

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

const finishPrealloc = 1000

// newPreallocatedWriter returns a writer over a file preallocated to
// finishPrealloc bytes, as the assembler leaves one before its articles land.
func newPreallocatedWriter(t *testing.T) *FileWriter {
	t.Helper()
	w := newTestFileWriter(t)
	if err := w.handle.Truncate(finishPrealloc); err != nil {
		t.Fatalf("preallocate: %v", err)
	}
	return w
}

func fileSize(t *testing.T, w *FileWriter) int64 {
	t.Helper()
	st, err := os.Stat(w.path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func TestFileWriter_FinishTruncatesToTheLastOwnedEnd(t *testing.T) {
	w := newPreallocatedWriter(t)
	if err := w.Accept(articleID{msgID: "a0", artIdx: 0}, 0, bytes.Repeat([]byte{1}, 100), 0); err != nil {
		t.Fatal(err)
	}
	if err := w.Accept(articleID{msgID: "a1", artIdx: 1}, 100, bytes.Repeat([]byte{2}, 150), 0); err != nil {
		t.Fatal(err)
	}
	syncs := 0
	w.syncFile = func() error { syncs++; return nil }

	if err := w.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if got := fileSize(t, w); got != 250 {
		t.Errorf("file is %d bytes after finish, want 250 (the last owned end)", got)
	}
	if syncs != 2 {
		t.Errorf("finish fsynced %d times, want 2 (before and after the truncate)", syncs)
	}
}

func TestFileWriter_FinishLeavesAFileWithNoOwnedRangeAtItsPreallocatedSize(t *testing.T) {
	w := newPreallocatedWriter(t)

	if err := w.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if got := fileSize(t, w); got != finishPrealloc {
		t.Errorf("file is %d bytes after finish with nothing owned, want %d — "+
			"it was truncated to zero", got, int64(finishPrealloc))
	}
}

func TestFileWriter_FinishTruncatesToTheEndOfASeededRange(t *testing.T) {
	w := newPreallocatedWriter(t)
	if err := w.owned.seed([]Range{{Off: 0, Len: 300}}); err != nil {
		t.Fatal(err)
	}

	if err := w.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if got := fileSize(t, w); got != 300 {
		t.Errorf("file is %d bytes after finish, want 300 (the seeded range's end)", got)
	}
}

func TestFileWriter_FinishNeverGrowsAFileShorterThanItsOwnedEnd(t *testing.T) {
	w := newTestFileWriter(t)
	if err := w.owned.seed([]Range{{Off: 0, Len: 500}}); err != nil {
		t.Fatal(err)
	}
	if err := w.handle.Truncate(200); err != nil {
		t.Fatal(err)
	}

	if err := w.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if got := fileSize(t, w); got != 200 {
		t.Errorf("file is %d bytes after finish, want 200 — finish grew it", got)
	}
}

func TestFileWriter_FinishReturnsAnFsyncErrorAndDoesNotTruncate(t *testing.T) {
	w := newPreallocatedWriter(t)
	if err := w.Accept(articleID{msgID: "a0", artIdx: 0}, 0, bytes.Repeat([]byte{1}, 100), 0); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("injected fsync failure")
	w.syncFile = func() error { return boom }

	err := w.finish()
	if !errors.Is(err, boom) {
		t.Fatalf("finish error = %v, want one wrapping %v", err, boom)
	}
	if got := fileSize(t, w); got != finishPrealloc {
		t.Errorf("file is %d bytes after a failed fsync, want %d — it was truncated anyway",
			got, int64(finishPrealloc))
	}
}

func TestOwnedRanges_MaxEnd(t *testing.T) {
	var o ownedRanges
	if got := o.maxEnd(); got != 0 {
		t.Errorf("maxEnd of an empty set = %d, want 0", got)
	}
	o.claim(Range{Off: 0, Len: 10}, articleID{artIdx: 0})
	o.claim(Range{Off: 40, Len: 10}, articleID{artIdx: 1})
	o.claim(Range{Off: 20, Len: 10}, articleID{artIdx: 2})
	if got := o.maxEnd(); got != 50 {
		t.Errorf("maxEnd = %d, want 50", got)
	}
}
