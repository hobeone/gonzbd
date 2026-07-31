package queue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
)

// A job that is present in q.byID but whose Manifest and JobProgress have been
// released is "non-resident". That state is ordinary, not exceptional: Add
// de-hydrates every StatusQueued job when a store or state directory is
// configured, and Pause reaches the same state for a job that is already
// downloading. Every exported Queue method that dereferences those fields must
// therefore tolerate it rather than panic — the daemon's worker goroutines
// carry no recover(), so a nil dereference here terminates the process.
//
// These tests pin that contract. They require a state directory: evictJobLocked
// only nils the fields when q.store != nil || q.stateDir != "", so a queue built
// with a bare New() keeps them populated and the tests would pass vacuously
// against unfixed code.

// newEvictedJobQueue returns a queue holding one paused, non-resident job with
// a single article already marked emitted, plus that job's ID.
func newEvictedJobQueue(t *testing.T) (*Queue, string) {
	t.Helper()

	q := New(WithStateDir(t.TempDir()))
	job := makeMultiFileJob(t, "evicted", 1, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := q.MarkArticleEmitted(job.ID, articleID(0, 0)); err != nil {
		t.Fatalf("MarkArticleEmitted: %v", err)
	}
	if err := q.Pause(job.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	// Guard the fixture itself: if Pause stopped de-hydrating, every assertion
	// below would pass for the wrong reason.
	q.mu.RLock()
	resident := q.byID[job.ID].manifest != nil || q.byID[job.ID].progress != nil
	q.mu.RUnlock()
	if resident {
		t.Fatal("fixture is not exercising the bug: job still resident after Pause")
	}

	return q, job.ID
}

// TestNonResidentJobMethodsDoNotPanic covers every exported Queue method known
// to dereference job.manifest or job.progress. Before the fix, the four
// by-message-ID and count/abort entry points panicked here.
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
			name: "ClearArticleEmitted",
			call: func(_ *testing.T, q *Queue, jobID string) error {
				return q.ClearArticleEmitted(jobID, articleID(0, 0))
			},
			wantNotResident: true,
		},
		{
			name: "MarkArticleEmitted",
			call: func(_ *testing.T, q *Queue, jobID string) error {
				return q.MarkArticleEmitted(jobID, articleID(0, 1))
			},
			wantNotResident: true,
		},
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
			name: "RecordDownload",
			call: func(_ *testing.T, q *Queue, jobID string) error {
				return q.RecordDownload(jobID, "news.example.com", 1024)
			},
			wantNotResident: true,
		},
		{
			name: "MarkArticlesDone",
			call: func(_ *testing.T, q *Queue, jobID string) error {
				return q.MarkArticlesDone(jobID, []string{articleID(0, 0)})
			},
			wantNotResident: true,
		},
		{
			name: "MarkArticlesDoneByIdx",
			call: func(_ *testing.T, q *Queue, jobID string) error {
				return q.MarkArticlesDoneByIdx(jobID, []int32{0})
			},
			wantNotResident: true,
		},
		{
			name: "MarkArticlesFailed",
			call: func(_ *testing.T, q *Queue, jobID string) error {
				_, err := q.MarkArticlesFailed(jobID, []string{articleID(0, 0)})
				return err
			},
			wantNotResident: true,
		},
		{
			name: "MarkArticlesFailedByIdx",
			call: func(_ *testing.T, q *Queue, jobID string) error {
				_, err := q.MarkArticlesFailedByIdx(jobID, []int32{0})
				return err
			},
			wantNotResident: true,
		},
		{
			name: "UndeferRecoveryVolumes",
			call: func(_ *testing.T, q *Queue, jobID string) error {
				return q.UndeferRecoveryVolumes(jobID, []int{0})
			},
			wantNotResident: true,
		},
		{
			name: "SetFileCRC32",
			call: func(_ *testing.T, q *Queue, jobID string) error {
				return q.SetFileCRC32(jobID, 0, 0x12345678)
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

	err := q.ClearArticleEmitted("no-such-job", articleID(0, 0))
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

	if err := q.MarkArticleEmitted(job.ID, articleID(0, 0)); err != nil {
		t.Fatalf("MarkArticleEmitted: %v", err)
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
// that ClearArticleEmitted would have cleared is transient: it is excluded
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

// TestRetry_NonResidentJobResetsFailedArticles pins the fix for issue #260:
// when a job is in StatusFailed and de-hydrated, calling Queue.Retry must
// hydrate the job, reset its failed articles in memory and in SQLite, and
// re-offer them for download upon promotion.
func TestRetry_NonResidentJobResetsFailedArticles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	repo := history.NewRepository(db)
	q := New(WithStore(NewSQLiteStore(repo.DB(), dir, repo)))

	job := makeMultiFileJob(t, "retry-test", 1, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q.PromoteNext(context.Background())

	// Mark article 0 as failed.
	failedArtID := articleID(0, 0)
	if _, err := q.MarkArticlesFailed(job.ID, []string{failedArtID}); err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
	}
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Transition to StatusFailed and evict so it becomes de-hydrated.
	if err := q.SetStatus(job.ID, constants.StatusFailed); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	q.mu.Lock()
	q.evictJobLocked(job)
	q.mu.Unlock()

	// Verify precondition: job is de-hydrated.
	q.mu.RLock()
	resident := q.byID[job.ID].manifest != nil || q.byID[job.ID].progress != nil
	q.mu.RUnlock()
	if resident {
		t.Fatal("fixture error: job is still resident after eviction")
	}

	// Now call Retry. Before #260 fix, ResetForRetry no-opped on nil progress,
	// leaving the failed article un-reset and un-dispatchable.
	if err := q.Retry(job.ID); err != nil {
		t.Fatalf("Retry: %v", err)
	}

	offered := map[string]bool{}
	q.ForEachUnfinishedArticle(func(a UnfinishedArticle) bool {
		if a.JobID == job.ID {
			offered[a.MessageID] = true
		}
		return true
	})

	if !offered[failedArtID] {
		t.Errorf("failed article %s was not re-offered after Retry on non-resident job", failedArtID)
	}
}

// TestTotalRemainingBytes_IncludesNonResidentJobs pins the fix for issue #262:
// TotalRemainingBytes must include remaining bytes for de-hydrated (queued or
// paused) jobs, not skip them when job.progress == nil.
func TestTotalRemainingBytes_IncludesNonResidentJobs(t *testing.T) {
	t.Parallel()

	q := New(WithStateDir(t.TempDir()))

	var expectedTotal int64
	for i := range 4 {
		job := makeMultiFileJob(t, fmt.Sprintf("job-%d", i), 1, 2)
		expectedTotal += job.manifest.TotalBytes()
		if err := q.Add(job); err != nil {
			t.Fatalf("Add: %v", err)
		}
		_ = q.Pause(job.ID)
	}

	// Because q has a stateDir, Add releases manifest & progress for Queued jobs.
	// Verify that all jobs are de-hydrated.
	q.mu.RLock()
	for _, job := range q.byID {
		if job.progress != nil || job.manifest != nil {
			t.Fatalf("fixture error: job %s is resident", job.ID)
		}
	}
	q.mu.RUnlock()

	got := q.TotalRemainingBytes()
	if got != expectedTotal {
		t.Errorf("TotalRemainingBytes() = %d, want %d for 4 non-resident jobs", got, expectedTotal)
	}
}

// TestJobManifest_ConcurrentEvictionNoRace pins the fix for issue #263:
// calling Job.Manifest() concurrently with queue eviction/pause must not
// trigger a data race or panic, even on a freshly constructed job without
// prior serial initialization.
func TestJobManifest_ConcurrentEvictionNoRace(t *testing.T) {
	t.Parallel()

	// 1. Verify zero-value job concurrency without serial initialization.
	freshJob := &Job{ID: "fresh-race-test"}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			_ = freshJob.Manifest()
			_ = freshJob.Progress()
		}
	}()
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			freshJob.SetManifest(nil)
			freshJob.SetProgress(nil)
		}
	}()
	wg.Wait()

	// 2. Verify full queue eviction and promotion concurrency.
	q := New(WithStateDir(t.TempDir()))
	job := makeMultiFileJob(t, "race-test", 1, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx2, cancel2 := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel2()

	wg.Add(2)
	go func() {
		defer wg.Done()
		for ctx2.Err() == nil {
			_ = job.Manifest()
			_ = job.Progress()
		}
	}()
	go func() {
		defer wg.Done()
		for ctx2.Err() == nil {
			q.PromoteNext(context.Background())
			_ = q.Pause(job.ID)
			_ = q.Resume(job.ID)
		}
	}()

	wg.Wait()
}

// TestSetStatus_ResidentHydrationFailureReportsError pins the fix for issue #264:
// when SetStatus transitions a job to a resident status, if hydration fails,
// it must return an error and not leave the job resident with nil fields.
func TestSetStatus_ResidentHydrationFailureReportsError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	q := New(WithStateDir(dir))
	job := makeMultiFileJob(t, "status-test", 1, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_ = q.Pause(job.ID)

	// Remove manifest from state dir so hydration will fail.
	manifestPath := filepath.Join(dir, "manifests", job.ID+".json.gz")
	if err := os.Remove(manifestPath); err != nil {
		t.Fatalf("os.Remove: %v", err)
	}

	err := q.SetStatus(job.ID, constants.StatusDownloading)
	if err == nil {
		t.Error("SetStatus(Downloading) = nil, want error when manifest is unreadable")
	}
	if job.Status != constants.StatusPaused {
		t.Errorf("job.Status = %v after hydration failure, want StatusPaused (zero mutation)", job.Status)
	}
	if job.Manifest() != nil || job.Progress() != nil {
		t.Error("job resident fields mutated on hydration failure, want nil")
	}
}

// TestSetStatusIf_HydratesResidentJob pins the SetStatusIf fix for issue #264:
// SetStatusIf must hydrate a job when transitioning to a resident status.
func TestSetStatusIf_HydratesResidentJob(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	q := New(WithStateDir(dir))
	job := makeMultiFileJob(t, "statusif-test", 1, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_ = q.Pause(job.ID)

	// Verify job is de-hydrated after Pause.
	if job.Manifest() != nil || job.Progress() != nil {
		t.Fatal("fixture error: job should be non-resident after Pause")
	}

	if err := q.SetStatusIf(job.ID, constants.StatusDownloading, constants.StatusPaused); err != nil {
		t.Fatalf("SetStatusIf: %v", err)
	}
	if job.Manifest() == nil || job.Progress() == nil {
		t.Error("SetStatusIf left job de-hydrated on resident status Downloading")
	}
}

// TestEncodeArticlesFailed_NilAndBoundsGuards verifies that encodeArticlesFailed
// and encodeArticlesDone return empty string without panicking when called on
// nil receivers or out-of-bounds file indices.
func TestEncodeArticlesFailed_NilAndBoundsGuards(t *testing.T) {
	t.Parallel()

	if got := encodeArticlesFailed(nil, 0); got != "" {
		t.Errorf("encodeArticlesFailed(nil, 0) = %q, want empty", got)
	}
	if got := encodeArticlesDone(nil, 0); got != "" {
		t.Errorf("encodeArticlesDone(nil, 0) = %q, want empty", got)
	}

	job := &Job{ID: "bounds-test"}
	if got := encodeArticlesFailed(job, 0); got != "" {
		t.Errorf("encodeArticlesFailed(unhydrated, 0) = %q, want empty", got)
	}
}

// TestSetStatus_IllegalTransitionDoesNotHydrate verifies that requesting an
// illegal status transition on a de-hydrated job fails fast without hydrating
// the job in memory.
func TestSetStatus_IllegalTransitionDoesNotHydrate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	q := New(WithStateDir(dir))
	job := makeMultiFileJob(t, "illegal-trans-test", 1, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_ = q.Pause(job.ID)

	// Precondition: job is de-hydrated in StatusPaused.
	if job.Manifest() != nil || job.Progress() != nil {
		t.Fatal("fixture error: job should be non-resident after Pause")
	}

	// Request illegal transition: StatusPaused -> StatusCompleted.
	err := q.SetStatus(job.ID, constants.StatusCompleted)
	if !errors.Is(err, ErrIllegalStatusTransition) {
		t.Errorf("SetStatus(Paused -> Completed) err = %v, want ErrIllegalStatusTransition", err)
	}

	// Verify job remains de-hydrated with zero hydration I/O.
	if job.Manifest() != nil || job.Progress() != nil {
		t.Error("job was unnecessarily hydrated on illegal status transition")
	}
}
