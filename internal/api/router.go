package api

import (
	"net/http"
)

// modeEntry binds a handler function to its required access level.
type modeEntry struct {
	handler func(w http.ResponseWriter, r *http.Request)
	level   AccessLevel
}

// modeTable maps mode= values to their handlers and access levels.
// Populated by Server.registerModes during construction.
type modeTable map[string]modeEntry

// handleAPI is the single /api endpoint. It extracts mode= from the
// query/form, looks it up in the mode table, enforces auth, and
// dispatches to the handler.
func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	mode := r.FormValue("mode") //nolint:gosec // G120: body already limited by loggingMiddleware's MaxBytesReader
	if mode == "" {
		respondError(w, http.StatusBadRequest, "missing mode parameter")
		return
	}

	entry, ok := s.modes[mode]
	if !ok {
		respondError(w, http.StatusBadRequest, "unknown mode: "+mode)
		return
	}

	level := callerLevel(r, s.auth)
	if level < entry.level {
		if level == 0 {
			respondError(w, http.StatusUnauthorized, "API key required")
		} else {
			respondError(w, http.StatusForbidden, "insufficient access level")
		}
		return
	}

	entry.handler(w, r)
}

// registerModes populates the mode dispatch table with the built-in
// handlers. Steps 6.2-6.4 expand this list with queue, history, config,
// control, and misc mode handlers.
func (s *Server) registerModes() {
	s.modes = modeTable{
		"version": {handler: s.modeVersion, level: LevelOpen},
		"auth":    {handler: s.modeAuth, level: LevelOpen},
	}
}

// modeVersion returns the server version. No auth required.
func (s *Server) modeVersion(w http.ResponseWriter, _ *http.Request) {
	respondOK(w, "version", s.version)
}

// modeAuth validates the supplied API key and returns its type.
// Matches Python's _api_auth behavior: returns "apikey", "nzbkey", or
// "badkey" depending on what was supplied.
func (s *Server) modeAuth(w http.ResponseWriter, r *http.Request) {
	key := apiKeyFromRequest(r)
	if key == "" {
		respondOK(w, "auth", "apikey")
		return
	}
	switch key {
	case s.auth.APIKey:
		respondOK(w, "auth", "apikey")
	case s.auth.NZBKey:
		respondOK(w, "auth", "nzbkey")
	default:
		respondOK(w, "auth", "badkey")
	}
}
