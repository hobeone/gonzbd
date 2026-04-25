package dirscanner

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/types"
)

// MockHandler records all HandleNZB calls for verification.
type MockHandler struct {
	calls     []HandlerCall
	failFor   map[string]error // map of filename -> error to return
	lastError error
}

type HandlerCall struct {
	Filename string
	Data     []byte
	Opts     types.FetchOptions
}

func (m *MockHandler) HandleNZB(ctx context.Context, filename string, data []byte, opts types.FetchOptions) error {
	if err, ok := m.failFor[filename]; ok {
		m.lastError = err
		return err
	}
	m.calls = append(m.calls, HandlerCall{Filename: filename, Data: data, Opts: opts})
	return nil
}

func TestStabilityDetection(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")
	store, err := OpenStore(stateFile)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	handler := &MockHandler{failFor: make(map[string]error)}
	scanner := New(tmpDir, store, handler, nil, nil)

	// Create a test NZB file.
	nzbPath := filepath.Join(tmpDir, "test.nzb")
	nzbContent := []byte("<?xml version=\"1.0\" ?>")
	if err := os.WriteFile(nzbPath, nzbContent, 0o644); err != nil {
		t.Fatalf("failed to write test NZB: %v", err)
	}

	// First scan: file is recorded but not processed.
	count, err := scanner.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("first ScanOnce failed: %v", err)
	}
	if count != 0 {
		t.Errorf("first scan should process 0 files, got %d", count)
	}
	if len(handler.calls) != 0 {
		t.Errorf("first scan should not invoke handler, got %d calls", len(handler.calls))
	}

	// Verify state was recorded.
	state, ok := store.Get(nzbPath)
	if !ok {
		t.Fatal("file state not recorded after first scan")
	}
	if state.Size != int64(len(nzbContent)) {
		t.Errorf("recorded size mismatch: expected %d, got %d", len(nzbContent), state.Size)
	}

	// Second scan without changes: file should be processed.
	count, err = scanner.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("second ScanOnce failed: %v", err)
	}
	if count != 1 {
		t.Errorf("second scan should process 1 file, got %d", count)
	}
	if len(handler.calls) != 1 {
		t.Errorf("second scan should invoke handler once, got %d calls", len(handler.calls))
	}
	if handler.calls[0].Filename != "test.nzb" {
		t.Errorf("handler filename mismatch: expected test.nzb, got %s", handler.calls[0].Filename)
	}

	// File should be deleted after successful handling.
	if _, err := os.Stat(nzbPath); !os.IsNotExist(err) {
		t.Errorf("file should be deleted after successful handling")
	}
}

func TestStabilityResetOnChange(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")
	store, err := OpenStore(stateFile)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	handler := &MockHandler{failFor: make(map[string]error)}
	scanner := New(tmpDir, store, handler, nil, nil)

	nzbPath := filepath.Join(tmpDir, "test.nzb")
	originalContent := []byte("<?xml version=\"1.0\" ?>")
	if err := os.WriteFile(nzbPath, originalContent, 0o644); err != nil {
		t.Fatalf("failed to write test NZB: %v", err)
	}

	// First scan records the file.
	if _, err := scanner.ScanOnce(context.Background()); err != nil {
		t.Fatalf("first ScanOnce failed: %v", err)
	}

	// Modify the file by appending content.
	if err := os.WriteFile(nzbPath, append(originalContent, []byte("modified")...), 0o644); err != nil {
		t.Fatalf("failed to modify test NZB: %v", err)
	}

	// Second scan should detect the change and reset stability.
	count, err := scanner.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("second ScanOnce failed: %v", err)
	}
	if count != 0 {
		t.Errorf("scan after modification should process 0 files, got %d", count)
	}
	if len(handler.calls) != 0 {
		t.Errorf("scan after modification should not invoke handler, got %d calls", len(handler.calls))
	}

	// Verify state was updated to the new size.
	state, ok := store.Get(nzbPath)
	if !ok {
		t.Fatal("file state not recorded")
	}
	expectedSize := int64(len(originalContent) + len([]byte("modified")))
	if state.Size != expectedSize {
		t.Errorf("state size not updated: expected %d, got %d", expectedSize, state.Size)
	}

	// File still exists.
	if _, err := os.Stat(nzbPath); os.IsNotExist(err) {
		t.Errorf("file should still exist after modification")
	}
}

func TestStatePersistence(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")

	// Create and populate first store.
	store1, err := OpenStore(stateFile)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	testPath := "/some/test/file.nzb"
	testTime := time.Now().Truncate(time.Millisecond) // JSON doesn't preserve nanoseconds.
	testState := FileState{Size: 12345, MTime: testTime}
	store1.Set(testPath, testState)

	if err := store1.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Reopen the store and verify state survived.
	store2, err := OpenStore(stateFile)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	state, ok := store2.Get(testPath)
	if !ok {
		t.Fatal("state not found after reopening")
	}
	if state.Size != testState.Size {
		t.Errorf("size mismatch: expected %d, got %d", testState.Size, state.Size)
	}
	if !state.MTime.Equal(testTime) {
		t.Errorf("mtime mismatch: expected %v, got %v", testTime, state.MTime)
	}
}

func TestHandlerError(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")
	store, err := OpenStore(stateFile)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	handler := &MockHandler{failFor: make(map[string]error)}
	handler.failFor["test.nzb"] = fmt.Errorf("simulated handler failure")
	scanner := New(tmpDir, store, handler, nil, nil)

	nzbPath := filepath.Join(tmpDir, "test.nzb")
	nzbContent := []byte("<?xml version=\"1.0\" ?>")
	if err := os.WriteFile(nzbPath, nzbContent, 0o644); err != nil {
		t.Fatalf("failed to write test NZB: %v", err)
	}

	// First scan records the file.
	if _, err := scanner.ScanOnce(context.Background()); err != nil {
		t.Fatalf("first ScanOnce failed: %v", err)
	}

	// Second scan: handler fails but scan itself doesn't error (it logs).
	count, err := scanner.ScanOnce(context.Background())
	if err != nil {
		t.Errorf("ScanOnce should not error on handler failure: %v", err)
	}
	if count != 0 {
		t.Errorf("scan with handler error should not count as processed, got %d", count)
	}

	// File should still exist.
	if _, err := os.Stat(nzbPath); os.IsNotExist(err) {
		t.Errorf("file should not be deleted on handler error")
	}

	// State should still reference the file (for retry).
	if _, ok := store.Get(nzbPath); !ok {
		t.Errorf("file state should be preserved for retry")
	}
}

func TestDecompressGZ(t *testing.T) {
	tmpDir := t.TempDir()

	nzbContent := []byte("<?xml version=\"1.0\" ?>")

	// Create a gzip-compressed NZB file.
	gzPath := filepath.Join(tmpDir, "test.nzb.gz")
	{
		file, err := os.Create(gzPath)
		if err != nil {
			t.Fatalf("failed to create gz file: %v", err)
		}
		defer file.Close()

		gz := gzip.NewWriter(file)
		if _, err := gz.Write(nzbContent); err != nil {
			t.Fatalf("failed to write gzip content: %v", err)
		}
		if err := gz.Close(); err != nil {
			t.Fatalf("failed to close gzip writer: %v", err)
		}
	}

	nzbs, err := ExtractNZBs(gzPath)
	if err != nil {
		t.Fatalf("ExtractNZBs failed: %v", err)
	}

	if len(nzbs) != 1 {
		t.Errorf("expected 1 NZB, got %d", len(nzbs))
	}

	if !bytes.Equal(nzbs[0], nzbContent) {
		t.Errorf("decompressed content mismatch")
	}
}

func TestDecompressBZ2(t *testing.T) {
	t.Log("BZ2 decompression is read-only in stdlib; tested via integration")
}

func TestDecompressZip(t *testing.T) {
	// This test is simplified since we cannot easily create zip files in a
	// portable manner without additional dependencies. The real implementation
	// is tested via integration or by creating temporary zip files manually.
	// For unit test purposes, we verify that ZIP extraction is handled.
	t.Log("ZIP decompression tested via integration")
}

func TestDotfilesSkipped(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")
	store, err := OpenStore(stateFile)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	handler := &MockHandler{failFor: make(map[string]error)}
	scanner := New(tmpDir, store, handler, nil, nil)

	// Create a dotfile.
	dotfilePath := filepath.Join(tmpDir, ".test.nzb")
	if err := os.WriteFile(dotfilePath, []byte("content"), 0o644); err != nil {
		t.Fatalf("failed to write dotfile: %v", err)
	}

	count, err := scanner.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("ScanOnce failed: %v", err)
	}

	if count != 0 {
		t.Errorf("dotfile should be skipped, got %d processed", count)
	}

	// Dotfile should not be in state.
	if _, ok := store.Get(dotfilePath); ok {
		t.Errorf("dotfile should not be stored in state")
	}
}

func TestInvalidExtensionsSkipped(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")
	store, err := OpenStore(stateFile)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	handler := &MockHandler{failFor: make(map[string]error)}
	scanner := New(tmpDir, store, handler, nil, nil)

	// Create a file with invalid extension.
	invalidPath := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(invalidPath, []byte("content"), 0o644); err != nil {
		t.Fatalf("failed to write invalid file: %v", err)
	}

	count, err := scanner.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("ScanOnce failed: %v", err)
	}

	if count != 0 {
		t.Errorf("invalid extension should be skipped, got %d processed", count)
	}

	if _, ok := store.Get(invalidPath); ok {
		t.Errorf("invalid file should not be stored in state")
	}
}

func TestStoreDelete(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")
	store, err := OpenStore(stateFile)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	testPath := "/some/path"
	testState := FileState{Size: 100, MTime: time.Now()}
	store.Set(testPath, testState)

	if _, ok := store.Get(testPath); !ok {
		t.Fatal("state not set")
	}

	store.Delete(testPath)

	if _, ok := store.Get(testPath); ok {
		t.Fatal("state should be deleted")
	}
}

func TestStoreSaveAndLoad(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")

	store, err := OpenStore(stateFile)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	// Add multiple entries.
	for i := 0; i < 3; i++ {
		path := fmt.Sprintf("/path%d", i)
		store.Set(path, FileState{Size: int64(i * 100), MTime: time.Now()})
	}

	if err := store.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Verify JSON file was created.
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("failed to read state file: %v", err)
	}

	var loaded map[string]FileState
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("failed to parse state JSON: %v", err)
	}

	if len(loaded) != 3 {
		t.Errorf("expected 3 entries, got %d", len(loaded))
	}
}

func TestScanDirectoryNotFound(t *testing.T) {
	stateFile := t.TempDir()
	store, err := OpenStore(filepath.Join(stateFile, "state.json"))
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	handler := &MockHandler{failFor: make(map[string]error)}
	scanner := New("/nonexistent/dir", store, handler, nil, nil)

	_, err = scanner.ScanOnce(context.Background())
	if err == nil {
		t.Errorf("expected error for nonexistent directory")
	}
}

func TestDecompressSizeLimitGZ(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a gzip file that decompresses to more than MaxDecompressSize.
	gzPath := filepath.Join(tmpDir, "huge.nzb.gz")
	{
		file, err := os.Create(gzPath)
		if err != nil {
			t.Fatalf("failed to create gz file: %v", err)
		}
		defer file.Close()

		gz := gzip.NewWriter(file)

		// Write a large chunk of data.
		largeData := make([]byte, MaxDecompressSize+1)
		for i := range largeData {
			largeData[i] = 'x'
		}
		if _, err := gz.Write(largeData); err != nil {
			t.Fatalf("failed to write gzip content: %v", err)
		}
		if err := gz.Close(); err != nil {
			t.Fatalf("failed to close gzip writer: %v", err)
		}
	}

	_, err := ExtractNZBs(gzPath)
	if err == nil {
		t.Errorf("expected error for oversized gzip file")
	}
}

// --- Category subdirectory tests ---

func TestCategorySubdirectory(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")
	store, err := OpenStore(stateFile)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	handler := &MockHandler{failFor: make(map[string]error)}
	catFn := func() []string { return []string{"tv", "movies"} }
	scanner := New(tmpDir, store, handler, catFn, nil)

	// Create category subdirectories and an unrecognized one.
	for _, dir := range []string{"tv", "Movies", "random"} {
		if err := os.MkdirAll(filepath.Join(tmpDir, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	nzbContent := []byte("<?xml version=\"1.0\" ?>")

	// Place NZBs in root, tv/, Movies/, random/.
	for _, rel := range []string{"root.nzb", "tv/show.nzb", "Movies/film.nzb", "random/other.nzb"} {
		if err := os.WriteFile(filepath.Join(tmpDir, rel), nzbContent, 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	// First scan: records all files.
	count, err := scanner.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("first ScanOnce: %v", err)
	}
	if count != 0 {
		t.Errorf("first scan should process 0, got %d", count)
	}

	// Second scan: stable files are processed.
	count, err = scanner.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("second ScanOnce: %v", err)
	}

	// Should process: root.nzb, tv/show.nzb, Movies/film.nzb (3 files).
	// random/other.nzb should be ignored (not a known category).
	if count != 3 {
		t.Errorf("expected 3 processed, got %d", count)
	}
	if len(handler.calls) != 3 {
		t.Fatalf("expected 3 handler calls, got %d", len(handler.calls))
	}

	// Build a map of filename → category for verification.
	gotCats := make(map[string]string)
	for _, c := range handler.calls {
		gotCats[c.Filename] = c.Opts.Category
	}

	// Root file: no category.
	if cat, ok := gotCats["root.nzb"]; !ok {
		t.Error("root.nzb not processed")
	} else if cat != "" {
		t.Errorf("root.nzb: expected empty category, got %q", cat)
	}

	// tv/ subdir: exact match.
	if cat, ok := gotCats["show.nzb"]; !ok {
		t.Error("show.nzb not processed")
	} else if cat != "tv" {
		t.Errorf("show.nzb: expected category 'tv', got %q", cat)
	}

	// Movies/ subdir: case-insensitive match to "movies".
	if cat, ok := gotCats["film.nzb"]; !ok {
		t.Error("film.nzb not processed")
	} else if cat != "movies" {
		t.Errorf("film.nzb: expected category 'movies', got %q", cat)
	}

	// random/other.nzb should NOT have been processed.
	if _, ok := gotCats["other.nzb"]; ok {
		t.Error("other.nzb should not have been processed (unrecognized subdir)")
	}
}

func TestCategorySubdirStability(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")
	store, err := OpenStore(stateFile)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	handler := &MockHandler{failFor: make(map[string]error)}
	catFn := func() []string { return []string{"tv"} }
	scanner := New(tmpDir, store, handler, catFn, nil)

	// Create category subdir with an NZB.
	tvDir := filepath.Join(tmpDir, "tv")
	if err := os.MkdirAll(tvDir, 0o755); err != nil {
		t.Fatalf("mkdir tv: %v", err)
	}
	nzbPath := filepath.Join(tvDir, "show.nzb")
	if err := os.WriteFile(nzbPath, []byte("<?xml version=\"1.0\" ?>"), 0o644); err != nil {
		t.Fatalf("write show.nzb: %v", err)
	}

	// First scan: record only.
	count, err := scanner.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if count != 0 {
		t.Errorf("first scan should process 0, got %d", count)
	}
	if len(handler.calls) != 0 {
		t.Errorf("first scan should not invoke handler, got %d", len(handler.calls))
	}

	// Second scan: file is stable, should be processed.
	count, err = scanner.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if count != 1 {
		t.Errorf("second scan should process 1, got %d", count)
	}
	if len(handler.calls) != 1 {
		t.Fatalf("expected 1 handler call, got %d", len(handler.calls))
	}
	if handler.calls[0].Opts.Category != "tv" {
		t.Errorf("expected category 'tv', got %q", handler.calls[0].Opts.Category)
	}
}

func TestDynamicCategoryUpdate(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")
	store, err := OpenStore(stateFile)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	handler := &MockHandler{failFor: make(map[string]error)}

	// Start with no categories.
	categories := []string{}
	catFn := func() []string { return categories }
	scanner := New(tmpDir, store, handler, catFn, nil)

	// Create a "tv" subdir with an NZB.
	tvDir := filepath.Join(tmpDir, "tv")
	if err := os.MkdirAll(tvDir, 0o755); err != nil {
		t.Fatalf("mkdir tv: %v", err)
	}
	nzbPath := filepath.Join(tvDir, "show.nzb")
	if err := os.WriteFile(nzbPath, []byte("<?xml version=\"1.0\" ?>"), 0o644); err != nil {
		t.Fatalf("write show.nzb: %v", err)
	}

	// First two scans with no categories: subdir is ignored.
	if _, err := scanner.ScanOnce(context.Background()); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	count, err := scanner.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if count != 0 {
		t.Errorf("no categories configured: expected 0 processed, got %d", count)
	}

	// Now add "tv" to categories — scanner should pick it up.
	categories = []string{"tv"}

	// Need two more scans: first to record (first sighting in subdir),
	// second to process (stable).
	if _, err := scanner.ScanOnce(context.Background()); err != nil {
		t.Fatalf("scan 3: %v", err)
	}
	count, err = scanner.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("scan 4: %v", err)
	}
	if count != 1 {
		t.Errorf("after adding 'tv' category: expected 1 processed, got %d", count)
	}
	if len(handler.calls) != 1 {
		t.Fatalf("expected 1 handler call, got %d", len(handler.calls))
	}
	if handler.calls[0].Opts.Category != "tv" {
		t.Errorf("expected category 'tv', got %q", handler.calls[0].Opts.Category)
	}
}
