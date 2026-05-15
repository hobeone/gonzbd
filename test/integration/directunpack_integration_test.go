//go:build integration

// Package directunpack integration tests exercise pure-Go RAR extraction
// via rardecode with multi-volume archives to verify WaitFS blocking and
// volume-boundary handling.
package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/directunpack"
)

// copyFixtures copies all .rar files from src into dst and returns a
// slice of the base filenames.
func copyFixtures(t *testing.T, src, dst string) []string {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".rar") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", e.Name(), err)
		}
		names = append(names, e.Name())
	}
	return names
}

// fixtureDir returns the absolute path to test/fixtures/rar/multivolume.
func fixtureDir(t *testing.T) string {
	t.Helper()
	// Walk up from the test file to find the project root.
	// The test is at test/integration/directunpack_integration_test.go
	// We need test/fixtures/rar/multivolume
	dir, err := filepath.Abs("../fixtures/rar/multivolume")
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("fixture dir not found at %s: %v", dir, err)
	}
	return dir
}

// expectedSHA256 reads the .sha256 file from the fixture directory.
func expectedSHA256(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir(t), "du_test_content.bin.sha256"))
	if err != nil {
		t.Fatalf("read sha256: %v", err)
	}
	// Format: "<hash>  <filename>\n"
	return strings.Fields(string(data))[0]
}

// TestDirectUnpackInOrder feeds volumes 1-6 in order and verifies that
// the extracted file matches the expected SHA-256.
func TestDirectUnpackInOrder(t *testing.T) {
	srcDir := fixtureDir(t)
	workDir := t.TempDir()
	extractDir := t.TempDir()

	names := copyFixtures(t, srcDir, workDir)
	if len(names) < 2 {
		t.Fatalf("expected at least 2 RAR volumes, got %d", len(names))
	}

	log := slog.Default().With("test", t.Name())
	du := directunpack.New(log, "test-job", workDir, extractDir, directunpack.Options{})

	du.SetAllFilenames(names)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Feed volumes in order with a small delay between each.
	for _, name := range names {
		du.Add(ctx, name, filepath.Join(workDir, name))
		time.Sleep(50 * time.Millisecond)
	}

	du.Wait()

	results := du.Results()
	if len(results) == 0 {
		t.Fatal("DirectUnpack produced no successful results")
	}

	// Verify the extracted file exists and matches.
	extractedFile := filepath.Join(extractDir, "du_test_content.bin")
	data, err := os.ReadFile(extractedFile)
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}

	hash := sha256.Sum256(data)
	got := hex.EncodeToString(hash[:])
	want := expectedSHA256(t)
	if got != want {
		t.Errorf("SHA-256 mismatch:\n  got:  %s\n  want: %s", got, want)
	}

	// Verify the SuccessSet records.
	for setname, ss := range results {
		t.Logf("Set %q: %d parts, %d extracted files", setname, len(ss.RarParts), len(ss.ExtractedFiles))
		if len(ss.RarParts) == 0 {
			t.Errorf("set %q has no RAR parts", setname)
		}
	}
}

// TestDirectUnpackOutOfOrder feeds volume 3, then 1, then 2, 4, 5, 6
// to verify that the DirectUnpacker correctly waits for vol 1 before
// starting, then handles out-of-order delivery.
func TestDirectUnpackOutOfOrder(t *testing.T) {
	srcDir := fixtureDir(t)
	workDir := t.TempDir()
	extractDir := t.TempDir()

	names := copyFixtures(t, srcDir, workDir)
	if len(names) < 6 {
		t.Fatalf("expected at least 6 RAR volumes, got %d", len(names))
	}

	log := slog.Default().With("test", t.Name())
	du := directunpack.New(log, "test-job", workDir, extractDir, directunpack.Options{})

	du.SetAllFilenames(names)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Deliver out of order: 3, 5, 1, 2, 4, 6
	order := []string{
		"du_test.part3.rar",
		"du_test.part5.rar",
		"du_test.part1.rar",
		"du_test.part2.rar",
		"du_test.part4.rar",
		"du_test.part6.rar",
	}
	for _, name := range order {
		du.Add(ctx, name, filepath.Join(workDir, name))
		time.Sleep(100 * time.Millisecond)
	}

	du.Wait()

	results := du.Results()
	if len(results) == 0 {
		t.Fatal("DirectUnpack produced no successful results")
	}

	// Verify the extracted file.
	extractedFile := filepath.Join(extractDir, "du_test_content.bin")
	data, err := os.ReadFile(extractedFile)
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}

	hash := sha256.Sum256(data)
	got := hex.EncodeToString(hash[:])
	want := expectedSHA256(t)
	if got != want {
		t.Errorf("SHA-256 mismatch:\n  got:  %s\n  want: %s", got, want)
	}
}

// TestDirectUnpackAbort verifies that Abort() stops extraction and
// Results() returns an empty map.
func TestDirectUnpackAbort(t *testing.T) {
	srcDir := fixtureDir(t)
	workDir := t.TempDir()
	extractDir := t.TempDir()

	names := copyFixtures(t, srcDir, workDir)

	log := slog.Default().With("test", t.Name())
	du := directunpack.New(log, "test-job", workDir, extractDir, directunpack.Options{})

	du.SetAllFilenames(names)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Feed only vol 1 to start the process.
	du.Add(ctx, "du_test.part1.rar", filepath.Join(workDir, "du_test.part1.rar"))
	time.Sleep(200 * time.Millisecond)

	// Abort while waiting for vol 2.
	du.Abort()

	results := du.Results()
	if len(results) != 0 {
		t.Errorf("expected empty results after abort, got %d sets", len(results))
	}
}
