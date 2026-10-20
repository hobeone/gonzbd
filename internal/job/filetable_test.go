package job

import "testing"

func TestNewFileTable_DerivesOffsetsAndTotals(t *testing.T) {
	ft := NewFileTable([]FileSpec{
		{Subject: "a.bin", Bytes: 300, ArticleCount: 3},
		{Subject: "a.vol00+01.par2", Bytes: 50, ArticleCount: 1},
		{Subject: "a.par2", Bytes: 10, ArticleCount: 1},
	})
	if got := ft.NumFiles(); got != 3 {
		t.Errorf("NumFiles = %d, want 3", got)
	}
	wantRanges := [][2]int{{0, 3}, {3, 4}, {4, 5}}
	for fi, w := range wantRanges {
		lo, hi := ft.Range(fi)
		if lo != w[0] || hi != w[1] {
			t.Errorf("Range(%d) = (%d,%d), want (%d,%d)", fi, lo, hi, w[0], w[1])
		}
	}
	if got := ft.TotalArticles(); got != 5 {
		t.Errorf("TotalArticles = %d, want 5", got)
	}
	if got := ft.TotalBytes(); got != 360 {
		t.Errorf("TotalBytes = %d, want 360", got)
	}
	if got := ft.RecoveryBytes(); got != 50 {
		t.Errorf("RecoveryBytes = %d, want 50", got)
	}
	if got := ft.RecoveryFiles(); got != 1 {
		t.Errorf("RecoveryFiles = %d, want 1 (the index .par2 is not recovery)", got)
	}
	if ft.IsPar2Recovery(0) || !ft.IsPar2Recovery(1) || ft.IsPar2Recovery(2) {
		t.Errorf("IsPar2Recovery = %v,%v,%v, want false,true,false",
			ft.IsPar2Recovery(0), ft.IsPar2Recovery(1), ft.IsPar2Recovery(2))
	}
	if ft.Subject(1) != "a.vol00+01.par2" || ft.Bytes(0) != 300 || ft.ArticleCount(0) != 3 {
		t.Errorf("accessors disagree with input: %q %d %d", ft.Subject(1), ft.Bytes(0), ft.ArticleCount(0))
	}
}

func TestNewFileTable_EmptyIsValid(t *testing.T) {
	ft := NewFileTable(nil)
	if ft.NumFiles() != 0 || ft.TotalArticles() != 0 || ft.TotalBytes() != 0 ||
		ft.RecoveryBytes() != 0 || ft.RecoveryFiles() != 0 {
		t.Errorf("empty table not zero: files=%d articles=%d bytes=%d",
			ft.NumFiles(), ft.TotalArticles(), ft.TotalBytes())
	}
	if len(ft.Specs()) != 0 {
		t.Errorf("Specs of empty table = %v, want empty", ft.Specs())
	}
}

func TestFileTable_SpecsIsACopy(t *testing.T) {
	in := []FileSpec{{Subject: "a.bin", Bytes: 7, ArticleCount: 2}}
	ft := NewFileTable(in)
	in[0].Subject = "mutated-input"
	got := ft.Specs()
	got[0].Subject = "mutated-output"
	got[0].Bytes = 999
	if ft.Subject(0) != "a.bin" || ft.Bytes(0) != 7 {
		t.Errorf("table changed through caller slices: %q %d", ft.Subject(0), ft.Bytes(0))
	}
	if again := ft.Specs(); again[0].Subject != "a.bin" {
		t.Errorf("Specs() aliases internal storage: %q", again[0].Subject)
	}
}
