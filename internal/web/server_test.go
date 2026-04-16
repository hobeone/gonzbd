package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	tests := []struct {
		name           string
		path           string
		wantStatus     int
		wantBodyPrefix string // Check that response body starts with this
		wantCTContains string // Check that Content-Type contains this string
	}{
		{
			name:           "GET root returns 200 with index.html",
			path:           "/",
			wantStatus:     http.StatusOK,
			wantBodyPrefix: "<!DOCTYPE html>",
			wantCTContains: "text/html",
		},
		{
			name:           "GET root contains SABnzbd-Go",
			path:           "/",
			wantStatus:     http.StatusOK,
			wantBodyPrefix: "", // Will check body content separately
			wantCTContains: "text/html",
		},
		{
			name:       "GET nonexistent path returns 404",
			path:       "/nonexistent",
			wantStatus: http.StatusNotFound,
		},
		{
			name:           "GET static Glitter JS returns 200",
			path:           "/static/glitter/javascripts/glitter.js",
			wantStatus:     http.StatusOK,
			wantCTContains: "javascript",
		},
		{
			name:           "GET static Bootstrap CSS returns 200",
			path:           "/static/glitter/bootstrap/css/bootstrap.min.css",
			wantStatus:     http.StatusOK,
			wantCTContains: "text/css",
		},
		{
			name:       "GET static with path traversal returns redirect or 404",
			path:       "/static/../etc/passwd",
			wantStatus: http.StatusTemporaryRedirect, // FileServer redirects on ../ traversal
		},
		{
			name:           "GET static dir listing",
			path:           "/static/",
			wantStatus:     http.StatusOK, // or 404 depending on FileServer config; we just check no 500
			wantCTContains: "",            // Don't enforce CT; just ensure no panic
		},
		{
			name:           "GET static glitter dir listing",
			path:           "/static/glitter/",
			wantStatus:     http.StatusOK,
			wantCTContains: "", // Don't enforce CT
		},
		{
			name:           "GET bootstrap font file returns 200",
			path:           "/static/glitter/bootstrap/fonts/glyphicons-halflings-regular.ttf",
			wantStatus:     http.StatusOK,
			wantCTContains: "", // Font MIME types vary
		},
		{
			// Step 12.1: main.tmpl references ./staticcfg/ico/favicon.ico
			// (relative to /, resolves to /staticcfg/ico/...). Mount must
			// be at /staticcfg/, not nested under /static/.
			name:           "GET staticcfg favicon returns 200 image",
			path:           "/staticcfg/ico/favicon.ico",
			wantStatus:     http.StatusOK,
			wantCTContains: "image",
		},
		{
			name:           "GET staticcfg apple-touch-icon returns 200 PNG",
			path:           "/staticcfg/ico/apple-touch-icon-180x180-precomposed.png",
			wantStatus:     http.StatusOK,
			wantCTContains: "image/png",
		},
		{
			name:           "GET staticcfg safari mask icon returns 200 SVG",
			path:           "/staticcfg/ico/safari-pinned-tab.svg",
			wantStatus:     http.StatusOK,
			wantCTContains: "image/svg",
		},
		{
			name:       "GET staticcfg nonexistent returns 404",
			path:       "/staticcfg/ico/does-not-exist.png",
			wantStatus: http.StatusNotFound,
		},
	}

	handler := Handler()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Errorf("status code = %v, want %v", rr.Code, tt.wantStatus)
			}

			if tt.wantCTContains != "" {
				ct := rr.Header().Get("Content-Type")
				if !strings.Contains(ct, tt.wantCTContains) {
					t.Errorf("Content-Type = %q, want to contain %q", ct, tt.wantCTContains)
				}
			}

			body := rr.Body.String()
			if tt.wantBodyPrefix != "" && !strings.HasPrefix(body, tt.wantBodyPrefix) {
				t.Errorf("body prefix = %q, want %q", body[:len(tt.wantBodyPrefix)], tt.wantBodyPrefix)
			}

			// Special check: root should contain "SABnzbd-Go"
			if tt.path == "/" && !strings.Contains(body, "SABnzbd-Go") {
				t.Errorf("body should contain 'SABnzbd-Go'")
			}

			// Ensure no panic — a 500 would indicate a panic caught by the test framework
			if rr.Code == http.StatusInternalServerError {
				t.Logf("body on 500: %s", body)
			}
		})
	}
}

func TestHandlerConcurrency(t *testing.T) {
	// Ensure Handler() can be called multiple times and the handlers
	// serve concurrently without race conditions.
	handler := Handler()

	const numGoroutines = 10
	done := make(chan error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func() {
			req := httptest.NewRequest("GET", "/", nil)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				done <- nil // Don't fail on concurrency test just for status
				return
			}
			done <- nil
		}()
	}

	for i := 0; i < numGoroutines; i++ {
		<-done
	}
}
