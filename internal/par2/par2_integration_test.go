//go:build integration

package par2

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// hasPar2 returns true and the path to the par2 binary if it is installed.
func hasPar2() (string, bool) {
	path, err := exec.LookPath("par2")
	return path, err == nil
}

// TestVerifyAndRepair exercises the external par2 CLI wrapper using checked-in
// fixtures (data.bin, data.par2, data.vol000+102.par2) without invoking par2 create.
func TestVerifyAndRepair(t *testing.T) {
	_, ok := hasPar2()
	if !ok {
		t.Skip("par2 binary not found in PATH; skipping integration tests")
	}

	dir := t.TempDir()
	mainFile := copyPar2Fixtures(t, dir)
	inputFile := filepath.Join(dir, "data.bin")

	data, err := os.ReadFile(inputFile)
	if err != nil {
		t.Fatalf("read input fixture: %v", err)
	}

	ctx := t.Context()

	t.Run("verify_good_set", func(t *testing.T) {
		res, err := verify(ctx, mainFile)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		if res.Status != StatusAllFilesOK {
			t.Errorf("Status = %v, want StatusAllFilesOK\nstdout: %s\nstderr: %s",
				res.Status, res.Stdout, res.Stderr)
		}
	})

	t.Run("verify_corrupt_set", func(t *testing.T) {
		// Corrupt a single byte in the input file.
		corrupt := make([]byte, len(data))
		copy(corrupt, data)
		corrupt[len(corrupt)/2] ^= 0xFF
		if err := os.WriteFile(inputFile, corrupt, 0o644); err != nil {
			t.Fatalf("write corrupt: %v", err)
		}
		t.Cleanup(func() {
			if err := os.WriteFile(inputFile, data, 0o644); err != nil {
				t.Logf("cleanup write: %v", err)
			}
		})

		res, err := verify(ctx, mainFile)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		// par2 may report RepairRequired or RepairPossible on corruption.
		if res.Status != StatusRepairRequired && res.Status != StatusRepairPossible {
			t.Errorf("Status = %v, want RepairRequired or RepairPossible\nstdout: %s\nstderr: %s",
				res.Status, res.Stdout, res.Stderr)
		}
	})

	t.Run("repair_corrupt_set", func(t *testing.T) {
		corrupt := make([]byte, len(data))
		copy(corrupt, data)
		corrupt[len(corrupt)/2] ^= 0xFF
		if err := os.WriteFile(inputFile, corrupt, 0o644); err != nil {
			t.Fatalf("write corrupt: %v", err)
		}

		res, err := repair(ctx, mainFile)
		if err != nil {
			t.Fatalf("repair: %v", err)
		}
		if !res.Success {
			t.Errorf("Repair.Success = false\nOutput: %s", res.Output)
		}

		// Verify the input file is restored.
		restored, err := os.ReadFile(inputFile)
		if err != nil {
			t.Fatalf("read restored: %v", err)
		}
		if len(restored) != len(data) {
			t.Fatalf("restored len %d, want %d", len(restored), len(data))
		}
		for i := range data {
			if restored[i] != data[i] {
				t.Errorf("byte %d mismatch after repair: got %02x, want %02x", i, restored[i], data[i])
				break
			}
		}
	})
}
