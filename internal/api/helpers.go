package api

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/types"
)

// toMBString formats bytes as a megabyte string like "1024.00" for SABnzbd API compatibility.
func toMBString(n int64) string {
	return strconv.FormatFloat(float64(n)/float64(1<<20), 'f', 2, 64)
}

// intParam reads a query parameter as int, returning 0 if absent or unparseable.
func intParam(r *http.Request, key string) int {
	v := formString(r, key)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// formString reads a query/form value. Centralizes the //nolint:gosec
// suppression — the body is size-limited by loggingMiddleware so G120
// (memory-exhaustion via unbounded form parsing) does not apply.
func formString(r *http.Request, key string) string {
	return r.FormValue(key) //nolint:gosec // G120: body already limited by loggingMiddleware's MaxBytesReader
}

// requireParam reads the named query/form value. If it is empty, it writes a
// 400 ("missing <key> parameter") and returns ok=false; the caller should
// return immediately. An optional label is appended in parentheses to clarify
// the SABnzbd-positional name the parameter carries, e.g.
// requireParam(w, r, "value", "nzo_id") → "missing value parameter (nzo_id)".
func (s *Server) requireParam(w http.ResponseWriter, r *http.Request, key, label string) (string, bool) {
	v := formString(r, key)
	if v == "" {
		msg := "missing " + key + " parameter"
		if label != "" {
			msg += " (" + label + ")"
		}
		s.respondError(w, http.StatusBadRequest, msg)
		return "", false
	}
	return v, true
}

// priorityParam reads the priority= query parameter and maps it to a Priority constant.
// Returns DefaultPriority (inherit from category) when the parameter is absent.
func priorityParam(r *http.Request) constants.Priority {
	s := r.FormValue("priority") //nolint:gosec // G120: body already limited
	if s == "" {
		return constants.DefaultPriority
	}
	return constants.Priority(int8(intParam(r, "priority"))) //nolint:gosec // G115: priority values fit in int8 by design
}

// ppParam extracts the post-processing level from the request.
// Returns types.PPInherit (-1) when absent, meaning "inherit from category".
func ppParam(r *http.Request) int {
	s := r.FormValue("pp") //nolint:gosec // G120: body already limited
	if s == "" {
		return types.PPInherit
	}
	return intParam(r, "pp")
}

// openFile wraps os.Open so the G304 gosec finding is isolated to one place.
// The caller is responsible for validating the path before calling openFile.
func openFile(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // G304: caller validates path is absolute and traversal-free
}

// splitCSV splits a comma-separated value string into trimmed non-empty tokens.
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// nonEmpty returns s if non-empty, otherwise fallback.
func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// formatDuration renders a non-negative whole-second duration as h:mm:ss
// (matching Python SABnzbd's timeleft format).
func formatDuration(seconds int) string {
	if seconds < 0 {
		seconds = 0
	}
	h := seconds / 3600
	m := (seconds % 3600) / 60
	s := seconds % 60
	return fmt.Sprintf("%d:%02d:%02d", h, m, s)
}

// isStateChangingMode returns true if the mode/name pair identifies a
// state-changing or destructive endpoint (set_config, shutdown, restart,
// pause, resume, disconnect, and queue/history deletions).
func isStateChangingMode(mode, name string) bool {
	switch mode {
	case "set_config", "shutdown", "restart", "pause", "resume", "disconnect":
		return true
	case "queue":
		return name == "delete" || name == "purge" || name == "delete_nzf"
	case "history":
		return name == "delete"
	}
	return false
}

// isStateChangingRequest returns true if r mutates server state, either
// by HTTP method (non-GET/HEAD) or by targeting a state-changing mode/name.
func isStateChangingRequest(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return true
	}
	mode := r.URL.Query().Get("mode")
	if mode == "" && r.Method == http.MethodPost {
		mode = formValue(r, "mode")
	}
	name := r.URL.Query().Get("name")
	if name == "" && r.Method == http.MethodPost {
		name = formValue(r, "name")
	}
	return isStateChangingMode(mode, name)
}
