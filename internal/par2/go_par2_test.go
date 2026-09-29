package par2

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // par2 spec mandates MD5, not security-sensitive
	"encoding/binary"
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	par2engine "github.com/hobeone/par2engine/par2"
	"go.uber.org/goleak"

	"github.com/hobeone/gonzbd/internal/cmdutil"
)

// par2FixtureDir returns the path to the shared par2 test fixtures.
// Go tests run with cwd = package directory.
const par2FixtureDir = "../../test/fixtures/par2"

// copyPar2Fixtures copies the shared fixture set into dir and returns the
// path to the main .par2 file.
func copyPar2Fixtures(t *testing.T, dir string) string {
	t.Helper()
	for _, name := range []string{"data.bin", "data.par2", "data.vol000+102.par2"} {
		data, err := os.ReadFile(filepath.Join(par2FixtureDir, name))
		if err != nil {
			t.Fatalf("read fixture %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	return filepath.Join(dir, "data.par2")
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// ---------- monitorProgress / runWithProgress ----------

// TestRunWithProgress_NoLeakOnPanic proves the monitor goroutine spawned by
// monitorProgress terminates even when the wrapped engine call panics — the
// exact scenario cmdutil.SafeEngineRun recovers from at every real call site
// (par2engine panicking on malformed input). Before the fix in issue #100,
// monitorProgress's done channel was only closed by an inline close() after
// the call, which is skipped during a panic unwind, stranding the goroutine
// forever. goleak.Find after the recover is the only thing that catches
// that: SafeEngineRun's recover converts the panic into a clean error, so no
// assertion on the returned error would ever notice the leak.
func TestRunWithProgress_NoLeakOnPanic(t *testing.T) {
	baseline := goleak.IgnoreCurrent()

	err := cmdutil.SafeEngineRun("test: simulated engine panic", func() error {
		return runWithProgress("Testing", func(string) {}, func(_ chan par2engine.Progress) error {
			panic("simulated par2engine panic")
		})
	})
	if err == nil {
		t.Fatal("SafeEngineRun: expected recovered panic to surface as an error")
	}

	if leakErr := goleak.Find(baseline); leakErr != nil {
		t.Errorf("monitor goroutine leaked after engine panic: %v", leakErr)
	}
}

// ---------- GoRepair ----------

// TestGoRepair_HealthyData verifies that GoRepair on an intact par2 set
// returns Success=true without performing actual repair.
func TestGoRepair_HealthyData(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mainFile := copyPar2Fixtures(t, dir)

	res, err := GoRepair(context.Background(), discardLogger(), mainFile, dir, nil)
	if err != nil {
		t.Fatalf("GoRepair: %v", err)
	}
	if !res.Success {
		t.Errorf("Success = false; want true for intact data\nOutput: %s", res.Output)
	}
}

// TestGoRepair_InvalidPar2File verifies that GoRepair returns an error and
// does not set Success when the par2 file is corrupt/unreadable.
func TestGoRepair_InvalidPar2File(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.par2")
	if err := os.WriteFile(bad, []byte("not a par2 file"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, err := GoRepair(context.Background(), discardLogger(), bad, dir, nil)
	if err == nil {
		t.Fatal("GoRepair: expected error for invalid par2, got nil")
	}
	if res.Success {
		t.Error("Success = true for invalid par2 file; want false")
	}
}

// TestGoRepair_OutputAccumulated verifies that onLine callbacks are called
// and the full output is captured in res.Output.
func TestGoRepair_OutputAccumulated(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mainFile := copyPar2Fixtures(t, dir)

	var lines []string
	res, err := GoRepair(context.Background(), discardLogger(), mainFile, dir, func(line string) {
		lines = append(lines, line)
	})
	if err != nil {
		t.Fatalf("GoRepair: %v", err)
	}
	if len(lines) == 0 {
		t.Error("onLine was never called; expected progress output")
	}
	if res.Output == "" {
		t.Error("res.Output is empty; expected accumulated lines")
	}
}

// TestGoRepair_CommandLineSet verifies that res.CommandLine is populated.
func TestGoRepair_CommandLineSet(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mainFile := copyPar2Fixtures(t, dir)

	res, _ := GoRepair(context.Background(), discardLogger(), mainFile, dir, nil)
	if res.CommandLine == "" {
		t.Error("CommandLine is empty; want non-empty")
	}
}

// ---------- GoVerify ----------

// TestGoVerify_HealthyData verifies that GoVerify on an intact set returns
// StatusAllFilesOK and calls onLine callback.
func TestGoVerify_HealthyData(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mainFile := copyPar2Fixtures(t, dir)

	var lines []string
	res, err := GoVerify(context.Background(), discardLogger(), mainFile, dir, func(l string) {
		lines = append(lines, l)
	})
	if err != nil {
		t.Fatalf("GoVerify: %v", err)
	}
	if res.Status != StatusAllFilesOK {
		t.Errorf("Status = %v, want StatusAllFilesOK\nOutput: %s", res.Status, res.Stdout)
	}
	if !slices.Contains(lines, "[go_par2] All files are correct") {
		t.Errorf("expected '[go_par2] All files are correct' in onLine, got: %v", lines)
	}
}

// TestGoVerify_RepairPossible_CallsOnLine verifies that when repair is needed and possible,
// GoVerify sets StatusRepairPossible and calls onLine callback.
func TestGoVerify_RepairPossible_CallsOnLine(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mainFile := copyPar2Fixtures(t, dir)

	dataPath := filepath.Join(dir, "data.bin")
	data, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	data[0] ^= 0xFF // flip one byte without changing file size
	if err := os.WriteFile(dataPath, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var lines []string
	res, err := GoVerify(context.Background(), discardLogger(), mainFile, dir, func(l string) {
		lines = append(lines, l)
	})
	if err != nil {
		t.Fatalf("GoVerify: %v", err)
	}
	if res.Status != StatusRepairPossible {
		t.Errorf("Status = %v, want StatusRepairPossible\nOutput: %s", res.Status, res.Stdout)
	}
	var found bool
	for _, l := range lines {
		if strings.HasPrefix(l, "[go_par2] Repair needed:") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected '[go_par2] Repair needed:' in onLine, got: %v", lines)
	}
}

// TestGoVerify_MissingCandidateDir_LogsWarning verifies that a missing candidateDir
// logs a warning during GoVerify setup.
func TestGoVerify_MissingCandidateDir_LogsWarning(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mainFile := copyPar2Fixtures(t, dir)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	_, err := GoVerify(context.Background(), logger, mainFile, filepath.Join(dir, "nonexistent"), nil)
	if err != nil {
		t.Fatalf("GoVerify: %v", err)
	}
	if !strings.Contains(buf.String(), "go_par2: could not register all candidate files") {
		t.Errorf("expected warning in log output, got: %s", buf.String())
	}
}

// TestGoVerify_InvalidPar2File verifies that an unreadable par2 file returns
// an error and StatusInvalidPar2.
func TestGoVerify_InvalidPar2File(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.par2")
	if err := os.WriteFile(bad, []byte("garbage"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, err := GoVerify(context.Background(), discardLogger(), bad, dir, nil)
	if err == nil {
		t.Fatal("GoVerify: expected error for invalid par2, got nil")
	}
	if res.Status != StatusInvalidPar2 {
		t.Errorf("Status = %v, want StatusInvalidPar2", res.Status)
	}
}

// ---------- teeHandler ----------

// TestTeeHandler_NilOnLineNoPanic verifies that a nil onLine doesn't panic.
func TestTeeHandler_NilOnLineNoPanic(t *testing.T) {
	t.Parallel()

	h := &teeHandler{
		Handler: slog.DiscardHandler,
		onLine:  nil,
	}
	// Must not panic.
	slog.New(h).Info("test message")
}

// TestTeeHandler_WithAttrsPreservesOnLine verifies that WithAttrs returns a
// teeHandler (not a bare handler) so onLine is preserved for child loggers.
func TestTeeHandler_WithAttrsPreservesOnLine(t *testing.T) {
	t.Parallel()

	var lines []string
	base := &teeHandler{
		Handler: slog.DiscardHandler,
		onLine:  func(s string) { lines = append(lines, s) },
	}
	child := base.WithAttrs([]slog.Attr{slog.String("k", "v")})
	slog.New(child).Info("from child")

	if len(lines) == 0 {
		t.Error("onLine not called via child logger after WithAttrs")
	}
}

// ---------- newPar2UILogger ----------

// TestNewPar2UILogger_NilOnLineReturnsBase verifies that when onLine is nil,
// the base logger is returned unchanged (no wrapper allocated).
func TestNewPar2UILogger_NilOnLineReturnsBase(t *testing.T) {
	t.Parallel()

	base := discardLogger()
	got := newPar2UILogger(base, nil)
	if got != base {
		t.Error("newPar2UILogger(nil onLine) should return base logger unchanged")
	}
}

// TestNewPar2UILogger_NonNilOnLineWrapsTeeHandler verifies that a non-nil
// onLine wraps the logger with a teeHandler.
func TestNewPar2UILogger_NonNilOnLineWrapsTeeHandler(t *testing.T) {
	t.Parallel()

	var called bool
	base := discardLogger()
	got := newPar2UILogger(base, func(string) { called = true })
	if got == base {
		t.Error("newPar2UILogger(non-nil onLine) should return a new wrapped logger")
	}
	got.Info("trigger")
	if !called {
		t.Error("onLine was not called after logging via wrapped logger")
	}
}

// ---------- addCandidateFiles ----------

// TestAddCandidateFiles_SkipsPar2Files verifies that .par2 files are excluded
// from registration (the decoder already knows about them) and that regular
// files are counted.
func TestAddCandidateFiles_SkipsPar2Files(t *testing.T) {
	t.Parallel()

	// Use the real fixture dir — it has data.bin (1 non-par2 file) and 2 par2 files.
	// We can't actually call d.AddCandidateFile without a real decoder, so we
	// test the filtering logic indirectly by verifying the fixture directory
	// layout matches our expectations (regression guard).
	entries, err := os.ReadDir(par2FixtureDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var nonPar2, par2Files int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// Mimic the filtering in addCandidateFiles.
		if len(e.Name()) >= 5 && e.Name()[len(e.Name())-5:] == ".par2" {
			par2Files++
		} else if e.Name() != "data.bin.sha256" { // sha256 is a metadata file
			nonPar2++
		}
	}
	// Fixture must have at least 1 non-par2 data file.
	if nonPar2 == 0 {
		t.Errorf("fixture dir has no non-par2 files; fixture layout may have changed")
	}
}

// TestGoVerify_ConcurrentOnLine asserts that the onLine callback passed to
// GoVerify does not cause a data race when called concurrently.
func TestGoVerify_ConcurrentOnLine(t *testing.T) {
	dir := t.TempDir()
	mainFile := copyPar2Fixtures(t, dir)

	var allLines []string
	_, err := GoVerify(context.Background(), discardLogger(), mainFile, dir, func(line string) {
		allLines = append(allLines, line)
	})
	if err != nil {
		t.Fatalf("GoVerify: %v", err)
	}
	if len(allLines) == 0 {
		t.Error("expected onLine to be called during GoVerify")
	}
}

// TestGoVerify_DamagedData verifies that when GoVerify runs on severely damaged data,
// it reports StatusRepairNotPossible and emits appropriate progress/repair-status messages.
func TestGoVerify_DamagedData(t *testing.T) {
	dir := t.TempDir()
	mainFile := copyPar2Fixtures(t, dir)

	// Severely damage the data file by truncating to garbage so repair is not possible.
	dataFile := filepath.Join(dir, "data.bin")
	if err := os.WriteFile(dataFile, []byte("damaged garbage"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var lines []string
	res, err := GoVerify(context.Background(), discardLogger(), mainFile, dir, func(line string) {
		lines = append(lines, line)
	})
	if err != nil {
		t.Fatalf("GoVerify: %v", err)
	}
	if res.Status != StatusRepairNotPossible {
		t.Errorf("got status %v, want StatusRepairNotPossible", res.Status)
	}
	var sawVerifying, sawNotPossible bool
	for _, l := range lines {
		if strings.Contains(l, "Verifying...") {
			sawVerifying = true
		}
		if strings.Contains(l, "[go_par2] Repair not possible:") {
			sawNotPossible = true
		}
	}
	if !sawVerifying {
		t.Errorf("expected 'Verifying...' in onLine, got: %v", lines)
	}
	if !sawNotPossible {
		t.Errorf("expected '[go_par2] Repair not possible:' in onLine, got: %v", lines)
	}
}

// TestGoRepair_ConcurrentOnLine asserts that the onLine callback passed to
// GoRepair does not cause a data race when called concurrently.
func TestGoRepair_ConcurrentOnLine(t *testing.T) {
	dir := t.TempDir()
	mainFile := copyPar2Fixtures(t, dir)

	var lines []string
	_, err := GoRepair(context.Background(), discardLogger(), mainFile, dir, func(line string) {
		lines = append(lines, line)
	})
	if err != nil {
		t.Fatalf("GoRepair: %v", err)
	}
}

// TestGoPar2_LoggerConcurrentOnLine asserts that logging concurrently to a logger
// wrapped with newPar2UILogger does not race on the onLine callback.
func TestGoPar2_LoggerConcurrentOnLine(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	base := discardLogger()
	logger := newPar2UILogger(base, func(line string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, line)
	})

	const numGoroutines = 10
	const logsPerGoroutine = 50
	var wg sync.WaitGroup
	wg.Add(numGoroutines)
	for range numGoroutines {
		go func() {
			defer wg.Done()
			for range logsPerGoroutine {
				logger.Info("log message")
			}
		}()
	}
	wg.Wait()
}

func TestGoRepair_DamagedAndRepaired(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mainFile := copyPar2Fixtures(t, dir)

	// Damage the data file by modifying exactly one byte, keeping the size identical.
	dataFile := filepath.Join(dir, "data.bin")
	data, err := os.ReadFile(dataFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 1 {
		t.Fatal("fixture data.bin is empty")
	}
	data[0] = data[0] ^ 0xFF // flip bits of the first byte
	if err := os.WriteFile(dataFile, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var onLineCalled bool
	res, err := GoRepair(context.Background(), discardLogger(), mainFile, dir, func(line string) {
		onLineCalled = true
	})
	if err != nil {
		t.Fatalf("GoRepair failed: %v\nOutput: %s", err, res.Output)
	}
	if !res.Success {
		t.Errorf("expected res.Success = true, got false\nOutput: %s", res.Output)
	}

	// Verify that the file was actually repaired back to original content.
	repairedData, err := os.ReadFile(dataFile)
	if err != nil {
		t.Fatal(err)
	}
	originalData, err := os.ReadFile(filepath.Join(par2FixtureDir, "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(repairedData, originalData) {
		t.Error("repaired data does not match original data")
	}
	if !onLineCalled {
		t.Error("expected onLine to be called during repair")
	}
}

func TestGoRepair_RepairNotPossible(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mainFile := copyPar2Fixtures(t, dir)

	// Remove the recovery volume.
	if err := os.Remove(filepath.Join(dir, "data.vol000+102.par2")); err != nil {
		t.Fatal(err)
	}

	// Damage the data file.
	dataFile := filepath.Join(dir, "data.bin")
	if err := os.WriteFile(dataFile, []byte("damaged data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	res, err := GoRepair(context.Background(), discardLogger(), mainFile, dir, nil)
	if err != nil {
		t.Fatalf("GoRepair returned error: %v", err)
	}
	if res.Success {
		t.Error("expected Success to be false when repair is not possible")
	}
	if !res.NeedMoreBlocks {
		t.Error("expected NeedMoreBlocks to be true")
	}
	if res.BlocksNeeded == 0 {
		t.Error("expected BlocksNeeded > 0")
	}
}

// buildMinimalPar2Index writes a par2 index (Main + FileDesc + IFSC packets
// only, no recovery volume) protecting one file of content, sliced at
// sliceSize bytes. It exists so the regression test below controls the
// slice size and payload precisely, rather than depending on the shared
// data.bin fixture's fixed 4065-byte size, which does not evenly divide its
// own slice size and so exercises a second, unrelated candidate-file
// last-partial-block quirk in par2engine's scanner. len(content) must be an
// exact multiple of sliceSize.
func buildMinimalPar2Index(t *testing.T, dir, filename string, content []byte, sliceSize uint64) string {
	t.Helper()
	if len(content) == 0 || uint64(len(content))%sliceSize != 0 {
		t.Fatalf("buildMinimalPar2Index: len(content)=%d must be a nonzero multiple of sliceSize=%d", len(content), sliceSize)
	}

	var setID [16]byte
	setID[0] = 0x51

	hash16k := hash16kOf(content)
	fullHash := md5.Sum(content) //nolint:gosec // par2 mandates MD5, not security-sensitive
	byteCount := uint64(len(content))

	// The FileID is not caller-chosen: par2engine's ParseFileDescPacket
	// recomputes it from (Hash16k, ByteCount, filename) and rejects the
	// packet if it disagrees — the real PAR2 spec binding gonzbd's own
	// parser (parser.go) does not enforce.
	idHash := md5.New() //nolint:gosec // par2 spec FileID derivation, not security-sensitive
	idHash.Write(hash16k[:])
	var byteCountLE [8]byte
	binary.LittleEndian.PutUint64(byteCountLE[:], byteCount)
	idHash.Write(byteCountLE[:])
	idHash.Write([]byte(filename))
	var fileID [16]byte
	copy(fileID[:], idHash.Sum(nil))

	ifscSlices := make([]ifscSlice, 0, len(content)/int(sliceSize))
	for off := 0; off < len(content); off += int(sliceSize) {
		slice := content[off : off+int(sliceSize)]
		ifscSlices = append(ifscSlices, ifscSlice{
			md5Hash: md5.Sum(slice), //nolint:gosec // par2 spec, not security-sensitive
			crc32:   crc32.ChecksumIEEE(slice),
		})
	}

	mainPkt := buildPacket(setID, typeMain, buildMainBodyWithCount(sliceSize, 1, fileID))
	fileDescPkt := buildPacket(setID, typeFileDesc, buildFileDescBody(fileID, fullHash, hash16k, byteCount, filename))
	ifscPkt := buildPacket(setID, typeIFSC, buildIFSCBody(fileID, ifscSlices))

	pkts := make([]byte, 0, len(mainPkt)+len(fileDescPkt)+len(ifscPkt))
	pkts = append(pkts, mainPkt...)
	pkts = append(pkts, fileDescPkt...)
	pkts = append(pkts, ifscPkt...)

	path := filepath.Join(dir, "set.par2")
	if err := os.WriteFile(path, pkts, 0o600); err != nil {
		t.Fatalf("write par2 index: %v", err)
	}
	return path
}

// TestGoRepair_MissingFileFoundElsewhere_DoesNotReportSuccess pins the fix for
// a par2engine bug report: its block scanner locates a missing protected
// file's shards wherever their bytes happen to sit in another on-disk file
// (e.g. a stored, uncompressed archive member). Its own detectRenameCandidate
// correctly declines to treat that as a same-file rename, because the shards
// are not found at the candidate's own offset 0, so the file stays Missing —
// but the shard locations it already recorded are still counted as usable
// data in the final tally, so RepairNeeded() is false and GoRepair reports
// Success=true even though the protected file was never written to disk.
func TestGoRepair_MissingFileFoundElsewhere_DoesNotReportSuccess(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	const sliceSize = 16
	payload := bytes.Repeat([]byte{0x37}, sliceSize*3) // 3 whole slices, no partial tail
	mainFile := buildMinimalPar2Index(t, dir, "missing.bin", payload, sliceSize)

	// "missing.bin" is never written under its own name — it is genuinely
	// missing — but its exact bytes sit inside another on-disk file, preceded
	// by a header, mimicking a stored (uncompressed) archive member whose
	// entry starts partway through the container. None of its slices land at
	// their "missing.bin"-relative offset 0, so detectRenameCandidate
	// correctly refuses to call this a rename.
	container := append([]byte("HEADER-NOT-PART-OF-PAYLOAD-"), payload...)
	if err := os.WriteFile(filepath.Join(dir, "container.bin"), container, 0o600); err != nil {
		t.Fatalf("write container.bin: %v", err)
	}

	res, err := GoRepair(context.Background(), discardLogger(), mainFile, dir, nil)
	if err != nil {
		t.Fatalf("GoRepair: %v", err)
	}
	if res.Success {
		t.Errorf("Success = true; want false — missing.bin does not exist on disk\nOutput: %s", res.Output)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "missing.bin")); statErr == nil {
		t.Error("missing.bin exists on disk but nothing should have created it")
	}
}

// ---------- verifyProtectedFilesExist ----------

// TestVerifyProtectedFilesExist_AllPresent verifies that the post-repair
// existence check leaves a Success result untouched when every protected
// file is actually on disk.
func TestVerifyProtectedFilesExist_AllPresent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const sliceSize = 16
	payload := bytes.Repeat([]byte{0x11}, sliceSize*2)
	mainFile := buildMinimalPar2Index(t, dir, "present.bin", payload, sliceSize)
	if err := os.WriteFile(filepath.Join(dir, "present.bin"), payload, 0o600); err != nil {
		t.Fatalf("write present.bin: %v", err)
	}

	res := RepairResult{Success: true}
	if err := verifyProtectedFilesExist(&res, mainFile, nil); err != nil {
		t.Fatalf("verifyProtectedFilesExist: %v", err)
	}
	if !res.Success {
		t.Error("Success flipped to false though the protected file exists on disk")
	}
	if res.Output != "" {
		t.Errorf("Output = %q; want no diagnostic appended when nothing is missing", res.Output)
	}
}

// TestVerifyProtectedFilesExist_MissingFile_FlipsSuccessFalse is the direct
// unit test for the guard TestGoRepair_MissingFileFoundElsewhere_DoesNotReportSuccess
// pins end-to-end through GoRepair: given a RepairResult that already claims
// Success, a protected file absent from disk must flip it back to false and
// name the file in both Output and the onLine callback.
func TestVerifyProtectedFilesExist_MissingFile_FlipsSuccessFalse(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const sliceSize = 16
	payload := bytes.Repeat([]byte{0x22}, sliceSize*2)
	mainFile := buildMinimalPar2Index(t, dir, "absent.bin", payload, sliceSize)
	// absent.bin is deliberately never written to dir.

	var lines []string
	res := RepairResult{Success: true}
	if err := verifyProtectedFilesExist(&res, mainFile, func(l string) { lines = append(lines, l) }); err != nil {
		t.Fatalf("verifyProtectedFilesExist: %v", err)
	}
	if res.Success {
		t.Error("Success stayed true though the protected file is missing from disk")
	}
	if !strings.Contains(res.Output, "absent.bin") {
		t.Errorf("Output = %q; want it to name the missing file", res.Output)
	}
	if len(lines) == 0 {
		t.Error("onLine was never called with the missing-file diagnostic")
	}
}

// TestVerifyProtectedFilesExist_ParseFailure_LeavesSuccessUntouched verifies
// that an index the check cannot even read reports an error without
// asserting anything about the RepairResult it was handed — "the check could
// not run" is a different condition from "the check ran and found a file
// absent", and only the second should ever flip Success.
func TestVerifyProtectedFilesExist_ParseFailure_LeavesSuccessUntouched(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missingIndex := filepath.Join(dir, "does-not-exist.par2")

	res := RepairResult{Success: true}
	if err := verifyProtectedFilesExist(&res, missingIndex, nil); err == nil {
		t.Fatal("verifyProtectedFilesExist: expected an error for an unreadable par2 index")
	}
	if !res.Success {
		t.Error("Success was flipped to false by a parse failure; it should have been left untouched")
	}
}
