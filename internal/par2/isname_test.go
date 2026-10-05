package par2

import "testing"

func TestIsPar2Name(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"movie.par2":             true,
		"movie.PAR2":             true,
		"movie.vol01+02.par2":    true,
		"evil.par2.exe":          false,
		"evil.PAR2x.scr":         false,
		"evil.par2 .exe":         false,
		"movie.part01.rar":       false,
		"par2":                   false,
		"movie.par2.part":        false,
		"dir.par2/movie.mkv":     false,
		"nested/movie.vol1.par2": true,
	} {
		if got := IsPar2Name(name); got != want {
			t.Errorf("IsPar2Name(%q) = %v, want %v", name, got, want)
		}
	}
}
