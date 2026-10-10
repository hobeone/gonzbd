package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// Pins on verifyPausedJobs: the startup verification of the jobs restored
// paused at Fetching, which runs after Start has returned.

// pauseAtFetching adds a job to a with two of file A's four articles written
// and recorded, takes it to Fetching and pauses it there, so that a restart
// over the same env restores it paused at Fetching with rows to verify.
func pauseAtFetching(t *testing.T, env *lrEnv, a *lrApp, name string) *job.Job {
	t.Helper()
	j := a.addJob(t, name, 4, 2)
	env.writeFileA(t, j, 4, 0, 1)
	a.recordFileA(t, j.ID(), false, 0, 1)
	a.dispatcher.Tick(t.Context())
	a.dispatcher.Tick(t.Context())
	if st := j.State().State; st != job.Fetching {
		t.Fatalf("fixture: %s is at %v, want Fetching", name, st)
	}
	if err := a.dispatcher.PauseJob(j.ID()); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	a.dispatcher.Tick(t.Context())
	return j
}

// blockingLoadHook returns a pausedVerifiedHook that reports each load on
// entered and then blocks until release is closed.
func blockingLoadHook(entered chan<- string, release <-chan struct{}) func(string, error) {
	return func(id string, _ error) {
		select {
		case entered <- id:
		default:
		}
		<-release
	}
}

// awaitEntered receives one load's job ID from entered.
func awaitEntered(t *testing.T, entered <-chan string) string {
	t.Helper()
	select {
	case id := <-entered:
		return id
	case <-time.After(10 * time.Second):
		t.Fatal("the paused job was never loaded")
		return ""
	}
}

// TestVerifyPausedJobs_StartReturnsBeforeTheLoadEnds pins the placement: the
// paused jobs' verification is not on Start's path, so the API can serve
// while it runs, and its result still lands on the job.
func TestVerifyPausedJobs_StartReturnsBeforeTheLoadEnds(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	j := pauseAtFetching(t, env, env.newApp(t), "paused")

	entered := make(chan string, 1)
	release := make(chan struct{})
	a2 := env.newApp(t, func(app *Application) {
		app.pausedVerifiedHook = blockingLoadHook(entered, release)
	})
	started := make(chan error, 1)
	go func() { started <- a2.Start(t.Context()) }()
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(10 * time.Second):
		close(release)
		t.Cleanup(func() {
			if err := <-started; err == nil {
				a2.StopAndJoin(t)
			}
		})
		t.Fatal("Start did not return while a paused job's verification was still running")
	}
	t.Cleanup(func() { a2.StopAndJoin(t) })

	awaitEntered(t, entered)
	close(release)
	j2 := a2.registered(t, j.ID())
	lrWaitFor(t, "the paused job's verified progress", j2.HasProgress)
	if !articleDone(j2, 0) || !articleDone(j2, 1) {
		t.Error("the paused job's verified articles are not Done")
	}
	if expected, remaining, _ := j2.ProgressFigures(); remaining >= expected {
		t.Errorf("remaining %d of %d after the verification landed; the queue would show 0%%", remaining, expected)
	}
}

// TestVerifyPausedJobs_OwedFilingIsNotDeferredBehindIt pins the ordering
// fileOwedUnwantedFailures keeps: a restored job the archive peek owes a
// filing is filed inside Start, before the first tick, whether or not the
// paused jobs' verification has run.
func TestVerifyPausedJobs_OwedFilingIsNotDeferredBehindIt(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a1 := env.newApp(t)
	paused := pauseAtFetching(t, env, a1, "paused")
	owed := a1.addJob(t, "owed", 2, 1)
	a1.dispatcher.Tick(t.Context())
	a1.dispatcher.Tick(t.Context())
	if st := owed.State().State; st != job.Fetching {
		t.Fatalf("fixture: the owed job is at %v, want Fetching", st)
	}
	if moved, err := a1.dispatcher.BlockUnwanted(owed, false); err != nil || !moved {
		t.Fatalf("BlockUnwanted = (%v, %v)", moved, err)
	}
	a1.dispatcher.Tick(t.Context()) // persists the block

	entered := make(chan string, 1)
	release := make(chan struct{})
	a2 := env.newApp(t, func(app *Application) {
		app.pausedVerifiedHook = blockingLoadHook(entered, release)
	})
	a2.start(t)
	t.Cleanup(func() { close(release) })

	owed2 := a2.registered(t, owed.ID())
	if !a2.postProcAdmissions.has(owed2) {
		t.Error("the owed filing was not made inside Start; it waited behind the paused jobs' verification")
	}
	paused2 := a2.registered(t, paused.ID())
	if paused2.Intent() != job.IntentPause {
		t.Errorf("the paused job's intent is %v after Start, want IntentPause", paused2.Intent())
	}
}

// TestVerifyPausedJobs_DeadlineLeavesTheJobUnloaded pins the per-job
// deadline: a load that exceeds it ends with the deadline's error, attaches
// nothing, parks nothing and deletes no row, so the job's resume verifies it
// again.
func TestVerifyPausedJobs_DeadlineLeavesTheJobUnloaded(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	j := pauseAtFetching(t, env, env.newApp(t), "slow")

	got := make(chan error, 1)
	a2 := env.newApp(t, func(app *Application) {
		app.pausedVerifyTimeout = time.Nanosecond
		app.pausedVerifiedHook = func(_ string, err error) {
			select {
			case got <- err:
			default:
			}
		}
	})
	a2.start(t)
	var err error
	select {
	case err = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the paused job was never loaded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("load = %v, want context.DeadlineExceeded", err)
	}
	j2 := a2.registered(t, j.ID())
	if j2.HasProgress() {
		t.Error("a load that exceeded its deadline attached progress")
	}
	if got := rowsFor(t, a2.repo.DB(), j.ID()); len(got) != 2 {
		t.Errorf("%d rows after the deadline, want the 2 recorded: an abandoned load deletes nothing", len(got))
	}
	if reason := a2.StallReason(j.ID()).Reason; reason != "" {
		t.Errorf("the deadline parked the job: %q", reason)
	}
}

// TestVerifyPausedJobs_StopsAtShutdown pins the cancellation: once Shutdown
// has cancelled app.ctx, the load in flight ends without attaching anything,
// no further paused job is loaded, and Shutdown joins the verifier.
//
// Not parallel: preadAt is a package-level seam.
func TestVerifyPausedJobs_StopsAtShutdown(t *testing.T) {
	env := newLREnv(t)
	a1 := env.newApp(t)
	first := pauseAtFetching(t, env, a1, "first")
	second := pauseAtFetching(t, env, a1, "second")

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	blockingPread(t, entered, release)
	a2 := env.newApp(t)
	a2.start(t)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the verification never read the first job's file")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- a2.Shutdown() }()
	lrWaitFor(t, "Shutdown to cancel the app's context", func() bool { return a2.ctx.Err() != nil })
	close(release)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return")
	}
	if a2.registered(t, first.ID()).HasProgress() {
		t.Error("the load in flight attached progress after Shutdown cancelled it")
	}
	if a2.registered(t, second.ID()).HasProgress() {
		t.Error("a paused job was loaded after Shutdown cancelled the verification")
	}
}

// TestVerifyPausedJobs_ShutdownJoinsTheVerifier pins that the verifier runs
// on app.wg: a Shutdown while a load is in flight waits for it, up to the
// step budget, rather than leaving it running behind the stopped app.
func TestVerifyPausedJobs_ShutdownJoinsTheVerifier(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	pauseAtFetching(t, env, env.newApp(t), "joined")

	entered := make(chan string, 1)
	release := make(chan struct{})
	a2 := env.newApp(t, func(app *Application) {
		app.shutdownStepTimeout = 200 * time.Millisecond
		app.pausedVerifiedHook = blockingLoadHook(entered, release)
	})
	a2.start(t)
	awaitEntered(t, entered)
	err := a2.Shutdown()
	close(release)
	if err == nil || !strings.Contains(err.Error(), "wg wait") {
		t.Errorf("Shutdown = %v during a load in flight, want the wg.Wait step to time out: the verifier is not joined", err)
	}
}

// blockingPread replaces the preadAt seam with one whose first call reports
// on entered and blocks until release is closed; later calls read. A first
// call left blocked for ten seconds fails instead, so a verification that
// wrongly runs on a test's own goroutine cannot hang the test.
//
// Not for a parallel test: preadAt is a package-level seam.
func blockingPread(t *testing.T, entered chan<- struct{}, release <-chan struct{}) {
	t.Helper()
	prev := preadAt
	var once bool
	preadAt = func(f *os.File, b []byte, off int64) (int, error) {
		if !once {
			once = true
			entered <- struct{}{}
			select {
			case <-release:
			case <-time.After(10 * time.Second):
				return 0, errors.New("blockingPread: the read was never released")
			}
		}
		return prev(f, b, off)
	}
	t.Cleanup(func() { preadAt = prev })
}

// TestVerifyPausedJobs_ResumeDuringTheLoad pins what a resume during the
// read-back meets: the tick's hydration waits for the load in flight, the job
// ends resident with the verified progress and running, and nothing settles
// it. Until the load lands, the queue row reports the header's bytes
// remaining.
//
// Not parallel: preadAt is a package-level seam.
func TestVerifyPausedJobs_ResumeDuringTheLoad(t *testing.T) {
	env := newLREnv(t)
	j := pauseAtFetching(t, env, env.newApp(t), "resumed")

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	blockingPread(t, entered, release)
	a2 := env.newApp(t)
	a2.start(t)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the verification never read the file")
	}
	row, ok := a2.dispatcher.Row(j.ID())
	if !ok {
		t.Fatal("the job is not registered")
	}
	if row.RemainingBytes != row.Header.Bytes {
		t.Errorf("remaining = %d of %d while the load is in flight, want the header's bytes", row.RemainingBytes, row.Header.Bytes)
	}
	if err := a2.dispatcher.ResumeJobByUser(j.ID()); err != nil {
		t.Fatalf("ResumeJobByUser: %v", err)
	}
	close(release)

	j2 := a2.registered(t, j.ID())
	lrWaitFor(t, "the resumed job to hold its lease on the verified content", func() bool {
		r, ok := a2.dispatcher.Row(j.ID())
		return ok && r.View.Holds && j2.Resident() && articleDone(j2, 1)
	})
	if !articleDone(j2, 0) {
		t.Error("a verified article is not Done after the resume")
	}
	if o := j2.State().Outcome; o != job.OutcomePending {
		t.Errorf("outcome = %v after a resume during the load, want none", o)
	}
	if reason := a2.StallReason(j.ID()).Reason; reason != "" {
		t.Errorf("the job is parked after a resume during the load: %q", reason)
	}
}

// TestVerifyPausedJobs_RemoveDuringTheLoad pins what a removal during the
// read-back meets: RemoveJob waits for the load in flight and then removes
// the job, its rows and its manifest, and the load leaves nothing behind.
//
// Not parallel: preadAt is a package-level seam.
func TestVerifyPausedJobs_RemoveDuringTheLoad(t *testing.T) {
	env := newLREnv(t)
	j := pauseAtFetching(t, env, env.newApp(t), "removed")

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	blockingPread(t, entered, release)
	a2 := env.newApp(t)
	a2.start(t)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the verification never read the file")
	}
	removed := make(chan error, 1)
	go func() { removed <- a2.RemoveJob(t.Context(), j.ID(), true) }()
	close(release)
	select {
	case err := <-removed:
		if err != nil {
			t.Fatalf("RemoveJob: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RemoveJob did not return after the load it waited on ended")
	}
	if _, ok := a2.dispatcher.Job(j.ID()); ok {
		t.Error("the job is still registered after RemoveJob")
	}
	if got := rowsFor(t, a2.repo.DB(), j.ID()); len(got) != 0 {
		t.Errorf("%d rows after RemoveJob, want none", len(got))
	}
	if _, err := os.Stat(env.filePath(j)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file A after RemoveJob with deleteFiles: stat err = %v, want not-exist", err)
	}
	lrWaitFor(t, "the verifier to finish", func() bool { return waitResumedSenders(a2.Application) == 0 })
}
