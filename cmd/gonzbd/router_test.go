package main

import (
	"net/http"
	"net/http/httptest"
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
	router := composeRouter(apiSrv, webHandler, false, nil, untrusted)

	req := httptest.NewRequest(http.MethodGet, "/debug/vars", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code == http.StatusOK {
		t.Fatalf("/debug/vars returned 200 for an untrusted caller; want non-200")
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
	router := composeRouter(apiSrv, webHandler, false, nil, trusted)

	req := httptest.NewRequest(http.MethodGet, "/debug/vars", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("/debug/vars returned %d for a trusted caller; want 200", rr.Code)
	}
}
