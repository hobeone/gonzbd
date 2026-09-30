package app

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// TestPersistAndCommit_LeavesALaterInstanceAlone: finalizing a job that left
// the dispatcher without a RemoveJob mark, after a new instance was registered
// under the same ID, must not cancel, park or deregister that new instance,
// and must not file the earlier run in history under the shared ID.
func TestPersistAndCommit_LeavesALaterInstanceAlone(t *testing.T) {
	app, repo, _ := newLifecycleTestApp(t)
	const id = "feedface00590a01"

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

	ppJob := &postproc.Job{Job: j1}
	err := app.finalizer.persistAndCommit(app.log, buildHistoryEntry(ppJob), ppJob)
	if !errors.Is(err, errFinalizedJobSuperseded) {
		t.Errorf("persistAndCommit = %v, want errFinalizedJobSuperseded", err)
	}

	got, ok := app.dispatcher.Job(id)
	if !ok || got != j2 {
		t.Fatalf("dispatcher.Job(%s) = (%p, %v) after finalizing the first instance, want the second instance %p still registered", id, got, ok, j2)
	}
	if intent := j2.Intent(); intent == job.IntentCancel {
		t.Errorf("the second instance has intent %v: finalizing the first instance cancelled it", intent)
	}
	if _, err := repo.Get(context.Background(), id); !errors.Is(err, history.ErrNotFound) {
		t.Errorf("history.Get(%s) err = %v, want ErrNotFound: the first instance's run was filed under the second's ID", id, err)
	}
}

// TestPersistAndCommit_LeavesALaterInstancesLaunchClaimAlone: finalizing a
// job that left the dispatcher without a RemoveJob mark, after a new
// instance was registered under the same ID and the dispatcher has since
// actually launched it, must not release that instance's launch claim.
//
// The second instance is given a real launch claim the same way
// TestJobFinalizerCancelled_LeavesALaterInstancesLaunchClaimAlone
// (job_finalizer_cancelled_test.go) does: a Runner that never reports
// (stateRecorder, stall_worker_test.go) leaves the claim held until
// something clears it. Survival is checked both via row.View.Running and via
// a further Tick not relaunching the job — d.launch's claimLaunched
// (internal/dispatch/worker.go) only starts a worker when the claim is not
// already held, so a cleared claim would let this Tick launch j2 a second
// time.
func TestPersistAndCommit_LeavesALaterInstancesLaunchClaimAlone(t *testing.T) {
	app := newTestApplication(t)
	runner := &stateRecorder{}
	d := dispatch.New(1, 1, time.Hour, time.Now, &appWorkers{app: app},
		nopResidency{}, nopDispatchStore{}, runner)
	app.dispatcher = d

	const id = "feedface00590b01"
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
		t.Fatalf("precondition: j2 is not running before persistAndCommit: ok=%v view=%+v", ok, row.View)
	}

	ppJob := &postproc.Job{Job: j1}
	err := app.finalizer.persistAndCommit(app.log, buildHistoryEntry(ppJob), ppJob)
	if !errors.Is(err, errFinalizedJobSuperseded) {
		t.Errorf("persistAndCommit = %v, want errFinalizedJobSuperseded", err)
	}

	row, ok := d.Row(id)
	if !ok {
		t.Fatalf("dispatcher.Row(%s): the second instance is no longer registered", id)
	}
	if !row.View.Running {
		t.Errorf("j2's launch claim did not survive finalizing the removed first instance: %+v", row.View)
	}

	d.Tick(ctx) // a cleared claim would relaunch j2 here
	if got, want := runner.ran(id), []job.State{job.Fetching}; !slices.Equal(got, want) {
		t.Errorf("stateRecorder.ran(%s) = %v, want %v: the launch claim did not survive, and the tick relaunched j2", id, got, want)
	}
}

// onMessage is a slog.Handler that runs fn when a record with message msg is
// logged: a seam at a point persistAndCommit logs and has no hook for.
type onMessage struct {
	msg string
	fn  func()
}

func (h onMessage) Enabled(context.Context, slog.Level) bool { return true }
func (h onMessage) Handle(_ context.Context, r slog.Record) error {
	if r.Message == h.msg {
		h.fn()
	}
	return nil
}
func (h onMessage) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h onMessage) WithGroup(string) slog.Handler      { return h }

// TestPersistAndCommit_WithoutTheLock_LeavesARetryRegisteredDuringTheTeardown:
// a finalizer that proceeds without the job's transition lock does not exclude
// a retry, so a later instance can be registered under the job's ID after the
// finalizer found the ID free. Its teardown must then leave that instance
// registered and uncancelled.
func TestPersistAndCommit_WithoutTheLock_LeavesARetryRegisteredDuringTheTeardown(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	stopping, stop := context.WithCancel(t.Context())
	stop()
	application.ctx = stopping
	const id = "feedface00593a01"

	j1 := job.New(id, "first", job.Policy{})
	if err := application.dispatcher.Add(t.Context(), j1, dispatch.Header{Name: "first"}); err != nil {
		t.Fatalf("Add(j1): %v", err)
	}
	if err := application.dispatcher.Remove(t.Context(), id); err != nil {
		t.Fatalf("Remove(j1): %v", err)
	}
	// Held, with app.ctx ended, so the finalizer proceeds without the lock
	// at once, as it does once finalizeTransitionWait has passed.
	claim, err := application.transitions.acquire(t.Context(), id)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer claim.release()

	// Registered where the finalizer has decided on its fallback teardown,
	// standing in for a RetryHistoryJob the missing lock did not exclude.
	j2 := job.New(id, "second", job.Policy{})
	var addErr error
	log := slog.New(onMessage{
		msg: "occupy failed during finalize; proceeding with fallback teardown",
		fn: func() {
			addErr = application.dispatcher.Add(t.Context(), j2, dispatch.Header{Name: "second"})
		},
	})

	ppJob := &postproc.Job{Job: j1}
	_ = application.finalizer.persistAndCommit(log, completedEntryFor(j1), ppJob)
	if addErr != nil {
		t.Fatalf("Add(j2): %v", addErr)
	}

	got, ok := application.dispatcher.Job(id)
	if !ok || got != j2 {
		t.Fatalf("dispatcher.Job(%s) = (%p, %v) after the first instance's teardown, want the second instance %p still registered", id, got, ok, j2)
	}
	if intent := j2.Intent(); intent == job.IntentCancel {
		t.Errorf("the second instance has intent %v: the first instance's teardown cancelled it", intent)
	}
	if row, _ := application.dispatcher.Row(id); row.Header.OperationalError != "" {
		t.Errorf("the second instance carries the operational error %q from the first instance's teardown", row.Header.OperationalError)
	}
}
