//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/rss"
)

// collectHandler records all items dispatched to it; safe for concurrent use.
type collectHandler struct {
	mu    sync.Mutex
	items []rss.Item
}

// HandleItem implements rss.Handler.
func (h *collectHandler) HandleItem(_ context.Context, item rss.Item, _ *rss.Feed) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.items = append(h.items, item)
	return nil
}

// collected returns a snapshot of all dispatched items.
func (h *collectHandler) collected() []rss.Item {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]rss.Item, len(h.items))
	copy(out, h.items)
	return out
}

// staticRSSFeed builds an httptest.Server that serves a static RSS 2.0 feed
// containing the given items (each described by a title, URL, and byte size).
func staticRSSFeed(t *testing.T, items []rssItemDef) *httptest.Server {
	t.Helper()
	feed := buildRSSXML(items)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write(feed) //nolint:errcheck // test handler
	}))
	t.Cleanup(ts.Close)
	return ts
}

// rssItemDef carries the test-authoring fields for one synthetic RSS item.
type rssItemDef struct {
	Title   string
	URL     string
	Bytes   int64
	PubDate time.Time
}

// buildRSSXML produces minimal but valid RSS 2.0 XML.
func buildRSSXML(items []rssItemDef) []byte {
	var s string
	s += `<?xml version="1.0" encoding="UTF-8"?>` + "\n"
	s += `<rss version="2.0"><channel>` + "\n"
	s += "<title>Integration Test Feed</title>\n"
	s += "<link>http://example.com</link>\n"
	s += "<description>test</description>\n"

	for _, item := range items {
		pub := item.PubDate
		if pub.IsZero() {
			pub = time.Now()
		}
		s += "<item>\n"
		s += fmt.Sprintf("  <title>%s</title>\n", item.Title)
		s += fmt.Sprintf("  <link>%s</link>\n", item.URL)
		s += fmt.Sprintf("  <guid>%s</guid>\n", item.URL)
		s += fmt.Sprintf("  <enclosure url=%q type=\"application/x-nzb\" length=%q/>\n",
			item.URL, fmt.Sprintf("%d", item.Bytes))
		s += fmt.Sprintf("  <pubDate>%s</pubDate>\n", pub.UTC().Format(time.RFC1123Z))
		s += "</item>\n"
	}

	s += "</channel></rss>\n"
	return []byte(s)
}

// mustCompileRSS compiles a regexp for filter use; fatals on error.
func mustCompileRSS(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("compile pattern %q: %v", pattern, err)
	}
	return re
}

// openIntegrationStore opens a dedup Store under t.TempDir().
func openIntegrationStore(t *testing.T) *rss.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dedup.json")
	store, err := rss.OpenStore(path)
	if err != nil {
		t.Fatalf("rss.OpenStore: %v", err)
	}
	return store
}

// TestRSS_IncludeExcludeFilters verifies that include and exclude filters
// are applied correctly.
func TestRSS_IncludeExcludeFilters(t *testing.T) {
	t.Parallel()

	items := []rssItemDef{
		{Title: "Linux.iso.nzb", URL: "http://example.com/linux", Bytes: 1024},
		{Title: "Windows.exe.nzb", URL: "http://example.com/windows", Bytes: 1024},
		{Title: "MacOS.dmg.nzb", URL: "http://example.com/macos", Bytes: 1024},
	}
	ts := staticRSSFeed(t, items)

	handler := &collectHandler{}
	store := openIntegrationStore(t)

	feed := rss.Feed{
		Name:    "test",
		URL:     ts.URL,
		Enabled: true,
		Filters: []rss.Filter{
			{Type: rss.IncludeFilter, Pattern: mustCompileRSS(t, `(?i)linux`), Name: "linux only"},
			{Type: rss.ExcludeFilter, Pattern: mustCompileRSS(t, `(?i)windows`), Name: "no windows"},
		},
	}

	scanner := rss.NewScanner([]rss.Feed{feed}, store, handler, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := scanner.ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}

	got := handler.collected()
	// Only Linux should have passed the include filter.
	if len(got) != 1 {
		t.Errorf("dispatched %d items; want 1", len(got))
		for _, item := range got {
			t.Logf("  dispatched: %q", item.Title)
		}
		return
	}
	if got[0].Title != "Linux.iso.nzb" {
		t.Errorf("dispatched item = %q; want %q", got[0].Title, "Linux.iso.nzb")
	}
}

// TestRSS_ExcludeFilter verifies that exclude-only filter chains drop matching
// items and pass everything else.
func TestRSS_ExcludeFilter(t *testing.T) {
	t.Parallel()

	items := []rssItemDef{
		{Title: "Good.Item.nzb", URL: "http://example.com/good", Bytes: 1024},
		{Title: "Spam.Item.nzb", URL: "http://example.com/spam", Bytes: 1024},
	}
	ts := staticRSSFeed(t, items)

	handler := &collectHandler{}
	store := openIntegrationStore(t)

	feed := rss.Feed{
		Name:    "test",
		URL:     ts.URL,
		Enabled: true,
		Filters: []rss.Filter{
			{Type: rss.ExcludeFilter, Pattern: mustCompileRSS(t, `(?i)spam`), Name: "no spam"},
		},
	}

	scanner := rss.NewScanner([]rss.Feed{feed}, store, handler, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := scanner.ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}

	got := handler.collected()
	if len(got) != 1 {
		t.Errorf("dispatched %d items; want 1", len(got))
		return
	}
	if got[0].Title != "Good.Item.nzb" {
		t.Errorf("dispatched item = %q; want %q", got[0].Title, "Good.Item.nzb")
	}
}

// TestRSS_SizeBounds verifies that items outside MinBytes/MaxBytes are dropped.
func TestRSS_SizeBounds(t *testing.T) {
	t.Parallel()

	items := []rssItemDef{
		{Title: "TooSmall.nzb", URL: "http://example.com/small", Bytes: 100},
		{Title: "JustRight.nzb", URL: "http://example.com/right", Bytes: 5000},
		{Title: "TooBig.nzb", URL: "http://example.com/big", Bytes: 100000},
	}
	ts := staticRSSFeed(t, items)

	handler := &collectHandler{}
	store := openIntegrationStore(t)

	feed := rss.Feed{
		Name:     "test",
		URL:      ts.URL,
		Enabled:  true,
		MinBytes: 1000,
		MaxBytes: 10000,
	}

	scanner := rss.NewScanner([]rss.Feed{feed}, store, handler, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := scanner.ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}

	got := handler.collected()
	if len(got) != 1 {
		t.Errorf("dispatched %d items; want 1", len(got))
		return
	}
	if got[0].Title != "JustRight.nzb" {
		t.Errorf("dispatched item = %q; want JustRight.nzb", got[0].Title)
	}
}

// TestRSS_Dedup verifies that re-running ScanOnce on the same feed does not
// re-dispatch items that were already seen.
func TestRSS_Dedup(t *testing.T) {
	t.Parallel()

	items := []rssItemDef{
		{Title: "Unique.Item.nzb", URL: "http://example.com/unique", Bytes: 1024},
	}
	ts := staticRSSFeed(t, items)

	handler := &collectHandler{}
	store := openIntegrationStore(t)

	feed := rss.Feed{
		Name:    "test",
		URL:     ts.URL,
		Enabled: true,
	}

	scanner := rss.NewScanner([]rss.Feed{feed}, store, handler, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First scan — item should be dispatched once.
	if err := scanner.ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce (first): %v", err)
	}
	if len(handler.collected()) != 1 {
		t.Fatalf("first scan: dispatched %d items; want 1", len(handler.collected()))
	}

	// Second scan — same feed, same store. Item should NOT be dispatched again.
	if err := scanner.ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce (second): %v", err)
	}
	if len(handler.collected()) != 1 {
		t.Errorf("second scan: dispatched %d total items; want 1 (dedup failed)", len(handler.collected()))
	}
}

// TestRSS_DisabledFeed verifies that a feed with Enabled=false is never polled.
func TestRSS_DisabledFeed(t *testing.T) {
	t.Parallel()

	items := []rssItemDef{
		{Title: "Should.Not.Appear.nzb", URL: "http://example.com/notappear", Bytes: 1024},
	}
	ts := staticRSSFeed(t, items)

	handler := &collectHandler{}
	store := openIntegrationStore(t)

	feed := rss.Feed{
		Name:    "disabled-feed",
		URL:     ts.URL,
		Enabled: false,
	}

	scanner := rss.NewScanner([]rss.Feed{feed}, store, handler, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := scanner.ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}

	if len(handler.collected()) != 0 {
		t.Errorf("disabled feed dispatched %d items; want 0", len(handler.collected()))
	}
}

// TestRSS_MaxAge verifies that items older than MaxAge are filtered out.
func TestRSS_MaxAge(t *testing.T) {
	t.Parallel()

	now := time.Now()
	items := []rssItemDef{
		{Title: "Fresh.nzb", URL: "http://example.com/fresh", Bytes: 1024, PubDate: now.Add(-1 * time.Hour)},
		{Title: "Old.nzb", URL: "http://example.com/old", Bytes: 1024, PubDate: now.Add(-48 * time.Hour)},
	}
	ts := staticRSSFeed(t, items)

	handler := &collectHandler{}
	store := openIntegrationStore(t)

	feed := rss.Feed{
		Name:    "test",
		URL:     ts.URL,
		Enabled: true,
		MaxAge:  24 * time.Hour,
	}

	scanner := rss.NewScanner([]rss.Feed{feed}, store, handler, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := scanner.ScanOnce(ctx); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}

	got := handler.collected()
	if len(got) != 1 {
		t.Errorf("dispatched %d items; want 1 (only Fresh.nzb)", len(got))
		return
	}
	if got[0].Title != "Fresh.nzb" {
		t.Errorf("dispatched item = %q; want Fresh.nzb", got[0].Title)
	}
}

// TODO: Test the urlgrabber.Grabber → queue integration path (RSS item URL →
// Grabber.Fetch → adds job to queue). Deferred because it requires wiring a
// full app.Application instance to the grabber, which substantially increases
// test complexity without testing different code from TestDownload_*. The
// composition is tested by TestAPI_AddURL; the RSS→grabber adapter is tested
// by cmd/sabnzbd/adapters.go at the integration level when running the daemon.
