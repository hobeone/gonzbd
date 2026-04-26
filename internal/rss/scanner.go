package rss

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Feed describes one RSS/Atom source and the rules to apply to its items.
type Feed struct {
	// Name is a human-readable label (must be unique within a Scanner).
	Name string
	// URL is the feed endpoint.
	URL string
	// Enabled controls whether the feed is polled during each scan pass.
	Enabled bool
	// Filters is the ordered rule chain applied to each item.
	Filters []Filter
	// MinBytes / MaxBytes bound accepted item sizes; 0 means no limit.
	MinBytes int64
	MaxBytes int64
	// MaxAge rejects items older than this duration; 0 means no limit.
	MaxAge time.Duration
}

// Handler receives items that passed all filter/dedup checks.
type Handler interface {
	HandleItem(ctx context.Context, item Item, feed *Feed) error
}

// Scanner polls a set of Feeds on a fixed interval and dispatches new items
// to a Handler.
type Scanner struct {
	feeds   []Feed
	store   *Store
	handler Handler
	client  *http.Client
	logger  *slog.Logger
}

// NewScanner constructs a Scanner. If client is nil, http.DefaultClient is used.
// If logger is nil, slog.Default() is used.
func NewScanner(feeds []Feed, store *Store, handler Handler, client *http.Client, logger *slog.Logger) *Scanner {
	if client == nil {
		client = http.DefaultClient
	}
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("component", "rss")
	return &Scanner{
		feeds:   feeds,
		store:   store,
		handler: handler,
		client:  client,
		logger:  log,
	}
}

// Run blocks, scanning all enabled feeds every interval until ctx is cancelled.
func (sc *Scanner) Run(ctx context.Context, interval time.Duration) error {
	if err := sc.ScanOnce(ctx); err != nil && ctx.Err() == nil {
		sc.logger.Error("rss: initial scan failed", slog.Any("err", err))
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := sc.ScanOnce(ctx); err != nil && ctx.Err() == nil {
				sc.logger.Error("rss: scan failed", slog.Any("err", err))
			}
		}
	}
}

// ScanOnce performs one full pass over all enabled feeds. Errors for individual
// feeds are logged and skipped; the method returns an error only for fatal
// conditions that affect the whole scan (e.g. store persistence failure).
func (sc *Scanner) ScanOnce(ctx context.Context) error {
	for i := range sc.feeds {
		feed := &sc.feeds[i]
		if !feed.Enabled {
			continue
		}
		if err := sc.scanFeed(ctx, feed); err != nil {
			sc.logger.Error("rss: feed scan error",
				slog.String("feed", feed.Name),
				slog.Any("err", err),
			)
		}
	}

	// Prune entries older than 30 days to prevent unbounded growth.
	if n := sc.store.Prune(30 * 24 * time.Hour); n > 0 {
		sc.logger.Info("rss: pruned old dedup entries", slog.Int("count", n))
	}

	if err := sc.store.Save(); err != nil {
		return fmt.Errorf("rss: save dedup store: %w", err)
	}
	return nil
}

// scanFeed fetches and processes one feed.
func (sc *Scanner) scanFeed(ctx context.Context, feed *Feed) error {
	items, err := Parse(ctx, feed.URL, sc.client)
	if err != nil {
		return err
	}

	filtered := Apply(items, feed.Filters, feed.MinBytes, feed.MaxBytes, feed.MaxAge)

	for _, item := range filtered {
		if sc.store.Seen(item.ID) {
			sc.logger.Debug("rss: skipping seen item",
				slog.String("feed", feed.Name),
				slog.String("id", item.ID),
				slog.String("title", item.Title),
			)
			continue
		}

		if err := sc.handler.HandleItem(ctx, item, feed); err != nil {
			sc.logger.Error("rss: handler error",
				slog.String("feed", feed.Name),
				slog.String("title", item.Title),
				slog.Any("err", err),
			)
			continue
		}

		sc.store.Record(item.ID)
		sc.logger.Info("rss: dispatched item",
			slog.String("feed", feed.Name),
			slog.String("title", item.Title),
		)
	}

	return nil
}
