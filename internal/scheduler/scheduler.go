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
	"unicode"

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
// Day-of-week uses standard cron convention: 0=Sunday … 6=Saturday.
// Named days (Mon, Tue, …) and months (Jan, Feb, …) are also supported
// courtesy of robfig/cron.
func Parse(line string) (ScheduleSpec, error) {
	line = strings.TrimSpace(line)
	parts := strings.Fields(line)
	if len(parts) < 6 {
		return ScheduleSpec{}, fmt.Errorf("schedule %q: need at least 6 fields (minute hour dom month dow action), got %d", line, len(parts))
	}

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
		// Find the start of the action name in line.
		// Since there could be duplicate words (e.g. "1 1"), we cannot just use strings.Index(line, action).
		// We must find the index of the 6th word (parts[5]) by skipping the first 5 words.
		idx := 0
		for range 5 {
			// Skip leading whitespace of the current word
			for idx < len(line) && unicode.IsSpace(rune(line[idx])) {
				idx++
			}
			// Skip the current word characters
			for idx < len(line) && !unicode.IsSpace(rune(line[idx])) {
				idx++
			}
		}
		actionIdx := strings.Index(line[idx:], action)
		if actionIdx != -1 {
			actionIdx += idx
			arg = strings.TrimSpace(line[actionIdx+len(action):])
		}
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
