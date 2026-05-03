package deobfuscate_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/deobfuscate"
	"github.com/hobeone/gonzbd/internal/fsutil"
)

func TestHasPopularExtension(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		filename string
		want     bool
	}{
		{"mkv is popular", "movie.mkv", true},
		{"rar is popular", "file.rar", true},
		{"par2 is popular", "set.par2", true},
		{"xyz is not popular", "file.xyz", false},
		{"no extension", "somefile", false},
		{"case insensitive", "VIDEO.MKV", true},
		{"jpg popular", "photo.jpg", true},
		{"random gibberish ext", "file.asdfgh", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := deobfuscate.HasPopularExtension(tc.filename)
			if got != tc.want {
				t.Errorf("HasPopularExtension(%q) = %v, want %v", tc.filename, got, tc.want)
			}
		})
	}
}

func TestFixExtension(t *testing.T) {
	t.Parallel()

	t.Run("jpg content with xyz extension", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "test1.xyz")
		// JPEG magic bytes
		content := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}

		rename, err := deobfuscate.FixExtension(path)
		if err != nil {
			t.Fatalf("FixExtension error: %v", err)
		}
		if rename.From == "" {
			t.Fatal("expected a rename, got zero-value")
		}
		if rename.To != filepath.Join(dir, "test1.xyz.jpg") {
			t.Errorf("expected To to end with .xyz.jpg, got %q", rename.To)
		}
		if _, err := os.Stat(rename.To); err != nil {
			t.Errorf("renamed file does not exist: %v", err)
		}
	})

	t.Run("png content with no popular extension", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "test2.asdf")
		// PNG magic bytes
		content := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}

		rename, err := deobfuscate.FixExtension(path)
		if err != nil {
			t.Fatalf("FixExtension error: %v", err)
		}
		if rename.From == "" {
			t.Fatal("expected a rename, got zero-value")
		}
		if rename.To != filepath.Join(dir, "test2.asdf.png") {
			t.Errorf("expected .asdf.png, got %q", rename.To)
		}
	})

	t.Run("correct jpg extension unchanged", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "test3.jpg")
		content := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}

		rename, err := deobfuscate.FixExtension(path)
		if err != nil {
			t.Fatalf("FixExtension error: %v", err)
		}
		if rename.From != "" {
			t.Errorf("expected no rename for correct extension, got %+v", rename)
		}
	})

	t.Run("unknown content no rename", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "test4.xyz")
		content := []byte("some random data that is definitely not a known format")
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}

		rename, err := deobfuscate.FixExtension(path)
		if err != nil {
			t.Fatalf("FixExtension error: %v", err)
		}
		if rename.From != "" {
			t.Errorf("expected no rename for unknown content, got %+v", rename)
		}
	})

	t.Run("empty file no rename", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "test5.xyz")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}

		rename, err := deobfuscate.FixExtension(path)
		if err != nil {
			t.Fatalf("FixExtension error: %v", err)
		}
		if rename.From != "" {
			t.Errorf("expected no rename for empty file, got %+v", rename)
		}
	})

	t.Run("mkv content detected", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "obfuscated.asdf")
		// EBML header (0x1A 0x45 0xDF 0xA3) + enough EBML structure
		// to include the matroska DocType marker.
		// This is a minimal valid EBML header that filetype recognizes as MKV.
		ebmlHeader := []byte{
			0x1A, 0x45, 0xDF, 0xA3, // EBML magic
			0x93,                   // size
			0x42, 0x86, 0x81, 0x01, // EBMLVersion: 1
			0x42, 0xF7, 0x81, 0x01, // EBMLReadVersion: 1
			0x42, 0xF2, 0x81, 0x04, // EBMLMaxIDLength: 4
			0x42, 0xF3, 0x81, 0x08, // EBMLMaxSizeLength: 8
			0x42, 0x82, 0x88, // DocType (length 8)
			'm', 'a', 't', 'r', 'o', 's', 'k', 'a', // "matroska"
		}
		if err := os.WriteFile(path, ebmlHeader, 0o644); err != nil {
			t.Fatal(err)
		}

		rename, err := deobfuscate.FixExtension(path)
		if err != nil {
			t.Fatalf("FixExtension error: %v", err)
		}
		if rename.From == "" {
			t.Fatal("expected a rename for MKV content, got zero-value")
		}
		if rename.To != filepath.Join(dir, "obfuscated.asdf.mkv") {
			t.Errorf("expected .asdf.mkv, got %q", rename.To)
		}
	})
}

func TestDeobfuscateSubtitles(t *testing.T) {
	t.Parallel()

	t.Run("renames non-matching srt", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// Big video file
		createFile(t, dir, "Some_Big_Movie.mkv", 10000)
		// Already matching srt — should not be renamed
		createFile(t, dir, "Some_Big_Movie.srt", 100)
		// Non-matching srt — should be renamed
		createFile(t, dir, "14_English.srt", 100)
		// Small non-srt file
		createFile(t, dir, "info.nfo", 50)

		renames, err := deobfuscate.Subtitles(dir)
		if err != nil {
			t.Fatal(err)
		}

		if len(renames) != 1 {
			t.Fatalf("expected 1 rename, got %d: %v", len(renames), renames)
		}
		expectedTo := filepath.Join(dir, "Some_Big_Movie.14_English.srt")
		if renames[0].To != expectedTo {
			t.Errorf("expected To=%q, got %q", expectedTo, renames[0].To)
		}
		if _, err := os.Stat(expectedTo); err != nil {
			t.Errorf("renamed file does not exist: %v", err)
		}
	})

	t.Run("no srt files no renames", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		createFile(t, dir, "video.mkv", 10000)
		createFile(t, dir, "info.nfo", 50)

		renames, err := deobfuscate.Subtitles(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) != 0 {
			t.Errorf("expected no renames, got %d", len(renames))
		}
	})

	t.Run("no biggest file no renames", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		createFile(t, dir, "a.mkv", 1000)
		createFile(t, dir, "b.mkv", 900)
		createFile(t, dir, "14_English.srt", 50)

		renames, err := deobfuscate.Subtitles(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) != 0 {
			t.Errorf("expected no renames when no biggest file, got %d", len(renames))
		}
	})

	t.Run("multiple non-matching srts", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		createFile(t, dir, "Movie_2024.mkv", 10000)
		createFile(t, dir, "eng.srt", 100)
		createFile(t, dir, "dut.srt", 100)

		renames, err := deobfuscate.Subtitles(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) != 2 {
			t.Fatalf("expected 2 renames, got %d: %v", len(renames), renames)
		}
	})
}

func TestDeobfuscate_DVDBluraySkip(t *testing.T) {
	t.Parallel()

	t.Run("skips when VIDEO_TS present", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// Create VIDEO_TS subdirectory
		os.MkdirAll(filepath.Join(dir, "VIDEO_TS"), 0o755)
		createFile(t, dir, "b082fa0beaa644d3aa01045d5b8d0b36.vob", 9001)

		renames, err := deobfuscate.Deobfuscate(dir, "Movie", fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) != 0 {
			t.Errorf("expected no renames for DVD structure, got %d", len(renames))
		}
	})

	t.Run("skips when BDMV present", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		os.MkdirAll(filepath.Join(dir, "BDMV"), 0o755)
		createFile(t, dir, "b082fa0beaa644d3aa01045d5b8d0b36.m2ts", 9001)

		renames, err := deobfuscate.Deobfuscate(dir, "Movie", fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) != 0 {
			t.Errorf("expected no renames for Bluray structure, got %d", len(renames))
		}
	})

	t.Run("case insensitive bdmv", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		os.MkdirAll(filepath.Join(dir, "bdmv"), 0o755)
		createFile(t, dir, "b082fa0beaa644d3aa01045d5b8d0b36.m2ts", 9001)

		renames, err := deobfuscate.Deobfuscate(dir, "Movie", fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) != 0 {
			t.Errorf("expected no renames for lowercase bdmv, got %d", len(renames))
		}
	})
}
