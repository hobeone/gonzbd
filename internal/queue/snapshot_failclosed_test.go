package queue

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Snapshot hydration used to swallow its read error: a job whose manifest
// could not be loaded came back with nil manifest/progress, indistinguishable
// from one that loaded successfully, and every downstream reader saw empty
// state instead of a failure.
//
// That is only safe to change now that a job reachable in the queue always has
// its manifest on disk — artifact deletion moved after the job leaves byID, so
// a job completing concurrently can no longer be caught mid-unlink. A
// hydration failure therefore means genuine corruption, which is worth
// reporting rather than rendering as zeroes.

// evictedJobWithManifestRemoved returns a store-backed queue holding one
// non-resident job whose manifest file has been deleted out from under it,
// plus the job ID. Deleting the file directly is the point: no ordinary code
// path produces this state any more, and the tests below are about what
// happens when something outside the queue's control has damaged it.
func evictedJobWithManifestRemoved(t *testing.T) (*Queue, string) {
	t.Helper()

	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	job := makeMultiFileJob(t, "corrupt-manifest", 1, 2)
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
		t.Fatal("fixture guard: job must be non-resident, or hydration never runs")
	}

	if err := os.Remove(filepath.Join(dir, "manifests", job.ID+".json.gz")); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}
	return q, job.ID
}

// TestSnapshotJobUnhydratableReportsError pins that a job which cannot be
// hydrated is reported, not returned with silently empty state.
func TestSnapshotJobUnhydratableReportsError(t *testing.T) {
	q, jobID := evictedJobWithManifestRemoved(t)

	got, err := q.SnapshotJob(jobID)
	if err == nil {
		t.Fatal("SnapshotJob: want an error when the manifest cannot be read, got nil")
	}
	if got != nil {
		t.Errorf("SnapshotJob returned a job alongside the error: %+v", got.ID)
	}
	// A damaged job must stay distinguishable from an absent one. Callers key
	// their response off that difference — a missing job is a 404, a corrupt
	// one is not.
	if errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, must not be reported as ErrNotFound", err)
	}
}

// TestSnapshotJobMissingIsStillNotFound is the other side of that split.
func TestSnapshotJobMissingIsStillNotFound(t *testing.T) {
	q, _ := evictedJobWithManifestRemoved(t)

	got, err := q.SnapshotJob("no-such-job")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
	if got != nil {
		t.Errorf("SnapshotJob returned %+v for a missing job, want nil", got.ID)
	}
}

// TestSnapshotFailsClosedRatherThanDroppingJobs is the decision this change
// turns on. Excluding an unhydratable job from the listing and reporting the
// error alongside the survivors was the tempting alternative, and it is worse:
// the job vanishes from the queue view entirely, so the operator cannot see
// that it is broken or act on it, and a caller that ignores the error silently
// renders a short list as if it were complete. Fail closed instead — no
// partial list.
func TestSnapshotFailsClosedRatherThanDroppingJobs(t *testing.T) {
	q, jobID := evictedJobWithManifestRemoved(t)

	// A second, healthy job, so a passing test cannot be explained by the
	// queue simply being empty.
	healthy := makeMultiFileJob(t, "healthy", 1, 1)
	if err := q.Add(healthy); err != nil {
		t.Fatalf("Add healthy: %v", err)
	}

	jobs, err := q.Snapshot()
	if err == nil {
		t.Fatal("Snapshot: want an error when a job cannot be hydrated, got nil")
	}
	if jobs != nil {
		t.Errorf("Snapshot returned %d jobs alongside the error; a partial list "+
			"lets a caller that ignores the error treat it as complete", len(jobs))
	}
	// The error has to name the offending job, or an operator cannot act on it.
	if !strings.Contains(err.Error(), jobID) {
		t.Errorf("error %q does not identify the failing job %s", err, jobID)
	}
}

// TestSnapshotSucceedsWhenAllJobsHydrate guards against the fix degenerating
// into "Snapshot always errors".
func TestSnapshotSucceedsWhenAllJobsHydrate(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	for _, name := range []string{"first", "second", "third"} {
		if err := q.Add(makeMultiFileJob(t, name, 1, 1)); err != nil {
			t.Fatalf("Add %s: %v", name, err)
		}
	}

	jobs, err := q.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(jobs) != 3 {
		t.Errorf("Snapshot returned %d jobs, want 3", len(jobs))
	}
	for _, j := range jobs {
		if j.Manifest() == nil || j.Progress() == nil {
			t.Errorf("job %s came back non-resident from a successful Snapshot", j.ID)
		}
	}
}
