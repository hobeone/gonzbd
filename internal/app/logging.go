package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

var (
	globalLevelVar    = &slog.LevelVar{}
	componentLevelsMu sync.RWMutex
	componentLevels   = make(map[string]slog.Level)
)

type dynamicMinLevel struct{}

func (dynamicMinLevel) Level() slog.Level {
	minLvl := globalLevelVar.Level()
	componentLevelsMu.RLock()
	defer componentLevelsMu.RUnlock()
	for _, lvl := range componentLevels {
		if lvl < minLvl {
			minLvl = lvl
		}
	}
	return minLvl
}

// SetLogLevels updates the global log level and per-component level overrides at runtime.
func SetLogLevels(global slog.Level, compLevels map[string]slog.Level) {
	globalLevelVar.Set(global)
	componentLevelsMu.Lock()
	componentLevels = make(map[string]slog.Level, len(compLevels))
	maps.Copy(componentLevels, compLevels)
	componentLevelsMu.Unlock()
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
	// Initialize global and component level overrides
	SetLogLevels(opts.Level, opts.ComponentLevels)

	var closer io.Closer
	var handlers []slog.Handler

	// Use dynamicMinLevel to always pass log messages through to filterHandler
	// where precise global/component filtering happens.
	minLevel := dynamicMinLevel{}

	// 1. Console handler (Colorized)
	handlers = append(handlers, tint.NewHandler(os.Stderr, &tint.Options{
		Level:      minLevel,
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
			Level:     minLevel,
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

	// Always wrap with filterHandler so overrides can be loaded at runtime
	h = &filterHandler{
		next: h,
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
	next slog.Handler

	// currentAttrs holds the attributes added via WithAttrs.
	currentAttrs []slog.Attr
}

func (f *filterHandler) Enabled(ctx context.Context, level slog.Level) bool {
	// We cannot check the component here (no access to attributes),
	// so be permissive and let Handle do the filtering.
	return f.next.Enabled(ctx, level)
}

func (f *filterHandler) Handle(ctx context.Context, r slog.Record) error {
	components := f.extractComponents(r)
	joined := joinComponents(components)

	if r.Level < resolveEffectiveLevel(components, joined) {
		return nil
	}

	// Emit a single canonical component attribute (the joined path) instead of
	// the stack of individually-scoped ones. WithAttrs already withheld
	// component from f.next, so injecting it here yields exactly one
	// component= key per line (e.g. "app/postproc/repair").
	return f.next.Handle(ctx, recordWithSingleComponent(r, joined))
}

// joinComponents builds the canonical component path from the ordered
// (least→most specific) component values by joining with "/". Component values
// are bare tokens (e.g. "app", "postproc", "repair"), so a plain join yields
// "app/postproc/repair".
func joinComponents(components []string) string {
	return strings.Join(components, "/")
}

// resolveEffectiveLevel determines the minimum level a record must meet to be
// emitted, given its ordered component chain and the joined canonical path.
func resolveEffectiveLevel(components []string, joined string) slog.Level {
	componentLevelsMu.RLock()
	defer componentLevelsMu.RUnlock()

	// 1. Walk the individual components most-specific (last) to least-specific
	//    (first); for each, also try slash-hierarchy parents. First match
	//    wins. Preserves every documented per-segment filter key.
	for _, p := range slices.Backward(components) {
		if lvl, ok := lookupComponentLevel(p); ok {
			return lvl
		}
	}
	// 2. Superset: the joined canonical path and its slash-prefixes, so the
	//    exact component string shown in logs (e.g. "app/postproc/repair") is
	//    itself a usable filter key.
	if lvl, ok := lookupComponentLevel(joined); ok {
		return lvl
	}
	return globalLevelVar.Level()
}

// lookupComponentLevel checks componentLevels for p and each of its
// slash-parents (e.g. "app/postproc/repair" → "app/postproc" → "app").
// The caller must hold componentLevelsMu.
func lookupComponentLevel(p string) (slog.Level, bool) {
	for p != "" {
		if lvl, ok := componentLevels[p]; ok {
			return lvl, true
		}
		idx := strings.LastIndex(p, "/")
		if idx == -1 {
			break
		}
		p = p[:idx]
	}
	return 0, false
}

// recordWithSingleComponent returns a copy of r with every "component"
// attribute removed and, when component is non-empty, exactly one "component"
// attribute appended.
func recordWithSingleComponent(r slog.Record, component string) slog.Record {
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key != "component" {
			nr.AddAttrs(a)
		}
		return true
	})
	if component != "" {
		nr.AddAttrs(slog.String("component", component))
	}
	return nr
}

// extractComponents collects all "component" attribute values in order.
// Handler attributes (.With) come first, then record attributes (per-call).
// The result is ordered from least-specific to most-specific — callers
// should walk it in reverse to find the most-specific matching rule.
func (f *filterHandler) extractComponents(r slog.Record) []string {
	var components []string

	// 1. Handler attributes (set via .With("component", "..."))
	for _, a := range f.currentAttrs {
		if a.Key == "component" {
			components = append(components, a.Value.String())
		}
	}

	// 2. Record attributes (set per-call)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "component" {
			components = append(components, a.Value.String())
		}
		return true
	})

	return components
}

func (f *filterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(f.currentAttrs)+len(attrs))
	copy(newAttrs, f.currentAttrs)
	copy(newAttrs[len(f.currentAttrs):], attrs)

	// Forward only non-component attrs to the sink handlers; component values
	// are re-emitted as a single joined attribute in Handle, so the sinks must
	// not accumulate the individually-scoped ones.
	forwarded := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		if a.Key != "component" {
			forwarded = append(forwarded, a)
		}
	}
	return &filterHandler{
		next:         f.next.WithAttrs(forwarded),
		currentAttrs: newAttrs,
	}
}

func (f *filterHandler) WithGroup(name string) slog.Handler {
	// Groups don't affect component filtering logic.
	return &filterHandler{
		next:         f.next.WithGroup(name),
		currentAttrs: f.currentAttrs,
	}
}
