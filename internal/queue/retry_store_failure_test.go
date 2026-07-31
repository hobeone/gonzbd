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
	failUpdate  bool
	failRestore bool
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
	if err := q.MarkArticlesDone(job.ID, []string{articleID(0, 0)}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}
	if _, err := q.MarkArticlesFailed(job.ID, []string{articleID(0, 1)}); err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
	}
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := q.SetStatus(job.ID, constants.StatusFailed); err != nil {
		t.Fatalf("SetStatus(Failed): %v", err)
	}
	job.Warning = "download failed"

	if job.Manifest() != nil || job.Progress() != nil {
		t.Fatal("fixture guard: job should be non-resident after StatusFailed")
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
	// Residency must be dropped: ResetForRetry already mutated this
	// JobProgress, and the un-written store row is the authority to
	// re-hydrate from.
	if job.Manifest() != nil || job.Progress() != nil {
		t.Error("job is still resident, so the mutated progress survived the rollback")
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
	if job.Manifest() != nil || job.Progress() != nil {
		t.Error("job is resident with progress that was never restored")
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
