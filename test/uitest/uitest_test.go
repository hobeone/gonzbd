//go:build uitest

//nolint:staticcheck // pre-existing page.WaitForTimeout deprecated warnings in Playwright tests
package uitest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mxschmitt/playwright-go"

	"github.com/hobeone/gonzbd/internal/api"
)

// ---------------------------------------------------------------------------
// Core Navigation & Layout
// ---------------------------------------------------------------------------

func TestPageLoads(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")

	title, err := page.Title()
	if err != nil {
		t.Fatalf("page.Title: %v", err)
	}
	if title == "" {
		t.Error("page title is empty")
	}
	t.Logf("page title: %q", title)

	// Navbar header should be visible.
	navbar := page.Locator("header")
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
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")

	// With no queue items, the page should show "Queue is empty".
	emptyText := page.GetByText("Queue is empty")
	err := emptyText.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	})
	if err != nil {
		t.Errorf("empty state text 'Queue is empty' not found: %v", err)
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
	screenshotOnFailure(t, page)
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
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")

	// Scope to the navbar header to avoid matching per-job pause buttons.
	nav := page.Locator("header")

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

func TestQueueJobPauseResume(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.seedQueue(t, 1)

	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")

	// Wait for the job row to render.
	jobText := page.GetByText("Test.Download.0.x264-GROUP")
	if err := jobText.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("job not visible: %v", err)
	}

	// Find the per-job pause button (inside the table row, not navbar).
	row := page.Locator("tr", playwright.PageLocatorOptions{
		HasText: "Test.Download.0",
	})
	pauseBtn := row.GetByTitle("Pause")
	if err := pauseBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(3000),
	}); err != nil {
		t.Fatalf("per-job Pause button not visible: %v", err)
	}

	if err := pauseBtn.Click(); err != nil {
		t.Fatalf("click per-job Pause: %v", err)
	}

	// Reload and verify the button changed to Resume.
	if _, err := page.Reload(playwright.PageReloadOptions{
		WaitUntil: playwright.WaitUntilStateNetworkidle,
	}); err != nil {
		t.Fatalf("reload: %v", err)
	}

	row2 := page.Locator("tr", playwright.PageLocatorOptions{
		HasText: "Test.Download.0",
	})
	resumeBtn := row2.GetByTitle("Resume")
	if err := resumeBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Errorf("per-job Resume button not visible after pause: %v", err)
	}
}

func TestQueueDeleteJob(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.seedQueue(t, 2)

	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")

	// Wait for both jobs.
	for i := range 2 {
		name := page.GetByText(fmt.Sprintf("Test.Download.%d.x264-GROUP", i))
		if err := name.First().WaitFor(playwright.LocatorWaitForOptions{
			State:   playwright.WaitForSelectorStateVisible,
			Timeout: playwright.Float(5000),
		}); err != nil {
			t.Fatalf("job %d not visible: %v", i, err)
		}
	}

	// Click the delete button on the first job (opens confirmation dialog).
	row := page.Locator("tr", playwright.PageLocatorOptions{
		HasText: "Test.Download.0",
	})
	deleteBtn := row.GetByTitle("Delete")
	if err := deleteBtn.Click(); err != nil {
		t.Fatalf("click Delete icon: %v", err)
	}

	// The confirmation dialog should appear with a "Delete Job" button.
	confirmBtn := page.GetByRole("button", playwright.PageGetByRoleOptions{Name: "Delete Job"})
	if err := confirmBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(3000),
	}); err != nil {
		t.Fatalf("Delete Job confirmation button not visible: %v", err)
	}
	if err := confirmBtn.Click(); err != nil {
		t.Fatalf("click Delete Job confirm: %v", err)
	}

	// Reload to force the SPA to re-fetch queue state.
	if _, err := page.Reload(playwright.PageReloadOptions{
		WaitUntil: playwright.WaitUntilStateNetworkidle,
	}); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// Job 1 should still be visible.
	name1 := page.GetByText("Test.Download.1.x264-GROUP")
	if err := name1.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Errorf("job 1 should still be visible: %v", err)
	}

	// Job 0 should be gone.
	name0 := page.GetByText("Test.Download.0.x264-GROUP")
	if err := name0.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateHidden,
		Timeout: playwright.Float(3000),
	}); err != nil {
		t.Errorf("job 0 should be gone after delete: %v", err)
	}
}

func TestQueueSearch(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.seedQueue(t, 5)

	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")

	// Wait for all items to render.
	name4 := page.GetByText("Test.Download.4.x264-GROUP")
	if err := name4.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("jobs not visible: %v", err)
	}

	// Find search input. The queue table has a search input.
	searchInput := page.GetByPlaceholder("Search")
	if err := searchInput.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(3000),
	}); err != nil {
		t.Fatalf("search input not visible: %v", err)
	}

	// Type a search term.
	if err := searchInput.First().Fill("Download.3"); err != nil {
		t.Fatalf("fill search: %v", err)
	}

	// Give the SPA time to filter (debounce + re-render).
	page.WaitForTimeout(1000)

	// Job 3 should still be visible.
	name3 := page.GetByText("Test.Download.3.x264-GROUP")
	if err := name3.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(3000),
	}); err != nil {
		t.Errorf("job 3 should be visible after search: %v", err)
	}
}

// ---------------------------------------------------------------------------
// History Operations
// ---------------------------------------------------------------------------

func TestHistoryDisplaysItems(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.seedHistory(t, 3, "Completed")

	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")

	// History is a section on the main page (not a separate tab).
	// Wait for history items to render.
	for i := range 3 {
		name := page.GetByText(fmt.Sprintf("History.Item.%d.x264-GROUP", i))
		if err := name.First().WaitFor(playwright.LocatorWaitForOptions{
			State:   playwright.WaitForSelectorStateVisible,
			Timeout: playwright.Float(10000),
		}); err != nil {
			t.Errorf("history item %d not visible: %v", i, err)
		}
	}
}

func TestHistoryShowsCategory(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.seedHistory(t, 1, "Completed")

	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")

	// Wait for history item.
	name := page.GetByText("History.Item.0.x264-GROUP")
	if err := name.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(10000),
	}); err != nil {
		t.Fatalf("history item not visible: %v", err)
	}

	// Category "Movies" should be visible in the row.
	category := page.GetByText("Movies")
	if err := category.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(3000),
	}); err != nil {
		t.Errorf("category 'Movies' not visible: %v", err)
	}
}

func TestHistoryEmptyState(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	// Don't seed any history.

	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")

	// History section should show "History is empty".
	emptyText := page.GetByText("History is empty")
	if err := emptyText.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Errorf("history empty state text not found: %v", err)
	}
}

func TestHistoryFailedFilter(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	// Seed 2 completed + 1 failed, using unique ID prefixes.
	env.seedHistoryWithPrefix(t, 2, "Completed", "comp")
	env.seedHistoryWithPrefix(t, 1, "Failed", "fail")

	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")

	// Wait for all history items to load.
	compItem := page.GetByText("comp.History.0.x264-GROUP")
	if err := compItem.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(10000),
	}); err != nil {
		t.Fatalf("completed history items not loaded: %v", err)
	}

	// Click "Failed only" checkbox.
	failedCheckbox := page.GetByText("Failed only")
	if err := failedCheckbox.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(3000),
	}); err != nil {
		t.Fatalf("'Failed only' label not visible: %v", err)
	}
	if err := failedCheckbox.Click(); err != nil {
		t.Fatalf("click 'Failed only': %v", err)
	}

	// Give the SPA time to re-fetch with filter.
	page.WaitForTimeout(1500)

	// The failed item should be visible.
	failItem := page.GetByText("fail.History.0.x264-GROUP")
	if err := failItem.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Errorf("failed item should be visible after filter: %v", err)
	}
}

func TestHistorySearch(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.seedHistory(t, 5, "Completed")

	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")

	// Wait for history to load.
	item0 := page.GetByText("History.Item.0.x264-GROUP")
	if err := item0.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(10000),
	}); err != nil {
		t.Fatalf("history not loaded: %v", err)
	}

	// Find the history search input (placeholder "Search history...").
	searchInput := page.GetByPlaceholder("Search history")
	if err := searchInput.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(3000),
	}); err != nil {
		t.Fatalf("history search input not visible: %v", err)
	}

	if err := searchInput.First().Fill("Item.3"); err != nil {
		t.Fatalf("fill history search: %v", err)
	}

	// Give debounce + re-fetch time.
	page.WaitForTimeout(1500)

	// Item 3 should be visible.
	item3 := page.GetByText("History.Item.3.x264-GROUP")
	if err := item3.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Errorf("history item 3 should be visible after search: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Settings Dialog
// ---------------------------------------------------------------------------

func TestSettingsOpensAndLoadsConfig(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")
	page.WaitForTimeout(1000)

	// Look for the settings/gear button.
	settingsBtn := page.Locator("button[title='Settings']")
	if err := settingsBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("Settings button not visible: %v", err)
	}

	if err := settingsBtn.Click(); err != nil {
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
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")
	page.WaitForTimeout(1000)

	// Open settings.
	settingsBtn := page.Locator("button[title='Settings']")
	if err := settingsBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("Settings button not visible: %v", err)
	}
	if err := settingsBtn.Click(); err != nil {
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

func TestStatusBarDisplaysData(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.seedQueue(t, 2)

	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")

	// The status bar should show the item count.
	page.WaitForTimeout(1000)

	// Look for status bar content showing queue info.
	statusArea := page.Locator("[data-testid='status-bar']")
	if err := statusArea.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("status bar not visible: %v", err)
	}

	// Verify actual content.
	text, err := statusArea.First().TextContent()
	if err != nil {
		t.Fatalf("could not read status bar text: %v", err)
	}
	if text == "" {
		t.Error("status bar text is empty")
	} else {
		t.Logf("status bar content: %q", text)
	}
}

func TestSpeedLimitSliderShowsWithBandwidthMax(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)

	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")

	// Push a metrics event with a configured bandwidth max of 10 MiB/s.
	env.APISrv.EventBroadcaster().Broadcast(api.Event{
		Type:          "metrics",
		Speed:         500000,
		SpeedLimit:    10 * 1024 * 1024,
		BandwidthMax:  10 * 1024 * 1024,
		BandwidthPerc: 100,
	})

	page.WaitForTimeout(1500)

	// Click the speed display to open the popover.
	speedBtn := page.Locator("button[title='Click to set speed limit']")
	if err := speedBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("speed button not visible: %v", err)
	}
	if err := speedBtn.Click(); err != nil {
		t.Fatalf("click speed button: %v", err)
	}

	// The popover dialog should appear.
	dialog := page.Locator("[role='dialog']")
	if err := dialog.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(3000),
	}); err != nil {
		t.Fatalf("speed limit popover not visible: %v", err)
	}

	// Should show the max bandwidth.
	maxLabel := page.GetByText("Max:")
	if err := maxLabel.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(2000),
	}); err != nil {
		t.Fatalf("Max label not visible: %v", err)
	}

	// Should contain a range slider.
	slider := dialog.Locator("input[type='range']")
	if err := slider.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(2000),
	}); err != nil {
		t.Fatalf("slider not visible: %v", err)
	}

	// Should show the current percentage.
	percText := page.GetByText("100%")
	if err := percText.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(2000),
	}); err != nil {
		t.Errorf("100%% label not visible: %v", err)
	}
}

func TestSpeedLimitSliderHiddenWhenNoBandwidthMax(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)

	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")

	// Push metrics with bandwidth_max=0 (no limit configured).
	env.APISrv.EventBroadcaster().Broadcast(api.Event{
		Type:          "metrics",
		Speed:         0,
		BandwidthMax:  0,
		BandwidthPerc: 100,
	})

	page.WaitForTimeout(1500)

	// Click the speed display.
	speedBtn := page.Locator("button[title='Click to set speed limit']")
	if err := speedBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("speed button not visible: %v", err)
	}
	if err := speedBtn.Click(); err != nil {
		t.Fatalf("click speed button: %v", err)
	}

	dialog := page.Locator("[role='dialog']")
	if err := dialog.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(3000),
	}); err != nil {
		t.Fatalf("speed limit popover not visible: %v", err)
	}

	// Should show "No bandwidth limit set" message.
	noLimitMsg := page.GetByText("No bandwidth limit set")
	if err := noLimitMsg.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(2000),
	}); err != nil {
		t.Errorf("'No bandwidth limit set' message not visible: %v", err)
	}

	// Slider should NOT be present.
	slider := dialog.Locator("input[type='range']")
	count, _ := slider.Count()
	if count > 0 {
		t.Errorf("slider should not be visible when bandwidth_max=0")
	}
}

// ---------------------------------------------------------------------------
// Warning Banner
// ---------------------------------------------------------------------------

func TestWarningBannerDisplays(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)

	// Inject a warning via the API server.
	env.APISrv.AddWarning("Test warning: disk space low")

	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")

	// Wait for the warning poll to pick it up.
	warningText := page.GetByText("disk space low")
	if err := warningText.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(10000),
	}); err != nil {
		t.Errorf("warning banner not visible: %v", err)
	}
}

func TestWarningBannerClear(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)

	// Inject warnings.
	env.APISrv.AddWarning("Warning 1")
	env.APISrv.AddWarning("Warning 2")

	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")

	// Wait for warnings to appear.
	warningText := page.GetByText("Warning 1")
	if err := warningText.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(10000),
	}); err != nil {
		t.Fatalf("warning banner not visible: %v", err)
	}

	// Click "Clear all" or dismiss button.
	clearBtn := page.GetByRole("button", playwright.PageGetByRoleOptions{Name: "Clear"})
	if err := clearBtn.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(3000),
	}); err != nil {
		t.Fatalf("Clear button not visible: %v", err)
	}
	if err := clearBtn.First().Click(); err != nil {
		t.Fatalf("click Clear: %v", err)
	}

	// After clearing, reload and verify warnings are gone.
	if _, err := page.Reload(playwright.PageReloadOptions{
		WaitUntil: playwright.WaitUntilStateNetworkidle,
	}); err != nil {
		t.Fatalf("reload: %v", err)
	}

	page.WaitForTimeout(2000)

	// Verify the warning text is no longer visible.
	warningText2 := page.GetByText("Warning 1")
	if err := warningText2.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateHidden,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Errorf("warning should be cleared: %v", err)
	}
}

// ---------------------------------------------------------------------------
// API endpoints (verify backend serves correctly)
// ---------------------------------------------------------------------------

func TestAPIVersionEndpoint(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	page := env.newPage(t)
	screenshotOnFailure(t, page)

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
	if !strings.Contains(body, "test-uitest") {
		t.Errorf("version response should contain 'test-uitest'; got: %s", body)
	}
	t.Logf("API version response: %s", body)
}

func TestAPIQueueEndpoint(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.seedQueue(t, 2)

	page := env.newPage(t)
	screenshotOnFailure(t, page)

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
	// Verify actual content, not just non-empty.
	if !strings.Contains(body, "Test.Download.0") {
		t.Errorf("queue response should contain seeded job name; got: %.500s", body)
	}
	t.Logf("Queue response (first 500 chars): %.500s", body)
}

func TestAPIHistoryEndpoint(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.seedHistory(t, 2, "Completed")

	page := env.newPage(t)
	screenshotOnFailure(t, page)

	resp, err := page.Request().Get(env.BaseURL + "/api?mode=history&apikey=" + testAPIKey)
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
	if !strings.Contains(body, "History.Item.0") {
		t.Errorf("history response should contain seeded entry; got: %.500s", body)
	}
}

// ---------------------------------------------------------------------------
// SPA cookie (API key injection)
// ---------------------------------------------------------------------------

func TestSPASetsAPIKeyCookie(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")

	cookies, err := page.Context().Cookies(env.BaseURL)
	if err != nil {
		t.Fatalf("get cookies: %v", err)
	}

	var found bool
	for _, c := range cookies {
		if c.Name == "gonzbd_apikey" {
			found = true
			// The cookie carries the API server's ephemeral session key,
			// not the configured APIKey — see AuthConfig.SessionKey.
			if c.Value != env.APISrv.SessionKey() {
				t.Errorf("gonzbd_apikey cookie = %q; want %q", c.Value, env.APISrv.SessionKey())
			}
			if c.Value == testAPIKey {
				t.Error("gonzbd_apikey cookie must not equal the permanent APIKey")
			}
			break
		}
	}
	if !found {
		t.Error("gonzbd_apikey cookie not set by SPA handler")
	}
}

func TestAddNZBViaUI(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)

	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")

	nzbPath := createTestNZBFile(t, "sample_add.nzb", "Sample.Add.NZB.Movie.x264-GROUP")

	addBtn := page.GetByRole("button", playwright.PageGetByRoleOptions{Name: "Add NZB"})
	if err := addBtn.Click(); err != nil {
		t.Fatalf("click Add NZB button: %v", err)
	}

	modalHeader := page.GetByText("Upload an NZB file or paste a URL.")
	if err := modalHeader.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("Add NZB modal header not visible: %v", err)
	}

	fileInput := page.Locator("input[type=file]")
	if err := fileInput.SetInputFiles(nzbPath); err != nil {
		t.Fatalf("SetInputFiles: %v", err)
	}

	uploadBtn := page.GetByRole("button", playwright.PageGetByRoleOptions{Name: "Upload"})
	if err := uploadBtn.Click(); err != nil {
		t.Fatalf("click Upload: %v", err)
	}

	jobRow := page.GetByText("sample_add.nzb")
	if err := jobRow.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("uploaded NZB job not visible in queue: %v", err)
	}
}

func TestAddNZBWithPriorityAndPaused(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")

	nzbPath := createTestNZBFile(t, "add_paused.nzb", "Add.Paused.NZB.Movie.x264-GROUP")

	addBtn := page.GetByRole("button", playwright.PageGetByRoleOptions{Name: "Add NZB"})
	if err := addBtn.Click(); err != nil {
		t.Fatalf("click Add NZB button: %v", err)
	}

	modalHeader := page.GetByText("Upload an NZB file or paste a URL.")
	if err := modalHeader.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("Add NZB modal header not visible: %v", err)
	}

	prioritySelect := page.Locator("#priority")
	prioVal, err := prioritySelect.InputValue()
	if err != nil {
		t.Fatalf("read priority select value: %v", err)
	}
	if prioVal != "-100" {
		t.Errorf("priority select default = %q, want %q", prioVal, "-100")
	}

	pausedCheckbox := page.Locator("#paused")
	if err := pausedCheckbox.Check(); err != nil {
		t.Fatalf("check paused checkbox: %v", err)
	}

	prioVal, err = prioritySelect.InputValue()
	if err != nil {
		t.Fatalf("read priority select value after checking paused: %v", err)
	}
	if prioVal != "-2" {
		t.Errorf("priority select value after check = %q, want %q (-2)", prioVal, "-2")
	}

	fileInput := page.Locator("input[type=file]")
	if err := fileInput.SetInputFiles(nzbPath); err != nil {
		t.Fatalf("SetInputFiles: %v", err)
	}

	uploadBtn := page.GetByRole("button", playwright.PageGetByRoleOptions{Name: "Upload"})
	if err := uploadBtn.Click(); err != nil {
		t.Fatalf("click Upload: %v", err)
	}

	jobRow := page.GetByText("add_paused.nzb")
	if err := jobRow.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("uploaded NZB job not visible in queue: %v", err)
	}

	pausedBadge := page.Locator("td").Filter(playwright.LocatorFilterOptions{HasText: "Paused"})
	if err := pausedBadge.First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("job status Paused badge not visible: %v", err)
	}
}

func TestDuplicateNZBWarningAndInteractivity(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)

	env.seedQueue(t, 1)

	page := env.newPage(t)
	screenshotOnFailure(t, page)
	env.navigate(t, page, "/")

	seededJob := page.GetByText("Test.Download.0.x264-GROUP")
	if err := seededJob.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("seeded job not visible: %v", err)
	}

	nzbPath := createTestNZBFile(t, "dup_test.nzb", "Duplicate.Subject.x264-GROUP")
	addBtn := page.GetByRole("button", playwright.PageGetByRoleOptions{Name: "Add NZB"})
	if err := addBtn.Click(); err != nil {
		t.Fatalf("click Add NZB: %v", err)
	}

	fileInput := page.Locator("input[type=file]")
	if err := fileInput.SetInputFiles(nzbPath); err != nil {
		t.Fatalf("SetInputFiles: %v", err)
	}

	uploadBtn := page.GetByRole("button", playwright.PageGetByRoleOptions{Name: "Upload"})
	if err := uploadBtn.Click(); err != nil {
		t.Fatalf("click Upload: %v", err)
	}

	if err := page.GetByText("dup_test.nzb").First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("dup_test.nzb not visible: %v", err)
	}

	modalHeader := page.GetByText("Upload an NZB file or paste a URL.")

	if err := addBtn.Click(); err != nil {
		t.Fatalf("click Add NZB second time: %v", err)
	}
	if err := fileInput.SetInputFiles(nzbPath); err != nil {
		t.Fatalf("SetInputFiles duplicate: %v", err)
	}
	if err := uploadBtn.Click(); err != nil {
		t.Fatalf("click Upload duplicate: %v", err)
	}

	if err := modalHeader.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateHidden,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("modal did not close after duplicate upload: %v", err)
	}

	if _, err := page.Reload(playwright.PageReloadOptions{
		WaitUntil: playwright.WaitUntilStateNetworkidle,
	}); err != nil {
		t.Fatalf("reload page after duplicate upload: %v", err)
	}

	dupWarning := page.GetByText("Duplicate NZBs found:")
	if err := dupWarning.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("duplicate warning banner not visible: %v", err)
	}

	if err := seededJob.Click(); err != nil {
		t.Fatalf("click seeded job row (testing pointer events): %v", err)
	}

	if err := page.GetByText("Par2 Recovery").First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("seeded job detail drawer did not expand: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}
