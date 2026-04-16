//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/par2"
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	//nolint:gosec // G204: par2 binary called with test-generated paths under TempDir
	cmd := exec.CommandContext(ctx, "par2", "create", parFile, dataFile)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("par2 create: %v\noutput: %s", err, out)
	}

	// Verify the par2 set.
	result, err := par2.Verify(ctx, parFile)
	if err != nil {
		t.Fatalf("par2.Verify: %v", err)
	}
	if result.Status != par2.StatusAllFilesOK {
		t.Errorf("par2.Verify status = %v; want StatusAllFilesOK\nstdout: %s\nstderr: %s",
			result.Status, result.Stdout, result.Stderr)
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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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

	// Verify — expect repair required or repair possible.
	verResult, err := par2.Verify(ctx, parFile)
	if err != nil {
		t.Fatalf("par2.Verify (corrupted): %v", err)
	}
	if verResult.Status == par2.StatusAllFilesOK {
		t.Fatal("expected par2.Verify to detect damage but it reported StatusAllFilesOK")
	}
	t.Logf("post-corruption status: %v (stdout: %s)", verResult.Status, verResult.Stdout)

	// Repair.
	repResult, err := par2.Repair(ctx, parFile)
	if err != nil {
		t.Fatalf("par2.Repair: %v", err)
	}
	if !repResult.Success {
		t.Fatalf("par2.Repair not successful (exit %d)\noutput: %s", repResult.ExitCode, repResult.Output)
	}

	// Verify again — expect OK.
	verResult2, err := par2.Verify(ctx, parFile)
	if err != nil {
		t.Fatalf("par2.Verify (after repair): %v", err)
	}
	if verResult2.Status != par2.StatusAllFilesOK {
		t.Errorf("after repair: status = %v; want StatusAllFilesOK\nstdout: %s\nstderr: %s",
			verResult2.Status, verResult2.Stdout, verResult2.Stderr)
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
