package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/api/apitest"
	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
)

// These call the config mode handlers directly, so each branch is pinned
// without the auth and routing layers in front of it.

func directCall(t *testing.T, handler func(http.ResponseWriter, *http.Request), target string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	handler(rr, httptest.NewRequest(http.MethodGet, target, nil))
	return rr
}

func TestModeGetConfig_SectionSelection(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.DownloadDir = "/data/incomplete" })
	s := testServerWithConfig(t, cfg)

	t.Run("misc alias returns the general section", func(t *testing.T) {
		t.Parallel()
		rr := directCall(t, s.modeGetConfig, "/api?mode=get_config&section=misc")
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "/data/incomplete") {
			t.Errorf("status = %d, body = %s; want 200 containing the download_dir", rr.Code, rr.Body.String())
		}
	})
	t.Run("unknown section is an empty object", func(t *testing.T) {
		t.Parallel()
		rr := directCall(t, s.modeGetConfig, "/api?mode=get_config&section=nope")
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"config":{}`) {
			t.Errorf("status = %d, body = %s; want 200 with an empty config object", rr.Code, rr.Body.String())
		}
	})
	t.Run("no section returns every section", func(t *testing.T) {
		t.Parallel()
		rr := directCall(t, s.modeGetConfig, "/api?mode=get_config")
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"misc"`) || !strings.Contains(rr.Body.String(), `"sorters":[]`) {
			t.Errorf("status = %d, body = %s; want 200 with misc and an empty sorters array", rr.Code, rr.Body.String())
		}
	})
	t.Run("config not wired", func(t *testing.T) {
		t.Parallel()
		rr := directCall(t, testServerWithConfig(t, nil).modeGetConfig, "/api?mode=get_config")
		if rr.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rr.Code)
		}
	})
}

func TestModeConfig_Dispatch(t *testing.T) {
	t.Parallel()
	s := testServer()
	for _, tc := range []struct {
		name   string
		action string
		want   int
	}{
		{"unknown", "bogus", http.StatusBadRequest},
		{"set_pause", "set_pause", http.StatusBadRequest},
		{"set_apikey", "set_apikey", http.StatusBadRequest},
		{"create_backup", "create_backup", http.StatusNotImplemented},
		{"speedlimit reaches its handler", "speedlimit", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rr := directCall(t, s.modeConfig, "/api?mode=config&name="+tc.action+"&value=0")
			if rr.Code != tc.want {
				t.Errorf("status = %d, want %d (body: %s)", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

func TestConfigSpeedLimit_Conversions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		value string
		want  int64
		code  int
	}{
		{"plain number is KiB/s", "500", 500 * 1024, http.StatusOK},
		{"suffixed value is a byte rate", "1M", 1 << 20, http.StatusOK},
		{"empty disables the limit", "", 0, http.StatusOK},
		{"garbage is rejected", "fast", 0, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Default()
			if err != nil {
				t.Fatalf("Default(): %v", err)
			}
			spy := &setConfigSpyApp{}
			s := testServerWithConfig(t, cfg)
			s.setAppServices(spy)

			rr := directCall(t, s.configSpeedLimit, "/api?mode=config&name=speedlimit&value="+tc.value)

			if rr.Code != tc.code {
				t.Fatalf("status = %d, want %d (body: %s)", rr.Code, tc.code, rr.Body.String())
			}
			spy.mu.Lock()
			got := spy.speedLimit
			spy.mu.Unlock()
			if got != tc.want {
				t.Errorf("speed limit applied = %d, want %d", got, tc.want)
			}
		})
	}

	t.Run("application not running", func(t *testing.T) {
		t.Parallel()
		s := New(Options{Config: &config.Config{}})
		rr := directCall(t, s.configSpeedLimit, "/api?mode=config&name=speedlimit&value=1")
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rr.Code)
		}
	})
}

type nntpProbeApp struct {
	apitest.NopApp
	mu  sync.Mutex
	got config.ServerConfig
}

func (a *nntpProbeApp) TestNNTPServer(_ context.Context, cfg config.ServerConfig) (app.NNTPTestResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.got = cfg
	return app.NNTPTestResult{}, nil
}

func TestConfigTestServer_RequestParameters(t *testing.T) {
	t.Parallel()

	t.Run("missing host", func(t *testing.T) {
		t.Parallel()
		s := testServer()
		rr := directCall(t, s.configTestServer, "/api?mode=config&name=test_server")
		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rr.Code)
		}
	})
	t.Run("ssl moves the default port to 563", func(t *testing.T) {
		t.Parallel()
		probe := &nntpProbeApp{}
		s := testServer()
		s.setAppServices(probe)
		rr := directCall(t, s.configTestServer, "/api?mode=config&name=test_server&host=news.example.com&ssl=1")
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"passed":true`) {
			t.Fatalf("status = %d, body = %s; want a passed result", rr.Code, rr.Body.String())
		}
		probe.mu.Lock()
		defer probe.mu.Unlock()
		if probe.got.Port != 563 || !probe.got.SSL || probe.got.Host != "news.example.com" {
			t.Errorf("probed %+v, want host news.example.com, port 563, ssl on", probe.got)
		}
	})
	t.Run("plain default port is 119", func(t *testing.T) {
		t.Parallel()
		probe := &nntpProbeApp{}
		s := testServer()
		s.setAppServices(probe)
		directCall(t, s.configTestServer, "/api?mode=config&name=test_server&host=news.example.com")
		probe.mu.Lock()
		defer probe.mu.Unlock()
		if probe.got.Port != 119 {
			t.Errorf("probed port = %d, want 119", probe.got.Port)
		}
	})
	t.Run("out of range ssl_verify is rejected", func(t *testing.T) {
		t.Parallel()
		s := testServer()
		rr := directCall(t, s.configTestServer, "/api?mode=config&name=test_server&host=h&ssl_verify=99")
		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (body: %s)", rr.Code, rr.Body.String())
		}
	})
}
