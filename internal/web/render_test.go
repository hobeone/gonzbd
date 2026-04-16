package web

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/i18n"
)

// TestBuildRenderContext_DerivedFields verifies all fields that BuildRenderContext
// derives from Config inputs.
func TestBuildRenderContext_DerivedFields(t *testing.T) {
	tests := []struct {
		name        string
		general     config.GeneralConfig
		rss         []config.RSSFeedConfig
		version     string
		wantAPIKey  string
		wantNZBKey  string
		wantVersion string
		wantLang    string
		wantRTL     bool
		wantLogout  bool
		wantRSS     bool
		wantWatched bool
	}{
		{
			name: "empty config defaults",
			general: config.GeneralConfig{
				APIKey:   "",
				NZBKey:   "",
				Language: "",
			},
			version:     "4.0.0",
			wantAPIKey:  "",
			wantNZBKey:  "",
			wantVersion: "4.0.0",
			wantLang:    "en",
			wantRTL:     false,
			wantLogout:  false,
			wantRSS:     false,
			wantWatched: false,
		},
		{
			name: "language en_US normalizes to en-us",
			general: config.GeneralConfig{
				Language: "en_US",
			},
			version:  "1.0.0",
			wantLang: "en-us",
			wantRTL:  false,
		},
		{
			name: "language he sets RTL true",
			general: config.GeneralConfig{
				Language: "he",
			},
			version:  "1.0.0",
			wantLang: "he",
			wantRTL:  true,
		},
		{
			name: "language ar sets RTL true",
			general: config.GeneralConfig{
				Language: "ar",
			},
			version:  "1.0.0",
			wantLang: "ar",
			wantRTL:  true,
		},
		{
			name: "language fa sets RTL true",
			general: config.GeneralConfig{
				Language: "fa",
			},
			version:  "1.0.0",
			wantLang: "fa",
			wantRTL:  true,
		},
		{
			name: "language with uppercase normalizes to lowercase",
			general: config.GeneralConfig{
				Language: "DE",
			},
			version:  "1.0.0",
			wantLang: "de",
			wantRTL:  false,
		},
		{
			name: "mixed case underscore language",
			general: config.GeneralConfig{
				Language: "zh_CN",
			},
			version:  "1.0.0",
			wantLang: "zh-cn",
			wantRTL:  false,
		},
		{
			name: "have_logout when username set",
			general: config.GeneralConfig{
				Username: "admin",
				Language: "en",
			},
			version:    "1.0.0",
			wantLang:   "en",
			wantLogout: true,
		},
		{
			name: "no logout when username empty",
			general: config.GeneralConfig{
				Username: "",
				Language: "en",
			},
			version:    "1.0.0",
			wantLang:   "en",
			wantLogout: false,
		},
		{
			name: "have_rss_defined when RSS feeds present",
			general: config.GeneralConfig{
				Language: "en",
			},
			rss:      []config.RSSFeedConfig{{Name: "feed1"}},
			version:  "1.0.0",
			wantLang: "en",
			wantRSS:  true,
		},
		{
			name: "no rss defined when RSS feeds empty",
			general: config.GeneralConfig{
				Language: "en",
			},
			rss:      nil,
			version:  "1.0.0",
			wantLang: "en",
			wantRSS:  false,
		},
		{
			name: "have_watched_dir when DirscanDir set",
			general: config.GeneralConfig{
				Language:   "en",
				DirscanDir: "/tmp/watch",
			},
			version:     "1.0.0",
			wantLang:    "en",
			wantWatched: true,
		},
		{
			name: "no watched dir when DirscanDir empty",
			general: config.GeneralConfig{
				Language:   "en",
				DirscanDir: "",
			},
			version:     "1.0.0",
			wantLang:    "en",
			wantWatched: false,
		},
		{
			name: "api and nzb keys passed through",
			general: config.GeneralConfig{
				APIKey:   "abc123def456",
				NZBKey:   "xyz789uvw012",
				Language: "en",
			},
			version:     "2.5.1",
			wantAPIKey:  "abc123def456",
			wantNZBKey:  "xyz789uvw012",
			wantVersion: "2.5.1",
			wantLang:    "en",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.With(func(c *config.Config) {
				c.General = tt.general
				c.RSS = tt.rss
			})

			ctx := BuildRenderContext(cfg, tt.version)

			if tt.wantAPIKey != "" && ctx.APIKey != tt.wantAPIKey {
				t.Errorf("APIKey = %q, want %q", ctx.APIKey, tt.wantAPIKey)
			}
			if tt.wantNZBKey != "" && ctx.NZBKey != tt.wantNZBKey {
				t.Errorf("NZBKey = %q, want %q", ctx.NZBKey, tt.wantNZBKey)
			}
			if tt.wantVersion != "" && ctx.Version != tt.wantVersion {
				t.Errorf("Version = %q, want %q", ctx.Version, tt.wantVersion)
			}
			if tt.wantLang != "" && ctx.ActiveLang != tt.wantLang {
				t.Errorf("ActiveLang = %q, want %q", ctx.ActiveLang, tt.wantLang)
			}
			if ctx.RTL != tt.wantRTL {
				t.Errorf("RTL = %v, want %v", ctx.RTL, tt.wantRTL)
			}
			if ctx.HaveLogout != tt.wantLogout {
				t.Errorf("HaveLogout = %v, want %v", ctx.HaveLogout, tt.wantLogout)
			}
			if ctx.HaveRSSDefined != tt.wantRSS {
				t.Errorf("HaveRSSDefined = %v, want %v", ctx.HaveRSSDefined, tt.wantRSS)
			}
			if ctx.HaveWatchedDir != tt.wantWatched {
				t.Errorf("HaveWatchedDir = %v, want %v", ctx.HaveWatchedDir, tt.wantWatched)
			}
		})
	}
}

// TestBuildRenderContext_StaticFields verifies fields that don't vary by config input.
func TestBuildRenderContext_StaticFields(t *testing.T) {
	cfg := &config.Config{}
	ctx := BuildRenderContext(cfg, "1.0.0")

	// Webdir must be the URL path (not a filesystem path) for embedded assets.
	if ctx.Webdir != "/static/glitter" {
		t.Errorf("Webdir = %q, want %q", ctx.Webdir, "/static/glitter")
	}

	// ColorScheme defaults to empty string (template treats as "Light").
	if ctx.ColorScheme != "" {
		t.Errorf("ColorScheme = %q, want empty string", ctx.ColorScheme)
	}

	// NewRelease and NewRelURL are empty placeholders for v1.
	if ctx.NewRelease != "" {
		t.Errorf("NewRelease = %q, want empty string", ctx.NewRelease)
	}
	if ctx.NewRelURL != "" {
		t.Errorf("NewRelURL = %q, want empty string", ctx.NewRelURL)
	}

	// HaveQuota is false in v1 (no quota config surface yet).
	if ctx.HaveQuota {
		t.Errorf("HaveQuota = true, want false in v1")
	}

	// Pid must be > 0 (the actual process ID).
	if ctx.Pid <= 0 {
		t.Errorf("Pid = %d, want > 0", ctx.Pid)
	}
}

// TestBuildRenderContext_BytesPerSecList verifies the slice marshals to []
// rather than null when empty. This matters because the template uses
// {{.BytesPerSecList}} and callers expect JSON-serializable output.
func TestBuildRenderContext_BytesPerSecList(t *testing.T) {
	cfg := &config.Config{}
	ctx := BuildRenderContext(cfg, "1.0.0")

	// Slice must be non-nil so JSON encodes to [] not null.
	if ctx.BytesPerSecList == nil {
		t.Error("BytesPerSecList is nil, want non-nil empty slice")
	}

	data, err := json.Marshal(ctx.BytesPerSecList)
	if err != nil {
		t.Fatalf("json.Marshal(BytesPerSecList): %v", err)
	}
	if string(data) != "[]" {
		t.Errorf("json.Marshal(BytesPerSecList) = %q, want %q", string(data), "[]")
	}
}

// TestHandlerWithContext_Renders verifies the / route renders main.html.tmpl
// with Version and APIKey injected into the HTML body.
func TestHandlerWithContext_Renders(t *testing.T) {
	rc := RenderContext{
		Version:         "3.7.0",
		APIKey:          "testkey123",
		ActiveLang:      "en",
		Webdir:          "/static/glitter",
		BytesPerSecList: []float64{},
	}

	handler := HandlerWithContext(rc)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	body := rr.Body.String()

	if !strings.Contains(body, "3.7.0") {
		t.Errorf("body does not contain version 3.7.0; got:\n%s", body)
	}

	if !strings.Contains(body, "testkey123") {
		t.Errorf("body does not contain apiKey testkey123; got:\n%s", body)
	}
}

// TestHandlerWithContext_JSEscaping verifies that APIKey is safely JS-escaped
// in the rendered page via the {{.APIKey | js}} template call.
func TestHandlerWithContext_JSEscaping(t *testing.T) {
	// A key containing characters that need JS escaping (angle brackets, quotes).
	rc := RenderContext{
		Version:         "1.0.0",
		APIKey:          `abc"def<ghi>jkl`,
		ActiveLang:      "en",
		Webdir:          "/static/glitter",
		BytesPerSecList: []float64{},
	}

	handler := HandlerWithContext(rc)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	body := rr.Body.String()

	// The raw unescaped key must NOT appear literally; it must be JS-escaped.
	if strings.Contains(body, `abc"def<ghi>jkl`) {
		t.Errorf("body contains unescaped APIKey; JS escaping did not apply")
	}
}

// TestHandler_BackwardCompat verifies that the zero-arg Handler() still works
// (backward compat for existing tests that don't need a render context).
func TestHandler_BackwardCompat(t *testing.T) {
	handler := Handler()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

// TestFuncMap_T verifies the T function integrates with i18n catalog.
func TestFuncMap_T(t *testing.T) {
	tests := []struct {
		name    string
		catalog i18n.Catalog
		key     string
		want    string
	}{
		{
			name:    "T with nil catalog returns key verbatim",
			catalog: nil,
			key:     "menu-queue",
			want:    "menu-queue",
		},
		{
			name:    "T with catalog hit returns translated value",
			catalog: i18n.Catalog{"menu-queue": "Queue"},
			key:     "menu-queue",
			want:    "Queue",
		},
		{
			name:    "T with catalog miss returns key verbatim",
			catalog: i18n.Catalog{"other": "Other"},
			key:     "menu-queue",
			want:    "menu-queue",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fm := newFuncMap(tt.catalog)
			tFn, ok := fm["T"]
			if !ok {
				t.Fatal("FuncMap missing 'T' entry")
			}
			fn, ok := tFn.(func(string) string)
			if !ok {
				t.Fatalf("FuncMap['T'] type = %T, want func(string) string", tFn)
			}
			got := fn(tt.key)
			if got != tt.want {
				t.Errorf("T(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

// TestFuncMap_StaticURL verifies the staticURL function prepends /static/glitter/.
func TestFuncMap_StaticURL(t *testing.T) {
	fm := newFuncMap(nil)
	staticURLFn, ok := fm["staticURL"]
	if !ok {
		t.Fatal("FuncMap missing 'staticURL' entry")
	}
	fn, ok := staticURLFn.(func(string) string)
	if !ok {
		t.Fatalf("FuncMap['staticURL'] type = %T, want func(string) string", staticURLFn)
	}

	tests := []struct {
		input string
		want  string
	}{
		{"images/logo.png", "/static/glitter/images/logo.png"},
		{"", "/static/glitter/"},
	}
	for _, tt := range tests {
		got := fn(tt.input)
		if got != tt.want {
			t.Errorf("staticURL(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestIncludeMessages_RootElementPresent verifies that the rendered output
// contains the expected messages root element with its data-bind attributes.
func TestIncludeMessages_RootElementPresent(t *testing.T) {
	rc := RenderContext{
		Version:         "1.0.0",
		ActiveLang:      "en",
		Webdir:          "/static/glitter",
		BytesPerSecList: []float64{},
	}

	handler := HandlerWithContext(rc)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	body := rr.Body.String()

	// The root element must be present.
	if !strings.Contains(body, `id="queue-messages"`) {
		t.Errorf("body does not contain id=\"queue-messages\"; got:\n%s", body)
	}

	// The data-bind attribute on the root must be present.
	if !strings.Contains(body, `data-bind="visible: hasMessages() || displayTabbed()"`) {
		t.Errorf("body does not contain data-bind=\"visible: hasMessages() || displayTabbed()\"; got:\n%s", body)
	}
}

// TestIncludeMessages_DataBindAttributePreservation verifies that all upstream
// data-bind attributes are present in the rendered output.
func TestIncludeMessages_DataBindAttributePreservation(t *testing.T) {
	// Expected data-bind attributes from the upstream include_messages.tmpl:
	// Line 1: data-bind="visible: hasMessages() || displayTabbed()"
	// Line 13: data-bind="attr: { 'rowspan': parseInt(nrWarnings())+1 }, click: clearWarnings"
	// Line 20: data-bind="css: 'label-' + css, text: type"
	// Line 21: data-bind="text: displayDateTime(timestamp, $parent.dateFormat(), 'X'), attr: { 'data-timestamp': timestamp }"
	// Line 22: data-bind="html: text"
	// Line 29: data-bind="attr: { 'colspan': 1 + !$data.hasOwnProperty('clear') }"
	// Line 30: data-bind="css: 'label-' + css, text: type"
	// Line 31: data-bind="html: text"
	// Line 35: data-bind="click: clear"
	expectedAttributes := []string{
		`visible: hasMessages() || displayTabbed()`,
		`attr: { 'rowspan': parseInt(nrWarnings())+1 }, click: clearWarnings`,
		`css: 'label-' + css, text: type`,
		`text: displayDateTime(timestamp, $parent.dateFormat(), 'X'), attr: { 'data-timestamp': timestamp }`,
		`html: text`,
		`attr: { 'colspan': 1 + !$data.hasOwnProperty('clear') }`,
		`click: clear`,
	}

	rc := RenderContext{
		Version:         "1.0.0",
		ActiveLang:      "en",
		Webdir:          "/static/glitter",
		BytesPerSecList: []float64{},
	}

	handler := HandlerWithContext(rc)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	body := rr.Body.String()

	for _, attr := range expectedAttributes {
		if !strings.Contains(body, attr) {
			t.Errorf("body does not contain data-bind attribute %q; got:\n%s", attr, body)
		}
	}
}

// TestIncludeMessages_NoCheetahTokens verifies that no Cheetah template tokens
// remain in the rendered output (indicating incomplete porting).
func TestIncludeMessages_NoCheetahTokens(t *testing.T) {
	rc := RenderContext{
		Version:         "1.0.0",
		ActiveLang:      "en",
		Webdir:          "/static/glitter",
		BytesPerSecList: []float64{},
	}

	handler := HandlerWithContext(rc)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	body := rr.Body.String()

	if strings.Contains(body, "$T(") {
		t.Errorf("body contains $T( token (incomplete Cheetah->Go conversion)")
	}

	if strings.Contains(body, "<!--#") {
		t.Errorf("body contains <!--# token (incomplete Cheetah->Go conversion)")
	}
}

// TestIncludeMessages_TranslationResolution verifies that translation keys
// resolve via the FuncMap when a populated catalog is provided.
func TestIncludeMessages_TranslationResolution(t *testing.T) {
	// Use a real key from the upstream file: 'none' (line 42 of upstream).
	catalog := i18n.Catalog{"none": "Nothing to show"}

	rc := RenderContext{
		Version:         "1.0.0",
		ActiveLang:      "en",
		Webdir:          "/static/glitter",
		BytesPerSecList: []float64{},
	}

	// Parse all templates with the populated catalog.
	tmpl, err := template.New("main.html.tmpl").Funcs(newFuncMap(catalog)).ParseFS(templatesFS, "templates/*.html.tmpl")
	if err != nil {
		t.Fatalf("template parse: %v", err)
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, rc); err != nil {
		t.Fatalf("template execute: %v", err)
	}

	body := buf.String()

	if !strings.Contains(body, "Nothing to show") {
		t.Errorf("body does not contain translated value 'Nothing to show'; got:\n%s", body)
	}
}
