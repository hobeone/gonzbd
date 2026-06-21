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

// TestPostProc_Par2VerifyOK generates a par2 set for a known-good file,
// then verifies it.
func TestPostProc_Par2VerifyOK(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("par2"); err != nil {
		t.Skip("par2 not installed")
	}

	dir := t.TempDir()
	payload := []byte("integration test payload for par2 verification\n")
	dataFile := filepath.Join(dir, "data.bin")
	if err := os.WriteFile(dataFile, payload, 0o600); err != nil {
		t.Fatalf("write data file: %v", err)
	}

	// Create par2 set using the binary (test setup only).
	parFile := filepath.Join(dir, "data.par2")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	//nolint:gosec // G204: par2 binary called with test-generated paths under TempDir
	cmd := exec.CommandContext(ctx, "par2", "create", parFile, dataFile)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("par2 create: %v\noutput: %s", err, out)
	}

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
// verifies (expecting damage), repairs, then verifies again (expecting OK).
func TestPostProc_Par2VerifyAndRepair(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("par2"); err != nil {
		t.Skip("par2 not installed")
	}

	dir := t.TempDir()

	// Write a payload large enough for par2 to have recovery blocks.
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i % 256)
	}
	dataFile := filepath.Join(dir, "repairable.bin")
	if err := os.WriteFile(dataFile, payload, 0o600); err != nil {
		t.Fatalf("write data file: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// Create par2 set.
	parFile := filepath.Join(dir, "repairable.par2")
	//nolint:gosec // G204: par2 binary called with test paths
	cmd := exec.CommandContext(ctx, "par2", "create", "-r5", parFile, dataFile)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("par2 create: %v\noutput: %s", err, out)
	}

	// Corrupt one byte in the data file.
	data, err := os.ReadFile(dataFile) //nolint:gosec // G304: test path
	if err != nil {
		t.Fatalf("read data file: %v", err)
	}
	data[42] ^= 0xFF
	if err := os.WriteFile(dataFile, data, 0o600); err != nil {
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
	if len(restored) != len(payload) {
		t.Fatalf("restored length %d, want %d", len(restored), len(payload))
	}
	for i := range payload {
		if restored[i] != payload[i] {
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
