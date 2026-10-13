package assembler

import (
	"bytes"
	"testing"
)

// TestOwnedRanges_Covered pins the arithmetic of covered: the bytes of a range
// that owned ranges cover, summed over every owner it touches.
func TestOwnedRanges_Covered(t *testing.T) {
	var o ownedRanges
	o.claim(Range{Off: 0, Len: 100}, articleID{artIdx: 0})
	o.claim(Range{Off: 200, Len: 100}, articleID{artIdx: 2})
	o.claim(Range{Off: 300, Len: 50}, articleID{artIdx: 5})

	cases := map[string]struct {
		r    Range
		want int64
	}{
		"inside one owner":                {Range{Off: 10, Len: 50}, 50},
		"identical to an owner":           {Range{Off: 0, Len: 100}, 100},
		"tail of one owner and a gap":     {Range{Off: 60, Len: 100}, 40},
		"spans a gap between two owners":  {Range{Off: 50, Len: 200}, 100},
		"two abutting owners exactly":     {Range{Off: 200, Len: 150}, 150},
		"three owners and both ends open": {Range{Off: 50, Len: 400}, 200},
		"touches nothing":                 {Range{Off: 100, Len: 100}, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := o.covered(tc.r); got != tc.want {
				t.Errorf("covered(%+v) = %d, want %d", tc.r, got, tc.want)
			}
		})
	}
}

// TestOverlap_RefusalReportsTheUncoveredBytes pins what acceptArticle hands
// OnArticleRejected for a range collision (#432): the article's bytes no other
// article's range covers, so the caller charges par2 for those alone. An
// out-of-range offset still reports WholeArticle.
func TestOverlap_RefusalReportsTheUncoveredBytes(t *testing.T) {
	cases := map[string]struct {
		off, n int64
		want   int64
	}{
		"wholly covered":           {off: 0, n: 1000, want: 0},
		"inside the incumbent":     {off: 100, n: 200, want: 0},
		"partly covered":           {off: 1800, n: 1000, want: 700},
		"straddles both neighbors": {off: 900, n: 1200, want: 0},
		"out of range":             {off: -5, n: 10, want: WholeArticle},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a := newHelperAssembler()
			var reports []int64
			a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string, uncovered int64) {
				if artIdx == 9 {
					reports = append(reports, uncovered)
				}
			}
			f := newHelperFile(t, t.TempDir(), "uncovered.dat", 3000)
			f.info.TotalParts = 3
			open := map[fileKey]*openFile{{jobID: "job", fileIdx: 0}: f}
			completed := map[fileKey]struct{}{}
			submit := func(idx int32, off, n int64) {
				a.processRequest(WriteRequest{
					JobID: "job", FileIdx: 0, ArtIdx: idx, MessageID: "m@x",
					Offset: off, Data: bytes.Repeat([]byte{'x'}, int(n)),
				}, open, completed)
			}
			// Incumbents [0,1000) and [1000,2100).
			submit(0, 0, 1000)
			submit(1, 1000, 1100)
			submit(9, tc.off, tc.n)

			if len(reports) != 1 || reports[0] != tc.want {
				t.Errorf("OnArticleRejected uncovered = %v for [%d,%d), want [%d]",
					reports, tc.off, tc.off+tc.n, tc.want)
			}
		})
	}
}
