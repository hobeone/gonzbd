package app

import (
	"os"
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
	slices.SortFunc(out.verified, durability.CompareWrittenRows) // the verified rows carry no order
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

// TestVerifyJobFiles_AZeroLengthRowDoesNotMoveTheTrimBound: a file finished by
// path is truncated to the end of its last verified row that claims a range.
// A zero-length row placed past that end claims none, so it must not hold
// the preallocated tail on disk.
func TestVerifyJobFiles_AZeroLengthRowDoesNotMoveTheTrimBound(t *testing.T) {
	t.Parallel()
	f := newVerifyFixture(t)
	f.write(t, append(slices.Clone(f.data[:3*verifyArt]), make([]byte, 3*verifyArt)...)) // a preallocated tail
	empty := durability.WrittenRow{FileIdx: 0, ArtIdx: 3, Offset: 5 * verifyArt, Length: 0}
	rows := append(pick(f.rows, 0, 1, 2), empty)

	res, err := f.run(t, t.Context(), rows, false)
	if err != nil {
		t.Fatalf("verifyJobFiles: %v", err)
	}
	if len(res.Verdicts) != 1 || !res.Verdicts[0].SetComplete {
		t.Fatalf("verdicts = %+v, want the file finished (SetComplete)", res.Verdicts)
	}
	st, err := os.Stat(f.path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got, want := st.Size(), int64(3*verifyArt); got != want {
		t.Errorf("finished file size = %d, want %d, the end of the last row with bytes: the zero-length row at %d moved the trim bound", got, want, empty.Offset)
	}
}
