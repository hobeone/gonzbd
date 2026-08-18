package queue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
)

// A job that is present in q.byID but whose Manifest has been released is
// "non-resident". JobProgress is never released (see
// docs/queue-lifecycle.md) — only the Manifest is evictable. That state is
// ordinary, not exceptional: Add de-hydrates every StatusQueued job when a
// store or state directory is configured, and Pause reaches the same state
// for a job that is already downloading. Every exported Queue method that
// dereferences the manifest must therefore tolerate its absence rather than
// panic — the daemon's worker goroutines carry no recover(), so a nil
// dereference here terminates the process.
//
// These tests pin that contract. They require a state directory: evictJobLocked
// only nils the manifest when q.store != nil || q.stateDir != "", so a queue
// built with a bare New() keeps it populated and the tests would pass
// vacuously against unfixed code.

// newEvictedJobQueue returns a queue holding one paused, non-resident job with
// a single article already marked emitted, plus that job's ID.
func newEvictedJobQueue(t *testing.T) (*Queue, string) {
	t.Helper()

	q := New(WithStateDir(t.TempDir()))
	job := makeMultiFileJob(t, "evicted", 1, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := q.MarkArticleEmittedByIdx(job.ID, artIdxFor(t, q, job.ID, articleID(0, 0))); err != nil {
		t.Fatalf("MarkArticleEmittedByIdx: %v", err)
	}
	if err := q.Pause(job.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	// Guard the fixture itself: if Pause stopped de-hydrating, every assertion
	// below would pass for the wrong reason. Progress is deliberately not
	// part of this check — it is never released (docs/queue-lifecycle.md),
	// so only the manifest is a residency signal.
	q.mu.RLock()
	resident := q.byID[job.ID].manifest != nil
	q.mu.RUnlock()
	if resident {
		t.Fatal("fixture is not exercising the bug: job still resident after Pause")
	}

	return q, job.ID
}

// TestNonResidentJobMethodsDoNotPanic covers every exported Queue method known
// to dereference job.manifest or job.progress. Before the fix, the by-message-ID
// and count/abort entry points panicked here. The by-message-ID verbs are gone
// now, replaced by the index-keyed ones this table still exercises.
//
// ForEachUnfinishedArticle belongs in this table for a different reason than
// the others: it never panicked on the ordinary bare-New()/no-persistence
// fixture, but once JobProgress became permanently resident
// (docs/queue-lifecycle.md) its guard — `job.progress == nil`, equivalent to
// manifest non-residency back when eviction dropped both together — stopped
// protecting the job.manifest.NumFiles() call a few lines below it. A queue
// package test alone would not have caught that regression (internal/queue
// and internal/downloader both still pass with the bad guard restored); it
// only surfaces as a nil-manifest panic in internal/app's buildDispatchPlan,
// in a downloader goroutine with no recover(). This table is what pins it at
// the source instead of relying on a cross-package integration test to catch
// a re-introduction.
func TestNonResidentJobMethodsDoNotPanic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// call invokes the method and returns the error it reported, or nil
		// for methods with no error channel.
		call func(t *testing.T, q *Queue, jobID string) error
		// wantNotResident is true when the method reports non-residency via
		// ErrJobNotResident rather than through a non-error return.
		wantNotResident bool
	}{
		{
			name: "CountUnfinishedArticles",
			call: func(t *testing.T, q *Queue, jobID string) error {
				n, err := q.CountUnfinishedArticles(jobID, 0)
				// The count must not be reported as a successful zero: the
				// caller in the app pipeline fails closed on the error, and a
				// silent 0 would be read as "this file is fully downloaded".
				if err == nil {
					t.Errorf("want error for non-resident job, got count=%d, err=nil", n)
				}
				return err
			},
			wantNotResident: true,
		},
		{
			name: "ClearArticleEmittedByIdx",
			call: func(_ *testing.T, q *Queue, jobID string) error {
				return q.ClearArticleEmittedByIdx(jobID, 0)
			},
			wantNotResident: true,
		},
		{
			name: "MarkArticleEmittedByIdx",
			call: func(_ *testing.T, q *Queue, jobID string) error {
				return q.MarkArticleEmittedByIdx(jobID, 0)
			},
			wantNotResident: true,
		},
		{
			name: "CheckEarlyAbort",
			call: func(t *testing.T, q *Queue, jobID string) error {
				// No error channel. A non-resident job has no live JobProgress,
				// so there is no failure rate to evaluate and nothing to abort.
				if q.CheckEarlyAbort(jobID) {
					t.Error("CheckEarlyAbort = true for a non-resident job, want false")
				}
				return nil
			},
		},
		{
			name: "ForEachUnfinishedArticle",
			call: func(t *testing.T, q *Queue, jobID string) error {
				// No error channel. A non-resident job (manifest nil, progress
				// non-nil) must be skipped before the manifest is ever
				// dereferenced, and must yield no articles — there is no
				// manifest to walk them against.
				q.ForEachUnfinishedArticle(func(a UnfinishedArticle) bool {
					if a.JobID == jobID {
						t.Errorf("non-resident job %s yielded unfinished article %s, want none", jobID, a.MessageID)
					}
					return true
				})
				return nil
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q, jobID := newEvictedJobQueue(t)

			err := tc.call(t, q, jobID)

			if !tc.wantNotResident {
				return
			}
			if !errors.Is(err, ErrJobNotResident) {
				t.Errorf("error = %v, want ErrJobNotResident", err)
			}
			// Non-residency must stay distinguishable from a missing job:
			// three downloader sites suppress ErrNotFound for log hygiene, and
			// collapsing the two would also silence genuine lookup failures.
			if errors.Is(err, ErrNotFound) {
				t.Errorf("error = %v, must not also satisfy ErrNotFound", err)
			}
		})
	}
}

// TestNonResidentJobMissingIsStillNotFound pins the other side of that
// distinction: a job that genuinely is not in the queue reports ErrNotFound,
// not ErrJobNotResident.
func TestNonResidentJobMissingIsStillNotFound(t *testing.T) {
	t.Parallel()

	q := New(WithStateDir(t.TempDir()))

	// A literal index: the queue holds no job at all, so there is nothing to
	// resolve a Message-ID against.
	err := q.ClearArticleEmittedByIdx("no-such-job", 0)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
	if errors.Is(err, ErrJobNotResident) {
		t.Errorf("error = %v, must not report a missing job as non-resident", err)
	}
}

// newStoreBackedPausedJob builds a store-backed queue holding one paused,
// non-resident job whose first article was marked emitted before the pause,
// and returns the queue, the job ID, and every article ID in the job.
//
// A store is required, not merely a state directory: without one, PromoteNext
// rehydrates via newJobProgress, which populates per-file counters but leaves
// job-level pendingArticles at zero, and ForEachUnfinishedArticle then skips
// the job entirely — which would make the caller's assertion unreachable
// rather than true.
//
// setupTestStore lives in package queue_test and is unreachable from here, so
// the store is constructed directly.
func newStoreBackedPausedJob(t *testing.T) (*Queue, string, []string) {
	t.Helper()

	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	repo := history.NewRepository(db)
	q := New(WithStore(NewSQLiteStore(repo.DB(), dir, repo)))

	job := makeMultiFileJob(t, "resume", 1, 3)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Add de-hydrates a StatusQueued job; PromoteNext brings it back so the
	// article can be marked emitted against live progress.
	q.PromoteNext(context.Background())

	if err := q.MarkArticleEmittedByIdx(job.ID, artIdxFor(t, q, job.ID, articleID(0, 0))); err != nil {
		t.Fatalf("MarkArticleEmittedByIdx: %v", err)
	}
	if err := q.Pause(job.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	articles := make([]string, 0, 3)
	for ai := range 3 {
		articles = append(articles, articleID(0, ai))
	}
	return q, job.ID, articles
}

// TestPausedJobArticlesRedispatchedAfterResume is why skipping the work on a
// non-resident job is safe rather than a silent regression. The emitted bit
// that ClearArticleEmittedByIdx would have cleared is transient: it is excluded
// from persistence, and Pause discards the whole JobProgress, so PromoteNext
// rebuilds it from stored ground truth on the way back in. Every article must
// therefore be offered again after Resume even though the clear never ran.
//
// This pins the transience property itself, not the guard — a future change
// that started persisting emitted would break here rather than silently
// stranding articles.
func TestPausedJobArticlesRedispatchedAfterResume(t *testing.T) {
	t.Parallel()

	q, jobID, articles := newStoreBackedPausedJob(t)

	if err := q.Resume(jobID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	q.PromoteNext(context.Background())

	offered := map[string]bool{}
	q.ForEachUnfinishedArticle(func(a UnfinishedArticle) bool {
		if a.JobID == jobID {
			offered[a.MessageID] = true
		}
		return true
	})

	for _, id := range articles {
		if !offered[id] {
			t.Errorf("article %s was not re-offered after resume; the emitted bit survived", id)
		}
	}
	if len(offered) != len(articles) {
		t.Errorf("offered %d articles, want %d", len(offered), len(articles))
	}
}

// newEvictedJobQueueWithDir is newEvictedJobQueue but also returns the state
// directory, so a test can reach in and corrupt the on-disk manifest to force
// a hydration failure on the way back to a resident status (issue #264).
func newEvictedJobQueueWithDir(t *testing.T) (q *Queue, jobID, dir string) {
	t.Helper()

	dir = t.TempDir()
	q = New(WithStateDir(dir))
	job := makeMultiFileJob(t, "evicted", 1, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := q.Pause(job.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	q.mu.RLock()
	resident := q.byID[job.ID].manifest != nil
	q.mu.RUnlock()
	if resident {
		t.Fatal("fixture is not exercising the bug: job still resident after Pause")
	}

	return q, job.ID, dir
}

// TestSetStatusHydrationFailureLeavesStatusUnchanged pins issue #264: SetStatus
// moving a de-hydrated job to a resident status (StatusDownloading) must not
// leave the job at that status with nil manifest when the on-disk manifest
// can't be read. Before the fix, the hydration read was wrapped in
// `if err := readGzJSON(...); err == nil { ... }` with no else branch, so a
// failed read silently left job.Status at the new resident status anyway.
//
// Progress is asserted present rather than nil: hydrateJobLocked's manifest
// read fails before it ever touches job.progress, so the pre-existing,
// always-resident progress (docs/queue-lifecycle.md) is untouched by this
// failure, not dropped.
func TestSetStatusHydrationFailureLeavesStatusUnchanged(t *testing.T) {
	t.Parallel()

	q, jobID, dir := newEvictedJobQueueWithDir(t)

	manifestPath := filepath.Join(dir, "manifests", jobID+".json.gz")
	if err := os.Remove(manifestPath); err != nil {
		t.Fatalf("removing manifest fixture: %v", err)
	}

	err := q.SetStatus(jobID, constants.StatusDownloading)
	if err == nil {
		t.Fatal("SetStatus: want error when manifest hydration fails, got nil")
	}

	q.mu.RLock()
	job := q.byID[jobID]
	status := job.Status
	manifest := job.manifest
	progress := job.progress
	q.mu.RUnlock()

	if status != constants.StatusPaused {
		t.Errorf("job.Status = %s, want %s (unchanged from before the failed call)", status, constants.StatusPaused)
	}
	if manifest != nil {
		t.Errorf("job left partially hydrated: manifest=%v, want nil", manifest)
	}
	if progress == nil {
		t.Error("job progress must never be nil, even after a failed hydration attempt")
	}
}

// TestSetStatusIfHydrationFailureLeavesStatusUnchanged is
// TestSetStatusHydrationFailureLeavesStatusUnchanged for SetStatusIf, which
// (unlike SetStatus) had no hydration branch at all before the fix and so
// left the job resident-but-nil unconditionally on this path. This is the
// path internal/downloader/dispatch.go uses to move Queued jobs to
// Downloading.
//
// The fixture needs a Queued-but-non-resident job. Pause doesn't produce one
// (Paused, not Queued), and Resume immediately re-hydrates via PromoteNext.
// Instead this fills the default 4-slot ActiveSet with resident jobs so a
// 5th job added afterward is promotion-eligible in status but never actually
// promoted/hydrated — Add() de-hydrates every new Queued job up front when a
// state dir is configured, exactly the state PromoteNext leaves jobs in
// while they wait for a slot to free up.
func TestSetStatusIfHydrationFailureLeavesStatusUnchanged(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	q := New(WithStateDir(dir))
	for i := range 4 {
		filler := makeMultiFileJob(t, fmt.Sprintf("filler%d", i), 1, 1)
		if err := q.Add(filler); err != nil {
			t.Fatalf("Add filler %d: %v", i, err)
		}
	}

	job := makeMultiFileJob(t, "overflow", 1, 1)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add overflow job: %v", err)
	}
	jobID := job.ID

	q.mu.RLock()
	queued := q.byID[jobID].Status == constants.StatusQueued
	resident := q.byID[jobID].manifest != nil
	q.mu.RUnlock()
	if !queued {
		t.Fatalf("fixture is not exercising the bug: overflow job status = %s, want Queued", q.byID[jobID].Status)
	}
	if resident {
		t.Fatal("fixture is not exercising the bug: overflow job is resident (ActiveSet not actually full)")
	}

	manifestPath := filepath.Join(dir, "manifests", jobID+".json.gz")
	if err := os.Remove(manifestPath); err != nil {
		t.Fatalf("removing manifest fixture: %v", err)
	}

	err := q.SetStatusIf(jobID, constants.StatusDownloading, constants.StatusQueued)
	if err == nil {
		t.Fatal("SetStatusIf: want error when manifest hydration fails, got nil")
	}

	q.mu.RLock()
	got := q.byID[jobID]
	status := got.Status
	manifest := got.manifest
	progress := got.progress
	q.mu.RUnlock()

	if status != constants.StatusQueued {
		t.Errorf("job.Status = %s, want %s (unchanged from before the failed call)", status, constants.StatusQueued)
	}
	if manifest != nil {
		t.Errorf("job left partially hydrated: manifest=%v, want nil", manifest)
	}
	if progress == nil {
		t.Error("job progress must never be nil, even after a failed hydration attempt")
	}
}
