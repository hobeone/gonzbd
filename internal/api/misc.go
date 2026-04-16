package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/sorting"
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

// modeGetScripts returns the list of available post-processing scripts.
// Currently always returns just "None" (no script directory scanning yet).
func (s *Server) modeGetScripts(w http.ResponseWriter, r *http.Request) {
	// TODO: Scan scripts directory when configured.
	scripts := []string{"None"}
	respondOK(w, "scripts", scripts)
}

// modeBrowse lists files and directories at a given path.
func (s *Server) modeBrowse(w http.ResponseWriter, r *http.Request) {
	dirPath := formString(r, "name")
	if dirPath == "" {
		respondError(w, http.StatusBadRequest, "missing name parameter (path)")
		return
	}

	// Validate: must be absolute, no ".." after cleaning
	cleaned := filepath.Clean(dirPath)
	if !filepath.IsAbs(cleaned) {
		respondError(w, http.StatusBadRequest, "path must be absolute")
		return
	}
	if strings.Contains(cleaned, "..") {
		respondError(w, http.StatusBadRequest, "path traversal not allowed")
		return
	}

	showFiles := formString(r, "show_files") == "1"
	showHidden := formString(r, "show_hidden_folders") == "1"

	entries, err := os.ReadDir(cleaned)
	if err != nil {
		respondError(w, http.StatusBadRequest, "cannot read directory: "+err.Error())
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

// modeEvalSort expands a sort template given a job name.
func (s *Server) modeEvalSort(w http.ResponseWriter, r *http.Request) {
	sortString := formString(r, "sort_string")
	jobName := formString(r, "job_name")

	if sortString == "" || jobName == "" {
		respondError(w, http.StatusBadRequest, "missing sort_string or job_name parameter")
		return
	}

	// Parse the job name to extract media info
	info := sorting.Parse(jobName)

	// Expand the sort template
	result := sorting.ExpandTemplate(sortString, info, "")

	respondOK(w, "result", result)
}

// modeWatchedNow triggers a manual scan of watched directories (not implemented).
func (s *Server) modeWatchedNow(w http.ResponseWriter, r *http.Request) {
	// TODO: Requires DirScanner integration.
	respondError(w, http.StatusNotImplemented, "not implemented in this build: watched_now")
}

// modeRssNow triggers a manual RSS feed refresh (not implemented).
func (s *Server) modeRssNow(w http.ResponseWriter, r *http.Request) {
	// TODO: Requires RSS feed processor integration.
	respondError(w, http.StatusNotImplemented, "not implemented in this build: rss_now")
}
