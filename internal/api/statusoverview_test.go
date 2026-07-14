package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/hobeone/gonzbd/internal/api/apitest"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
)

type statusOverviewSpyApp struct {
	apitest.NopApp
	binaryVersions  app.BinaryVersions
	articleCache    int64
	downloadDirFree int64
	downloadDirErr  error
}

func (a *statusOverviewSpyApp) BinaryVersionsInfo() app.BinaryVersions { return a.binaryVersions }
func (a *statusOverviewSpyApp) ArticleCacheBytes() int64               { return a.articleCache }
func (a *statusOverviewSpyApp) DownloadDirFreeBytes(context.Context) (int64, error) {
	return a.downloadDirFree, a.downloadDirErr
}

func TestModeStatusOverview_ReturnsGeneralAndSystemSections(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) {
		c.General.APIKey = testAPIKey
		c.General.DownloadDir = "/tmp/downloads"
		c.Downloads.MinFreeSpace = 1024 * 1024 * 1024
	})
	spy := &statusOverviewSpyApp{
		binaryVersions:  app.BinaryVersions{Par2Version: "1.0", UnrarVersion: "6.24", SevenzVersion: "23.01"},
		articleCache:    12345,
		downloadDirFree: 987654321,
	}
	s := New(Options{Version: "v1.2.0", Commit: "abc123", Config: cfg, App: spy})

	rr := apiGet(t, s.Handler(), "/api?mode=status_overview&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)

	general, ok := m["general"].(map[string]any)
	if !ok {
		t.Fatalf("expected general section, got %v", m)
	}
	if general["version"] != "v1.2.0" {
		t.Errorf("general.version = %v; want v1.2.0", general["version"])
	}
	unrar, ok := general["unrar"].(map[string]any)
	if !ok || unrar["version"] != "6.24" {
		t.Errorf("general.unrar.version = %v; want 6.24", general["unrar"])
	}

	system, ok := m["system"].(map[string]any)
	if !ok {
		t.Fatalf("expected system section, got %v", m)
	}
	if system["article_cache_bytes"].(float64) != 12345 {
		t.Errorf("system.article_cache_bytes = %v; want 12345", system["article_cache_bytes"])
	}
	if system["download_dir_free_bytes"].(float64) != 987654321 {
		t.Errorf("system.download_dir_free_bytes = %v; want 987654321", system["download_dir_free_bytes"])
	}
	if system["min_free_space_bytes"].(float64) != 1024*1024*1024 {
		t.Errorf("system.min_free_space_bytes = %v; want %d", system["min_free_space_bytes"], 1024*1024*1024)
	}
}

func TestModeStatusOverview_DownloadDirFreeBytesError(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.APIKey = testAPIKey })
	spy := &statusOverviewSpyApp{downloadDirErr: errors.New("stat failed")}
	s := New(Options{Config: cfg, App: spy})

	rr := apiGet(t, s.Handler(), "/api?mode=status_overview&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	system, ok := m["system"].(map[string]any)
	if !ok {
		t.Fatalf("expected system section, got %v", m)
	}
	if system["download_dir_free_bytes"].(float64) != 0 {
		t.Errorf("download_dir_free_bytes = %v; want 0 on error", system["download_dir_free_bytes"])
	}
}

func TestModeStatusOverview_RequiresAuth(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.APIKey = testAPIKey })
	s := New(Options{Config: cfg, App: &statusOverviewSpyApp{}})

	rr := apiGet(t, s.Handler(), "/api?mode=status_overview")
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401 without apikey", rr.Code)
	}
}
