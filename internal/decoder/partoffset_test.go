package decoder

import (
	"errors"
	"math"
	"testing"
)

// The `=ypart begin=` header comes from the article body returned by the NNTP
// server and is parsed as an unbounded int64. A hostile or compromised server
// can therefore claim an arbitrary offset, which the assembler would otherwise
// hand straight to WriteAt. maxPartOffset rejects implausible values at parse
// time; the authoritative per-file bound lives in the assembler.
func TestDecodeArticle_RejectsImplausiblePartOffset(t *testing.T) {
	raw := makeRaw(64)

	tests := []struct {
		name       string
		beginParam int64
		wantErr    bool
	}{
		{
			name:       "MaxInt64 begin is rejected",
			beginParam: math.MaxInt64,
			wantErr:    true,
		},
		{
			name:       "just past the cap is rejected",
			beginParam: maxPartOffset + 1,
			wantErr:    true,
		},
		{
			name:       "at the cap is accepted",
			beginParam: maxPartOffset,
			wantErr:    false,
		},
		{
			name:       "a realistic large-file offset is accepted",
			beginParam: 50 << 30, // 50 GiB into the file
			wantErr:    false,
		},
		{
			name:       "an ordinary offset is accepted",
			beginParam: 1,
			wantErr:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			article := yencEncodePart("hostile.bin", 1, 2, raw,
				math.MaxInt64, tc.beginParam, tc.beginParam+int64(len(raw))-1)

			_, err := DecodeArticle(article)

			if tc.wantErr {
				if !errors.Is(err, errMalformed) {
					t.Errorf("begin=%d: got err %v, want errMalformed — an implausible "+
						"part offset must not reach the assembler", tc.beginParam, err)
				}
				return
			}
			if errors.Is(err, errMalformed) {
				t.Errorf("begin=%d: legitimate offset rejected as malformed", tc.beginParam)
			}
		})
	}
}

// The cap must sit far above any real posting so it never rejects legitimate
// traffic. Pinned so the constant cannot be tightened into the plausible range
// without a deliberate test change.
func TestMaxPartOffset_LeavesHeadroomForRealFiles(t *testing.T) {
	const largestPlausibleFile = 1 << 41 // 2 TiB
	if maxPartOffset <= largestPlausibleFile {
		t.Errorf("maxPartOffset = %d is not comfortably above the largest plausible "+
			"Usenet posting (%d); it would reject legitimate articles",
			int64(maxPartOffset), int64(largestPlausibleFile))
	}
}
