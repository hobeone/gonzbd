//go:build uitest

// Package uitest provides browser-based integration tests for the sabnzbd-go
// web UI using Playwright. Tests exercise the full stack: Go backend serving
// the embedded Svelte SPA, interacted with via a real Chromium browser.
//
// Run with:
//
//	go test -tags=uitest -v ./test/uitest/...
//
// Prerequisites:
//   - The UI must be pre-built: cd ui && npm run build
//   - Playwright browsers must be installed (cached in ~/.cache/ms-playwright-go/)
package uitest

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/hobeone/sabnzbd-go/internal/api"
	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/constants"
	"github.com/hobeone/sabnzbd-go/internal/queue"
	"github.com/hobeone/sabnzbd-go/internal/web"
	"github.com/hobeone/sabnzbd-go/ui"
	"github.com/playwright-community/playwright-go"
)

const testAPIKey = "uitest-api-key-1234"

// mockApp satisfies api.ApplicationReloader for UI tests.
type mockApp struct {
	q *queue.Queue
}

func (m *mockApp) ReloadDownloader(_ []config.ServerConfig) error { return nil }

func (m *mockApp) RetryHistoryJob(_ context.Context, _ string) error { return nil }

func (m *mockApp) AddJob(_ context.Context, job *queue.Job, _ []byte, _ bool) error {
	if m.q != nil {
		return m.q.Add(job)
	}
	return nil
}

func (m *mockApp) RemoveJob(id string) error {
	if m.q != nil {
		return m.q.Remove(id)
	}
	return nil
}

func (m *mockApp) RemoveHistoryJob(_ context.Context, _ string, _ bool) error {
	return nil
}

// testEnv bundles everything needed for a UI test.
type testEnv struct {
	Server  *httptest.Server
	BaseURL string
	Queue   *queue.Queue
	APISrv  *api.Server
	PW      *playwright.Playwright
	Browser playwright.Browser
}

// newTestEnv starts a test HTTP server serving both API and SPA, plus a
// Playwright browser. Call env.Close() in a t.Cleanup.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	// Verify the embedded UI dist exists.
	if _, err := fs.Stat(ui.DistFS, "dist/index.html"); err != nil {
		t.Fatal("ui/dist/index.html not found — run 'cd ui && npm run build' first")
	}

	q := queue.New()
	ma := &mockApp{q: q}

	// Provide a minimal Config so get_config doesn't return 500.
	// Initialize all slices to non-nil so JSON encodes as [] not null.
	cfg := &config.Config{
		General: config.GeneralConfig{
			Host: "0.0.0.0",
			Port: 8080,
		},
		Servers:    []config.ServerConfig{},
		Categories: []config.CategoryConfig{},
		Sorters:    []config.SorterConfig{},
		Schedules:  []config.ScheduleConfig{},
		RSS:        []config.RSSFeedConfig{},
	}

	apiSrv := api.New(api.Options{
		Auth: api.AuthConfig{
			APIKey:          testAPIKey,
			LocalhostBypass: true,
		},
		Version: "test-uitest",
		Queue:   q,
		Config:  cfg,
		App:     ma,
		Logger:  slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})

	// Build a mux combining API + SPA handlers.
	mux := http.NewServeMux()
	mux.Handle("/api", apiSrv.Handler())
	mux.Handle("/api/ws", apiSrv.Handler())
	webHandler, err := web.Handler(testAPIKey)
	if err != nil {
		t.Fatalf("web.Handler: %v", err)
	}
	mux.Handle("/", webHandler)

	ts := httptest.NewServer(mux)

	// Launch Playwright.
	pw, err := playwright.Run()
	if err != nil {
		ts.Close()
		t.Fatalf("playwright.Run: %v", err)
	}

	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(true),
	})
	if err != nil {
		_ = pw.Stop()
		ts.Close()
		t.Fatalf("chromium.Launch: %v", err)
	}

	env := &testEnv{
		Server:  ts,
		BaseURL: ts.URL,
		Queue:   q,
		APISrv:  apiSrv,
		PW:      pw,
		Browser: browser,
	}

	t.Cleanup(func() {
		_ = browser.Close()
		_ = pw.Stop()
		ts.Close()
	})

	return env
}

// newPage creates a new browser page for a test.
func (e *testEnv) newPage(t *testing.T) playwright.Page {
	t.Helper()
	page, err := e.Browser.NewPage()
	if err != nil {
		t.Fatalf("browser.NewPage: %v", err)
	}
	t.Cleanup(func() {
		_ = page.Close()
	})
	return page
}

// navigate goes to a path on the test server and waits for network idle.
func (e *testEnv) navigate(t *testing.T, page playwright.Page, path string) {
	t.Helper()
	url := fmt.Sprintf("%s%s", e.BaseURL, path)
	if _, err := page.Goto(url, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateNetworkidle,
	}); err != nil {
		t.Fatalf("page.Goto(%s): %v", url, err)
	}
}

// seedQueue adds n placeholder jobs to the queue for testing.
func (e *testEnv) seedQueue(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		job := &queue.Job{
			ID:             fmt.Sprintf("test-job-%04d", i),
			Name:           fmt.Sprintf("Test.Download.%d.x264-GROUP", i),
			Filename:       fmt.Sprintf("test_%d.nzb", i),
			Category:       "TV",
			Status:         constants.StatusQueued,
			TotalBytes:     int64((i + 1) * 100 * 1024 * 1024),
			RemainingBytes: int64((i + 1) * 50 * 1024 * 1024),
		}
		if err := e.Queue.Add(job); err != nil {
			t.Fatalf("queue.Add: %v", err)
		}
	}
}
