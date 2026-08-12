package assembler

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

type fileWriterOpt func(*FileWriter)

// withCacheBytes gives the writer a cache of n bytes. Zero disables caching,
// so an article is written straight through.
func withCacheBytes(n int64) fileWriterOpt {
	return func(w *FileWriter) { w.wc = newWriteCache(n) }
}

// withWriteError makes every WriteAt fail with err, injected on the writeAt
// field before first use — the same override shape diskProbe.statfs uses.
func withWriteError(err error) fileWriterOpt {
	return func(w *FileWriter) {
		w.writeAt = func([]byte, int64) (int, error) { return 0, err }
	}
}

func newTestFileWriter(t *testing.T, opts ...fileWriterOpt) *FileWriter {
	t.Helper()
	path := filepath.Join(t.TempDir(), "target.dat")
	fh, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	t.Cleanup(func() { _ = fh.Close() })
	w := newFileWriter(fh, path, fileKey{jobID: "job1", fileIdx: 0}, newWriteCache(0))
	for _, o := range opts {
		o(w)
	}
	return w
}

// TestFileWriter_DrainReportsOnlyWrittenArticles is the pin for S2. An article
// sitting in the write cache has NOT reached disk, and Drain is the barrier's
// only evidence — so a buffered-but-unwritten article must not appear in its
// return value.
func TestFileWriter_DrainReportsOnlyWrittenArticles(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(1<<20))

	// Buffer an article without triggering a contiguous flush. Offset 4096 is
	// above the cursor, so no run forms.
	if err := w.Accept(articleID{msgID: "a5", artIdx: 5}, 4096, bytes.Repeat([]byte{1}, 100)); err != nil {
		t.Fatal(err)
	}
	if got := w.writtenSoFar(); len(got) != 0 {
		t.Fatalf("writtenSoFar = %v before any drain, want empty", got)
	}

	got, err := w.Drain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ArtIdx != 5 {
		t.Fatalf("Drain = %v, want one entry for article 5", got)
	}
	if got[0].Offset != 4096 || got[0].Length != 100 {
		t.Errorf("Drain reported range {%d,%d}, want {4096,100} — the barrier "+
			"charges bytes durable from this, so a wrong extent misreports progress",
			got[0].Offset, got[0].Length)
	}
}

// TestFileWriter_FailedWriteIsNotReportedAsWritten pins the deferred-write
// failure path. Drain's contract to the barrier is the only evidence it has,
// so an article whose WriteAt failed must be absent from the report and the
// fault must come back classified.
func TestFileWriter_FailedWriteIsNotReportedAsWritten(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(1<<20), withWriteError(syscall.ENOSPC))

	if err := w.Accept(articleID{msgID: "a5", artIdx: 5}, 4096, bytes.Repeat([]byte{1}, 100)); err != nil {
		t.Fatal(err)
	}
	got, err := w.Drain(context.Background())
	if err == nil {
		t.Fatal("Drain returned nil error after ENOSPC")
	}
	var f *storagefault.Fault
	if !errors.As(err, &f) {
		t.Fatalf("Drain error = %T, want *storagefault.Fault", err)
	}
	if !f.Retryable() {
		t.Error("ENOSPC classified permanent")
	}
	for _, a := range got {
		if a.ArtIdx == 5 {
			t.Fatal("article 5 reported written although its WriteAt failed")
		}
	}
}

// TestFileWriter_DirectWriteFailureIsNotReported is the uncached half of the
// same claim. Both paths append to w.written and both must do it only below a
// successful writeAt; pinning one says nothing about the other.
func TestFileWriter_DirectWriteFailureIsNotReported(t *testing.T) {
	w := newTestFileWriter(t, withWriteError(syscall.EIO))

	err := w.Accept(articleID{msgID: "a1", artIdx: 1}, 0, bytes.Repeat([]byte{7}, 64))
	if err == nil {
		t.Fatal("Accept returned nil error after EIO on an uncached write")
	}
	var f *storagefault.Fault
	if !errors.As(err, &f) {
		t.Fatalf("Accept error = %T, want *storagefault.Fault", err)
	}
	if got := w.writtenSoFar(); len(got) != 0 {
		t.Fatalf("writtenSoFar = %v after a failed direct write, want empty", got)
	}
}

// TestFileWriter_TruncateNeverGrowsTheFile pins S6, carried over from the
// deleted resume_extent_test.go: metadata may shrink a file and never grow it.
// Growing appends zeros, which asserts content that exists nowhere, and a job
// with no par2 has no stage that would notice.
func TestFileWriter_TruncateNeverGrowsTheFile(t *testing.T) {
	w := newTestFileWriter(t)
	if err := w.Accept(articleID{msgID: "a0", artIdx: 0}, 0, []byte("0123")); err != nil {
		t.Fatal(err)
	}

	if err := w.Truncate(100 << 10); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	st, err := os.Stat(w.path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 4 {
		t.Errorf("file is %d bytes after a truncate to 100KiB, want 4 — the "+
			"truncate grew it, appending exactly the trailing zeros it exists to strip",
			st.Size())
	}
}

// TestFileWriter_TruncateShrinksToTheBound is the other direction: a real trim
// must still happen, or pre-allocation's trailing zeros survive and par2
// reports a healthy file as damaged.
func TestFileWriter_TruncateShrinksToTheBound(t *testing.T) {
	w := newTestFileWriter(t)
	if err := w.Accept(articleID{msgID: "a0", artIdx: 0}, 0, bytes.Repeat([]byte{9}, 4096)); err != nil {
		t.Fatal(err)
	}
	if err := w.Truncate(1000); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	st, err := os.Stat(w.path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 1000 {
		t.Errorf("file is %d bytes after a truncate to 1000, want 1000", st.Size())
	}
}

// TestAssembler_HasNoAckSurface pins X2 from the assembler's side.
func TestAssembler_HasNoAckSurface(t *testing.T) {
	ot := reflect.TypeFor[Options]()
	forbidden := []string{
		"MarkArticlesDone", "MarkArticlesDoneByIdx", "MarkArticlesFailed",
		"MarkArticlesFailedByIdx", "SetFileExtents",
	}
	for field := range ot.Fields() {
		if slices.Contains(forbidden, field.Name) {
			t.Errorf("Options.%s still exists — the assembler must have no ack authority", field.Name)
		}
	}
}

// TestNoSymbolNamesWriteAtDurable guards the naming trap: a state named durable
// that means only "reached WriteAt" is the conflation S2 exists to prevent, and
// leaving one would teach the next reader the bug back.
//
// This test earned its place immediately — the symbol survived the first pass
// of the cutover, in a const block between two deleted functions, and this is
// what found it. A report had already claimed it was gone.
//
// Scoped to '*.go' across the WHOLE repository, not to internal/assembler.
// Repo-wide because the name must not reappear in any package; Go files only
// because docs/ legitimately discuss the old symbol when explaining why it was
// removed, and a test that fails on prose describing history would be noise
// that trains its own suppression.
func TestNoSymbolNamesWriteAtDurable(t *testing.T) {
	// The needle is assembled at run time so this file does not contain the
	// literal and therefore cannot match itself. A self-listing grep test
	// fails forever for a reason that has nothing to do with the code it
	// guards, and the usual repair is to delete the guard.
	needle := "outcome" + "Durable"
	out, err := exec.Command("git", "grep", "-n", needle, "--", "*.go").CombinedOutput()
	if err == nil && len(out) > 0 {
		t.Errorf("%s still present; a not-durable state must not be named durable:\n%s", needle, out)
	}
}

// TestFileWriter_TakeHandsOverAndResets pins Drain's hand-off contract. take
// must clear the buffer, or every barrier re-reports every article this writer
// has ever written and the slice grows without bound.
func TestFileWriter_TakeHandsOverAndResets(t *testing.T) {
	w := newTestFileWriter(t)
	if err := w.Accept(articleID{msgID: "a1", artIdx: 1}, 0, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	first := w.take()
	if len(first) != 1 {
		t.Fatalf("take = %v, want one article", first)
	}
	if got := w.writtenSoFar(); len(got) != 0 {
		t.Fatalf("writtenSoFar = %v after take, want empty — a second barrier would re-report it forever", got)
	}
	if second := w.take(); len(second) != 0 {
		t.Errorf("take = %v on an emptied writer, want empty", second)
	}
}

// TestFileWriter_NoteWrittenCarriesTheArticlesOwnRange pins what the barrier
// charges bytes from. A run's articles are coalesced into one buffer before
// the write, so each one's own offset and length have to be carried rather
// than derived from the run — getting this wrong misreports every coalesced
// article's extent at once.
func TestFileWriter_NoteWrittenCarriesTheArticlesOwnRange(t *testing.T) {
	w := newTestFileWriter(t)
	w.noteWritten(articleID{msgID: "a3", artIdx: 3}, 512, 128)

	got := w.writtenSoFar()
	if len(got) != 1 {
		t.Fatalf("writtenSoFar = %v, want one article", got)
	}
	a := got[0]
	if a.ArtIdx != 3 || a.Offset != 512 || a.Length != 128 {
		t.Errorf("WrittenArticle = %+v, want {ArtIdx:3 Offset:512 Length:128}", a)
	}
	if a.FileIdx != 0 {
		t.Errorf("FileIdx = %d, want 0 — the barrier places the article in the wrong file's bitmap otherwise", a.FileIdx)
	}
}

// TestFileWriter_WriteOneFailureClearsSeenDone pins the seen-set correction on
// a failed write. seenDone means "accepted and counted"; after the write is
// lost the article is still counted but is no longer on its way to disk, and
// leaving it there makes a re-delivery look like a duplicate to skip.
func TestFileWriter_WriteOneFailureClearsSeenDone(t *testing.T) {
	w := newTestFileWriter(t, withWriteError(syscall.EIO))
	w.seenDone["a9"] = 0

	if err := w.writeOne(bufferedArticle{offset: 0, data: []byte("xy"), id: articleID{msgID: "a9", artIdx: 9}}); err == nil {
		t.Fatal("writeOne returned nil after EIO")
	}
	if _, still := w.seenDone["a9"]; still {
		t.Error("a9 is still in seenDone after its write failed; a re-delivery would be skipped as a duplicate")
	}
	if _, failed := w.seenFailed["a9"]; !failed {
		t.Error("a9 was not moved to seenFailed")
	}
}
