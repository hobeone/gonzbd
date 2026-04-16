package web

import (
	"strings"
	"testing"

	"github.com/hobeone/sabnzbd-go/internal/i18n"
)

// baseOverlaysRC returns a minimal valid RenderContext for overlays tests.
func baseOverlaysRC() RenderContext {
	return RenderContext{
		Version:         "4.3.0",
		ActiveLang:      "en",
		Webdir:          "/static/glitter",
		BytesPerSecList: []float64{},
		APIKey:          "testoverlaykey",
	}
}

// --- Layer 1: Standard 4-point validation ---

// TestIncludeOverlays_RootElementPresent verifies that a stable root-level
// modal element from include_overlays.html.tmpl is present in the output.
// modal-options is the first and largest modal in the upstream.
func TestIncludeOverlays_RootElementPresent(t *testing.T) {
	body := renderMenu(t, baseOverlaysRC())

	if !strings.Contains(body, `id="modal-options"`) {
		t.Errorf("body does not contain modal-options root element (id=\"modal-options\")")
	}
}

// TestIncludeOverlays_TranslationResolution verifies that translation keys
// inside overlays resolve via the FuncMap when a populated catalog is provided.
func TestIncludeOverlays_TranslationResolution(t *testing.T) {
	// 'Glitter-addNZB' appears in the modal-add-nzb header.
	catalog := i18n.Catalog{"Glitter-addNZB": "Add NZB"}
	body := renderMenuWithCatalog(t, baseOverlaysRC(), catalog)

	if !strings.Contains(body, "Add NZB") {
		t.Errorf("body does not contain translated value 'Add NZB' for key 'Glitter-addNZB'")
	}
}

// TestIncludeOverlays_NoCheetahTokens verifies that no Cheetah template tokens
// remain in the rendered output — $T( and <!--# are the two sentinel forms.
func TestIncludeOverlays_NoCheetahTokens(t *testing.T) {
	body := renderMenu(t, baseOverlaysRC())

	if strings.Contains(body, "$T(") {
		t.Errorf("body contains $T( token (incomplete Cheetah->Go conversion)")
	}
	if strings.Contains(body, "<!--#") {
		t.Errorf("body contains <!--# token (incomplete Cheetah->Go conversion)")
	}
}

// TestIncludeOverlays_DataBindCount verifies that the expected number of
// data-bind attributes from upstream include_overlays.tmpl are preserved.
// Upstream has 125 data-bind attributes. We assert at least 120 to allow for
// any that are genuinely inside omitted/stubbed blocks.
func TestIncludeOverlays_DataBindCount(t *testing.T) {
	body := renderMenu(t, baseOverlaysRC())
	// Count data-bind attributes across the whole rendered page.
	total := countOccurrences(body, "data-bind=")
	// The overlays template alone has 125 upstream data-bind attributes;
	// the full page will have all previous templates too (menu 25, queue 76, history 55, messages ~9).
	// Subtract prior templates: assert overlays contributes at least 120.
	// We do a whole-page count with a generous floor to catch gross losses.
	if total < 280 {
		t.Errorf("whole-page data-bind count = %d, want at least 280 (overlays 125 + prior ~160)", total)
	}
}

// --- Layer 2: Modal count ---

// TestIncludeOverlays_ModalCount verifies that the expected number of modal
// <div class="modal"> elements from upstream are present in the rendered output.
// Upstream include_overlays.tmpl has 10 modals (class="modal fade" or similar).
func TestIncludeOverlays_ModalCount(t *testing.T) {
	body := renderMenu(t, baseOverlaysRC())

	// Count occurrences of 'class="modal' which matches all modal variants:
	// class="modal fade", class="modal modal-delete-job fade", class="modal modal-small fade".
	count := countOccurrences(body, `class="modal `)
	if count < 10 {
		t.Errorf("modal count = %d, want at least 10 (matching upstream include_overlays.tmpl)", count)
	}
}

// --- Layer 3: Spot-check representative modals ---

// TestIncludeOverlays_ModalOptions verifies the options/status modal renders
// with its outer div and header title.
func TestIncludeOverlays_ModalOptions(t *testing.T) {
	catalog := i18n.Catalog{"Glitter-statusInterfaceOptions": "Status and Interface Options"}
	body := renderMenuWithCatalog(t, baseOverlaysRC(), catalog)

	if !strings.Contains(body, `id="modal-options"`) {
		t.Errorf("body missing modal-options outer div")
	}
	if !strings.Contains(body, "Status and Interface Options") {
		t.Errorf("body missing modal-options header title translation")
	}
}

// TestIncludeOverlays_ModalAddNZB verifies the add-NZB modal renders with
// its outer div and heading.
func TestIncludeOverlays_ModalAddNZB(t *testing.T) {
	catalog := i18n.Catalog{"Glitter-addNZB": "Add NZB File"}
	body := renderMenuWithCatalog(t, baseOverlaysRC(), catalog)

	if !strings.Contains(body, `id="modal-add-nzb"`) {
		t.Errorf("body missing modal-add-nzb outer div")
	}
	if !strings.Contains(body, "Add NZB File") {
		t.Errorf("body missing modal-add-nzb header title translation")
	}
}

// TestIncludeOverlays_ModalCustomPause verifies the custom-pause modal renders
// with its outer div and heading. This is the last modal in upstream.
func TestIncludeOverlays_ModalCustomPause(t *testing.T) {
	catalog := i18n.Catalog{"Glitter-pauseFor": "Pause For"}
	body := renderMenuWithCatalog(t, baseOverlaysRC(), catalog)

	if !strings.Contains(body, `id="modal-custom-pause"`) {
		t.Errorf("body missing modal-custom-pause outer div")
	}
	if !strings.Contains(body, "Pause For") {
		t.Errorf("body missing modal-custom-pause header title translation")
	}
}
