package api

import (
	"net/http"

	"github.com/hobeone/sabnzbd-go/internal/downloader"
)

// modeFullStatus returns a full status snapshot including queue paused state and slot count.
func (s *Server) modeFullStatus(w http.ResponseWriter, r *http.Request) {
	if s.queue == nil {
		s.respondError(w, http.StatusInternalServerError, "queue not wired")
		return
	}

	paused := s.queue.IsPaused()
	noofslots := s.queue.Len()

	statusData := map[string]any{
		"paused":       paused,
		"noofslots":    noofslots,
		"last_warning": "",
	}

	respondOK(w, "status", statusData)
}

// modeStatus handles mode=status with sub-actions via name= parameter.
// All sub-actions are stubbed (not implemented).
func (s *Server) modeStatus(w http.ResponseWriter, r *http.Request) {
	action := formString(r, "name")
	if action == "" {
		// No action: fall through to fullstatus behavior
		s.modeFullStatus(w, r)
		return
	}

	// All sub-actions (unblock_server, delete_orphan, etc.) are not implemented
	switch action {
	case "unblock_server", "delete_orphan", "delete_all_orphan", "add_orphan", "add_all_orphan":
		s.respondError(w, http.StatusNotImplemented, "not implemented in this build: "+action)
	default:
		s.respondError(w, http.StatusBadRequest, "unknown status action: "+action)
	}
}

// modeWarnings returns the warning list.
func (s *Server) modeWarnings(w http.ResponseWriter, r *http.Request) {
	action := formString(r, "name")
	if action == "clear" {
		s.ClearWarnings()
		respondOK(w, "warnings", []string{})
		return
	}

	respondOK(w, "warnings", s.Warnings())
}

// modeServerStats returns per-server connection statistics including
// health, BPS, penalty state, and per-connection article activity.
func (s *Server) modeServerStats(w http.ResponseWriter, r *http.Request) {
	if s.app == nil {
		s.respondError(w, http.StatusInternalServerError, "app not wired")
		return
	}
	servers := s.app.ServerStatus()
	if servers == nil {
		servers = []downloader.ServerSnapshot{}
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"servers": servers,
	})
}
