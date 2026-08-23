package durability

import (
	"context"
	"hash/crc32"
	"testing"

	"github.com/hobeone/gonzbd/internal/crc32util"
)

// crcOf returns crc32.ChecksumIEEE of b, for building fixtures whose CRC32
// fields are real checksums rather than arbitrary numbers — the merge tests
// below combine these values, so a made-up CRC would make a wrong Combine
// call look identical to a correct one.
func crcOf(b []byte) uint32 { return crc32.ChecksumIEEE(b) }

// TestSQLiteRunStore_MergesAbuttingInBothOffsetAndIndex pins the core merge
// rule: two articles whose offsets and article indices are both contiguous
// collapse into one row, with the combined CRC.
func TestSQLiteRunStore_MergesAbuttingInBothOffsetAndIndex(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))

	a := []byte("hello, ")
	b := []byte("world!!!")

	arts := []DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: int32(len(a)), CRC32: crcOf(a)},
		{FileIdx: 0, ArtIdx: 1, Offset: int64(len(a)), Length: int32(len(b)), CRC32: crcOf(b)},
	}
	if err := rs.Commit(ctx, "job-1", arts); err != nil {
		t.Fatal(err)
	}

	got, err := rs.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ForFile returned %d rows, want 1 merged row: %+v", len(got), got)
	}
	want := crc32util.Combine(crcOf(a), crcOf(b), int64(len(b)))
	r := got[0]
	if r.FirstArtIdx != 0 || r.LastArtIdx != 1 || r.Offset != 0 || r.Length != int64(len(a)+len(b)) {
		t.Fatalf("merged row = %+v, want FirstArtIdx=0 LastArtIdx=1 Offset=0 Length=%d", r, len(a)+len(b))
	}
	if r.CRC32 != want {
		t.Errorf("merged CRC32 = %#x, want %#x", r.CRC32, want)
	}
}

// TestSQLiteRunStore_OffsetAbutsIndexDoesNot pins that offset contiguity
// alone is not enough: a gap in article index (a missing article between
// them) must keep the rows separate even though their bytes are adjacent.
func TestSQLiteRunStore_OffsetAbutsIndexDoesNot(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))

	arts := []DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x1111},
		{FileIdx: 0, ArtIdx: 5, Offset: 100, Length: 50, CRC32: 0x2222}, // index gap: 1..4 missing
	}
	if err := rs.Commit(ctx, "job-1", arts); err != nil {
		t.Fatal(err)
	}

	got, err := rs.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ForFile returned %d rows, want 2 (offset-adjacent but index-discontiguous must not merge): %+v", len(got), got)
	}
}

// TestSQLiteRunStore_IndexAbutsOffsetDoesNot pins the mirror case: article
// index contiguity alone is not enough when the byte offsets leave a gap —
// e.g. a hole from a still-unwritten neighbour.
func TestSQLiteRunStore_IndexAbutsOffsetDoesNot(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))

	arts := []DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x1111},
		{FileIdx: 0, ArtIdx: 1, Offset: 150, Length: 50, CRC32: 0x2222}, // offset gap: 100..150
	}
	if err := rs.Commit(ctx, "job-1", arts); err != nil {
		t.Fatal(err)
	}

	got, err := rs.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ForFile returned %d rows, want 2 (index-adjacent but offset-discontiguous must not merge): %+v", len(got), got)
	}
}

// TestSQLiteRunStore_RedeliveredArticleIsDropped pins that a re-delivery of
// an article a stored run already covers is dropped entirely, not inserted
// as a second row — Commit must be idempotent against the barrier's
// at-least-once redelivery.
func TestSQLiteRunStore_RedeliveredArticleIsDropped(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))

	first := []DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 500, CRC32: 0xAAAA},
		{FileIdx: 0, ArtIdx: 1, Offset: 500, Length: 500, CRC32: 0xBBBB},
	}
	if err := rs.Commit(ctx, "job-1", first); err != nil {
		t.Fatal(err)
	}
	before, err := rs.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Re-deliver article 1 alone — its ArtIdx is already covered by the
	// stored merged row [0,1].
	redelivered := []DurableArticle{
		{FileIdx: 0, ArtIdx: 1, Offset: 500, Length: 500, CRC32: 0xBBBB},
	}
	if err := rs.Commit(ctx, "job-1", redelivered); err != nil {
		t.Fatal(err)
	}

	after, err := rs.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("ForFile returned %d rows after redelivery, want 1 (dropped, not inserted): %+v", len(after), after)
	}
	if after[0] != before[0] {
		t.Errorf("stored row changed after a dropped redelivery: before=%+v after=%+v", before[0], after[0])
	}
}

// TestSQLiteRunStore_RedeliveredAdjacentToNewIsNotAFalseOverlap pins the
// design doc's §6 worked example: articles 5-9 are re-delivered alongside
// genuinely new articles 10-12. Grouping before subtracting would form one
// run [5,12] that no stored row covers, inserting it beside the stored
// [0,9] row and producing a false overlap (Σ length exceeding the file's
// true size). Subtracting first must leave only the true new work, [10,12],
// which sits adjacent to the stored row and so merges into it — giving one
// row [0,12] whose Σ length equals the file's real size.
func TestSQLiteRunStore_RedeliveredAdjacentToNewIsNotAFalseOverlap(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))

	const artLen = 100 // bytes per article, uniform for simplicity

	mkArts := func(first, last int32) []DurableArticle {
		var out []DurableArticle
		for i := first; i <= last; i++ {
			out = append(out, DurableArticle{
				FileIdx: 0, ArtIdx: i, Offset: int64(i) * artLen, Length: artLen,
				CRC32: crcOf([]byte{byte(i)}), // distinct-ish, not used for length math here
			})
		}
		return out
	}

	// First commit: articles 0-9 land and merge into one stored row.
	if err := rs.Commit(ctx, "job-1", mkArts(0, 9)); err != nil {
		t.Fatal(err)
	}

	// Second commit: a redelivery of 5-9 arrives alongside genuinely new
	// 10-12, in one batch — exactly the shape that breaks whole-run dedup.
	if err := rs.Commit(ctx, "job-1", mkArts(5, 12)); err != nil {
		t.Fatal(err)
	}

	got, err := rs.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ForFile returned %d rows, want 1 — got %+v", len(got), got)
	}
	r := got[0]
	if r.FirstArtIdx != 0 || r.LastArtIdx != 12 {
		t.Fatalf("merged row spans art %d..%d, want 0..12: %+v", r.FirstArtIdx, r.LastArtIdx, r)
	}
	wantLen := int64(13 * artLen) // 13 articles, 0..12 inclusive
	if r.Length != wantLen {
		t.Fatalf("Σ length = %d, want %d — a false overlap means dedup happened after grouping, not before", r.Length, wantLen)
	}
}

// TestSQLiteRunStore_MergeIsOrderIndependent pins that the same set of
// articles produces the same stored rows regardless of the order they
// arrive in within one Commit call.
func TestSQLiteRunStore_MergeIsOrderIndependent(t *testing.T) {
	ctx := context.Background()

	forward := []DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 10, CRC32: crcOf([]byte("0123456789"))},
		{FileIdx: 0, ArtIdx: 1, Offset: 10, Length: 10, CRC32: crcOf([]byte("abcdefghij"))},
		{FileIdx: 0, ArtIdx: 2, Offset: 20, Length: 10, CRC32: crcOf([]byte("ABCDEFGHIJ"))},
	}
	reversed := []DurableArticle{forward[2], forward[0], forward[1]}

	rsFwd := NewSQLiteRunStore(openTestDB(t))
	if err := rsFwd.Commit(ctx, "job-1", forward); err != nil {
		t.Fatal(err)
	}
	gotFwd, err := rsFwd.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}

	rsRev := NewSQLiteRunStore(openTestDB(t))
	if err := rsRev.Commit(ctx, "job-1", reversed); err != nil {
		t.Fatal(err)
	}
	gotRev, err := rsRev.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(gotFwd) != 1 || len(gotRev) != 1 {
		t.Fatalf("want exactly one merged row each way: forward=%+v reversed=%+v", gotFwd, gotRev)
	}
	if gotFwd[0] != gotRev[0] {
		t.Errorf("merge depends on arrival order: forward=%+v reversed=%+v", gotFwd[0], gotRev[0])
	}
}

// TestSQLiteRunStore_CRCMatchesRealBytes pins associativity against actual
// data: for N articles written contiguously, the merged row's CRC32 must
// equal crc32.ChecksumIEEE of the real concatenated bytes — not merely some
// value crc32util.Combine happens to produce. Everything durable_runs
// asserts about a whole file's integrity rests on this equality.
func TestSQLiteRunStore_CRCMatchesRealBytes(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))

	chunks := [][]byte{
		[]byte("the quick brown fox "),
		[]byte("jumps over "),
		[]byte("the lazy dog"),
		[]byte(", twice, for good measure."),
	}
	var whole []byte
	arts := make([]DurableArticle, 0, len(chunks))
	offset := int64(0)
	for i, c := range chunks {
		arts = append(arts, DurableArticle{
			FileIdx: 0, ArtIdx: int32(i), Offset: offset, Length: int32(len(c)), CRC32: crcOf(c),
		})
		whole = append(whole, c...)
		offset += int64(len(c))
	}

	if err := rs.Commit(ctx, "job-1", arts); err != nil {
		t.Fatal(err)
	}
	got, err := rs.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ForFile returned %d rows, want 1 fully-merged row: %+v", len(got), got)
	}
	want := crcOf(whole)
	if got[0].CRC32 != want {
		t.Errorf("merged CRC32 = %#x, want %#x (crc32.ChecksumIEEE of the real concatenated bytes)", got[0].CRC32, want)
	}
}

// TestSQLiteRunStore_MergesTwoMultiArticleRunsAcrossCommits pins that two
// runs which are each already multi-article BEFORE they meet — one from an
// earlier Commit, one grouped fresh within the current Commit — still fold
// into one correct row. Each entry in the fold (run.go's mergeAdjacentRuns)
// contributes its OWN accumulated Length to Combine, and left-to-right
// folding makes that equivalent to a single Combine call using the whole
// right-hand run's length — it is not a distinct code path, so this test
// does not by itself distinguish "combine per run" from "combine per
// article": every fold here happens to have a single-article run as the
// right-hand operand (see mergeAdjacentRuns), so r.Length and "one article's
// length" agree throughout. That distinguishing case is
// TestSQLiteRunStore_CombineUsesWholeRunLengthNotOneArticle below.
func TestSQLiteRunStore_MergesTwoMultiArticleRunsAcrossCommits(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))

	left1 := []byte("AAAA")
	left2 := []byte("BBBB")
	right1 := []byte("CCCCCC")
	right2 := []byte("DDDDDD")

	// First Commit: articles 0-1 merge into one run, [0,8).
	firstBatch := []DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: int32(len(left1)), CRC32: crcOf(left1)},
		{FileIdx: 0, ArtIdx: 1, Offset: int64(len(left1)), Length: int32(len(left2)), CRC32: crcOf(left2)},
	}
	if err := rs.Commit(ctx, "job-1", firstBatch); err != nil {
		t.Fatal(err)
	}

	// Second Commit: articles 2-3 arrive together, group into their own
	// run [8,20) before folding against the stored run.
	secondBatch := []DurableArticle{
		{FileIdx: 0, ArtIdx: 2, Offset: int64(len(left1) + len(left2)), Length: int32(len(right1)), CRC32: crcOf(right1)},
		{FileIdx: 0, ArtIdx: 3, Offset: int64(len(left1) + len(left2) + len(right1)), Length: int32(len(right2)), CRC32: crcOf(right2)},
	}
	if err := rs.Commit(ctx, "job-1", secondBatch); err != nil {
		t.Fatal(err)
	}

	got, err := rs.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ForFile returned %d rows, want 1 fully-merged row: %+v", len(got), got)
	}

	whole := append(append(append(append([]byte{}, left1...), left2...), right1...), right2...)
	want := crcOf(whole)
	if got[0].CRC32 != want {
		t.Errorf("merged CRC32 = %#x, want %#x", got[0].CRC32, want)
	}
}

// TestSQLiteRunStore_CombineUsesWholeRunLengthNotOneArticle pins the
// specific arithmetic trap the design doc's Step 2 calls out: Combine's
// len2 argument must be the incoming run's WHOLE length, not one article's.
//
// That distinction is only reachable on a LEFTWARD extension: new articles
// landing immediately BEFORE an already-merged multi-article stored run.
// mergeAdjacentRuns always builds a fresh incoming article as a
// single-article Run, and combined is sorted by Offset ascending, so a
// stored multi-article run can only ever appear as the RIGHT-hand operand
// (`r`) of a fold — never the left (`cur`). A fold whose new articles land
// AFTER the stored run (as in
// TestSQLiteRunStore_MergesTwoMultiArticleRunsAcrossCommits above) never
// puts a multi-article run on the right, so it cannot exercise this: r is a
// single new article at every step there, and "r.Length" and "one article's
// length" are the same number throughout. Landing new articles BEFORE the
// stored run is the one shape that forces r to be the multi-article stored
// run itself, which is what this test does: articles 2-3 commit first and
// merge into a 2-article stored run, then articles 0-1 commit second,
// merge with EACH OTHER into their own 2-article run, and finally fold
// against the stored run as r — so Combine's len2 must be the stored run's
// whole Length (12), not right1's length alone (6).
//
// A mutation that replaces r.Length with a per-article length
// (r.Length / (r.LastArtIdx-r.FirstArtIdx+1)) is silent on every other test
// in this file, because every other fold's r is already single-article.
// Confirmed empirically: with that mutation applied, this test alone fails;
// every other test in the package stays green. See the fix report for the
// observed failure and the restored green run.
func TestSQLiteRunStore_CombineUsesWholeRunLengthNotOneArticle(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))

	left1 := []byte("AAAA")
	left2 := []byte("BBBB")
	right1 := []byte("CCCCCC")
	right2 := []byte("DDDDDD")

	// First Commit: articles 2-3 land and merge into one stored run,
	// [8,20) — a 2-article run that will later be the RIGHT-hand operand
	// of a fold.
	firstBatch := []DurableArticle{
		{FileIdx: 0, ArtIdx: 2, Offset: int64(len(left1) + len(left2)), Length: int32(len(right1)), CRC32: crcOf(right1)},
		{FileIdx: 0, ArtIdx: 3, Offset: int64(len(left1) + len(left2) + len(right1)), Length: int32(len(right2)), CRC32: crcOf(right2)},
	}
	if err := rs.Commit(ctx, "job-1", firstBatch); err != nil {
		t.Fatal(err)
	}

	// Second Commit: articles 0-1 land to the LEFT of the stored run,
	// group into their own run [0,8), and then that run's fold against
	// the stored run puts the stored run — whole Length 12 — on the
	// right-hand side of the final Combine call.
	secondBatch := []DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: int32(len(left1)), CRC32: crcOf(left1)},
		{FileIdx: 0, ArtIdx: 1, Offset: int64(len(left1)), Length: int32(len(left2)), CRC32: crcOf(left2)},
	}
	if err := rs.Commit(ctx, "job-1", secondBatch); err != nil {
		t.Fatal(err)
	}

	got, err := rs.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ForFile returned %d rows, want 1 fully-merged row: %+v", len(got), got)
	}

	whole := append(append(append(append([]byte{}, left1...), left2...), right1...), right2...)
	want := crcOf(whole)
	if got[0].CRC32 != want {
		t.Errorf("merged CRC32 = %#x, want %#x — Combine must use the stored run's WHOLE length (12), not one article's (6)", got[0].CRC32, want)
	}
}

// TestSQLiteRunStore_SameOffsetKeepsTheLongerRow pins the exact-offset
// collision, which merging alone cannot resolve: two entries at the SAME
// offset never abut (r.Offset == cur.Offset+cur.Length is false for a
// non-empty cur), so both reach insertRuns, and both address the one primary
// key (job_id, file_idx, offset). INSERT OR REPLACE then keeps whichever was
// written last.
//
// The case is reachable, and dispatch.go's UU block documents how: a
// multi-part UU post yields several articles that all assert offset 0.
// FileWriter.acceptedAt resolves that within one open-file episode, but it is
// per-open-episode residency, so across a restart or a close-handles cycle a
// second article at offset 0 reaches the run store beside the stored row.
//
// What must not be lost is the LONGER row. FinalizeFile computes its truncate
// bound from the stored runs, so if the 100-byte row survives and the
// 1000-byte one is discarded, a LATER finalize — a retry, a re-completion —
// truncates a 1000-byte file to 100 bytes. (The finalize that commits the
// collision is itself safe: it computes its bound before the commit.) Keeping
// the longer row makes the bound conservative in the safe direction.
//
// Both commit orders are pinned because they take different paths to the same
// outcome: with the long row stored first the drop does the work, and with the
// short row stored first the sort's FirstArtIdx tiebreak decides which of two
// equal-length entries at offset 0 becomes the fold's accumulator — pick the
// wrong one and articles 1-9 no longer abut it, leaving two rows.
func TestSQLiteRunStore_SameOffsetKeepsTheLongerRow(t *testing.T) {
	const artLen = 100

	// tiling is articles 0-9, each 100 bytes, covering [0,1000) — they merge
	// into a single stored run of length 1000.
	tiling := func() []DurableArticle {
		out := make([]DurableArticle, 0, 10)
		for i := range int32(10) {
			out = append(out, DurableArticle{
				FileIdx: 0, ArtIdx: i, Offset: int64(i) * artLen, Length: artLen,
				CRC32: crcOf([]byte{byte(i)}),
			})
		}
		return out
	}
	// intruder is a second article asserting offset 0, as a multi-part UU
	// post's later segment does.
	intruder := []DurableArticle{
		{FileIdx: 0, ArtIdx: 10, Offset: 0, Length: artLen, CRC32: crcOf([]byte{0xAA})},
	}

	cases := []struct {
		name   string
		first  []DurableArticle
		second []DurableArticle
	}{
		{"long row stored first", tiling(), intruder},
		{"short row stored first", intruder, tiling()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			rs := NewSQLiteRunStore(openTestDB(t))

			if err := rs.Commit(ctx, "job-1", c.first); err != nil {
				t.Fatal(err)
			}
			if err := rs.Commit(ctx, "job-1", c.second); err != nil {
				t.Fatal(err)
			}

			got, err := rs.ForFile(ctx, "job-1", 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("ForFile returned %d rows, want 1 — two entries share offset 0, so one must be dropped rather than silently REPLACEd: %+v", len(got), got)
			}
			r := got[0]
			if r.Offset != 0 || r.Length != 10*artLen {
				t.Fatalf("stored row = %+v, want Offset=0 Length=%d — the LONGER row must survive, or a later FinalizeFile truncates a %d-byte file to %d", r, 10*artLen, 10*artLen, r.Length)
			}
			if r.FirstArtIdx != 0 || r.LastArtIdx != 9 {
				t.Errorf("stored row spans art %d..%d, want 0..9: %+v", r.FirstArtIdx, r.LastArtIdx, r)
			}
		})
	}
}
