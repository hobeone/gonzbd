package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testSPAFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":   {Data: []byte(`<!DOCTYPE html><html><body>SABnzbd-Go</body></html>`)},
		"_app/test.js": {Data: []byte(`console.log("test")`)},
		"robots.txt":   {Data: []byte("User-agent: *\nDisallow:")},
		"favicon.ico":  {Data: []byte("fake-icon-bytes")},
	}
}

func TestNewSPAHandler_RootServesIndexHTML(t *testing.T) {
	handler := NewSPAHandler(testSPAFS(), "test-key")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "SABnzbd-Go") {
		t.Errorf("GET / body missing 'SABnzbd-Go'")
	}
}

func TestNewSPAHandler_StaticFileServedDirectly(t *testing.T) {
	handler := NewSPAHandler(testSPAFS(), "test-key")

	tests := []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{"/_app/test.js", http.StatusOK, `console.log`},
		{"/robots.txt", http.StatusOK, "User-agent"},
		{"/favicon.ico", http.StatusOK, "fake-icon-bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest("GET", tt.path, nil))
			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rr.Code, tt.wantStatus)
			}
			if !strings.Contains(rr.Body.String(), tt.wantBody) {
				t.Errorf("body missing %q", tt.wantBody)
			}
		})
	}
}

func TestNewSPAHandler_UnknownPathFallsBackToIndex(t *testing.T) {
	handler := NewSPAHandler(testSPAFS(), "test-key")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", "/some/deep/route", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /some/deep/route status = %d, want 200 (SPA catch-all)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "SABnzbd-Go") {
		t.Errorf("SPA catch-all body missing 'SABnzbd-Go' (should serve index.html)")
	}
}
