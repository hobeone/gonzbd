package postproc

import (
	"context"
	"log/slog"
	"sync"
)

type testLogHandler struct {
	mu         sync.Mutex
	warnLogged bool
}

func (h *testLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return true
}

func (h *testLogHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.Level == slog.LevelWarn {
		h.warnLogged = true
	}
	return nil
}

func (h *testLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *testLogHandler) WithGroup(name string) slog.Handler       { return h }
