package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/job"
)

// TestUnlessGone pins the classification every dispatcher-refusal site in
// this package shares: nil and ErrNotFound, however wrapped, are dropped, and
// any other error is returned unchanged.
func TestUnlessGone(t *testing.T) {
	t.Parallel()
	other := errors.New("sched: lease is not outstanding")
	cases := []struct {
		name string
		err  error
		want error
	}{
		{name: "nil", err: nil, want: nil},
		{name: "gone", err: fmt.Errorf("dispatch: Yielded: no job %q: %w", "j", dispatch.ErrNotFound), want: nil},
		{name: "other", err: other, want: other},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unlessGone(tc.err); got != tc.want { //nolint:errorlint // identity is the assertion
				t.Errorf("unlessGone(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// refusingReporter answers every report with err.
type refusingReporter struct{ err error }

func (r refusingReporter) FinishedJob(*job.Job, job.Outcome) error          { return r.err }
func (r refusingReporter) Yielded(string) error                             { return r.err }
func (r refusingReporter) AdvanceFrom(*job.Job, job.State, job.State) error { return r.err }

// TestAppRunner_RefusedReportIsLoggedUnlessGone pins the runner's handling of
// a refused Yielded or FinishedJob: a refusal for a job that is gone is
// dropped, and any other is logged at Warn, since the job may still hold its
// lease or slot. The no-application branches of runFetch, runAssess and
// runPostProc reach Yielded through yield; failWith reaches FinishedJob.
func TestAppRunner_RefusedReportIsLoggedUnlessGone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	calls := map[string]func(r *appRunner){
		"runFetch":    func(r *appRunner) { r.runFetch(ctx, "j1") },
		"runAssess":   func(r *appRunner) { r.runAssess(ctx, "j1") },
		"runPostProc": func(r *appRunner) { r.runPostProc(ctx, "j1", job.Repairing) },
		"failWith":    func(r *appRunner) { r.failWith(job.New("j1", "n", job.Policy{}), nil) },
	}
	errs := map[string]struct {
		err  error
		warn bool
	}{
		"gone":  {fmt.Errorf("wrapped: %w", dispatch.ErrNotFound), false},
		"other": {errors.New("sched: lease is not outstanding"), true},
	}
	for callName, call := range calls {
		for errName, tc := range errs {
			t.Run(callName+"/"+errName, func(t *testing.T) {
				var logs bytes.Buffer
				r := &appRunner{report: refusingReporter{err: tc.err}, log: slog.New(slog.NewTextHandler(&logs, nil))}
				call(r)
				if got := strings.Contains(logs.String(), "level=WARN"); got != tc.warn {
					t.Errorf("warned = %v, want %v; log:\n%s", got, tc.warn, logs.String())
				}
			})
		}
	}
}

// TestPipelineClearEmitted_RefusalIsLoggedAtError pins clearEmitted's log: a
// result whose instance cannot clear the bit (here, one with no progress) is
// logged at Error rather than dropped.
func TestPipelineClearEmitted_RefusalIsLoggedAtError(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	p := &pipeline{log: slog.New(slog.NewTextHandler(&logs, nil))}
	p.clearEmitted(&downloader.ArticleResult{Job: job.New("j1", "n", job.Policy{}), MessageID: "a@b", ArtIdx: 0})
	if !strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("a refused clear was not logged at Error; log:\n%s", logs.String())
	}
}
