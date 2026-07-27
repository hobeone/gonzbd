package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
)

// sabSectionAlias maps SABnzbd section names to our internal section names.
// SABnzbd uses "misc" for the catch-all general settings section; we use
// "general" internally. Sonarr/Radarr call get_config and expect
// config.misc.complete_dir to exist, so we must accept and emit "misc".
var sabSectionAlias = map[string]string{
	"misc": "general",
}

// sabSectionReverse maps our internal section names to SABnzbd names for
// JSON output. Inverse of sabSectionAlias.
var sabSectionReverse = map[string]string{
	"general": "misc",
}

// resolveSection maps external SABnzbd section names (e.g. "misc") to internal ones ("general").
func resolveSection(section string) string {
	if internal, ok := sabSectionAlias[section]; ok {
		return internal
	}
	return section
}

// externalSection maps internal section names (e.g. "general") to external SABnzbd ones ("misc").
func externalSection(section string) string {
	if external, ok := sabSectionReverse[section]; ok {
		return external
	}
	return section
}

// modeGetConfig returns the current configuration as JSON.
//
// The response uses SABnzbd-compatible section names so that third-party
// clients (Sonarr, Radarr, NZB360) can read expected paths like
// config.misc.complete_dir without error.
func (s *Server) modeGetConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireConfig(w) {
		return
	}

	section := formValue(r, "section")

	// Marshal a redacted copy of the config to JSON first, then optionally
	// filter by section. Redacted() replaces all credential fields
	// (API/NZB keys, server passwords, notification secrets) with a
	// placeholder so this response never leaks secrets — see SEC-2.
	redacted := s.config.Redacted()
	raw, marshalErr := json.Marshal(redacted)
	if marshalErr != nil {
		s.respondError(w, http.StatusInternalServerError, "marshal config: "+marshalErr.Error())
		return
	}

	// Unmarshal into a generic map so we can remap section names.
	var full map[string]json.RawMessage
	if err := json.Unmarshal(raw, &full); err != nil {
		s.respondError(w, http.StatusInternalServerError, "unmarshal config: "+err.Error())
		return
	}

	// Remap internal section names → SABnzbd names (e.g. "general" → "misc").
	remapped := make(map[string]json.RawMessage, len(full))
	for k, v := range full {
		remapped[externalSection(k)] = v
	}

	injectSABDefaults(remapped)

	// Ensure "sorters" is always present as an empty array, never omitted.
	// Sonarr iterates config.Sorters and will NRE if the key is absent.
	if _, ok := remapped["sorters"]; !ok {
		remapped["sorters"] = json.RawMessage("[]")
	}

	if section != "" {
		// Resolve SABnzbd aliases: "misc" → look up "general" data
		// (which is now stored under "misc" in remapped).
		sectionData, ok := remapped[section]
		if !ok {
			// Section not found — return empty object rather than error,
			// matching SABnzbd behavior.
			respondOK(w, "config", json.RawMessage("{}"))
			return
		}
		respondOK(w, "config", sectionData)
		return
	}

	respondOK(w, "config", remapped)
}

// injectSABDefaults merges SABnzbd-specific fields that Sonarr/Radarr
// expect into the "misc" section of the remapped config. Fields already
// present in the section are left unchanged — we only inject missing keys.
// Errors are silently ignored (unmarshal/marshal failures leave the section
// unchanged).
//
// Sorting is intentionally not implemented — it is handled by external apps
// (Sonarr, Radarr, etc.). These fields return false/empty so Sonarr doesn't
// warn about missing configuration.
func injectSABDefaults(remapped map[string]json.RawMessage) {
	miscRaw, ok := remapped["misc"]
	if !ok {
		return
	}
	var miscMap map[string]json.RawMessage
	if err := json.Unmarshal(miscRaw, &miscMap); err != nil {
		return
	}
	sabDefaults := map[string]any{
		"pre_check":                false,
		"enable_tv_sorting":        false,
		"tv_categories":            []string{},
		"enable_movie_sorting":     false,
		"movie_categories":         []string{},
		"enable_date_sorting":      false,
		"date_categories":          []string{},
		"history_retention":        "",
		"history_retention_option": "all",
		"history_retention_number": 0,
	}
	for field, val := range sabDefaults {
		if _, exists := miscMap[field]; !exists {
			if b, err := json.Marshal(val); err == nil {
				miscMap[field] = b
			}
		}
	}
	if b, err := json.Marshal(miscMap); err == nil {
		remapped["misc"] = b
	}
}

// modeSetConfig sets configuration parameters.
func (s *Server) modeSetConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireConfig(w) {
		return
	}

	section := resolveSection(formValue(r, "section"))
	keyword := formValue(r, "keyword")
	value := formValue(r, "value")

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
	if section == "servers" && s.reload != nil {
		servers := s.config.GetServers()
		if err := s.reload.ReloadDownloader(servers); err != nil {
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
	if section == "downloads" && s.downloads != nil && (keyword == "bandwidth_max" || keyword == "bandwidth_perc") {
		s.applySpeedLimit()
	}

	// Hot-apply postproc and download options for the next job.
	s.reloadSection(section)

	// Hot-apply directory changes: create the directory and push it to the
	// running application so future jobs use the new path immediately.
	if section == "general" && (keyword == "download_dir" || keyword == "complete_dir") {
		if warning := s.applyDirectoryChange(keyword); warning != "" {
			respondJSON(w, http.StatusOK, map[string]any{
				"status":  true,
				"value":   value,
				"warning": warning,
			})
			return
		}
	}

	respondOK(w, "value", value)
}

// applySpeedLimit reads bandwidth_max and bandwidth_perc from the live
// config and pushes the computed limit to the running downloader.
func (s *Server) applySpeedLimit() {
	dl := s.config.GetDownloads()
	maxBytes := int64(dl.BandwidthMax)
	perc := int(dl.BandwidthPerc)
	if perc <= 0 || perc > 100 {
		perc = 100
	}
	bytesPerSec := maxBytes * int64(perc) / 100
	s.downloads.SetSpeedLimit(bytesPerSec)
	s.downloads.SetBandwidthMax(maxBytes)
	s.downloads.SetBandwidthPerc(perc)
	s.log.Info("speed limit applied", "bytes_per_sec", bytesPerSec, "bandwidth_max", maxBytes, "bandwidth_perc", perc)
}

// reloadSection handles the runtime reloading of section-specific configuration options.
// Snapshotting the config is done under a read lock, and calling the reloader is done
// after releasing it to prevent self-deadlock (since sync.RWMutex is not reentrant).
func (s *Server) reloadSection(section string) {
	if s.reload == nil {
		return
	}
	switch section {
	case "postproc":
		pp := s.config.GetPostProc()
		gen := s.config.GetGeneral()
		s.reload.ReloadPostProcOptions(pp, gen.ScriptDir)
	case "downloads":
		d := s.config.GetDownloads()
		s.reload.ReloadDownloadOptions(d)
	case "general":
		g := s.config.GetGeneral()
		s.reload.ReloadGeneralOptions(g)
	}
}

// applyDirectoryChange handles the runtime directory creation and application for general config changes.
// Returns a warning string if directory creation failed.
func (s *Server) applyDirectoryChange(keyword string) string {
	if s.downloads == nil {
		return ""
	}
	gen := s.config.GetGeneral()
	var dir string
	if keyword == "download_dir" {
		dir = gen.DownloadDir
	} else {
		dir = gen.CompleteDir
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		s.log.Error("create directory", "keyword", keyword, "dir", dir, "error", err)
		return fmt.Sprintf("config saved but could not create %s: %v", dir, err)
	}
	if keyword == "download_dir" {
		s.downloads.SetDownloadDir(dir)
	} else {
		s.downloads.SetCompleteDir(dir)
	}
	s.log.Info("directory updated", "keyword", keyword, "dir", dir)
	return ""
}

// modeConfig handles mode=config with sub-actions via name= parameter.
func (s *Server) modeConfig(w http.ResponseWriter, r *http.Request) {
	action := formValue(r, "name")
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
	if s.downloads == nil {
		s.respondError(w, http.StatusServiceUnavailable, "application not running")
		return
	}
	raw := formValue(r, "value")
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

	s.downloads.SetSpeedLimit(bytesPerSec)

	// Persist as the new bandwidth_max so restarts honour the change.
	if s.config != nil {
		bsVal := config.ByteSize(bytesPerSec)
		_ = s.config.Set("downloads", "bandwidth_max", bsVal.String()) //nolint:errcheck // in-memory update failure is ignored; Save will catch disk errors
		if s.configPath != "" {
			if err := s.config.Save(s.configPath); err != nil {
				s.log.Error("persist speed limit", "error", err)
				s.respondError(w, http.StatusInternalServerError, err.Error())
				return
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
//
// Note: Unlike urlgrabber, configTestServer deliberately permits private/RFC1918
// and loopback IP addresses. Usenet servers may be hosted on private LANs or
// local test ports.
func (s *Server) configTestServer(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}

	host := formValue(r, "host")
	if host == "" {
		s.respondError(w, http.StatusBadRequest, "missing host parameter")
		return
	}

	port, _ := strconv.Atoi(formValue(r, "port")) //nolint:errcheck // handled by port == 0 check
	if port == 0 {
		port = 119
	}
	ssl := formValue(r, "ssl") == "1"
	if ssl && port == 119 {
		port = 563
	}

	sslVerifyStr := formValue(r, "ssl_verify")
	sslVerify := config.SSLVerifyHostname // safe default
	if sslVerifyStr != "" {
		if v, err := strconv.ParseInt(sslVerifyStr, 10, 8); err == nil {
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
		Username:    formValue(r, "username"),
		Password:    formValue(r, "password"),
		SSL:         ssl,
		SSLVerify:   sslVerify,
		Connections: 1,
		Timeout:     int(testServerTimeout.Seconds()),
	}

	ctx, cancel := context.WithTimeout(r.Context(), testServerTimeout)
	defer cancel()

	_, err := s.status.TestNNTPServer(ctx, cfg)
	if err != nil {

		s.log.Warn("test_server failed", "host", host, "port", port, "error", err)
		respondOK(w, "result", map[string]any{
			"passed":  false,
			"message": err.Error(),
		})
		return
	}

	s.log.Info("test_server passed", "host", host, "port", port)
	respondOK(w, "result", map[string]any{
		"passed":  true,
		"message": "Connection successful!",
	})
}
