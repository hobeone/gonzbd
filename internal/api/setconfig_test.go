package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/api/apitest"

	"github.com/hobeone/gonzbd/internal/config"
)

// TestModeSetConfig_BasicRoundTrip verifies a set followed by a get
// returns the updated value.
func TestModeSetConfig_BasicRoundTrip(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	s := testServerWithConfig(t, cfg)
	h := s.Handler()

	// Set download_dir to a new value.
	rr := apiGet(t, h, "/api?mode=set_config&section=misc&keyword=download_dir&value=/tmp/test_dir&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("set_config status = %d; want 200; body = %s", rr.Code, rr.Body.String())
	}

	m := decodeJSON(t, rr)
	if val, _ := m["value"].(string); val != "/tmp/test_dir" {
		t.Errorf("set_config response value = %q; want /tmp/test_dir", val)
	}

	// Read it back via get_config section=misc.
	rr = apiGet(t, h, "/api?mode=get_config&section=misc&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("get_config status = %d; want 200", rr.Code)
	}
	m = decodeJSON(t, rr)
	cfgVal, ok := m["config"].(map[string]any)
	if !ok {
		t.Fatalf("config missing or wrong type: %T", m["config"])
	}
	if dir, _ := cfgVal["download_dir"].(string); dir != "/tmp/test_dir" {
		t.Errorf("download_dir = %q; want /tmp/test_dir", dir)
	}
}

// TestModeSetConfig_MissingSection verifies that a set_config call
// without a section returns 400.
func TestModeSetConfig_MissingSection(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	s := testServerWithConfig(t, cfg)

	rr := apiGet(t, s.Handler(), "/api?mode=set_config&keyword=download_dir&value=/tmp/x&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400; body = %s", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	if _, ok := m["error"]; !ok {
		t.Error("expected error field in response")
	}
}

// TestModeSetConfig_InvalidKeyword verifies that setting a non-existent
// keyword returns 400.
func TestModeSetConfig_InvalidKeyword(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	s := testServerWithConfig(t, cfg)

	rr := apiGet(t, s.Handler(), "/api?mode=set_config&section=misc&keyword=nonexistent_field_xyz&value=bad&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 for unknown keyword; body = %s", rr.Code, rr.Body.String())
	}
}

// TestModeSetConfig_MiscAlias verifies that section=misc is accepted
// as an alias for section=general.
func TestModeSetConfig_MiscAlias(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	s := testServerWithConfig(t, cfg)
	h := s.Handler()

	// Set via "misc" alias.
	rr := apiGet(t, h, "/api?mode=set_config&section=misc&keyword=host&value=0.0.0.0&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("set_config via 'misc' status = %d; want 200; body = %s", rr.Code, rr.Body.String())
	}

	// Read via "general" (internal name mapped to "misc" in output).
	rr = apiGet(t, h, "/api?mode=get_config&section=misc&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("get_config status = %d; want 200", rr.Code)
	}
	m := decodeJSON(t, rr)
	cfgVal := m["config"].(map[string]any)
	if host, _ := cfgVal["host"].(string); host != "0.0.0.0" {
		t.Errorf("host = %q; want 0.0.0.0", host)
	}
}

// TestModeSetConfig_PersistsToDisk verifies that set_config writes the
// updated config to the config file on disk.
func TestModeSetConfig_PersistsToDisk(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "gonzbd.yaml")

	// Save initial config.
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("Save(): %v", err)
	}

	s := New(Options{
		Version:    "1.0.0-test",
		Config:     cfg,
		ConfigPath: cfgPath,
	})

	cfg.With(func(c *config.Config) {
		c.General.APIKey = testAPIKey
		c.General.NZBKey = testNZBKey
	})

	// Set a value.
	rr := apiGet(t, s.Handler(), "/api?mode=set_config&section=misc&keyword=download_dir&value=/tmp/persisted&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("set_config status = %d; want 200; body = %s", rr.Code, rr.Body.String())
	}

	// Read the file back and verify.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("config file is empty after set_config")
	}

	// Load the saved config and verify the value.
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	dir := loaded.GetGeneral().DownloadDir
	if dir != "/tmp/persisted" {
		t.Errorf("persisted download_dir = %q; want /tmp/persisted", dir)
	}
}

// TestModeSetConfig_ConcurrentSafe verifies that concurrent set_config
// calls don't race.
func TestModeSetConfig_ConcurrentSafe(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	s := testServerWithConfig(t, cfg)
	h := s.Handler()

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			rr := apiGet(t, h, "/api?mode=set_config&section=misc&keyword=download_dir&value=/tmp/dir"+strconv.Itoa(i)+"&apikey="+testAPIKey)
			if rr.Code != http.StatusOK {
				t.Errorf("concurrent set_config status = %d; body = %s", rr.Code, rr.Body.String())
			}
		})
	}
	wg.Wait()
}

type speedLimitSpyApp struct {
	apitest.NopApp
	mu         sync.Mutex
	speedLimit int64
	maxSpeed   int64
	perc       int
}

func (a *speedLimitSpyApp) SetSpeedLimit(v int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.speedLimit = v
}

func (a *speedLimitSpyApp) SetBandwidthMax(v int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.maxSpeed = v
}

func (a *speedLimitSpyApp) SetBandwidthPerc(v int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.perc = v
}

// TestModeSetConfig_SpeedLimits verifies that setting downloads/bandwidth_max
// and downloads/bandwidth_perc hot-applies the limit via the App interface.
func TestModeSetConfig_SpeedLimits(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) {
		c.General.APIKey = testAPIKey
		c.General.NZBKey = testNZBKey
	})

	app := &speedLimitSpyApp{}
	s := New(Options{
		Version: "1.0.0-test",
		Config:  cfg,
		App:     app,
	})

	h := s.Handler()

	// 1. Set bandwidth_max to 5000000 bytes/sec
	rr := apiGet(t, h, "/api?mode=set_config&section=downloads&keyword=bandwidth_max&value=5000000&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("set_config status = %d; want 200; body = %s", rr.Code, rr.Body.String())
	}

	// 2. Set bandwidth_perc to 50%
	rr = apiGet(t, h, "/api?mode=set_config&section=downloads&keyword=bandwidth_perc&value=50&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("set_config status = %d; want 200; body = %s", rr.Code, rr.Body.String())
	}

	app.mu.Lock()
	maxSpeed := app.maxSpeed
	perc := app.perc
	speedLimit := app.speedLimit
	app.mu.Unlock()

	if maxSpeed != 5000000 {
		t.Errorf("maxSpeed = %d; want 5000000", maxSpeed)
	}
	if perc != 50 {
		t.Errorf("perc = %d; want 50", perc)
	}
	if speedLimit != 2500000 {
		t.Errorf("speedLimit = %d; want 2500000", speedLimit)
	}
}
