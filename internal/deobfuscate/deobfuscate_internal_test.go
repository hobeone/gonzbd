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
