package rss

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"
)

// ----------------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------------

func mustCompile(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("compile pattern %q: %v", pattern, err)
	}
	return re
}

func makeItem(title string, size int64, published time.Time) Item {
	return Item{
		ID:        title,
		Title:     title,
		URL:       "https://example.com/nzb/" + title,
		Size:      size,
		Published: published,
	}
}

// collectHandler records all items dispatched to it.
type collectHandler struct {
	mu    sync.Mutex
	items []Item
}

func (h *collectHandler) HandleItem(_ context.Context, item Item, _ *Feed) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.items = append(h.items, item)
	return nil
}

func (h *collectHandler) collected() []Item {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Item, len(h.items))
	copy(out, h.items)
	return out
}

// tempStore returns a Store backed by a temp file that is cleaned up when t ends.
func tempStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "dedup.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return s
}

// ----------------------------------------------------------------------------
// Filter tests
// ----------------------------------------------------------------------------

func TestFilterInclude(t *testing.T) {
	t.Parallel()
	f := Filter{Type: IncludeFilter, Pattern: mustCompile(t, `(?i)linux`)}
	tests := []struct {
		title string
		want  bool
	}{
		{"Ubuntu Linux 22.04 ISO", true},
		{"Windows 11 Pro", false},
		{"Arch LINUX minimal", true},
	}
	for _, tc := range tests {
		got := f.Match(makeItem(tc.title, 0, time.Now()))
		if got != tc.want {
			t.Errorf("IncludeFilter.Match(%q) = %v, want %v", tc.title, got, tc.want)
		}
	}
}

func TestFilterExclude(t *testing.T) {
	t.Parallel()
	f := Filter{Type: ExcludeFilter, Pattern: mustCompile(t, `(?i)sample`)}
	items := []Item{
		makeItem("Show.S01E01.HDTV", 100, time.Now()),
		makeItem("Show.S01E01.HDTV.Sample", 10, time.Now()),
		makeItem("Movie.2024.SAMPLE.mkv", 20, time.Now()),
	}
	got := Apply(items, []Filter{f}, 0, 0, 0)
	if len(got) != 1 {
		t.Fatalf("expected 1 item after exclude, got %d", len(got))
	}
	if got[0].Title != "Show.S01E01.HDTV" {
		t.Errorf("unexpected surviving item: %q", got[0].Title)
	}
}

func TestFilterRequire(t *testing.T) {
	t.Parallel()
	f := Filter{Type: RequireFilter, Pattern: mustCompile(t, `(?i)1080p`)}
	items := []Item{
		makeItem("Show.S02E01.1080p.BluRay", 4000, time.Now()),
		makeItem("Show.S02E01.720p.BluRay", 2000, time.Now()),
		makeItem("Show.S02E01.480p.HDTV", 1000, time.Now()),
	}
	got := Apply(items, []Filter{f}, 0, 0, 0)
	if len(got) != 1 {
		t.Fatalf("expected 1 item after require, got %d", len(got))
	}
	if got[0].Title != "Show.S02E01.1080p.BluRay" {
		t.Errorf("unexpected surviving item: %q", got[0].Title)
	}
}

func TestFilterIgnore(t *testing.T) {
	t.Parallel()
	f := Filter{Type: IgnoreFilter, Pattern: mustCompile(t, `(?i)\bCAM\b`)}
	items := []Item{
		makeItem("Movie.2024.WEB-DL.1080p", 4000, time.Now()),
		makeItem("Movie.2024.CAM.RARBG", 700, time.Now()),
	}
	got := Apply(items, []Filter{f}, 0, 0, 0)
	if len(got) != 1 {
		t.Fatalf("expected 1 item after ignore, got %d", len(got))
	}
	if got[0].Title != "Movie.2024.WEB-DL.1080p" {
		t.Errorf("unexpected surviving item: %q", got[0].Title)
	}
}

func TestFilterNilPattern(t *testing.T) {
	t.Parallel()
	f := Filter{Type: IncludeFilter, Pattern: nil}
	item := makeItem("Anything", 0, time.Now())
	if f.Match(item) {
		t.Error("nil pattern should never match")
	}
}

// ----------------------------------------------------------------------------
// Size filter tests
// ----------------------------------------------------------------------------

func TestFilterSize(t *testing.T) {
	t.Parallel()
	now := time.Now()
	items := []Item{
		makeItem("tiny", 100, now),
		makeItem("medium", 500*1024*1024, now), // 500 MB
		makeItem("large", 5000*1024*1024, now), // 5 GB
		makeItem("nosize", 0, now),             // size unknown → always passes
	}

	tests := []struct {
		name       string
		minBytes   int64
		maxBytes   int64
		wantTitles []string
	}{
		{
			name:       "min only",
			minBytes:   200 * 1024 * 1024,
			maxBytes:   0,
			wantTitles: []string{"medium", "large", "nosize"},
		},
		{
			name:       "max only",
			minBytes:   0,
			maxBytes:   1024 * 1024 * 1024, // 1 GB
			wantTitles: []string{"tiny", "medium", "nosize"},
		},
		{
			name:       "min and max",
			minBytes:   200 * 1024 * 1024,
			maxBytes:   1024 * 1024 * 1024,
			wantTitles: []string{"medium", "nosize"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Apply(items, nil, tc.minBytes, tc.maxBytes, 0)
			if len(got) != len(tc.wantTitles) {
				t.Fatalf("want %d items, got %d", len(tc.wantTitles), len(got))
			}
			for i, want := range tc.wantTitles {
				if got[i].Title != want {
					t.Errorf("item[%d]: want %q, got %q", i, want, got[i].Title)
				}
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Age filter tests
// ----------------------------------------------------------------------------

func TestFilterAge(t *testing.T) {
	t.Parallel()
	now := time.Now()
	items := []Item{
		makeItem("fresh", 0, now.Add(-1*time.Hour)),
		makeItem("dayOld", 0, now.Add(-25*time.Hour)),
		makeItem("weekOld", 0, now.Add(-8*24*time.Hour)),
	}

	got := Apply(items, nil, 0, 0, 24*time.Hour)
	if len(got) != 1 {
		t.Fatalf("want 1 fresh item, got %d", len(got))
	}
	if got[0].Title != "fresh" {
		t.Errorf("unexpected item: %q", got[0].Title)
	}
}

// ----------------------------------------------------------------------------
// Dedup tests
// ----------------------------------------------------------------------------

func TestDedupSeenRecord(t *testing.T) {
	t.Parallel()
	s := tempStore(t)

	if s.Seen("abc") {
		t.Fatal("new id should not be seen")
	}
	s.Record("abc")
	if !s.Seen("abc") {
		t.Fatal("recorded id must be seen")
	}
	// Second Record is a no-op (no panic, still seen).
	s.Record("abc")
	if !s.Seen("abc") {
		t.Fatal("still seen after double record")
	}
}

func TestDedupPersistence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "dedup.json")

	s1, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	s1.Record("id-1")
	s1.Record("id-2")
	if err = s1.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore reload: %v", err)
	}
	if !s2.Seen("id-1") || !s2.Seen("id-2") {
		t.Error("reloaded store missing ids")
	}
}

func TestDedupPrune(t *testing.T) {
	t.Parallel()
	s := tempStore(t)

	s.Record("new-item")
	// Manually backdate one entry.
	s.mu.Lock()
	s.seen["old-item"] = time.Now().Add(-31 * 24 * time.Hour)
	s.mu.Unlock()

	removed := s.Prune(30 * 24 * time.Hour)
	if removed != 1 {
		t.Fatalf("want 1 pruned, got %d", removed)
	}
	if s.Seen("old-item") {
		t.Error("old-item should be gone after prune")
	}
	if !s.Seen("new-item") {
		t.Error("new-item should survive prune")
	}
}

func TestDedupOpenNonExistentFile(t *testing.T) {
	t.Parallel()
	s, err := OpenStore("/tmp/rss_nonexistent_xyz.json")
	if err != nil {
		t.Fatalf("should succeed on missing file: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil store")
	}
}

func TestDedupOpenBadJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := OpenStore(path)
	if err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

// ----------------------------------------------------------------------------
// Scanner.ScanOnce integration test
// ----------------------------------------------------------------------------

// testRSSFeed returns a canned RSS feed with three items; pubDates are set to
// now so age-based filters never reject them.
func testRSSFeed() string {
	now := time.Now().UTC().Format(time.RFC1123Z)
	return `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Feed</title>
    <link>https://example.com</link>
    <item>
      <title>Show.S01E01.1080p.BluRay</title>
      <guid>guid-1001</guid>
      <link>https://example.com/nzb/1001</link>
      <pubDate>` + now + `</pubDate>
    </item>
    <item>
      <title>Show.S01E02.720p.HDTV</title>
      <guid>guid-1002</guid>
      <link>https://example.com/nzb/1002</link>
      <pubDate>` + now + `</pubDate>
    </item>
    <item>
      <title>Show.S01E03.CAM.RARBG</title>
      <guid>guid-1003</guid>
      <link>https://example.com/nzb/1003</link>
      <pubDate>` + now + `</pubDate>
    </item>
  </channel>
</rss>`
}

func TestScannerScanOnce(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(testRSSFeed()))
	}))
	defer srv.Close()

	store := tempStore(t)
	handler := &collectHandler{}

	feeds := []Feed{
		{
			Name:    "test",
			URL:     srv.URL,
			Enabled: true,
			// Exclude CAM releases.
			Filters: []Filter{
				{Type: ExcludeFilter, Pattern: mustCompile(t, `(?i)\bCAM\b`)},
			},
		},
	}

	sc := NewScanner(feeds, store, handler, srv.Client(), nil)
	if err := sc.ScanOnce(context.Background()); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}

	items := handler.collected()
	if len(items) != 2 {
		t.Fatalf("want 2 dispatched items, got %d", len(items))
	}
	// Both non-CAM items should be present.
	titles := map[string]bool{items[0].Title: true, items[1].Title: true}
	if !titles["Show.S01E01.1080p.BluRay"] || !titles["Show.S01E02.720p.HDTV"] {
		t.Errorf("unexpected titles: %v", titles)
	}

	// Second scan — all items already seen, handler must not be called again.
	handler2 := &collectHandler{}
	sc2 := NewScanner(feeds, store, handler2, srv.Client(), nil)
	if err := sc2.ScanOnce(context.Background()); err != nil {
		t.Fatalf("ScanOnce (2nd): %v", err)
	}
	if got := handler2.collected(); len(got) != 0 {
		t.Fatalf("second scan should dispatch 0 items, got %d", len(got))
	}
}

func TestScannerDisabledFeed(t *testing.T) {
	t.Parallel()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(testRSSFeed()))
	}))
	defer srv.Close()

	store := tempStore(t)
	handler := &collectHandler{}
	feeds := []Feed{{Name: "disabled", URL: srv.URL, Enabled: false}}

	sc := NewScanner(feeds, store, handler, srv.Client(), nil)
	if err := sc.ScanOnce(context.Background()); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if called {
		t.Error("disabled feed should not be fetched")
	}
	if n := len(handler.collected()); n != 0 {
		t.Errorf("disabled feed should dispatch 0 items, got %d", n)
	}
}

func TestScannerContextCancel(t *testing.T) {
	t.Parallel()
	store := tempStore(t)
	handler := &collectHandler{}
	feeds := []Feed{{Name: "empty", URL: "http://127.0.0.1:0/nonexistent", Enabled: true}}

	sc := NewScanner(feeds, store, handler, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := sc.Run(ctx, 100*time.Millisecond)
	if err == nil {
		t.Error("Run should return context error")
	}
}
