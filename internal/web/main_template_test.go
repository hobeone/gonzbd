package web

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hobeone/sabnzbd-go/internal/i18n"
)

// renderMain is a test helper that executes the full template set (including
// main.html.tmpl and all partials) with the given RenderContext and returns
// the rendered body string.
func renderMain(t *testing.T, rc RenderContext) string {
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

// renderMainWithCatalog is like renderMain but with an i18n catalog.
func renderMainWithCatalog(t *testing.T, rc RenderContext, cat i18n.Catalog) string {
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

// baseMainRC returns a minimal valid RenderContext for main template tests.
func baseMainRC() RenderContext {
	return RenderContext{
		Version:         "4.0.0",
		APIKey:          "testapikey123",
		ActiveLang:      "en",
		Webdir:          "/static/glitter",
		BytesPerSecList: []float64{},
	}
}

// --- Layer 1: Structural sanity ---

// TestMainTemplate_Doctype verifies the rendered output starts with a DOCTYPE
// declaration and contains a properly formed <html> element with a lang attribute.
func TestMainTemplate_Doctype(t *testing.T) {
	body := renderMain(t, baseMainRC())

	if !strings.HasPrefix(strings.TrimSpace(body), "<!DOCTYPE html>") {
		t.Errorf("body does not start with <!DOCTYPE html>; got prefix:\n%s", body[:min(200, len(body))])
	}

	if !strings.Contains(body, `<html lang="en"`) {
		t.Errorf("body does not contain <html lang=\"en\">; got:\n%s", body[:min(500, len(body))])
	}
}

// TestMainTemplate_RequiredMetaTags verifies required meta tags are present.
func TestMainTemplate_RequiredMetaTags(t *testing.T) {
	body := renderMain(t, baseMainRC())

	requiredMeta := []string{
		`name="viewport"`,
		`name="application-name" content="SABnzbd"`,
	}
	for _, meta := range requiredMeta {
		if !strings.Contains(body, meta) {
			t.Errorf("body missing required meta tag %q", meta)
		}
	}
}

// TestMainTemplate_AllFourPartialsRendered verifies all four partial templates
// are included in the rendered output by checking structural identifiers.
func TestMainTemplate_AllFourPartialsRendered(t *testing.T) {
	body := renderMain(t, baseMainRC())

	partialChecks := []struct {
		name  string
		probe string
	}{
		{"queue partial", `id="queue-tab"`},
		{"history partial", `id="history-tab"`},
		{"messages partial", `id="queue-messages"`},
		{"overlays partial", `<div class="modal`},
	}
	for _, check := range partialChecks {
		if !strings.Contains(body, check.probe) {
			t.Errorf("body missing %s indicator %q", check.name, check.probe)
		}
	}
}

// --- Layer 2: JS injection ---

// TestMainTemplate_APIKeyJSVariable verifies the apiKey JS variable is present
// with the configured value (JS-escaped, so it appears as a quoted string).
func TestMainTemplate_APIKeyJSVariable(t *testing.T) {
	rc := baseMainRC()
	rc.APIKey = "myapikey456"
	body := renderMain(t, rc)

	// The js escaper produces "myapikey456" (with surrounding quotes).
	if !strings.Contains(body, `var apiKey = "myapikey456"`) {
		t.Errorf("body does not contain var apiKey = \"myapikey456\"; got body snippet:\n%s",
			extractSnippet(body, "apiKey"))
	}
}

// TestMainTemplate_DisplayLangJSVariable verifies the displayLang JS variable
// is present with the configured language value.
func TestMainTemplate_DisplayLangJSVariable(t *testing.T) {
	rc := baseMainRC()
	rc.ActiveLang = "de"
	body := renderMain(t, rc)

	if !strings.Contains(body, `var displayLang = "de"`) {
		t.Errorf("body does not contain var displayLang = \"de\"; got body snippet:\n%s",
			extractSnippet(body, "displayLang"))
	}
}

// TestMainTemplate_GlitterTranslatePopulated verifies that glitterTranslate keys
// are present in the rendered output. When catalog is empty, values fall back
// to the key itself. When populated, the translated value appears.
func TestMainTemplate_GlitterTranslatePopulated(t *testing.T) {
	// Layer 2 case A: nil catalog → key as fallback value.
	body := renderMain(t, baseMainRC())

	keysToCheck := []struct {
		jsVar   string
		keyFall string
	}{
		{"glitterTranslate.paused", "post-Paused"},
		{"glitterTranslate.confirm", "confirm"},
		{"glitterTranslate.fetch", "Glitter-fetch"},
	}
	for _, kv := range keysToCheck {
		if !strings.Contains(body, kv.jsVar) {
			t.Errorf("body missing JS variable %q", kv.jsVar)
		}
		// With nil catalog, the T function returns the key as the value.
		if !strings.Contains(body, kv.keyFall) {
			t.Errorf("body missing fallback key %q as translation value", kv.keyFall)
		}
	}

	// Layer 2 case B: populated catalog → translated value appears.
	cat := i18n.Catalog{
		"post-Paused": "Paused",
		"confirm":     "OK",
	}
	bodyTranslated := renderMainWithCatalog(t, baseMainRC(), cat)
	if !strings.Contains(bodyTranslated, `glitterTranslate.paused`) {
		t.Errorf("translated body missing glitterTranslate.paused")
	}
	if !strings.Contains(bodyTranslated, `"Paused"`) {
		t.Errorf("translated body missing translated value \"Paused\"")
	}
}

// TestMainTemplate_NoRawCheetahTokens verifies no unresolved Cheetah-style tokens
// remain in the rendered output. This catches incomplete $T(), $apikey, $active_lang,
// $bytespersec_list conversions.
func TestMainTemplate_NoRawCheetahTokens(t *testing.T) {
	body := renderMain(t, baseMainRC())

	forbiddenTokens := []string{
		"$T(",
		"$apikey",
		"$active_lang",
		"$bytespersec_list",
		"<!--#",
		"$version",
		"$color_scheme",
	}
	for _, tok := range forbiddenTokens {
		if strings.Contains(body, tok) {
			t.Errorf("body contains unresolved token %q (incomplete Cheetah conversion)", tok)
		}
	}
}

// --- Layer 3: Asset references ---

// TestMainTemplate_CSSLinks verifies that the main CSS files are referenced
// via /static/glitter/ absolute paths.
func TestMainTemplate_CSSLinks(t *testing.T) {
	body := renderMain(t, baseMainRC())

	cssChecks := []string{
		`href="/static/glitter/bootstrap/css/bootstrap.min.css`,
		`href="/static/glitter/stylesheets/glitter.css`,
	}
	for _, css := range cssChecks {
		if !strings.Contains(body, css) {
			t.Errorf("body missing CSS link %q", css)
		}
	}
}

// TestMainTemplate_JSScriptTags verifies that the main JS bundles are referenced
// as external script tags under /static/glitter/javascripts/.
func TestMainTemplate_JSScriptTags(t *testing.T) {
	body := renderMain(t, baseMainRC())

	jsChecks := []string{
		`src="/static/glitter/javascripts/glitter.js"`,
		`src="/static/glitter/javascripts/knockout-3.5.1.min.js"`,
		`src="/static/glitter/javascripts/jquery-3.5.1.min.js"`,
	}
	for _, js := range jsChecks {
		if !strings.Contains(body, js) {
			t.Errorf("body missing JS script tag %q", js)
		}
	}
}

// TestMainTemplate_FaviconLink verifies the favicon link references /staticcfg/ico/favicon.ico.
func TestMainTemplate_FaviconLink(t *testing.T) {
	body := renderMain(t, baseMainRC())

	if !strings.Contains(body, `href="/staticcfg/ico/favicon.ico`) {
		t.Errorf("body missing favicon link to /staticcfg/ico/favicon.ico")
	}
}

// --- Layer 4: Conditionals ---

// TestMainTemplate_RTLConditional verifies dir="rtl" is present when RTL=true
// and absent when RTL=false.
func TestMainTemplate_RTLConditional(t *testing.T) {
	rcRTL := baseMainRC()
	rcRTL.RTL = true
	rcRTL.ActiveLang = "he"
	bodyRTL := renderMain(t, rcRTL)

	if !strings.Contains(bodyRTL, `dir="rtl"`) {
		t.Errorf("RTL=true: body missing dir=\"rtl\"")
	}

	rcLTR := baseMainRC()
	rcLTR.RTL = false
	bodyLTR := renderMain(t, rcLTR)

	if strings.Contains(bodyLTR, `dir="rtl"`) {
		t.Errorf("RTL=false: body unexpectedly contains dir=\"rtl\"")
	}
}

// TestMainTemplate_ColorSchemeConditional verifies that when ColorScheme is a
// non-empty, non-"Light" value, the colorscheme CSS link is present. When
// ColorScheme is "" or "Light", that link is absent.
func TestMainTemplate_ColorSchemeConditional(t *testing.T) {
	// Dark scheme present.
	rcDark := baseMainRC()
	rcDark.ColorScheme = "Dark"
	bodyDark := renderMain(t, rcDark)

	if !strings.Contains(bodyDark, `href="/static/glitter/stylesheets/colorschemes/Dark.css`) {
		t.Errorf("ColorScheme=Dark: body missing Dark.css link")
	}

	// Empty scheme absent.
	rcEmpty := baseMainRC()
	rcEmpty.ColorScheme = ""
	bodyEmpty := renderMain(t, rcEmpty)

	if strings.Contains(bodyEmpty, `/colorschemes/`) {
		t.Errorf("ColorScheme=\"\": body unexpectedly contains /colorschemes/ link")
	}

	// Light scheme absent.
	rcLight := baseMainRC()
	rcLight.ColorScheme = "Light"
	bodyLight := renderMain(t, rcLight)

	if strings.Contains(bodyLight, `/colorschemes/`) {
		t.Errorf("ColorScheme=Light: body unexpectedly contains /colorschemes/ link")
	}
}

// TestMainTemplate_LangConditional verifies that the moment.js locale script
// is included when ActiveLang != "en" and excluded when ActiveLang == "en".
func TestMainTemplate_LangConditional(t *testing.T) {
	rcDe := baseMainRC()
	rcDe.ActiveLang = "de"
	bodyDe := renderMain(t, rcDe)

	if !strings.Contains(bodyDe, `momentjs_locale/de.js`) {
		t.Errorf("ActiveLang=de: body missing moment.js locale script for de")
	}

	rcEn := baseMainRC()
	rcEn.ActiveLang = "en"
	bodyEn := renderMain(t, rcEn)

	if strings.Contains(bodyEn, `momentjs_locale/`) {
		t.Errorf("ActiveLang=en: body unexpectedly contains momentjs_locale/ script")
	}
}

// --- Layer 5: Regression — existing partial tests ---
// (These do not need new test functions since the existing tests in render_test.go,
// menu_test.go, and overlays_test.go cover them. This file's tests add the shell
// layer on top.)

// --- Helpers ---

// extractSnippet returns a small context window around the first occurrence of
// needle in s, or the entire string if needle is not found.
func extractSnippet(s, needle string) string {
	idx := strings.Index(s, needle)
	if idx == -1 {
		return "(not found in body)"
	}
	start := max(0, idx-100)
	end := min(len(s), idx+200)
	return "..." + s[start:end] + "..."
}
