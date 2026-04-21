package deobfuscate_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/sabnzbd-go/internal/deobfuscate"
)

func TestIsProbablyObfuscated(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		filename string
		want     bool
	}{
		// Certainly obfuscated
		{"32 hex digits", "b082fa0beaa644d3aa01045d5b8d0b36.mkv", true},
		{"40+ hex+dots", "0675e29e9abfd2.f7d069dab0b853283cc1b069a25f82.6547.avi", true},
		{"square-bracket + 30-hex", "[BlaBla] 5937bc5e32146ebef89a622e4a23f07b0d3757 [Brrr].mkv", true},
		{"abc.xyz prefix", "abc.xyz.a4c567edbcbf27.BLA.mkv", true},
		// Not obfuscated
		{"mixed case with separator", "Great Distro.mkv", false},
		{"three separators", "this is a download.mkv", false},
		{"letters+digits+sep", "Beast 2020.mkv", false},
		{"capital start mostly lowercase", "Catullus.mkv", false},
		{"tv show name", "The.Good.Place.S01E01.1080p.mkv", false},
		// Default obfuscated (pure lowercase, no separators, long)
		{"all lowercase no sep long", "abcdefghijk.mkv", true},
		// Path is handled correctly (only basename checked)
		{"full path not obfuscated", "/some/dir/My.Great.Show.S02E05.mkv", false},
		{"full path obfuscated", "/some/dir/b082fa0beaa644d3aa01045d5b8d0b36.mkv", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := deobfuscate.IsProbablyObfuscated(tc.filename)
			if got != tc.want {
				t.Errorf("IsProbablyObfuscated(%q) = %v, want %v", tc.filename, got, tc.want)
			}
		})
	}
}

func TestBiggestFile(t *testing.T) {
	t.Parallel()

	t.Run("empty list", func(t *testing.T) {
		t.Parallel()
		path, ok, err := deobfuscate.BiggestFile(nil)
		if err != nil || ok || path != "" {
			t.Errorf("expected empty result, got path=%q ok=%v err=%v", path, ok, err)
		}
	})

	t.Run("single file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		f := createFile(t, dir, "only.mkv", 1000)
		path, ok, err := deobfuscate.BiggestFile([]string{f})
		if err != nil {
			t.Fatal(err)
		}
		if !ok || path != f {
			t.Errorf("single file: got path=%q ok=%v, want path=%q ok=true", path, ok, f)
		}
	})

	t.Run("biggest is 3x larger", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		big := createFile(t, dir, "big.mkv", 9001)
		small := createFile(t, dir, "small.mkv", 3000)
		path, ok, err := deobfuscate.BiggestFile([]string{big, small})
		if err != nil {
			t.Fatal(err)
		}
		if !ok || path != big {
			t.Errorf("3x case: got path=%q ok=%v, want path=%q ok=true", path, ok, big)
		}
	})

	t.Run("biggest not 3x larger", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		a := createFile(t, dir, "a.mkv", 5000)
		b := createFile(t, dir, "b.mkv", 4000)
		_, ok, err := deobfuscate.BiggestFile([]string{a, b})
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("expected ok=false when biggest is not 3x larger")
		}
	})

	t.Run("multiple files none qualify", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		a := createFile(t, dir, "a.mkv", 1000)
		b := createFile(t, dir, "b.mkv", 900)
		c := createFile(t, dir, "c.mkv", 800)
		_, ok, err := deobfuscate.BiggestFile([]string{a, b, c})
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("expected ok=false for three similar-sized files")
		}
	})
}

func TestDeobfuscate(t *testing.T) {
	t.Parallel()

	t.Run("renames obfuscated biggest file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// Obfuscated name: 32 hex chars
		big := createFile(t, dir, "b082fa0beaa644d3aa01045d5b8d0b36.mkv", 9001)
		small := createFile(t, dir, "b082fa0beaa644d3aa01045d5b8d0b36.nfo", 100)

		renames, err := deobfuscate.Deobfuscate(dir, "Cool.Show.S01E01")
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) == 0 {
			t.Fatal("expected at least one rename")
		}

		// big should be gone, new name should exist
		if _, err := os.Stat(big); !os.IsNotExist(err) {
			t.Errorf("original big file still exists: %s", big)
		}
		_ = small // may or may not be renamed depending on stem match

		expectedBig := filepath.Join(dir, "Cool.Show.S01E01.mkv")
		if _, err := os.Stat(expectedBig); err != nil {
			t.Errorf("expected renamed file not found: %s", expectedBig)
		}
	})

	t.Run("no rename when not obfuscated", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		createFile(t, dir, "Great.Show.S01E01.1080p.mkv", 9001)
		createFile(t, dir, "other.nfo", 100)

		renames, err := deobfuscate.Deobfuscate(dir, "SomeName")
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) != 0 {
			t.Errorf("expected no renames, got %d", len(renames))
		}
	})

	t.Run("no rename when biggest not 3x", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		createFile(t, dir, "b082fa0beaa644d3aa01045d5b8d0b36.mkv", 5000)
		createFile(t, dir, "abcdefghijklmnopqrstuvwxyz012345.mkv", 4500)

		renames, err := deobfuscate.Deobfuscate(dir, "SomeName")
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) != 0 {
			t.Errorf("expected no renames for equally-sized files, got %d", len(renames))
		}
	})

	t.Run("excluded extension not renamed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		createFile(t, dir, "b082fa0beaa644d3aa01045d5b8d0b36.rar", 9001)
		createFile(t, dir, "tiny.nfo", 10)

		renames, err := deobfuscate.Deobfuscate(dir, "SomeName")
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) != 0 {
			t.Errorf("expected no renames for excluded extension, got %d", len(renames))
		}
	})
}

// createFile writes size bytes of zeros to dir/name and returns the full path.
func createFile(t *testing.T, dir, name string, size int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	data := make([]byte, size)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("createFile %s: %v", path, err)
	}
	return path
}
