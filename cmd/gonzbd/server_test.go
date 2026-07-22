package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

	httpSrv, httpsSrv := newServers(cfg, handler, handler, slog.Default())

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
			if tc.srv.ErrorLog == nil {
				t.Errorf("expected ErrorLog to be set, got nil")
			}
		})
	}
}

func TestContentSecurityPolicy(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		scriptHashes []string
		wantCSP      string
	}{
		{
			name:         "no inline scripts",
			scriptHashes: nil,
			wantCSP:      "script-src 'self'; default-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self' ws: wss:; frame-ancestors 'none';",
		},
		{
			name:         "one inline script hash",
			scriptHashes: []string{"'sha256-AAAA='"},
			wantCSP:      "script-src 'self' 'sha256-AAAA='; default-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self' ws: wss:; frame-ancestors 'none';",
		},
		{
			name:         "multiple inline script hashes",
			scriptHashes: []string{"'sha256-AAAA='", "'sha256-BBBB='"},
			wantCSP:      "script-src 'self' 'sha256-AAAA=' 'sha256-BBBB='; default-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self' ws: wss:; frame-ancestors 'none';",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := contentSecurityPolicy(tc.scriptHashes)
			if got != tc.wantCSP {
				t.Errorf("contentSecurityPolicy(%v) = %q; want %q", tc.scriptHashes, got, tc.wantCSP)
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

	scriptHashes := []string{"'sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='"}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			trustedFn := func(*http.Request) bool { return true }
			router := composeRouter(apiSrv, dummyHandler, tc.isHTTPS, scriptHashes, trustedFn)
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
			wantCSP := "script-src 'self' 'sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='; default-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self' ws: wss:; frame-ancestors 'none';"
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

func TestShutdownServeMode_ConcurrentListeners(t *testing.T) {
	httpStarted := make(chan struct{})
	httpTestSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(httpStarted)
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer httpTestSrv.Close()

	httpsStarted := make(chan struct{})
	httpsTestSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(httpsStarted)
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer httpsTestSrv.Close()

	// Launch in-flight request to HTTP server.
	go func() {
		resp, err := httpTestSrv.Client().Get(httpTestSrv.URL)
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-httpStarted

	// Stagger HTTPS request launch by 50ms.
	time.Sleep(50 * time.Millisecond)
	go func() {
		resp, err := httpsTestSrv.Client().Get(httpsTestSrv.URL)
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-httpsStarted

	httpSrv := httpTestSrv.Config
	httpsSrv := httpsTestSrv.Config

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	start := time.Now()
	// Pass a 200ms timeout context. In parallel with independent contexts, both handlers drain cleanly in parallel (~100ms).
	shutdownServeMode(httpSrv, httpsSrv, nil, "", nil, logger, 200*time.Millisecond)
	elapsed := time.Since(start)

	// Verify both listeners drained cleanly with zero logged warnings or context starvation errors.
	if logOutput := logBuf.String(); strings.Contains(logOutput, "shutdown") {
		t.Errorf("shutdownServeMode logged shutdown error: %s", logOutput)
	}

	if elapsed >= 300*time.Millisecond {
		t.Errorf("shutdownServeMode took %v; want < 300ms", elapsed)
	}
}
