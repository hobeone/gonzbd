package decoder

import (
	"bytes"
	"math"
	"strconv"
	"testing"
)

// TestTotalSizeIsDeclaredNotValidated pins that Article.TotalSize carries the
// poster's =ybegin size= figure through unvalidated, whatever it says.
//
// The point is not that this behaviour is desirable — it is that the type's
// doc comment used to claim the opposite ("TotalSize equals len(Data)" for a
// single-part article) and nothing in the package disagreed with the comment,
// because a comment is neither type-checked nor executed. #347 was that
// mismatch. Replacing one unenforced sentence with another would leave the
// same hole, so the corrected comment is pinned here instead.
//
// If a validation of `size` is ever added, this test SHOULD fail. Its job is
// to make that a deliberate decision with a visible cost rather than a silent
// change of contract under callers who read the field.
func TestTotalSizeIsDeclaredNotValidated(t *testing.T) {
	raw := []byte("forty-six bytes of entirely ordinary payload!!")
	if len(raw) != 46 {
		t.Fatalf("fixture is %d bytes, the case in #347 is 46", len(raw))
	}

	// A correct single-part article, whose =ybegin size= is then rewritten.
	// The TRAILER keeps the true length, so the one size check the decoder
	// does run still passes and the CRC still matches — which is exactly why
	// the total goes unnoticed.
	base := yencEncode("declared.bin", raw)

	for _, tc := range []struct {
		name     string
		declared string
		want     int64
	}{
		{"absurdly large", "999999", 999999},
		{"negative", "-5", -5},
		{"max int64", strconv.FormatInt(math.MaxInt64, 10), math.MaxInt64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := bytes.Replace(base,
				[]byte("size="+strconv.Itoa(len(raw))+" name="),
				[]byte("size="+tc.declared+" name="), 1)
			if bytes.Equal(body, base) {
				t.Fatal("fixture rewrite matched nothing; the =ybegin size= field was not replaced")
			}

			art, err := DecodeArticle(body)
			if err != nil {
				t.Fatalf("DecodeArticle: %v — a bogus =ybegin size= is not supposed to be an error, "+
					"and if it now is, this test has done its job: update Article.TotalSize's "+
					"doc comment, which currently tells callers the value is unvalidated", err)
			}
			if art.TotalSize != tc.want {
				t.Errorf("TotalSize = %d, want %d — the declared figure is passed through, not derived",
					art.TotalSize, tc.want)
			}
			if len(art.Data) != len(raw) {
				t.Errorf("len(Data) = %d, want %d — the PART's length is checked against the "+
					"trailer and must be unaffected by the total", len(art.Data), len(raw))
			}
			if !bytes.Equal(art.Data, raw) {
				t.Errorf("decoded payload does not round-trip")
			}
			if art.Offset != 0 {
				t.Errorf("Offset = %d, want 0 for a single-part article", art.Offset)
			}
		})
	}

	// The half the doc comment gets to keep: for this single-part article the
	// declared total happens to equal len(Data). Asserting it here records
	// that the old comment described a CONVENTION well-behaved posters follow,
	// which is why it read as an invariant for so long.
	art, err := DecodeArticle(base)
	if err != nil {
		t.Fatalf("DecodeArticle on the unmodified fixture: %v", err)
	}
	if art.TotalSize != int64(len(raw)) {
		t.Errorf("TotalSize = %d, want %d for a well-formed single-part article",
			art.TotalSize, len(raw))
	}
}
