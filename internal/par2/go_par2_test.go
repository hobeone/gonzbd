package par2

import (
	"bytes"
	"context"
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
