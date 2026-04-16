package api

import (
	"encoding/json"
	"net/http"
)

// modeGetConfig returns the current configuration as JSON.
func (s *Server) modeGetConfig(w http.ResponseWriter, r *http.Request) {
	if s.config == nil {
		respondError(w, http.StatusInternalServerError, "config not wired")
		return
	}

	// TODO: Implement filtering by section= and keyword= query params.
	// For now, return the full config.
	// Marshal the config to JSON for return.
	data, err := json.Marshal(s.config)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "marshal config: "+err.Error())
		return
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		respondError(w, http.StatusInternalServerError, "unmarshal config: "+err.Error())
		return
	}

	respondOK(w, "config", m)
}

// modeSetConfig sets configuration parameters (not implemented).
func (s *Server) modeSetConfig(w http.ResponseWriter, r *http.Request) {
	// TODO: Phase 7 work. Requires config reload and persistence hooks.
	respondError(w, http.StatusNotImplemented, "not implemented in this build: set_config")
}

// modeConfig handles mode=config with sub-actions via name= parameter.
func (s *Server) modeConfig(w http.ResponseWriter, r *http.Request) {
	action := formString(r, "name")
	switch action {
	case "speedlimit":
		// TODO: Requires Downloader interface with LimitSpeed.
		respondError(w, http.StatusNotImplemented, "not implemented in this build: speedlimit")
	case "set_pause":
		// Not in spec
		respondError(w, http.StatusBadRequest, "unknown config action: "+action)
	case "set_apikey", "set_nzbkey":
		// Not in spec for get_config/set_config modes
		respondError(w, http.StatusBadRequest, "unknown config action: "+action)
	case "test_server":
		// TODO: Requires live NNTP connectivity test.
		respondError(w, http.StatusNotImplemented, "not implemented in this build: test_server")
	case "create_backup":
		// TODO: Requires backup mechanism.
		respondError(w, http.StatusNotImplemented, "not implemented in this build: create_backup")
	default:
		respondError(w, http.StatusBadRequest, "unknown config action: "+action)
	}
}
