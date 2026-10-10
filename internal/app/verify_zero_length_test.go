package app

import (
	"slices"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// TestReadBackFile_VerifiesAZeroLengthRow: a zero-length article's row (the
// live door accepts it) verifies against CRC 0, claims no range so it never
// fails the article it sits inside, and a negative length is still deleted
// unread. A zero-length row with a non-zero CRC is deleted.
func TestReadBackFile_VerifiesAZeroLengthRow(t *testing.T) {
	t.Parallel()
	f := newVerifyFixture(t)
	empty := durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Offset: verifyArt / 2, Length: 0}
	badEmpty := durability.WrittenRow{FileIdx: 0, ArtIdx: 2, Offset: 3 * verifyArt, Length: 0, CRC32: 9}
	negative := durability.WrittenRow{FileIdx: 0, ArtIdx: 3, Offset: 0, Length: -1}
	first := f.rows[0]
	rows := []durability.WrittenRow{first, empty, badEmpty, negative}
	slices.SortFunc(rows, durability.CompareWrittenRows)

	out, err := readBackFile(t.Context(), f.path, rows, make([]byte, 1000), false)
	if err != nil {
		t.Fatalf("readBackFile: %v", err)
	}
	if want := []durability.WrittenRow{first, empty}; !slices.Equal(out.verified, want) {
		t.Errorf("verified = %+v, want %+v: the empty row sits inside article 0 and must neither fail it nor be failed", out.verified, want)
	}
	if len(out.failed) != 0 {
		t.Errorf("failed = %v, want none", out.failed)
	}
	slices.Sort(out.deleted)
	if want := []int32{2, 3}; !slices.Equal(out.deleted, want) {
		t.Errorf("deleted = %v, want %v: the zero-length row with a CRC and the negative length", out.deleted, want)
	}
}
