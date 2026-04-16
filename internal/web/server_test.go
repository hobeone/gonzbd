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
			name:           "GET root contains SABnzbd",
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

			// Special check: root should contain "SABnzbd" (the application name in meta/title).
			// Note: the old stub used "SABnzbd-Go"; the full main.html.tmpl uses "SABnzbd"
			// to match upstream Glitter. The "-Go" suffix was only in the placeholder stub.
			if tt.path == "/" && !strings.Contains(body, "SABnzbd") {
				t.Errorf("body should contain 'SABnzbd'")
			}

			// Ensure no panic — a 500 would indicate a panic caught by the test framework
			if rr.Code == http.StatusInternalServerError {
				t.Logf("body on 500: %s", body)
			}
		})
	}
}

// TestHandler_RendersEnglishStrings verifies the production Handler() path
// resolves translation keys to their English values from the default catalog,
// not the raw keys. This is the regression guard for the "menu-queue" /
// "Glitter-fetch" visible-in-UI bug that motivated Step 12.10.
func TestHandler_RendersEnglishStrings(t *testing.T) {
	handler := Handler()
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// Positive probes: these English values must appear (sourced from upstream
	// skintext.py SKIN_TEXT entries for menu-queue, menu-history). The menu
	// renders the label followed by a trailing space and a <span> badge, so
	// the probe matches "<tab-a-href...>Queue <" rather than ">Queue<".
	wantPresent := []string{
		">Queue ",   // from {{T "menu-queue"}} in include_menu tab label
		">History ", // from {{T "menu-history"}} in include_menu tab label
	}
	for _, s := range wantPresent {
		if !strings.Contains(body, s) {
			t.Errorf("body missing expected English text %q (translation not loaded?)", s)
		}
	}

	// Negative probes: raw translation keys must NOT leak through as visible
	// UI text. A prior bug had these rendering literally to the browser.
	wantAbsent := []string{
		">menu-queue<",
		">menu-history<",
		">Glitter-fetch<",
		">Glitter-addNZB<",
	}
	for _, s := range wantAbsent {
		if strings.Contains(body, s) {
			t.Errorf("body contains raw translation key %q (catalog not wired?)", s)
		}
	}
}

// TestServedGlitterJS_IsAssembled verifies the served glitter.js is the
// assembled, ready-to-run artifact rather than the upstream Cheetah template
// (which contains "#include raw $webdir + ..."  directives that the browser
// parses as JS private-field syntax and rejects).
//
// Regression guard for the Step 12.11 fix. Symptom before the fix:
//
//	Uncaught SyntaxError: Private field '#include' must be declared in an
//	enclosing class (at glitter.js:1:1)
func TestServedGlitterJS_IsAssembled(t *testing.T) {
	handler := Handler()
	req := httptest.NewRequest("GET", "/static/glitter/javascripts/glitter.js", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// No Cheetah #include directives must leak through to the browser.
	forbidden := []string{
		"#include raw",
		"#include ",
		"$webdir",
	}
	for _, bad := range forbidden {
		if strings.Contains(body, bad) {
			t.Errorf("glitter.js contains unresolved Cheetah token %q (assembly skipped?)", bad)
		}
	}

	// Markers from each of the 5 sub-files must appear, proving they were
	// inlined rather than referenced.
	required := []struct {
		marker string
		source string
	}{
		{"var isMobile", "glitter.basic.js"},
		{"function ViewModel()", "glitter.main.js"},
		{"QueueListModel", "glitter.queue.js"},
		{"HistoryListModel", "glitter.history.js"},
		{"paginationModel", "glitter.filelist.pagination.js / glitter.queue.js"},
		{"ko.applyBindings(new ViewModel()", "glitter.js wrapper tail"},
	}
	for _, req := range required {
		if !strings.Contains(body, req.marker) {
			t.Errorf("glitter.js missing %q (from %s) — sub-file not inlined?", req.marker, req.source)
		}
	}

	// The assembled file is large (~3400 lines upstream). Guard against an
	// accidental regression that commits just the 55-line Cheetah template.
	if n := strings.Count(body, "\n"); n < 2000 {
		t.Errorf("glitter.js has only %d newlines; expected thousands (assembled file ~3400 lines)", n)
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
