package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
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
// query string or POST form body, looks it up in the mode table,
// enforces auth, and dispatches to the handler.
//
// Third-party apps (Sonarr, Radarr, NZB360, etc.) may send mode as a
// POST form field rather than a URL query parameter. The body-size
// limit is already enforced by the middleware's MaxBytesReader, so
// parsing the form body here is safe from DoS.
func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	if mode == "" && r.Method == http.MethodPost {
		// Fall back to the POST form body. For multipart uploads
		// ParseMultipartForm reads the body under MaxBytesReader's
		// limit. For url-encoded forms, ParseForm is lightweight.
		mode = formValue(r, "mode")
	}
	if mode == "" {
		s.respondError(w, http.StatusBadRequest, "missing mode parameter")
		return
	}

	entry, ok := s.modes[mode]
	if !ok {
		s.respondError(w, http.StatusBadRequest, "unknown mode: "+mode)
		return
	}

	level := callerLevel(r, s.getAuth())
	if level < entry.level {
		if level == 0 {
			s.respondError(w, http.StatusUnauthorized, "API key required")
		} else {
			s.respondError(w, http.StatusForbidden, "insufficient access level")
		}
		return
	}

	entry.handler(w, r)
}

// formValue extracts a non-file form field from a POST request body.
// It handles both url-encoded and multipart/form-data content types.
// Unlike r.FormValue(), it does not fall back to query parameters
// (the caller handles that separately) and uses our controlled memory
// limits for multipart parsing.
func formValue(r *http.Request, key string) string {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		// Parse multipart with our limit. If already parsed,
		// ParseMultipartForm is a no-op.
		const maxMem = 10 * 1024 * 1024 // 10 MiB
		if err := r.ParseMultipartForm(maxMem); err != nil {
			return ""
		}
		if r.MultipartForm != nil {
			if vs := r.MultipartForm.Value[key]; len(vs) > 0 {
				return vs[0]
			}
		}
		return ""
	}
	// URL-encoded form body — ParseForm handles this.
	if err := r.ParseForm(); err != nil {
		return ""
	}
	return r.PostFormValue(key)
}

// registerModes populates the mode dispatch table with the built-in
// handlers. Steps 6.2-6.4 expand this list with queue, history, config,
// control, and misc mode handlers.
func (s *Server) registerModes() {
	s.modes = modeTable{
		"version":      {handler: s.modeVersion, level: LevelOpen},
		"auth":         {handler: s.modeAuth, level: LevelOpen},
		"queue":        {handler: s.modeQueue, level: LevelProtected},
		"addfile":      {handler: s.modeAddFile, level: LevelProtected},
		"addurl":       {handler: s.modeAddURL, level: LevelProtected},
		"addlocalfile": {handler: s.modeAddLocalFile, level: LevelProtected},
		"history":      {handler: s.modeHistory, level: LevelProtected},
		// Status modes
		"fullstatus":      {handler: s.modeFullStatus, level: LevelProtected},
		"status":          {handler: s.modeStatus, level: LevelProtected},
		"warnings":        {handler: s.modeWarnings, level: LevelProtected},
		"server_stats":    {handler: s.modeServerStats, level: LevelProtected},
		"status_overview": {handler: s.modeStatusOverview, level: LevelProtected},
		// Config modes
		"config":     {handler: s.modeConfig, level: LevelAdmin},
		"get_config": {handler: s.modeGetConfig, level: LevelAdmin},
		"set_config": {handler: s.modeSetConfig, level: LevelAdmin},
		// speedlimit is a SABnzbd top-level mode alias for
		// config&name=speedlimit; some clients (e.g. NZB360) call it
		// directly. Same handler, same behavior.
		"speedlimit": {handler: s.configSpeedLimit, level: LevelAdmin},
		// Control modes
		"pause":      {handler: s.modePause, level: LevelAdmin},
		"resume":     {handler: s.modeResume, level: LevelAdmin},
		"shutdown":   {handler: s.modeShutdown, level: LevelAdmin},
		"restart":    {handler: s.modeRestart, level: LevelAdmin},
		"disconnect": {handler: s.modeDisconnect, level: LevelAdmin},
		"pause_pp":   {handler: s.modePausePP, level: LevelAdmin},
		"resume_pp":  {handler: s.modeResumePP, level: LevelAdmin},
		// Misc modes
		"about":       {handler: s.modeAbout, level: LevelProtected},
		"get_cats":    {handler: s.modeGetCats, level: LevelProtected},
		"get_scripts": {handler: s.modeGetScripts, level: LevelProtected},
		"browse":      {handler: s.modeBrowse, level: LevelAdmin},
		"watched_now": {handler: s.modeWatchedNow, level: LevelProtected},
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
	key, _ := apiKeyFromRequest(r)
	if key == "" {
		respondOK(w, "auth", "apikey")
		return
	}
	auth := s.getAuth()
	// Use constant-time comparison to prevent timing attacks.
	// This endpoint is LevelOpen (unauthenticated).
	switch {
	case subtle.ConstantTimeCompare([]byte(key), []byte(auth.APIKey)) == 1:
		respondOK(w, "auth", "apikey")
	case subtle.ConstantTimeCompare([]byte(key), []byte(auth.NZBKey)) == 1:
		respondOK(w, "auth", "nzbkey")
	default:
		respondOK(w, "auth", "badkey")
	}
}
