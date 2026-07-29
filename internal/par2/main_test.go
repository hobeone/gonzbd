package par2

import (
	"log/slog"
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package's tests if any goroutine outlives them.
//
// This package spawns a progress-monitor goroutine per verify/repair
// (monitorProgress), whose only exit condition is the caller closing the
// progress channel. Because par2engine is expected to panic on malformed
// input — that is precisely why every call site is wrapped in
// cmdutil.SafeEngineRun — a close() that is not deferred is skipped during
// unwind and the monitor is stranded forever. See issue #100.
//
// Leak detection is the only thing that catches that: the job still fails
// cleanly, so no assertion on the returned error would ever notice.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	goleak.VerifyTestMain(m)
}
