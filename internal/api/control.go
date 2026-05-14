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
	if s.app != nil {
		s.app.PauseDownloads()
	}
	s.log.Info("downloads paused")
	respondStatus(w)
}

// modeResume resumes all downloads.
func (s *Server) modeResume(w http.ResponseWriter, r *http.Request) {
	if s.queue == nil {
		s.respondError(w, http.StatusInternalServerError, "queue not wired")
		return
	}

	s.queue.ResumeAll()
	if s.app != nil {
		s.app.ResumeDownloads()
	}
	s.log.Info("downloads resumed")
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

// modeDisconnect disconnects all idle NNTP connections. Workers stay alive
// and will re-dial lazily when new work arrives.
func (s *Server) modeDisconnect(w http.ResponseWriter, r *http.Request) {
	if s.app == nil {
		s.respondError(w, http.StatusInternalServerError, "app not wired")
		return
	}
	s.app.DisconnectAll()
	s.log.Info("NNTP connections disconnected")
	respondStatus(w)
}

// modePausePP pauses the post-processing pipeline.
func (s *Server) modePausePP(w http.ResponseWriter, r *http.Request) {
	if s.app == nil {
		s.respondError(w, http.StatusInternalServerError, "app not wired")
		return
	}
	s.app.PausePostProcessor()
	s.log.Info("post-processing paused")
	respondStatus(w)
}

// modeResumePP resumes the post-processing pipeline.
func (s *Server) modeResumePP(w http.ResponseWriter, r *http.Request) {
	if s.app == nil {
		s.respondError(w, http.StatusInternalServerError, "app not wired")
		return
	}
	s.app.ResumePostProcessor()
	s.log.Info("post-processing resumed")
	respondStatus(w)
}
