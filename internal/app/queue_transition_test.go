package app

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// TestRemoveJob_WaitsForAnInFlightTransition: while another actor holds the
// job, RemoveJob neither takes it out of the queue nor reclaims its rows, and
// once the holder releases it removes the job as asked.
func TestRemoveJob_WaitsForAnInFlightTransition(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 2)
	seedDurability(t, application, j.ID())

	claim, err := application.transitions.acquire(t.Context(), j.ID())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	removed := make(chan error, 1)
	go func() { removed <- application.RemoveJob(context.Background(), j.ID(), false) }()
	requireStillWaiting(t, removed, "RemoveJob")
	if _, held := application.dispatcher.Job(j.ID()); !held {
		t.Error("RemoveJob took the job out of the queue while another actor held it")
	}
	if runs, _ := durabilityRowCounts(t, application, j.ID()); runs != 1 {
		t.Errorf("written rows = %d, want 1: RemoveJob reclaimed rows another actor holds", runs)
	}
	claim.release()

	if err := receiveWithin(t, removed, "RemoveJob"); err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}
	if _, held := application.dispatcher.Job(j.ID()); held {
		t.Error("the job is still queued after RemoveJob returned")
	}
	assertRowsGone(t, application, j.ID(), "a job removed once the holder released it")
}

// completedEntryFor is the history entry a successful finalization of j files.
func completedEntryFor(j *job.Job) history.Entry {
	return history.Entry{NzoID: j.ID(), Name: j.Name(), Status: "Completed", Completed: time.Now()}
}

// TestFinalize_WaitsForAnInFlightTransition: while another actor holds the
// job, the finalizer neither files it in history nor takes it out of the
// queue, and once the holder releases it does both.
func TestFinalize_WaitsForAnInFlightTransition(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 2)
	application.ctx = t.Context()

	claim, err := application.transitions.acquire(t.Context(), j.ID())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	committed := make(chan error, 1)
	go func() {
		committed <- application.finalizer.persistAndCommit(slog.Default(), completedEntryFor(j), &postproc.Job{Job: j})
	}()
	requireStillWaiting(t, committed, "persistAndCommit")
	if _, err := application.historyRepo.Get(t.Context(), j.ID()); !errors.Is(err, history.ErrNotFound) {
		t.Errorf("history.Get err = %v, want ErrNotFound: the finalizer filed a job another actor held", err)
	}
	if _, held := application.dispatcher.Job(j.ID()); !held {
		t.Error("the finalizer took the job out of the queue while another actor held it")
	}
	claim.release()

	if err := receiveWithin(t, committed, "persistAndCommit"); err != nil {
		t.Fatalf("persistAndCommit: %v", err)
	}
	if _, err := application.historyRepo.Get(t.Context(), j.ID()); err != nil {
		t.Errorf("the job is not in history once the holder released it: %v", err)
	}
}

// TestFinalize_DoesNotFileAJobRemovedWhileItWaited: a finalizer that waits out
// a RemoveJob of its job finds that instance marked removed once it holds the
// lock, and files nothing: the job the user removed must not reappear in
// history.
func TestFinalize_DoesNotFileAJobRemovedWhileItWaited(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 2)
	application.ctx = t.Context()

	committed := make(chan error, 1)
	application.removeJobHook = func(string) {
		go func() {
			committed <- application.finalizer.persistAndCommit(slog.Default(), completedEntryFor(j), &postproc.Job{Job: j})
		}()
		requireStillWaiting(t, committed, "persistAndCommit")
	}
	if err := application.RemoveJob(t.Context(), j.ID(), false); err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}

	if err := receiveWithin(t, committed, "persistAndCommit"); !errors.Is(err, errFinalizedJobRemoved) {
		t.Errorf("persistAndCommit err = %v, want errFinalizedJobRemoved for a job removed while it waited", err)
	}
	if _, err := application.historyRepo.Get(t.Context(), j.ID()); !errors.Is(err, history.ErrNotFound) {
		t.Errorf("history.Get err = %v, want ErrNotFound: the finalizer filed a job the user removed", err)
	}
}

// TestFinalize_DoesNotFileAJobRemovedWithoutTheLock: a finalizer that proceeds
// without the lock, because the process is stopping, while a RemoveJob of its
// job is under way files nothing either.
func TestFinalize_DoesNotFileAJobRemovedWithoutTheLock(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 2)
	stopping, stop := context.WithCancel(t.Context())
	stop()
	application.ctx = stopping

	var finalizeErr error
	application.removeJobHook = func(string) {
		finalizeErr = application.finalizer.persistAndCommit(slog.Default(), completedEntryFor(j), &postproc.Job{Job: j})
	}
	if err := application.RemoveJob(t.Context(), j.ID(), false); err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}

	if !errors.Is(finalizeErr, errFinalizedJobRemoved) {
		t.Errorf("persistAndCommit err = %v, want errFinalizedJobRemoved", finalizeErr)
	}
	if _, err := application.historyRepo.Get(t.Context(), j.ID()); !errors.Is(err, history.ErrNotFound) {
		t.Errorf("history.Get err = %v, want ErrNotFound: the finalizer filed a job the user removed", err)
	}
}

// TestFinalize_FilesAJobThatLeftTheQueueWithoutARemoval: a job gone from the
// dispatcher that no RemoveJob took is still filed. The tick evicts a
// never-run job once the finalizer's own Cancel has marked it cancelled, and
// the retry of a complete job and startup both finalize never-run jobs.
func TestFinalize_FilesAJobThatLeftTheQueueWithoutARemoval(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 2)
	application.ctx = t.Context()
	if err := application.dispatcher.Remove(t.Context(), j.ID()); err != nil {
		t.Fatalf("the eviction stand-in: %v", err)
	}

	if err := application.finalizer.persistAndCommit(slog.Default(), completedEntryFor(j), &postproc.Job{Job: j}); err != nil {
		t.Fatalf("persistAndCommit: %v", err)
	}
	if _, err := application.historyRepo.Get(t.Context(), j.ID()); err != nil {
		t.Errorf("the job is not in history: %v; a completed job that left the queue without a removal was lost", err)
	}
}

// TestFinalize_SkipsAJobWhoseRemovalFailed: a RemoveJob whose dispatcher.Remove
// fails keeps its mark on the instance it leaves registered. A finalizer of
// that instance files nothing and leaves it registered, and a later RemoveJob
// still takes it.
func TestFinalize_SkipsAJobWhoseRemovalFailed(t *testing.T) {
	t.Parallel()
	application, j, _ := newAppWithCustomDispatchStore(t, 1)
	application.ctx = t.Context()

	if err := application.RemoveJob(t.Context(), j.ID(), false); err == nil {
		t.Fatal("RemoveJob succeeded; the store was set to fail its first delete")
	}
	if _, held := application.dispatcher.Job(j.ID()); !held {
		t.Fatal("the failed removal took the job out of the queue")
	}

	err := application.finalizer.persistAndCommit(slog.Default(), completedEntryFor(j), &postproc.Job{Job: j})
	if !errors.Is(err, errFinalizedJobRemoved) {
		t.Errorf("persistAndCommit on a job whose removal failed = %v, want errFinalizedJobRemoved", err)
	}
	if _, err := application.historyRepo.Get(t.Context(), j.ID()); err == nil {
		t.Error("the job whose removal failed was filed in history")
	}
	if _, held := application.dispatcher.Job(j.ID()); !held {
		t.Fatal("the skipped finalizer took the job out of the queue")
	}

	if err := application.RemoveJob(t.Context(), j.ID(), false); err != nil {
		t.Errorf("a later RemoveJob of the job = %v, want it removed", err)
	}
	if _, held := application.dispatcher.Job(j.ID()); held {
		t.Error("the job is still queued after a later RemoveJob succeeded")
	}
}

// TestFinalize_ProceedsWithoutTheLockOnceTheProcessIsStopping pins the wait's
// bound: it ends with app.ctx, so a finalizer running during shutdown does not
// spend any of its step's budget on a holder, and still commits the job.
func TestFinalize_ProceedsWithoutTheLockOnceTheProcessIsStopping(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 2)
	stopping, stop := context.WithCancel(t.Context())
	stop()
	application.ctx = stopping

	claim, err := application.transitions.acquire(t.Context(), j.ID())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer claim.release()

	start := time.Now()
	if err := application.finalizer.persistAndCommit(slog.Default(), completedEntryFor(j), &postproc.Job{Job: j}); err != nil {
		t.Fatalf("persistAndCommit: %v", err)
	}
	if took := time.Since(start); took >= finalizeTransitionWait {
		t.Errorf("persistAndCommit took %v on a stopping process: it waited out the "+
			"%v cap instead of ending with app.ctx", took, finalizeTransitionWait)
	}
	if _, err := application.historyRepo.Get(t.Context(), j.ID()); err != nil {
		t.Errorf("the job is not in history: %v", err)
	}
}
