package par2

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHasMagic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"par2 signature", write("a", append([]byte("PAR2\x00PKT"), 1, 2, 3)), true},
		{"other content", write("b", []byte("Rar!\x1a\x07\x01\x00 not par2")), false},
		{"shorter than the signature", write("c", []byte("PAR2")), false},
		{"empty", write("d", nil), false},
	}
	for _, tc := range cases {
		got, err := HasMagic(tc.path)
		if err != nil {
			t.Errorf("%s: HasMagic: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: HasMagic = %v, want %v", tc.name, got, tc.want)
		}
	}
	if _, err := HasMagic(filepath.Join(dir, "missing")); err == nil {
		t.Error("HasMagic on a missing file: err = nil, want an error")
	}
}
