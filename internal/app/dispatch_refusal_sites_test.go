package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/sched"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// refusalApp is an unstarted application over a fresh env, logging to logs.
func refusalApp(t *testing.T) (*lrApp, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	a := newLREnv(t).newApp(t, func(app *Application) {
		app.log = slog.New(slog.NewTextHandler(logs, nil))
	})
	return a, logs
}

// fetchingJob adds a job and ticks it to Fetching, holding its lease.
func fetchingJob(t *testing.T, a *lrApp, name string) *job.Job {
	t.Helper()
	j := a.addJob(t, name, 2, 1)
	a.dispatcher.Tick(t.Context())
	a.dispatcher.Tick(t.Context())
	if st := j.State().State; st != job.Fetching || !j.HoldsLease() {
		t.Fatalf("fixture: %s is at %v holding a lease = %v, want Fetching holding one", name, st, j.HoldsLease())
	}
	return j
}

// strandUnissuedLease leaves j, a job at Fetching, holding a lease its
// dispatcher's pool no longer counts as issued: the lease is taken, the job
// parked (which reclaims it from the pool), and the lease granted back. The
// next Park or Settle of j then fails the pool's reclaim with
// sched.ErrNotOutstanding, which is how a refusal other than ErrNotFound is
// staged on a real dispatcher.
func strandUnissuedLease(t *testing.T, a *lrApp, j *job.Job) {
	t.Helper()
	l := j.Surrender()
	if l == nil {
		t.Fatal("fixture: the job holds no lease")
	}
	if err := j.Grant(l); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := a.dispatcher.Yielded(j.ID()); err != nil {
		t.Fatalf("Yielded: %v", err)
	}
	if err := j.Grant(l); err != nil {
		t.Fatalf("Grant after the park: %v", err)
	}
}

// TestYieldStoppedWorkers_YieldsOnlyTheNamedStates pins the state filter: a
// Fetching job is left alone by the post-processing states' yield and parked
// by the Fetching one.
func TestYieldStoppedWorkers_YieldsOnlyTheNamedStates(t *testing.T) {
	t.Parallel()
	a, _ := refusalApp(t)
	j := fetchingJob(t, a, "fetching")
	var errs []error
	a.yieldStoppedWorkers(&errs, job.Repairing, job.Extracting, job.Finalizing)
	if !j.HoldsLease() {
		t.Error("the post-processing states' yield parked a job at Fetching")
	}
	a.yieldStoppedWorkers(&errs, job.Fetching)
	if j.HoldsLease() {
		t.Error("the Fetching yield left the job at Fetching holding its lease")
	}
	if len(errs) != 0 {
		t.Errorf("errs = %v, want none", errs)
	}
}

// TestYieldStoppedWorkers_CollectsARefusal pins that a refused yield reaches
// the teardown's errs, and that a nil errs is tolerated.
func TestYieldStoppedWorkers_CollectsARefusal(t *testing.T) {
	t.Parallel()
	a, _ := refusalApp(t)
	j := fetchingJob(t, a, "stranded")
	strandUnissuedLease(t, a, j)
	var errs []error
	a.yieldStoppedWorkers(&errs, job.Fetching)
	if len(errs) != 1 || !errors.Is(errs[0], sched.ErrNotOutstanding) {
		t.Errorf("errs = %v, want one wrapping sched.ErrNotOutstanding", errs)
	}
	a.yieldStoppedWorkers(nil, job.Fetching) // must not panic
}

// TestStopWorkers_YieldsFetchingJobs pins stopWorkers' call: once the
// downloader has stopped, a job at Fetching is parked.
func TestStopWorkers_YieldsFetchingJobs(t *testing.T) {
	t.Parallel()
	a, _ := refusalApp(t)
	j := fetchingJob(t, a, "fetching")
	var errs []error
	a.stopWorkers(time.Second, &errs)
	if j.HoldsLease() {
		t.Error("stopWorkers left a job at Fetching holding its lease")
	}
	if len(errs) != 0 {
		t.Errorf("errs = %v, want none", errs)
	}
}

func stallTestFault() *storagefault.Fault {
	return &storagefault.Fault{Op: "write", Path: "/data/x.bin", Err: syscall.ENOSPC}
}

// TestStall_RefusedPauseIsLogged pins stall's PauseJob site: a refusal other
// than a gone job or a latched cancel is logged at Warn.
func TestStall_RefusedPauseIsLogged(t *testing.T) {
	t.Parallel()
	a, logs := refusalApp(t)
	j := fetchingJob(t, a, "stranded")
	strandUnissuedLease(t, a, j)
	a.Stall(j, stallTestFault())
	if got := logs.String(); !strings.Contains(got, "stall: pausing the job failed") {
		t.Errorf("a refused pause was not logged; log:\n%s", got)
	}
}

// TestStall_PauseRefusedByARemovalIsNotLogged pins the race stall drops: a
// fault landing between RemoveJob's Cancel and its Remove finds the cancel
// latched, PauseJob refuses with job.ErrIntentLatched, and nothing is warned.
func TestStall_PauseRefusedByARemovalIsNotLogged(t *testing.T) {
	t.Parallel()
	a, logs := refusalApp(t)
	j := a.addJob(t, "removed", 2, 1)
	stalled := false
	a.removeCancelGapHook = func(string) {
		a.Stall(j, stallTestFault())
		stalled = true
	}
	if err := a.RemoveJob(t.Context(), j.ID(), false); err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}
	if !stalled {
		t.Fatal("fixture: the stall did not run between the cancel and the removal")
	}
	if got := logs.String(); strings.Contains(got, "stall: pausing the job failed") {
		t.Errorf("a pause refused by a removal's latched cancel was warned about; log:\n%s", got)
	}
}

// TestRemoveJob_RefusedCancelIsLogged pins RemoveJob's early Cancel site: a
// refusal is logged at Warn rather than discarded. The job has recorded its
// next state, so it is not running and the cancel settles it, and the settle's
// reclaim fails.
func TestRemoveJob_RefusedCancelIsLogged(t *testing.T) {
	t.Parallel()
	a, logs := refusalApp(t)
	j := fetchingJob(t, a, "stranded")
	strandUnissuedLease(t, a, j)
	if err := j.SetNext(job.Assessing); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_ = a.RemoveJob(ctx, j.ID(), false) // its own outcome is not what this pins
	if got := logs.String(); !strings.Contains(got, "remove: cancelling the job failed") {
		t.Errorf("a refused cancel was not logged; log:\n%s", got)
	}
}
