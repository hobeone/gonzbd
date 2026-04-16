// Package rss fetches and processes RSS/Atom feeds, applying filter rules,
// deduplicating seen items, and delivering matched items to a pluggable handler.
package rss

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/mmcdole/gofeed"
)

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
func Parse(ctx context.Context, url string, client *http.Client) ([]Item, error) {
	fp := gofeed.NewParser()
	fp.Client = client

	feed, err := fp.ParseURLWithContext(url, ctx)
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
		Title: fi.Title,
	}

	// Prefer enclosure link for direct NZB/binary downloads.
	for _, enc := range fi.Enclosures {
		if enc.URL != "" {
			item.URL = enc.URL
			if enc.Length != "" {
				item.Size = parseEnclosureLength(enc.Length)
			}
			break
		}
	}
	if item.URL == "" {
		item.URL = fi.Link
	}

	// Use GUID as stable ID; fall back to link.
	if fi.GUID != "" {
		item.ID = fi.GUID
	} else {
		item.ID = item.URL
	}

	// InfoURL is the human-facing page when the GUID looks like a URL and
	// differs from the download link.
	if fi.GUID != "" && fi.GUID != item.URL && len(fi.GUID) > 4 && fi.GUID[:4] == "http" {
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

// parseEnclosureLength converts an enclosure length string to int64.
func parseEnclosureLength(s string) int64 {
	var n int64
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int64(c-'0')
		} else {
			break
		}
	}
	return n
}
