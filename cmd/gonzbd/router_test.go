package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/api"
	"github.com/hobeone/gonzbd/internal/config"
)

func TestComposeRouter_DebugVarsRequiresTrust(t *testing.T) {
	t.Parallel()

	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default() failed: %v", err)
	}
	apiSrv := api.New(api.Options{Config: cfg})
	webHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	untrusted := func(*http.Request) bool { return false }
	router := composeRouter(apiSrv, webHandler, false, nil, untrusted, slog.New(slog.DiscardHandler))

	req := httptest.NewRequest(http.MethodGet, "/debug/vars", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("/debug/vars returned %d for an untrusted caller; want 404", rr.Code)
	}
}

func TestComposeRouter_DebugVarsAllowedForTrustedCaller(t *testing.T) {
	t.Parallel()

	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default() failed: %v", err)
	}
	apiSrv := api.New(api.Options{Config: cfg})
	webHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	trusted := func(*http.Request) bool { return true }
	router := composeRouter(apiSrv, webHandler, false, nil, trusted, slog.New(slog.DiscardHandler))

	req := httptest.NewRequest(http.MethodGet, "/debug/vars", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("/debug/vars returned %d for a trusted caller; want 200", rr.Code)
	}
}

// TestComposeRouter_StaticAssetRequestsAreLogged proves that a request
// served by the SPA/static-asset handler (mounted at "/") produces a log
// line via the logger passed to composeRouter, since internal/web has no
// logging of its own.
func TestComposeRouter_StaticAssetRequestsAreLogged(t *testing.T) {
	t.Parallel()

	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default() failed: %v", err)
	}
	apiSrv := api.New(api.Options{Config: cfg})
	webHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	trusted := func(*http.Request) bool { return true }
	router := composeRouter(apiSrv, webHandler, false, nil, trusted, logger)

	req := httptest.NewRequest(http.MethodGet, "/index.html", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	out := buf.String()
	if !strings.Contains(out, "/index.html") {
		t.Errorf("expected a log line for the static asset request, got: %q", out)
	}
	if !strings.Contains(out, "status=200") {
		t.Errorf("expected status=200 in the log line, got: %q", out)
	}
}

// TestComposeRouter_DebugRequestsAreLogged proves /debug/ requests are
// logged too, since http.DefaultServeMux's expvar handler has no logging
// of its own either.
func TestComposeRouter_DebugRequestsAreLogged(t *testing.T) {
	t.Parallel()

	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default() failed: %v", err)
	}
	apiSrv := api.New(api.Options{Config: cfg})
	webHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	trusted := func(*http.Request) bool { return true }
	router := composeRouter(apiSrv, webHandler, false, nil, trusted, logger)

	req := httptest.NewRequest(http.MethodGet, "/debug/vars", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if !strings.Contains(buf.String(), "/debug/vars") {
		t.Errorf("expected a log line for the /debug/vars request, got: %q", buf.String())
	}
}

// TestComposeRouter_APIRequestsNotDoubleLogged proves that /api requests
// are logged exactly once, by the API server's own internal logging, and
// are not additionally logged by composeRouter's static/debug logging
// wrapper (which must only wrap the handlers that have no logging of
// their own).
func TestComposeRouter_APIRequestsNotDoubleLogged(t *testing.T) {
	t.Parallel()

	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default() failed: %v", err)
	}

	var apiBuf bytes.Buffer
	apiLogger := slog.New(slog.NewTextHandler(&apiBuf, nil))
	apiSrv := api.New(api.Options{Config: cfg, Logger: apiLogger})

	webHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	var webBuf bytes.Buffer
	webLogger := slog.New(slog.NewTextHandler(&webBuf, nil))
	trusted := func(*http.Request) bool { return true }
	router := composeRouter(apiSrv, webHandler, false, nil, trusted, webLogger)

	req := httptest.NewRequest(http.MethodGet, "/api?mode=version", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	apiLines := strings.Count(strings.TrimSpace(apiBuf.String()), "\n") + 1
	if apiBuf.Len() == 0 {
		apiLines = 0
	}
	if apiLines != 1 {
		t.Errorf("expected exactly 1 log line from the API server's own logger, got %d:\n%s", apiLines, apiBuf.String())
	}
	if webBuf.Len() != 0 {
		t.Errorf("expected no log output from composeRouter's web/debug logger for an /api request, got: %q", webBuf.String())
	}
}
