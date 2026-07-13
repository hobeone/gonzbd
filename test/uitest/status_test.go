//go:build uitest

package uitest

import (
	"testing"

	"github.com/mxschmitt/playwright-go"
)

// ---------------------------------------------------------------------------
// Status Page
// ---------------------------------------------------------------------------

// TestStatusPageLoadsAndShowsGeneralInfo verifies that navigating directly to
// /status (not via a click-through from the dashboard) renders the page's
// core sections. This is a regression guard for a defect fixed by the News
// Servers work: a direct navigation didn't start the WebSocket telemetry
// subscription, so the News Servers section rendered empty. Asserting "News
// Servers" is visible after a direct env.navigate proves that fix end-to-end.
func TestStatusPageLoadsAndShowsGeneralInfo(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/status")

	heading := page.GetByText("Status", playwright.PageGetByTextOptions{Exact: new(true)})
	if err := heading.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("Status page heading not visible: %v", err)
	}

	generalInfo := page.GetByText("General Info")
	if err := generalInfo.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Errorf("General Info section not visible: %v", err)
	}

	systemInfo := page.GetByText("System Info")
	if err := systemInfo.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Errorf("System Info section not visible: %v", err)
	}

	// This is the specific scenario Task 9 fixed: on a direct navigation to
	// /status, the WebSocket telemetry subscription must be started so the
	// News Servers section is populated rather than left empty.
	newsServers := page.GetByText("News Servers")
	if err := newsServers.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Errorf("News Servers section not visible after direct navigation: %v", err)
	}
}
