package assembler

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
	return &openFile{
		handle:   fh,
		info:     FileInfo{Path: path, ExpectedSize: expectedSize},
		key:      fileKey{jobID: "job", fileIdx: 0},
		crcValid: true,
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

func TestRecordPendingDoneAndFailed(t *testing.T) {
	t.Run("routes to the index maps when the ByIdx callbacks are set", func(t *testing.T) {
		a := newHelperAssembler()
		a.opts.MarkArticlesDoneByIdx = func(string, []int32) error { return nil }
		a.opts.MarkArticlesFailedByIdx = func(string, []int32) ([]int32, error) { return nil, nil }
		// Both message-ID callbacks are set too, to pin the precedence: the
		// index form wins when both are available.
		a.opts.MarkArticlesDone = func(string, []string) error { return nil }
		a.opts.MarkArticlesFailed = func(string, []string) ([]string, error) { return nil, nil }

		a.recordPendingDone("job", "<a@x>", 7)
		a.recordPendingFailed("job", "<b@x>", 9)

		if got := a.pendingDoneByIdx["job"]; !slices.Equal(got, []int32{7}) {
			t.Errorf("pendingDoneByIdx = %v, want [7]", got)
		}
		if got := a.pendingFailedByIdx["job"]; !slices.Equal(got, []int32{9}) {
			t.Errorf("pendingFailedByIdx = %v, want [9]", got)
		}
		if len(a.pendingDone) != 0 || len(a.pendingFailed) != 0 {
			t.Errorf("message-ID maps were also written: done=%v failed=%v", a.pendingDone, a.pendingFailed)
		}
	})

	t.Run("falls back to the message-ID maps", func(t *testing.T) {
		a := newHelperAssembler()
		a.opts.MarkArticlesDone = func(string, []string) error { return nil }
		a.opts.MarkArticlesFailed = func(string, []string) ([]string, error) { return nil, nil }

		a.recordPendingDone("job", "<a@x>", 7)
		a.recordPendingFailed("job", "<b@x>", 9)

		if got := a.pendingDone["job"]; !slices.Equal(got, []string{"<a@x>"}) {
			t.Errorf("pendingDone = %v, want [<a@x>]", got)
		}
		if got := a.pendingFailed["job"]; !slices.Equal(got, []string{"<b@x>"}) {
			t.Errorf("pendingFailed = %v, want [<b@x>]", got)
		}
	})

	t.Run("records nothing when no callback is wired", func(t *testing.T) {
		// Without a sink there is nobody to flush to, and accumulating would
		// grow both maps for the lifetime of the process.
		a := newHelperAssembler()
		a.recordPendingDone("job", "<a@x>", 7)
		a.recordPendingFailed("job", "<b@x>", 9)
		if len(a.pendingDone)+len(a.pendingFailed)+len(a.pendingDoneByIdx)+len(a.pendingFailedByIdx) != 0 {
			t.Error("recorded an ack with no callback to deliver it to")
		}
	})

	t.Run("appends rather than replacing, and keys by job", func(t *testing.T) {
		a := newHelperAssembler()
		a.opts.MarkArticlesDone = func(string, []string) error { return nil }
		a.recordPendingDone("j1", "<a@x>", 0)
		a.recordPendingDone("j1", "<b@x>", 1)
		a.recordPendingDone("j2", "<c@x>", 2)
		if got := a.pendingDone["j1"]; !slices.Equal(got, []string{"<a@x>", "<b@x>"}) {
			t.Errorf("pendingDone[j1] = %v, want both message IDs", got)
		}
		if got := a.pendingDone["j2"]; !slices.Equal(got, []string{"<c@x>"}) {
			t.Errorf("pendingDone[j2] = %v, want [<c@x>] — jobs must not share a slice", got)
		}
	})
}

func TestHandleLateDuplicate(t *testing.T) {
	// A late article is one arriving for a file already finalized. It must
	// still be acked, or the queue waits forever for an article the assembler
	// has quietly dropped — and the ack has to carry the article's real
	// outcome, not a blanket Done.
	t.Run("acks a successful late article as done", func(t *testing.T) {
		a := newHelperAssembler()
		a.opts.MarkArticlesDone = func(string, []string) error { return nil }
		a.opts.MarkArticlesFailed = func(string, []string) ([]string, error) { return nil, nil }
		a.handleLateDuplicate(WriteRequest{JobID: "job", MessageID: "<a@x>"})
		if got := a.pendingDone["job"]; !slices.Equal(got, []string{"<a@x>"}) {
			t.Errorf("pendingDone = %v, want [<a@x>]", got)
		}
		if len(a.pendingFailed) != 0 {
			t.Errorf("a successful article was acked as failed: %v", a.pendingFailed)
		}
	})

	t.Run("acks a failed late article as failed", func(t *testing.T) {
		a := newHelperAssembler()
		a.opts.MarkArticlesDone = func(string, []string) error { return nil }
		a.opts.MarkArticlesFailed = func(string, []string) ([]string, error) { return nil, nil }
		a.handleLateDuplicate(WriteRequest{JobID: "job", MessageID: "<b@x>", FatalErr: os.ErrNotExist})
		if got := a.pendingFailed["job"]; !slices.Equal(got, []string{"<b@x>"}) {
			t.Errorf("pendingFailed = %v, want [<b@x>]", got)
		}
		if len(a.pendingDone) != 0 {
			t.Errorf("a failed article was acked as done: %v", a.pendingDone)
		}
	})
}

func TestRecordArticleCRC(t *testing.T) {
	dir := t.TempDir()

	t.Run("records the part with its offset and length", func(t *testing.T) {
		a := newHelperAssembler()
		f := newHelperFile(t, dir, "crc.dat", 0)
		a.recordArticleCRC(f, WriteRequest{Offset: 40, Data: []byte("abcd"), CRC: 0xAABBCCDD})
		if len(f.crcParts) != 1 {
			t.Fatalf("crcParts = %v, want one part", f.crcParts)
		}
		got := f.crcParts[0]
		if got.offset != 40 || got.crc != 0xAABBCCDD || got.len != 4 {
			t.Errorf("part = %+v, want offset 40, crc 0xAABBCCDD, len 4", got)
		}
		if !f.crcValid {
			t.Error("a recorded part cleared crcValid")
		}
	})

	t.Run("a zero CRC over real bytes invalidates the whole-file CRC", func(t *testing.T) {
		// UU-encoded articles arrive with no CRC. Their bytes are real, so the
		// combined CRC would be computed over a range with a hole in its
		// evidence — par2 would then be handed a mismatch on an intact file.
		a := newHelperAssembler()
		f := newHelperFile(t, dir, "crc0.dat", 0)
		a.recordArticleCRC(f, WriteRequest{Offset: 0, Data: []byte("abcd"), CRC: 0})
		if len(f.crcParts) != 0 {
			t.Errorf("crcParts = %v, want none recorded for a CRC-less article", f.crcParts)
		}
		if f.crcValid {
			t.Error("crcValid survived an article with no CRC")
		}
	})

	t.Run("a zero-length article with no CRC leaves crcValid alone", func(t *testing.T) {
		// It contributes no bytes, so it puts no range beyond the parts' reach.
		a := newHelperAssembler()
		f := newHelperFile(t, dir, "crcempty.dat", 0)
		a.recordArticleCRC(f, WriteRequest{Offset: 0, Data: nil, CRC: 0})
		if !f.crcValid {
			t.Error("a zero-length article cleared crcValid")
		}
	})
}

func TestNoteWritten(t *testing.T) {
	dir := t.TempDir()

	t.Run("advances the mark and stages the extent", func(t *testing.T) {
		a := newHelperAssembler()
		f := newHelperFile(t, dir, "note.dat", 0)
		a.noteWritten(f, 100, 20)
		if f.maxWritten != 120 {
			t.Errorf("maxWritten = %d, want 120", f.maxWritten)
		}
		if got := a.pendingExtent[f.key].maxWritten; got != 120 {
			t.Errorf("staged extent maxWritten = %d, want 120", got)
		}
	})

	t.Run("never retreats", func(t *testing.T) {
		// Articles arrive out of order, so a later write can sit below the
		// high-water mark. Taking each write literally would walk the mark
		// backwards and the completion truncate would cut the file short.
		a := newHelperAssembler()
		f := newHelperFile(t, dir, "note2.dat", 0)
		a.noteWritten(f, 100, 20)
		a.noteWritten(f, 0, 4)
		if f.maxWritten != 120 {
			t.Errorf("maxWritten = %d, want 120 — an earlier offset moved the mark back", f.maxWritten)
		}
		if got := a.pendingExtent[f.key].maxWritten; got != 120 {
			t.Errorf("staged extent maxWritten = %d, want 120", got)
		}
	})

	t.Run("leaves a staged cursor untouched", func(t *testing.T) {
		// The two figures travel together but are staged by different paths;
		// noteWritten must not clear a cursor the cache staged.
		a := newHelperAssembler()
		f := newHelperFile(t, dir, "note3.dat", 0)
		a.pendingExtent = map[fileKey]fileExtent{f.key: {cursor: 64}}
		a.noteWritten(f, 0, 8)
		if got := a.pendingExtent[f.key]; got.cursor != 64 || got.maxWritten != 8 {
			t.Errorf("staged extent = %+v, want cursor 64 and maxWritten 8", got)
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
	a.opts.OnFileComplete = func(string, int, uint32) { completions++ }

	f1 := newHelperFile(t, dir, "close1.dat", 0)
	f2 := newHelperFile(t, dir, "close2.dat", 0)
	f2.key = fileKey{jobID: "job", fileIdx: 1}
	open := map[fileKey]*openFile{f1.key: f1, f2.key: f2}

	a.closeAll(open)

	if completions != 0 {
		t.Errorf("closeAll fired %d completion callbacks, want 0", completions)
	}
	// A second Close returns an error on an already-closed handle, which is
	// how this asserts the first one landed.
	if err := f1.handle.Close(); err == nil {
		t.Error("f1 was still open after closeAll")
	}
	if err := f2.handle.Close(); err == nil {
		t.Error("f2 was still open after closeAll")
	}
}

func TestCloseAll_ContinuesPastAFailedClose(t *testing.T) {
	// One bad handle must not strand the rest open for the process's lifetime.
	dir := t.TempDir()
	a := newHelperAssembler()
	bad := newHelperFile(t, dir, "bad.dat", 0)
	_ = bad.handle.Close() // make its Close in closeAll fail
	good := newHelperFile(t, dir, "good.dat", 0)
	good.key = fileKey{jobID: "job", fileIdx: 1}

	a.closeAll(map[fileKey]*openFile{bad.key: bad, good.key: good})

	if err := good.handle.Close(); err == nil {
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
		st, err := f.handle.Stat()
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
