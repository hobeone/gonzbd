package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- Parse tests -----------------------------------------------------------

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		line    string
		wantErr bool
		action  string
		arg     string
	}{
		{
			name:   "speedlimit weekday range",
			line:   "30 14 * * 1-5 speedlimit 1000",
			action: "speedlimit",
			arg:    "1000",
		},
		{
			name:   "wildcard pause",
			line:   "* * * * * pause",
			action: "pause",
		},
		{
			name:   "stride on hours",
			line:   "0 */4 * * * foo",
			action: "foo",
		},
		{
			name:   "comma list minutes",
			line:   "0,15,30,45 * * * * bar",
			action: "bar",
		},
		{
			name:    "too few fields",
			line:    "30 14 * *",
			wantErr: true,
		},
		{
			name:    "garbage integer in minute",
			line:    "abc 14 * * * pause",
			wantErr: true,
		},
		{
			name:    "minute out of range",
			line:    "60 14 * * * pause",
			wantErr: true,
		},
		{
			name:    "hour out of range",
			line:    "0 25 * * * pause",
			wantErr: true,
		},
		{
			name:    "empty line",
			line:    "",
			wantErr: true,
		},
		{
			name:   "action name matches a field earlier in the line and len is 6",
			line:   "* * * * 1 1",
			action: "1",
			arg:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec, err := Parse(tc.line)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) expected error, got nil", tc.line)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", tc.line, err)
			}

			if spec.Action != tc.action {
				t.Errorf("Action: got %q, want %q", spec.Action, tc.action)
			}
			if spec.Arg != tc.arg {
				t.Errorf("Arg: got %q, want %q", spec.Arg, tc.arg)
			}
		})
	}
}

// ---- Matches tests ---------------------------------------------------------

func TestMatches(t *testing.T) {
	t.Parallel()

	spec, err := Parse("30 14 * * 1-5 speedlimit 1000")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	mon1430 := time.Date(2024, 4, 15, 14, 30, 0, 0, time.UTC) // Monday
	sat1430 := time.Date(2024, 4, 20, 14, 30, 0, 0, time.UTC) // Saturday
	mon1431 := time.Date(2024, 4, 15, 14, 31, 0, 0, time.UTC)

	if !spec.Matches(mon1430) {
		t.Error("expected match at Mon 14:30")
	}
	if spec.Matches(sat1430) {
		t.Error("expected no match at Sat 14:30")
	}
	if spec.Matches(mon1431) {
		t.Error("expected no match at Mon 14:31")
	}
}

func TestMatchesWildcard(t *testing.T) {
	t.Parallel()

	spec, err := Parse("* * * * * pause")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Should match any minute.
	t1 := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2024, 12, 31, 23, 59, 0, 0, time.UTC)
	if !spec.Matches(t1) {
		t.Error("expected wildcard to match any time")
	}
	if !spec.Matches(t2) {
		t.Error("expected wildcard to match any time")
	}
}

func TestMatchesStride(t *testing.T) {
	t.Parallel()

	spec, err := Parse("0 */4 * * * foo")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Should match hours 0, 4, 8, 12, 16, 20.
	for _, h := range []int{0, 4, 8, 12, 16, 20} {
		at := time.Date(2024, 1, 1, h, 0, 0, 0, time.UTC)
		if !spec.Matches(at) {
			t.Errorf("expected match at hour %d", h)
		}
	}
	// Should not match hour 3.
	at := time.Date(2024, 1, 1, 3, 0, 0, 0, time.UTC)
	if spec.Matches(at) {
		t.Error("expected no match at hour 3")
	}
}

func TestMatchesCommaList(t *testing.T) {
	t.Parallel()

	spec, err := Parse("0,15,30,45 * * * * bar")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	for _, m := range []int{0, 15, 30, 45} {
		at := time.Date(2024, 1, 1, 10, m, 0, 0, time.UTC)
		if !spec.Matches(at) {
			t.Errorf("expected match at minute %d", m)
		}
	}
	at := time.Date(2024, 1, 1, 10, 7, 0, 0, time.UTC)
	if spec.Matches(at) {
		t.Error("expected no match at minute 7")
	}
}

func TestMatchesSundayDOW0(t *testing.T) {
	t.Parallel()

	// Standard cron: 0 = Sunday.
	sun := time.Date(2024, 4, 21, 10, 0, 0, 0, time.UTC) // Sunday
	mon := time.Date(2024, 4, 22, 10, 0, 0, 0, time.UTC) // Monday

	spec, err := Parse("0 10 * * 0 act")
	if err != nil {
		t.Fatalf("Parse dow=0: %v", err)
	}
	if !spec.Matches(sun) {
		t.Error("dow=0 should match Sunday")
	}
	if spec.Matches(mon) {
		t.Error("dow=0 should not match Monday")
	}
}

func TestParseNamedDays(t *testing.T) {
	t.Parallel()

	// robfig/cron supports named days — verify we can use them.
	spec, err := Parse("0 10 * * MON-FRI test_action")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	mon := time.Date(2024, 4, 15, 10, 0, 0, 0, time.UTC) // Monday
	sat := time.Date(2024, 4, 20, 10, 0, 0, 0, time.UTC) // Saturday

	if !spec.Matches(mon) {
		t.Error("expected match on Monday for MON-FRI")
	}
	if spec.Matches(sat) {
		t.Error("expected no match on Saturday for MON-FRI")
	}
}

// ---- Tick dispatch tests ---------------------------------------------------

func TestTickDispatch(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	calls := map[string][]string{} // action -> args

	reg := NewRegistry()
	reg.Register("speedlimit", func(_ context.Context, arg string) error {
		mu.Lock()
		calls["speedlimit"] = append(calls["speedlimit"], arg)
		mu.Unlock()
		return nil
	})

	spec, err := Parse("30 14 * * 1-5 speedlimit 1000")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	logger := slog.Default()
	sched := New([]ScheduleSpec{spec}, reg, logger)

	matchTime := time.Date(2024, 4, 15, 14, 30, 0, 0, time.UTC) // Mon
	missTime := time.Date(2024, 4, 15, 14, 29, 0, 0, time.UTC)

	sched.Tick(t.Context(), missTime)
	mu.Lock()
	if len(calls["speedlimit"]) != 0 {
		mu.Unlock()
		t.Errorf("expected no dispatch at miss time, got %d calls", len(calls["speedlimit"]))
	} else {
		mu.Unlock()
	}

	sched.Tick(t.Context(), matchTime)
	mu.Lock()
	if len(calls["speedlimit"]) != 1 || calls["speedlimit"][0] != "1000" {
		mu.Unlock()
		t.Errorf("expected 1 dispatch with arg '1000', got %v", calls["speedlimit"])
	} else {
		mu.Unlock()
	}
}

// ---- Oneshot tests ---------------------------------------------------------

func TestOneshotDue(t *testing.T) {
	t.Parallel()

	q := NewOneshotQueue()

	now := time.Date(2024, 4, 15, 10, 0, 0, 0, time.UTC)
	future := now.Add(5 * time.Minute)

	q.Add(Oneshot{FireAt: now, Action: "resume", Arg: ""})
	q.Add(Oneshot{FireAt: future, Action: "pause", Arg: ""})

	due := q.Due(now)
	if len(due) != 1 {
		t.Fatalf("expected 1 due oneshot, got %d", len(due))
	}
	if due[0].Action != "resume" {
		t.Errorf("expected due action 'resume', got %q", due[0].Action)
	}
	if q.Len() != 1 {
		t.Errorf("expected 1 remaining oneshot, got %d", q.Len())
	}
}

func TestOneshotDispatchedViaScheduler(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	fired := []string{}

	reg := NewRegistry()
	reg.Register("resume", func(_ context.Context, arg string) error {
		mu.Lock()
		fired = append(fired, "resume:"+arg)
		mu.Unlock()
		return nil
	})

	sched := New(nil, reg, slog.Default())

	fireAt := time.Date(2024, 4, 15, 10, 0, 0, 0, time.UTC)
	sched.Oneshots().Add(Oneshot{FireAt: fireAt, Action: "resume", Arg: "server1"})

	sched.Tick(t.Context(), fireAt)

	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 1 || fired[0] != "resume:server1" {
		t.Errorf("expected oneshot to fire with 'resume:server1', got %v", fired)
	}
	if sched.Oneshots().Len() != 0 {
		t.Errorf("expected oneshot queue empty after fire, len=%d", sched.Oneshots().Len())
	}
}

// ---- Handler error isolation test -----------------------------------------

func TestHandlerErrorDoesNotBlockOthers(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	secondFired := false

	reg := NewRegistry()
	reg.Register("fail_action", func(_ context.Context, _ string) error {
		return errors.New("intentional failure")
	})
	reg.Register("ok_action", func(_ context.Context, _ string) error {
		mu.Lock()
		secondFired = true
		mu.Unlock()
		return nil
	})

	failSpec, err := Parse("0 10 * * * fail_action")
	if err != nil {
		t.Fatalf("Parse fail_action: %v", err)
	}
	okSpec, err := Parse("0 10 * * * ok_action")
	if err != nil {
		t.Fatalf("Parse ok_action: %v", err)
	}

	sched := New([]ScheduleSpec{failSpec, okSpec}, reg, slog.Default())
	tick := time.Date(2024, 4, 15, 10, 0, 0, 0, time.UTC)
	sched.Tick(t.Context(), tick)

	mu.Lock()
	defer mu.Unlock()
	if !secondFired {
		t.Error("second handler must fire even when first returns an error")
	}
}

// ---- Unknown action test ---------------------------------------------------

func TestUnknownActionReturnsError(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	err := reg.Dispatch(t.Context(), "nonexistent", "")
	if err == nil {
		t.Fatal("expected error for unknown action, got nil")
	}
	if !errors.Is(err, ErrUnknownAction) {
		t.Errorf("expected ErrUnknownAction, got: %v", err)
	}
}

func TestUnknownActionViaSchedulerContinues(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	knownFired := false

	reg := NewRegistry()
	reg.Register("known", func(_ context.Context, _ string) error {
		mu.Lock()
		knownFired = true
		mu.Unlock()
		return nil
	})

	unknownSpec, err := Parse("0 10 * * * unknown_action")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	knownSpec, err := Parse("0 10 * * * known")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	sched := New([]ScheduleSpec{unknownSpec, knownSpec}, reg, slog.Default())
	sched.Tick(t.Context(), time.Date(2024, 4, 15, 10, 0, 0, 0, time.UTC))

	mu.Lock()
	defer mu.Unlock()
	if !knownFired {
		t.Error("known handler must fire even when preceding unknown action fails")
	}
}

func TestNewWithNilLogger(t *testing.T) {
	t.Parallel()
	sched := New(nil, NewRegistry(), nil)
	if sched.logger == nil {
		t.Error("expected default logger to be set, got nil")
	}
}

func TestTickDispatchLogging(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register("fail_action", func(_ context.Context, _ string) error {
		return errors.New("intentional failure")
	})
	reg.Register("ok_action", func(_ context.Context, _ string) error {
		return nil
	})

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	failSpec, err := Parse("0 10 * * * fail_action")
	if err != nil {
		t.Fatalf("Parse fail_action: %v", err)
	}
	okSpec, err := Parse("0 10 * * * ok_action")
	if err != nil {
		t.Fatalf("Parse ok_action: %v", err)
	}

	sched := New([]ScheduleSpec{failSpec, okSpec}, reg, logger)
	tick := time.Date(2024, 4, 15, 10, 0, 0, 0, time.UTC)

	sched.Tick(t.Context(), tick)

	logOutput := buf.String()
	if !strings.Contains(logOutput, "scheduled action failed") {
		t.Error("expected log warning 'scheduled action failed' for fail_action")
	}
	if strings.Contains(logOutput, "ok_action") {
		t.Error("did not expect warning log for ok_action")
	}
}

func TestOneshotDispatchLogging(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register("fail_action", func(_ context.Context, _ string) error {
		return errors.New("intentional failure")
	})
	reg.Register("ok_action", func(_ context.Context, _ string) error {
		return nil
	})

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	sched := New(nil, reg, logger)
	fireAt := time.Date(2024, 4, 15, 10, 0, 0, 0, time.UTC)
	sched.Oneshots().Add(Oneshot{FireAt: fireAt, Action: "fail_action", Arg: ""})
	sched.Oneshots().Add(Oneshot{FireAt: fireAt, Action: "ok_action", Arg: ""})

	sched.Tick(t.Context(), fireAt)

	logOutput := buf.String()
	if !strings.Contains(logOutput, "one-shot action failed") {
		t.Error("expected log warning 'one-shot action failed' for fail_action")
	}
	if strings.Contains(logOutput, "ok_action") {
		t.Error("did not expect warning log for ok_action")
	}
}
