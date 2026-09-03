//go:build uitest

// Package uitest provides browser-based integration tests for the gonzbd
// web UI using Playwright. Tests exercise the full stack: Go backend serving
// the embedded Svelte SPA, interacted with via a real Chromium browser.
//
// Run with:
//
//	go test -tags=uitest -v ./test/uitest/...
//
// Prerequisites:
//   - The UI must be pre-built: cd ui && bun run build
//   - Playwright browsers must be installed (cached in ~/.cache/ms-playwright-go/)
package uitest

import (
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/api/apitest"

	"github.com/mxschmitt/playwright-go"

	"github.com/hobeone/gonzbd/internal/api"
	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
	"github.com/hobeone/gonzbd/internal/web"
	"github.com/hobeone/gonzbd/ui"
)

const testAPIKey = "uitest-api-key-1234"

// testEnv bundles everything needed for a UI test.
type testEnv struct {
	Server     *httptest.Server
	BaseURL    string
	Dispatcher *dispatch.Dispatcher
	APISrv     *api.Server
	HistDB     *history.DB
	HistR      *history.Repository
	PW         *playwright.Playwright
	Browser    playwright.Browser
}

type uiStubWorkers struct{}

func (uiStubWorkers) Abort(string) {}

type uiStubResidency struct{}

func (uiStubResidency) Hydrate(context.Context, string) error { return nil }
func (uiStubResidency) Evict(string)                          {}

type uiStubStore struct{}

func (uiStubStore) Load(context.Context) ([]dispatch.Persisted, error) { return nil, nil }
func (uiStubStore) Save(context.Context, dispatch.Persisted) error     { return nil }
func (uiStubStore) Delete(context.Context, string) error               { return nil }

type uiStubRunner struct{}

func (uiStubRunner) Run(context.Context, string, job.State) {}

func newTestDispatcher(t *testing.T) *dispatch.Dispatcher {
	t.Helper()
	d := dispatch.New(10, 10, time.Hour, time.Now, uiStubWorkers{}, uiStubResidency{}, uiStubStore{}, uiStubRunner{})
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("dispatcher.Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })
	return d
}

// newTestEnv starts a test HTTP server serving both API and SPA, plus a
// Playwright browser. Call env.Close() in a t.Cleanup.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	// Verify the embedded UI dist exists.
	if _, err := fs.Stat(ui.DistFS, "dist/index.html"); err != nil {
		t.Fatal("ui/dist/index.html not found — run 'cd ui && bun run build' first")
	}

	d := newTestDispatcher(t)
	ma := apitest.NopApp{Dispatcher: d}

	// In-memory history database.
	histDB, err := history.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("history.Open(:memory:): %v", err)
	}
	histR := history.NewRepository(histDB)

	// Provide a minimal Config so get_config doesn't return 500.
	// Initialize all slices to non-nil so JSON encodes as [] not null.
	cfg := &config.Config{
		General: config.GeneralConfig{
			Host:   "0.0.0.0",
			Port:   4289,
			APIKey: testAPIKey,
		},
		Servers:    []config.ServerConfig{},
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

	// Build a mux combining API + SPA handlers.
	mux := http.NewServeMux()
	mux.Handle("/api", apiSrv.Handler())
	mux.Handle("/api/ws", apiSrv.Handler())
	// Mirror production wiring: the SPA cookie carries the API server's
	// ephemeral session key, not General.APIKey.
	webHandler, err := web.Handler(apiSrv.SessionKey, func(*http.Request) bool { return true })
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

// newPage creates a new browser page for a test.
func (e *testEnv) newPage(t *testing.T) playwright.Page { //nolint:ireturn // playwright.Page is the interface Browser.NewPage itself returns; there is no concrete type to return instead
	t.Helper()
	page, err := e.Browser.NewPage()
	if err != nil {
		t.Fatalf("browser.NewPage: %v", err)
	}
	var logs []string
	page.OnConsole(func(msg playwright.ConsoleMessage) {
		logs = append(logs, fmt.Sprintf("%s: %s", msg.Type(), msg.Text()))
	})
	t.Cleanup(func() {
		if t.Failed() && len(logs) > 0 {
			t.Logf("--- Browser Console Logs ---")
			for _, log := range logs {
				t.Logf("[BROWSER] %s", log)
			}
			t.Logf("----------------------------")
		}
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
		// Two equal-sized articles totaling TotalBytes; marking the first
		// done leaves RemainingBytes at exactly half, matching the
		// half-downloaded fixture the UI tests expect.
		half := int64((i + 1) * 50 * 1024 * 1024)
		parsed := &nzb.NZB{Files: []nzb.File{
			{Subject: "file.bin", Bytes: half * 2, Articles: []nzb.Article{
				{ID: fmt.Sprintf("test-job-%04d-a@t", i), Bytes: int(half), Number: 1},
				{ID: fmt.Sprintf("test-job-%04d-b@t", i), Bytes: int(half), Number: 2},
			}},
		}}
		id := fmt.Sprintf("test-job-%04d", i)
		name := fmt.Sprintf("Test.Download.%d.x264-GROUP", i)
		filename := fmt.Sprintf("test_%d.nzb", i)
		j, hdr, err := app.BuildIngestJob(nil, parsed, filename, types.FetchOptions{
			JobID:    id,
			NzbName:  name,
			Category: "TV",
		}, slog.Default())
		if err != nil {
			t.Fatalf("BuildIngestJob: %v", err)
		}
		if err := e.Dispatcher.Add(j, hdr); err != nil {
			t.Fatalf("Dispatcher.Add: %v", err)
		}
		ackDone(t, e.Dispatcher, id, fmt.Sprintf("test-job-%04d-a@t", i))
	}
}

// seedHistory adds n history entries with the given status.
func (e *testEnv) seedHistory(t *testing.T, n int, status string) {
	t.Helper()
	for i := range n {
		entry := history.Entry{
			NzoID:        fmt.Sprintf("hist-%04d", i),
			Name:         fmt.Sprintf("History.Item.%d.x264-GROUP", i),
			NzbName:      fmt.Sprintf("hist_%d.nzb", i),
			Category:     "Movies",
			Status:       status,
			Bytes:        int64((i + 1) * 200 * 1024 * 1024),
			DownloadTime: 120,
			Completed:    time.Now().Add(-time.Duration(i) * time.Hour),
		}
		if status == "Failed" {
			entry.FailMessage = fmt.Sprintf("Unpacking failed for item %d", i)
		}
		if err := e.HistR.Add(t.Context(), entry); err != nil {
			t.Fatalf("history.Add: %v", err)
		}
	}
}

// seedHistoryWithPrefix adds n history entries with a custom ID/name prefix
// to avoid NzoID collisions when seeding both completed and failed entries
// in the same test.
func (e *testEnv) seedHistoryWithPrefix(t *testing.T, n int, status, prefix string) {
	t.Helper()
	for i := range n {
		entry := history.Entry{
			NzoID:        fmt.Sprintf("%s-%04d", prefix, i),
			Name:         fmt.Sprintf("%s.History.%d.x264-GROUP", prefix, i),
			NzbName:      fmt.Sprintf("%s_%d.nzb", prefix, i),
			Category:     "Movies",
			Status:       status,
			Bytes:        int64((i + 1) * 200 * 1024 * 1024),
			DownloadTime: 120,
			Completed:    time.Now().Add(-time.Duration(i) * time.Hour),
		}
		if status == "Failed" {
			entry.FailMessage = fmt.Sprintf("Unpacking failed for %s item %d", prefix, i)
		}
		if err := e.HistR.Add(t.Context(), entry); err != nil {
			t.Fatalf("history.Add: %v", err)
		}
	}
}

// screenshotOnFailure registers a cleanup that captures a full-page screenshot
// and logs page HTML content when the test fails.
func screenshotOnFailure(t *testing.T, page playwright.Page) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			_ = os.MkdirAll("screenshots", 0o755)
			path := fmt.Sprintf("screenshots/%s.png", t.Name())
			if _, err := page.Screenshot(playwright.PageScreenshotOptions{
				Path:     new(path),
				FullPage: new(true),
			}); err != nil {
				t.Logf("screenshot failed: %v", err)
			} else {
				t.Logf("Screenshot saved: %s", path)
			}
			if content, err := page.Content(); err == nil {
				t.Logf("Page HTML on failure:\n%s", content)
			}
		}
	})
}

// createTestNZBFile writes a minimal valid NZB file to t.TempDir() and returns its absolute path.
func createTestNZBFile(t *testing.T, filename, fileSubject string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, filename)
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE nzb PUBLIC "-//newzBin//DTD NZB 1.1//EN" "http://www.newzbin.com/DTD/nzb-1.1.dtd">
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
 <file poster="poster@example.com" date="1600000000" subject="%s">
  <groups>
   <group>alt.binaries.test</group>
  </groups>
  <segments>
   <segment bytes="1024" number="1">art-%s-1@example.com</segment>
  </segments>
 </file>
</nzb>`, fileSubject, filename)

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile NZB: %v", err)
	}
	return path
}
