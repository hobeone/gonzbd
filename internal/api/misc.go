package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// modeGetCats returns the list of configured categories.
func (s *Server) modeGetCats(w http.ResponseWriter, r *http.Request) {
	cats := []string{"*"} // Always include wildcard

	if s.config != nil {
		for _, cat := range s.config.GetCategories() {
			cats = append(cats, cat.Name)
		}
	}

	respondOK(w, "categories", cats)
}

// modeGetScripts returns the list of available post-processing scripts by
// scanning the configured script directory for executable files.
func (s *Server) modeGetScripts(w http.ResponseWriter, r *http.Request) {
	scripts := []string{"None"}

	var scriptDir string
	if s.config != nil {
		scriptDir = s.config.GetGeneral().ScriptDir
	}

	if scriptDir != "" {
		entries, err := os.ReadDir(scriptDir)
		if err == nil {
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				info, err := e.Info()
				if err != nil {
					continue
				}
				// Include files that are executable by the owner.
				if info.Mode()&0o100 != 0 {
					scripts = append(scripts, e.Name())
				}
			}
		}
	}

	respondOK(w, "scripts", scripts)
}

// configuredRoots returns the set of absolute directories the browse/
// addlocalfile file pickers are allowed to read from: the download,
// complete, dirscan, and script directories. Empty/unset config fields are
// skipped (never treated as "any path").
func (s *Server) configuredRoots() []string {
	var roots []string
	if s.config == nil {
		return roots
	}
	gen := s.config.GetGeneral()
	for _, dir := range []string{
		gen.DownloadDir,
		gen.CompleteDir,
		gen.DirscanDir,
		gen.ScriptDir,
	} {
		if dir != "" {
			roots = append(roots, filepath.Clean(dir))
		}
	}
	return roots
}

// pathWithinConfiguredRoots reports whether cleaned (an already
// filepath.Clean'd absolute path) is equal to or nested under one of the
// configured picker roots (SEC-6). Used by both modeBrowse and
// modeAddLocalFile so the filesystem-enumeration surface is limited to
// directories the operator has already told gonzbd about.
//
// A lexical prefix match alone cannot detect a symlink under a configured
// root pointing outside it (e.g. download_dir/escape -> /etc), so once the
// lexical check passes, this also resolves symlinks on both cleaned and
// the roots and re-checks containment on the resolved paths. If cleaned
// (or its immediate parent) doesn't exist yet, symlink resolution is
// skipped and the lexical match stands: there is nothing on disk yet for a
// symlink to redirect through, and the caller's subsequent read/open
// produces its own accurate not-found error.
func (s *Server) pathWithinConfiguredRoots(cleaned string) bool {
	roots := s.configuredRoots()
	if !withinAnyRoot(cleaned, roots) {
		return false
	}

	resolved, ok := resolveExistingAncestor(cleaned)
	if !ok {
		// No component of cleaned exists on disk yet (not even "/", which
		// can't happen, so this is effectively unreachable) — nothing to
		// resolve; the lexical match already performed is all the
		// containment check this path can support.
		return true
	}

	var resolvedRoots []string
	for _, root := range roots {
		if r, err := filepath.EvalSymlinks(root); err == nil {
			resolvedRoots = append(resolvedRoots, r)
		}
	}
	return withinAnyRoot(resolved, resolvedRoots)
}

// resolveExistingAncestor resolves symlinks for the deepest existing
// ancestor of path (path itself, if it exists, otherwise its nearest
// existing parent directory), then rejoins the nonexistent suffix
// components verbatim — a nonexistent component can't itself be a symlink
// pointing anywhere, so only the existing prefix needs resolution. ok is
// false only if no ancestor resolves (practically unreachable, since "/"
// always exists).
func resolveExistingAncestor(path string) (resolved string, ok bool) {
	suffix := ""
	for {
		if r, err := filepath.EvalSymlinks(path); err == nil {
			if suffix == "" {
				return r, true
			}
			return filepath.Join(r, suffix), true
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", false
		}
		if suffix == "" {
			suffix = filepath.Base(path)
		} else {
			suffix = filepath.Join(filepath.Base(path), suffix)
		}
		path = parent
	}
}

// withinAnyRoot reports whether path is equal to or nested under one of
// roots. path and roots must already be filepath.Clean'd (or both
// consistently symlink-resolved) for the prefix comparison to be valid.
func withinAnyRoot(path string, roots []string) bool {
	for _, root := range roots {
		if path == root {
			return true
		}
		// A root of exactly "/" already ends in the separator; appending
		// another would build "//", which no cleaned path can ever have
		// as a prefix, making the whole root unmatchable.
		prefix := root
		if prefix != string(filepath.Separator) {
			prefix += string(filepath.Separator)
		}
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// modeBrowse lists files and directories at a given path.
func (s *Server) modeBrowse(w http.ResponseWriter, r *http.Request) {
	dirPath, ok := s.requireParam(w, r, "name", "path")
	if !ok {
		return
	}

	// Validate: must be absolute.
	cleaned := filepath.Clean(dirPath)
	if !filepath.IsAbs(cleaned) {
		s.respondError(w, http.StatusBadRequest, "path must be absolute")
		return
	}
	if !s.pathWithinConfiguredRoots(cleaned) {
		s.respondError(w, http.StatusForbidden, "path is outside configured directories")
		return
	}

	showFiles := formValue(r, "show_files") == "1"
	showHidden := formValue(r, "show_hidden_folders") == "1"

	entries, err := os.ReadDir(cleaned)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, "cannot read directory: "+err.Error())
		return
	}

	type pathEntry struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Dir  bool   `json:"dir"`
	}

	var paths []pathEntry
	for _, e := range entries {
		// Skip hidden entries unless showHidden is true
		if !showHidden && strings.HasPrefix(e.Name(), ".") {
			continue
		}

		// Skip files unless showFiles is true
		if !e.IsDir() && !showFiles {
			continue
		}

		fullPath := filepath.Join(cleaned, e.Name())
		paths = append(paths, pathEntry{
			Name: e.Name(),
			Path: fullPath,
			Dir:  e.IsDir(),
		})
	}

	// Ensure paths is never nil so JSON encodes as [] not null
	if paths == nil {
		paths = []pathEntry{}
	}

	respondOK(w, "paths", paths)
}

// modeWatchedNow triggers a manual scan of watched directories (not implemented).
func (s *Server) modeWatchedNow(w http.ResponseWriter, r *http.Request) {
	// TODO: Requires DirScanner integration.
	s.respondError(w, http.StatusNotImplemented, "not implemented in this build: watched_now")
}
