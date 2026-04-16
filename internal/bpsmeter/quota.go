package bpsmeter

import (
	"sync"
	"time"
)

// Period is the quota rollover cadence.
type Period int

const (
	// DailyPeriod resets at StartHour local time every day.
	DailyPeriod Period = iota + 1
	// WeeklyPeriod resets at Monday 00:00 local time (adjusted by StartHour).
	WeeklyPeriod
	// MonthlyPeriod resets on the 1st of each month at StartHour local time.
	MonthlyPeriod
)

// QuotaConfig holds all immutable parameters for a Quota.
type QuotaConfig struct {
	Period    Period
	Budget    int64          // bytes; 0 means unlimited
	StartHour int            // rollover hour-of-day (0–23)
	Location  *time.Location // nil ⇒ time.Local
}

// ExceedHandler is invoked once when usage crosses the budget within a period.
// Called synchronously from Add; keep it non-blocking.
type ExceedHandler func(usage, budget int64)

// Quota tracks bandwidth usage against a configurable budget and period.
// Safe for concurrent callers.
type Quota struct {
	mu sync.Mutex

	cfg      QuotaConfig
	clock    func() time.Time
	onExceed ExceedHandler

	periodStart time.Time
	usage       int64
	exceeded    bool // fired this period already
}

// NewQuota creates a Quota with the given config, clock, and exceed handler.
// onExceed may be nil. Pass nil for clock to use time.Now.
func NewQuota(cfg QuotaConfig, clock func() time.Time, onExceed ExceedHandler) *Quota {
	if clock == nil {
		clock = time.Now
	}
	loc := cfg.Location
	if loc == nil {
		loc = time.Local
	}
	cfg.Location = loc

	now := clock()
	return &Quota{
		cfg:         cfg,
		clock:       clock,
		onExceed:    onExceed,
		periodStart: periodStart(cfg.Period, now, cfg.StartHour, cfg.Location),
	}
}

// periodStart returns the start of the period containing t.
func periodStart(p Period, t time.Time, startHour int, loc *time.Location) time.Time {
	t = t.In(loc)
	switch p {
	case DailyPeriod:
		base := time.Date(t.Year(), t.Month(), t.Day(), startHour, 0, 0, 0, loc)
		if t.Before(base) {
			// We're before today's reset hour; period started yesterday.
			base = base.AddDate(0, 0, -1)
		}
		return base
	case WeeklyPeriod:
		// Walk back to Monday.
		wd := int(t.Weekday())
		if wd == 0 {
			wd = 7 // Sunday → 7
		}
		monday := t.AddDate(0, 0, -(wd - 1))
		base := time.Date(monday.Year(), monday.Month(), monday.Day(), startHour, 0, 0, 0, loc)
		if t.Before(base) {
			base = base.AddDate(0, 0, -7)
		}
		return base
	case MonthlyPeriod:
		base := time.Date(t.Year(), t.Month(), 1, startHour, 0, 0, 0, loc)
		if t.Before(base) {
			// Before this month's reset; period started last month.
			base = base.AddDate(0, -1, 0)
		}
		return base
	default:
		return t
	}
}

// nextPeriodStart returns the start of the NEXT period after ps.
// ps must already be the canonical start of a period (produced by periodStart).
func nextPeriodStart(p Period, ps time.Time) time.Time {
	switch p {
	case DailyPeriod:
		return ps.AddDate(0, 0, 1)
	case WeeklyPeriod:
		return ps.AddDate(0, 0, 7)
	case MonthlyPeriod:
		return ps.AddDate(0, 1, 0)
	default:
		return ps.Add(24 * time.Hour)
	}
}

// maybeRollover checks whether the current clock time has crossed the period
// boundary and resets usage if so. Must be called with mu held.
func (q *Quota) maybeRollover() {
	now := q.clock()
	next := nextPeriodStart(q.cfg.Period, q.periodStart)
	if now.Before(next) {
		return
	}
	// One or more periods have elapsed; recalculate from now.
	q.periodStart = periodStart(q.cfg.Period, now, q.cfg.StartHour, q.cfg.Location)
	q.usage = 0
	q.exceeded = false
}

// Add adds n bytes to the current period. If the period boundary has been
// crossed since the last call, the running total is reset before adding.
// If the new total crosses cfg.Budget, the ExceedHandler fires (once per period).
func (q *Quota) Add(n int64) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.maybeRollover()
	q.usage += n

	if q.cfg.Budget > 0 && !q.exceeded && q.usage >= q.cfg.Budget {
		q.exceeded = true
		if q.onExceed != nil {
			// Call without lock — handler must not re-enter Quota.
			usage := q.usage
			budget := q.cfg.Budget
			q.mu.Unlock()
			q.onExceed(usage, budget)
			q.mu.Lock()
		}
	}
}

// UsageAndBudget returns current period usage and the configured budget.
func (q *Quota) UsageAndBudget() (usage, budget int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.maybeRollover()
	return q.usage, q.cfg.Budget
}

// PeriodStart returns the start time of the current period.
func (q *Quota) PeriodStart() time.Time {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.maybeRollover()
	return q.periodStart
}

// Exceeded returns true if the current period's usage has crossed budget.
func (q *Quota) Exceeded() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.maybeRollover()
	return q.exceeded
}
