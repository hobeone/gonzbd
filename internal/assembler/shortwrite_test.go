package assembler

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// withShortWrite makes the first WriteAt of more than n bytes actually write
// the leading n bytes and then report io.ErrShortWrite.
//
// The leading bytes are on disk while the article is rolled back. withWriteError
// cannot produce this shape — it writes nothing.
func withShortWrite(n int) fileWriterOpt {
	return func(w *fileWriter) {
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
// The load-bearing property is the last three assertions: the article is
// rolled back, not permanently failed, and routeOutcome returns it to
// OUTSTANDING. A1 forbids a storage fault from
// resolving an article, so it is coming back. The file therefore cannot reach
// TotalParts, OnFileComplete cannot fire, and no truncate runs while the
// unrecorded partial bytes exist. By the time the file can finalize, the
// article has either been re-fetched — overwriting the partial bytes at the
// same offset — or failed through server exhaustion, which writes nothing.
//
// Resolve a short write Rejected instead of RolledBack and the file can
// finalize with partial bytes above a bound derived from the other articles'
// records, which is silent corruption rather than a repairable hole.
func TestFileWriter_ShortWriteLeavesNoClaimOverPartialBytes(t *testing.T) {
	w := newTestFileWriter(t, withShortWrite(50))
	a := newHelperAssembler()
	var unwritten []int32
	a.opts.OnArticlesUnwritten = func(_ string, _ int, arts []int32) { unwritten = append(unwritten, arts...) }
	f := &openFile{w: w, info: FileInfo{Dir: filepath.Dir(w.path), Name: filepath.Base(w.path), ExpectedSize: 1000}}

	a.handleSuccessArticle(f, WriteRequest{
		JobID: "job", FileIdx: 0, ArtIdx: 0, MessageID: "a0",
		Offset: 0, Data: bytes.Repeat([]byte{0xAA}, 100),
	})

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

	if len(w.unsynced) != 0 {
		t.Errorf("%d articles noted written for a write that failed; a record built "+
			"from them would claim bytes that are only partly on disk", len(w.unsynced))
	}
	if _, failed := w.seenFailed[0]; failed {
		t.Error("a storage fault resolved the article against itself (A1); the file " +
			"can then finalize over the partial bytes this write left behind")
	}
	if w.parts() != 0 {
		t.Errorf("parts = %d, want 0: the rolled-back article gave its part back", w.parts())
	}
	if len(unwritten) != 1 || unwritten[0] != 0 {
		t.Errorf("OnArticlesUnwritten = %v, want [0]: the article is Outstanding again, "+
			"so its re-fetch overwrites the partial bytes before the file can finalize", unwritten)
	}
}
