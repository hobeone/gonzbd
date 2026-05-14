// Package scheduler provides a cron-based scheduler that dispatches registered
// action handlers at the appropriate minute boundaries.
//
// Schedule parsing is delegated to github.com/robfig/cron/v3, which provides
// a battle-tested implementation of standard 5-field cron expressions with
// support for ranges, strides, comma lists, named days/months, and more.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// ScheduleSpec is a parsed cron-like expression derived from one schedule line.
type ScheduleSpec struct {
	schedule cron.Schedule
	Action   string
	Arg      string
}

// Parse turns a schedule line like "30 14 * * 1-5 speedlimit 1000" into a
// ScheduleSpec. The first five space-delimited tokens are the cron fields
// (minute hour dom month dow). Token six is the action name. An optional
// seventh token (everything after the action) is the argument.
//
// Day-of-week values 0 and 7 both mean Sunday (standard cron convention).
// Named days (Mon, Tue, …) and months (Jan, Feb, …) are also supported
// courtesy of robfig/cron.
func Parse(line string) (ScheduleSpec, error) {
	line = strings.TrimSpace(line)
	parts := strings.Fields(line)
	if len(parts) < 6 {
		return ScheduleSpec{}, fmt.Errorf("schedule %q: need at least 6 fields (minute hour dom month dow action), got %d", line, len(parts))
	}

	// Normalize day-of-week: SABnzbd uses 1=Mon … 7=Sun, and standard
	// cron allows both 0 and 7 for Sunday. robfig/cron only accepts 0-6,
	// so rewrite any "7" to "0" in the dow field.
	parts[4] = normalizeDOW(parts[4])

	// Rejoin the first 5 fields as a standard cron expression.
	cronExpr := strings.Join(parts[:5], " ")

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	schedule, err := parser.Parse(cronExpr)
	if err != nil {
		return ScheduleSpec{}, fmt.Errorf("schedule %q: %w", line, err)
	}

	action := parts[5]
	var arg string
	if len(parts) > 6 {
		// Everything after the action name is the argument, preserving spaces.
		actionIdx := strings.Index(line, parts[5])
		arg = strings.TrimSpace(line[actionIdx+len(parts[5]):])
	}

	return ScheduleSpec{
		schedule: schedule,
		Action:   action,
		Arg:      arg,
	}, nil
}

// Matches reports whether the spec fires at time t (minute precision).
func (s ScheduleSpec) Matches(t time.Time) bool {
	// robfig/cron's Next returns the next activation time at or after the
	// given time. To check if t matches, we ask for the next activation
	// at one second before the current minute and see if it falls within
	// the same minute.
	truncated := t.Truncate(time.Minute)
	query := truncated.Add(-1 * time.Second)
	next := s.schedule.Next(query)
	return next.Equal(truncated)
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
			// Compute next from the wall clock *after* Tick completes.
			// If Tick took >1 minute, using tick.Truncate would yield a
			// past time, making time.Until(next) negative and causing an
			// immediate re-fire. Using s.clock() ensures we always sleep
			// until the next whole minute boundary.
			now := s.clock()
			next = now.Truncate(time.Minute).Add(time.Minute)
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

// normalizeDOW rewrites "7" to "0" in a day-of-week cron field. Both mean
// Sunday in traditional cron, but robfig/cron only accepts 0-6. This handles
// bare "7", comma lists like "5,6,7", and ranges like "1-7" (rewritten to
// "1-6,0" so both the range and Sunday are preserved).
func normalizeDOW(field string) string {
	if !strings.Contains(field, "7") {
		return field
	}

	segments := strings.Split(field, ",")
	var out []string
	addSunday := false

	for _, seg := range segments {
		if seg == "7" {
			addSunday = true
			continue
		}
		// Handle ranges like "1-7" → "1-6" + add Sunday.
		if before, after, ok := strings.Cut(seg, "-"); ok {
			if after == "7" {
				out = append(out, before+"-6")
				addSunday = true
				continue
			}
			if before == "7" {
				out = append(out, "0-"+after)
				continue
			}
		}
		// Handle strides like "*/7" — no normalization needed (stride, not value).
		out = append(out, seg)
	}

	if addSunday {
		out = append(out, "0")
	}
	return strings.Join(out, ",")
}
