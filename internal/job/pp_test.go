package job

import "testing"

func TestParsePP(t *testing.T) {
	for n := range 4 {
		p, err := ParsePP(n)
		if err != nil {
			t.Fatalf("ParsePP(%d) = %v, want nil", n, err)
		}
		if p.Int() != n {
			t.Errorf("ParsePP(%d).Int() = %d, want %d", n, p.Int(), n)
		}
		if p.Policy() != PolicyFromPP(n) {
			t.Errorf("ParsePP(%d).Policy() = %+v, want %+v", n, p.Policy(), PolicyFromPP(n))
		}
	}
	for _, n := range []int{-100, -1, 4, 7, 256} {
		if p, err := ParsePP(n); err == nil {
			t.Errorf("ParsePP(%d) = %d, nil; want an error", n, p.Int())
		}
	}
}

func TestClampPP(t *testing.T) {
	cases := []struct{ in, want int }{
		{-100, 0}, {-1, 0}, {0, 0}, {1, 1}, {2, 2}, {3, 3}, {4, 3}, {7, 3}, {256, 3},
	}
	for _, tc := range cases {
		if got := ClampPP(tc.in).Int(); got != tc.want {
			t.Errorf("ClampPP(%d).Int() = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestPP_ZeroIsDownloadOnly(t *testing.T) {
	var p PP
	if p.Int() != 0 || p.Policy() != (Policy{}) {
		t.Errorf("zero PP = %d / %+v, want 0 / download-only", p.Int(), p.Policy())
	}
}
