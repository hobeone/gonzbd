package app

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// TestPersistAndCommit_RefusesARetryWhileItCommits: a finalizer that proceeds
// without the job's transition lock takes its fallback teardown for an
// instance no longer registered. A retry of the job's ID attempted while that
// teardown runs must be refused, or it registers a later instance the
// teardown's by-ID steps then act on. Once the finalizer has returned, a retry
// is admitted.
func TestPersistAndCommit_RefusesARetryWhileItCommits(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newLifecycleTestApp(t)
	stopping, stop := context.WithCancel(t.Context())
	stop()
	application.ctx = stopping
	const id = "feedface00649a01"
	// The FAILED entry a retry needs while the instance is still being
	// finalized: one its own retry could not delete.
	addRetryableEntry(t, repo, adminDir, id, "")

	j1 := job.New(id, "first", job.Policy{})
	if err := application.dispatcher.Add(t.Context(), j1, dispatch.Header{Name: "first"}); err != nil {
		t.Fatalf("Add(j1): %v", err)
	}
	if err := application.dispatcher.Remove(t.Context(), id); err != nil {
		t.Fatalf("Remove(j1): %v", err)
	}

	// Held, with app.ctx ended, so the finalizer proceeds without the lock at
	// once. Released where the finalizer has decided on its fallback teardown,
	// so the retry below is not refused merely because the lock is held.
	claim, err := application.transitions.acquire(t.Context(), id)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	released := false
	defer func() {
		if !released {
			claim.release()
		}
	}()
	var during error
	// The retry must be refused at its claim, not later: the checks further
	// in are for a finalizer that starts after the claim.
	claimedDuring := false
	log := slog.New(onMessage{
		msg: "occupy failed during finalize; proceeding with fallback teardown",
		fn: func() {
			claim.release()
			released = true
			application.retryClaimedHook = func(string) { claimedDuring = true }
			during = application.RetryHistoryJob(context.Background(), id)
			application.retryClaimedHook = nil
		},
	})

	ppJob := &postproc.Job{Job: j1}
	_ = application.finalizer.persistAndCommit(log, completedEntryFor(j1), ppJob)
	if !released {
		t.Fatal("fixture guard: the finalizer never took its fallback teardown")
	}
	if !errors.Is(during, errJobInTransition) {
		t.Errorf("RetryHistoryJob while the finalizer commits = %v, want errJobInTransition", during)
	}
	if claimedDuring {
		t.Error("the retry claimed the ID while its finalizer was committing it")
	}

	if err := application.RetryHistoryJob(context.Background(), id); err != nil {
		t.Errorf("RetryHistoryJob after the finalizer returned = %v, want the retry admitted", err)
	}
}

// TestRetryHistoryJob_RefusedByAFinalizerOfAGivenBackRemoval: a RemoveJob
// whose dispatcher.Remove fails gives its mark back and leaves the instance
// registered and cancelled, and the tick then evicts it as a cancelled job that
// never ran. A retry can find the ID free and pass its early finalizing check,
// and a finalizer of that instance can then start, find no mark, and take its
// fallback teardown without the lock. The retry must be refused before it
// registers, or the ID ends up both queued and filed in history.
//
// Real: RemoveJob on an ended context (so Remove fails and unmarks), the
// dispatcher tick that evicts, the retry, and persistAndCommit's fallback with
// app.ctx ended (the lock bypass). Stood in for:
//   - the FAILED entry beside a live instance, which in production is left by
//     an earlier retry whose history delete failed, is seeded directly;
//   - the hand-over of the never-run instance to post-processing, which in
//     production is a by-ID maybeFinalize caller (pipeline.onJobHopeless, the
//     downloader's OnJobHopeless, or Fail) paused before beginHandOver, is
//     replaced by calling persistAndCommit on the instance directly.
func TestRetryHistoryJob_RefusedByAFinalizerOfAGivenBackRemoval(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newLifecycleTestApp(t)
	const id = "feedface00682a01"
	addRetryableEntry(t, repo, adminDir, id, "")

	j1 := job.New(id, "never-run", job.Policy{})
	if err := application.dispatcher.Add(t.Context(), j1, dispatch.Header{Name: "never-run"}); err != nil {
		t.Fatalf("Add(j1): %v", err)
	}

	ended, end := context.WithCancel(t.Context())
	end()
	if err := application.RemoveJob(ended, id, false); err == nil {
		t.Fatal("fixture guard: RemoveJob on an ended context succeeded, so it never gave its mark back")
	}
	if application.transitions.wasRemoved(j1) {
		t.Fatal("fixture guard: the failed RemoveJob left its mark standing")
	}
	application.dispatcher.Tick(t.Context())
	if _, held := application.dispatcher.Job(id); held {
		t.Fatal("fixture guard: the tick did not evict the cancelled never-run instance")
	}

	// Held in its job_files seed: past its early finalizing check, before the
	// check before it registers.
	retry, blk := startBlockedRetry(t, application, id)

	stopping, stop := context.WithCancel(t.Context())
	stop()
	application.ctx = stopping
	var during error
	released := false
	log := slog.New(onMessage{
		msg: "occupy failed during finalize; proceeding with fallback teardown",
		fn: func() {
			released = true
			close(blk.release)
			during = receiveWithin(t, retry, "the retry")
		},
	})
	ppJob := &postproc.Job{Job: j1}
	_ = application.finalizer.persistAndCommit(log, completedEntryFor(j1), ppJob)
	if !released {
		t.Fatal("fixture guard: the finalizer never took its fallback teardown")
	}

	_, queued := application.dispatcher.Job(id)
	_, histErr := repo.Get(t.Context(), id)
	if queued && histErr == nil {
		t.Errorf("job %s is both queued and filed in history: the retry registered under the finalizer's teardown", id)
	}
	if !errors.Is(during, errJobInTransition) {
		t.Errorf("the retry blocked while the finalizer committed = %v, want errJobInTransition", during)
	}
}

// TestRetryHistoryJob_AFinalizerStartingAfterTheClaimKeepsItsState: a retry
// that claimed the ID before a finalizer of it started finds no instance
// registered, since the finalizer's own CancelJob lets the tick evict a
// never-run job. It must stop before it changes anything, or its manifest
// write, job_files seed and the reclaim its refusal runs take the state the
// finalizer is about to read and file.
func TestRetryHistoryJob_AFinalizerStartingAfterTheClaimKeepsItsState(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newLifecycleTestApp(t)
	const id = "feedface00649a04"
	addRetryableEntry(t, repo, adminDir, id, "")

	// The finalizer's job: its manifest and job_files rows, and no
	// registration, as after the tick evicted it.
	j1, _ := newPar2Job(t, id, "finalizing", []par2FileSpec{
		{subject: "a.bin", bytes: 100},
		{subject: "b.bin", bytes: 100},
		{subject: "c.bin", bytes: 100},
	})
	if err := writeJobManifest(adminDir, j1); err != nil {
		t.Fatalf("writeJobManifest: %v", err)
	}
	m, err := j1.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if err := seedJobFiles(t.Context(), application.durable, id, m.NumFiles(), j1.FileFetchPolicy); err != nil {
		t.Fatalf("seedJobFiles: %v", err)
	}
	rows := jobFilesCount(t, application, id)

	var end func()
	application.retryClaimedHook = func(string) {
		end = application.transitions.beginFinalize(id)
	}
	err = application.RetryHistoryJob(context.Background(), id)
	if end == nil {
		t.Fatal("fixture guard: the retry never claimed the ID")
	}
	if !errors.Is(err, errJobInTransition) {
		t.Errorf("RetryHistoryJob with a finalizer started after its claim = %v, want errJobInTransition", err)
	}
	f, oErr := openManifestIn(manifestDir(adminDir), id)
	if oErr != nil {
		t.Errorf("the finalizer's manifest is gone after the refused retry: %v", oErr)
	} else {
		disk, dErr := decodeManifest(f)
		_ = f.Close()
		if dErr != nil || disk.NumFiles() != m.NumFiles() {
			t.Errorf("the finalizer's manifest was rewritten by the refused retry: %d files (err %v), want %d", disk.NumFiles(), dErr, m.NumFiles())
		}
	}
	if got := jobFilesCount(t, application, id); got != rows {
		t.Errorf("job_files rows = %d after the refused retry, want the finalizer's %d", got, rows)
	}

	end()
	application.retryClaimedHook = nil
	if err := application.RetryHistoryJob(context.Background(), id); err != nil {
		t.Errorf("RetryHistoryJob after the finalizer ended = %v, want the retry admitted", err)
	}
}

// TestPruneHistory_SkipsAJobBeingFinalized: a retention sweep leaves the entry
// of a job a finalizer is committing, which may be filing it, and takes it once
// the finalizer has returned.
func TestPruneHistory_SkipsAJobBeingFinalized(t *testing.T) {
	t.Parallel()
	application, repo, _ := newLifecycleTestApp(t)
	application.config.General.HistoryFailedRetentionDays = 30
	const id = "feedface00649a03"
	if err := repo.Add(t.Context(), history.Entry{
		NzoID: id, Name: id, Status: "Failed", Completed: time.Now().AddDate(0, 0, -90),
	}, nil); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	end := application.transitions.beginFinalize(id)
	if _, err := application.PruneHistory(t.Context()); err != nil {
		t.Fatalf("PruneHistory: %v", err)
	}
	if _, err := repo.Get(t.Context(), id); err != nil {
		t.Errorf("the sweep deleted the entry of a job a finalizer is committing: %v", err)
	}

	end()
	if _, err := application.PruneHistory(t.Context()); err != nil {
		t.Fatalf("second PruneHistory: %v", err)
	}
	if _, err := repo.Get(t.Context(), id); err == nil {
		t.Error("a later sweep did not take the entry once the finalizer had returned")
	}
}
