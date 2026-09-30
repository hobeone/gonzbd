package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// TestJobFinalizerCancelled_LeavesALaterInstancesLaunchClaimAlone: a callback
// that arrives for a removed instance must not release the launch claim of a
// retry the dispatcher has since actually launched under the same ID. The
// second instance is given a real launch claim here, taken by the
// dispatcher's own tick through claimLaunched (internal/dispatch/worker.go),
// not fabricated on the job object: a Runner that never reports (stateRecorder,
// stall_worker_test.go) leaves that claim held until something clears it.
//
// cancelled must release the claim by INSTANCE (YieldedJob), not by ID
// (Yielded): a by-ID release finds whichever job is currently registered
// under the ID, and would park the second instance's live worker and clear
// its claim regardless of which instance the callback names. Two things are
// asserted after cancelled returns: row.View.Running still reports the
// scheduler lease j2 holds, and — the assertion that actually pins
// d.launched[id]'s survival, the same way stall_worker_test.go's
// TestStall_LeavesALiveAssessingWorkerAlone does — a further Tick does not
// relaunch j2: d.launch's claimLaunched (internal/dispatch/worker.go) only
// starts a worker when the claim is not already held, so a cleared claim
// would let this Tick launch j2 a second time.
func TestJobFinalizerCancelled_LeavesALaterInstancesLaunchClaimAlone(t *testing.T) {
	app := newTestApplication(t)
	runner := &stateRecorder{}
	d := dispatch.New(1, 1, time.Hour, time.Now, &appWorkers{app: app},
		nopResidency{}, nopDispatchStore{}, runner)
	app.dispatcher = d

	const id = "feedface00584f01"
	ctx := context.Background()

	j1 := job.New(id, "first", job.Policy{})
	if err := d.Add(ctx, j1, dispatch.Header{Name: "first"}); err != nil {
		t.Fatalf("Add(j1): %v", err)
	}
	if err := d.Remove(ctx, id); err != nil {
		t.Fatalf("Remove(j1): %v", err)
	}

	j2 := job.New(id, "second", job.Policy{})
	if err := d.Add(ctx, j2, dispatch.Header{Name: "second"}); err != nil {
		t.Fatalf("Add(j2): %v", err)
	}
	d.Tick(ctx) // j2 begins at Fetching
	d.Tick(ctx) // j2 granted a lease and launched at Fetching; stateRecorder never reports

	if row, ok := d.Row(id); !ok || !row.View.Running {
		t.Fatalf("precondition: j2 is not running before cancelled: ok=%v view=%+v", ok, row.View)
	}

	app.finalizer.cancelled(&postproc.Job{Job: j1})

	if got, ok := d.Job(id); !ok || got != j2 {
		t.Fatalf("dispatcher.Job(%s) = (%p, %v), want the second instance %p", id, got, ok, j2)
	}
	row, ok := d.Row(id)
	if !ok {
		t.Fatalf("dispatcher.Row(%s): the second instance is no longer registered", id)
	}
	if !row.View.Running {
		t.Errorf("j2's launch claim did not survive a cancelled callback for the removed first instance: %+v", row.View)
	}

	d.Tick(ctx) // a cleared claim would relaunch j2 here
	if got, want := runner.ran(id), []job.State{job.Fetching}; !slices.Equal(got, want) {
		t.Errorf("stateRecorder.ran(%s) = %v, want %v: the launch claim did not survive, and the tick relaunched j2", id, got, want)
	}
}

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
