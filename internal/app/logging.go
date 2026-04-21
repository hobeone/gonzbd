package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/lmittmann/tint"
)

// LoggingOptions configures structured logging behavior.
type LoggingOptions struct {
	// Level is the minimum log level (Debug, Info, Warn, Error).
	Level slog.Level
	// LogFile is the path to the log file. Empty means stderr only.
	LogFile string
	// AddSource adds file:line annotations to each log record.
	AddSource bool

	// Allow restricts logging to only these components (e.g., "downloader").
	Allow []string
	// Deny suppresses logging from these components.
	Deny []string
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

	// 1. Console handler (Colorized)
	handlers = append(handlers, tint.NewHandler(os.Stderr, &tint.Options{
		Level:      opts.Level,
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
			Level:     opts.Level,
			AddSource: opts.AddSource,
		}))
	}

	// 3. Combine and wrap with filtering
	var h slog.Handler
	if len(handlers) == 1 {
		h = handlers[0]
	} else {
		h = &multiHandler{handlers: handlers}
	}

	if len(opts.Allow) > 0 || len(opts.Deny) > 0 {
		h = &filterHandler{
			next:  h,
			allow: opts.Allow,
			deny:  opts.Deny,
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
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil {
				return err
			}
		}
	}
	return nil
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

// filterHandler drops log records based on component allow/deny lists.
type filterHandler struct {
	next  slog.Handler
	allow []string
	deny  []string

	// currentAttrs holds the attributes added via WithAttrs.
	currentAttrs []slog.Attr
}

func (f *filterHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return f.next.Enabled(ctx, level)
}

func (f *filterHandler) Handle(ctx context.Context, r slog.Record) error {
	// Extract "component" from the record or from the handler's fixed attributes.
	component := ""

	// 1. Check record attributes
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "component" {
			component = a.Value.String()
			return false // stop iteration
		}
		return true
	})

	// 2. Check handler attributes if not found in record
	if component == "" {
		for _, a := range f.currentAttrs {
			if a.Key == "component" {
				component = a.Value.String()
				break
			}
		}
	}

	// Only apply filters if a component was identified.
	if component != "" {
		// Deny list takes precedence.
		for _, d := range f.deny {
			if d == component {
				return nil
			}
		}
		// If allow list is present, component MUST be in it.
		if len(f.allow) > 0 {
			found := false
			for _, a := range f.allow {
				if a == component {
					found = true
					break
				}
			}
			if !found {
				return nil
			}
		}
	}

	return f.next.Handle(ctx, r)
}

func (f *filterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &filterHandler{
		next:         f.next.WithAttrs(attrs),
		allow:        f.allow,
		deny:         f.deny,
		currentAttrs: append(f.currentAttrs, attrs...),
	}
}

func (f *filterHandler) WithGroup(name string) slog.Handler {
	// Groups don't affect component filtering logic.
	return &filterHandler{
		next:         f.next.WithGroup(name),
		allow:        f.allow,
		deny:         f.deny,
		currentAttrs: f.currentAttrs,
	}
}
