package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// TestJobFinalizerCancelled_LeavesALaterInstanceAlone: a callback that arrives
// for a removed instance must not cancel the instance a retry has since
// registered under the same ID. A never-run job with cancel intent is evicted
// by the tick, so the retry would disappear.
func TestJobFinalizerCancelled_LeavesALaterInstanceAlone(t *testing.T) {
	app := newTestApplication(t)
	const id = "feedface00584d01"

	j1 := job.New(id, "first", job.Policy{})
	if err := app.dispatcher.Add(context.Background(), j1, dispatch.Header{Name: "first"}); err != nil {
		t.Fatalf("Add(j1): %v", err)
	}
	if err := app.dispatcher.Remove(context.Background(), id); err != nil {
		t.Fatalf("Remove(j1): %v", err)
	}
	j2 := job.New(id, "second", job.Policy{})
	if err := app.dispatcher.Add(context.Background(), j2, dispatch.Header{Name: "second"}); err != nil {
		t.Fatalf("Add(j2): %v", err)
	}

	app.finalizer.cancelled(&postproc.Job{Job: j1})

	if got, ok := app.dispatcher.Job(id); !ok || got != j2 {
		t.Fatalf("dispatcher.Job(%s) = (%p, %v), want the second instance %p", id, got, ok, j2)
	}
	if intent := j2.Intent(); intent == job.IntentCancel {
		t.Errorf("the second instance has intent %v: a callback for the removed first instance cancelled it", intent)
	}
}

// TestJobFinalizerCancelled_LogsACancelFailure: a cancel error that does not
// mean the instance is gone reaches the log. The job has finished its
// Fetching work, so it is not running and the cancel settles it, and settling
// returns a lease the dispatcher's pool never issued, which fails.
func TestJobFinalizerCancelled_LogsACancelFailure(t *testing.T) {
	app := newTestApplication(t)
	var logBuf bytes.Buffer
	app.log = slog.New(slog.NewTextHandler(&logBuf, nil))
	const id = "feedface00584e01"

	j := job.New(id, "foreign-lease", job.Policy{})
	if err := app.dispatcher.Add(context.Background(), j, dispatch.Header{Name: "foreign-lease"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := j.BeginAttempt(time.Now()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	if err := j.SetNext(job.Assessing); err != nil {
		t.Fatalf("SetNext(Assessing): %v", err)
	}
	if err := j.Grant(job.NewLease(58401)); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	app.finalizer.cancelled(&postproc.Job{Job: j})

	if logs := logBuf.String(); !strings.Contains(logs, "cancelling the job failed") {
		t.Errorf("no cancel-failure warning logged; log:\n%s", logs)
	}
}

func TestWarnUnlessGone(t *testing.T) {
	cases := []struct {
		name string
		err  error
		warn bool
	}{
		{name: "nil", err: nil, warn: false},
		{name: "instance gone", err: fmt.Errorf("dispatch: Yielded: no job %q: %w", "j", dispatch.ErrNotFound), warn: false},
		{name: "other failure", err: errors.New("sched: lease is not outstanding"), warn: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			warnUnlessGone(slog.New(slog.NewTextHandler(&logBuf, nil)), "release failed", "j", tc.err)
			if got := strings.Contains(logBuf.String(), "release failed"); got != tc.warn {
				t.Errorf("logged = %v, want %v; log:\n%s", got, tc.warn, logBuf.String())
			}
		})
	}
}
