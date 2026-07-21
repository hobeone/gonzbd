package dirscanner

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/types"
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

func (m *MockHandler) HandleNZB(ctx context.Context, filename string, data []byte, opts types.FetchOptions) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err, ok := m.failFor[filename]; ok {
		m.lastError = err
		return "", err
	}
	m.calls = append(m.calls, HandlerCall{Filename: filename, Data: data, Opts: opts})
	return "", nil
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
	count, err := scanner.ScanOnce(t.Context())
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
	count, err = scanner.ScanOnce(t.Context())
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
	if _, err := scanner.ScanOnce(t.Context()); err != nil {
		t.Fatalf("first ScanOnce failed: %v", err)
	}

	// Modify the file by appending content.
	if err := os.WriteFile(nzbPath, append(originalContent, []byte("modified")...), 0o644); err != nil {
		t.Fatalf("failed to modify test NZB: %v", err)
	}

	// Second scan should detect the change and reset stability.
	count, err := scanner.ScanOnce(t.Context())
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
	if _, err := scanner.ScanOnce(t.Context()); err != nil {
		t.Fatalf("first ScanOnce failed: %v", err)
	}

	// Second scan: handler fails but scan itself doesn't error (it logs).
	count, err := scanner.ScanOnce(t.Context())
	if err != nil {
		t.Errorf("ScanOnce should not error on handler failure: %v", err)
	}
	if count != 0 {
		t.Errorf("scan with handler error should not count as processed, got %d", count)
	}

	// On handler failure the scanner renames the file to .failed and removes
	// its state entry (not retried automatically).
	failedPath := nzbPath + ".failed"
	if _, err := os.Stat(failedPath); os.IsNotExist(err) {
		t.Errorf("file should be renamed to .failed on handler error")
	}

	// Original file should be gone (renamed).
	if _, err := os.Stat(nzbPath); !os.IsNotExist(err) {
		t.Errorf("original file should not exist after handler error (it gets renamed)")
	}

	// State entry should be removed.
	if _, ok := store.Get(nzbPath); ok {
		t.Errorf("file state should be removed after handler error")
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
	tmpDir := t.TempDir()
	// stdlib compress/bzip2 is decompress-only, so the compressed bytes are
	// embedded as a fixture (a real `bzip2 -9` stream) rather than produced at
	// test time. Decompresses to `<?xml version="1.0" ?>`.
	bz2Data := []byte{66, 90, 104, 57, 49, 65, 89, 38, 83, 89, 7, 88, 122, 103, 0, 0, 3, 153, 128, 80, 1, 96, 7, 130, 39, 153, 64, 32, 0, 49, 76, 152, 153, 6, 70, 20, 209, 166, 141, 168, 15, 72, 153, 91, 179, 91, 27, 14, 9, 236, 18, 97, 199, 197, 220, 145, 78, 20, 36, 1, 214, 30, 153, 192}
	want := []byte(`<?xml version="1.0" ?>`)

	bz2Path := filepath.Join(tmpDir, "test.nzb.bz2")
	if err := os.WriteFile(bz2Path, bz2Data, 0o644); err != nil {
		t.Fatalf("write bz2 file: %v", err)
	}

	nzbs, err := ExtractNZBs(bz2Path)
	if err != nil {
		t.Fatalf("ExtractNZBs: %v", err)
	}
	if len(nzbs) != 1 {
		t.Fatalf("expected 1 NZB, got %d", len(nzbs))
	}
	if !bytes.Equal(nzbs[0], want) {
		t.Errorf("decompressed content = %q, want %q", nzbs[0], want)
	}
}

func TestDecompressZip(t *testing.T) {
	tmpDir := t.TempDir()
	nzbA := []byte(`<?xml version="1.0" ?><nzb>A</nzb>`)
	nzbB := []byte(`<?xml version="1.0" ?><nzb>B</nzb>`)

	zipPath := filepath.Join(tmpDir, "test.zip")
	file, err := os.Create(zipPath)
	if err != nil {
		t.Fatalf("create zip file: %v", err)
	}
	defer file.Close()

	zw := zip.NewWriter(file)
	writeMember := func(name string, content []byte) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip.Create(%s): %v", name, err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatalf("write zip member %s: %v", name, err)
		}
	}
	// extractZip iterates members in archive order and skips non-.nzb files,
	// so the readme between the two NZBs must not shift the result order.
	writeMember("first.nzb", nzbA)
	writeMember("readme.txt", []byte("ignore me"))
	writeMember("second.nzb", nzbB)
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}

	nzbs, err := ExtractNZBs(zipPath)
	if err != nil {
		t.Fatalf("ExtractNZBs: %v", err)
	}
	if len(nzbs) != 2 {
		t.Fatalf("expected 2 NZBs (readme.txt skipped), got %d", len(nzbs))
	}
	if !bytes.Equal(nzbs[0], nzbA) {
		t.Errorf("nzbs[0] = %q, want %q", nzbs[0], nzbA)
	}
	if !bytes.Equal(nzbs[1], nzbB) {
		t.Errorf("nzbs[1] = %q, want %q", nzbs[1], nzbB)
	}
}

func TestDecompressZip_NoNZBFiles(t *testing.T) {
	tmpDir := t.TempDir()
	zipPath := filepath.Join(tmpDir, "empty.zip")
	file, err := os.Create(zipPath)
	if err != nil {
		t.Fatalf("create zip file: %v", err)
	}
	defer file.Close()

	zw := zip.NewWriter(file)
	w, err := zw.Create("readme.txt")
	if err != nil {
		t.Fatalf("zip.Create: %v", err)
	}
	if _, err := w.Write([]byte("no nzb here")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}

	_, err = ExtractNZBs(zipPath)
	if err == nil {
		t.Fatal("expected error for zip with no .nzb members, got nil")
	}
	if !strings.Contains(err.Error(), "no .nzb files") {
		t.Errorf("error = %q, want it to mention 'no .nzb files'", err.Error())
	}
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

	count, err := scanner.ScanOnce(t.Context())
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

	count, err := scanner.ScanOnce(t.Context())
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
	for i := range 3 {
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

	_, err = scanner.ScanOnce(t.Context())
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
	count, err := scanner.ScanOnce(t.Context())
	if err != nil {
		t.Fatalf("first ScanOnce: %v", err)
	}
	if count != 0 {
		t.Errorf("first scan should process 0, got %d", count)
	}

	// Second scan: stable files are processed.
	count, err = scanner.ScanOnce(t.Context())
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

	// All handler calls must use PPInherit so NewJob resolves PP from
	// the category config. A zero value would silently skip post-processing.
	for _, c := range handler.calls {
		if c.Opts.PP != types.PPInherit {
			t.Errorf("%s: expected PP=%d (PPInherit), got %d", c.Filename, types.PPInherit, c.Opts.PP)
		}
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
	count, err := scanner.ScanOnce(t.Context())
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
	count, err = scanner.ScanOnce(t.Context())
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
	// PP must be PPInherit (-1) so NewJob resolves it from the category
	// config. A zero value (0) would silently disable all post-processing.
	if handler.calls[0].Opts.PP != types.PPInherit {
		t.Errorf("expected PP=%d (PPInherit), got %d", types.PPInherit, handler.calls[0].Opts.PP)
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
	if _, err := scanner.ScanOnce(t.Context()); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	count, err := scanner.ScanOnce(t.Context())
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
	if _, err := scanner.ScanOnce(t.Context()); err != nil {
		t.Fatalf("scan 3: %v", err)
	}
	count, err = scanner.ScanOnce(t.Context())
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

func TestRarFilesIgnored(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")
	store, err := OpenStore(stateFile)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	handler := &MockHandler{failFor: make(map[string]error)}
	scanner := New(tmpDir, store, handler, nil, nil)

	// Create a .rar file — should be ignored by isValidExtension.
	if err := os.WriteFile(filepath.Join(tmpDir, "test.rar"), []byte("fake rar data"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()

	// Scan 1: observe
	if _, _, err := scanner.scanDir(ctx, tmpDir, ""); err != nil {
		t.Fatal(err)
	}
	// Scan 2: should still not process .rar
	_, count, err := scanner.scanDir(ctx, tmpDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 processed for .rar file, got %d", count)
	}
	if len(handler.calls) != 0 {
		t.Errorf("expected no handler calls for .rar, got %d", len(handler.calls))
	}
}

func TestCorruptedFileRenamedToFailed(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")
	store, err := OpenStore(stateFile)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	handler := &MockHandler{failFor: make(map[string]error)}
	scanner := New(tmpDir, store, handler, nil, nil)

	// Create a .zip file with corrupted contents.
	corruptedFile := filepath.Join(tmpDir, "bad.zip")
	if err := os.WriteFile(corruptedFile, []byte("this is not a zip file"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()

	// Scan 1: observe the file.
	if _, _, err := scanner.scanDir(ctx, tmpDir, ""); err != nil {
		t.Fatal(err)
	}

	// Scan 2: file is stable, extraction fails, should be renamed to .failed.
	_, count, err := scanner.scanDir(ctx, tmpDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 processed for corrupted file, got %d", count)
	}

	// Verify original file is gone and .failed file exists.
	if _, err := os.Stat(corruptedFile); !os.IsNotExist(err) {
		t.Error("corrupted file should have been renamed away")
	}
	if _, err := os.Stat(corruptedFile + ".failed"); err != nil {
		t.Errorf("expected .failed file to exist: %v", err)
	}

	// Scan 3: .failed file should NOT be picked up again.
	_, count, err = scanner.scanDir(ctx, tmpDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 processed on third scan, got %d", count)
	}
}

// TestScanPassesInheritSentinels verifies that the dirscanner emits the
// "inherit from category" sentinels (PPInherit, DefaultPriority) in
// FetchOptions, rather than zero values. If this regresses, watched-folder
// jobs silently downgrade to PP=0 / Priority=Normal even when the category
// config specifies PP=7 or a non-default priority.
func TestScanPassesInheritSentinels(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := OpenStore(filepath.Join(tmpDir, "state.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	handler := &MockHandler{failFor: make(map[string]error)}
	scanner := New(tmpDir, store, handler, nil, nil)

	nzbPath := filepath.Join(tmpDir, "x.nzb")
	if err := os.WriteFile(nzbPath, []byte("<?xml version=\"1.0\" ?>"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// First scan records, second scan dispatches.
	if _, err := scanner.ScanOnce(t.Context()); err != nil {
		t.Fatalf("first ScanOnce: %v", err)
	}
	if _, err := scanner.ScanOnce(t.Context()); err != nil {
		t.Fatalf("second ScanOnce: %v", err)
	}
	if len(handler.calls) != 1 {
		t.Fatalf("expected 1 handler call, got %d", len(handler.calls))
	}
	got := handler.calls[0].Opts
	if got.PP != types.PPInherit {
		t.Errorf("PP: expected PPInherit (%d), got %d — category PP will be ignored downstream",
			types.PPInherit, got.PP)
	}
	if got.Priority != constants.DefaultPriority {
		t.Errorf("Priority: expected DefaultPriority (%d), got %d — category Priority will be ignored downstream",
			constants.DefaultPriority, got.Priority)
	}
}

// ---------------------------------------------------------------------------
// pruneRemovedFiles — focused unit tests for all 3 prune branches
// ---------------------------------------------------------------------------

func TestPruneRemovedFiles_PrunesFileInScannedDirWhenMissing(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := OpenStore(filepath.Join(tmpDir, "state.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	scanner := New(tmpDir, store, &MockHandler{failFor: make(map[string]error)}, nil, nil)

	// Record a file as seen.
	filePath := filepath.Join(tmpDir, "gone.nzb")
	store.Set(filePath, FileState{Size: 100, MTime: time.Now()})

	// scannedDirs contains tmpDir, but currentScan does NOT list the file
	// (it was deleted between scans).
	scannedDirs := map[string]bool{filepath.Clean(tmpDir): true}
	currentScan := map[string]FileState{} // file absent

	scanner.pruneRemovedFiles(scannedDirs, currentScan)

	if _, ok := store.Get(filePath); ok {
		t.Error("stale entry should have been pruned from store")
	}
}

func TestPruneRemovedFiles_KeepsFileInUnscannedExistingDir(t *testing.T) {
	tmpDir := t.TempDir()
	// Create a real subdirectory that won't be in scannedDirs.
	// It MUST exist on disk so the os.Stat branch doesn't prune it.
	unscannedDir := filepath.Join(tmpDir, "unscanned")
	if err := os.MkdirAll(unscannedDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	store, err := OpenStore(filepath.Join(tmpDir, "state.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	scanner := New(tmpDir, store, &MockHandler{failFor: make(map[string]error)}, nil, nil)

	filePath := filepath.Join(unscannedDir, "pending.nzb")
	store.Set(filePath, FileState{Size: 50, MTime: time.Now()})

	// scannedDirs does NOT include unscannedDir, so the file should be kept.
	scannedDirs := map[string]bool{filepath.Clean(tmpDir): true}
	currentScan := map[string]FileState{} // file absent from this scan pass

	scanner.pruneRemovedFiles(scannedDirs, currentScan)

	if _, ok := store.Get(filePath); !ok {
		t.Error("file in unscanned (but existing) dir should NOT be pruned")
	}
}

func TestPruneRemovedFiles_PrunesFileWhoseParentDirIsGone(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := OpenStore(filepath.Join(tmpDir, "state.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	scanner := New(tmpDir, store, &MockHandler{failFor: make(map[string]error)}, nil, nil)

	// Record a file in a directory that no longer exists on disk.
	deletedDir := filepath.Join(tmpDir, "removed_category")
	filePath := filepath.Join(deletedDir, "old.nzb")
	store.Set(filePath, FileState{Size: 200, MTime: time.Now()})
	// deletedDir is NOT created on disk, simulating a removed category folder.

	scannedDirs := map[string]bool{filepath.Clean(tmpDir): true}
	currentScan := map[string]FileState{}

	scanner.pruneRemovedFiles(scannedDirs, currentScan)

	if _, ok := store.Get(filePath); ok {
		t.Error("file whose parent dir is gone should be pruned from store")
	}
}

func TestPruneRemovedFiles_KeepsPresentFile(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := OpenStore(filepath.Join(tmpDir, "state.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	scanner := New(tmpDir, store, &MockHandler{failFor: make(map[string]error)}, nil, nil)

	filePath := filepath.Join(tmpDir, "present.nzb")
	state := FileState{Size: 30, MTime: time.Now()}
	store.Set(filePath, state)

	// File IS in currentScan — it should be retained.
	scannedDirs := map[string]bool{filepath.Clean(tmpDir): true}
	currentScan := map[string]FileState{filePath: state}

	scanner.pruneRemovedFiles(scannedDirs, currentScan)

	if _, ok := store.Get(filePath); !ok {
		t.Error("file present in currentScan should NOT be pruned")
	}
}

// ---------------------------------------------------------------------------
// processScannedFile — PartialError boundary
// ---------------------------------------------------------------------------

// TestProcessScannedFile_PartialError verifies that when handleStableFile
// returns a PartialError (some NZBs from an archive imported, some failed),
// the source file is NOT renamed to .failed. The file is left for retry.
func TestProcessScannedFile_PartialError(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := OpenStore(filepath.Join(tmpDir, "state.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	// Build a ZIP archive containing 2 NZB files.
	zipPath := filepath.Join(tmpDir, "batch.zip")
	if err := createZipWithNZBs(t, zipPath, []string{"a.nzb", "b.nzb"}); err != nil {
		t.Fatalf("create zip: %v", err)
	}

	// Handler succeeds for "batch.zip[1]" (a.nzb) but fails for "batch.zip[2]"
	// (b.nzb), producing a PartialError from handleStableFile.
	handler := &MockHandler{
		failFor: map[string]error{
			"batch.zip[2]": fmt.Errorf("simulated import failure for second NZB"),
		},
	}
	scanner := New(tmpDir, store, handler, nil, nil)

	// First processScannedFile call records the file (first sighting).
	ok, err := scanner.processScannedFile(t.Context(), zipPath, "batch.zip", "", map[string]FileState{})
	if err != nil {
		t.Fatalf("first scan unexpected error: %v", err)
	}
	if ok {
		t.Error("first scan should not process (not yet stable)")
	}

	// Second call: file is stable (same state), triggers extraction.
	ok, err = scanner.processScannedFile(t.Context(), zipPath, "batch.zip", "", map[string]FileState{})
	if err == nil {
		t.Fatal("expected error from PartialError, got nil")
	}
	pe, isPE := errors.AsType[*PartialError](err)
	if !isPE {
		t.Errorf("expected PartialError, got %T: %v", err, err)
	}
	if pe.Failed != 1 {
		t.Errorf("expected pe.Failed = 1, got %d", pe.Failed)
	}
	if pe.Total != 2 {
		t.Errorf("expected pe.Total = 2, got %d", pe.Total)
	}
	if ok {
		t.Error("processScannedFile should return false on partial error")
	}

	// CRITICAL: the source ZIP must NOT be renamed to .failed on PartialError.
	if _, err := os.Stat(zipPath); os.IsNotExist(err) {
		t.Error("source ZIP was removed/renamed on PartialError — it should be kept for retry")
	}
	if _, err := os.Stat(zipPath + ".failed"); !os.IsNotExist(err) {
		t.Error("source ZIP was renamed to .failed on PartialError — it should be kept for retry")
	}
}

// createZipWithNZBs creates a ZIP archive at path containing one empty .nzb
// file for each name in names.
func createZipWithNZBs(t *testing.T, path string, names []string) error {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	w := zip.NewWriter(f)
	defer w.Close() //nolint:errcheck
	for _, name := range names {
		fw, err := w.Create(name)
		if err != nil {
			return err
		}
		if _, err := fw.Write([]byte("<?xml version=\"1.0\" ?>")); err != nil {
			return err
		}
	}
	return nil
}

func TestScannerUnexportedHelpersDirect(t *testing.T) {
	t.Parallel()

	t.Run("isValidExtension", func(t *testing.T) {
		cases := []struct {
			filename string
			want     bool
		}{
			{"test.nzb", true},
			{"test.nzb.gz", true},
			{"test.nzb.bz2", true},
			{"test.zip", true},
			{"TEST.NZB", true},
			{"TEST.NZB.GZ", true},
			{"test.Nzb.Bz2", true},
			{"test.ZIP", true},
			{"test.rar.nzb", true},
			{"test.nzb.rar", false},
			{"test.txt", false},
			{"test.rar", false},
			{"", false},
		}

		for _, tc := range cases {
			t.Run(tc.filename, func(t *testing.T) {
				got := isValidExtension(tc.filename)
				if got != tc.want {
					t.Errorf("isValidExtension(%q) = %t, want %t", tc.filename, got, tc.want)
				}
			})
		}
	})

	t.Run("buildCategoryMap", func(t *testing.T) {
		t.Run("nil catFn", func(t *testing.T) {
			s := &Scanner{catFn: nil}
			got := s.buildCategoryMap()
			if got != nil {
				t.Errorf("buildCategoryMap() for nil catFn: expected nil, got %v", got)
			}
		})

		t.Run("empty catFn", func(t *testing.T) {
			s := &Scanner{catFn: func() []string { return []string{} }}
			got := s.buildCategoryMap()
			if got != nil {
				t.Errorf("buildCategoryMap() for empty catFn: expected nil, got %v", got)
			}
		})

		t.Run("valid categories case-insensitive mapping", func(t *testing.T) {
			s := &Scanner{
				catFn: func() []string {
					return []string{"TV", "movies", "Anime", "tv"}
				},
			}
			got := s.buildCategoryMap()
			want := map[string]string{
				"tv":     "tv",
				"movies": "movies",
				"anime":  "Anime",
			}
			if len(got) != len(want) {
				t.Fatalf("buildCategoryMap() length mismatch: got %d, want %d", len(got), len(want))
			}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("key %q: got %q, want %q", k, got[k], v)
				}
			}
		})
	})
}

type testLogHandler struct {
	mu       sync.Mutex
	records  []slog.Record
	onRecord func(slog.Record)
}

func (h *testLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return true
}

func (h *testLogHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	if h.onRecord != nil {
		h.onRecord(r)
	}
	return nil
}

func (h *testLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *testLogHandler) WithGroup(name string) slog.Handler       { return h }

func (h *testLogHandler) hasMessage(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if strings.Contains(r.Message, msg) {
			return true
		}
	}
	return false
}

type deleteFileHandler struct {
	fileToDelete string
	errToReturn  error
}

func (h *deleteFileHandler) HandleNZB(ctx context.Context, filename string, data []byte, opts types.FetchOptions) (string, error) {
	os.Remove(h.fileToDelete)
	return "", h.errToReturn
}

func TestScanner_Run(t *testing.T) {
	t.Run("success_processed", func(t *testing.T) {
		tmpDir := t.TempDir()
		store, _ := OpenStore(filepath.Join(tmpDir, "state.json"))
		handler := &MockHandler{failFor: make(map[string]error)}

		// Generous hang-guard: cancel() is triggered deterministically via the
		// onRecord callback when "scan complete" is logged. The timeout only
		// fires if the scanner hangs entirely — it must not race normal execution
		// under -race or high parallelism.
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		lh := &testLogHandler{
			onRecord: func(r slog.Record) {
				if strings.Contains(r.Message, "scan complete") {
					cancel()
				}
			},
		}
		logger := slog.New(lh)
		scanner := New(tmpDir, store, handler, nil, logger)

		nzbPath := filepath.Join(tmpDir, "test.nzb")
		os.WriteFile(nzbPath, []byte("<?xml version=\"1.0\" ?>"), 0o644)

		if _, err := scanner.ScanOnce(ctx); err != nil {
			t.Fatal(err)
		}

		err := scanner.Run(ctx, 1*time.Millisecond)
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run failed: %v", err)
		}

		if !lh.hasMessage("scan complete") {
			t.Error("expected \"scan complete\" log message")
		}
	})

	t.Run("scan_failed", func(t *testing.T) {
		store, _ := OpenStore(filepath.Join(t.TempDir(), "state.json"))
		handler := &MockHandler{failFor: make(map[string]error)}

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		lh := &testLogHandler{
			onRecord: func(r slog.Record) {
				if strings.Contains(r.Message, "scan failed") {
					cancel()
				}
			},
		}
		logger := slog.New(lh)
		scanner := New("/nonexistent/dir", store, handler, nil, logger)

		err := scanner.Run(ctx, 1*time.Millisecond)
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run failed: %v", err)
		}

		if !lh.hasMessage("scan failed") {
			t.Error("expected scan failed log message")
		}
	})

	t.Run("no_files_processed", func(t *testing.T) {
		tmpDir := t.TempDir()
		store, _ := OpenStore(filepath.Join(tmpDir, "state.json"))
		handler := &MockHandler{failFor: make(map[string]error)}

		// 500ms hang-guard: allows at least one full scan tick on an empty dir.
		// 20ms was within OS scheduling jitter under -race.
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()

		lh := &testLogHandler{}
		logger := slog.New(lh)
		scanner := New(tmpDir, store, handler, nil, logger)

		err := scanner.Run(ctx, 2*time.Millisecond)
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run failed: %v", err)
		}

		if lh.hasMessage("scan complete") {
			t.Error("did not expect \"scan complete\" log when no files were processed")
		}
	})
}

func TestScanner_LogWarnings(t *testing.T) {
	t.Run("rename_failed", func(t *testing.T) {
		tmpDir := t.TempDir()
		store, _ := OpenStore(filepath.Join(tmpDir, "state.json"))

		nzbPath := filepath.Join(tmpDir, "test.nzb")
		os.WriteFile(nzbPath, []byte("<?xml version=\"1.0\" ?>"), 0o644)

		lh := &testLogHandler{}
		logger := slog.New(lh)

		h := &deleteFileHandler{
			fileToDelete: nzbPath,
			errToReturn:  fmt.Errorf("simulated failure"),
		}
		scanner := New(tmpDir, store, h, nil, logger)

		scanner.ScanOnce(t.Context())
		scanner.ScanOnce(t.Context())

		if !lh.hasMessage("failed to rename file") {
			t.Error("expected failed to rename file warning message")
		}
	})

	t.Run("rename_succeeds", func(t *testing.T) {
		tmpDir := t.TempDir()
		store, _ := OpenStore(filepath.Join(tmpDir, "state.json"))

		nzbPath := filepath.Join(tmpDir, "test.nzb")
		os.WriteFile(nzbPath, []byte("<?xml version=\"1.0\" ?>"), 0o644)

		lh := &testLogHandler{}
		logger := slog.New(lh)

		handler := &MockHandler{
			failFor: map[string]error{
				"test.nzb": fmt.Errorf("simulated failure"),
			},
		}
		scanner := New(tmpDir, store, handler, nil, logger)

		scanner.ScanOnce(t.Context())
		scanner.ScanOnce(t.Context())

		if lh.hasMessage("failed to rename file") {
			t.Error("did not expect failed to rename file warning message")
		}
	})

	t.Run("remove_failed", func(t *testing.T) {
		tmpDir := t.TempDir()
		store, _ := OpenStore(filepath.Join(tmpDir, "state.json"))

		nzbPath := filepath.Join(tmpDir, "test.nzb")
		os.WriteFile(nzbPath, []byte("<?xml version=\"1.0\" ?>"), 0o644)

		lh := &testLogHandler{}
		logger := slog.New(lh)

		h := &deleteFileHandler{fileToDelete: nzbPath}
		scanner := New(tmpDir, store, h, nil, logger)

		scanner.ScanOnce(t.Context())
		scanner.ScanOnce(t.Context())

		if !lh.hasMessage("failed to delete file after successful handling") {
			t.Error("expected failed to delete file warning message")
		}
	})

	t.Run("save_failed", func(t *testing.T) {
		tmpDir := t.TempDir()
		store, _ := OpenStore(filepath.Join(tmpDir, "state.json"))

		lh := &testLogHandler{}
		logger := slog.New(lh)

		scanner := New(tmpDir, store, &MockHandler{}, nil, logger)

		store.path = "/nonexistent/path/state.json"
		store.Set("somefile", FileState{})

		scanner.ScanOnce(t.Context())

		if !lh.hasMessage("failed to save state") {
			t.Error("expected failed to save state warning message")
		}
	})
}

func TestScanOnce_CancellationPropagates(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := OpenStore(filepath.Join(tmpDir, "state.json"))
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	handler := &MockHandler{failFor: make(map[string]error)}
	scanner := New(tmpDir, store, handler, nil, nil)

	nzbPath := filepath.Join(tmpDir, "test.nzb")
	if err := os.WriteFile(nzbPath, []byte("<?xml version=\"1.0\" ?>"), 0o644); err != nil {
		t.Fatal(err)
	}

	// First scan records the file.
	if _, err := scanner.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Create a cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Second scan (file is stable): we run with cancelled context.
	_, err = scanner.ScanOnce(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// Since it was cancelled (not a real failure), the file must NOT be renamed to .failed.
	if _, err := os.Stat(nzbPath); os.IsNotExist(err) {
		t.Errorf("expected original file to remain, but it is missing")
	}
	if _, err := os.Stat(nzbPath + ".failed"); err == nil {
		t.Errorf("did not expect file to be renamed to .failed")
	}

	// And its state entry in the store must NOT be deleted.
	if _, ok := store.Get(nzbPath); !ok {
		t.Errorf("expected file state to remain in store, but it was deleted")
	}
}

func TestScanOnce_SubdirCancellationPropagates(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := OpenStore(filepath.Join(tmpDir, "state.json"))
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	handler := &MockHandler{failFor: make(map[string]error)}
	catFn := func() []string { return []string{"tv"} }
	scanner := New(tmpDir, store, handler, catFn, nil)

	tvDir := filepath.Join(tmpDir, "tv")
	if err := os.MkdirAll(tvDir, 0o755); err != nil {
		t.Fatal(err)
	}
	nzbPath := filepath.Join(tvDir, "show.nzb")
	if err := os.WriteFile(nzbPath, []byte("<?xml version=\"1.0\" ?>"), 0o644); err != nil {
		t.Fatal(err)
	}

	// First scan records the file.
	if _, err := scanner.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Create a cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Second scan (stable): run with cancelled context.
	_, err = scanner.ScanOnce(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestScanCategorySubdirs_SubdirError(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "state.json")
	store, err := OpenStore(stateFile)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	handler := &MockHandler{failFor: make(map[string]error)}
	catFn := func() []string { return []string{"tv"} }
	scanner := New(tmpDir, store, handler, catFn, nil)

	subDir := filepath.Join(tmpDir, "tv")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Make the subdirectory completely unreadable to force os.ReadDir failure.
	if err := os.Chmod(subDir, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(subDir, 0755) })

	// Scan should continue and not fail with an error because it logs and skips the bad category subdir.
	count, err := scanner.ScanOnce(t.Context())
	if err != nil {
		t.Fatalf("ScanOnce returned unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 processed files, got %d", count)
	}
}
