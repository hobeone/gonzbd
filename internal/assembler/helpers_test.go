package assembler

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// The helpers exercised here are reached in production only through
// processRequest and the worker loop, which is why the end-to-end tests cover
// their behaviour without naming them. These call them directly, so a change
// to one of them fails against its own contract rather than against whichever
// pipeline test happened to route through it.

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
		key:  key,
	}
}

func TestOffsetInRange(t *testing.T) {
	a := newHelperAssembler()
	dir := t.TempDir()

	t.Run("rejects a negative offset", func(t *testing.T) {
		f := newHelperFile(t, dir, "neg.dat", 100)
		if a.offsetInRange(f, WriteRequest{Offset: -1, Data: []byte("x")}) {
			t.Error("accepted a negative offset")
		}
	})

	t.Run("rejects an offset+length that overflows int64", func(t *testing.T) {
		f := newHelperFile(t, dir, "ovf.dat", 100)
		const maxInt64 = int64(^uint64(0) >> 1)
		if a.offsetInRange(f, WriteRequest{Offset: maxInt64 - 1, Data: []byte("xxxx")}) {
			t.Error("accepted an offset whose end wraps negative")
		}
	})

	t.Run("accepts anything in range when no size was declared", func(t *testing.T) {
		// ExpectedSize == 0 means the NZB declared no size, so the bound
		// cannot be enforced and only the two checks above apply.
		f := newHelperFile(t, dir, "nosize.dat", 0)
		if !a.offsetInRange(f, WriteRequest{Offset: 1 << 40, Data: []byte("xxxx")}) {
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
		if !a.offsetInRange(f, WriteRequest{Offset: 896, Data: []byte("abcd")}) {
			t.Error("rejected a write ending exactly at the slack limit")
		}
	})

	t.Run("rejects a write ending one byte past the slack limit", func(t *testing.T) {
		f := newHelperFile(t, dir, "over.dat", 800)
		if a.offsetInRange(f, WriteRequest{Offset: 897, Data: []byte("abcd")}) {
			t.Error("accepted a write ending past the slack limit")
		}
	})

	t.Run("accepts a write when the slack arithmetic would overflow", func(t *testing.T) {
		// A degenerate ExpectedSize must not make the bound itself wrap; the
		// helper gives up on the bound rather than rejecting a real write.
		const maxInt64 = int64(^uint64(0) >> 1)
		f := newHelperFile(t, dir, "slackovf.dat", maxInt64-8)
		if !a.offsetInRange(f, WriteRequest{Offset: 0, Data: []byte("abcd")}) {
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
	f2.key = fileKey{jobID: "job", fileIdx: 1}
	open := map[fileKey]*openFile{f1.key: f1, f2.key: f2}

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
	good.key = fileKey{jobID: "job", fileIdx: 1}

	a.drainAndCloseAll(map[fileKey]*openFile{bad.key: bad, good.key: good})

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
		open := map[fileKey]*openFile{f.key: f}

		for i := range 5 {
			wc.buffer(f.key, bufferedArticle{
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
		wc.buffer(f.key, bufferedArticle{offset: 0, data: []byte("abcd"), id: articleID{msgID: "<a@x>"}})
		a.relievePressure(wc, map[fileKey]*openFile{f.key: f})
		if !wc.buffered(f.key, 0) {
			t.Error("relievePressure flushed an article while under the threshold")
		}
	})
}
