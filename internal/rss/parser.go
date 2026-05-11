// Package rss fetches and processes RSS/Atom feeds, applying filter rules,
// deduplicating seen items, and delivering matched items to a pluggable handler.
package rss

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
)

// maxFeedSize caps the response body read from an RSS/Atom feed.
// 10 MiB is generous — real feeds are typically under 1 MiB.
const maxFeedSize = 10 * 1024 * 1024 // 10 MB

// Item is a normalized feed entry ready for filter evaluation.
type Item struct {
	// GUID or link used as the stable dedup key.
	ID string
	// Title is the entry's headline.
	Title string
	// URL is the download link (NZB, magnet, etc.).
	URL string
	// InfoURL is the human-readable page for the item, if distinct from URL.
	InfoURL string
	// Category is the feed-supplied category string (may be empty).
	Category string
	// Size is the reported byte size from the enclosure or description.
	Size int64
	// Published is the item publication timestamp in UTC.
	Published time.Time
}

// Parse fetches one feed URL and returns normalized Items.
// The caller-supplied client controls timeouts and transport options.
// The response body is capped at maxFeedSize to prevent OOM from
// maliciously large responses.
func Parse(ctx context.Context, url string, client *http.Client) ([]Item, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("rss: build request for %q: %w", url, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rss: fetch %q: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rss: fetch %q: HTTP %d", url, resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, maxFeedSize+1)
	fp := gofeed.NewParser()
	feed, err := fp.Parse(limited)
	if err != nil {
		return nil, fmt.Errorf("rss: parse %q: %w", url, err)
	}

	items := make([]Item, 0, len(feed.Items))
	for _, fi := range feed.Items {
		item := normalizeItem(fi)
		if item.URL == "" {
			continue
		}
		items = append(items, item)
	}
	return items, nil
}

// normalizeItem converts a gofeed.Item to our Item type.
func normalizeItem(fi *gofeed.Item) Item {
	item := Item{
		Title: stripHTMLTags(fi.Title),
	}

	// Prefer enclosure link for direct NZB/binary downloads.
	for _, enc := range fi.Enclosures {
		if enc.URL != "" {
			item.URL = sanitizeURL(enc.URL)
			if enc.Length != "" {
				item.Size = parseEnclosureLength(enc.Length)
			}
			break
		}
	}
	if item.URL == "" {
		item.URL = sanitizeURL(fi.Link)
	}

	// Use GUID as stable ID; fall back to link.
	if fi.GUID != "" {
		// If GUID looks like an HTTP URL keep it; otherwise strip tags.
		if isSafeURL(fi.GUID) {
			item.ID = fi.GUID
		} else {
			item.ID = stripHTMLTags(fi.GUID)
		}
	} else {
		item.ID = item.URL
	}

	// InfoURL is the human-facing page when the GUID looks like a URL and
	// differs from the download link.
	if fi.GUID != "" && fi.GUID != item.URL && isSafeURL(fi.GUID) {
		item.InfoURL = fi.GUID
	}

	switch {
	case fi.PublishedParsed != nil:
		item.Published = fi.PublishedParsed.UTC()
	case fi.UpdatedParsed != nil:
		item.Published = fi.UpdatedParsed.UTC()
	default:
		item.Published = time.Now().UTC()
	}

	if len(fi.Categories) > 0 {
		item.Category = fi.Categories[0]
	}

	return item
}

// htmlTagRe matches HTML/XML tags for stripping.
var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

// stripHTMLTags removes all HTML tags from s.
func stripHTMLTags(s string) string {
	return htmlTagRe.ReplaceAllString(s, "")
}

// isSafeURL returns true if u starts with http:// or https://.
func isSafeURL(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// sanitizeURL returns the URL if it uses a safe scheme (http/https),
// or empty string otherwise. This blocks javascript:, data:, file:///
// and other dangerous schemes from propagating to the web UI.
func sanitizeURL(u string) string {
	if isSafeURL(u) {
		return u
	}
	// Allow empty strings and relative URLs (no scheme).
	if u == "" || (!strings.Contains(u, ":") && !strings.HasPrefix(u, "//")) {
		return u
	}
	return ""
}

// parseEnclosureLength converts an enclosure length string to int64.
// Caps at math.MaxInt64 to prevent overflow from maliciously long digit strings.
func parseEnclosureLength(s string) int64 {
	var n int64
	for _, c := range s {
		if c >= '0' && c <= '9' {
			if n > math.MaxInt64/10 || (n == math.MaxInt64/10 && c > '7') {
				return math.MaxInt64 // cap to prevent overflow
			}
			n = n*10 + int64(c-'0')
		} else {
			break
		}
	}
	return n
}
