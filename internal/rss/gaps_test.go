package rss

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

// ---------- parseEnclosureLength ----------

func TestParseEnclosureLength_Normal(t *testing.T) {
	t.Parallel()
	got := parseEnclosureLength("12345678")
	if got != 12345678 {
		t.Errorf("got %d, want 12345678", got)
	}
}

func TestParseEnclosureLength_Empty(t *testing.T) {
	t.Parallel()
	got := parseEnclosureLength("")
	if got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestParseEnclosureLength_TrailingText(t *testing.T) {
	t.Parallel()
	// Should parse up to first non-digit.
	got := parseEnclosureLength("1024 bytes")
	if got != 1024 {
		t.Errorf("got %d, want 1024", got)
	}
}

func TestParseEnclosureLength_Overflow(t *testing.T) {
	t.Parallel()
	// Very large number should cap at MaxInt64.
	got := parseEnclosureLength("99999999999999999999999")
	if got != math.MaxInt64 {
		t.Errorf("got %d, want MaxInt64", got)
	}
}

func TestParseEnclosureLength_NonDigit(t *testing.T) {
	t.Parallel()
	got := parseEnclosureLength("abc")
	if got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

// ---------- normalizeItem ----------

func TestNormalizeItem_EnclosureURL(t *testing.T) {
	t.Parallel()
	fi := &gofeed.Item{
		Title: "Test",
		Link:  "http://example.com",
		GUID:  "guid-123",
		Enclosures: []*gofeed.Enclosure{
			{URL: "http://download.com/file.nzb", Length: "5000"},
		},
		PublishedParsed: new(time.Now()),
	}
	item := normalizeItem(fi)
	if item.URL != "http://download.com/file.nzb" {
		t.Errorf("URL = %q, want enclosure URL", item.URL)
	}
	if item.Size != 5000 {
		t.Errorf("Size = %d, want 5000", item.Size)
	}
	if item.ID != "guid-123" {
		t.Errorf("ID = %q, want guid-123", item.ID)
	}
}

func TestNormalizeItem_FallbackToLink(t *testing.T) {
	t.Parallel()
	fi := &gofeed.Item{
		Title:           "Test",
		Link:            "http://example.com/page",
		GUID:            "guid-456",
		PublishedParsed: new(time.Now()),
	}
	item := normalizeItem(fi)
	if item.URL != "http://example.com/page" {
		t.Errorf("URL = %q, want link fallback", item.URL)
	}
}

func TestNormalizeItem_NoGUID(t *testing.T) {
	t.Parallel()
	fi := &gofeed.Item{
		Title:           "Test",
		Link:            "http://example.com/link",
		PublishedParsed: new(time.Now()),
	}
	item := normalizeItem(fi)
	if item.ID != "http://example.com/link" {
		t.Errorf("ID should fall back to URL: %q", item.ID)
	}
}

func TestNormalizeItem_InfoURL(t *testing.T) {
	t.Parallel()
	fi := &gofeed.Item{
		Title:           "Test",
		Link:            "http://download.com/file.nzb",
		GUID:            "http://info.example.com/details",
		PublishedParsed: new(time.Now()),
	}
	item := normalizeItem(fi)
	if item.InfoURL != "http://info.example.com/details" {
		t.Errorf("InfoURL = %q, want GUID when it's an HTTP URL and differs from URL", item.InfoURL)
	}
}

func TestNormalizeItem_UpdatedFallback(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	fi := &gofeed.Item{
		Title:         "Test",
		Link:          "http://example.com",
		UpdatedParsed: &now,
	}
	item := normalizeItem(fi)
	if item.Published.IsZero() {
		t.Error("Published should use UpdatedParsed when PublishedParsed is nil")
	}
}

func TestNormalizeItem_NoDates(t *testing.T) {
	t.Parallel()
	fi := &gofeed.Item{
		Title: "Test",
		Link:  "http://example.com",
	}
	item := normalizeItem(fi)
	if item.Published.IsZero() {
		t.Error("Published should default to now when no dates available")
	}
}

func TestNormalizeItem_Category(t *testing.T) {
	t.Parallel()
	fi := &gofeed.Item{
		Title:           "Test",
		Link:            "http://example.com",
		Categories:      []string{"TV", "HD"},
		PublishedParsed: new(time.Now()),
	}
	item := normalizeItem(fi)
	if item.Category != "TV" {
		t.Errorf("Category = %q, want first category 'TV'", item.Category)
	}
}

func TestNormalizeItem_EmptyEnclosure(t *testing.T) {
	t.Parallel()
	fi := &gofeed.Item{
		Title: "Test",
		Link:  "http://example.com",
		Enclosures: []*gofeed.Enclosure{
			{URL: ""}, // empty URL should be skipped
		},
		PublishedParsed: new(time.Now()),
	}
	item := normalizeItem(fi)
	if item.URL != "http://example.com" {
		t.Errorf("URL = %q, want link fallback after empty enclosure", item.URL)
	}
}

// ---------- Store (dedup) ----------

func TestStore_SeenAndRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dedup.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	if s.Seen("item-1") {
		t.Error("should not be seen yet")
	}

	s.Record("item-1")
	if !s.Seen("item-1") {
		t.Error("should be seen after Record")
	}

	// Double record is no-op.
	s.Record("item-1")
}

func TestStore_SaveAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dedup.json")
	s, _ := OpenStore(path)

	s.Record("item-a")
	s.Record("item-b")

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload.
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore reload: %v", err)
	}
	if !s2.Seen("item-a") {
		t.Error("item-a should be seen after reload")
	}
	if !s2.Seen("item-b") {
		t.Error("item-b should be seen after reload")
	}
}

func TestStore_SaveNotDirty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dedup.json")
	s, _ := OpenStore(path)

	// Not dirty: save should be a no-op.
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("file created when nothing was dirty")
	}
}

func TestStore_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dedup.json")
	os.WriteFile(path, []byte("not json"), 0644)

	_, err := OpenStore(path)
	if err == nil {
		t.Error("expected error for corrupt JSON")
	}
}

func TestStore_Prune(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dedup.json")
	s, _ := OpenStore(path)

	// Add items: one "old" and one "new".
	s.Record("old-item")
	s.Record("new-item")

	// Manually backdate old-item.
	s.mu.Lock()
	s.seen["old-item"] = time.Now().UTC().Add(-48 * time.Hour)
	s.mu.Unlock()

	removed := s.Prune(24 * time.Hour)
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if s.Seen("old-item") {
		t.Error("old-item should have been pruned")
	}
	if !s.Seen("new-item") {
		t.Error("new-item should still be present")
	}
}

//go:fix inline
func timePtr(t time.Time) *time.Time { return new(t) }
