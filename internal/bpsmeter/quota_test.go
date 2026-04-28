package bpsmeter

import (
	"sync/atomic"
	"testing"
	"time"
)

// ---------- periodStart ----------

func TestPeriodStart_Daily(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	// 2024-04-28 15:00 UTC with startHour=3 → period started 2024-04-28 03:00.
	now := time.Date(2024, 4, 28, 15, 0, 0, 0, loc)
	ps := periodStart(DailyPeriod, now, 3, loc)
	want := time.Date(2024, 4, 28, 3, 0, 0, 0, loc)
	if !ps.Equal(want) {
		t.Errorf("got %v, want %v", ps, want)
	}
}

func TestPeriodStart_Daily_BeforeStartHour(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	// 2024-04-28 01:00 with startHour=3 → period started 2024-04-27 03:00.
	now := time.Date(2024, 4, 28, 1, 0, 0, 0, loc)
	ps := periodStart(DailyPeriod, now, 3, loc)
	want := time.Date(2024, 4, 27, 3, 0, 0, 0, loc)
	if !ps.Equal(want) {
		t.Errorf("got %v, want %v", ps, want)
	}
}

func TestPeriodStart_Weekly(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	// 2024-04-28 is Sunday. Monday was 2024-04-22.
	now := time.Date(2024, 4, 28, 15, 0, 0, 0, loc)
	ps := periodStart(WeeklyPeriod, now, 0, loc)
	want := time.Date(2024, 4, 22, 0, 0, 0, 0, loc)
	if !ps.Equal(want) {
		t.Errorf("got %v, want %v", ps, want)
	}
}

func TestPeriodStart_Monthly(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	now := time.Date(2024, 4, 15, 12, 0, 0, 0, loc)
	ps := periodStart(MonthlyPeriod, now, 0, loc)
	want := time.Date(2024, 4, 1, 0, 0, 0, 0, loc)
	if !ps.Equal(want) {
		t.Errorf("got %v, want %v", ps, want)
	}
}

func TestPeriodStart_Monthly_BeforeStartHour(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	// 2024-04-01 02:00 with startHour=6 → period started 2024-03-01 06:00.
	now := time.Date(2024, 4, 1, 2, 0, 0, 0, loc)
	ps := periodStart(MonthlyPeriod, now, 6, loc)
	want := time.Date(2024, 3, 1, 6, 0, 0, 0, loc)
	if !ps.Equal(want) {
		t.Errorf("got %v, want %v", ps, want)
	}
}

func TestPeriodStart_DefaultPeriod(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	now := time.Date(2024, 4, 15, 12, 0, 0, 0, loc)
	ps := periodStart(Period(99), now, 0, loc) // unknown period
	if !ps.Equal(now) {
		t.Errorf("default period should return now, got %v", ps)
	}
}

// ---------- nextPeriodStart ----------

func TestNextPeriodStart_Daily(t *testing.T) {
	t.Parallel()
	ps := time.Date(2024, 4, 28, 3, 0, 0, 0, time.UTC)
	next := nextPeriodStart(DailyPeriod, ps)
	want := time.Date(2024, 4, 29, 3, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("got %v, want %v", next, want)
	}
}

func TestNextPeriodStart_Weekly(t *testing.T) {
	t.Parallel()
	ps := time.Date(2024, 4, 22, 0, 0, 0, 0, time.UTC)
	next := nextPeriodStart(WeeklyPeriod, ps)
	want := time.Date(2024, 4, 29, 0, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("got %v, want %v", next, want)
	}
}

func TestNextPeriodStart_Monthly(t *testing.T) {
	t.Parallel()
	ps := time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)
	next := nextPeriodStart(MonthlyPeriod, ps)
	want := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("got %v, want %v", next, want)
	}
}

func TestNextPeriodStart_Default(t *testing.T) {
	t.Parallel()
	ps := time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)
	next := nextPeriodStart(Period(99), ps)
	want := ps.Add(24 * time.Hour)
	if !next.Equal(want) {
		t.Errorf("got %v, want %v", next, want)
	}
}

// ---------- Quota Exceeded ----------

func TestQuota_Exceeded(t *testing.T) {
	t.Parallel()
	now := time.Date(2024, 4, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	var exceeded atomic.Bool
	q := NewQuota(QuotaConfig{
		Period:   DailyPeriod,
		Budget:   1000,
		Location: time.UTC,
	}, clock, func(usage, budget int64) {
		exceeded.Store(true)
	})

	if q.Exceeded() {
		t.Error("should not be exceeded initially")
	}

	q.Add(500)
	if q.Exceeded() {
		t.Error("should not be exceeded at 500/1000")
	}

	q.Add(600) // total = 1100 > 1000
	if !q.Exceeded() {
		t.Error("should be exceeded at 1100/1000")
	}
	if !exceeded.Load() {
		t.Error("ExceedHandler should have been called")
	}
}

func TestQuota_Rollover(t *testing.T) {
	t.Parallel()
	now := time.Date(2024, 4, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	q := NewQuota(QuotaConfig{
		Period:   DailyPeriod,
		Budget:   1000,
		Location: time.UTC,
	}, clock, nil)

	q.Add(1500)
	if !q.Exceeded() {
		t.Fatal("should be exceeded")
	}

	// Advance clock past the next period.
	now = time.Date(2024, 4, 29, 12, 0, 0, 0, time.UTC)

	usage, budget := q.UsageAndBudget()
	if usage != 0 {
		t.Errorf("usage = %d after rollover, want 0", usage)
	}
	if budget != 1000 {
		t.Errorf("budget = %d, want 1000", budget)
	}
	if q.Exceeded() {
		t.Error("should not be exceeded after rollover")
	}
}

func TestNewQuota_NilClock(t *testing.T) {
	t.Parallel()
	q := NewQuota(QuotaConfig{
		Period:   DailyPeriod,
		Budget:   1000,
		Location: time.UTC,
	}, nil, nil)

	if q == nil {
		t.Fatal("quota is nil")
	}
	if q.Exceeded() {
		t.Error("should not be exceeded with nil clock")
	}
}

func TestNewQuota_NilLocation(t *testing.T) {
	t.Parallel()
	q := NewQuota(QuotaConfig{
		Period: DailyPeriod,
		Budget: 1000,
		// Location is nil — should default to time.Local.
	}, nil, nil)

	if q == nil {
		t.Fatal("quota is nil")
	}
}
