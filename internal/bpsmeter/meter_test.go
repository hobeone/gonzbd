package bpsmeter

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// --- helpers -----------------------------------------------------------------

// fixedClock returns a clock function backed by an atomic int64 (Unix nanoseconds)
// so tests can advance time without real sleeps.
func fixedClock(start time.Time) (clock func() time.Time, advance func(time.Duration)) {
	var ns atomic.Int64
	ns.Store(start.UnixNano())
	clk := func() time.Time { return time.Unix(0, ns.Load()).UTC() }
	adv := func(d time.Duration) { ns.Add(int64(d)) }
	return clk, adv
}

// --- Meter tests -------------------------------------------------------------

func TestMeterBPSSmoothing(t *testing.T) {
	t.Parallel()
	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	clk, adv := fixedClock(start)
	m := NewMeter(10*time.Second, clk)

	m.Record("", 1000)
	adv(1 * time.Second)
	m.Record("", 1000)

	// At t=1s: 2000 bytes within the 10 s window → BPS = 200.
	bps := m.BPS("")
	if bps != 200.0 {
		t.Fatalf("expected BPS = 200 at t=1s, got %f", bps)
	}

	// Advance to t=5s — still within window; BPS should remain positive.
	adv(4 * time.Second)
	bps5 := m.BPS("")
	if bps5 != 200.0 {
		t.Fatalf("expected BPS = 200 at t=5s, got %f", bps5)
	}

	// Advance to t=15s — all samples now outside the 10 s window; BPS must be 0.
	adv(10 * time.Second)
	bps15 := m.BPS("")
	if bps15 != 0 {
		t.Fatalf("expected 0 BPS at t=15s (outside window), got %f", bps15)
	}
}

func TestMeterPerServer(t *testing.T) {
	t.Parallel()
	clk, _ := fixedClock(time.Now())
	m := NewMeter(10*time.Second, clk)

	m.Record("news1", 100)
	m.Record("news1", 200)
	m.Record("news2", 400)

	snap := m.Snapshot()

	news1, ok := snap.Servers["news1"]
	if !ok {
		t.Fatal("news1 missing from snapshot")
	}
	if news1.Total != 300 {
		t.Fatalf("news1 total: want 300, got %d", news1.Total)
	}

	news2, ok := snap.Servers["news2"]
	if !ok {
		t.Fatal("news2 missing from snapshot")
	}
	if news2.Total != 400 {
		t.Fatalf("news2 total: want 400, got %d", news2.Total)
	}

	// Aggregate should be sum of both.
	if snap.Total != 700 {
		t.Fatalf("aggregate total: want 700, got %d", snap.Total)
	}
}

// --- Persistence tests -------------------------------------------------------

func TestPersistenceRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	clk, _ := fixedClock(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC))
	m1 := NewMeter(10*time.Second, clk)
	m1.Record("s1", 1000)
	m1.Record("s2", 2000)

	state := m1.Capture()
	if err := SaveState(path, state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	loaded, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}

	m2 := NewMeter(10*time.Second, clk)
	m2.Restore(loaded)

	if m2.Total("") != 3000 {
		t.Fatalf("restored lifetime total: want 3000, got %d", m2.Total(""))
	}
	if m2.Total("s1") != 1000 {
		t.Fatalf("restored s1 total: want 1000, got %d", m2.Total("s1"))
	}
	if m2.Total("s2") != 2000 {
		t.Fatalf("restored s2 total: want 2000, got %d", m2.Total("s2"))
	}
}

// --- Limiter tests -----------------------------------------------------------

func TestLimiterDisabled(t *testing.T) {
	t.Parallel()
	l := NewLimiter(0)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := l.Wait(ctx, 1<<20); err != nil {
		t.Fatalf("Wait on disabled limiter returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Millisecond {
		t.Fatalf("disabled limiter took too long: %v", elapsed)
	}
}

func TestLimiterActive(t *testing.T) {
	t.Parallel()
	// NewLimiter floors burst at minBurst (256 KiB), which at a realistic rate
	// makes the refill this test waits out take a full second. Bypass that
	// floor by constructing the wrapped rate.Limiter directly (same-package
	// access to the unexported lim field) with a small burst/rate pair that
	// preserves the same ratio — refill from empty to burst takes burst/bps
	// seconds regardless of scale, so this exercises the identical blocking
	// behavior NewLimiter's production floor does, just faster.
	const bps = 1000  // 1000 tokens/sec
	const burst = 100 // refill from empty takes burst/bps = 100ms
	l := &Limiter{lim: rate.NewLimiter(rate.Limit(bps), burst)}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// Drain the initial burst.
	if err := l.Wait(ctx, burst); err != nil {
		t.Fatalf("first Wait returned error: %v", err)
	}

	// This second request must wait for tokens to refill.
	start := time.Now()
	if err := l.Wait(ctx, burst); err != nil {
		t.Fatalf("second Wait returned error: %v", err)
	}
	elapsed := time.Since(start)
	// At 1000 tokens/s with a 100-token burst, refill takes ~100ms. Use 40ms
	// as lower bound with CI slack (the same 40% margin the original
	// 400ms/1s check used).
	if elapsed < 40*time.Millisecond {
		t.Fatalf("limiter too fast on second Wait: elapsed %v, expected >=40ms", elapsed)
	}
}

func TestLimiterSetRate(t *testing.T) {
	t.Parallel()
	// Start very slow.
	l := NewLimiter(1)

	// Bump to a fast rate.
	l.SetRate(10_000_000) // 10 MB/s

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := l.Wait(ctx, 1000); err != nil {
		t.Fatalf("Wait after SetRate returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Fatalf("after SetRate to fast, Wait took too long: %v", elapsed)
	}
}

func TestMeterFlush(t *testing.T) {
	t.Parallel()
	clk, _ := fixedClock(time.Now())
	m := NewMeter(5*time.Second, clk)

	m.Record("s1", 10000)
	m.Record("s2", 20000)

	if bps := m.BPS(""); bps <= 0 {
		t.Fatalf("pre-flush BPS should be positive, got %f", bps)
	}

	m.Flush()

	if bps := m.BPS(""); bps != 0 {
		t.Fatalf("post-flush aggregate BPS should be 0, got %f", bps)
	}
	if bps := m.BPS("s1"); bps != 0 {
		t.Fatalf("post-flush s1 BPS should be 0, got %f", bps)
	}

	// Lifetime totals are preserved.
	if total := m.Total(""); total != 30000 {
		t.Fatalf("post-flush aggregate total should be 30000, got %d", total)
	}
	if total := m.Total("s1"); total != 10000 {
		t.Fatalf("post-flush s1 total should be 10000, got %d", total)
	}
}

func TestLimiterRateDecreaseResetsTokens(t *testing.T) {
	t.Parallel()
	// Start with a fast rate so the burst bucket is large.
	l := NewLimiter(10_000_000) // 10 MB/s, burst = 10 MB

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	// Decrease to 10 KB/s. The old limiter had ~10 MB of tokens;
	// the new one should start fresh with burst = 256 KiB.
	l.SetRate(10_000) // 10 KB/s

	// Drain the initial burst (256 KiB at minBurst).
	if err := l.Wait(ctx, minBurst); err != nil {
		t.Fatalf("first Wait after decrease: %v", err)
	}

	// The next Wait should block because tokens are exhausted.
	// At 10 KB/s, 256 KiB takes ~26 seconds — way longer than our
	// 100ms deadline, so we expect a timeout error.
	fastCtx, fastCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer fastCancel()

	err := l.Wait(fastCtx, minBurst)
	if err == nil {
		t.Fatal("expected Wait to block after rate decrease + burst drain, but it returned nil")
	}
}

func TestMeterRecordBytes(t *testing.T) {
	t.Parallel()
	clk, _ := fixedClock(time.Now())
	m := NewMeter(5*time.Second, clk)

	// RecordBytes should behave identically to Record.
	m.RecordBytes("s1", 1000)
	m.RecordBytes("s1", 2000)

	if total := m.Total("s1"); total != 3000 {
		t.Fatalf("RecordBytes total: want 3000, got %d", total)
	}
	if bps := m.BPS("s1"); bps <= 0 {
		t.Fatalf("RecordBytes BPS should be positive, got %f", bps)
	}
}

func TestLimiterRate_Unlimited(t *testing.T) {
	t.Parallel()
	l := NewLimiter(0) // unlimited
	if r := l.Rate(); r != 0 {
		t.Fatalf("Rate() = %f; want 0 for unlimited limiter", r)
	}
}

func TestLimiterRate_Active(t *testing.T) {
	t.Parallel()
	const bps = 1_000_000 // 1 MB/s
	l := NewLimiter(bps)
	if r := l.Rate(); r != bps {
		t.Fatalf("Rate() = %f; want %d", r, bps)
	}
}

func TestLimiterRate_AfterSetRate(t *testing.T) {
	t.Parallel()
	l := NewLimiter(1000)

	// Increase rate.
	l.SetRate(5_000_000)
	if r := l.Rate(); r != 5_000_000 {
		t.Fatalf("Rate() after increase = %f; want 5000000", r)
	}

	// Disable.
	l.SetRate(0)
	if r := l.Rate(); r != 0 {
		t.Fatalf("Rate() after disable = %f; want 0", r)
	}

	// Re-enable.
	l.SetRate(2048)
	if r := l.Rate(); r != 2048 {
		t.Fatalf("Rate() after re-enable = %f; want 2048", r)
	}
}

// H14: Wait with n exceeding burst should return an error from x/time/rate.
// This verifies the limiter doesn't panic or deadlock on oversized requests.
func TestLimiterWait_ExceedsBurst(t *testing.T) {
	t.Parallel()
	// Rate = 1000 B/s, burst = max(1000, 256*1024) = 262144 bytes.
	l := NewLimiter(1000)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Request far more tokens than the burst allows. x/time/rate returns
	// an error immediately for n > burst (no infinite wait).
	err := l.Wait(ctx, 10_000_000) // 10 MB >> burst of 262144
	if err == nil {
		t.Fatal("Wait(n >> burst) should return an error, got nil")
	}
}

// H14 follow-up: Wait with n <= burst should succeed when tokens are available.
func TestLimiterWait_WithinBurst(t *testing.T) {
	t.Parallel()
	// Rate = 10 MB/s → burst = max(10_000_000, 256*1024) = 10_000_000.
	l := NewLimiter(10_000_000)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Request within the burst size — should succeed immediately.
	if err := l.Wait(ctx, 1024); err != nil {
		t.Fatalf("Wait(1024) should succeed within burst, got: %v", err)
	}
}

// H14: Wait on cancelled context returns context error.
func TestLimiterWait_CancelledContext(t *testing.T) {
	t.Parallel()
	l := NewLimiter(1) // Very low rate (1 B/s) so token acquisition would block.

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	err := l.Wait(ctx, 1000)
	if err == nil {
		t.Fatal("Wait with cancelled context should return error")
	}
}

// L10: Record with zero bytes should not panic or corrupt BPS.
func TestMeterRecord_ZeroBytes(t *testing.T) {
	t.Parallel()
	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	clk, adv := fixedClock(start)
	m := NewMeter(10*time.Second, clk)

	m.Record("", 0)
	adv(1 * time.Second)
	bps := m.BPS("")
	if bps != 0 {
		t.Errorf("BPS after recording 0 bytes = %f, want 0", bps)
	}
}

// L10: Record with negative bytes should not panic or produce negative BPS.
func TestMeterRecord_NegativeBytes(t *testing.T) {
	t.Parallel()
	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	clk, adv := fixedClock(start)
	m := NewMeter(10*time.Second, clk)

	// Record some positive data first.
	m.Record("", 5000)
	adv(1 * time.Second)

	// Record negative bytes — should not panic.
	m.Record("", -1000)
	adv(1 * time.Second)

	bps := m.BPS("")
	// BPS should be non-negative — negative records shouldn't cause negative BPS.
	if bps < 0 {
		t.Errorf("BPS after negative record = %f, should not be negative", bps)
	}

	// Total should reflect net accounting: 5000 + (-1000) = 4000.
	if got := m.Total(""); got != 4000 {
		t.Errorf("Total after negative record = %d, want 4000", got)
	}
}

func TestNewMeter_ZeroWindow(t *testing.T) {
	t.Parallel()
	m := NewMeter(0, nil)
	if m.window != defaultWindow {
		t.Errorf("expected window to be defaultWindow (%v), got %v", defaultWindow, m.window)
	}
}

func TestMeterBPS_RingBufferWrap(t *testing.T) {
	t.Parallel()
	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	clk, adv := fixedClock(start)
	// Window is 10s. int(10) + 2 = 12 buckets.
	m := NewMeter(10*time.Second, clk)

	// Record 100 bytes every second for 10 seconds (from t=1 to t=10).
	// Total written: 1000 bytes.
	// We want to verify that no bucket overwrites occur early.
	for range 10 {
		adv(1 * time.Second)
		m.Record("", 100)
	}

	// At t=10s, BPS is sum of t=1 to t=10 (10s window).
	// Total bytes in window should be 1000.
	bps := m.BPS("")
	if bps != 100.0 {
		t.Errorf("expected BPS = 100.0, got %f", bps)
	}
}

func TestLimiterSetRate_Detailed(t *testing.T) {
	t.Parallel()
	l := NewLimiter(1000) // case l.lim == nil

	// Rate unchanged
	l.SetRate(1000)
	if r := l.Rate(); r != 1000 {
		t.Errorf("expected rate to remain 1000, got %f", r)
	}

	// Rate increased
	l.SetRate(2000)
	if r := l.Rate(); r != 2000 {
		t.Errorf("expected rate to increase to 2000, got %f", r)
	}

	// Rate decreased
	l.SetRate(500)
	if r := l.Rate(); r != 500 {
		t.Errorf("expected rate to decrease to 500, got %f", r)
	}

	// Rate disabled (0)
	l.SetRate(0)
	if r := l.Rate(); r != 0 {
		t.Errorf("expected rate to be 0 (disabled), got %f", r)
	}

	// Rate disabled again (negative)
	l.SetRate(-100)
	if r := l.Rate(); r != 0 {
		t.Errorf("expected rate to be 0 (disabled negative), got %f", r)
	}
}
