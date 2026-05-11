package rss

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mmcdole/gofeed"
)

// TestParse_HugeFeed verifies that a feed response larger than maxFeedSize
// causes a parse failure rather than consuming unbounded memory (C6).
func TestParse_HugeFeed(t *testing.T) {
	t.Parallel()

	// Serve a response that exceeds maxFeedSize. We write a valid RSS
	// prolog followed by a huge blob of junk data that pushes the body
	// well past the limit.
	const prolog = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Huge</title><description>`
	const epilog = `</description></channel></rss>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(prolog))
		// Write junk data exceeding maxFeedSize.
		chunk := strings.Repeat("X", 64*1024)
		written := len(prolog)
		for written < maxFeedSize+1024 {
			_, _ = w.Write([]byte(chunk))
			written += len(chunk)
		}
		_, _ = w.Write([]byte(epilog))
	}))
	defer srv.Close()

	_, err := Parse(context.Background(), srv.URL, srv.Client())
	if err == nil {
		t.Fatal("expected error for feed exceeding maxFeedSize")
	}
	// The error should come from the XML parser hitting an unexpected EOF
	// on the limited reader, or from gofeed failing to parse the truncated body.
	t.Logf("got expected error: %v", err)
}

// TestParse_NormalFeed ensures the size limit does not break normal feeds.
func TestParse_NormalFeed(t *testing.T) {
	t.Parallel()

	feed := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Normal</title>
    <item>
      <title>Test Item</title>
      <link>https://example.com/test.nzb</link>
      <guid>guid-1</guid>
    </item>
  </channel>
</rss>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(feed))
	}))
	defer srv.Close()

	items, err := Parse(context.Background(), srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}
	if items[0].Title != "Test Item" {
		t.Errorf("Title = %q, want %q", items[0].Title, "Test Item")
	}
}

// TestNormalizeItem_XSSInTitle verifies that HTML tags (including script tags)
// are stripped from feed item titles to prevent XSS.
func TestNormalizeItem_XSSInTitle(t *testing.T) {
	t.Parallel()

	fi := &gofeed.Item{
		Title: `<script>alert(1)</script>Safe Title<b>bold</b>`,
		Link:  "https://example.com/test.nzb",
		GUID:  "guid-1",
	}
	item := normalizeItem(fi)
	if item.Title != "alert(1)Safe Titlebold" {
		t.Errorf("Title = %q, want %q", item.Title, "alert(1)Safe Titlebold")
	}
}

// TestNormalizeItem_DangerousURLSchemes verifies that javascript:, data:, and
// file:/// URLs are rejected (set to empty) while http/https pass through.
func TestNormalizeItem_DangerousURLSchemes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		link    string
		wantURL string
	}{
		{"javascript", "javascript:alert(1)", ""},
		{"data", "data:text/html,<script>alert(1)</script>", ""},
		{"file", "file:///etc/passwd", ""},
		{"http", "http://example.com/test.nzb", "http://example.com/test.nzb"},
		{"https", "https://example.com/test.nzb", "https://example.com/test.nzb"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fi := &gofeed.Item{
				Title: "Test",
				Link:  tc.link,
				GUID:  "guid-" + tc.name,
			}
			item := normalizeItem(fi)
			if item.URL != tc.wantURL {
				t.Errorf("URL = %q, want %q", item.URL, tc.wantURL)
			}
		})
	}
}

// TestNormalizeItem_NormalItem verifies that a normal item without XSS
// payloads passes through correctly with all fields populated.
func TestNormalizeItem_NormalItem(t *testing.T) {
	t.Parallel()

	fi := &gofeed.Item{
		Title: "My.Show.S01E01.1080p.WEB-DL",
		Link:  "https://example.com/test.nzb",
		GUID:  "https://example.com/details/12345",
	}
	item := normalizeItem(fi)
	if item.Title != "My.Show.S01E01.1080p.WEB-DL" {
		t.Errorf("Title = %q", item.Title)
	}
	if item.URL != "https://example.com/test.nzb" {
		t.Errorf("URL = %q", item.URL)
	}
	if item.ID != "https://example.com/details/12345" {
		t.Errorf("ID = %q", item.ID)
	}
	// GUID is a different HTTP URL, so InfoURL should be set.
	if item.InfoURL != "https://example.com/details/12345" {
		t.Errorf("InfoURL = %q", item.InfoURL)
	}
}
