// Package scheduler provides a cron-like scheduler that parses SABnzbd's
// 5-field schedule format and dispatches registered action handlers at the
// appropriate minute boundaries.
//
// Supported field syntax per position: * (any), integer, a,b,c (comma list),
// a-b (inclusive range), */n (stride). Month and day-of-month fields are
// accepted but always match (SABnzbd schedules use minute/hour/dow only in
// practice; those fields are parsed for completeness).
//
// Not supported: named day/month values (e.g. "Mon", "Jan"), L/W/# modifiers,
// combined range+stride like "1-5/2".
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// ScheduleSpec is a parsed cron-like expression derived from one schedule line.
type ScheduleSpec struct {
	Minutes     []int
	Hours       []int
	DaysOfMonth []int
	Months      []int
	DaysOfWeek  []int
	Action      string
	Arg         string
}

// Parse turns a schedule line like "30 14 * * 1-5 speedlimit 1000" into a
// ScheduleSpec. The first five space-delimited tokens are the cron fields
// (minute hour dom month dow). Token six is the action name. An optional
// seventh token (everything after the action) is the argument.
//
// Field ranges:
//
//	minute:     0–59
//	hour:       0–23
//	day-of-month: 1–31
//	month:      1–12
//	day-of-week: 0–7 (0 and 7 both mean Sunday; SABnzbd uses 1=Mon … 7=Sun)
func Parse(line string) (ScheduleSpec, error) {
	line = strings.TrimSpace(line)
	parts := strings.Fields(line)
	if len(parts) < 6 {
		return ScheduleSpec{}, fmt.Errorf("schedule %q: need at least 6 fields (minute hour dom month dow action), got %d", line, len(parts))
	}

	var spec ScheduleSpec
	var err error

	// minute 0-59
	spec.Minutes, err = parseField(parts[0], 0, 59)
	if err != nil {
		return ScheduleSpec{}, fmt.Errorf("schedule %q: minute field: %w", line, err)
	}

	// hour 0-23
	spec.Hours, err = parseField(parts[1], 0, 23)
	if err != nil {
		return ScheduleSpec{}, fmt.Errorf("schedule %q: hour field: %w", line, err)
	}

	// day-of-month 1-31
	spec.DaysOfMonth, err = parseField(parts[2], 1, 31)
	if err != nil {
		return ScheduleSpec{}, fmt.Errorf("schedule %q: dom field: %w", line, err)
	}

	// month 1-12
	spec.Months, err = parseField(parts[3], 1, 12)
	if err != nil {
		return ScheduleSpec{}, fmt.Errorf("schedule %q: month field: %w", line, err)
	}

	// day-of-week 0-7 (0 and 7 = Sunday)
	spec.DaysOfWeek, err = parseField(parts[4], 0, 7)
	if err != nil {
		return ScheduleSpec{}, fmt.Errorf("schedule %q: dow field: %w", line, err)
	}

	spec.Action = parts[5]
	if len(parts) > 6 {
		// Everything after the action name is the argument, preserving spaces.
		actionIdx := strings.Index(line, parts[5])
		rest := strings.TrimSpace(line[actionIdx+len(parts[5]):])
		spec.Arg = rest
	}

	return spec, nil
}

// parseField expands a single cron field token into the set of matching
// integers. fieldMin and fieldMax define the valid range; * expands to
// [fieldMin..fieldMax].
func parseField(token string, fieldMin, fieldMax int) ([]int, error) {
	var result []int

	for _, segment := range strings.Split(token, ",") {
		vals, err := parseSegment(segment, fieldMin, fieldMax)
		if err != nil {
			return nil, err
		}
		result = append(result, vals...)
	}

	// Deduplicate preserving first-seen order (in practice duplicates only
	// arise from overlapping comma entries).
	seen := make(map[int]bool, len(result))
	deduped := result[:0]
	for _, v := range result {
		if !seen[v] {
			seen[v] = true
			deduped = append(deduped, v)
		}
	}

	return deduped, nil
}

// parseSegment handles a single comma segment: *, n, a-b, */n, or a-b/n.
func parseSegment(seg string, fieldMin, fieldMax int) ([]int, error) {
	stride := 1
	base := seg

	if idx := strings.Index(seg, "/"); idx >= 0 {
		strideStr := seg[idx+1:]
		var err error
		stride, err = strconv.Atoi(strideStr)
		if err != nil || stride < 1 {
			return nil, fmt.Errorf("invalid stride %q", strideStr)
		}
		base = seg[:idx]
	}

	var lo, hi int

	switch {
	case base == "*":
		lo, hi = fieldMin, fieldMax

	case strings.Contains(base, "-"):
		parts := strings.SplitN(base, "-", 2)
		var err error
		lo, err = strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("invalid range start %q", parts[0])
		}
		hi, err = strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("invalid range end %q", parts[1])
		}
		if lo > hi {
			return nil, fmt.Errorf("range start %d > end %d", lo, hi)
		}

	default:
		n, err := strconv.Atoi(base)
		if err != nil {
			return nil, fmt.Errorf("invalid integer %q", base)
		}
		lo, hi = n, n
	}

	if lo < fieldMin || hi > fieldMax {
		return nil, fmt.Errorf("value %d-%d out of allowed range [%d, %d]", lo, hi, fieldMin, fieldMax)
	}

	var result []int
	for v := lo; v <= hi; v += stride {
		result = append(result, v)
	}
	return result, nil
}

// Matches reports whether the spec fires at time t (minute precision).
// All non-wildcard fields must match simultaneously. A nil or empty slice
// for any field means "match all" (produced by * expansion).
func (s ScheduleSpec) Matches(t time.Time) bool {
	return contains(s.Minutes, t.Minute()) &&
		contains(s.Hours, t.Hour()) &&
		contains(s.DaysOfMonth, t.Day()) &&
		contains(s.Months, int(t.Month())) &&
		matchesDOW(s.DaysOfWeek, t.Weekday())
}

// matchesDOW maps time.Weekday (0=Sun … 6=Sat) to SABnzbd's convention
// (1=Mon … 7=Sun; 0 also accepted as Sunday for cron compat) and checks.
func matchesDOW(dow []int, wd time.Weekday) bool {
	// SABnzbd: 1=Mon, 2=Tue, … 6=Sat, 7=Sun
	// time.Weekday: 0=Sun, 1=Mon, … 6=Sat
	var sabDay int
	if wd == time.Sunday {
		sabDay = 7
	} else {
		sabDay = int(wd) // Mon=1 … Sat=6
	}
	// Also allow 0 for Sunday (standard cron).
	for _, d := range dow {
		if d == sabDay || (d == 0 && wd == time.Sunday) {
			return true
		}
	}
	return false
}

func contains(vals []int, v int) bool {
	for _, x := range vals {
		if x == v {
			return true
		}
	}
	return false
}

// Scheduler owns the periodic tick loop and dispatches actions.
type Scheduler struct {
	schedules []ScheduleSpec
	registry  *Registry
	oneshots  *OneshotQueue
	clock     func() time.Time
	logger    *slog.Logger
}

// New creates a Scheduler with the given schedules and registry. If logger is
// nil, slog.Default() is used. The clock field is injectable for tests; pass
// nil to use time.Now.
func New(schedules []ScheduleSpec, registry *Registry, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("component", "scheduler")
	return &Scheduler{
		schedules: schedules,
		registry:  registry,
		oneshots:  NewOneshotQueue(),
		clock:     time.Now,
		logger:    log,
	}
}

// Oneshots returns the internal OneshotQueue so callers can enqueue delayed
// events (e.g. "resume server after penalty").
func (s *Scheduler) Oneshots() *OneshotQueue {
	return s.oneshots
}

// Run ticks every minute aligned to the wall clock, dispatching any matching
// schedules and any expired one-shots. Blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	// Align the first tick to the next whole minute.
	now := s.clock()
	next := now.Truncate(time.Minute).Add(time.Minute)
	timer := time.NewTimer(time.Until(next))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case tick := <-timer.C:
			s.Tick(ctx, tick)
			// Schedule the next tick exactly one minute after the last aligned
			// boundary, guarding against drift.
			next = tick.Truncate(time.Minute).Add(time.Minute)
			timer.Reset(time.Until(next))
		}
	}
}

// Tick runs one evaluation at time t. Exported so tests can drive the
// scheduler without sleeping.
func (s *Scheduler) Tick(ctx context.Context, t time.Time) {
	for i := range s.schedules {
		spec := &s.schedules[i]
		if !spec.Matches(t) {
			continue
		}
		s.logger.Info("dispatching scheduled action",
			slog.String("action", spec.Action),
			slog.String("arg", spec.Arg),
			slog.Time("at", t),
		)
		if err := s.registry.Dispatch(ctx, spec.Action, spec.Arg); err != nil {
			s.logger.Warn("scheduled action failed",
				slog.String("action", spec.Action),
				slog.String("arg", spec.Arg),
				slog.Any("error", err),
			)
		}
	}

	for _, o := range s.oneshots.Due(t) {
		s.logger.Info("dispatching one-shot action",
			slog.String("action", o.Action),
			slog.String("arg", o.Arg),
			slog.Time("fire_at", o.FireAt),
		)
		if err := s.registry.Dispatch(ctx, o.Action, o.Arg); err != nil {
			s.logger.Warn("one-shot action failed",
				slog.String("action", o.Action),
				slog.String("arg", o.Arg),
				slog.Any("error", err),
			)
		}
	}
}
