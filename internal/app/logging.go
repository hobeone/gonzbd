package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lmittmann/tint"
)

// LoggingOptions configures structured logging behavior.
type LoggingOptions struct {
	// Level is the global minimum log level (Debug, Info, Warn, Error).
	Level slog.Level
	// LogFile is the path to the log file. Empty means stderr only.
	LogFile string
	// AddSource adds file:line annotations to each log record.
	AddSource bool

	// ComponentLevels overrides Level for specific components.
	// Keys are component names (e.g., "api", "downloader").
	// Values are slog.Level values. Components not listed inherit Level.
	// Use config.LevelOff to completely silence a component.
	ComponentLevels map[string]slog.Level
}

// Setup returns a configured *slog.Logger that writes to stderr and
// optionally to a log file. The logger is also installed as slog.Default
// so that package-level slog.* calls work.
//
// The returned io.Closer is the log file handle (nil if LogFile is empty).
// Caller must call Close on it during shutdown to flush and close the file.
//
// If LogFile is non-empty, Setup creates the parent directory with mode
// 0o750 and opens the file with O_APPEND|O_CREATE|O_WRONLY, mode 0o640.
func Setup(opts LoggingOptions) (*slog.Logger, io.Closer, error) {
	var closer io.Closer
	var handlers []slog.Handler

	// When per-component levels are set, the base handlers must accept
	// the lowest configured level so that the filterHandler can do
	// fine-grained suppression. Without this, a component override like
	// "downloader: debug" would be blocked by a global "info" handler.
	handlerLevel := opts.Level
	for _, lvl := range opts.ComponentLevels {
		if lvl < handlerLevel {
			handlerLevel = lvl
		}
	}

	// 1. Console handler (Colorized)
	handlers = append(handlers, tint.NewHandler(os.Stderr, &tint.Options{
		Level:      handlerLevel,
		AddSource:  opts.AddSource,
		TimeFormat: time.TimeOnly,
	}))

	// 2. File handler (Plain text)
	if opts.LogFile != "" {
		if err := os.MkdirAll(filepath.Dir(opts.LogFile), 0o750); err != nil {
			return nil, nil, fmt.Errorf("create log directory: %w", err)
		}

		f, err := os.OpenFile(opts.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640) //nolint:gosec // G302: log file mode is intentionally group-readable
		if err != nil {
			return nil, nil, fmt.Errorf("open log file %s: %w", opts.LogFile, err)
		}
		closer = f

		handlers = append(handlers, slog.NewTextHandler(f, &slog.HandlerOptions{
			Level:     handlerLevel,
			AddSource: opts.AddSource,
		}))
	}

	// 3. Combine and wrap with per-component filtering
	var h slog.Handler
	if len(handlers) == 1 {
		h = handlers[0]
	} else {
		h = &multiHandler{handlers: handlers}
	}

	if len(opts.ComponentLevels) > 0 {
		h = &filterHandler{
			next:   h,
			global: opts.Level,
			levels: opts.ComponentLevels,
		}
	}

	logger := slog.New(h)

	// Install as the default logger for package-level slog.* calls.
	slog.SetDefault(logger)

	return logger, closer, nil
}

// multiHandler broadcasts log records to multiple handlers.
type multiHandler struct {
	handlers []slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: next}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: next}
}

// filterHandler applies per-component log level overrides.
// Records from a component with an entry in the levels map must meet
// that level to be emitted. Records from unlisted components (or with
// no component attribute) pass through to the next handler unchanged
// (subject to its own level check).
type filterHandler struct {
	next   slog.Handler
	global slog.Level
	levels map[string]slog.Level

	// currentAttrs holds the attributes added via WithAttrs.
	currentAttrs []slog.Attr
}

func (f *filterHandler) Enabled(ctx context.Context, level slog.Level) bool {
	// We cannot check the component here (no access to attributes),
	// so be permissive and let Handle do the filtering.
	return f.next.Enabled(ctx, level)
}

func (f *filterHandler) Handle(ctx context.Context, r slog.Record) error {
	component := f.extractComponent(r)

	// Determine the effective level. Check hierarchical matches:
	// "postproc/unpack" -> "postproc/unpack", then "postproc".
	effectiveLevel := f.global
	p := component
	for p != "" {
		if lvl, ok := f.levels[p]; ok {
			effectiveLevel = lvl
			break
		}
		// Try parent component.
		idx := strings.LastIndex(p, "/")
		if idx == -1 {
			break
		}
		p = p[:idx]
	}

	if r.Level < effectiveLevel {
		return nil
	}

	return f.next.Handle(ctx, r)
}

// extractComponent finds the most recent "component" attribute.
// Attributes added in the log call itself (r.Attrs) take precedence over
// attributes added to the logger via .With (f.currentAttrs). Within each,
// the last occurrence wins.
func (f *filterHandler) extractComponent(r slog.Record) string {
	var component string

	// 1. Check handler attributes (set via .With("component", "..."))
	for _, a := range f.currentAttrs {
		if a.Key == "component" {
			component = a.Value.String()
		}
	}

	// 2. Check record attributes (set per-call)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "component" {
			component = a.Value.String()
		}
		return true
	})

	return component
}

func (f *filterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(f.currentAttrs)+len(attrs))
	copy(newAttrs, f.currentAttrs)
	copy(newAttrs[len(f.currentAttrs):], attrs)
	return &filterHandler{
		next:         f.next.WithAttrs(attrs),
		global:       f.global,
		levels:       f.levels,
		currentAttrs: newAttrs,
	}
}

func (f *filterHandler) WithGroup(name string) slog.Handler {
	// Groups don't affect component filtering logic.
	return &filterHandler{
		next:         f.next.WithGroup(name),
		global:       f.global,
		levels:       f.levels,
		currentAttrs: f.currentAttrs,
	}
}
