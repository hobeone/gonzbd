package assembler

import (
	"bytes"
	"io"
	"os"
	"testing"
)

// withShortWrite makes the first WriteAt of more than n bytes actually write
// the leading n bytes and then report io.ErrShortWrite.
//
// The leading bytes are on disk while the article is rolled back. withWriteError
// cannot produce this shape — it writes nothing.
func withShortWrite(n int) fileWriterOpt {
	return func(w *FileWriter) {
		orig := w.writeAt
		fired := false
		w.writeAt = func(p []byte, off int64) (int, error) {
			if fired || len(p) <= n {
				return orig(p, off)
			}
			fired = true
			got, _ := orig(p[:n], off)
			return got, io.ErrShortWrite
		}
	}
}

// TestFileWriter_ShortWriteLeavesNoClaimOverPartialBytes pins the properties
// that bound a short write's blast radius to the article's own bytes.
//
// A short write leaves real bytes on disk that no record describes. writeOne
// cannot tell it from a total failure and rolls the article back, which is safe
// only because of what it does with the article.
//
// The load-bearing property is the last assertion: fail routes it to
// OUTSTANDING, not to permanently failed. A1 forbids a storage fault from
// resolving an article, so it is coming back. The file therefore cannot reach
// TotalParts, OnFileComplete cannot fire, and no truncate runs while the
// unrecorded partial bytes exist. By the time the file can finalize, the
// article has either been re-fetched — overwriting the partial bytes at the
// same offset — or failed through server exhaustion, which writes nothing.
//
// Change fail to resolve these permanently and the file can finalize with
// partial bytes above a bound derived from the other articles' records, which
// is silent corruption rather than a repairable hole.
func TestFileWriter_ShortWriteLeavesNoClaimOverPartialBytes(t *testing.T) {
	w := newTestFileWriter(t, withShortWrite(50))

	if err := w.Accept(articleID{msgID: "a0", artIdx: 0}, 0, bytes.Repeat([]byte{0xAA}, 100), 0); err == nil {
		t.Fatal("Accept returned nil error after a short write; the fault must reach the caller")
	}

	onDisk, rerr := os.ReadFile(w.path)
	if rerr != nil {
		t.Fatalf("read target: %v", rerr)
	}
	var nonZero int
	for _, b := range onDisk {
		if b != 0 {
			nonZero++
		}
	}
	if nonZero == 0 {
		t.Fatal("no bytes reached disk; this test no longer exercises a SHORT write, " +
			"so it cannot pin what happens to the bytes one leaves behind")
	}

	written, err := w.Drain()
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(written) != 0 {
		t.Errorf("Drain reported %d articles for a write that failed; a record built "+
			"from this report would claim bytes that are only partly on disk", len(written))
	}
	if faulted := w.takeFaulted(); len(faulted) != 1 {
		t.Errorf("takeFaulted returned %d articles, want 1 routed back to Outstanding: "+
			"a storage fault must never resolve an article (A1), or the file can "+
			"finalize over the partial bytes this write left behind", len(faulted))
	}
}
