package rss

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
