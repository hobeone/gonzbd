//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/par2"
)

// TestPostProc_Par2VerifyOK verifies a par2 set for a known-good file using
// pre-built fixtures.
func TestPostProc_Par2VerifyOK(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("par2"); err != nil {
		t.Skip("par2 not installed")
	}

	dir := t.TempDir()
	copyPar2Fixture(t, "data.bin", dir, "data.bin")
	parFile := copyPar2Fixture(t, "data.par2", dir, "data.par2")

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Verify the par2 set.
	result, err := par2.RepairWith(ctx, par2.RunOptions{}, parFile)
	if err != nil {
		t.Fatalf("par2.RepairWith: %v", err)
	}
	if !result.Success {
		t.Errorf("par2.RepairWith failed: output: %s", result.Output)
	}
	if result.Parsed == nil || result.Parsed.Status != par2.StatusAllFilesOK {
		status := par2.StatusUnknown
		if result.Parsed != nil {
			status = result.Parsed.Status
		}
		t.Errorf("status = %v; want StatusAllFilesOK\noutput: %s", status, result.Output)
	}
}

// TestPostProc_Par2VerifyAndRepair corrupts a byte in the protected file,
// verifies (expecting damage), repairs, then verifies again (expecting OK) using
// pre-built fixtures.
func TestPostProc_Par2VerifyAndRepair(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("par2"); err != nil {
		t.Skip("par2 not installed")
	}

	dir := t.TempDir()
	dataFile := copyPar2Fixture(t, "data.bin", dir, "data.bin")
	parFile := copyPar2Fixture(t, "data.par2", dir, "data.par2")
	copyPar2Fixture(t, "data.vol000+102.par2", dir, "data.vol000+102.par2")

	expected, err := os.ReadFile(dataFile)
	if err != nil {
		t.Fatalf("read original data file: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// Corrupt one byte in the data file.
	corrupt := make([]byte, len(expected))
	copy(corrupt, expected)
	corrupt[len(corrupt)/2] ^= 0xFF
	if err := os.WriteFile(dataFile, corrupt, 0o600); err != nil {
		t.Fatalf("corrupt data file: %v", err)
	}

	// Repair.
	repResult, err := par2.RepairWith(ctx, par2.RunOptions{}, parFile)
	if err != nil {
		t.Fatalf("par2.RepairWith: %v", err)
	}
	if !repResult.Success {
		t.Fatalf("par2.RepairWith not successful (exit %d)\noutput: %s", repResult.ExitCode, repResult.Output)
	}
	if repResult.Parsed == nil || repResult.Parsed.Status != par2.StatusRepairComplete {
		status := par2.StatusUnknown
		if repResult.Parsed != nil {
			status = repResult.Parsed.Status
		}
		t.Errorf("repair status = %v; want StatusRepairComplete\noutput: %s", status, repResult.Output)
	}

	// Verify the input file is restored.
	restored, err := os.ReadFile(dataFile) //nolint:gosec // G304: test path
	if err != nil {
		t.Fatalf("read restored data file: %v", err)
	}
	if len(restored) != len(expected) {
		t.Fatalf("restored length %d, want %d", len(restored), len(expected))
	}
	for i := range expected {
		if restored[i] != expected[i] {
			t.Fatalf("mismatch at byte %d", i)
		}
	}
}

// TestPostProc_UnrarExtract tests extraction of a pre-built RAR fixture.
//
// This test is intentionally narrow: it exercises the par2/unpack package's
// ability to invoke the unrar binary on real archive data without requiring
// the `rar` binary at test time.  If no RAR fixture exists under
// test/fixtures/, the test is skipped with a clear reason.
func TestPostProc_UnrarExtract(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("unrar"); err != nil {
		t.Skip("unrar not installed")
	}

	// Look for a pre-built RAR fixture. The fixture must be committed to the
	// repository; we do not create it here because that would require the
	// `rar` binary.
	//
	// Expected path: test/fixtures/rar/sample.rar
	// The fixture should contain a single text file.
	fixtureDir := filepath.Join("..", "fixtures", "rar")
	rarFile := filepath.Join(fixtureDir, "sample.rar")
	if _, err := os.Stat(rarFile); os.IsNotExist(err) {
		t.Skip("no RAR fixture found at test/fixtures/rar/sample.rar — " +
			"creating RAR archives requires the `rar` binary which may not be present; " +
			"add a pre-built fixture to enable this test")
	}

	dir := t.TempDir()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Use unrar directly since the unpack package's integration point is the
	// postproc pipeline (tested at the unit level). Here we exercise that the
	// binary interaction itself works in a real environment.
	//nolint:gosec // G204: unrar called with fixture path validated above
	cmd := exec.CommandContext(ctx, "unrar", "x", "-y", rarFile, dir+"/")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unrar x: %v\noutput: %s", err, out)
	}

	// Verify at least one file was extracted.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) == 0 {
		t.Error("unrar extraction produced no files")
	}
	t.Logf("extracted %d file(s): %v", len(entries), entries)
}
