package nntp

import "testing"

func TestExpectedBodyCodeDirect(t *testing.T) {
	t.Parallel()
	if cmdBody != 1 {
		t.Errorf("cmdBody = %d, want 1", cmdBody)
	}
	cases := []struct {
		name     string
		kind     cmdKind
		wantCode int
		wantOk   bool
	}{
		{"cmdBody", cmdBody, 222, true},
		{"cmdArticle", cmdArticle, 220, true},
		{"cmdHead", cmdHead, 221, true},
		{"cmdCapabilities", cmdCapabilities, 101, true},
		{"cmdStat", cmdStat, 0, false},
		{"unknown kind", cmdKind(99), 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotCode, gotOk := tc.kind.expectedBodyCode()
			if gotCode != tc.wantCode || gotOk != tc.wantOk {
				t.Errorf("expectedBodyCode() = (%d, %v), want (%d, %v)",
					gotCode, gotOk, tc.wantCode, tc.wantOk)
			}
		})
	}
}
