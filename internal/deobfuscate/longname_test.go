package deobfuscate_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/deobfuscate"
)

// M23: FixExtension with a long filename (near NAME_MAX=255) that would exceed
// 255 bytes after appending the detected extension.

func TestFixExtension_LongFilename(t *testing.T) {
	t.Parallel()
	log := slog.Default()

	t.Run("filename at limit succeeds when append stays within bounds", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// Create a filename of 250 chars (with .xyz = 254 bytes, appending .jpg → 258 > 255).
		// Actually: baseName (246) + ".xyz" (4) = 250 total. +".jpg" = 254 total — within limit.
		baseName := strings.Repeat("A", 242) + ".xyz"
		path := filepath.Join(dir, baseName)
		// JPEG magic bytes
		content := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}

		rename, err := deobfuscate.FixExtension(log, path)
		if err != nil {
			t.Fatalf("FixExtension error: %v", err)
		}
		if rename.From == "" {
			t.Fatal("expected a rename for JPEG content with .xyz extension")
		}
		expected := path + ".jpg"
		if rename.To != expected {
			t.Errorf("To = %q, want %q", rename.To, expected)
		}
		if _, err := os.Stat(rename.To); err != nil {
			t.Errorf("renamed file does not exist: %v", err)
		}
	})

	t.Run("filename exceeding NAME_MAX after append produces OS error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// Create a filename that's exactly 255 bytes (at NAME_MAX).
		// Appending ".jpg" would make it 259 bytes → os.Rename should fail.
		baseName := strings.Repeat("B", 251) + ".xyz" // 255 bytes total
		path := filepath.Join(dir, baseName)
		// JPEG magic bytes
		content := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := deobfuscate.FixExtension(log, path)
		// Appending ".jpg" to a 255-byte filename creates a 259-byte target.
		// The OS should reject this rename with ENAMETOOLONG.
		if err == nil {
			t.Error("expected error when renaming exceeds NAME_MAX, but got nil")
		}
	})

	t.Run("popular extension on long filename is skipped", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// Long filename with a popular extension — should be skipped entirely.
		baseName := strings.Repeat("C", 247) + ".mkv" // 251 bytes
		path := filepath.Join(dir, baseName)
		content := []byte("video content data")
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}

		rename, err := deobfuscate.FixExtension(log, path)
		if err != nil {
			t.Fatalf("FixExtension error: %v", err)
		}
		if rename.From != "" {
			t.Errorf("expected no rename for popular extension, got %+v", rename)
		}
	})
}
