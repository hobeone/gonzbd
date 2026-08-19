package assembler

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The helpers exercised here are reached in production only through
// processRequest and the worker loop, which is why the end-to-end tests cover
// their behaviour without naming them. These call them directly, so a change
// to one of them fails against its own contract rather than against whichever
// pipeline test happened to route through it.

// writeArticle submits req under the identity carried on req itself, so a
// fixture stays one readable literal instead of being split into a ref and a
// payload at every call site.
//
// Deriving the ref from the request is safe HERE and would not be safe in
// production. ArticleRef exists so that a caller OUTSIDE this package cannot
// reach the worker with an identity nobody supplied; tests are inside it and
// could always construct any request they liked, so nothing is being smuggled
// past the owner. internal/app's call sites, which are the ones the guarantee
// is about, pass a real ref.
//
// The cost is that these call sites no longer see WriteArticle's signature, so
// a change to ArticleRef's shape will not break them. That is deliberate: their
// subject is the assembler's behaviour, not its parameter list.
func writeArticle(ctx context.Context, a *Assembler, req WriteRequest) error {
	return a.WriteArticle(ctx, ArticleRef{
		JobID:     req.JobID,
		FileIdx:   req.FileIdx,
		ArtIdx:    req.ArtIdx,
		MessageID: req.MessageID,
	}, req)
}

// TestWriteArticle_RefOverridesTheRequestsOwnIdentity pins the sentence
// WriteArticle's doc comment asserts: the ref is authoritative and the
// request's own identity fields are not read on this path.
//
// Nothing else exercises it. Every other call site passes an identity that
// already agrees with the ref — the writeArticle helper derives one from the
// other, and both internal/app sites build them from the same ArticleResult —
// so the overwrite could be deleted entirely and the whole suite would stay
// green while the doc comment kept claiming it.
//
// The fixture makes the two disagree on all four fields. If the request won,
// the write would be routed to a job and file that were never registered and
// the file under test would never complete.
func TestWriteArticle_RefOverridesTheRequestsOwnIdentity(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	path := registerFile(t, dir, files, "real-job", 0, 1)

	completed := make(chan int, 1)
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(_ string, fileIdx int) { completed <- fileIdx }
	a := startAssembler(t, opts)

	err := a.WriteArticle(t.Context(), ArticleRef{
		JobID: "real-job", FileIdx: 0, ArtIdx: 3, MessageID: "real@id",
	}, WriteRequest{
		// Every identity field here is wrong, and deliberately so.
		JobID: "ghost-job", FileIdx: 9, ArtIdx: 99, MessageID: "ghost@id",
		Offset: 0, Data: []byte("AAAA"),
	})
	if err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}

	select {
	case got := <-completed:
		if got != 0 {
			t.Errorf("completed file %d, want 0", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the file never completed, so the request's identity was used " +
			"instead of the ref's: the write went to a job and file that were " +
			"never registered")
	}

	if got := readFile(t, path); string(got) != "AAAA" {
		t.Errorf("file contents = %q, want %q", got, "AAAA")
	}
}

// testArtIdx narrows a loop counter to the ArtIdx field's type.
//
// It exists so the conversion's lint suppression is written once rather than at
// every fixture that numbers its articles from a loop variable. Test article
// counts are small by construction, so the narrowing cannot lose information.
func testArtIdx(i int) int32 {
	return int32(i) //nolint:gosec // G115: test article counts are far below int32
}

// newHelperAssembler builds an Assembler as a literal, without Start, for
// tests that drive one helper. Nothing here reads a.reqs or the worker state.
func newHelperAssembler() *Assembler {
	return &Assembler{log: slog.Default()}
}

// newHelperFile opens a real file under dir and wraps it in an openFile.
// A real handle rather than a stub: closeAll and the write helpers call
// through to the os layer, and a stub would pin the mock instead.
func newHelperFile(t *testing.T, dir, name string, expectedSize int64) *openFile {
	t.Helper()
	path := filepath.Join(dir, name)
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = fh.Close() })
	key := fileKey{jobID: "job", fileIdx: 0}
	return &openFile{
		w:    newFileWriter(fh, path, key, newWriteCache(0)),
		info: FileInfo{Path: path, ExpectedSize: expectedSize},
	}
}

func TestOffsetInRange(t *testing.T) {
	a := newHelperAssembler()
	dir := t.TempDir()

	t.Run("rejects a negative offset", func(t *testing.T) {
		f := newHelperFile(t, dir, "neg.dat", 100)
		if offsetOK(a, f, WriteRequest{Offset: -1, Data: []byte("x")}) {
			t.Error("accepted a negative offset")
		}
	})

	t.Run("rejects an offset+length that overflows int64", func(t *testing.T) {
		f := newHelperFile(t, dir, "ovf.dat", 100)
		const maxInt64 = int64(^uint64(0) >> 1)
		if offsetOK(a, f, WriteRequest{Offset: maxInt64 - 1, Data: []byte("xxxx")}) {
			t.Error("accepted an offset whose end wraps negative")
		}
	})

	t.Run("accepts anything in range when no size was declared", func(t *testing.T) {
		// ExpectedSize == 0 means the NZB declared no size, so the bound
		// cannot be enforced and only the two checks above apply.
		f := newHelperFile(t, dir, "nosize.dat", 0)
		if !offsetOK(a, f, WriteRequest{Offset: 1 << 40, Data: []byte("xxxx")}) {
			t.Error("rejected a write against a file with no declared size")
		}
	})

	t.Run("accepts a write ending exactly at the slack limit", func(t *testing.T) {
		// The bound is ExpectedSize plus ExpectedSize/offsetSlackDivisor, not
		// ExpectedSize itself: 800 + 100 here. Asserting on the limit rather
		// than on ExpectedSize is what makes the rejection below meaningful —
		// a test that only probed past ExpectedSize would pass against a
		// version with no bound at all up to the slack.
		f := newHelperFile(t, dir, "exact.dat", 800)
		if !offsetOK(a, f, WriteRequest{Offset: 896, Data: []byte("abcd")}) {
			t.Error("rejected a write ending exactly at the slack limit")
		}
	})

	t.Run("rejects a write ending one byte past the slack limit", func(t *testing.T) {
		f := newHelperFile(t, dir, "over.dat", 800)
		if offsetOK(a, f, WriteRequest{Offset: 897, Data: []byte("abcd")}) {
			t.Error("accepted a write ending past the slack limit")
		}
	})

	t.Run("accepts a write when the slack arithmetic would overflow", func(t *testing.T) {
		// A degenerate ExpectedSize must not make the bound itself wrap; the
		// helper gives up on the bound rather than rejecting a real write.
		const maxInt64 = int64(^uint64(0) >> 1)
		f := newHelperFile(t, dir, "slackovf.dat", maxInt64-8)
		if !offsetOK(a, f, WriteRequest{Offset: 0, Data: []byte("abcd")}) {
			t.Error("rejected a write because the slack limit overflowed")
		}
	})
}

func TestCloseAll(t *testing.T) {
	// Closing on worker exit must not fire completion callbacks: a partial
	// file is not a completed one, and downstream treats OnFileComplete as
	// permission to start post-processing.
	dir := t.TempDir()
	a := newHelperAssembler()
	var completions int
	a.opts.OnFileComplete = func(string, int) { completions++ }

	f1 := newHelperFile(t, dir, "close1.dat", 0)
	f2 := newHelperFile(t, dir, "close2.dat", 0)
	f2.w.key = fileKey{jobID: "job", fileIdx: 1}
	open := map[fileKey]*openFile{f1.w.key: f1, f2.w.key: f2}

	a.drainAndCloseAll(open)

	if completions != 0 {
		t.Errorf("closeAll fired %d completion callbacks, want 0", completions)
	}
	// A second Close returns an error on an already-closed handle, which is
	// how this asserts the first one landed.
	if err := f1.w.handle.Close(); err == nil {
		t.Error("f1 was still open after closeAll")
	}
	if err := f2.w.handle.Close(); err == nil {
		t.Error("f2 was still open after closeAll")
	}
}

func TestCloseAll_ContinuesPastAFailedClose(t *testing.T) {
	// One bad handle must not strand the rest open for the process's lifetime.
	dir := t.TempDir()
	a := newHelperAssembler()
	bad := newHelperFile(t, dir, "bad.dat", 0)
	_ = bad.w.handle.Close() // make its Close in closeAll fail
	good := newHelperFile(t, dir, "good.dat", 0)
	good.w.key = fileKey{jobID: "job", fileIdx: 1}

	a.drainAndCloseAll(map[fileKey]*openFile{bad.w.key: bad, good.w.key: good})

	if err := good.w.handle.Close(); err == nil {
		t.Error("the second handle was left open after the first failed to close")
	}
}

func TestRelievePressure(t *testing.T) {
	dir := t.TempDir()

	t.Run("flushes until the cache is back under the threshold", func(t *testing.T) {
		a := newHelperAssembler()
		// pressure() trips above 90% of the limit.
		wc := newWriteCache(100)
		f := newHelperFile(t, dir, "press.dat", 0)
		open := map[fileKey]*openFile{f.w.key: f}

		for i := range 5 {
			wc.buffer(f.w.key, bufferedArticle{
				offset: int64(i * 20),
				data:   []byte("12345678901234567890"),
				id:     articleID{msgID: "<x@y>", artIdx: int32(i)},
			})
		}
		if !wc.pressure() {
			t.Fatal("fixture did not put the cache under pressure; the test would pass vacuously")
		}

		a.relievePressure(wc, open)

		if wc.pressure() {
			t.Errorf("still under pressure after relievePressure: used=%d limit=%d", wc.used, wc.limit)
		}
		// The flushed bytes must actually have reached the file, not just left
		// the cache's accounting.
		st, err := f.w.handle.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() == 0 {
			t.Error("relievePressure emptied the cache without writing anything")
		}
	})

	t.Run("is a no-op when there is no pressure", func(t *testing.T) {
		a := newHelperAssembler()
		wc := newWriteCache(1 << 20)
		f := newHelperFile(t, dir, "nopress.dat", 0)
		wc.buffer(f.w.key, bufferedArticle{offset: 0, data: []byte("abcd"), id: articleID{msgID: "<a@x>"}})
		a.relievePressure(wc, map[fileKey]*openFile{f.w.key: f})
		if !wc.buffered(f.w.key, 0) {
			t.Error("relievePressure flushed an article while under the threshold")
		}
	})
}

// TestDrainAndClose_FlushesBeforeClosing pins the shutdown ordering. A file
// closed with articles still buffered loses their bytes silently: they were
// never written, so no Drain ever reported them, and the queue is told nothing
// in either direction.
func TestDrainAndClose_FlushesBeforeClosing(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	f := newHelperFile(t, dir, "drainclose.dat", 0)
	f.w.wc = newWriteCache(1 << 20)

	if err := f.w.Accept(articleID{msgID: "a0", artIdx: 0}, 0, []byte("abcdefgh")); err != nil {
		t.Fatal(err)
	}
	// Still buffered: nothing has reached the file yet.
	if st, err := os.Stat(f.info.Path); err == nil && st.Size() != 0 {
		t.Fatalf("fixture wrote through instead of buffering; size=%d", st.Size())
	}

	a.drainAndClose(f)

	st, err := os.Stat(f.info.Path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 8 {
		t.Errorf("file is %d bytes after drainAndClose, want 8 — buffered bytes were dropped on close", st.Size())
	}
}

// TestAcceptArticle_OutOfRangeOffsetIsNotReported pins the security check's
// effect on the barrier's evidence. offsetOutOfRange rejects an attacker-supplied
// yEnc offset; the article must then be absent from what Drain reports, or the
// barrier acks an article whose bytes were deliberately never written.
func TestAcceptArticle_OutOfRangeOffsetIsNotReported(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	f := newHelperFile(t, dir, "oor.dat", 1024)

	a.acceptArticle(f, articleID{msgID: "bad", artIdx: 4}, WriteRequest{
		JobID: "job", FileIdx: 0, ArtIdx: 4, MessageID: "bad",
		Offset: 1 << 40, Data: []byte("evil"),
	})

	if got := f.w.writtenSoFar(); len(got) != 0 {
		t.Errorf("writtenSoFar = %v after a rejected offset, want empty", got)
	}
	if _, failed := f.w.seenFailed["bad"]; !failed {
		t.Error("the rejected article was not moved to seenFailed")
	}
}

// TestHandleLateDuplicate_ReturnsTheBufferAndClaimsNothing pins what is left
// of the late-duplicate path once the acks are gone: it must release the
// decoder buffer (or the pool leaks one per late article) and must assert
// nothing about the article, since the file it belonged to is already closed.
//
// The body had NO assertion of any kind and could only fail by panicking, so
// deleting decoder.PutBuffer from handleLateDuplicate — the exact leak the doc
// names — left it green. It now observes the release, via the pool itself: a
// buffer obtained from GetBuffer, handed to handleLateDuplicate and then
// re-requested must come back as the SAME backing array. That is a real
// observation of the Put, not a proxy for it.
//
// Bounded claim: sync.Pool may drop entries at any time, so a Get returning a
// DIFFERENT array does not by itself prove the Put was missing. The assertion
// therefore runs only when the pool hands something back, and the second case
// (FatalErr, nil Data) asserts the complementary thing — that a request with no
// buffer does not put a nil or foreign slice into the pool.
// TestHandleLateDuplicate_ReturnsTheBufferAndClaimsNothing pins what is left
// of the late-duplicate path once the acks are gone: it must release the
// decoder buffer (or the pool leaks one per late article) and must assert
// nothing about the article, since the file it belonged to is already closed.
//
// # Why this observes the CALL and not the pool
//
// Two earlier versions of this test were wrong, in instructive ways.
//
// The first had no assertion at all and could only fail by panicking, so
// deleting the release left it green.
//
// The second put a real buffer in and swept decoder's pool for it. That is
// unobservable, not merely flaky: decoder's pool is a process-global
// sync.Pool, and sync.Pool is EMPTIED AT EVERY GC. A collection landing between
// the release and the sweep loses the buffer and the test reports a leak that
// never happened — measured at 4 failures in 12 package runs under -race, with
// the production code correct. Raising the retry count makes it worse, since
// each extra iteration is another window for a GC. A probe confirmed the
// mechanism directly: runtime.GC() between Put and Get loses the buffer 100% of
// the time.
//
// So the assertion moved to the seam. a.putBuffer records the release, which is
// exactly the fact under test, and it depends on no global state and no
// collector timing.
func TestHandleLateDuplicate_ReturnsTheBufferAndClaimsNothing(t *testing.T) {
	a := newHelperAssembler()
	var released [][]byte
	a.putBuffer = func(b []byte) { released = append(released, b) }

	data := []byte("xyz")
	// nil writer: the buffer must come back whether or not there is a writer
	// left to consult about the article's state.
	a.handleLateDuplicate(nil, WriteRequest{
		JobID: "job", FileIdx: 0, ArtIdx: 2, MessageID: "late", Data: data,
	})

	if len(released) != 1 {
		t.Fatalf("released %d buffers, want exactly 1; the late-duplicate path "+
			"leaks one decoder buffer per late article", len(released))
	}
	// Identity, not just count: releasing some other slice would satisfy a
	// bare count and still leak the article's buffer.
	if &released[0][:1][0] != &data[:1][0] {
		t.Errorf("released a different buffer than the request carried")
	}

	// A late duplicate carrying no data must release nothing — passing a nil
	// slice to the pool is not the same as not calling it. Without this the
	// FatalErr arm has no coverage at all.
	released = nil
	a.handleLateDuplicate(nil, WriteRequest{
		JobID: "job", FileIdx: 0, ArtIdx: 2, MessageID: "late", FatalErr: os.ErrClosed,
	})
	if len(released) != 0 {
		t.Errorf("released %d buffers for a data-less late duplicate, want 0", len(released))
	}
}

// TestReleaseBuffer_FallsBackToTheDecoderPool pins the nil-hook path, which is
// what every production Assembler built outside New() uses. Without it the seam
// could be left nil-only-safe by accident and silently stop releasing.
func TestReleaseBuffer_FallsBackToTheDecoderPool(t *testing.T) {
	// The hook, when set, receives exactly the slice handed in. This is the
	// half that is observable; see below for the half that is not.
	a := newHelperAssembler()
	if a.putBuffer != nil {
		t.Fatal("newHelperAssembler set putBuffer; this test needs the nil path")
	}
	buf := make([]byte, 8)
	var seen [][]byte
	a.putBuffer = func(b []byte) { seen = append(seen, b) }
	a.releaseBuffer(buf)
	if len(seen) != 1 || &seen[0][:1][0] != &buf[:1][0] {
		t.Fatalf("releaseBuffer delivered %d buffers to the hook, want exactly the one passed in", len(seen))
	}

	// With a nil hook the only observable is that it does not panic — whether
	// the slice reached decoder's pool is NOT checkable, because sync.Pool is
	// emptied at every GC (see TestHandleLateDuplicate for the measurement
	// that established this). So the fallback is pinned negatively, by the
	// assertion below that production never takes it.
	a.putBuffer = nil
	a.releaseBuffer(buf)
	a.releaseBuffer(nil)

	// The load-bearing assertion: an Assembler built the production way has a
	// non-nil hook, so the nil path above is reachable only from tests that
	// construct the struct directly. Without this, New() could stop setting
	// putBuffer and nothing would notice.
	prod := New(Options{FileInfo: func(string, int) (FileInfo, error) { return FileInfo{}, nil }}, nil)
	if prod.putBuffer == nil {
		t.Error("New() left putBuffer nil; production would take the fallback path, " +
			"which no test can observe")
	}
}

// TestControlOps_CancelledContextBeatsEveryOtherOutcome is the DETERMINISTIC
// half of the cancelled-context contract.
//
// TestAssembler_CloseJobHandles_ContextCanceled asserts the same rule against a
// started assembler, but it can only ever be a probabilistic pin: without the
// up-front ctx.Err() check both selects have ctx.Done() ready and Go picks
// among ready cases at random, so that test passed ~95% of the time against the
// broken code and failed about 1 package run in 20 under -race.
//
// This one cannot be probabilistic. On a NOT-STARTED assembler the two possible
// answers are distinguishable with no scheduling involved at all: the ctx check
// yields context.Canceled, the started check yields ErrNotStarted. Asserting
// context.Canceled therefore pins the ORDER of the two checks, which is the
// thing the fix actually established.
func TestControlOps_CancelledContextBeatsEveryOtherOutcome(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	for name, call := range map[string]func(*Assembler) error{
		"CloseJobHandles": func(a *Assembler) error { return a.CloseJobHandles(ctx, "job1") },
		"CancelJob":       func(a *Assembler) error { return a.CancelJob(ctx, "job1") },
	} {
		t.Run(name, func(t *testing.T) {
			// Never started, so ErrNotStarted is the answer if the ctx check
			// does not come first.
			a := &Assembler{log: slog.Default()}
			if err := call(a); !errors.Is(err, context.Canceled) {
				t.Errorf("%s with a cancelled context = %v, want context.Canceled; "+
					"an already-cancelled context must be honoured before any other "+
					"state check and before any work is enqueued", name, err)
			}
		})
	}
}

// offsetOK inverts offsetOutOfRange for TestOffsetInRange's assertions, which
// read as "this offset is acceptable" rather than "this offset is not out of
// range". The reason string the real signature also returns is exercised where
// it is consumed, in TestRejectedOffsetIsNotCountedAndIsReported.
func offsetOK(a *Assembler, f *openFile, req WriteRequest) bool {
	_, bad := a.offsetOutOfRange(f, req)
	return !bad
}
