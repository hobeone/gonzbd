package api

import (
	"net/http"
)

// modePause pauses all downloads.
func (s *Server) modePause(w http.ResponseWriter, r *http.Request) {
	if s.queue == nil {
		s.respondError(w, http.StatusInternalServerError, "queue not wired")
		return
	}

	s.queue.PauseAll()
	respondStatus(w)
}

// modeResume resumes all downloads.
func (s *Server) modeResume(w http.ResponseWriter, r *http.Request) {
	if s.queue == nil {
		s.respondError(w, http.StatusInternalServerError, "queue not wired")
		return
	}

	s.queue.ResumeAll()
	respondStatus(w)
}

// modeShutdown initiates server shutdown (not implemented).
func (s *Server) modeShutdown(w http.ResponseWriter, r *http.Request) {
	// TODO: Requires shutdown hook wired to main server loop.
	s.respondError(w, http.StatusNotImplemented, "not implemented in this build: shutdown")
}

// modeRestart restarts the server (not implemented).
func (s *Server) modeRestart(w http.ResponseWriter, r *http.Request) {
	// TODO: Requires restart mechanism.
	s.respondError(w, http.StatusNotImplemented, "not implemented in this build: restart")
}

// modeDisconnect disconnects all NNTP connections (not implemented).
func (s *Server) modeDisconnect(w http.ResponseWriter, r *http.Request) {
	// TODO: Requires Downloader interface with Disconnect method.
	s.respondError(w, http.StatusNotImplemented, "not implemented in this build: disconnect")
}

// modePausePP pauses post-processing (not implemented).
func (s *Server) modePausePP(w http.ResponseWriter, r *http.Request) {
	// TODO: Requires post-processor pause mechanism.
	s.respondError(w, http.StatusNotImplemented, "not implemented in this build: pause_pp")
}

// modeResumePP resumes post-processing (not implemented).
func (s *Server) modeResumePP(w http.ResponseWriter, r *http.Request) {
	// TODO: Requires post-processor pause mechanism.
	s.respondError(w, http.StatusNotImplemented, "not implemented in this build: resume_pp")
}
