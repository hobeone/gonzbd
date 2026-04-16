package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// ---- Parse tests -----------------------------------------------------------

func TestParse(t *testing.T) {
	t.Parallel()

	type want struct {
		minutes     []int
		hours       []int
		daysOfMonth []int
		months      []int
		daysOfWeek  []int
		action      string
		arg         string
	}

	tests := []struct {
		name    string
		line    string
		wantErr bool
		want    want
	}{
		{
			name: "speedlimit weekday range",
			line: "30 14 * * 1-5 speedlimit 1000",
			want: want{
				minutes:     []int{30},
				hours:       []int{14},
				daysOfMonth: makeRange(1, 31),
				months:      makeRange(1, 12),
				daysOfWeek:  []int{1, 2, 3, 4, 5},
				action:      "speedlimit",
				arg:         "1000",
			},
		},
		{
			name: "wildcard pause",
			line: "* * * * * pause",
			want: want{
				minutes:     makeRange(0, 59),
				hours:       makeRange(0, 23),
				daysOfMonth: makeRange(1, 31),
				months:      makeRange(1, 12),
				daysOfWeek:  makeRange(0, 7),
				action:      "pause",
				arg:         "",
			},
		},
		{
			name: "stride on hours",
			line: "0 */4 * * * foo",
			want: want{
				minutes:     []int{0},
				hours:       []int{0, 4, 8, 12, 16, 20},
				daysOfMonth: makeRange(1, 31),
				months:      makeRange(1, 12),
				daysOfWeek:  makeRange(0, 7),
				action:      "foo",
				arg:         "",
			},
		},
		{
			name: "comma list minutes",
			line: "0,15,30,45 * * * * bar",
			want: want{
				minutes:     []int{0, 15, 30, 45},
				hours:       makeRange(0, 23),
				daysOfMonth: makeRange(1, 31),
				months:      makeRange(1, 12),
				daysOfWeek:  makeRange(0, 7),
				action:      "bar",
				arg:         "",
			},
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
			name:    "dow out of range",
			line:    "0 0 * * 8 pause",
			wantErr: true,
		},
		{
			name:    "bad stride",
			line:    "0 */0 * * * pause",
			wantErr: true,
		},
		{
			name:    "inverted range",
			line:    "0 5-3 * * * pause",
			wantErr: true,
		},
		{
			name:    "empty line",
			line:    "",
			wantErr: true,
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

			assertIntSlice(t, "Minutes", spec.Minutes, tc.want.minutes)
			assertIntSlice(t, "Hours", spec.Hours, tc.want.hours)
			assertIntSlice(t, "DaysOfMonth", spec.DaysOfMonth, tc.want.daysOfMonth)
			assertIntSlice(t, "Months", spec.Months, tc.want.months)
			assertIntSlice(t, "DaysOfWeek", spec.DaysOfWeek, tc.want.daysOfWeek)

			if spec.Action != tc.want.action {
				t.Errorf("Action: got %q, want %q", spec.Action, tc.want.action)
			}
			if spec.Arg != tc.want.arg {
				t.Errorf("Arg: got %q, want %q", spec.Arg, tc.want.arg)
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

	sched.Tick(context.Background(), missTime)
	mu.Lock()
	if len(calls["speedlimit"]) != 0 {
		mu.Unlock()
		t.Errorf("expected no dispatch at miss time, got %d calls", len(calls["speedlimit"]))
	} else {
		mu.Unlock()
	}

	sched.Tick(context.Background(), matchTime)
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

	sched.Tick(context.Background(), fireAt)

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
	sched.Tick(context.Background(), tick)

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
	err := reg.Dispatch(context.Background(), "nonexistent", "")
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
	sched.Tick(context.Background(), time.Date(2024, 4, 15, 10, 0, 0, 0, time.UTC))

	mu.Lock()
	defer mu.Unlock()
	if !knownFired {
		t.Error("known handler must fire even when preceding unknown action fails")
	}
}

// ---- helpers ---------------------------------------------------------------

func makeRange(lo, hi int) []int {
	s := make([]int, 0, hi-lo+1)
	for i := lo; i <= hi; i++ {
		s = append(s, i)
	}
	return s
}

func assertIntSlice(t *testing.T, field string, got, want []int) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: len mismatch: got %v, want %v", field, got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s[%d]: got %d, want %d (full: got %v, want %v)", field, i, got[i], want[i], got, want)
			return
		}
	}
}
