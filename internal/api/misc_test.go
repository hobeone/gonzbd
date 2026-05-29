package api

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
)

func TestModeGetCats_WithConfig(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Categories: []config.CategoryConfig{
			{Name: "movies"},
			{Name: "tv"},
			{Name: "music"},
		},
	}
	s := testServerWithConfig(t, cfg)

	rr := apiGet(t, s.Handler(), "/api?mode=get_cats&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	if m["status"] != true {
		t.Errorf("status = %v; want true", m["status"])
	}

	cats, ok := m["categories"].([]any)
	if !ok {
		t.Fatalf("categories not an array")
	}

	// Should have wildcard + 3 categories
	if len(cats) != 4 {
		t.Errorf("categories length = %d; want 4", len(cats))
	}

	if cats[0] != "*" {
		t.Errorf("first category = %v; want wildcard", cats[0])
	}
}

func TestModeGetCats_NilConfig(t *testing.T) {
	t.Parallel()
	s := testServer() // No config

	rr := apiGet(t, s.Handler(), "/api?mode=get_cats&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	cats, ok := m["categories"].([]any)
	if !ok {
		t.Fatalf("categories not an array")
	}

	// Should have just wildcard
	if len(cats) != 1 {
		t.Errorf("categories length = %d; want 1", len(cats))
	}

	if cats[0] != "*" {
		t.Errorf("first category = %v; want wildcard", cats[0])
	}
}

func TestModeGetScripts_ReturnsNone(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=get_scripts&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	if m["status"] != true {
		t.Errorf("status = %v; want true", m["status"])
	}

	scripts, ok := m["scripts"].([]any)
	if !ok {
		t.Fatalf("scripts not an array")
	}

	if len(scripts) != 1 {
		t.Errorf("scripts length = %d; want 1", len(scripts))
	}

	if scripts[0] != "None" {
		t.Errorf("first script = %v; want None", scripts[0])
	}
}

func TestModeBrowse_ValidDir(t *testing.T) {
	t.Parallel()
	s := testServer()

	// Use /tmp as a safe directory to browse
	rr := apiGet(t, s.Handler(), "/api?mode=browse&name=/tmp&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	if m["status"] != true {
		t.Errorf("status = %v; want true", m["status"])
	}

	paths, ok := m["paths"].([]any)
	if !ok {
		t.Fatalf("paths not an array")
	}

	// /tmp exists and should have some entries or be empty, but should be valid
	if paths == nil {
		t.Errorf("paths is nil (should be array)")
	}
}

func TestModeBrowse_PathTraversal(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=browse&name=/tmp/../../etc/passwd&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (path traversal)", rr.Code)
	}
}

func TestModeBrowse_RelativePath(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=browse&name=relative/path&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (relative path)", rr.Code)
	}
}

func TestModeBrowse_MissingName(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=browse&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (missing name)", rr.Code)
	}
}

func TestModeBrowse_ShowFiles(t *testing.T) {
	t.Parallel()
	s := testServer()

	// Create a temporary directory with a file
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "testfile.txt")
	if err := os.WriteFile(tmpFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("create temp file: %v", err)
	}

	// Browse without show_files
	rr := apiGet(t, s.Handler(), "/api?mode=browse&name="+tmpDir+"&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	paths, ok := m["paths"].([]any)
	if !ok {
		t.Fatalf("paths not an array")
	}

	// Should have no files (only dirs)
	for _, p := range paths {
		pathObj := p.(map[string]any)
		if !pathObj["dir"].(bool) {
			t.Errorf("got file when show_files=0 not set")
		}
	}

	// Browse with show_files
	rr = apiGet(t, s.Handler(), "/api?mode=browse&name="+tmpDir+"&show_files=1&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m = decodeJSON(t, rr)
	paths, ok = m["paths"].([]any)
	if !ok {
		t.Fatalf("paths not an array")
	}

	// Should have the test file
	found := false
	for _, p := range paths {
		pathObj := p.(map[string]any)
		if pathObj["name"].(string) == "testfile.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("test file not found when show_files=1")
	}
}

func TestModeWatchedNow_NotImplemented(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=watched_now&apikey="+testAPIKey)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501", rr.Code)
	}
}

func TestModeBrowse_HiddenFolders(t *testing.T) {
	t.Parallel()
	s := testServer()

	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".hidden_dir"), 0o755); err != nil {
		t.Fatalf("create hidden dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "visible_dir"), 0o755); err != nil {
		t.Fatalf("create visible dir: %v", err)
	}

	// Without show_hidden_folders: hidden dir should be excluded.
	rr := apiGet(t, s.Handler(), "/api?mode=browse&name="+tmpDir+"&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	m := decodeJSON(t, rr)
	paths := m["paths"].([]any)
	for _, p := range paths {
		obj := p.(map[string]any)
		if obj["name"].(string) == ".hidden_dir" {
			t.Error("hidden dir should be excluded without show_hidden_folders=1")
		}
	}
	if len(paths) != 1 {
		t.Errorf("expected 1 visible entry, got %d", len(paths))
	}

	// With show_hidden_folders=1: hidden dir should be included.
	rr = apiGet(t, s.Handler(), "/api?mode=browse&name="+tmpDir+"&show_hidden_folders=1&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	m = decodeJSON(t, rr)
	paths = m["paths"].([]any)
	var foundHidden bool
	for _, p := range paths {
		obj := p.(map[string]any)
		if obj["name"].(string) == ".hidden_dir" {
			foundHidden = true
		}
	}
	if !foundHidden {
		t.Error("hidden dir should be included with show_hidden_folders=1")
	}
	if len(paths) != 2 {
		t.Errorf("expected 2 entries with hidden shown, got %d", len(paths))
	}
}

func TestModeBrowse_NonexistentDir(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=browse&name=/nonexistent_path_abc123&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 for nonexistent dir", rr.Code)
	}
}
