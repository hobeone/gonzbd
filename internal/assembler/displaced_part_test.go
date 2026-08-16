package assembler

import (
	"testing"
)

// TestAccept_DisplacedArticleGivesBackItsPart pins a producer of w.faulted on
// a path where NOTHING failed, which is why it went unpaired for so long.
//
// Two segments resolving to the same offset is not exotic — FileInfo.TotalParts'
// own doc says the assembler trusts the caller here. Article X is buffered and
// counted toward partsWritten. Article Y arrives at the same offset, wc.buffer
// pools X's bytes and returns it as displaced, and Accept calls w.fail(X) with
// uncount:true. If the run is not yet long enough to flush, Accept returns nil,
// handleSuccessArticle returns true, and Y is counted as well.
//
// The file is then one part closer to TotalParts with nothing behind it — the
// hazard releaseFaulted's own doc names, reached with no storage failure at
// all — until some unrelated later rollback happened to drain the set.
func TestAccept_DisplacedArticleGivesBackItsPart(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	var rolledBack []int32
	a.opts.OnArticlesUnwritten = func(_ string, _ int, artIdxs []int32) {
		rolledBack = append(rolledBack, artIdxs...)
	}

	f := newHelperFile(t, dir, "displaced.dat", 0)
	f.w.wc = newWriteCache(1 << 20)
	f.info.TotalParts = 4

	// X, at offset 0.
	if !a.handleSuccessArticle(f, WriteRequest{
		JobID: "job", FileIdx: 0, ArtIdx: 0, MessageID: "x", Offset: 0, Data: []byte("XXXX"),
	}) {
		t.Fatal("X was not accepted, so the fixture never buffered it")
	}
	f.partsWritten++

	// Y, at the SAME offset, which displaces X from the cache.
	if !a.handleSuccessArticle(f, WriteRequest{
		JobID: "job", FileIdx: 0, ArtIdx: 1, MessageID: "y", Offset: 0, Data: []byte("YYYY"),
	}) {
		t.Fatal("Y was not accepted, so the fixture never displaced X")
	}
	f.partsWritten++

	if len(rolledBack) == 0 {
		t.Fatal("nothing was rolled back, so the fixture did not displace X and this " +
			"test proves nothing about the displaced-article path")
	}
	if f.partsWritten != 1 {
		t.Errorf("partsWritten = %d, want 1 — X was displaced and its bytes pooled, so "+
			"only Y is behind a part. Counting both leaves the file one part closer to "+
			"TotalParts with nothing behind it, and a later article can then fire "+
			"OnFileComplete over bytes that never reached WriteAt", f.partsWritten)
	}
}
