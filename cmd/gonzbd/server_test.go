package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/api"
	"github.com/hobeone/gonzbd/internal/config"
)

func TestNewServersTimeouts(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default() failed: %v", err)
	}

	handler := http.NotFoundHandler()

	httpSrv, httpsSrv := newServers(cfg, handler, handler)

	type serverTestCase struct {
		name string
		srv  *http.Server
	}

	testCases := []serverTestCase{
		{name: "HTTP Server", srv: httpSrv},
		{name: "HTTPS Server", srv: httpsSrv},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.srv == nil {
				t.Fatal("server is nil")
			}

			if tc.srv.ReadHeaderTimeout != 10*time.Second {
				t.Errorf("expected ReadHeaderTimeout = 10s, got %v", tc.srv.ReadHeaderTimeout)
			}
			if tc.srv.ReadTimeout != 30*time.Second {
				t.Errorf("expected ReadTimeout = 30s, got %v", tc.srv.ReadTimeout)
			}
			if tc.srv.WriteTimeout != 30*time.Second {
				t.Errorf("expected WriteTimeout = 30s, got %v", tc.srv.WriteTimeout)
			}
			if tc.srv.IdleTimeout != 120*time.Second {
				t.Errorf("expected IdleTimeout = 120s, got %v", tc.srv.IdleTimeout)
			}
		})
	}
}

func TestSecurityHeaders(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default() failed: %v", err)
	}

	apiSrv := api.New(api.Options{
		Config: cfg,
	})

	dummyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	testCases := []struct {
		name     string
		isHTTPS  bool
		wantHSTS bool
	}{
		{
			name:     "HTTP headers",
			isHTTPS:  false,
			wantHSTS: false,
		},
		{
			name:     "HTTPS headers",
			isHTTPS:  true,
			wantHSTS: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			router := composeRouter(apiSrv, dummyHandler, tc.isHTTPS)
			req := httptest.NewRequest(http.MethodGet, "/random-path", nil)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			headers := rr.Header()

			gotContentTypeOptions := headers.Get("X-Content-Type-Options")
			if gotContentTypeOptions != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q; want %q", gotContentTypeOptions, "nosniff")
			}

			gotFrameOptions := headers.Get("X-Frame-Options")
			if gotFrameOptions != "DENY" {
				t.Errorf("X-Frame-Options = %q; want %q", gotFrameOptions, "DENY")
			}

			gotReferrerPolicy := headers.Get("Referrer-Policy")
			if gotReferrerPolicy != "strict-origin-when-cross-origin" {
				t.Errorf("Referrer-Policy = %q; want %q", gotReferrerPolicy, "strict-origin-when-cross-origin")
			}

			gotCSP := headers.Get("Content-Security-Policy")
			wantCSP := "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self' ws: wss:; frame-ancestors 'none';"
			if gotCSP != wantCSP {
				t.Errorf("Content-Security-Policy = %q; want %q", gotCSP, wantCSP)
			}

			gotHSTS := headers.Get("Strict-Transport-Security")
			if tc.wantHSTS {
				wantHSTSStr := "max-age=63072000; includeSubDomains; preload"
				if gotHSTS != wantHSTSStr {
					t.Errorf("Strict-Transport-Security = %q; want %q", gotHSTS, wantHSTSStr)
				}
			} else {
				if gotHSTS != "" {
					t.Errorf("Strict-Transport-Security = %q; want empty string", gotHSTS)
				}
			}
		})
	}
}
