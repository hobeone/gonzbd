package par2

import (
	"bytes"
	"context"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// corruptButAllBlocksPresent builds the par2 fixture set in a fresh directory
// and damages data.bin so that the engine can still locate every one of its
// blocks somewhere: a damaged file that needs no recovery data, only a
// rebuild from the blocks it located.
type corruptButAllBlocksPresent struct {
	name   string
	damage func(t *testing.T, dir string, intact []byte)
}

var corruptButAllBlocksPresentCases = []corruptButAllBlocksPresent{
	{
		// An intact copy under an obfuscated name holds every block, so the
		// damaged data.bin's missing block reads as "present".
		name: "intact copy under another name",
		damage: func(t *testing.T, dir string, intact []byte) {
			t.Helper()
			writeFile(t, dir, "a8f3c0d2e1", intact)
			damaged := bytes.Clone(intact)
			damaged[len(damaged)/2] ^= 0xFF
			writeFile(t, dir, "data.bin", damaged)
		},
	},
	{
		// Bytes inserted mid-file shift the tail, but the sliding scan still
		// finds every block at its new offset inside data.bin itself.
		name: "bytes inserted mid-file",
		damage: func(t *testing.T, dir string, intact []byte) {
			t.Helper()
			mid := len(intact) / 2
			damaged := make([]byte, 0, len(intact)+777)
			damaged = append(damaged, intact[:mid]...)
			damaged = append(damaged, bytes.Repeat([]byte{0xA5}, 777)...)
			damaged = append(damaged, intact[mid:]...)
			writeFile(t, dir, "data.bin", damaged)
		},
	},
}

func setUpCorruptCase(t *testing.T, tc corruptButAllBlocksPresent) (dir, mainFile string, intact []byte) {
	t.Helper()
	dir = t.TempDir()
	mainFile = copyPar2Fixtures(t, dir)
	intact, err := os.ReadFile(filepath.Join(dir, "data.bin"))
	if err != nil {
		t.Fatalf("read data.bin: %v", err)
	}
	tc.damage(t, dir, intact)
	return dir, mainFile, intact
}

// TestGoRepair_CorruptFileWithAllBlocksPresent_IsRepairedNatively pins that
// GoRepair rebuilds a damaged file whose every block was located elsewhere,
// and reports success only with the file's original bytes on disk. A run
// that leaves the file damaged must not succeed: success suppresses the
// external par2 fallback and files the payload as repaired.
func TestGoRepair_CorruptFileWithAllBlocksPresent_IsRepairedNatively(t *testing.T) {
	t.Parallel()
	for _, tc := range corruptButAllBlocksPresentCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, mainFile, intact := setUpCorruptCase(t, tc)

			res, err := GoRepair(context.Background(), discardLogger(), mainFile, candidatesIn(t, dir), nil)
			if err != nil {
				t.Fatalf("GoRepair: %v", err)
			}
			requireRepairedNatively(t, res, filepath.Join(dir, "data.bin"), intact)
		})
	}
}

// TestGoVerify_CorruptFileWithAllBlocksPresent_IsRepairPossible is GoVerify's
// half: the set needs a rebuild, and the rebuild needs no recovery data.
func TestGoVerify_CorruptFileWithAllBlocksPresent_IsRepairPossible(t *testing.T) {
	t.Parallel()
	for _, tc := range corruptButAllBlocksPresentCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, mainFile, _ := setUpCorruptCase(t, tc)

			res, err := GoVerify(context.Background(), discardLogger(), mainFile, candidatesIn(t, dir), nil)
			if err != nil {
				t.Fatalf("GoVerify: %v", err)
			}
			requireRebuildPossible(t, res)
		})
	}
}

// requireRebuildPossible fails t unless res reports a repair that is possible
// and rebuilds one damaged file.
func requireRebuildPossible(t *testing.T, res VerifyResult) {
	t.Helper()
	if res.Status != StatusRepairPossible {
		t.Errorf("Status = %v, want StatusRepairPossible\nOutput: %s", res.Status, res.Stdout)
	}
	if !strings.Contains(res.Stdout, "files_to_rebuild=1") {
		t.Errorf("Stdout does not report the rebuild: %s", res.Stdout)
	}
}

// requireRepairedNatively fails t unless res is a native success that left
// path holding want, with the rebuild named in the output.
func requireRepairedNatively(t *testing.T, res RepairResult, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s does not hold its original bytes after GoRepair\nOutput: %s", filepath.Base(path), res.Output)
	}
	if !res.Success {
		t.Errorf("Success = false; want the native engine to rebuild the file from its located blocks\nOutput: %s", res.Output)
	}
	if !strings.Contains(res.Output, "1 damaged file(s) to rebuild") {
		t.Errorf("output does not report the rebuild\nOutput: %s", res.Output)
	}
}

// TestGoRepair_DamagedBlockWhoseContentRecurs_IsRepairedNatively covers a
// file whose damaged block holds content that recurs elsewhere in the same
// file, as zero padding and other repetitive payloads do. The scan locates
// the damaged block's correct bytes at another block's offset, so no block
// is missing and the set has no recovery data at all; the file is still
// damaged, and must be rebuilt rather than reported correct.
func TestGoRepair_DamagedBlockWhoseContentRecurs_IsRepairedNatively(t *testing.T) {
	t.Parallel()

	const sliceSize = 16
	intact := slices.Concat(
		make([]byte, sliceSize*3),             // three all-zero blocks
		bytes.Repeat([]byte{0x5A}, sliceSize), // and one other
	)
	damaged := bytes.Clone(intact)
	copy(damaged[sliceSize:2*sliceSize], bytes.Repeat([]byte{0xFF}, sliceSize))

	setUp := func(t *testing.T) (dir, mainFile string) {
		t.Helper()
		dir = t.TempDir()
		mainFile = buildMinimalPar2Index(t, dir, "zeros.bin", intact, sliceSize)
		writeFile(t, dir, "zeros.bin", damaged)
		return dir, mainFile
	}

	t.Run("GoRepair", func(t *testing.T) {
		t.Parallel()
		dir, mainFile := setUp(t)
		res, err := GoRepair(context.Background(), discardLogger(), mainFile, candidatesIn(t, dir), nil)
		if err != nil {
			t.Fatalf("GoRepair: %v", err)
		}
		requireRepairedNatively(t, res, filepath.Join(dir, "zeros.bin"), intact)
	})
	t.Run("GoVerify", func(t *testing.T) {
		t.Parallel()
		dir, mainFile := setUp(t)
		res, err := GoVerify(context.Background(), discardLogger(), mainFile, candidatesIn(t, dir), nil)
		if err != nil {
			t.Fatalf("GoVerify: %v", err)
		}
		requireRebuildPossible(t, res)
	})
}

// misnamedOnly builds the par2 fixture set in a fresh directory with data.bin
// moved, intact, to an obfuscated name: a set whose only repair is a rename.
func misnamedOnly(t *testing.T) (dir, mainFile string, intact []byte) {
	t.Helper()
	dir = t.TempDir()
	mainFile = copyPar2Fixtures(t, dir)
	intact, err := os.ReadFile(filepath.Join(dir, "data.bin"))
	if err != nil {
		t.Fatalf("read data.bin: %v", err)
	}
	if err := os.Rename(filepath.Join(dir, "data.bin"), filepath.Join(dir, "a8f3c0d2e1")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	return dir, mainFile, intact
}

// TestGoVerify_RenameOnly_ReportsTheRename pins that a set needing only a
// rename is reported as a possible repair, counted as a rename and not as a
// rebuild, in both Stdout and the summary line.
func TestGoVerify_RenameOnly_ReportsTheRename(t *testing.T) {
	t.Parallel()
	dir, mainFile, _ := misnamedOnly(t)

	var lines []string
	res, err := GoVerify(context.Background(), discardLogger(), mainFile, candidatesIn(t, dir), func(l string) {
		lines = append(lines, l)
	})
	if err != nil {
		t.Fatalf("GoVerify: %v", err)
	}
	if res.Status != StatusRepairPossible {
		t.Errorf("Status = %v, want StatusRepairPossible\nOutput: %s", res.Status, res.Stdout)
	}
	for _, want := range []string{"files_to_rename=1", "files_to_rebuild=0"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("Stdout lacks %q: %s", want, res.Stdout)
		}
	}
	if !slices.ContainsFunc(lines, func(l string) bool {
		return strings.Contains(l, "Repair needed:") && strings.Contains(l, "1 misnamed file(s) to rename")
	}) {
		t.Errorf("no summary line reports the rename: %v", lines)
	}
}

// TestGoRepair_RenameOnly_RestoresTheName pins GoRepair's half: data.bin is
// back under its own name with its original bytes.
func TestGoRepair_RenameOnly_RestoresTheName(t *testing.T) {
	t.Parallel()
	dir, mainFile, intact := misnamedOnly(t)

	res, err := GoRepair(context.Background(), discardLogger(), mainFile, candidatesIn(t, dir), nil)
	if err != nil {
		t.Fatalf("GoRepair: %v", err)
	}
	if !res.Success {
		t.Errorf("Success = false\nOutput: %s", res.Output)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "data.bin")); err != nil || !bytes.Equal(got, intact) {
		t.Errorf("data.bin does not hold its original bytes (err=%v)\nOutput: %s", err, res.Output)
	}
}

// noisePayload returns n deterministic pseudo-random bytes for seed, with no
// repeating structure for the engine's block search to find.
func noisePayload(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.UintN(256))
	}
	return b
}

// TestGoRepair_ParityAndRebuildInOneSet covers a set where one file needs
// recovery data and another needs only a rebuild. testdata/mixed holds a
// par2cmdline 0.8.1 set made with
//
//	par2 create -s1024 -c2 -n1 set.par2 a.bin b.bin
//
// over a.bin = noisePayload(0x10, 8192) and b.bin = noisePayload(0x60, 4096).
// The payload is noise because a periodic one repeats each block's content
// elsewhere in the file, which lets the engine rebuild a flipped block without
// recovery data. Here a.bin has one mid-block byte flipped, so one block is
// missing and parity rebuilds it, and
// b.bin is missing while its bytes sit inside container.bin after a header,
// so every block of it is located but not as a rename.
func TestGoRepair_ParityAndRebuildInOneSet(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"set.par2", "set.vol0+2.par2"} {
		body, err := os.ReadFile(filepath.Join("testdata", "mixed", name))
		if err != nil {
			t.Fatalf("read fixture %s: %v", name, err)
		}
		writeFile(t, dir, name, body)
	}
	a, b := noisePayload(0x10, 8192), noisePayload(0x60, 4096)
	damaged := bytes.Clone(a)
	damaged[2048+512] ^= 0xFF
	writeFile(t, dir, "a.bin", damaged)
	writeFile(t, dir, "container.bin", append([]byte("HEADER-NOT-PART-OF-PAYLOAD-"), b...))

	res, err := GoRepair(context.Background(), discardLogger(), filepath.Join(dir, "set.par2"), candidatesIn(t, dir), nil)
	if err != nil {
		t.Fatalf("GoRepair: %v\nOutput: %s", err, res.Output)
	}
	if !res.Success {
		t.Errorf("Success = false\nOutput: %s", res.Output)
	}
	for name, want := range map[string][]byte{"a.bin": a, "b.bin": b} {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s does not hold its original bytes (err=%v)", name, err)
		}
	}
	if !strings.Contains(res.Output, "Repair required: 1 missing blocks, 2 damaged file(s) to rebuild") {
		t.Errorf("output does not report both the missing block and the rebuilds\nOutput: %s", res.Output)
	}
}

// symlinkAtProtectedName builds the par2 fixture set in a fresh directory and
// replaces data.bin with a symlink to an intact copy in the same directory.
// par2engine opens the name through os.Root, which follows a link whose
// target resolves inside the directory, so the engine finds nothing to
// repair; gonzbd's own check requires a regular file at the name.
func symlinkAtProtectedName(t *testing.T) (dir, mainFile string) {
	t.Helper()
	dir = t.TempDir()
	mainFile = copyPar2Fixtures(t, dir)
	if err := os.Rename(filepath.Join(dir, "data.bin"), filepath.Join(dir, "payload")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.Symlink("payload", filepath.Join(dir, "data.bin")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	return dir, mainFile
}

// TestGoRepair_SymlinkAtProtectedName_IsNotSuccess pins that GoRepair's clean
// verdict is protectedFilesIntact's as well as the engine's: the engine
// reports nothing to repair here, and only gonzbd's check refuses.
func TestGoRepair_SymlinkAtProtectedName_IsNotSuccess(t *testing.T) {
	t.Parallel()
	dir, mainFile := symlinkAtProtectedName(t)

	res, err := GoRepair(context.Background(), discardLogger(), mainFile, candidatesIn(t, dir), nil)
	if err != nil {
		t.Fatalf("GoRepair: %v", err)
	}
	if res.Success {
		t.Errorf("Success = true with a symlink at data.bin\nOutput: %s", res.Output)
	}
	if !strings.Contains(res.Output, `Engine reported "All files are correct", but`) {
		t.Errorf("output does not show the engine's clean verdict being overruled\nOutput: %s", res.Output)
	}
}

// TestGoVerify_SymlinkAtProtectedName_IsNotAllFilesOK is GoVerify's half.
func TestGoVerify_SymlinkAtProtectedName_IsNotAllFilesOK(t *testing.T) {
	t.Parallel()
	dir, mainFile := symlinkAtProtectedName(t)

	res, err := GoVerify(context.Background(), discardLogger(), mainFile, candidatesIn(t, dir), nil)
	if err != nil {
		t.Fatalf("GoVerify: %v", err)
	}
	if res.Status != StatusRepairRequired {
		t.Errorf("Status = %v, want StatusRepairRequired\nOutput: %s", res.Status, res.Stdout)
	}
}

// ---------- protectedFilesIntact ----------

// TestProtectedFilesIntact_RegularFilePassesWithoutHashing pins that the
// check judges what is at each name and leaves content to the engine: the
// bytes here do not match the index, and the check still passes.
func TestProtectedFilesIntact_RegularFilePassesWithoutHashing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mainFile := buildMinimalPar2Index(t, dir, "present.bin", bytes.Repeat([]byte{0x11}, 32), 16)
	writeFile(t, dir, "present.bin", bytes.Repeat([]byte{0x12}, 32))

	if problem := protectedFilesIntact(mainFile); problem != "" {
		t.Errorf("problem = %q; want none for a regular file at the protected name", problem)
	}
}

func TestProtectedFilesIntact_Missing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mainFile := buildMinimalPar2Index(t, dir, "absent.bin", bytes.Repeat([]byte{0x22}, 32), 16)

	problem := protectedFilesIntact(mainFile)
	if !strings.Contains(problem, "absent.bin (missing)") {
		t.Errorf("problem = %q; want it to name absent.bin as missing", problem)
	}
}

func TestProtectedFilesIntact_SymlinkIsMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	payload := bytes.Repeat([]byte{0x55}, 32)
	mainFile := buildMinimalPar2Index(t, dir, "link.bin", payload, 16)
	writeFile(t, dir, "target.bin", payload)
	if err := os.Symlink("target.bin", filepath.Join(dir, "link.bin")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if problem := protectedFilesIntact(mainFile); !strings.Contains(problem, "link.bin (missing)") {
		t.Errorf("problem = %q; want a symlink at the protected name to read as missing", problem)
	}
}

// TestProtectedFilesIntact_IndexUnreadable_Refuses pins that a check which
// cannot run is a refusal, not a pass.
func TestProtectedFilesIntact_IndexUnreadable_Refuses(t *testing.T) {
	t.Parallel()
	missingIndex := filepath.Join(t.TempDir(), "does-not-exist.par2")

	if problem := protectedFilesIntact(missingIndex); problem == "" {
		t.Error("problem is empty for an index that could not be read; want a refusal")
	}
}
