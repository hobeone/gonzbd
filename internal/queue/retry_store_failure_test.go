package queue

import (
	"context"
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

var errInjected = errors.New("injected store failure")

// failingStore delegates every Store call to the real SQLiteStore, failing
// only the one method under test. Injecting the failure at the interface
// rather than by breaking the database matters here: Retry calls
// RestoreJobProgress and Update against the same job_files table, so dropping
// the table would fail both at once and neither test could attribute the
// resulting behavior to the method it is about.
type failingStore struct {
	Store
	failUpdate        bool
	failRestore       bool
	failUpdateBatch   bool
	failList          bool
	failArticleCounts bool
	failIsPaused      bool
	failSetPaused     bool
	failPrune         bool
	pruneCalls        int
}

func (f *failingStore) SetPaused(ctx context.Context, paused bool) error {
	if f.failSetPaused {
		return errInjected
	}
	return f.Store.SetPaused(ctx, paused)
}

func (f *failingStore) Prune(ctx context.Context) error {
	f.pruneCalls++
	if f.failPrune {
		return errInjected
	}
	return f.Store.Prune(ctx)
}

func (f *failingStore) IsPaused(ctx context.Context) (bool, error) {
	if f.failIsPaused {
		return false, errInjected
	}
	return f.Store.IsPaused(ctx)
}

func (f *failingStore) List(ctx context.Context) ([]*Job, error) {
	if f.failList {
		return nil, errInjected
	}
	return f.Store.List(ctx)
}

func (f *failingStore) ArticleCountsByJob(ctx context.Context) (map[string][]FileMeta, error) {
	if f.failArticleCounts {
		return nil, errInjected
	}
	return f.Store.ArticleCountsByJob(ctx)
}

func (f *failingStore) UpdateBatch(ctx context.Context, jobs []*Job) error {
	if f.failUpdateBatch {
		return errInjected
	}
	return f.Store.UpdateBatch(ctx, jobs)
}

func (f *failingStore) Update(ctx context.Context, job *Job) error {
	if f.failUpdate {
		return errInjected
	}
	return f.Store.Update(ctx, job)
}

func (f *failingStore) RestoreJobProgress(ctx context.Context, job *Job) error {
	if f.failRestore {
		return errInjected
	}
	return f.Store.RestoreJobProgress(ctx, job)
}

// newFailedNonResidentJob builds a store-backed queue holding one job that has
// downloaded one article, failed another, been flushed to SQLite, and then
// transitioned to StatusFailed — which evicts it. This is the state a real
// job is in when a user clicks Retry.
func newFailedNonResidentJob(t *testing.T) (*Queue, *failingStore, *Job) {
	t.Helper()

	real, dir := setupResidencyTestStore(t)
	fs := &failingStore{Store: real}
	q := New(WithStore(fs), WithStateDir(dir), WithMaxActiveJobs(1))

	job := makeMultiFileJob(t, "retry-store-fail", 1, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	ackDone(t, q, job.ID, articleID(0, 0))
	ackFailed(t, q, job.ID, articleID(0, 1))
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := q.SetStatus(job.ID, constants.StatusFailed); err != nil {
		t.Fatalf("SetStatus(Failed): %v", err)
	}
	job.Warning = "download failed"

	if manifestResident(job) {
		t.Fatal("fixture guard: job should be non-resident after StatusFailed")
	}
	if job.Progress() == nil {
		t.Fatal("fixture guard: progress must never be nil")
	}
	return q, fs, job
}

// TestRetryPersistFailureRollsBackJob pins the consequence of discarding the
// store write Retry depends on. That write exists because PromoteNext calls
// RestoreJobProgress for every job it promotes, including one already
// resident: if the reset is not persisted first, promotion re-reads the stale
// row and undoes it. Reporting success while that happens reproduces the very
// defect #260 is about, so a failed write must roll the job back and say so.
func TestRetryPersistFailureRollsBackJob(t *testing.T) {
	q, fs, job := newFailedNonResidentJob(t)

	// Capture the accurate, persisted progress before Retry's ResetForRetry
	// mutates it in place, so the rollback assertions below can confirm it
	// was restored rather than left at the optimistic reset.
	before := job.Progress()
	wantResolved := before.ArticlesResolved()
	wantFailed := before.ArticlesFailed()
	wantFailedBytes := before.FailedBytes()
	wantRemaining := before.RemainingBytes()

	fs.failUpdate = true
	err := q.Retry(job.ID)

	if err == nil {
		t.Fatal("Retry: want error when persisting the reset fails, got nil")
	}
	if !errors.Is(err, errInjected) {
		t.Errorf("error = %v, want it to wrap the store failure", err)
	}
	if job.Status != constants.StatusFailed {
		t.Errorf("Status = %s, want %s — a failed retry must not leave the job moved",
			job.Status, constants.StatusFailed)
	}
	if job.Warning != "download failed" {
		t.Errorf("Warning = %q, want it restored", job.Warning)
	}
	// The manifest must still be evicted (rollback does not promote the
	// job). Progress is never nil (docs/queue-lifecycle.md); what matters
	// is that ResetForRetry's in-place mutation did not survive the
	// rollback — the un-written store row still reflects the pre-mutation
	// values, so a resident-but-mutated JobProgress would let
	// TotalRemainingBytes and friends report numbers nothing on disk backs
	// up.
	if manifestResident(job) {
		t.Error("job is resident after a rolled-back Retry, want the manifest still evicted")
	}
	p := job.Progress()
	if p == nil {
		t.Fatal("progress must never be nil")
	}
	if got := p.ArticlesResolved(); got != wantResolved {
		t.Errorf("ArticlesResolved = %d, want %d (pre-mutation value; ResetForRetry's reset must not survive the rollback)", got, wantResolved)
	}
	if got := p.ArticlesFailed(); got != wantFailed {
		t.Errorf("ArticlesFailed = %d, want %d (pre-mutation value)", got, wantFailed)
	}
	if got := p.FailedBytes(); got != wantFailedBytes {
		t.Errorf("FailedBytes = %d, want %d (pre-mutation value)", got, wantFailedBytes)
	}
	if got := p.RemainingBytes(); got != wantRemaining {
		t.Errorf("RemainingBytes = %d, want %d (pre-mutation value)", got, wantRemaining)
	}

	// The rollback must leave a retry still possible once the store recovers.
	fs.failUpdate = false
	if err := q.Retry(job.ID); err != nil {
		t.Fatalf("Retry after the store recovered: %v", err)
	}
	var offered []string
	q.ForEachUnfinishedArticle(func(a UnfinishedArticle) bool {
		if a.JobID == job.ID {
			offered = append(offered, a.MessageID)
		}
		return true
	})
	if len(offered) != 1 || offered[0] != articleID(0, 1) {
		t.Errorf("after recovery, offered %v, want exactly [%s]", offered, articleID(0, 1))
	}
}

// TestHydrateRestoreFailureLeavesJobNonResident pins the other half. A job
// hydrates in two steps: newJobProgress builds an all-zero JobProgress, then
// RestoreJobProgress fills it from the stored counters. Keeping the zero
// progress when the second step fails would present a part-downloaded job as
// having downloaded nothing — and because Retry persists what it hydrates,
// that empty progress would be written back over the real record.
func TestHydrateRestoreFailureLeavesJobNonResident(t *testing.T) {
	q, fs, job := newFailedNonResidentJob(t)
	// ResetForRetry never runs in this scenario: hydrateJobLocked's
	// RestoreJobProgress failure makes Retry return before reaching it, so
	// this is the exact progress that must survive the failed attempt.
	before := job.Progress()

	fs.failRestore = true
	err := q.Retry(job.ID)

	if err == nil {
		t.Fatal("Retry: want error when restoring progress fails, got nil")
	}
	if !errors.Is(err, errInjected) {
		t.Errorf("error = %v, want it to wrap the store failure", err)
	}
	if job.Status != constants.StatusFailed {
		t.Errorf("Status = %s, want %s", job.Status, constants.StatusFailed)
	}
	if manifestResident(job) {
		t.Error("job is resident (manifest not evicted) after a failed hydration attempt")
	}
	// Progress must never be nil (docs/queue-lifecycle.md). What actually
	// needs preventing is the fabricated all-zero JobProgress that
	// hydrateJobLocked builds before RestoreJobProgress gets a chance to
	// fill it in — that one must not survive in progress's place; the
	// pre-existing, accurate one must.
	if got := job.Progress(); got != before {
		t.Error("job progress was replaced with the fresh, never-restored JobProgress instead of the prior one")
	}

	// The real record must be intact: once the store recovers, the completed
	// article is still completed and only the failed one is re-offered.
	fs.failRestore = false
	if err := q.Retry(job.ID); err != nil {
		t.Fatalf("Retry after the store recovered: %v", err)
	}
	var offered []string
	q.ForEachUnfinishedArticle(func(a UnfinishedArticle) bool {
		if a.JobID == job.ID {
			offered = append(offered, a.MessageID)
		}
		return true
	})
	if len(offered) != 1 || offered[0] != articleID(0, 1) {
		t.Errorf("after recovery, offered %v, want exactly [%s] — the completed article was lost",
			offered, articleID(0, 1))
	}
}
