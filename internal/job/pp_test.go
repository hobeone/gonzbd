package job

import "testing"

func TestClampPP(t *testing.T) {
	cases := []struct{ in, want int }{
		{-100, 0}, {-1, 0}, {0, 0}, {1, 1}, {2, 2}, {3, 3}, {4, 3}, {7, 3}, {256, 3},
	}
	for _, tc := range cases {
		p := ClampPP(tc.in)
		if p.Int() != tc.want {
			t.Errorf("ClampPP(%d).Int() = %d, want %d", tc.in, p.Int(), tc.want)
		}
		if p.Policy() != PolicyFromPP(tc.want) {
			t.Errorf("ClampPP(%d).Policy() = %+v, want %+v", tc.in, p.Policy(), PolicyFromPP(tc.want))
		}
	}
}

func TestPP_ZeroIsDownloadOnly(t *testing.T) {
	var p PP
	if p.Int() != 0 || p.Policy() != (Policy{}) {
		t.Errorf("zero PP = %d / %+v, want 0 / download-only", p.Int(), p.Policy())
	}
}
