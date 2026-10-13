package par2

import (
	"bytes"
	"context"
	"log/slog"
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

// symlinkAtProtectedName builds the par2 fixture set in a fresh directory and
// replaces data.bin with a symlink to an intact copy. par2engine opens the
// name through os.Root, which follows the link, so the engine finds nothing
// to repair; gonzbd's own check requires a regular file at the name.
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

func newEngineVerified(names ...string) *engineVerified {
	v := &engineVerified{names: make(map[string]struct{})}
	for _, n := range names {
		v.add(n)
	}
	return v
}

func TestProtectedFilesIntact_HashMatches(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	payload := bytes.Repeat([]byte{0x11}, 32)
	mainFile := buildMinimalPar2Index(t, dir, "present.bin", payload, 16)
	writeFile(t, dir, "present.bin", payload)

	if problem := protectedFilesIntact(mainFile, nil); problem != "" {
		t.Errorf("problem = %q; want none for a file whose MD5 matches", problem)
	}
}

func TestProtectedFilesIntact_HashMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	payload := bytes.Repeat([]byte{0x33}, 32)
	mainFile := buildMinimalPar2Index(t, dir, "bad.bin", payload, 16)
	damaged := bytes.Clone(payload)
	damaged[5] ^= 0xFF
	writeFile(t, dir, "bad.bin", damaged)

	problem := protectedFilesIntact(mainFile, newEngineVerified("other.bin"))
	if !strings.Contains(problem, "bad.bin (hash mismatch)") {
		t.Errorf("problem = %q; want it to name bad.bin as a hash mismatch", problem)
	}
}

// TestProtectedFilesIntact_EngineVerifiedIsNotReread pins that a file the
// engine's report logged as OK is not hashed a second time: the bytes on disk
// here do not match, and only the recorded engine verdict makes it pass.
func TestProtectedFilesIntact_EngineVerifiedIsNotReread(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	payload := bytes.Repeat([]byte{0x44}, 32)
	mainFile := buildMinimalPar2Index(t, dir, "seen.bin", payload, 16)
	writeFile(t, dir, "seen.bin", bytes.Repeat([]byte{0x45}, 32))

	if problem := protectedFilesIntact(mainFile, newEngineVerified("seen.bin")); problem != "" {
		t.Errorf("problem = %q; want none for a file the engine verified", problem)
	}
}

func TestProtectedFilesIntact_Missing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mainFile := buildMinimalPar2Index(t, dir, "absent.bin", bytes.Repeat([]byte{0x22}, 32), 16)

	// Engine-verified does not excuse a file that is not on disk.
	problem := protectedFilesIntact(mainFile, newEngineVerified("absent.bin"))
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

	if problem := protectedFilesIntact(mainFile, nil); !strings.Contains(problem, "link.bin (missing)") {
		t.Errorf("problem = %q; want a symlink at the protected name to read as missing", problem)
	}
}

func TestProtectedFilesIntact_Unreadable(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0 file")
	}
	dir := t.TempDir()
	payload := bytes.Repeat([]byte{0x66}, 32)
	mainFile := buildMinimalPar2Index(t, dir, "locked.bin", payload, 16)
	writeFile(t, dir, "locked.bin", payload)
	if err := os.Chmod(filepath.Join(dir, "locked.bin"), 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if problem := protectedFilesIntact(mainFile, nil); !strings.Contains(problem, "locked.bin (unreadable") {
		t.Errorf("problem = %q; want an unreadable file reported, not passed", problem)
	}
}

// TestProtectedFilesIntact_IndexUnreadable_Refuses pins that a check which
// cannot run is a refusal, not a pass.
func TestProtectedFilesIntact_IndexUnreadable_Refuses(t *testing.T) {
	t.Parallel()
	missingIndex := filepath.Join(t.TempDir(), "does-not-exist.par2")

	if problem := protectedFilesIntact(missingIndex, nil); problem == "" {
		t.Error("problem is empty for an index that could not be read; want a refusal")
	}
}

// ---------- verdictRecorder ----------

// TestVerdictRecorder_RecordsEngineOKReport runs the real engine over an
// intact set and requires that its report reached engineVerified. If a
// par2engine upgrade rewords the report, this fails rather than every
// protected file silently being hashed twice.
func TestVerdictRecorder_RecordsEngineOKReport(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mainFile := copyPar2Fixtures(t, dir)

	d, verified, err := newDecoder(context.Background(), discardLogger(), mainFile, candidatesIn(t, dir), nil, nil, nil)
	if err != nil {
		t.Fatalf("newDecoder: %v", err)
	}
	defer d.Close() //nolint:errcheck // read-only
	if err := d.VerifyScans(context.Background(), nil); err != nil {
		t.Fatalf("VerifyScans: %v", err)
	}
	if !verified.has("data.bin") {
		t.Error("engine's OK report for data.bin was not recorded")
	}
}

func TestVerdictRecorder_IgnoresOtherRecordsAndPassesThrough(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	verified := newEngineVerified()
	log := slog.New(&verdictRecorder{
		Handler:  slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}),
		verified: verified,
	}).With("component", "go_par2").WithGroup("g")

	log.Info("File status: OK", "file", "a.bin")
	log.Warn("File status: CORRUPT (hash mismatch, all blocks present)", "file", "b.bin")

	if !verified.has("a.bin") {
		t.Error("OK record was not recorded though the wrapped handler is disabled at Info")
	}
	if verified.has("b.bin") {
		t.Error("a CORRUPT record was recorded as verified")
	}
	if strings.Contains(buf.String(), "a.bin") {
		t.Errorf("an Info record reached a Warn-level handler: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "b.bin") {
		t.Errorf("the Warn record did not reach the wrapped handler: %s", buf.String())
	}
}
