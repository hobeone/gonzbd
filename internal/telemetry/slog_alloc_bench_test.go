//go:build bench

package telemetry_test

// Benchmarks backing the "discarded log calls still allocate" rule in
// docs/go-standards.md § General Performance Rules.
//
// Behind a `bench` build tag: these measure stdlib log/slog, not this package,
// so they must not land in `go test -bench=. ./internal/telemetry/` and pass
// themselves off as telemetry benchmarks. Run them explicitly with
// `go test -tags=bench -bench=Slog -benchmem ./internal/telemetry/`.
//
// Every Debug call below is DISCARDED — the logger is set to Info. They still
// allocate, because each non-constant argument is boxed into `any` at the call
// site, before Debug is entered and before the level check inside it runs.
//
// These exist so the numbers quoted in the standards doc are reproducible
// rather than asserted. They pin a property of log/slog and the compiler's
// interface boxing, not of gonzbd code, so they assert nothing and cannot fail
// — re-run them with `go test -bench=Slog -benchmem ./internal/telemetry/` if
// a Go upgrade makes you doubt the documented figures.

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// A real handler behind a level filter, deliberately not slog.DiscardHandler:
// the point being measured is that *level filtering* does not avoid the
// call-site allocation. DiscardHandler reports Enabled()==false for every
// level and so would not model gonzbd's actual configuration, where components
// like `downloader` run at info and their Debug calls are filtered out.
//
//nolint:sloglint // see above — DiscardHandler would not model level filtering
var discardInfo = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))

// Package-level so the compiler cannot fold these into constants.
var (
	benchServer   = "news.example.com"
	benchWorker   = "news-17"
	benchReason   = "idle"
	benchSmallInt = 42    // within runtime.staticuint64s (0..255)
	benchBigInt   = 99999 // outside it
)

// The shape that caused #336: three non-constant string args. Expect 3 allocs.
func BenchmarkSlogDiscarded_ThreeStringVars(b *testing.B) {
	for b.Loop() {
		discardInfo.Debug("disconnected from server",
			"server", benchServer, "worker", benchWorker, "reason", benchReason)
	}
}

func BenchmarkSlogDiscarded_NoArgs(b *testing.B) {
	for b.Loop() {
		discardInfo.Debug("msg")
	}
}

// Constant keys AND constant values box into read-only statics. Expect 0 allocs.
func BenchmarkSlogDiscarded_AllConstant(b *testing.B) {
	for b.Loop() {
		discardInfo.Debug("msg", "k1", "c1", "k2", "c2", "k3", "c3")
	}
}

func BenchmarkSlogDiscarded_OneStringVar(b *testing.B) {
	for b.Loop() {
		discardInfo.Debug("msg", "k1", benchServer)
	}
}

// Small ints hit runtime.staticuint64s. Expect 0 allocs.
func BenchmarkSlogDiscarded_SmallIntVar(b *testing.B) {
	for b.Loop() {
		discardInfo.Debug("msg", "k1", benchSmallInt)
	}
}

func BenchmarkSlogDiscarded_BigIntVar(b *testing.B) {
	for b.Loop() {
		discardInfo.Debug("msg", "k1", benchBigInt)
	}
}

// The two allocation-free ways to keep a Debug call in a hot path.
func BenchmarkSlogDiscarded_EnabledGuard(b *testing.B) {
	ctx := context.Background()
	for b.Loop() {
		if discardInfo.Enabled(ctx, slog.LevelDebug) {
			discardInfo.Debug("disconnected from server",
				"server", benchServer, "worker", benchWorker, "reason", benchReason)
		}
	}
}

func BenchmarkSlogDiscarded_LogAttrs(b *testing.B) {
	ctx := context.Background()
	for b.Loop() {
		discardInfo.LogAttrs(ctx, slog.LevelDebug, "disconnected from server",
			slog.String("server", benchServer),
			slog.String("worker", benchWorker),
			slog.String("reason", benchReason))
	}
}
