package api

import (
	"net/http"

	"github.com/hobeone/gonzbd/internal/downloader"
)

// modeFullStatus returns a full status snapshot including queue paused state and slot count.
func (s *Server) modeFullStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireQueue(w) {
		return
	}

	var paused bool
	var noofslots int
	if s.dispatcher != nil {
		paused = s.dispatcher.Paused()
		noofslots = s.dispatcher.Len()
	}

	// Resolve the absolute complete_dir path. Sonarr reads
	// status.completedir when the config's complete_dir is relative.
	var completeDir string
	if s.config != nil {
		completeDir = s.config.GetGeneral().CompleteDir
	}

	statusData := map[string]any{
		"paused":       paused,
		"noofslots":    noofslots,
		"last_warning": "",
		"completedir":  completeDir,
	}

	respondOK(w, "status", statusData)
}

// modeStatus handles mode=status with sub-actions via name= parameter.
func (s *Server) modeStatus(w http.ResponseWriter, r *http.Request) {
	action := formValue(r, "name")
	if action == "" {
		// No action: fall through to fullstatus behavior
		s.modeFullStatus(w, r)
		return
	}

	switch action {
	case "unblock_server":
		s.statusUnblockServer(w, r)
	case "test_connection":
		s.statusTestConnection(w, r)
	case "test_disk_speed":
		s.statusTestDiskSpeed(w, r)
	case "check_update":
		s.statusCheckUpdate(w, r)
	case "build_info":
		s.statusBuildInfo(w, r)
	case "config":
		s.statusConfig(w, r)
	// Stubbed sub-actions: not yet implemented.
	case "delete_orphan", "delete_all_orphan", "add_orphan", "add_all_orphan":
		s.respondError(w, http.StatusNotImplemented, "not implemented in this build: "+action)
	default:
		s.respondError(w, http.StatusBadRequest, "unknown status action: "+action)
	}
}

// statusUnblockServer clears penalties on a named server.
// SABnzbd convention: value = server name.
func (s *Server) statusUnblockServer(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	name, ok := s.requireParam(w, r, "value", "server name")
	if !ok {
		return
	}
	if !s.downloads.UnblockServer(name) {
		s.respondError(w, http.StatusNotFound, "server not found: "+name)
		return
	}
	s.log.Info("server unblocked", "server", name)
	respondStatus(w)
}

// modeWarnings returns the warning list.
func (s *Server) modeWarnings(w http.ResponseWriter, r *http.Request) {
	action := formValue(r, "name")
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
	if !s.requireApp(w) {
		return
	}
	servers := s.status.ServerStatus()
	if servers == nil {
		servers = []downloader.ServerSnapshot{}
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"servers": servers,
	})
}
