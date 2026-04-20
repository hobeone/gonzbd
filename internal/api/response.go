// Package api implements SABnzbd's HTTP API server. All API calls are
// GET or POST to /api with a mode= query parameter that selects the
// handler. This mirrors the Python SABnzbd API exactly so that existing
// clients (Sonarr, Radarr, mobile apps, user scripts) work unchanged.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// respondJSON writes a JSON response with the given HTTP status code.
// The body is marshaled from v; Content-Type is always application/json.
func respondJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v) //nolint:errcheck // write to ResponseWriter; error unrecoverable
}

// respondOK writes a JSON success envelope:
//
//	{"status": true, "<keyword>": <data>}
//
// When keyword is empty and data is a map or struct, the data is
// merged directly into the top level (matching Python's behavior for
// composite results like queue listings).
func respondOK(w http.ResponseWriter, keyword string, data any) {
	var body any
	if keyword == "" {
		body = data
	} else {
		body = map[string]any{
			"status": true,
			keyword:  data,
		}
	}
	respondJSON(w, http.StatusOK, body)
}

// respondStatus writes a bare {"status": true} envelope (no data).
func respondStatus(w http.ResponseWriter) {
	respondJSON(w, http.StatusOK, map[string]any{"status": true})
}

// respondError writes a JSON error envelope:
//
//	{"status": false, "error": "<msg>"}
//
// code is the HTTP status code (typically 400, 401, 403, or 500).
func respondError(w http.ResponseWriter, code int, msg string) {
	if code >= 500 {
		slog.Error("api response error", "status", code, "error", msg)
	}
	respondJSON(w, code, map[string]any{
		"status": false,
		"error":  msg,
	})
}
