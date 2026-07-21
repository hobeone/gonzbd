package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/api/apitest"

	"github.com/hobeone/gonzbd/internal/config"
)

func testServerWithConfig(t *testing.T, cfg *config.Config) *Server {
	if cfg != nil {
		cfg.With(func(c *config.Config) {
			c.General.APIKey = testAPIKey
			c.General.NZBKey = testNZBKey
		})
	}
	t.Helper()
	return New(Options{
		Version: "1.0.0-test",
		Config:  cfg,
	})
}

func TestModeGetConfig_Default(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	s := testServerWithConfig(t, cfg)

	rr := apiGet(t, s.Handler(), "/api?mode=get_config&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	if m["status"] != true {
		t.Errorf("status = %v; want true", m["status"])
	}

	if _, ok := m["config"]; !ok {
		t.Errorf("config key missing from response")
	}
}

// TestModeGetConfig_RedactsSecrets proves SEC-2: mode=get_config must never
// emit credential values in cleartext. It sets a distinctive value on every
// secret field covered by config.Redacted() and asserts none of them appear
// anywhere in the response body.
func TestModeGetConfig_RedactsSecrets(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}

	const (
		secretServerPassword = "s3cr3t-server-password-XYZ"
		secretEmailPassword  = "s3cr3t-email-password-XYZ"
		secretAppriseURL     = "https://discord.com/api/webhooks/123/s3cr3t-token-XYZ"
		secretAppriseService = "https://slack.com/hooks/s3cr3t-service-XYZ"
	)

	cfg.With(func(c *config.Config) {
		c.Servers = []config.ServerConfig{{
			Name:     "test-server",
			Host:     "news.example.com",
			Password: secretServerPassword,
		}}
		c.Notifications.Email.Password = secretEmailPassword
		c.Notifications.Apprise.URL = secretAppriseURL
		c.Notifications.Apprise.ServiceURL = secretAppriseService
	})

	s := testServerWithConfig(t, cfg)

	rr := apiGet(t, s.Handler(), "/api?mode=get_config&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	body := rr.Body.String()

	secrets := map[string]string{
		"apikey":          testAPIKey,
		"nzbkey":          testNZBKey,
		"server password": secretServerPassword,
		"email password":  secretEmailPassword,
		"apprise url":     secretAppriseURL,
		"apprise service": secretAppriseService,
	}
	for name, secret := range secrets {
		if strings.Contains(body, secret) {
			t.Errorf("response body leaks %s (%q)", name, secret)
		}
	}
}

func TestModeSetConfig_NoConfigWired(t *testing.T) {
	t.Parallel()
	s := New(Options{Version: "1.0.0"})
	rr := apiGet(t, s.Handler(), "/api?mode=set_config&apikey="+testAPIKey)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", rr.Code)
	}
}

func TestModeConfig_Speedlimit(t *testing.T) {
	t.Parallel()
	s := testServer()

	// Plain number → KiB/s convention (500 → 512000 B/s).
	rr := apiGet(t, s.Handler(), "/api?mode=config&name=speedlimit&value=500&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	if v, _ := m["value"].(float64); v != 500*1024 {
		t.Errorf("value = %v; want %d", m["value"], 500*1024)
	}

	// Disable limiting.
	rr = apiGet(t, s.Handler(), "/api?mode=config&name=speedlimit&value=0&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
}

func TestModeConfig_Speedlimit_SaveError(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	s := testServerWithConfig(t, cfg)
	s.setAppServices(apitest.NopApp{})
	s.configPath = "/dev/null/impossible/gonzbd.yaml"

	rr := apiGet(t, s.Handler(), "/api?mode=config&name=speedlimit&value=500&apikey="+testAPIKey)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500, got %d", rr.Code, rr.Code)
	}
}

func TestModeSetConfig_ExtraUnrarParamsDisallowed(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	s := testServerWithConfig(t, cfg)

	rr := apiGet(t, s.Handler(), "/api?mode=set_config&section=postproc&keyword=extra_unrar_params&value=-df&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400, got %d", rr.Code, rr.Code)
	}
}

// TestModeSpeedlimit_TopLevel verifies the SABnzbd-compatible top-level
// mode=speedlimit alias behaves identically to mode=config&name=speedlimit.
func TestModeSpeedlimit_TopLevel(t *testing.T) {
	t.Parallel()
	s := testServer()

	// Plain number → KiB/s convention (500 → 512000 B/s).
	rr := apiGet(t, s.Handler(), "/api?mode=speedlimit&value=500&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	if v, _ := m["value"].(float64); v != 500*1024 {
		t.Errorf("value = %v; want %d", m["value"], 500*1024)
	}

	// Without an API key the admin-level mode is rejected.
	rr = apiGet(t, s.Handler(), "/api?mode=speedlimit&value=0")
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401 without apikey", rr.Code)
	}
}

func TestModeConfig_TestServer_MissingHost(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=config&name=test_server&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rr.Code)
	}
	m := decodeJSON(t, rr)
	if m["error"] != "missing host parameter" {
		t.Errorf("error = %v; want 'missing host parameter'", m["error"])
	}
}

func TestModeConfig_TestServer_UnreachableHost(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=config&name=test_server&host=192.0.2.1&port=119&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	m := decodeJSON(t, rr)
	result, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("result missing or not a map: %v", m["result"])
	}
	if result["passed"] != false {
		t.Errorf("passed = %v; want false", result["passed"])
	}
	msg, _ := result["message"].(string)
	if msg == "" {
		t.Error("message should be non-empty for a failed connection")
	}
}

func TestModeConfig_CreateBackupNotImplemented(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=config&name=create_backup&apikey="+testAPIKey)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501", rr.Code)
	}
}

func TestModeConfig_UnknownAction(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=config&name=unknown&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rr.Code)
	}
}

func TestGetConfigConcurrentSafe(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	s := testServerWithConfig(t, cfg)

	// Run set_config and get_config concurrently to ensure no data races
	// when get_config serializes the config structure.

	done := make(chan struct{})
	go func() {
		for i := range 100 {
			rr := apiGet(t, s.Handler(), "/api?mode=set_config&section=general&keyword=download_dir&value=dir"+strconv.Itoa(i)+"&apikey="+testAPIKey)
			if rr.Code != http.StatusOK {
				t.Errorf("set_config failed: %d, body: %s", rr.Code, rr.Body.String())
			}
		}
		close(done)
	}()

	for range 100 {
		rr := apiGet(t, s.Handler(), "/api?mode=get_config&apikey="+testAPIKey)
		if rr.Code != http.StatusOK {
			t.Errorf("get_config failed: %d", rr.Code)
		}
	}

	<-done
}

// --- Sonarr/Radarr compatibility: section filtering ---

// TestGetConfig_SectionFilter verifies that section=misc returns the
// general config section (remapped for SABnzbd compatibility).
func TestGetConfig_SectionFilter(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	s := testServerWithConfig(t, cfg)

	// SABnzbd uses "misc" for the general settings section.
	// Sonarr/Radarr request section=misc to find complete_dir.
	rr := apiGet(t, s.Handler(), "/api?mode=get_config&section=misc&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	configVal, ok := m["config"]
	if !ok {
		t.Fatal("config key missing from response")
	}
	// The section value should be a map (the general config object).
	cfgMap, ok := configVal.(map[string]any)
	if !ok {
		t.Fatalf("config value is %T; want map[string]any", configVal)
	}
	// Should contain fields from GeneralConfig (like "host", "complete_dir").
	if _, hasHost := cfgMap["host"]; !hasHost {
		t.Error("section=misc should contain 'host' field")
	}
	if _, hasCompleteDir := cfgMap["complete_dir"]; !hasCompleteDir {
		t.Error("section=misc should contain 'complete_dir' field (Sonarr reads this)")
	}
	// Should NOT contain top-level fields like "servers" (that's a separate section).
	if _, hasServers := cfgMap["servers"]; hasServers {
		t.Error("section=misc should NOT contain 'servers' (that's the full config, not a section)")
	}
}

// TestGetConfig_FullResponseUsesMisc verifies that the full get_config
// response uses "misc" instead of "general" in the JSON keys.
func TestGetConfig_FullResponseUsesMisc(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	s := testServerWithConfig(t, cfg)

	rr := apiGet(t, s.Handler(), "/api?mode=get_config&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	configVal, ok := m["config"]
	if !ok {
		t.Fatal("config key missing from response")
	}
	cfgMap, ok := configVal.(map[string]any)
	if !ok {
		t.Fatalf("config value is %T; want map[string]any", configVal)
	}

	// Must have "misc", not "general".
	if _, hasMisc := cfgMap["misc"]; !hasMisc {
		t.Error("full config response should have 'misc' key (SABnzbd-compatible)")
	}
	if _, hasGeneral := cfgMap["general"]; hasGeneral {
		t.Error("full config response should NOT have 'general' key (remapped to 'misc')")
	}
	// Other sections should be unchanged.
	if _, hasServers := cfgMap["servers"]; !hasServers {
		t.Error("full config response should have 'servers' key")
	}
	if _, hasCategories := cfgMap["categories"]; !hasCategories {
		t.Error("full config response should have 'categories' key")
	}
}

// TestGetConfig_SectionGeneralReturnsEmpty verifies that the old
// "general" section name returns empty (since it was remapped to "misc").
func TestGetConfig_SectionGeneralReturnsEmpty(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	s := testServerWithConfig(t, cfg)

	rr := apiGet(t, s.Handler(), "/api?mode=get_config&section=general&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	configVal, ok := m["config"]
	if !ok {
		t.Fatal("config key missing from response")
	}
	cfgMap, ok := configVal.(map[string]any)
	if !ok {
		t.Fatalf("config value is %T; want map[string]any", configVal)
	}
	// "general" is no longer a valid section name — it was remapped to "misc".
	// Should return an empty object.
	if len(cfgMap) != 0 {
		t.Errorf("section=general should return empty config (remapped to misc), got %v", cfgMap)
	}
}

// TestGetConfig_SectionNotFound verifies that a nonexistent section returns
// an empty object rather than an error.
func TestGetConfig_SectionNotFound(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	s := testServerWithConfig(t, cfg)

	rr := apiGet(t, s.Handler(), "/api?mode=get_config&section=nonexistent_section&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (even for unknown section)", rr.Code)
	}

	m := decodeJSON(t, rr)
	configVal, ok := m["config"]
	if !ok {
		t.Fatal("config key missing from response")
	}
	cfgMap, ok := configVal.(map[string]any)
	if !ok {
		t.Fatalf("config value is %T; want map[string]any (empty object)", configVal)
	}
	if len(cfgMap) != 0 {
		t.Errorf("unknown section should return empty config, got %v", cfgMap)
	}
}

// --- Sonarr compatibility tests ---

// TestGetConfig_SonarrMiscFields verifies the misc section includes all fields
// that Sonarr reads: sorting toggles, retention settings, and pre_check.
func TestGetConfig_SonarrMiscFields(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	s := testServerWithConfig(t, cfg)

	rr := apiGet(t, s.Handler(), "/api?mode=get_config&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	cfgMap := m["config"].(map[string]any)
	misc := cfgMap["misc"].(map[string]any)

	// These fields must be present for Sonarr's category validation and
	// RemovesCompletedDownloads check.
	requiredFields := []string{
		"pre_check",
		"enable_tv_sorting",
		"tv_categories",
		"enable_movie_sorting",
		"movie_categories",
		"enable_date_sorting",
		"date_categories",
		"history_retention",
		"history_retention_option",
		"history_retention_number",
		"complete_dir",
	}

	for _, field := range requiredFields {
		if _, ok := misc[field]; !ok {
			t.Errorf("misc.%s missing from config response (Sonarr reads this)", field)
		}
	}

	// Sorting must be disabled by default.
	for _, field := range []string{"enable_tv_sorting", "enable_movie_sorting", "enable_date_sorting", "pre_check"} {
		if val, ok := misc[field].(bool); ok && val {
			t.Errorf("misc.%s = true; want false (default)", field)
		}
	}
}

// TestGetConfig_SortersAlwaysPresent verifies the sorters key is always an
// array (never omitted). Sonarr iterates config.Sorters.
func TestGetConfig_SortersAlwaysPresent(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	s := testServerWithConfig(t, cfg)

	rr := apiGet(t, s.Handler(), "/api?mode=get_config&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	cfgMap := m["config"].(map[string]any)

	sorters, ok := cfgMap["sorters"]
	if !ok {
		t.Fatal("sorters key missing from config response (Sonarr iterates this)")
	}
	arr, ok := sorters.([]any)
	if !ok {
		t.Fatalf("sorters is %T; want array", sorters)
	}
	// Default config has no sorters; should be empty array not null.
	if arr == nil {
		t.Error("sorters should be empty array, not null")
	}
}

func TestInjectSABDefaultsDirect(t *testing.T) {
	t.Parallel()

	t.Run("missing misc key", func(t *testing.T) {
		remapped := map[string]json.RawMessage{
			"servers": json.RawMessage(`[]`),
		}
		injectSABDefaults(remapped)
		if len(remapped) != 1 {
			t.Errorf("len(remapped) = %d, want 1", len(remapped))
		}
	})

	t.Run("invalid json in misc", func(t *testing.T) {
		remapped := map[string]json.RawMessage{
			"misc": json.RawMessage(`{invalid-json`),
		}
		injectSABDefaults(remapped)
		if string(remapped["misc"]) != `{invalid-json` {
			t.Errorf("misc was modified: %s", string(remapped["misc"]))
		}
	})

	t.Run("inject all defaults", func(t *testing.T) {
		remapped := map[string]json.RawMessage{
			"misc": json.RawMessage(`{}`),
		}
		injectSABDefaults(remapped)

		var m map[string]any
		if err := json.Unmarshal(remapped["misc"], &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		expected := []string{
			"pre_check", "enable_tv_sorting", "tv_categories",
			"enable_movie_sorting", "movie_categories", "enable_date_sorting",
			"date_categories", "history_retention", "history_retention_option",
			"history_retention_number",
		}
		for _, key := range expected {
			if _, ok := m[key]; !ok {
				t.Errorf("missing default key: %s", key)
			}
		}
	})

	t.Run("preserve existing fields", func(t *testing.T) {
		remapped := map[string]json.RawMessage{
			"misc": json.RawMessage(`{"pre_check":true,"existing_custom_field":"val"}`),
		}
		injectSABDefaults(remapped)

		var m map[string]any
		if err := json.Unmarshal(remapped["misc"], &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		if m["pre_check"] != true {
			t.Errorf("pre_check was overwritten, got %v, want true", m["pre_check"])
		}
		if m["existing_custom_field"] != "val" {
			t.Errorf("existing_custom_field was overwritten, got %v, want 'val'", m["existing_custom_field"])
		}
		if _, ok := m["enable_tv_sorting"]; !ok {
			t.Error("missing injected default key: enable_tv_sorting")
		}
	})
}

var _ AppServices = (*apitest.NopApp)(nil)

type setConfigSpyApp struct {
	apitest.NopApp
	mu                  sync.Mutex
	reloadDownloaderErr error
	reloadedDownloader  int
	reloadedPostProc    int
	reloadedDownloads   int
	reloadedGeneral     int
	downloadDir         string
	completeDir         string
	speedLimit          int64
	bandwidthMax        int64
	bandwidthPerc       int
}

func (a *setConfigSpyApp) ReloadDownloader(_ []config.ServerConfig) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reloadedDownloader++
	return a.reloadDownloaderErr
}

func (a *setConfigSpyApp) ReloadPostProcOptions(_ config.PostProcConfig, _ string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reloadedPostProc++
}

func (a *setConfigSpyApp) ReloadDownloadOptions(_ config.DownloadConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reloadedDownloads++
}

func (a *setConfigSpyApp) ReloadGeneralOptions(_ config.GeneralConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reloadedGeneral++
}

func (a *setConfigSpyApp) SetDownloadDir(dir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.downloadDir = dir
}

func (a *setConfigSpyApp) SetCompleteDir(dir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.completeDir = dir
}

func (a *setConfigSpyApp) SetSpeedLimit(limit int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.speedLimit = limit
}

func (a *setConfigSpyApp) SetBandwidthMax(m int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bandwidthMax = m
}

func (a *setConfigSpyApp) SetBandwidthPerc(p int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bandwidthPerc = p
}

func TestModeSetConfig_Comprehensive(t *testing.T) {
	t.Parallel()

	t.Run("config_not_wired", func(t *testing.T) {
		t.Parallel()
		s := New(Options{Version: "1.0.0"})
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api?mode=set_config&section=misc", nil)
		s.modeSetConfig(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d; want 500", rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "config not wired") {
			t.Errorf("body = %q; want to contain 'config not wired'", rr.Body.String())
		}
	})

	t.Run("save_error", func(t *testing.T) {
		t.Parallel()
		cfg, err := config.Default()
		if err != nil {
			t.Fatalf("Default(): %v", err)
		}
		s := testServerWithConfig(t, cfg)
		s.configPath = "/dev/null/impossible/gonzbd.yaml"

		rr := apiGet(t, s.Handler(), "/api?mode=set_config&section=misc&keyword=download_dir&value=/tmp/test&apikey="+testAPIKey)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d; want 500", rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "persist config") {
			t.Errorf("body = %q; want to contain 'persist config'", rr.Body.String())
		}
	})

	t.Run("reload_servers_success", func(t *testing.T) {
		t.Parallel()
		cfg, err := config.Default()
		if err != nil {
			t.Fatalf("Default(): %v", err)
		}
		spy := &setConfigSpyApp{}
		s := New(Options{
			Version: "1.0.0-test",
			Config:  cfg,
			App:     spy,
		})
		cfg.With(func(c *config.Config) {
			c.General.APIKey = testAPIKey
		})

		rr := apiGet(t, s.Handler(), "/api?mode=set_config&section=servers&value=%5B%7B%22name%22%3A%22s1%22%2C%22host%22%3A%22news.example.com%22%2C%22port%22%3A119%2C%22connections%22%3A4%2C%22timeout%22%3A60%2C%22pipelining_requests%22%3A2%7D%5D&apikey="+testAPIKey)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
		}
		spy.mu.Lock()
		count := spy.reloadedDownloader
		spy.mu.Unlock()
		if count != 1 {
			t.Errorf("reloadedDownloader = %d; want 1", count)
		}
	})

	t.Run("reload_servers_error", func(t *testing.T) {
		t.Parallel()
		cfg, err := config.Default()
		if err != nil {
			t.Fatalf("Default(): %v", err)
		}
		spy := &setConfigSpyApp{reloadDownloaderErr: errors.New("server reload failed")}
		s := New(Options{
			Version: "1.0.0-test",
			Config:  cfg,
			App:     spy,
		})
		cfg.With(func(c *config.Config) {
			c.General.APIKey = testAPIKey
		})

		rr := apiGet(t, s.Handler(), "/api?mode=set_config&section=servers&value=%5B%7B%22name%22%3A%22s1%22%2C%22host%22%3A%22news.example.com%22%2C%22port%22%3A119%2C%22connections%22%3A4%2C%22timeout%22%3A60%2C%22pipelining_requests%22%3A2%7D%5D&apikey="+testAPIKey)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
		}
		m := decodeJSON(t, rr)
		warning, _ := m["warning"].(string)
		if !strings.Contains(warning, "server reload failed") {
			t.Errorf("warning = %q; want to contain 'server reload failed'", warning)
		}
		spy.mu.Lock()
		count := spy.reloadedDownloader
		spy.mu.Unlock()
		if count != 1 {
			t.Errorf("reloadedDownloader = %d; want 1", count)
		}
	})

	t.Run("bandwidth_perc_out_of_bounds", func(t *testing.T) {
		t.Parallel()
		cfg, err := config.Default()
		if err != nil {
			t.Fatalf("Default(): %v", err)
		}
		spy := &setConfigSpyApp{}
		s := New(Options{
			Version: "1.0.0-test",
			Config:  cfg,
			App:     spy,
		})
		cfg.With(func(c *config.Config) {
			c.General.APIKey = testAPIKey
			c.Downloads.BandwidthPerc = 150
		})

		s.applySpeedLimit()
		spy.mu.Lock()
		perc1 := spy.bandwidthPerc
		spy.mu.Unlock()
		if perc1 != 100 {
			t.Errorf("bandwidthPerc for 150 = %d; want 100", perc1)
		}

		cfg.With(func(c *config.Config) {
			c.Downloads.BandwidthPerc = 0
		})
		s.applySpeedLimit()
		spy.mu.Lock()
		perc2 := spy.bandwidthPerc
		spy.mu.Unlock()
		if perc2 != 100 {
			t.Errorf("bandwidthPerc for 0 = %d; want 100", perc2)
		}

		cfg.With(func(c *config.Config) {
			c.Downloads.BandwidthPerc = 50
		})
		rr := apiGet(t, s.Handler(), "/api?mode=set_config&section=downloads&keyword=bandwidth_max&value=5000000&apikey="+testAPIKey)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200", rr.Code)
		}
		spy.mu.Lock()
		perc3 := spy.bandwidthPerc
		spy.mu.Unlock()
		if perc3 != 50 {
			t.Errorf("bandwidthPerc = %d; want 50", perc3)
		}
	})

	t.Run("reload_options_sections", func(t *testing.T) {
		t.Parallel()
		cfg, err := config.Default()
		if err != nil {
			t.Fatalf("Default(): %v", err)
		}
		spy := &setConfigSpyApp{}
		s := New(Options{
			Version: "1.0.0-test",
			Config:  cfg,
			App:     spy,
		})
		cfg.With(func(c *config.Config) {
			c.General.APIKey = testAPIKey
		})

		// postproc
		rr := apiGet(t, s.Handler(), "/api?mode=set_config&section=postproc&keyword=enable_unrar&value=true&apikey="+testAPIKey)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200", rr.Code)
		}
		// downloads
		rr = apiGet(t, s.Handler(), "/api?mode=set_config&section=downloads&keyword=min_free_space&value=100&apikey="+testAPIKey)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200", rr.Code)
		}
		// general
		rr = apiGet(t, s.Handler(), "/api?mode=set_config&section=general&keyword=host&value=127.0.0.1&apikey="+testAPIKey)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200", rr.Code)
		}

		spy.mu.Lock()
		pp := spy.reloadedPostProc
		dl := spy.reloadedDownloads
		gen := spy.reloadedGeneral
		spy.mu.Unlock()

		if pp != 1 {
			t.Errorf("reloadedPostProc = %d; want 1", pp)
		}
		if dl != 1 {
			t.Errorf("reloadedDownloads = %d; want 1", dl)
		}
		if gen != 1 {
			t.Errorf("reloadedGeneral = %d; want 1", gen)
		}
	})

	t.Run("mkdir_dirs_success", func(t *testing.T) {
		t.Parallel()
		cfg, err := config.Default()
		if err != nil {
			t.Fatalf("Default(): %v", err)
		}
		spy := &setConfigSpyApp{}
		s := New(Options{
			Version: "1.0.0-test",
			Config:  cfg,
			App:     spy,
		})
		cfg.With(func(c *config.Config) {
			c.General.APIKey = testAPIKey
		})

		tmpDir := t.TempDir()
		downDir := filepath.Join(tmpDir, "down")
		compDir := filepath.Join(tmpDir, "comp")

		// download_dir
		rr := apiGet(t, s.Handler(), "/api?mode=set_config&section=general&keyword=download_dir&value="+downDir+"&apikey="+testAPIKey)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200", rr.Code)
		}
		// complete_dir
		rr = apiGet(t, s.Handler(), "/api?mode=set_config&section=general&keyword=complete_dir&value="+compDir+"&apikey="+testAPIKey)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200", rr.Code)
		}

		if _, err := os.Stat(downDir); err != nil {
			t.Errorf("downDir not created: %v", err)
		}
		if _, err := os.Stat(compDir); err != nil {
			t.Errorf("compDir not created: %v", err)
		}

		spy.mu.Lock()
		d1 := spy.downloadDir
		d2 := spy.completeDir
		spy.mu.Unlock()

		if d1 != downDir {
			t.Errorf("downloadDir = %q; want %q", d1, downDir)
		}
		if d2 != compDir {
			t.Errorf("completeDir = %q; want %q", d2, compDir)
		}
	})

	t.Run("mkdir_error", func(t *testing.T) {
		t.Parallel()
		cfg, err := config.Default()
		if err != nil {
			t.Fatalf("Default(): %v", err)
		}
		spy := &setConfigSpyApp{}
		s := New(Options{
			Version: "1.0.0-test",
			Config:  cfg,
			App:     spy,
		})
		cfg.With(func(c *config.Config) {
			c.General.APIKey = testAPIKey
		})

		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "somefile")
		if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		invalidDir := filepath.Join(filePath, "subdir")

		rr := apiGet(t, s.Handler(), "/api?mode=set_config&section=general&keyword=download_dir&value="+invalidDir+"&apikey="+testAPIKey)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200", rr.Code)
		}
		m := decodeJSON(t, rr)
		warning, _ := m["warning"].(string)
		if !strings.Contains(warning, "could not create") {
			t.Errorf("warning = %q; want to contain 'could not create'", warning)
		}
	})
}

func TestModeConfig_Speedlimit_NoApp(t *testing.T) {
	t.Parallel()
	s := testServer()
	s.setAppServices(nil)
	rr := apiGet(t, s.Handler(), "/api?mode=config&name=speedlimit&value=500&apikey="+testAPIKey)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", rr.Code)
	}
}

func TestModeConfig_Speedlimit_InvalidValue(t *testing.T) {
	t.Parallel()
	s := testServer()
	rr := apiGet(t, s.Handler(), "/api?mode=config&name=speedlimit&value=invalid&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rr.Code)
	}
}

func TestModeConfig_TestServer_InvalidSSLVerify(t *testing.T) {
	t.Parallel()
	s := testServer()
	rr := apiGet(t, s.Handler(), "/api?mode=config&name=test_server&host=localhost&ssl_verify=99&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rr.Code)
	}
}

func TestModeConfig_SpecialActions(t *testing.T) {
	t.Parallel()
	s := testServer()
	for _, act := range []string{"set_pause", "set_apikey", "set_nzbkey"} {
		rr := apiGet(t, s.Handler(), "/api?mode=config&name="+act+"&apikey="+testAPIKey)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("action=%s: status = %d; want 400", act, rr.Code)
		}
	}
}
