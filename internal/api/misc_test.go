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

func TestModeGetScripts_WithScripts(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Write an executable script file.
	scriptPath := filepath.Join(tmpDir, "myscript.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho ok"), 0755); err != nil {
		t.Fatalf("failed to write test script: %v", err)
	}

	// Write a non-executable file.
	nonExecPath := filepath.Join(tmpDir, "readme.txt")
	if err := os.WriteFile(nonExecPath, []byte("hello"), 0644); err != nil {
		t.Fatalf("failed to write readme: %v", err)
	}

	cfg := &config.Config{}
	cfg.General.APIKey = testAPIKey
	cfg.General.NZBKey = testNZBKey
	cfg.General.ScriptDir = tmpDir

	s := testServerWithConfig(t, cfg)

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

	var foundScript, foundReadme, foundNone bool
	for _, sc := range scripts {
		name := sc.(string)
		switch name {
		case "myscript.sh":
			foundScript = true
		case "readme.txt":
			foundReadme = true
		case "None":
			foundNone = true
		}
	}

	if !foundNone {
		t.Error("expected 'None' in scripts")
	}
	if !foundScript {
		t.Error("expected 'myscript.sh' to be listed in scripts")
	}
	if foundReadme {
		t.Error("expected 'readme.txt' to be excluded from scripts because it is not executable")
	}
}

func TestModeBrowse_ValidDir(t *testing.T) {
	t.Parallel()
	downloadDir := t.TempDir()
	cfg := &config.Config{
		General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey, DownloadDir: downloadDir},
	}
	s := testServerWithConfig(t, cfg)

	rr := apiGet(t, s.Handler(), "/api?mode=browse&name="+downloadDir+"&apikey="+testAPIKey)
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

	// downloadDir exists and should have some entries or be empty, but should be valid
	if paths == nil {
		t.Errorf("paths is nil (should be array)")
	}
}

func TestModeBrowse_PathTraversal(t *testing.T) {
	t.Parallel()
	s := testServer()

	// No roots are configured on this server, so any absolute path --
	// traversal or not -- is now rejected as outside the configured
	// allowlist (SEC-6), before the request ever reaches the filesystem.
	rr := apiGet(t, s.Handler(), "/api?mode=browse&name=/tmp/../../etc/passwd&apikey="+testAPIKey)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 (path outside configured roots)", rr.Code)
	}
}

func TestModeBrowse_RejectsPathOutsideConfiguredRoots(t *testing.T) {
	t.Parallel()
	downloadDir := t.TempDir()
	cfg := &config.Config{
		General: config.GeneralConfig{
			APIKey: testAPIKey, NZBKey: testNZBKey,
			DownloadDir: downloadDir,
		},
	}
	s := testServerWithConfig(t, cfg)

	// /etc is outside every configured root.
	rr := apiGet(t, s.Handler(), "/api?mode=browse&name=/etc&apikey="+testAPIKey)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 (path outside configured roots)", rr.Code)
	}
}

// TestModeBrowse_RejectsSymlinkEscapeFromConfiguredRoot is a regression
// guard for SEC-6's residual symlink gap: a symlink physically located
// inside a configured root but pointing outside it must not be treated as
// contained by a lexical-only prefix check.
func TestModeBrowse_RejectsSymlinkEscapeFromConfiguredRoot(t *testing.T) {
	t.Parallel()
	downloadDir := t.TempDir()
	outsideDir := t.TempDir()

	escape := filepath.Join(downloadDir, "escape")
	if err := os.Symlink(outsideDir, escape); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	cfg := &config.Config{
		General: config.GeneralConfig{
			APIKey: testAPIKey, NZBKey: testNZBKey,
			DownloadDir: downloadDir,
		},
	}
	s := testServerWithConfig(t, cfg)

	// escape's lexical path is under downloadDir, but it resolves to
	// outsideDir — must be rejected.
	rr := apiGet(t, s.Handler(), "/api?mode=browse&name="+escape+"&apikey="+testAPIKey)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 (symlink escapes configured root)", rr.Code)
	}
}

func TestModeBrowse_AllowsPathWithinDownloadDir(t *testing.T) {
	t.Parallel()
	downloadDir := t.TempDir()
	cfg := &config.Config{
		General: config.GeneralConfig{
			APIKey: testAPIKey, NZBKey: testNZBKey,
			DownloadDir: downloadDir,
		},
	}
	s := testServerWithConfig(t, cfg)

	rr := apiGet(t, s.Handler(), "/api?mode=browse&name="+downloadDir+"&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (path within configured DownloadDir)", rr.Code)
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

	// Create a temporary directory with a file
	tmpDir := t.TempDir()
	cfg := &config.Config{
		General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey, DownloadDir: tmpDir},
	}
	s := testServerWithConfig(t, cfg)

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

	tmpDir := t.TempDir()
	cfg := &config.Config{
		General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey, DownloadDir: tmpDir},
	}
	s := testServerWithConfig(t, cfg)

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
	downloadDir := t.TempDir()
	cfg := &config.Config{
		General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey, DownloadDir: downloadDir},
	}
	s := testServerWithConfig(t, cfg)

	// The nonexistent subdirectory is still within the configured root, so
	// this exercises the "cannot read directory" branch rather than the
	// SEC-6 allowlist check.
	rr := apiGet(t, s.Handler(), "/api?mode=browse&name="+filepath.Join(downloadDir, "nonexistent_subdir")+"&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 for nonexistent dir", rr.Code)
	}
}

func TestResolveExistingAncestor(t *testing.T) {
	t.Parallel()

	t.Run("existing path resolves directly", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		resolved, ok := resolveExistingAncestor(dir)
		if !ok {
			t.Fatalf("resolveExistingAncestor(%q) ok = false, want true", dir)
		}
		want, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatalf("EvalSymlinks(%q): %v", dir, err)
		}
		if resolved != want {
			t.Errorf("resolved = %q, want %q", resolved, want)
		}
	})

	t.Run("nonexistent leaf rejoins onto resolved existing parent", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		target := filepath.Join(dir, "nonexistent")
		resolved, ok := resolveExistingAncestor(target)
		if !ok {
			t.Fatalf("resolveExistingAncestor(%q) ok = false, want true", target)
		}
		wantParent, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatalf("EvalSymlinks(%q): %v", dir, err)
		}
		want := filepath.Join(wantParent, "nonexistent")
		if resolved != want {
			t.Errorf("resolved = %q, want %q", resolved, want)
		}
	})

	t.Run("multi-level nonexistent suffix walks up to existing ancestor", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		target := filepath.Join(dir, "a", "b", "c")
		resolved, ok := resolveExistingAncestor(target)
		if !ok {
			t.Fatalf("resolveExistingAncestor(%q) ok = false, want true", target)
		}
		wantParent, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatalf("EvalSymlinks(%q): %v", dir, err)
		}
		want := filepath.Join(wantParent, "a", "b", "c")
		if resolved != want {
			t.Errorf("resolved = %q, want %q", resolved, want)
		}
	})

	t.Run("symlink in an existing prefix resolves to its real target", func(t *testing.T) {
		t.Parallel()
		realDir := t.TempDir()
		parentDir := t.TempDir()
		link := filepath.Join(parentDir, "link")
		if err := os.Symlink(realDir, link); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		target := filepath.Join(link, "nonexistent-child")

		resolved, ok := resolveExistingAncestor(target)
		if !ok {
			t.Fatalf("resolveExistingAncestor(%q) ok = false, want true", target)
		}
		wantReal, err := filepath.EvalSymlinks(realDir)
		if err != nil {
			t.Fatalf("EvalSymlinks(%q): %v", realDir, err)
		}
		want := filepath.Join(wantReal, "nonexistent-child")
		if resolved != want {
			t.Errorf("resolved = %q, want %q (symlink should resolve to its real target)", resolved, want)
		}
	})
}

// TestWithinAnyRoot_RootSlash is a regression guard: a configured root of
// exactly "/" must match ordinary absolute paths. The naive
// root+separator construction turns "/" into "//", which no cleaned path
// can ever have as a prefix, silently rejecting everything.
func TestWithinAnyRoot_RootSlash(t *testing.T) {
	t.Parallel()
	roots := []string{"/"}

	cases := []string{"/", "/etc", "/etc/passwd", "/home/user/file.txt"}
	for _, path := range cases {
		if !withinAnyRoot(path, roots) {
			t.Errorf("withinAnyRoot(%q, %q) = false, want true", path, roots)
		}
	}
}

// TestWithinAnyRoot_OrdinaryRootUnaffected confirms the "/" special-case
// doesn't change behavior for a normal (non-"/") root.
func TestWithinAnyRoot_OrdinaryRootUnaffected(t *testing.T) {
	t.Parallel()
	roots := []string{"/data/downloads"}

	if !withinAnyRoot("/data/downloads", roots) {
		t.Error("withinAnyRoot(root itself) = false, want true")
	}
	if !withinAnyRoot("/data/downloads/file.txt", roots) {
		t.Error("withinAnyRoot(nested path) = false, want true")
	}
	if withinAnyRoot("/data/downloads-evil", roots) {
		t.Error("withinAnyRoot(sibling sharing a string prefix without separator) = true, want false")
	}
	if withinAnyRoot("/etc", roots) {
		t.Error("withinAnyRoot(unrelated path) = true, want false")
	}
}
