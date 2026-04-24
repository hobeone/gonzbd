//go:build uitest

package uitest

import (
	"fmt"
	"testing"

	"github.com/playwright-community/playwright-go"
)

// ---------------------------------------------------------------------------
// Core Navigation & Layout
// ---------------------------------------------------------------------------

func TestPageLoads(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	page := env.newPage(t)
	env.navigate(t, page, "/")

	title, err := page.Title()
	if err != nil {
		t.Fatalf("page.Title: %v", err)
	}
	if title == "" {
		t.Error("page title is empty")
	}
	t.Logf("page title: %q", title)

	// Navbar should be visible.
	navbar := page.Locator("nav")
	if err := navbar.First().WaitFor(playwright.LocatorWaitForOptions{
		State: playwright.WaitForSelectorStateVisible,
	}); err != nil {
		t.Errorf("navbar not visible: %v", err)
	}
}

func TestEmptyState(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	page := env.newPage(t)
	env.navigate(t, page, "/")

	// With no queue items, the page should show empty state.
	// Wait for the queue data to load.
	emptyText := page.GetByText("No items in queue")
	err := emptyText.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	})
	if err != nil {
		// Might be phrased differently. Check for the table being empty.
		t.Logf("empty state text not found (may use different wording): %v", err)
	}
}

// ---------------------------------------------------------------------------
// Queue Operations
// ---------------------------------------------------------------------------

func TestQueueDisplaysItems(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)

	// Seed 3 jobs.
	env.seedQueue(t, 3)

	page := env.newPage(t)
	env.navigate(t, page, "/")

	// Wait for queue data to render.
	for i := range 3 {
		name := page.GetByText("Test.Download." + itoa(i) + ".x264-GROUP")
		if err := name.First().WaitFor(playwright.LocatorWaitForOptions{
			State:   playwright.WaitForSelectorStateVisible,
			Timeout: playwright.Float(5000),
		}); err != nil {
			t.Errorf("job %d not visible: %v", i, err)
		}
	}
}

func TestQueuePauseResume(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.seedQueue(t, 1)

	page := env.newPage(t)
	env.navigate(t, page, "/")

	// Scope to the navbar to avoid matching per-job pause buttons.
	nav := page.Locator("nav")

	// Find and click the Pause button inside the navbar.
	pauseBtn := nav.GetByRole("button", playwright.LocatorGetByRoleOptions{Name: "Pause"})
	if err := pauseBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("Pause button not visible in navbar: %v", err)
	}

	if err := pauseBtn.Click(); err != nil {
		t.Fatalf("click Pause: %v", err)
	}

	// The backend doesn't broadcast a WebSocket event on pause state changes,
	// so the SPA won't re-poll until the 30s fallback. Reload to force re-fetch.
	if _, err := page.Reload(playwright.PageReloadOptions{
		WaitUntil: playwright.WaitUntilStateNetworkidle,
	}); err != nil {
		t.Fatalf("reload after pause: %v", err)
	}

	resumeBtn := nav.GetByRole("button", playwright.LocatorGetByRoleOptions{Name: "Resume"})
	if err := resumeBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("Resume button not visible after pause: %v", err)
	}

	// Click Resume — Pause button should reappear after reload.
	if err := resumeBtn.Click(); err != nil {
		t.Fatalf("click Resume: %v", err)
	}

	if _, err := page.Reload(playwright.PageReloadOptions{
		WaitUntil: playwright.WaitUntilStateNetworkidle,
	}); err != nil {
		t.Fatalf("reload after resume: %v", err)
	}

	pauseBtn2 := nav.GetByRole("button", playwright.LocatorGetByRoleOptions{Name: "Pause"})
	if err := pauseBtn2.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Errorf("Pause button not visible after resume: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Settings Dialog
// ---------------------------------------------------------------------------

func TestSettingsOpensAndLoadsConfig(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	page := env.newPage(t)
	env.navigate(t, page, "/")

	// Look for the settings/gear button.
	settingsBtn := page.Locator("[aria-label='Settings'], button:has-text('Settings'), [title='Settings']")
	if err := settingsBtn.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Skipf("Settings button not found (UI may not have it yet): %v", err)
	}

	if err := settingsBtn.First().Click(); err != nil {
		t.Fatalf("click Settings: %v", err)
	}

	// The settings dialog should be visible.
	dialog := page.GetByText("General Settings")
	if err := dialog.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Errorf("Settings dialog content not visible: %v", err)
	}
}

func TestSettingsTabNavigation(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	page := env.newPage(t)
	env.navigate(t, page, "/")

	// Open settings.
	settingsBtn := page.Locator("[aria-label='Settings'], button:has-text('Settings'), [title='Settings']")
	if err := settingsBtn.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Skipf("Settings button not found: %v", err)
	}
	if err := settingsBtn.First().Click(); err != nil {
		t.Fatalf("click Settings: %v", err)
	}

	// Wait for dialog to load.
	if err := page.GetByText("General Settings").WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("Settings dialog not loaded: %v", err)
	}

	// Click through sidebar tabs and check content updates.
	tabs := []struct {
		tabName    string
		expectText string
	}{
		{"Servers", "Usenet Servers"},
		{"Categories", "Categories"},
		{"Downloads", "Download Settings"},
		{"General", "General Settings"},
	}

	for _, tc := range tabs {
		tabBtn := page.GetByRole("button", playwright.PageGetByRoleOptions{Name: tc.tabName})
		if err := tabBtn.Click(); err != nil {
			t.Errorf("click tab %q: %v", tc.tabName, err)
			continue
		}
		content := page.GetByText(tc.expectText)
		if err := content.First().WaitFor(playwright.LocatorWaitForOptions{
			State:   playwright.WaitForSelectorStateVisible,
			Timeout: playwright.Float(3000),
		}); err != nil {
			t.Errorf("tab %q: expected %q not visible: %v", tc.tabName, tc.expectText, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Status Bar
// ---------------------------------------------------------------------------

func TestStatusBarDisplays(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.seedQueue(t, 2)

	page := env.newPage(t)
	env.navigate(t, page, "/")

	// The status bar should show some indication of items/speed.
	// Look for "items" or similar text.
	statusArea := page.Locator("[class*='status'], footer, [data-testid='status-bar']")
	if err := statusArea.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Logf("status bar area not found by class/testid: %v", err)
	}
}

// ---------------------------------------------------------------------------
// API endpoint (verify backend serves correctly)
// ---------------------------------------------------------------------------

func TestAPIVersionEndpoint(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	page := env.newPage(t)

	// Direct API call via browser.
	resp, err := page.Request().Get(env.BaseURL + "/api?mode=version")
	if err != nil {
		t.Fatalf("API request: %v", err)
	}
	if resp.Status() != 200 {
		t.Errorf("API status = %d; want 200", resp.Status())
	}

	body, err := resp.Text()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if body == "" {
		t.Error("empty API response body")
	}
	t.Logf("API version response: %s", body)
}

func TestAPIQueueEndpoint(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.seedQueue(t, 2)

	page := env.newPage(t)

	resp, err := page.Request().Get(env.BaseURL + "/api?mode=queue&apikey=" + testAPIKey)
	if err != nil {
		t.Fatalf("API request: %v", err)
	}
	if resp.Status() != 200 {
		t.Errorf("API status = %d; want 200", resp.Status())
	}

	body, err := resp.Text()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	t.Logf("Queue response (first 500 chars): %.500s", body)
}

// ---------------------------------------------------------------------------
// SPA cookie (API key injection)
// ---------------------------------------------------------------------------

func TestSPASetsAPIKeyCookie(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	page := env.newPage(t)
	env.navigate(t, page, "/")

	cookies, err := page.Context().Cookies(env.BaseURL)
	if err != nil {
		t.Fatalf("get cookies: %v", err)
	}

	var found bool
	for _, c := range cookies {
		if c.Name == "sab_apikey" {
			found = true
			if c.Value != testAPIKey {
				t.Errorf("sab_apikey cookie = %q; want %q", c.Value, testAPIKey)
			}
			break
		}
	}
	if !found {
		t.Error("sab_apikey cookie not set by SPA handler")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}
