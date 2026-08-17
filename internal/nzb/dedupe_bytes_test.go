package nzb

import (
	"strings"
	"testing"
)

// File.Bytes must stay exactly the sum of Articles[].Bytes, which is what its
// own doc promises and what every consumer assumes.
//
// A dropped duplicate is not in the manifest, so it can never be downloaded
// and can never be failed. JobProgress.sizeFigures derives both the job's
// expected size and its remaining bytes from this field
// (`left := f.Bytes - f.BytesDownloaded - f.FailedBytes`), so counting bytes
// for an article that cannot arrive leaves them stranded in `remaining`: the
// job over-reports its size and under-reports its percentage for as long as
// the file is incomplete.
//
// Excluding them is also the existing convention — segments rejected for
// implausible size are already left out — and it is the right answer in the
// likelier of the two ambiguous cases, where the duplicate row is spurious and
// the file genuinely has one part fewer than the NZB claims.
func TestParse_FileBytesExcludesDroppedDuplicates(t *testing.T) {
	src := nzbWithSegments(seg(1, 100, "dup@h") + seg(2, 100, "dup@h") + seg(3, 100, "other@h"))

	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	f := got.Files[0]
	if len(f.Articles) != 2 {
		t.Fatalf("len(Articles) = %d, want 2", len(f.Articles))
	}
	if f.Bytes != 200 {
		t.Errorf("File.Bytes = %d, want 200 — a dropped duplicate's bytes were "+
			"counted, so they can never be downloaded or failed and stay stranded "+
			"in the job's remaining-bytes figure", f.Bytes)
	}
}

// The invariant stated in File.Bytes's own doc, pinned directly rather than
// inferred from a total: whatever the parser drops and for whatever reason,
// the field equals the sum of the articles it kept.
func TestParse_FileBytesEqualsTheSumOfItsArticles(t *testing.T) {
	src := nzbWithSegments(
		seg(1, 100, "a@h")+seg(2, 250, "b@h")+seg(3, 100, "a@h")+seg(4, 0, "c@h"),
		seg(1, 100, "b@h")+seg(2, 400, "d@h"),
	)

	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for i, f := range got.Files {
		var sum int64
		for _, a := range f.Articles {
			sum += int64(a.Bytes)
		}
		if f.Bytes != sum {
			t.Errorf("Files[%d].Bytes = %d, but its articles sum to %d — the field "+
				"and its own contents disagree", i, f.Bytes, sum)
		}
	}
}
