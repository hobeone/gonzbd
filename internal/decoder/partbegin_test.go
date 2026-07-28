package decoder

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// yencPartWithRawBegin builds a multi-part yEnc article with an arbitrary
// literal `begin=` value, so tests can supply values that do not fit int64 or
// are otherwise unrepresentable via the typed helper.
func yencPartWithRawBegin(t *testing.T, begin string, raw []byte) []byte {
	t.Helper()
	article := yencEncodePart("part.bin", 1, 2, raw, 1<<20, 1, int64(len(raw)))
	lines := strings.SplitN(string(article), "\r\n", 3)
	if len(lines) < 3 || !strings.HasPrefix(lines[1], "=ypart") {
		t.Fatalf("unexpected article shape; line[1]=%q", lines[1])
	}
	lines[1] = fmt.Sprintf("=ypart begin=%s end=%d", begin, len(raw))
	return []byte(strings.Join(lines, "\r\n"))
}

// The upper bound added alongside maxPartOffset left the lower end of the
// range unvalidated: a negative begin, or one that overflows int64 (so
// strconv.ParseInt fails and beginVal silently stays 0), both bypassed the cap
// and were coerced to offset 0.
//
// begin=0 stays lenient on purpose. yEnc begin is 1-based, so a poster
// emitting 0 most plausibly means "offset 0" — which is what the code already
// does. Rejecting it would gain only spec-purity while risking real articles.
func TestDecodeArticle_MalformedPartBegin(t *testing.T) {
	raw := makeRaw(32)

	tests := []struct {
		name    string
		begin   string
		wantErr bool
	}{
		{
			name:    "negative begin is rejected",
			begin:   "-5",
			wantErr: true,
		},
		{
			name:    "begin overflowing int64 is rejected",
			begin:   "99999999999999999999999",
			wantErr: true,
		},
		{
			name:    "non-numeric begin is rejected",
			begin:   "abc",
			wantErr: true,
		},
		{
			name:    "begin=0 is accepted and treated as offset 0",
			begin:   "0",
			wantErr: false,
		},
		{
			name:    "begin=1 is accepted",
			begin:   "1",
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			article := yencPartWithRawBegin(t, tc.begin, raw)
			_, err := DecodeArticle(article)

			if tc.wantErr {
				if !errors.Is(err, errMalformed) {
					t.Errorf("begin=%q: got err %v, want errMalformed — a malformed "+
						"part offset must be rejected, not silently coerced to 0",
						tc.begin, err)
				}
				return
			}
			if errors.Is(err, errMalformed) {
				t.Errorf("begin=%q: rejected as malformed, want accepted", tc.begin)
			}
		})
	}
}
