//go:build uitest

package uitest

import (
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/api/apitest"

	"github.com/mxschmitt/playwright-go"

	"github.com/hobeone/gonzbd/internal/api"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/web"
	"github.com/hobeone/gonzbd/ui"
)

// ---------------------------------------------------------------------------
// Status Page
// ---------------------------------------------------------------------------

// newTestEnvWithServer mirrors newTestEnv but wires a NopApp that reports one
// configured News Server via ServerStatus(), and a matching config.Servers
// entry. The shared newTestEnv (harness_test.go) hardcodes an empty-server
// apitest.NopApp{Queue: q}, which can never report a configured server — its
// ServerStatus() stub always returned nil regardless of config. This helper
// exists so tests that need a real, named server to appear in API/telemetry
// responses (e.g. the News Servers row in TestStatusPageLoadsAndShowsGeneralInfo)
// have one to assert against, distinct from the ~20 other uitest tests that
// depend on newTestEnv's fixed empty-server shape.
func newTestEnvWithServer(t *testing.T) *testEnv {
	t.Helper()

	if _, err := fs.Stat(ui.DistFS, "dist/index.html"); err != nil {
		t.Fatal("ui/dist/index.html not found — run 'cd ui && bun run build' first")
	}

	d := newTestDispatcher(t)
	ma := apitest.NopApp{
		Dispatcher: d,
		ServerSnapshotsVal: []downloader.ServerSnapshot{
			{
				Name:           "test-server",
				Host:           "news.example.test",
				Port:           119,
				MaxConnections: 10,
				ActiveConns:    3,
				Enabled:        true,
				Active:         true,
			},
		},
	}

	histDB, err := history.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("history.Open(:memory:): %v", err)
	}
	histR := history.NewRepository(histDB)

	cfg := &config.Config{
		General: config.GeneralConfig{
			Host:   "0.0.0.0",
			Port:   4289,
			APIKey: testAPIKey,
		},
		Servers: []config.ServerConfig{
			{
				Name:        "test-server",
				Host:        "news.example.test",
				Port:        119,
				Connections: 10,
				Enable:      true,
			},
		},
		Categories: []config.CategoryConfig{},
	}

	apiSrv := api.New(api.Options{
		Version:    "test-uitest",
		Dispatcher: d,
		Config:     cfg,
		App:        ma,
		History:    histR,
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})

	mux := http.NewServeMux()
	mux.Handle("/api", apiSrv.Handler())
	mux.Handle("/api/ws", apiSrv.Handler())
	webHandler, err := web.Handler(apiSrv.SessionKey, func(*http.Request) bool { return true })
	if err != nil {
		t.Fatalf("web.Handler: %v", err)
	}
	mux.Handle("/", webHandler)

	ts := httptest.NewServer(mux)

	pw, err := playwright.Run()
	if err != nil {
		ts.Close()
		t.Fatalf("playwright.Run: %v", err)
	}

	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: new(true),
	})
	if err != nil {
		_ = pw.Stop()
		ts.Close()
		t.Fatalf("chromium.Launch: %v", err)
	}

	env := &testEnv{
		Server:     ts,
		BaseURL:    ts.URL,
		Dispatcher: d,
		APISrv:     apiSrv,
		HistDB:     histDB,
		HistR:      histR,
		PW:         pw,
		Browser:    browser,
	}

	t.Cleanup(func() {
		_ = browser.Close()
		_ = pw.Stop()
		ts.Close()
		_ = histDB.Close()
	})

	return env
}

// TestStatusPageLoadsAndShowsGeneralInfo verifies that navigating directly to
// /status (not via a click-through from the dashboard) renders the page's
// core sections, including a populated News Servers row. This is a
// regression guard for a defect fixed by the News Servers work: a direct
// navigation didn't start the WebSocket telemetry subscription, so the News
// Servers section rendered empty.
//
// The assertion below targets the configured server's name as it appears in
// a rendered News Servers row (populated only via the WebSocket telemetry
// event's Servers field — see internal/api/events.go and
// ui/src/lib/stores/telemetry.svelte.ts's getServerStats()), not the static
// "News Servers" section heading. The heading renders unconditionally
// whenever mode=status_overview succeeds, regardless of whether telemetry
// ever started, so asserting on it alone would not catch a regression where
// startTelemetry()/stopTelemetry() were removed from the page's
// onMount/onDestroy.
func TestStatusPageLoadsAndShowsGeneralInfo(t *testing.T) {
	t.Parallel()
	env := newTestEnvWithServer(t)
	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/status")

	heading := page.GetByText("Status", playwright.PageGetByTextOptions{Exact: new(true)})
	if err := heading.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("Status page heading not visible: %v", err)
	}

	generalInfo := page.GetByText("General Info")
	if err := generalInfo.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Errorf("General Info section not visible: %v", err)
	}

	systemInfo := page.GetByText("System Info")
	if err := systemInfo.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Errorf("System Info section not visible: %v", err)
	}

	// This is the specific scenario Task 9 fixed: on a direct navigation to
	// /status, the WebSocket telemetry subscription must be started so the
	// News Servers section is populated with a real server row rather than
	// left empty.
	//
	// This uitest harness wires a bare api.Server directly against a
	// NopApp — it never runs internal/app.Application's runMetricsPush
	// ticker, which in production is what periodically broadcasts a
	// "metrics" WS event carrying Servers (built from
	// Application.downloaderStats.ServerStatus()). So NopApp's
	// ServerSnapshotsVal alone is never surfaced to the page in this test
	// environment. To exercise the same code path the ticker would, this
	// test broadcasts that "metrics" event itself, on a short retry loop,
	// through env.APISrv.EventBroadcaster() — the same broadcaster the real
	// app would use. If telemetry's WS subscription is not wired (e.g.
	// onMount's startTelemetry() is removed), subscribeWS() is never called
	// from the page and no client connects to receive this broadcast, so
	// the row never appears and this loop times out.
	serverSnapshot := []downloader.ServerSnapshot{
		{
			Name:           "test-server",
			Host:           "news.example.test",
			Port:           119,
			MaxConnections: 10,
			ActiveConns:    3,
			Enabled:        true,
			Active:         true,
		},
	}

	serverRow := page.GetByText("test-server (news.example.test:119)")
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		env.APISrv.EventBroadcaster().Broadcast(api.Event{
			Type:    "metrics",
			Servers: serverSnapshot,
		})
		lastErr = serverRow.WaitFor(playwright.LocatorWaitForOptions{
			State:   playwright.WaitForSelectorStateVisible,
			Timeout: playwright.Float(200),
		})
		if lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		t.Errorf("News Servers row for test-server not visible after direct navigation: %v", lastErr)
	}
}
