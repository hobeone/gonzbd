package api

import (
	"net/http"
)

// modePause pauses all downloads.
func (s *Server) modePause(w http.ResponseWriter, r *http.Request) {
	if !s.requireQueue(w) {
		return
	}

	if s.dispatcher != nil {
		s.dispatcher.Pause()
	}
	if s.downloads != nil {
		s.downloads.PauseDownloads()
	}
	s.log.Info("downloads paused")
	respondStatus(w)
}

// modeResume resumes all downloads.
func (s *Server) modeResume(w http.ResponseWriter, r *http.Request) {
	if !s.requireQueue(w) {
		return
	}

	if s.dispatcher != nil {
		s.dispatcher.Resume()
	}
	if s.downloads != nil {
		s.downloads.ResumeDownloads()
	}
	s.log.Info("downloads resumed")
	respondStatus(w)
}

// modeShutdown initiates a graceful application exit. The process exits
// cleanly so the process manager (systemd, Docker) can handle restarts.
func (s *Server) modeShutdown(w http.ResponseWriter, r *http.Request) {
	if s.shutdownFunc == nil {
		s.respondError(w, http.StatusNotImplemented, "not implemented in this build: shutdown")
		return
	}
	s.log.Info("shutdown requested via API")
	respondStatus(w)
	// Call shutdown asynchronously so the HTTP response is sent first.
	go s.shutdownFunc()
}

// modeRestart is an alias for modeShutdown. Go binaries don't self-restart;
// the process manager (systemd, Docker, etc.) is expected to restart the
// process after a clean exit.
func (s *Server) modeRestart(w http.ResponseWriter, r *http.Request) {
	s.modeShutdown(w, r)
}

// modeDisconnect disconnects all idle NNTP connections. Workers stay alive
// and will re-dial lazily when new work arrives.
func (s *Server) modeDisconnect(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	s.downloads.DisconnectAll()
	s.log.Info("NNTP connections disconnected")
	respondStatus(w)
}

// modePausePP and modeResumePP are not supported: gonzbd's post-processing
// pipeline has no pause/resume control independent of download pause (see
// docs/sabnzbd_spec.md). The modes stay registered rather than 404ing so
// SABnzbd-API clients that probe them (Sonarr, Radarr, NZB360-style tools)
// get a clear "not implemented" instead of treating a missing route as a
// connectivity failure. Mirrors the nil-shutdownFunc pattern in
// modeShutdown.
func (s *Server) modePausePP(w http.ResponseWriter, r *http.Request) {
	s.respondError(w, http.StatusNotImplemented, "not implemented in this build: pause_pp")
}

func (s *Server) modeResumePP(w http.ResponseWriter, r *http.Request) {
	s.respondError(w, http.StatusNotImplemented, "not implemented in this build: resume_pp")
}
