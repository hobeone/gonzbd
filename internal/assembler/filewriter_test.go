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
// It walks the working tree with grep rather than asking git, because git grep
// sees only TRACKED files: a reintroduction in a file not yet added would pass
// a git-based check and then land with the commit that adds it.
//
// Scoped to Go sources across the whole repository. Repo-wide because the name
// must not reappear in any package; Go-only because docs/ legitimately discuss
// the old symbol when explaining why it was removed, and a test that fails on
// prose describing history is noise that trains its own suppression.
func TestNoSymbolNamesWriteAtDurable(t *testing.T) {
	// The needle is assembled at run time so this file does not contain the
	// literal and therefore cannot match itself. A self-listing grep test
	// fails forever for a reason that has nothing to do with the code it
	// guards, and the usual repair is to delete the guard.
	needle := "outcome" + "Durable"
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	out, err := exec.Command("grep", "-rn", "--include=*.go", needle, root).CombinedOutput()
	if err == nil && len(out) > 0 {
		t.Errorf("%s still present; a not-durable state must not be named durable:\n%s", needle, out)
	}
}

// repoRoot returns the module root, so the grep above covers every package
// rather than only the one the test happens to run in.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

// TestFileWriter_TakeReportsUntilTheCycleIsConfirmed pins the two halves of the
// hand-off separately, because they fail in opposite directions.
//
// take must move the new articles out of w.written, or an article is reported
// twice within one unconfirmed window and the report grows per barrier. And it
// must NOT drop the unconfirmed set, or a barrier whose Sync failed strands
// articles that no later Drain will ever mention again — which for a completed
// file is the bound FinalizeFile trims to, sitting below real bytes.
//
// The Sync is the only thing that may discard the set, so it is asserted last
// and on its own.
func TestFileWriter_TakeReportsUntilTheCycleIsConfirmed(t *testing.T) {
	w := newTestFileWriter(t)
	if err := w.Accept(articleID{msgID: "a1", artIdx: 1}, 0, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	first := w.take()
	if len(first) != 1 {
		t.Fatalf("take = %v, want one article", first)
	}
	if got := w.writtenSoFar(); len(got) != 0 {
		t.Errorf("writtenSoFar = %v after take, want empty — the article would be reported "+
			"twice within one window and the report would grow per barrier", got)
	}
	if second := w.take(); len(second) != 1 {
		t.Errorf("take = %v with no Sync in between, want the same one article — a barrier "+
			"whose Sync failed would otherwise strand it with no ack able to reach it", second)
	}
	if err := w.Sync(t.Context()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := w.unconfirmed(); len(got) == 0 {
		t.Error("the report was released by the fsync. The barrier's commit and ack " +
			"follow it and can both fail, and a release here leaves the retry with " +
			"nothing to re-report")
	}
	w.Confirm()
	if got := w.unconfirmed(); len(got) != 0 {
		t.Errorf("unconfirmed = %v after Confirm, want empty — the cycle landed, so "+
			"keeping them grows the report without bound", got)
	}
	if third := w.take(); len(third) != 0 {
		t.Errorf("take = %v after Confirm, want empty", third)
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

// TestFileWriter_TruncateIgnoresANegativeBound pins the degenerate input. A
// negative bound reaches Truncate only from a derivation that produced
// nonsense, and os.File.Truncate would return an error for it; treating it as
// "nothing to do" keeps a completed file intact rather than failing the job
// over an arithmetic result nothing can act on.
func TestFileWriter_TruncateIgnoresANegativeBound(t *testing.T) {
	w := newTestFileWriter(t)
	if err := w.Accept(articleID{msgID: "a0", artIdx: 0}, 0, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if err := w.Truncate(-1); err != nil {
		t.Fatalf("Truncate(-1) = %v, want nil", err)
	}
	st, err := os.Stat(w.path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 4 {
		t.Errorf("file is %d bytes after Truncate(-1), want 4 unchanged", st.Size())
	}
}

// TestFileWriter_DrainOnACancelledContextClaimsNothing pins B4's shape at the
// writer: a cancelled context must stop the drain and report a storage fault,
// never return the buffered articles as though their bytes had landed.
func TestFileWriter_DrainOnACancelledContextClaimsNothing(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(1<<20))
	if err := w.Accept(articleID{msgID: "a1", artIdx: 1}, 4096, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := w.Drain(ctx)
	if err == nil {
		t.Fatal("Drain returned nil error on a cancelled context")
	}
	var f *storagefault.Fault
	if !errors.As(err, &f) {
		t.Fatalf("Drain error = %T, want *storagefault.Fault", err)
	}
	for _, a := range got {
		if a.ArtIdx == 1 {
			t.Fatal("article 1 reported written although the drain was cancelled before its write")
		}
	}
}

// TestFileWriter_StatOnAClosedHandleIsAStorageFault pins Stat's failure branch.
// The barrier reads this pair as the S7 validity stamp, so a failure must come
// back classified rather than as a zero size that would look like an empty file.
func TestFileWriter_StatOnAClosedHandleIsAStorageFault(t *testing.T) {
	w := newTestFileWriter(t)
	if err := w.handle.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, err := w.Stat()
	if err == nil {
		t.Fatal("Stat on a closed handle returned nil error; a zero size would read as an empty file")
	}
	var f *storagefault.Fault
	if !errors.As(err, &f) {
		t.Fatalf("Stat error = %T, want *storagefault.Fault", err)
	}
}

// TestFileWriter_DrainReleasesUnattemptedBuffersOnFailure pins the pooling half
// of Drain's error path. Everything after the failing write was never attempted
// and still holds a pooled buffer; leaking those costs one decoder buffer per
// article for the rest of the drain.
func TestFileWriter_DrainReleasesUnattemptedBuffersOnFailure(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(1<<20), withWriteError(syscall.ENOSPC))
	for i, off := range []int64{4096, 8192, 12288} {
		if err := w.Accept(articleID{msgID: string(rune('a' + i)), artIdx: int32(i)}, off, make([]byte, 64)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := w.Drain(context.Background())
	if err == nil {
		t.Fatal("Drain returned nil after ENOSPC")
	}
	if len(got) != 0 {
		t.Errorf("Drain reported %d articles written although every WriteAt failed", len(got))
	}
}

// TestFileWriter_CoalescedRunWriteFailureReportsNothing pins flushRun's error
// branch. A coalesced run is one WriteAt for many articles, so a failure loses
// all of their bytes at once — reporting any of them would claim bytes the file
// does not have for articles whose originals are already pooled.
func TestFileWriter_CoalescedRunWriteFailureReportsNothing(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(4<<20), withWriteError(syscall.EIO))

	// Fill a contiguous run past contiguousRunSize so a flush actually fires.
	const chunk = 64 << 10
	var off int64
	var lastErr error
	for i := range 12 {
		lastErr = w.Accept(articleID{msgID: string(rune('a' + i)), artIdx: int32(i)}, off, make([]byte, chunk))
		off += chunk
		if lastErr != nil {
			break
		}
	}
	if lastErr == nil {
		t.Fatal("no coalesced flush fired; the fixture never reached contiguousRunSize")
	}
	var f *storagefault.Fault
	if !errors.As(lastErr, &f) {
		t.Fatalf("run write error = %T, want *storagefault.Fault", lastErr)
	}
	if got := w.writtenSoFar(); len(got) != 0 {
		t.Errorf("writtenSoFar = %d articles after the run's WriteAt failed, want 0", len(got))
	}
}

// TestFileWriter_CoalescedRunReportsEveryArticlesOwnRange pins the success half
// of coalescing, which is the write cache's entire purpose: many articles
// become one WriteAt, and each must still be reported with its OWN offset and
// length.
//
// The ranges cannot be recovered from the coalesced buffer — it is flat, and
// the originals are pooled before the write — so they are carried on runPart.
// Getting that wrong misreports every article in the run at once, and the
// barrier charges durable bytes from exactly these numbers.
func TestFileWriter_CoalescedRunReportsEveryArticlesOwnRange(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(4<<20))

	const chunk = 64 << 10
	const n = 12 // 768 KiB, past contiguousRunSize (512 KiB)
	for i := range n {
		if err := w.Accept(
			articleID{msgID: string(rune('a' + i)), artIdx: int32(i)}, //nolint:gosec // G115: loop bound is 12
			int64(i)*chunk, make([]byte, chunk),
		); err != nil {
			t.Fatalf("Accept %d: %v", i, err)
		}
	}

	got := w.writtenSoFar()
	if len(got) == 0 {
		t.Fatal("no coalesced flush fired; the fixture never reached contiguousRunSize")
	}
	for _, a := range got {
		wantOff := int64(a.ArtIdx) * chunk
		if a.Offset != wantOff {
			t.Errorf("article %d reported at offset %d, want %d — the run's ranges were "+
				"derived from the coalesced buffer instead of carried per article",
				a.ArtIdx, a.Offset, wantOff)
		}
		if a.Length != chunk {
			t.Errorf("article %d reported length %d, want %d", a.ArtIdx, a.Length, chunk)
		}
	}
	// The bytes must actually be on disk at those offsets, not merely claimed.
	st, err := os.Stat(w.path)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(len(got)) * chunk; st.Size() < want {
		t.Errorf("file is %d bytes but %d articles were reported written", st.Size(), len(got))
	}
}

// TestFileWriter_SyncDoesNotDiscardAnArticleNoDrainReported pins the split
// between w.written and w.reported, which is the half the earlier pin missed.
//
// Folding the two — having Sync clear both — left every package green, and it
// is the exact loss this writer's own doc calls out: an article accepted
// BETWEEN a Drain and its Sync is covered by that fsync but was never handed
// to the barrier, so nothing can ever ack it. It stays Outstanding for a file
// the assembler has already tombstoned, which no re-fetch can reach.
//
// Sequenced deliberately: a1 is reported and then CONFIRMED, a2 arrives inside
// the window, and only the second Drain may mention a2.
//
// The Confirm is load-bearing rather than ceremony. Sync used to release the
// report itself, which lost it to any failure between the fsync and the
// barrier's commit; the release moved to Confirm, which the barrier calls only
// once the commit and the ack have both landed. a2 is untouched by it because
// it was never reported — the split between w.written and w.reported is
// exactly what keeps the two apart.
func TestFileWriter_SyncDoesNotDiscardAnArticleNoDrainReported(t *testing.T) {
	w := newTestFileWriter(t)
	if err := w.Accept(articleID{msgID: "a1", artIdx: 1}, 0, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if first := w.take(); len(first) != 1 || first[0].ArtIdx != 1 {
		t.Fatalf("first take = %v, want article 1 alone", first)
	}

	// Accepted after the Drain that reported a1, before the Sync that
	// confirms it. The worker handles Drain and Sync as two separate control
	// messages, so a write can land between them.
	if err := w.Accept(articleID{msgID: "a2", artIdx: 2}, 4, []byte("efgh")); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The barrier's commit and ack have landed, so the reported set may go.
	w.Confirm()

	second := w.take()
	if len(second) != 1 {
		t.Fatalf("second take = %v, want exactly article 2 — a1 was released by the "+
			"Confirm and a2 was never reported", second)
	}
	if second[0].ArtIdx != 2 {
		t.Errorf("second take = %v, want article 2: the Sync discarded an article no Drain had "+
			"reported, so no ack can ever reach it and it stays Outstanding on a file the "+
			"assembler has already tombstoned", second)
	}
}
