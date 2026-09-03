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
	v := formValue(r, key)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// formValue extracts a form or query parameter value by key.
// For POST/PUT/PATCH requests with form bodies (both url-encoded and multipart/form-data),
// body values take precedence over URL query string values.
// Uses bounded memory (10 MiB) when parsing multipart/form-data to prevent memory exhaustion.
func formValue(r *http.Request, key string) string {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		const maxMem = 10 * 1024 * 1024 // 10 MiB
		if err := r.ParseMultipartForm(maxMem); err == nil && r.MultipartForm != nil {
			if vs := r.MultipartForm.Value[key]; len(vs) > 0 && vs[0] != "" {
				return vs[0]
			}
		}
	} else if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
		if err := r.ParseForm(); err == nil {
			if v := r.PostFormValue(key); v != "" {
				return v
			}
		}
	}
	if r.Form != nil && len(r.Form[key]) > 0 && r.Form[key][0] != "" {
		return r.Form[key][0]
	}
	return r.URL.Query().Get(key)
}

// requireParam reads the named query/form value. If it is empty, it writes a
// 400 ("missing <key> parameter") and returns ok=false; the caller should
// return immediately. An optional label is appended in parentheses to clarify
// the SABnzbd-positional name the parameter carries, e.g.
// requireParam(w, r, "value", "nzo_id") → "missing value parameter (nzo_id)".
func (s *Server) requireParam(w http.ResponseWriter, r *http.Request, key, label string) (string, bool) {
	v := formValue(r, key)
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
	s := formValue(r, "priority")
	if s == "" {
		return constants.DefaultPriority
	}
	return constants.Priority(int8(intParam(r, "priority"))) //nolint:gosec // G115: priority values fit in int8 by design
}

// ppParam extracts the post-processing level from the request.
// Returns types.PPInherit (-1) when absent, meaning "inherit from category".
func ppParam(r *http.Request) int {
	s := formValue(r, "pp")
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

// isSystemMutationMode returns true if mode mutates application-level state.
func isSystemMutationMode(mode string) bool {
	switch mode {
	case "set_config", "shutdown", "restart", "pause", "resume", "disconnect", "addurl", "addfile", "addlocalfile", "pause_pp", "resume_pp", "speedlimit":
		return true
	default:
		return false
	}
}

// isQueueMutation returns true if a queue action name mutates state.
//
// Fail closed: only the read-only listing actions ("" and "list") are
// exempt; every other (including any future) sub-action is treated as
// state-changing. Enumerating mutating actions instead let several real
// mutations (pause_all, resume_all, rename, change_script, change_cat, …)
// silently bypass the cookie-auth POST requirement. This is safe for the
// web UI (it issues all queue mutations via POST and only ever GETs the
// bare list/detail with an empty name) and for third-party apps (they
// authenticate with ?apikey=, not a cookie, so the POST gate never applies).
func isQueueMutation(name string) bool {
	switch name {
	case "", "list":
		return false
	default:
		return true
	}
}

// isHistoryMutation returns true if a history action name mutates state.
// Fail closed for the same reasons as isQueueMutation (retry and
// mark_as_completed were previously unguarded).
func isHistoryMutation(name string) bool {
	switch name {
	case "", "list":
		return false
	default:
		return true
	}
}

// isStateChangingMode returns true if the mode/name pair identifies a
// state-changing or destructive endpoint, for the cookie-auth POST-only
// CSRF gate. It fails closed for the sub-action-bearing modes.
func isStateChangingMode(mode, name string) bool {
	if isSystemMutationMode(mode) {
		return true
	}
	switch mode {
	case "queue":
		return isQueueMutation(name)
	case "history":
		return isHistoryMutation(name)
	case "warnings":
		// Only name=clear mutates; the bare listing (empty name) is a
		// read the UI polls via GET.
		return name == "clear"
	case "config":
		// Every mode=config sub-action mutates config or probes a server;
		// the UI reads configuration via the distinct get_config mode and
		// never issues a cookie GET to mode=config.
		return true
	default:
		return false
	}
}

// isStateChangingRequest returns true if r mutates server state, either
// by HTTP method (non-GET/HEAD) or by targeting a state-changing mode/name.
func isStateChangingRequest(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return true
	}
	mode := r.URL.Query().Get("mode")
	name := r.URL.Query().Get("name")
	return isStateChangingMode(mode, name)
}

// requireDependencies checks that all requested dependencies are wired on the server.
// If any requested dependency is nil, it writes a 500 internal server error and returns false.
func (s *Server) requireDependencies(w http.ResponseWriter, queue, app, config, history, grabber bool) bool {
	if queue && s.dispatcher == nil && s.queue == nil {
		s.respondError(w, http.StatusInternalServerError, "queue not wired")
		return false
	}
	// The four role fields are co-assigned from one Options.App (see
	// setAppServices), so any one being nil means the app is unwired. Check a
	// single canonical role field here — it is equivalent to the old s.app.
	if app && s.jobs == nil {
		s.respondError(w, http.StatusInternalServerError, "app not wired")
		return false
	}
	if config && s.config == nil {
		s.respondError(w, http.StatusInternalServerError, "config not wired")
		return false
	}
	if history && s.history == nil {
		s.respondError(w, http.StatusInternalServerError, "history not wired")
		return false
	}
	if grabber && s.grabber == nil {
		s.respondError(w, http.StatusInternalServerError, "url grabber not wired")
		return false
	}
	return true
}

func (s *Server) requireQueue(w http.ResponseWriter) bool {
	return s.requireDependencies(w, true, false, false, false, false)
}

func (s *Server) requireApp(w http.ResponseWriter) bool {
	return s.requireDependencies(w, false, true, false, false, false)
}

func (s *Server) requireConfig(w http.ResponseWriter) bool {
	return s.requireDependencies(w, false, false, true, false, false)
}

func (s *Server) requireHistory(w http.ResponseWriter) bool {
	return s.requireDependencies(w, false, false, false, true, false)
}

func (s *Server) requireGrabber(w http.ResponseWriter) bool {
	return s.requireDependencies(w, false, false, false, false, true)
}
