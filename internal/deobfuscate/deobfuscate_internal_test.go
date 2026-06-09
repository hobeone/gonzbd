package deobfuscate

import (
	"crypto/md5"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestIsCapitalStartMostlyLowercase(t *testing.T) {
	cases := []struct {
		name     string
		basename string
		lower    int
		upper    int
		want     bool
	}{
		{"normal capital start", "Catullus", 7, 1, true},
		{"mostly upper", "CATULLUS", 0, 8, false},
		{"too short lower count", "Cat", 2, 1, false},
		{"no capital start", "catullus", 8, 0, false},
		{"exact boundary ratio 0.25", "House", 4, 1, true},
		{"above ratio limit 0.33", "Cats", 3, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isCapitalStartMostlyLowercase(tc.basename, tc.lower, tc.upper)
			if got != tc.want {
				t.Errorf("isCapitalStartMostlyLowercase(%q, %d, %d) = %v, want %v", tc.basename, tc.lower, tc.upper, got, tc.want)
			}
		})
	}
}

func TestIsShortSimpleWord(t *testing.T) {
	cases := []struct {
		name     string
		basename string
		upper    int
		decs     int
		seps     int
		want     bool
	}{
		{"short simple", "cat", 0, 0, 0, true},
		{"short simple with upper", "Cat", 1, 0, 0, false},
		{"within length limit", "catullus", 0, 0, 0, true},
		{"too short", "ca", 0, 0, 0, false},
		{"too long", "abcdefghijk", 0, 0, 0, false},
		{"contains decs", "cat1", 0, 1, 0, false},
		{"contains one sep", "ca.t", 0, 0, 1, true},
		{"contains two seps", "c.a.t", 0, 0, 2, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isShortSimpleWord(tc.basename, tc.upper, tc.decs, tc.seps)
			if got != tc.want {
				t.Errorf("isShortSimpleWord(%q, %d, %d, %d) = %v, want %v", tc.basename, tc.upper, tc.decs, tc.seps, got, tc.want)
			}
		})
	}
}

func TestResolveConflictingRename(t *testing.T) {
	tmpDir := t.TempDir()
	log := slog.Default()

	// Create test files.
	obfuscatedPath := filepath.Join(tmpDir, "obfuscated.txt")
	content := []byte("hello conflicting rename")
	if err := os.WriteFile(obfuscatedPath, content, 0o644); err != nil {
		t.Fatalf("failed to write obfuscated file: %v", err)
	}

	desiredPath := filepath.Join(tmpDir, "desired.txt")
	newPath := filepath.Join(tmpDir, "desired.1.txt")

	hash := md5.Sum(content)

	t.Run("identical duplicate file on disk", func(t *testing.T) {
		// Create desired file with identical content.
		if err := os.WriteFile(desiredPath, content, 0o644); err != nil {
			t.Fatalf("failed to write desired file: %v", err)
		}

		finalPath, skip := resolveConflictingRename(log, obfuscatedPath, "obfuscated.txt", desiredPath, newPath, hash)
		if !skip {
			t.Error("expected skip=true for identical duplicate file")
		}
		if finalPath != "" {
			t.Errorf("expected finalPath=\"\", got %q", finalPath)
		}

		// Obfuscated file should be removed.
		if _, err := os.Stat(obfuscatedPath); !os.IsNotExist(err) {
			t.Error("expected obfuscated file to be deleted")
		}
	})

	t.Run("different content conflict", func(t *testing.T) {
		// Recreate obfuscated file.
		if err := os.WriteFile(obfuscatedPath, content, 0o644); err != nil {
			t.Fatalf("failed to write obfuscated file: %v", err)
		}

		// Create desired file with different content.
		if err := os.WriteFile(desiredPath, []byte("different content"), 0o644); err != nil {
			t.Fatalf("failed to write desired file: %v", err)
		}

		finalPath, skip := resolveConflictingRename(log, obfuscatedPath, "obfuscated.txt", desiredPath, newPath, hash)
		if skip {
			t.Error("expected skip=false for different content conflict")
		}
		if finalPath != newPath {
			t.Errorf("expected finalPath=%q, got %q", newPath, finalPath)
		}
	})
}

func TestHasCollisionSuffixDirect(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		want     bool
	}{
		{"collision mkv.1", "video.mkv.1", true},
		{"collision rar.99", "archive.rar.99", true},
		{"collision 7z.02", "archive.7z.02", true},
		{"no extension suffix", "video.mkv", false},
		{"unpopular extension", "video.xyz.1", false},
		{"non-numeric suffix", "video.mkv.abc", false},
		{"empty string", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hasCollisionSuffix(tc.filename)
			if got != tc.want {
				t.Errorf("hasCollisionSuffix(%q) = %v, want %v", tc.filename, got, tc.want)
			}
		})
	}
}

func TestContainsIgnoredMovieFolderDirect(t *testing.T) {
	tmpDir := t.TempDir()

	// Initially empty directory should return false.
	if containsIgnoredMovieFolder(tmpDir) {
		t.Error("expected false for empty directory")
	}

	// Non-ignored directory should return false.
	if err := os.Mkdir(filepath.Join(tmpDir, "some_folder"), 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if containsIgnoredMovieFolder(tmpDir) {
		t.Error("expected false for non-ignored folder")
	}

	// Ignored folder (e.g. BDMV, case-insensitively) should return true.
	if err := os.Mkdir(filepath.Join(tmpDir, "bdmv"), 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if !containsIgnoredMovieFolder(tmpDir) {
		t.Error("expected true when bdmv folder exists")
	}
}

func TestExtractRARUsefulNameDirect(t *testing.T) {
	tmpDir := t.TempDir()
	log := slog.Default()

	// 1. Empty directory
	if got := extractRARUsefulName(tmpDir, log); got != "" {
		t.Errorf("extractRARUsefulName empty: got %q, want empty", got)
	}

	// 2. Directory with non-rar files
	if err := os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if got := extractRARUsefulName(tmpDir, log); got != "" {
		t.Errorf("extractRARUsefulName non-rar: got %q, want empty", got)
	}

	// 3. Directory with a valid RAR file
	fixturePath := filepath.Join("..", "..", "test", "fixtures", "rar", "sample.rar")
	fixtureData, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("failed to read fixture rar: %v", err)
	}

	rarPath := filepath.Join(tmpDir, "a1b2c3d4.rar")
	if err := os.WriteFile(rarPath, fixtureData, 0o644); err != nil {
		t.Fatalf("copy rar: %v", err)
	}

	got := extractRARUsefulName(tmpDir, log)
	if got != "sample" {
		t.Errorf("extractRARUsefulName(sample.rar) = %q, want 'sample'", got)
	}

	// Test nil logger doesn't panic
	if gotNil := extractRARUsefulName(tmpDir, nil); gotNil != "sample" {
		t.Errorf("extractRARUsefulName with nil logger: got %q, want 'sample'", gotNil)
	}
}

// ---------- Direct Content Equal Helpers ----------

func TestContentEqualAndStreamEqual_Direct(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()

	// 1. Files with different sizes
	f1 := filepath.Join(tmp, "f1.txt")
	f2 := filepath.Join(tmp, "f2.txt")
	if err := os.WriteFile(f1, []byte("short"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f2, []byte("longer file content"), 0644); err != nil {
		t.Fatal(err)
	}

	equal, err := contentEqual(f1, f2, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if equal {
		t.Error("expected false for different sizes")
	}

	// 2. Small files (<=16KB), identical content
	f3 := filepath.Join(tmp, "f3.txt")
	content3 := []byte("hello world small file")
	if err := os.WriteFile(f3, content3, 0644); err != nil {
		t.Fatal(err)
	}
	hash3 := md5.Sum(content3)

	equal, err = contentEqual(f3, f3, hash3)
	if err != nil {
		t.Fatal(err)
	}
	if !equal {
		t.Error("expected true for identical small files")
	}

	// 3. Small files (<=16KB), different hashes
	equal, err = contentEqual(f3, f1, hash3)
	if err != nil {
		t.Fatal(err)
	}
	if equal {
		t.Error("expected false for different small files")
	}

	// 4. Large files (>16KB), identical content
	largeContent := make([]byte, 40*1024)
	for i := range largeContent {
		largeContent[i] = byte(i % 256)
	}
	f4 := filepath.Join(tmp, "f4.txt")
	f5 := filepath.Join(tmp, "f5.txt")
	if err := os.WriteFile(f4, largeContent, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f5, largeContent, 0644); err != nil {
		t.Fatal(err)
	}
	hash4 := md5.Sum(largeContent[:16384])

	equal, err = contentEqual(f4, f5, hash4)
	if err != nil {
		t.Fatal(err)
	}
	if !equal {
		t.Error("expected true for identical large files")
	}

	// 5. Large files, different content in the middle (after 16KB)
	largeContentDiff := make([]byte, len(largeContent))
	copy(largeContentDiff, largeContent)
	largeContentDiff[30*1024] = 99
	f6 := filepath.Join(tmp, "f6.txt")
	if err := os.WriteFile(f6, largeContentDiff, 0644); err != nil {
		t.Fatal(err)
	}

	equal, err = contentEqual(f4, f6, hash4)
	if err != nil {
		t.Fatal(err)
	}
	if equal {
		t.Error("expected false for large files with different content in the streaming section")
	}

	// Test streamEqual directly
	equal, err = streamEqual(f4, f5)
	if err != nil {
		t.Fatal(err)
	}
	if !equal {
		t.Error("streamEqual expected true")
	}

	equal, err = streamEqual(f4, f6)
	if err != nil {
		t.Fatal(err)
	}
	if equal {
		t.Error("streamEqual expected false")
	}

	// 6. Exactly 32KB files (multiple of chunk size) to kill mutation on errB == io.EOF
	exactChunkContent := make([]byte, 32*1024)
	for i := range exactChunkContent {
		exactChunkContent[i] = byte(i % 256)
	}
	f7 := filepath.Join(tmp, "f7.txt")
	f8 := filepath.Join(tmp, "f8.txt")
	if err := os.WriteFile(f7, exactChunkContent, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f8, exactChunkContent, 0644); err != nil {
		t.Fatal(err)
	}
	hash7 := md5.Sum(exactChunkContent[:16384])
	equal, err = contentEqual(f7, f8, hash7)
	if err != nil {
		t.Fatal(err)
	}
	if !equal {
		t.Error("expected true for identical exactly 32KB files")
	}
}
