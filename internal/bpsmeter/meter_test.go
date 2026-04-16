package bpsmeter

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
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
	if bps <= 0 {
		t.Fatalf("expected positive BPS at t=1s, got %f", bps)
	}

	// Advance to t=5s — still within window; BPS should remain positive.
	adv(4 * time.Second)
	bps5 := m.BPS("")
	if bps5 <= 0 {
		t.Fatalf("expected positive BPS at t=5s, got %f", bps5)
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

// --- Quota tests -------------------------------------------------------------

func TestQuotaRollover(t *testing.T) {
	t.Parallel()
	// Start clock at 23:59 on some day.
	start := time.Date(2024, 3, 15, 23, 59, 0, 0, time.UTC)
	clk, adv := fixedClock(start)

	cfg := QuotaConfig{
		Period:    DailyPeriod,
		Budget:    1_000_000,
		StartHour: 0,
		Location:  time.UTC,
	}
	q := NewQuota(cfg, clk, nil)

	q.Add(1000)
	usage, _ := q.UsageAndBudget()
	if usage != 1000 {
		t.Fatalf("pre-rollover usage: want 1000, got %d", usage)
	}

	// Advance past midnight.
	adv(2 * time.Minute)

	q.Add(500)
	usage, _ = q.UsageAndBudget()
	if usage != 500 {
		t.Fatalf("post-rollover usage: want 500 (reset), got %d", usage)
	}
}

func TestQuotaExceedCallback(t *testing.T) {
	t.Parallel()
	clk, _ := fixedClock(time.Now().UTC())

	var callCount int
	var lastUsage, lastBudget int64
	handler := func(usage, budget int64) {
		callCount++
		lastUsage = usage
		lastBudget = budget
	}

	cfg := QuotaConfig{
		Period:   MonthlyPeriod,
		Budget:   1000,
		Location: time.UTC,
	}
	q := NewQuota(cfg, clk, handler)

	q.Add(600) // 600 — no callback
	if callCount != 0 {
		t.Fatal("callback fired too early")
	}

	q.Add(300) // 900 — still under budget
	if callCount != 0 {
		t.Fatal("callback fired at 900/1000")
	}

	q.Add(200) // 1100 — crosses 1000
	if callCount != 1 {
		t.Fatalf("callback should fire once, fired %d times", callCount)
	}
	if lastUsage != 1100 || lastBudget != 1000 {
		t.Fatalf("callback args: want usage=1100 budget=1000, got usage=%d budget=%d", lastUsage, lastBudget)
	}

	// Further adds in same period must NOT re-fire.
	q.Add(500)
	if callCount != 1 {
		t.Fatalf("callback re-fired; count=%d", callCount)
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

	cfg := QuotaConfig{Period: MonthlyPeriod, Budget: 1_000_000, Location: time.UTC}
	q1 := NewQuota(cfg, clk, nil)
	q1.Add(3000)

	state := Capture(m1, q1)
	if err := SaveState(path, state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	loaded, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}

	m2 := NewMeter(10*time.Second, clk)
	q2 := NewQuota(cfg, clk, nil)
	Restore(m2, q2, loaded)

	if m2.Total("") != 3000 {
		t.Fatalf("restored lifetime total: want 3000, got %d", m2.Total(""))
	}
	if m2.Total("s1") != 1000 {
		t.Fatalf("restored s1 total: want 1000, got %d", m2.Total("s1"))
	}
	if m2.Total("s2") != 2000 {
		t.Fatalf("restored s2 total: want 2000, got %d", m2.Total("s2"))
	}

	usage, _ := q2.UsageAndBudget()
	if usage != 3000 {
		t.Fatalf("restored quota usage: want 3000, got %d", usage)
	}
	if !q2.PeriodStart().Equal(q1.PeriodStart()) {
		t.Fatalf("period start mismatch: %v vs %v", q2.PeriodStart(), q1.PeriodStart())
	}
}

// --- Limiter tests -----------------------------------------------------------

func TestLimiterDisabled(t *testing.T) {
	t.Parallel()
	l := NewLimiter(0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
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
	// Rate: 256 KiB/s. Burst clamps to minBurst=256 KiB (max of bps and minBurst).
	// First Wait(burst) drains all pre-accumulated tokens immediately.
	// Second Wait(burst) must block ~1 second while new tokens accumulate.
	const bps = 256 * 1024 // 256 KiB/s
	l := NewLimiter(bps)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Drain the initial burst.
	if err := l.Wait(ctx, minBurst); err != nil {
		t.Fatalf("first Wait returned error: %v", err)
	}

	// This second request must wait for tokens to refill.
	start := time.Now()
	if err := l.Wait(ctx, minBurst); err != nil {
		t.Fatalf("second Wait returned error: %v", err)
	}
	elapsed := time.Since(start)
	// At 256 KiB/s, 256 KiB takes ~1 s. Use 400 ms as lower bound with CI slack.
	if elapsed < 400*time.Millisecond {
		t.Fatalf("limiter too fast on second Wait: elapsed %v, expected >=400ms", elapsed)
	}
}

func TestLimiterSetRate(t *testing.T) {
	t.Parallel()
	// Start very slow.
	l := NewLimiter(1)

	// Bump to a fast rate.
	l.SetRate(10_000_000) // 10 MB/s

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := l.Wait(ctx, 1000); err != nil {
		t.Fatalf("Wait after SetRate returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Fatalf("after SetRate to fast, Wait took too long: %v", elapsed)
	}
}
