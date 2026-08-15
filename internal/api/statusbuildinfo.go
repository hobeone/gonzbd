package api

import (
	"net/http"
	"runtime"
	"runtime/debug"
)

// buildDependency is a single Go module dependency compiled into this
// binary, as reported by the runtime's embedded build info.
type buildDependency struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

// resolvedDependency returns the dependency actually compiled in: if m is
// overridden by a go.mod "replace" directive, that target's path/version is
// what ended up in the binary, not m's own.
func resolvedDependency(m *debug.Module) buildDependency {
	if m.Replace != nil {
		m = m.Replace
	}
	return buildDependency{Path: m.Path, Version: m.Version}
}

// statusBuildInfo returns the version string (with git commit and build
// date), Go toolchain version, and the full list of Go module dependencies
// linked into this binary. The dependency list comes from
// debug.ReadBuildInfo(), which reflects exactly what was compiled in --
// not go.mod, which can drift (unused requires, build constraints).
func (s *Server) statusBuildInfo(w http.ResponseWriter, _ *http.Request) {
	deps := []buildDependency{}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, m := range bi.Deps {
			deps = append(deps, resolvedDependency(m))
		}
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"status":     true,
		"version":    s.version,
		"commit":     s.commit,
		"build_date": s.date,
		"go_version": runtime.Version(),
		"deps":       deps,
	})
}
