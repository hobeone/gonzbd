package postproc

import "sync/atomic"

// toggle is an embeddable, goroutine-safe enable/disable flag for stages.
//
// The zero value is enabled. SetEnabled may be called from any goroutine
// (e.g. an API handler reacting to a config change) while a job is running
// in the postproc worker, so the flag is stored atomically.
//
// Embed it by value in a stage struct to promote SetEnabled/enabled:
//
//	type FooStage struct {
//	    toggle
//	    Log *slog.Logger
//	}
//
// Stages are always used as pointers and never copied, so embedding the
// (non-copyable) atomic by value is safe.
type toggle struct {
	disabled atomic.Bool
}

// SetEnabled enables or disables the stage at runtime. Thread-safe.
func (t *toggle) SetEnabled(enabled bool) { t.disabled.Store(!enabled) }

// enabled reports whether the stage is currently enabled.
func (t *toggle) enabled() bool { return !t.disabled.Load() }

// IsEnabled is the exported counterpart of enabled, for use in tests.
func (t *toggle) IsEnabled() bool { return !t.disabled.Load() }
