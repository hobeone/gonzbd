package api

import (
	"net/http"
	"strconv"
	"testing"

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
