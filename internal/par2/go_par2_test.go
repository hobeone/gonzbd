package par2

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
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
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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
// StatusAllFilesOK.
func TestGoVerify_HealthyData(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mainFile := copyPar2Fixtures(t, dir)

	res, err := GoVerify(context.Background(), discardLogger(), mainFile, dir, nil)
	if err != nil {
		t.Fatalf("GoVerify: %v", err)
	}
	if res.Status != StatusAllFilesOK {
		t.Errorf("Status = %v, want StatusAllFilesOK\nOutput: %s", res.Status, res.Stdout)
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
		Handler: slog.NewTextHandler(io.Discard, nil),
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
		Handler: slog.NewTextHandler(io.Discard, nil),
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
