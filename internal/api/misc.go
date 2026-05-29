package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/hobeone/gonzbd/internal/config"
)

// modeGetCats returns the list of configured categories.
func (s *Server) modeGetCats(w http.ResponseWriter, r *http.Request) {
	cats := []string{"*"} // Always include wildcard

	if s.config != nil {
		s.config.WithRead(func(c *config.Config) {
			for _, cat := range c.Categories {
				cats = append(cats, cat.Name)
			}
		})
	}

	respondOK(w, "categories", cats)
}

// modeGetScripts returns the list of available post-processing scripts by
// scanning the configured script directory for executable files.
func (s *Server) modeGetScripts(w http.ResponseWriter, r *http.Request) {
	scripts := []string{"None"}

	var scriptDir string
	if s.config != nil {
		s.config.WithRead(func(c *config.Config) {
			scriptDir = c.General.ScriptDir
		})
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

// modeBrowse lists files and directories at a given path.
func (s *Server) modeBrowse(w http.ResponseWriter, r *http.Request) {
	dirPath := formString(r, "name")
	if dirPath == "" {
		s.respondError(w, http.StatusBadRequest, "missing name parameter (path)")
		return
	}

	// Validate: must be absolute.
	cleaned := filepath.Clean(dirPath)
	if !filepath.IsAbs(cleaned) {
		s.respondError(w, http.StatusBadRequest, "path must be absolute")
		return
	}

	showFiles := formString(r, "show_files") == "1"
	showHidden := formString(r, "show_hidden_folders") == "1"

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
