package par2

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// corruptButAllBlocksPresent builds the par2 fixture set in a fresh directory
// and damages data.bin so that par2engine v1.0.7 can still locate every one
// of its blocks — the shape in which ShardCounts.RepairNeeded() reports false
// even though the engine itself logs data.bin as not intact.
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

// TestGoRepair_CorruptFileWithAllBlocksPresent_IsNotSuccess pins that GoRepair
// never reports success while a protected file's full-file MD5 differs from
// the par2 description. Success here suppresses the external par2 fallback
// and files a corrupt payload as repaired.
func TestGoRepair_CorruptFileWithAllBlocksPresent_IsNotSuccess(t *testing.T) {
	t.Parallel()
	for _, tc := range corruptButAllBlocksPresentCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, mainFile, intact := setUpCorruptCase(t, tc)

			res, err := GoRepair(context.Background(), discardLogger(), mainFile, candidatesIn(t, dir), nil)
			if err != nil {
				t.Fatalf("GoRepair: %v", err)
			}
			got, readErr := os.ReadFile(filepath.Join(dir, "data.bin"))
			if readErr != nil {
				t.Fatalf("read data.bin: %v", readErr)
			}
			if res.Success && !bytes.Equal(got, intact) {
				t.Errorf("Success = true but data.bin does not match its par2 hash\nOutput: %s", res.Output)
			}
			if res.NeedMoreBlocks {
				t.Errorf("NeedMoreBlocks = true; the set has ample recovery data, so the external fallback must stay reachable")
			}
		})
	}
}

// TestGoVerify_CorruptFileWithAllBlocksPresent_IsNotAllFilesOK is GoVerify's
// half of the same verdict.
func TestGoVerify_CorruptFileWithAllBlocksPresent_IsNotAllFilesOK(t *testing.T) {
	t.Parallel()
	for _, tc := range corruptButAllBlocksPresentCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, mainFile, _ := setUpCorruptCase(t, tc)

			res, err := GoVerify(context.Background(), discardLogger(), mainFile, candidatesIn(t, dir), nil)
			if err != nil {
				t.Fatalf("GoVerify: %v", err)
			}
			if res.Status == StatusAllFilesOK {
				t.Errorf("Status = StatusAllFilesOK for a file whose hash does not match\nOutput: %s", res.Stdout)
			}
		})
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
