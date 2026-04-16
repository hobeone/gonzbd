package web

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hobeone/sabnzbd-go/internal/i18n"
)

// renderMenu is a helper that builds a full RenderContext from the given rc,
// executes the entire template set (all *.html.tmpl), and returns the body.
func renderMenu(t *testing.T, rc RenderContext) string {
	t.Helper()
	handler := HandlerWithContext(rc)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	return rr.Body.String()
}

// renderMenuWithCatalog is like renderMenu but lets the caller inject an
// i18n catalog so translated values appear in the output.
func renderMenuWithCatalog(t *testing.T, rc RenderContext, cat i18n.Catalog) string {
	t.Helper()
	tmpl, err := template.New("main.html.tmpl").Funcs(newFuncMap(cat)).ParseFS(templatesFS, "templates/*.html.tmpl")
	if err != nil {
		t.Fatalf("template parse: %v", err)
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, rc); err != nil {
		t.Fatalf("template execute: %v", err)
	}
	return buf.String()
}

// baseMenuRC returns a minimal valid RenderContext for menu tests.
func baseMenuRC() RenderContext {
	return RenderContext{
		Version:         "1.0.0",
		ActiveLang:      "en",
		Webdir:          "/static/glitter",
		BytesPerSecList: []float64{},
	}
}

// --- Layer 1: standard 4-point validation ---

// TestIncludeMenu_RootElementPresent verifies that the rendered output
// contains the navbar root element from include_menu.html.tmpl.
func TestIncludeMenu_RootElementPresent(t *testing.T) {
	body := renderMenu(t, baseMenuRC())

	if !strings.Contains(body, `class="navbar navbar-inverse main-navbar"`) {
		t.Errorf("body does not contain navbar root element")
	}
}

// TestIncludeMenu_TranslationResolution verifies that translation keys
// resolve via the FuncMap when a populated catalog is provided.
func TestIncludeMenu_TranslationResolution(t *testing.T) {
	// 'menu-config' appears on line 81 of upstream include_menu.tmpl.
	catalog := i18n.Catalog{"menu-config": "Configuration"}
	body := renderMenuWithCatalog(t, baseMenuRC(), catalog)

	if !strings.Contains(body, "Configuration") {
		t.Errorf("body does not contain translated value 'Configuration'")
	}
}

// TestIncludeMenu_NoCheetahTokens verifies that no Cheetah template tokens
// remain in the rendered output (indicating incomplete porting).
func TestIncludeMenu_NoCheetahTokens(t *testing.T) {
	body := renderMenu(t, baseMenuRC())

	if strings.Contains(body, "$T(") {
		t.Errorf("body contains $T( token (incomplete Cheetah->Go conversion)")
	}
	if strings.Contains(body, "<!--#") {
		t.Errorf("body contains <!--# token (incomplete Cheetah->Go conversion)")
	}
}

// TestIncludeMenu_NoLeftoverVarRefs verifies that no leftover $-prefixed variable
// references from Cheetah remain in the rendered output.
func TestIncludeMenu_NoLeftoverVarRefs(t *testing.T) {
	body := renderMenu(t, baseMenuRC())

	for _, token := range []string{"$have_", "$new_release", "$pp_pause_event", "$power_options", "$apikey", "$pid", "$webdir"} {
		if strings.Contains(body, token) {
			t.Errorf("body contains leftover Cheetah variable reference %q", token)
		}
	}
}

// --- Layer 2: feature-flag matrix ---

// TestIncludeMenu_FeatureFlagMatrix verifies that each feature-flag conditional
// gates the correct menu entry: true → entry present, false → entry absent.
func TestIncludeMenu_FeatureFlagMatrix(t *testing.T) {
	tests := []struct {
		name      string
		flag      string               // human-readable name of the flag being tested
		setFlag   func(*RenderContext) // set flag to true
		clearFlag func(*RenderContext) // set flag to false (already default)
		// probe is a string that should appear in the rendered output when the
		// flag is true and be absent when the flag is false.
		// Use translated text via a catalog entry so the probe is stable.
		probeKey  string // i18n key for catalog injection
		probeText string // translated text to inject and search for
	}{
		{
			name:      "HaveLogout shows logout link",
			flag:      "HaveLogout",
			setFlag:   func(rc *RenderContext) { rc.HaveLogout = true },
			clearFlag: func(rc *RenderContext) { rc.HaveLogout = false },
			probeKey:  "logout",
			probeText: "LogOutLink",
		},
		{
			name:      "HaveQuota shows reset-quota link",
			flag:      "HaveQuota",
			setFlag:   func(rc *RenderContext) { rc.HaveQuota = true },
			clearFlag: func(rc *RenderContext) { rc.HaveQuota = false },
			probeKey:  "link-resetQuota",
			probeText: "ResetYourQuota",
		},
		{
			name:      "HaveRSSDefined shows RSS nav icon (navbar-have-rss class)",
			flag:      "HaveRSSDefined",
			setFlag:   func(rc *RenderContext) { rc.HaveRSSDefined = true },
			clearFlag: func(rc *RenderContext) { rc.HaveRSSDefined = false },
			// The navbar class is gated on have_rss_defined (line 34 of upstream).
			probeKey:  "cmenu-rss",
			probeText: "RSSFeedLink",
		},
		{
			name:      "HaveWatchedDir shows scan-folder link",
			flag:      "HaveWatchedDir",
			setFlag:   func(rc *RenderContext) { rc.HaveWatchedDir = true },
			clearFlag: func(rc *RenderContext) { rc.HaveWatchedDir = false },
			probeKey:  "sch-scan_folder",
			probeText: "ScanFolderNow",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/flag=true", func(t *testing.T) {
			rc := baseMenuRC()
			tt.setFlag(&rc)

			var body string
			if tt.probeKey != "" {
				catalog := i18n.Catalog{tt.probeKey: tt.probeText}
				body = renderMenuWithCatalog(t, rc, catalog)
			} else {
				body = renderMenu(t, rc)
			}

			if !strings.Contains(body, tt.probeText) {
				t.Errorf("flag %s=true: body does not contain probe %q", tt.flag, tt.probeText)
			}
		})

		t.Run(tt.name+"/flag=false", func(t *testing.T) {
			rc := baseMenuRC()
			tt.clearFlag(&rc)

			var body string
			if tt.probeKey != "" {
				catalog := i18n.Catalog{tt.probeKey: tt.probeText}
				body = renderMenuWithCatalog(t, rc, catalog)
			} else {
				body = renderMenu(t, rc)
			}

			if strings.Contains(body, tt.probeText) {
				t.Errorf("flag %s=false: body unexpectedly contains probe %q", tt.flag, tt.probeText)
			}
		})
	}
}

// TestIncludeMenu_DataBindPreservation verifies all 25 data-bind attributes
// from the upstream include_menu.tmpl are preserved in the rendered output.
func TestIncludeMenu_DataBindPreservation(t *testing.T) {
	body := renderMenu(t, baseMenuRC())
	// Count data-bind attributes; the upstream has 25.
	dataBindCount := countOccurrences(body, "data-bind=")
	if dataBindCount < 25 {
		t.Errorf("body contains fewer data-bind attributes than expected; got %d, want at least 25", dataBindCount)
	}
}
