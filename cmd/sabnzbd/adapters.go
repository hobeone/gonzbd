// Adapters that bridge the Phase 7 subsystems to the queue and to each
// other. Kept in cmd/sabnzbd so the subsystems themselves stay
// dependency-free and reusable in isolation.
package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/nzb"
	"github.com/hobeone/sabnzbd-go/internal/queue"
	"github.com/hobeone/sabnzbd-go/internal/rss"
	"github.com/hobeone/sabnzbd-go/internal/scheduler"
	"github.com/hobeone/sabnzbd-go/internal/urlgrabber"
)

// ingestHandler satisfies both dirscanner.Handler and urlgrabber.Handler.
// It takes raw NZB bytes produced by either source and enqueues a job.
type ingestHandler struct {
	queue  *queue.Queue
	logger *slog.Logger
}

func (h *ingestHandler) HandleNZB(_ context.Context, filename string, data []byte) error {
	parsed, err := nzb.Parse(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("parse nzb %q: %w", filename, err)
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: filename})
	if err != nil {
		return fmt.Errorf("create job %q: %w", filename, err)
	}
	if err := h.queue.Add(job); err != nil {
		return fmt.Errorf("enqueue %q: %w", filename, err)
	}
	h.logger.Info("ingested nzb", "filename", filename, "files", len(job.Files), "bytes", job.TotalBytes)
	return nil
}

// rssToURLHandler turns accepted RSS items into URL-grabber fetches. It
// satisfies rss.Handler.
type rssToURLHandler struct {
	grabber *urlgrabber.Grabber
	logger  *slog.Logger
}

func (h *rssToURLHandler) HandleItem(ctx context.Context, item rss.Item, feed *rss.Feed) error {
	h.logger.Info("rss dispatch", "feed", feed.Name, "title", item.Title, "url", item.URL)
	_, err := h.grabber.Fetch(ctx, item.URL)
	return err
}

// schedulesFromConfig builds scheduler.ScheduleSpec values from the
// config.ScheduleConfig list. Python's ScheduleConfig has only minute/hour/dow
// fields, so the day-of-month and month fields are synthesized as "*".
// Disabled schedules are skipped.
func schedulesFromConfig(scs []config.ScheduleConfig) ([]scheduler.ScheduleSpec, error) {
	out := make([]scheduler.ScheduleSpec, 0, len(scs))
	for _, sc := range scs {
		if !sc.Enabled {
			continue
		}
		minute := fallback(sc.Minute, "*")
		hour := fallback(sc.Hour, "*")
		dow := fallback(sc.DayOfWeek, "*")
		line := fmt.Sprintf("%s %s * * %s %s", minute, hour, dow, sc.Action)
		if sc.Arguments != "" {
			line += " " + sc.Arguments
		}
		spec, err := scheduler.Parse(line)
		if err != nil {
			return nil, fmt.Errorf("schedule %q: %w", sc.Name, err)
		}
		out = append(out, spec)
	}
	return out, nil
}

// feedsFromConfig builds rss.Feed values from config.RSSFeedConfig, compiling
// the title regex once per filter at boot time rather than on every scan.
func feedsFromConfig(rcs []config.RSSFeedConfig) ([]rss.Feed, error) {
	out := make([]rss.Feed, 0, len(rcs))
	for _, rc := range rcs {
		filters, err := compileFilters(rc.Name, rc.Filters)
		if err != nil {
			return nil, err
		}
		out = append(out, rss.Feed{
			Name:    rc.Name,
			URL:     rc.URI,
			Enabled: rc.Enable,
			Filters: filters,
		})
	}
	return out, nil
}

func compileFilters(feedName string, fcs []config.RSSFilterConfig) ([]rss.Filter, error) {
	out := make([]rss.Filter, 0, len(fcs))
	for _, fc := range fcs {
		if !fc.Enabled || fc.Title == "" {
			continue
		}
		re, err := regexp.Compile(fc.Title)
		if err != nil {
			return nil, fmt.Errorf("feed %q filter %q: %w", feedName, fc.Name, err)
		}
		out = append(out, rss.Filter{
			Type:    filterTypeFromString(fc.Type),
			Pattern: re,
			Name:    fc.Name,
		})
	}
	return out, nil
}

func filterTypeFromString(t string) rss.FilterType {
	switch strings.ToLower(t) {
	case "require":
		return rss.RequireFilter
	case "must_not_match":
		return rss.ExcludeFilter
	case "ignore":
		return rss.IgnoreFilter
	default:
		return rss.IncludeFilter
	}
}

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
