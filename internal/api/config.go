package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/nntp"
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

// modeSetConfig sets configuration parameters.
func (s *Server) modeSetConfig(w http.ResponseWriter, r *http.Request) {
	if s.config == nil {
		respondError(w, http.StatusInternalServerError, "config not wired")
		return
	}

	section := formString(r, "section")
	keyword := formString(r, "keyword")
	value := formString(r, "value")

	if section == "" {
		respondError(w, http.StatusBadRequest, "missing section")
		return
	}

	if err := s.config.Set(section, keyword, value); err != nil {
		respondError(w, http.StatusBadRequest, "set config: "+err.Error())
		return
	}

	// Persist to disk if path is known.
	if s.configPath != "" {
		if err := s.config.Save(s.configPath); err != nil {
			s.log.Error("persist config", "path", s.configPath, "error", err)
			respondError(w, http.StatusInternalServerError, "persist config: "+err.Error())
			return
		}
	}

	respondOK(w, "value", value)
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
		s.configTestServer(w, r)
	case "create_backup":
		// TODO: Requires backup mechanism.
		respondError(w, http.StatusNotImplemented, "not implemented in this build: create_backup")
	default:
		respondError(w, http.StatusBadRequest, "unknown config action: "+action)
	}
}

const testServerTimeout = 15 * time.Second

// configTestServer dials an NNTP server using the parameters from the
// request, verifies the greeting and authentication, then closes the
// connection. The result tells the caller whether the server is reachable
// and accepts the supplied credentials.
func (s *Server) configTestServer(w http.ResponseWriter, r *http.Request) {
	host := formString(r, "host")
	if host == "" {
		respondError(w, http.StatusBadRequest, "missing host parameter")
		return
	}

	port, _ := strconv.Atoi(formString(r, "port"))
	if port == 0 {
		port = 119
	}
	ssl := formString(r, "ssl") == "1"
	if ssl && port == 119 {
		port = 563
	}

	sslVerify, _ := strconv.Atoi(formString(r, "ssl_verify"))

	cfg := config.ServerConfig{
		Name:     "test",
		Host:     host,
		Port:     port,
		Username: formString(r, "username"),
		Password: formString(r, "password"),
		SSL:      ssl,
		SSLVerify: config.SSLVerify(sslVerify),
		Connections: 1,
		Timeout:  int(testServerTimeout.Seconds()),
	}

	ctx, cancel := context.WithTimeout(r.Context(), testServerTimeout)
	defer cancel()

	conn, err := nntp.Dial(ctx, cfg)
	if err != nil {
		s.log.Warn("test_server failed", "host", host, "port", port, "error", err)
		respondOK(w, "result", map[string]any{
			"passed":  false,
			"message": err.Error(),
		})
		return
	}
	_ = conn.Close() //nolint:errcheck // test connection; close error is irrelevant

	s.log.Info("test_server passed", "host", host, "port", port)
	respondOK(w, "result", map[string]any{
		"passed":  true,
		"message": "Connection successful!",
	})
}
