package par2_test

import (
	"cmp"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hobeone/sabnzbd-go/internal/par2"
)

// hasPar2 returns true and the path to the par2 binary if it is installed.
func hasPar2() (string, bool) {
	path, err := exec.LookPath("par2")
	return path, err == nil
}

// touch creates an empty file at path, creating parent directories as needed.
func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	f.Close()
}

// ---- FindPar2Files tests (no par2 binary needed) -------------------------

func TestFindPar2Files(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		layout    []string          // files to create relative to tmpdir
		wantSets  int               // expected number of sets
		wantNames []string          // sorted expected set names
		wantMain  map[string]string // set name → expected MainFile basename; "" means empty
	}{
		{
			name: "single set with main and extras",
			layout: []string{
				"movie.par2",
				"movie.vol000+01.par2",
				"movie.vol001+02.par2",
			},
			wantSets:  1,
			wantNames: []string{"movie"},
			wantMain:  map[string]string{"movie": "movie.par2"},
		},
		{
			name: "two independent sets",
			layout: []string{
				"alpha.par2",
				"alpha.vol000+01.par2",
				"beta.par2",
				"beta.vol000+04.par2",
				"beta.vol004+08.par2",
			},
			wantSets:  2,
			wantNames: []string{"alpha", "beta"},
			wantMain: map[string]string{
				"alpha": "alpha.par2",
				"beta":  "beta.par2",
			},
		},
		{
			name:     "empty directory",
			layout:   []string{},
			wantSets: 0,
		},
		{
			name: "non-par2 files are ignored",
			layout: []string{
				"movie.mkv",
				"movie.nfo",
				"movie.par2",
			},
			wantSets:  1,
			wantNames: []string{"movie"},
			wantMain:  map[string]string{"movie": "movie.par2"},
		},
		{
			name: "only volume files no main",
			layout: []string{
				"show.vol000+01.par2",
				"show.vol001+02.par2",
			},
			wantSets:  1,
			wantNames: []string{"show"},
			wantMain:  map[string]string{"show": ""},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			for _, f := range tc.layout {
				touch(t, filepath.Join(dir, f))
			}

			sets, err := par2.FindPar2Files(dir)
			if err != nil {
				t.Fatalf("FindPar2Files: %v", err)
			}

			if len(sets) != tc.wantSets {
				t.Fatalf("got %d sets, want %d", len(sets), tc.wantSets)
			}

			// Sort returned sets for stable comparison.
			slices.SortFunc(sets, func(a, b par2.Set) int { return cmp.Compare(a.Name, b.Name) })

			if tc.wantNames != nil {
				for i, s := range sets {
					if s.Name != tc.wantNames[i] {
						t.Errorf("set[%d].Name = %q, want %q", i, s.Name, tc.wantNames[i])
					}
				}
			}

			for _, s := range sets {
				wantMain, ok := tc.wantMain[s.Name]
				if !ok {
					continue
				}
				gotMain := filepath.Base(s.MainFile)
				if gotMain == "." {
					gotMain = "" // empty string when MainFile is ""
				}
				if wantMain == "" && s.MainFile == "" {
					continue
				}
				if gotMain != wantMain {
					t.Errorf("set %q MainFile = %q, want %q", s.Name, gotMain, wantMain)
				}
			}
		})
	}
}

// ---- Integration tests (require par2 binary) ------------------------------

func TestVerifyAndRepair(t *testing.T) {
	_, ok := hasPar2()
	if !ok {
		t.Skip("par2 binary not found in PATH; skipping integration tests")
	}

	dir := t.TempDir()
	inputFile := filepath.Join(dir, "input.bin")

	// Write 1 KiB of known data.
	data := make([]byte, 1024)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := os.WriteFile(inputFile, data, 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}

	// Generate par2 files: -n1 creates a single recovery file.
	createCmd := exec.Command("par2", "c", "-n1", filepath.Join(dir, "set"), inputFile)
	createCmd.Dir = dir
	out, err := createCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("par2 create failed: %v\n%s", err, out)
	}

	// Locate the main (non-volume) par2 file.
	sets, err := par2.FindPar2Files(dir)
	if err != nil || len(sets) == 0 {
		t.Fatalf("FindPar2Files after create: err=%v sets=%v", err, sets)
	}
	mainFile := sets[0].MainFile
	if mainFile == "" {
		// Fall back to first extra if no non-volume file was created.
		mainFile = sets[0].ExtraFiles[0]
	}

	ctx := context.Background()

	t.Run("verify_good_set", func(t *testing.T) {
		res, err := par2.Verify(ctx, mainFile)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if res.Status != par2.StatusAllFilesOK {
			t.Errorf("Status = %v, want StatusAllFilesOK\nstdout: %s\nstderr: %s",
				res.Status, res.Stdout, res.Stderr)
		}
	})

	t.Run("verify_corrupt_set", func(t *testing.T) {
		// Corrupt a single byte in the middle of the input file.
		corrupt := make([]byte, len(data))
		copy(corrupt, data)
		corrupt[512] ^= 0xFF
		if err := os.WriteFile(inputFile, corrupt, 0o644); err != nil {
			t.Fatalf("write corrupt: %v", err)
		}
		t.Cleanup(func() {
			// Restore for the repair sub-test.
			if err := os.WriteFile(inputFile, corrupt, 0o644); err != nil {
				t.Logf("cleanup write: %v", err)
			}
		})

		res, err := par2.Verify(ctx, mainFile)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		// par2 may report RepairRequired or RepairPossible on corruption.
		if res.Status != par2.StatusRepairRequired && res.Status != par2.StatusRepairPossible {
			t.Errorf("Status = %v, want RepairRequired or RepairPossible\nstdout: %s\nstderr: %s",
				res.Status, res.Stdout, res.Stderr)
		}
	})

	t.Run("repair_corrupt_set", func(t *testing.T) {
		// Ensure file is corrupted (in case sub-tests run in isolation).
		corrupt := make([]byte, len(data))
		copy(corrupt, data)
		corrupt[512] ^= 0xFF
		if err := os.WriteFile(inputFile, corrupt, 0o644); err != nil {
			t.Fatalf("write corrupt: %v", err)
		}

		res, err := par2.Repair(ctx, mainFile)
		if err != nil {
			t.Fatalf("Repair: %v", err)
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
