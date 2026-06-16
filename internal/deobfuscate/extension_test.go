package deobfuscate_test

import (
	"context"
	"log/slog"
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
		// Collision suffixes — popular ext + short numeric suffix.
		{"rar.1 collision", "movie.part01.rar.1", true},
		{"rar.01 collision", "movie.rar.01", true},
		{"mkv.2 collision", "video.mkv.2", true},
		{"7z.1 collision", "archive.7z.1", true},
		{"flac.1 collision", "album.flac.1", true},
		// NOT collision suffixes — numeric suffix too long or not popular base.
		{"xyz.1 not popular base", "file.xyz.1", false},
		// Note: file.rar.1234 now matches as a 4-digit split-part name
		// (SABnzbd parity — rarVolumeRe accepts \d{3,4}$), even though it
		// isn't a collision suffix per collisionSuffixRe. Both treat
		// the file as "known" so the test moves to the rarVolumeRe group.
		// Multi-volume RAR / 7z naming (mirrors SABnzbd RAR_RE).
		// User-reported case: NZB subject ".r00" — magic-byte sniffing
		// would otherwise append ".rar" and break unrar's volume chain.
		{"legacy r00 volume", "movie.r00", true},
		{"legacy r99 volume", "movie.r99", true},
		{"legacy s00 volume", "movie.s00", true},
		{"legacy v50 volume", "movie.v50", true},
		{"modern part01.rar", "movie.part01.rar", true},
		{"modern part123.rar", "movie.part123.rar", true},
		{"split 001 part", "movie.001", true},
		{"split 9999 part", "movie.9999", true},
		{"case insensitive R00", "MOVIE.R00", true},
		// Should NOT match.
		{"r9 too short", "file.r9", false},
		{"r100 wrong shape", "file.r100", false}, // 3 digits but only 1-letter prefix → not a 2-digit volume
		{"99 too short for split", "file.99", false},
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
	log := slog.Default()

	t.Run("jpg content with xyz extension", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "test1.xyz")
		// JPEG magic bytes
		content := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}

		rename, err := deobfuscate.FixExtension(context.Background(), log, path)
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

		rename, err := deobfuscate.FixExtension(context.Background(), log, path)
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

		rename, err := deobfuscate.FixExtension(context.Background(), log, path)
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

		rename, err := deobfuscate.FixExtension(context.Background(), log, path)
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

		rename, err := deobfuscate.FixExtension(context.Background(), log, path)
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

		rename, err := deobfuscate.FixExtension(context.Background(), log, path)
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

	t.Run("legacy .r00 volume not renamed", func(t *testing.T) {
		// User-reported bug: NZB volume "for.all.mankind...r00" had
		// ".rar" appended after magic-byte sniffing, producing
		// "for.all.mankind...r00.rar" — which broke unrar because the
		// rarset's volume manifest names the file ".r00" (no .rar).
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "for.all.mankind.s05e07.french.720p.web.h264-higgsboson.r00")
		// RAR magic bytes — would otherwise trigger the rename.
		header := []byte{'R', 'a', 'r', '!', 0x1A, 0x07, 0x00}
		content := make([]byte, 0, len(header)+256)
		content = append(content, header...)
		content = append(content, make([]byte, 256)...)
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}

		rename, err := deobfuscate.FixExtension(context.Background(), log, path)
		if err != nil {
			t.Fatalf("FixExtension error: %v", err)
		}
		if rename.From != "" {
			t.Errorf("expected no rename for legacy RAR volume, got %+v", rename)
		}
		// Original path must still exist with its original name.
		if _, err := os.Stat(path); err != nil {
			t.Errorf("original .r00 should still exist: %v", err)
		}
	})

	t.Run("modern partNN.rar volume not renamed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "movie.part02.rar")
		header := []byte{'R', 'a', 'r', '!', 0x1A, 0x07, 0x00}
		content := make([]byte, 0, len(header)+256)
		content = append(content, header...)
		content = append(content, make([]byte, 256)...)
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		rename, err := deobfuscate.FixExtension(context.Background(), log, path)
		if err != nil {
			t.Fatal(err)
		}
		if rename.From != "" {
			t.Errorf("expected no rename for .part02.rar, got %+v", rename)
		}
	})

	t.Run("collision suffix rar.1 is not renamed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "LOVEBITES.part01.rar.1")
		// RAR5 magic bytes
		content := make([]byte, 0, 8+256)
		content = append(content, 'R', 'a', 'r', '!', 0x1A, 0x07, 0x01, 0x00)
		content = append(content, make([]byte, 256)...) // pad
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}

		rename, err := deobfuscate.FixExtension(context.Background(), log, path)
		if err != nil {
			t.Fatalf("FixExtension error: %v", err)
		}
		if rename.From != "" {
			t.Errorf("expected no rename for collision-suffixed .rar.1 file, got %+v", rename)
		}
		// Verify the original file is untouched.
		if _, err := os.Stat(path); err != nil {
			t.Errorf("original file should still exist: %v", err)
		}
	})

	t.Run("collision suffix mkv.2 is not renamed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "video.mkv.2")
		content := []byte("some video content")
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}

		rename, err := deobfuscate.FixExtension(context.Background(), log, path)
		if err != nil {
			t.Fatalf("FixExtension error: %v", err)
		}
		if rename.From != "" {
			t.Errorf("expected no rename for collision-suffixed .mkv.2 file, got %+v", rename)
		}
	})

	t.Run("nil logger accepted", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "test_nil.xyz")
		content := []byte("random data")
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}

		rename, err := deobfuscate.FixExtension(context.Background(), nil, path)
		if err != nil {
			t.Fatalf("FixExtension error with nil logger: %v", err)
		}
		if rename.From != "" {
			t.Errorf("expected no rename, got %+v", rename)
		}
	})

	t.Run("rar3 content with non-popular extension", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "test_rar3.xyz")
		content := []byte{0x52, 0x61, 0x72, 0x21, 0x1A, 0x07, 0x00}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}

		rename, err := deobfuscate.FixExtension(context.Background(), slog.Default(), path)
		if err != nil {
			t.Fatalf("FixExtension error: %v", err)
		}
		if rename.From == "" {
			t.Fatal("expected a rename for RAR3 content")
		}
		if rename.To != path+".rar" {
			t.Errorf("expected destination %q, got %q", path+".rar", rename.To)
		}
	})

	t.Run("rar5 content with non-popular extension", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "test_rar5.xyz")
		content := []byte{0x52, 0x61, 0x72, 0x21, 0x1A, 0x07, 0x01, 0x00}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}

		rename, err := deobfuscate.FixExtension(context.Background(), slog.Default(), path)
		if err != nil {
			t.Fatalf("FixExtension error: %v", err)
		}
		if rename.From == "" {
			t.Fatal("expected a rename for RAR5 content")
		}
		if rename.To != path+".rar" {
			t.Errorf("expected destination %q, got %q", path+".rar", rename.To)
		}
	})
}

func TestFixExtension_NoOverwriteOnCollision(t *testing.T) {
	t.Parallel()
	log := slog.Default()

	t.Run("rar branch does not overwrite existing sibling", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		// Pre-create the would-be target name with known contents.
		target := filepath.Join(dir, "archive.xyz.rar")
		if err := os.WriteFile(target, []byte("OLD"), 0o644); err != nil {
			t.Fatal(err)
		}

		// Source file: RAR3 magic bytes, non-popular extension — FixExtension
		// will want to rename it to archive.xyz.rar, which already exists.
		path := filepath.Join(dir, "archive.xyz")
		rarMagic := []byte{0x52, 0x61, 0x72, 0x21, 0x1A, 0x07, 0x00}
		content := make([]byte, 0, len(rarMagic)+256)
		content = append(content, rarMagic...)
		content = append(content, make([]byte, 256)...)
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}

		rename, err := deobfuscate.FixExtension(context.Background(), log, path)
		if err != nil {
			t.Fatalf("FixExtension error: %v", err)
		}
		if rename.From == "" {
			t.Fatal("expected a rename, got zero-value")
		}

		// The pre-existing sibling must still have its original contents — not overwritten.
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("could not read pre-existing sibling: %v", err)
		}
		if string(got) != "OLD" {
			t.Errorf("pre-existing sibling was overwritten: contents = %q, want %q", string(got), "OLD")
		}

		// The renamed file must have landed on a unique name (GetUniqueFilename
		// appends .1 before the extension: "archive.xyz.1.rar").
		wantTo := filepath.Join(dir, "archive.xyz.1.rar")
		if rename.To != wantTo {
			t.Errorf("expected unique target %q, got %q", wantTo, rename.To)
		}
		if _, err := os.Stat(rename.To); err != nil {
			t.Errorf("renamed file does not exist at unique path: %v", err)
		}
	})

	t.Run("filetype branch does not overwrite existing sibling", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		// Pre-create the would-be target name with known contents.
		// FixExtension will detect JPEG content and want to rename to
		// "photo.xyz.jpg", which already exists.
		target := filepath.Join(dir, "photo.xyz.jpg")
		if err := os.WriteFile(target, []byte("OLD"), 0o644); err != nil {
			t.Fatal(err)
		}

		// Source file: JPEG magic bytes, non-popular extension.
		path := filepath.Join(dir, "photo.xyz")
		jpegMagic := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}
		if err := os.WriteFile(path, jpegMagic, 0o644); err != nil {
			t.Fatal(err)
		}

		rename, err := deobfuscate.FixExtension(context.Background(), log, path)
		if err != nil {
			t.Fatalf("FixExtension error: %v", err)
		}
		if rename.From == "" {
			t.Fatal("expected a rename, got zero-value")
		}

		// The pre-existing sibling must still have its original contents.
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("could not read pre-existing sibling: %v", err)
		}
		if string(got) != "OLD" {
			t.Errorf("pre-existing sibling was overwritten: contents = %q, want %q", string(got), "OLD")
		}

		// The renamed file must have landed on a unique name.
		// GetUniqueFilename("photo.xyz.jpg") → "photo.xyz.1.jpg"
		wantTo := filepath.Join(dir, "photo.xyz.1.jpg")
		if rename.To != wantTo {
			t.Errorf("expected unique target %q, got %q", wantTo, rename.To)
		}
		if _, err := os.Stat(rename.To); err != nil {
			t.Errorf("renamed file does not exist at unique path: %v", err)
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

		renames, err := deobfuscate.Subtitles(slog.Default(), dir)
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

		renames, err := deobfuscate.Subtitles(slog.Default(), dir)
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

		renames, err := deobfuscate.Subtitles(slog.Default(), dir)
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

		renames, err := deobfuscate.Subtitles(slog.Default(), dir)
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

		renames, err := deobfuscate.Deobfuscate(context.Background(), slog.Default(), dir, "Movie", fsutil.SanitizeOptions{})
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

		renames, err := deobfuscate.Deobfuscate(context.Background(), slog.Default(), dir, "Movie", fsutil.SanitizeOptions{})
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

		renames, err := deobfuscate.Deobfuscate(context.Background(), slog.Default(), dir, "Movie", fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) != 0 {
			t.Errorf("expected no renames for lowercase bdmv, got %d", len(renames))
		}
	})
}
