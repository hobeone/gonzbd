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
		s.respondError(w, http.StatusInternalServerError, "config not wired")
		return
	}

	// TODO: Implement filtering by section= and keyword= query params.
	// For now, return the full config.
	// Marshal the config to JSON for return.
	var raw json.RawMessage
	var marshalErr error
	s.config.WithRead(func(cfg *config.Config) {
		raw, marshalErr = json.Marshal(cfg)
	})
	if marshalErr != nil {
		s.respondError(w, http.StatusInternalServerError, "marshal config: "+marshalErr.Error())
		return
	}

	respondOK(w, "config", raw)
}

// modeSetConfig sets configuration parameters.
func (s *Server) modeSetConfig(w http.ResponseWriter, r *http.Request) {
	if s.config == nil {
		s.respondError(w, http.StatusInternalServerError, "config not wired")
		return
	}

	section := formString(r, "section")
	keyword := formString(r, "keyword")
	value := formString(r, "value")

	if section == "" {
		s.respondError(w, http.StatusBadRequest, "missing section")
		return
	}

	if err := s.config.Set(section, keyword, value); err != nil {
		s.respondError(w, http.StatusBadRequest, "set config: "+err.Error())
		return
	}

	// Persist to disk if path is known.
	if s.configPath != "" {
		if err := s.config.Save(s.configPath); err != nil {
			s.log.Error("persist config", "path", s.configPath, "error", err)
			s.respondError(w, http.StatusInternalServerError, "persist config: "+err.Error())
			return
		}
	}

	// Hot-reload core components if their configuration changed.
	if section == "servers" && s.app != nil {
		var servers []config.ServerConfig
		s.config.WithRead(func(cfg *config.Config) {
			servers = make([]config.ServerConfig, len(cfg.Servers))
			copy(servers, cfg.Servers)
		})
		if err := s.app.ReloadDownloader(servers); err != nil {
			s.log.Error("reload servers", "error", err)
			// Return 200 because the config was saved, but add a warning.
			respondJSON(w, http.StatusOK, map[string]any{
				"status":  true,
				"value":   value,
				"warning": "config saved but server reload failed: " + err.Error(),
			})
			return
		}
		s.log.Info("nntp servers reloaded")
	}

	// Hot-apply bandwidth limit changes without requiring a restart.
	if section == "downloads" && s.app != nil && (keyword == "bandwidth_max" || keyword == "bandwidth_perc") {
		s.applySpeedLimit()
	}

	respondOK(w, "value", value)
}

// applySpeedLimit reads bandwidth_max and bandwidth_perc from the live
// config and pushes the computed limit to the running downloader.
func (s *Server) applySpeedLimit() {
	var bytesPerSec int64
	s.config.WithRead(func(cfg *config.Config) {
		max := int64(cfg.Downloads.BandwidthMax)
		perc := cfg.Downloads.BandwidthPerc
		if perc <= 0 || perc > 100 {
			perc = 100
		}
		bytesPerSec = max * int64(perc) / 100
	})
	s.app.SetSpeedLimit(bytesPerSec)
	s.log.Info("speed limit applied", "bytes_per_sec", bytesPerSec)
}

// modeConfig handles mode=config with sub-actions via name= parameter.
func (s *Server) modeConfig(w http.ResponseWriter, r *http.Request) {
	action := formString(r, "name")
	switch action {
	case "speedlimit":
		s.configSpeedLimit(w, r)
	case "set_pause":
		// Not in spec
		s.respondError(w, http.StatusBadRequest, "unknown config action: "+action)
	case "set_apikey", "set_nzbkey":
		// Not in spec for get_config/set_config modes
		s.respondError(w, http.StatusBadRequest, "unknown config action: "+action)
	case "test_server":
		s.configTestServer(w, r)
	case "create_backup":
		// TODO: Requires backup mechanism.
		s.respondError(w, http.StatusNotImplemented, "not implemented in this build: create_backup")
	default:
		s.respondError(w, http.StatusBadRequest, "unknown config action: "+action)
	}
}

// configSpeedLimit handles mode=config&name=speedlimit&value=N.
//
// The value parameter follows the SABnzbd convention:
//   - An integer is interpreted as KiB/s (e.g. "500" → 512000 B/s).
//   - A suffixed string is parsed as an absolute byte rate (e.g. "1M").
//   - "0" or empty disables limiting.
//
// The limit is applied immediately to the running downloader and also
// persisted to the config so it survives a restart.
func (s *Server) configSpeedLimit(w http.ResponseWriter, r *http.Request) {
	if s.app == nil {
		s.respondError(w, http.StatusServiceUnavailable, "application not running")
		return
	}
	raw := formString(r, "value")
	if raw == "" {
		raw = "0"
	}

	// SABnzbd convention: plain numbers are KiB/s.
	var bytesPerSec int64
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		bytesPerSec = n * 1024
	} else {
		parsed, parseErr := config.ParseByteSize(raw)
		if parseErr != nil {
			s.respondError(w, http.StatusBadRequest, "invalid speed limit: "+parseErr.Error())
			return
		}
		bytesPerSec = int64(parsed)
	}

	s.app.SetSpeedLimit(bytesPerSec)

	// Persist as the new bandwidth_max so restarts honour the change.
	if s.config != nil {
		bsVal := config.ByteSize(bytesPerSec)
		_ = s.config.Set("downloads", "bandwidth_max", bsVal.String())
		if s.configPath != "" {
			if err := s.config.Save(s.configPath); err != nil {
				s.log.Error("persist speed limit", "error", err)
			}
		}
	}

	s.log.Info("speed limit set", "bytes_per_sec", bytesPerSec)
	respondOK(w, "value", bytesPerSec)
}

const testServerTimeout = 15 * time.Second

// configTestServer dials an NNTP server using the parameters from the
// request, verifies the greeting and authentication, then closes the
// connection. The result tells the caller whether the server is reachable
// and accepts the supplied credentials.
func (s *Server) configTestServer(w http.ResponseWriter, r *http.Request) {
	host := formString(r, "host")
	if host == "" {
		s.respondError(w, http.StatusBadRequest, "missing host parameter")
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

	sslVerifyStr := formString(r, "ssl_verify")
	sslVerify := config.SSLVerifyHostname // safe default
	if sslVerifyStr != "" {
		if v, err := strconv.Atoi(sslVerifyStr); err == nil {
			sslVerify = config.SSLVerify(v)
		}
	}
	if err := sslVerify.Validate(); err != nil {
		s.respondError(w, http.StatusBadRequest, "invalid ssl_verify value: "+err.Error())
		return
	}

	cfg := config.ServerConfig{
		Name:        "test",
		Host:        host,
		Port:        port,
		Username:    formString(r, "username"),
		Password:    formString(r, "password"),
		SSL:         ssl,
		SSLVerify:   sslVerify,
		Connections: 1,
		Timeout:     int(testServerTimeout.Seconds()),
	}

	ctx, cancel := context.WithTimeout(r.Context(), testServerTimeout)
	defer cancel()

	conn, err := nntp.Dial(ctx, cfg, nntp.WithLogger(s.log))
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
