package par2

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// distinctBlocks returns n blocks of sliceSize bytes, no two alike, so a scan
// cannot find a damaged block's content at another block's offset.
func distinctBlocks(n, sliceSize int) []byte {
	out := make([]byte, 0, n*sliceSize)
	for i := range n {
		for j := range sliceSize {
			out = append(out, byte(i*31+j*7+1))
		}
	}
	return out
}

// TestGoVerify_BlocksNeeded pins the two figures GoVerify adds to its result:
// BlocksNeeded counts the damaged blocks of an index with no recovery volume,
// and DataBlocks counts every data block, damaged or not.
func TestGoVerify_BlocksNeeded(t *testing.T) {
	t.Parallel()
	const sliceSize, blocks = 16, 6
	intact := distinctBlocks(blocks, sliceSize)

	for _, tc := range []struct {
		name    string
		damaged []int
	}{
		{"intact", nil},
		{"one block", []int{2}},
		{"three blocks", []int{0, 3, 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			mainFile := buildMinimalPar2Index(t, dir, "blocks.bin", intact, sliceSize)
			data := slices.Clone(intact)
			for _, b := range tc.damaged {
				data[b*sliceSize] ^= 0xFF
			}
			writeFile(t, dir, "blocks.bin", data)

			res, err := GoVerify(context.Background(), discardLogger(), mainFile, candidatesIn(t, dir), nil)
			if err != nil {
				t.Fatalf("GoVerify: %v", err)
			}
			if res.BlocksNeeded != len(tc.damaged) {
				t.Errorf("BlocksNeeded = %d, want %d\nStdout: %s", res.BlocksNeeded, len(tc.damaged), res.Stdout)
			}
			if res.DataBlocks != blocks {
				t.Errorf("DataBlocks = %d, want %d\nStdout: %s", res.DataBlocks, blocks, res.Stdout)
			}
		})
	}
}

// TestGoVerify_BlocksNeeded_IsNetOfRecoveryOnDisk pins that a damaged block
// the recovery volumes on disk can cover leaves BlocksNeeded at 0.
func TestGoVerify_BlocksNeeded_IsNetOfRecoveryOnDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mainFile := copyPar2Fixtures(t, dir)
	data, err := os.ReadFile(filepath.Join(dir, "data.bin"))
	if err != nil {
		t.Fatalf("read data.bin: %v", err)
	}
	data[20] ^= 0xFF // inside the text header, so no shifted scan finds the block elsewhere
	writeFile(t, dir, "data.bin", data)

	res, err := GoVerify(context.Background(), discardLogger(), mainFile, candidatesIn(t, dir), nil)
	if err != nil {
		t.Fatalf("GoVerify: %v", err)
	}
	if res.Status != StatusRepairPossible || res.BlocksNeeded != 0 {
		t.Errorf("Status = %v, BlocksNeeded = %d, want StatusRepairPossible and 0\nStdout: %s",
			res.Status, res.BlocksNeeded, res.Stdout)
	}
	if res.DataBlocks == 0 {
		t.Errorf("DataBlocks = 0, want the set's data blocks\nStdout: %s", res.Stdout)
	}
}

func TestCandidateFiles(t *testing.T) {
	t.Parallel()

	t.Run("lists data files, not par2 files or directories", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		want := []string{filepath.Join(dir, "data.bin"), filepath.Join(dir, "movie.mkv")}
		for _, p := range want {
			writeFile(t, dir, filepath.Base(p), []byte("x"))
		}
		writeFile(t, dir, "data.par2", []byte("x"))
		writeFile(t, dir, "data.vol001+10.PAR2", []byte("x"))
		if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}

		got, err := CandidateFiles(dir)
		if err != nil {
			t.Fatalf("CandidateFiles: %v", err)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("CandidateFiles = %v, want %v", got, want)
		}
	})

	t.Run("empty directory", func(t *testing.T) {
		t.Parallel()
		got, err := CandidateFiles(t.TempDir())
		if err != nil || len(got) != 0 {
			t.Errorf("CandidateFiles = %v, %v, want none", got, err)
		}
	})

	t.Run("missing directory", func(t *testing.T) {
		t.Parallel()
		if _, err := CandidateFiles(filepath.Join(t.TempDir(), "absent")); err == nil {
			t.Error("CandidateFiles on a missing directory returned no error")
		}
	})
}
