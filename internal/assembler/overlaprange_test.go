package assembler

import (
	"bytes"
	"os"
	"testing"
)

// TestOverlap_PartialRangeOverwritesADurableArticle probes #387: collision
// detection keys on an article's exact START offset, so two articles whose
// byte ranges overlap without sharing a start offset are not detected.
//
// A occupies [0, 1000). B occupies [500, 1500). They overlap on [500, 1000),
// but acceptedAt is keyed on the offset alone, so B's probe at 500 misses A's
// entry at 0 and nothing compares the ranges.
//
// Both articles go straight to WriteAt, so A's bytes are on disk before B
// arrives.
func TestOverlap_PartialRangeOverwritesADurableArticle(t *testing.T) {
	t.Skip("#387: FileWriter detects collisions by exact start offset only, so THIS " +
		"layer does not see the overlap and the bytes are overwritten. The durability " +
		"layer withholds the whole-file CRC because the overlapping runs do not merge " +
		"into a single row, so par2 runs — but what this pins is " +
		"that the write happens at all. Kept executable rather than deleted: #387 " +
		"records that its original probe was thrown away and had to be rebuilt. Remove " +
		"this Skip when prevention lands; it must fail before it passes.")

	dir := t.TempDir()
	a := newHelperAssembler()

	var rejected []int32
	var unwritten []int32
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		rejected = append(rejected, artIdx)
	}
	a.opts.OnArticlesUnwritten = func(_ string, _ int, artIdxs []int32) {
		unwritten = append(unwritten, artIdxs...)
	}

	f := newHelperFile(t, dir, "overlap.dat", 0)
	f.info.TotalParts = 2
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}
	completed := map[fileKey]struct{}{}

	// A first, fully written before B is submitted.
	a.processRequest(WriteRequest{
		JobID: "job", FileIdx: 0, ArtIdx: 0, MessageID: "a@example",
		Offset: 0, Data: bytes.Repeat([]byte("A"), 1000),
	}, open, completed)

	// B starts 500 bytes into A's range.
	a.processRequest(WriteRequest{
		JobID: "job", FileIdx: 0, ArtIdx: 1, MessageID: "b@example",
		Offset: 500, Data: bytes.Repeat([]byte("B"), 1000),
	}, open, completed)

	got, err := os.ReadFile(f.info.Path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	// A's durable range [500, 1000) must still hold A's bytes. If B was allowed
	// to land, this range reads "B" and the run recorded for A — a CRC over
	// [0, 1000) — is asserting a checksum the file no longer satisfies.
	overlap := got[500:1000]
	if !bytes.Equal(overlap, bytes.Repeat([]byte("A"), 500)) {
		t.Errorf("A's durable range [500,1000) holds %q, want all 'A' — B overwrote "+
			"part of an article that was already written, and nothing detected it "+
			"(rejected=%v unwritten=%v)",
			string(overlap[:min(16, len(overlap))]), rejected, unwritten)
	}

	if len(rejected) == 0 {
		t.Errorf("no article was rejected, but two "+
			"articles claimed overlapping ranges [0,1000) and [500,1500) — the "+
			"overlap went entirely undetected (file len=%d)", len(got))
	}
}

// TestOverlap_ContainedOverlapStillCompletesTheFile is the variant that matters
// more than the one above. The overlapping article ends at or before the top of
// the file's recorded runs, so the file does NOT grow — every byte is covered, the part
// count reaches TotalParts, and the file finalizes as healthy.
//
// A0 [0,100), A1 [100,200), X [150,200). X overlaps A1 without sharing its
// start offset, so acceptedAt's exact-key probe at 150 misses.
func TestOverlap_ContainedOverlapStillCompletesTheFile(t *testing.T) {
	t.Skip("#387: as above, THE ASSEMBLER does not see the overlap. Note what this one " +
		"shows that the other does not: the file COMPLETES, which is what let the " +
		"barrier publish a whole-file CRC. That half is fixed — the overlapping article " +
		"abuts nothing, so it gets a durable_runs row of its own, and a whole-file CRC " +
		"is published only for a file that holds exactly ONE row starting at offset 0, " +
		"so par2 runs. What remains " +
		"unfixed, and what this pins, is that the bytes are overwritten in the first place.")

	dir := t.TempDir()
	a := newHelperAssembler()

	var rejected []int32
	var completed int
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		rejected = append(rejected, artIdx)
	}
	a.opts.OnFileComplete = func(_ string, _ int) {
		completed++
	}

	f := newHelperFile(t, dir, "contained.dat", 0)
	f.info.TotalParts = 3
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}
	completedSet := map[fileKey]struct{}{}

	submit := func(idx int32, msg string, off int64, b byte, n int) {
		a.processRequest(WriteRequest{
			JobID: "job", FileIdx: 0, ArtIdx: idx, MessageID: msg,
			Offset: off, Data: bytes.Repeat([]byte{b}, n),
		}, open, completedSet)
	}
	submit(0, "a0@example", 0, 'A', 100)
	submit(1, "a1@example", 100, 'B', 100)
	submit(2, "x@example", 150, 'X', 50) // contained in A1's range

	got, err := os.ReadFile(f.info.Path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	if !bytes.Equal(got[150:200], bytes.Repeat([]byte("B"), 50)) {
		t.Errorf("A1's durable range [150,200) holds %q, want all 'B' — X overwrote "+
			"it undetected (rejected=%v parts=%d completed=%d filelen=%d)",
			string(got[150:200]), rejected, f.w.parts(), completed, len(got))
	}
	if f.w.parts() >= f.info.TotalParts && len(rejected) == 0 {
		t.Errorf("the file reached parts=%d/%d with nothing rejected, "+
			"so the ASSEMBLER finalizes it as healthy over a range that was overwritten. "+
			"The downstream consequence is fixed elsewhere — the barrier withholds the "+
			"whole-file CRC for this shape so par2 runs — "+
			"but that does not undo the overwrite, which is what this pins",
			f.w.parts(), f.info.TotalParts)
	}
}
