package queue

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

// The five manifest scalars must be readable from a job whose manifest has
// been evicted. That is the entire point of promoting them: a reporting
// path must never need a manifest.
func TestJobScalarsSurviveEviction(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	job := makeMultiFileJob(t, "scalars", 2, 3) // 2 files, 3 articles each
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	wantBytes := job.TotalBytes()
	if wantBytes == 0 {
		t.Fatal("fixture is useless: TotalBytes is zero while resident")
	}

	if err := q.Pause(job.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	q.mu.RLock()
	resident := q.byID[job.ID].manifest != nil
	q.mu.RUnlock()
	if resident {
		t.Fatal("fixture guard: job still resident after Pause, nothing is being tested")
	}

	if got := job.TotalBytes(); got != wantBytes {
		t.Errorf("TotalBytes after eviction = %d, want %d", got, wantBytes)
	}
	if got := job.NumFiles(); got != 2 {
		t.Errorf("NumFiles after eviction = %d, want 2", got)
	}
	if got := job.NumArticles(); got != 6 {
		t.Errorf("NumArticles after eviction = %d, want 6", got)
	}
}

// TestSnapshotJob_QueuedAfterRestart_ScalarsNotZero reproduces the gap named
// in the Task 3 review: a StatusQueued job restored via SQLiteStore.Get
// (through Loader.Load's store-backed branch) never has its manifest loaded
// by Get — that only happens for resident-status jobs — so the in-memory Job
// that comes back from a restart has zero scalars. Queue.SnapshotJob then
// clones that job and calls hydrateSnapshot, which reads the manifest from
// disk for other reasons (building progress) but, before this fix, never
// used it to backfill the five scalars. The result: an API response built
// from SnapshotJob reports zero size for a queued job that survived a
// restart, even though the manifest was in hand the whole time.
func TestSnapshotJob_QueuedAfterRestart_ScalarsNotZero(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	// Occupy the single active slot so the job under test stays queued and
	// non-resident even before the restart.
	filler := makeMultiFileJob(t, "filler-active", 1, 1)
	if err := q.Add(filler); err != nil {
		t.Fatalf("Add filler: %v", err)
	}

	job := makeMultiFileJob(t, "queued-scalars", 2, 3) // 2 files, 3 articles each
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	wantBytes := job.TotalBytes()
	if wantBytes == 0 {
		t.Fatal("fixture is useless: TotalBytes is zero before restart")
	}

	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload a fresh Queue from the same store/dir — the real restart path.
	// q2 is a brand-new Queue with an empty byID; every job comes back via
	// store.List()/SQLiteStore.Get rather than any in-memory carry-over.
	q2, err := Load(dir, WithStore(store), WithMaxActiveJobs(1))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	q2.mu.RLock()
	restored, ok := q2.byID[job.ID]
	q2.mu.RUnlock()
	if !ok {
		t.Fatalf("job %s missing after reload", job.ID)
	}
	if restored.Status != constants.StatusQueued {
		t.Fatalf("fixture guard: job status after reload = %v, want StatusQueued", restored.Status)
	}
	if restored.manifest != nil {
		t.Fatal("fixture guard: job resident after reload, nothing is being tested")
	}

	snap := q2.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("SnapshotJob returned nil")
	}
	if got := snap.TotalBytes(); got != wantBytes {
		t.Errorf("SnapshotJob(...).TotalBytes() = %d, want %d", got, wantBytes)
	}
	if got := snap.NumFiles(); got != 2 {
		t.Errorf("SnapshotJob(...).NumFiles() = %d, want 2", got)
	}
	if got := snap.NumArticles(); got != 6 {
		t.Errorf("SnapshotJob(...).NumArticles() = %d, want 6", got)
	}
}

// TestPromoteNext_RestoredJobScalarsSynced covers the second zero-scalar
// site the Task 3 review flagged: PromoteNext attaches a manifest to a
// restored, non-resident StatusQueued job (job.manifest == nil) when
// promoting it to StatusDownloading, using its own manifest read rather than
// hydrateJobLocked's. Before the fix, that attach path never backfilled the
// five scalars, so a job promoted after a restart reported zero size for the
// rest of its in-memory life — including while actively downloading.
func TestPromoteNext_RestoredJobScalarsSynced(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	// Occupy the single active slot so jobB stays queued and non-resident.
	filler := makeMultiFileJob(t, "promote-filler", 1, 1)
	if err := q.Add(filler); err != nil {
		t.Fatalf("Add filler: %v", err)
	}
	jobB := makeMultiFileJob(t, "promote-scalars", 2, 3) // 2 files, 3 articles each
	if err := q.Add(jobB); err != nil {
		t.Fatalf("Add jobB: %v", err)
	}
	wantBytes := jobB.TotalBytes()
	if wantBytes == 0 {
		t.Fatal("fixture is useless: TotalBytes is zero before restart")
	}

	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload — jobB comes back via SQLiteStore.Get with zero scalars, since
	// Get only loads a manifest for resident statuses and jobB is
	// StatusQueued.
	q2, err := Load(dir, WithStore(store), WithMaxActiveJobs(1))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	q2.mu.RLock()
	restored, ok := q2.byID[jobB.ID]
	q2.mu.RUnlock()
	if !ok {
		t.Fatalf("jobB missing after reload")
	}
	if restored.Status != constants.StatusQueued || restored.manifest != nil {
		t.Fatalf("fixture guard: jobB status=%v manifest!=nil=%v after reload, want StatusQueued/nil manifest",
			restored.Status, restored.manifest != nil)
	}
	if got := restored.TotalBytes(); got != 0 {
		t.Fatalf("fixture guard: jobB.TotalBytes() = %d before promotion, want 0 (nothing to test)", got)
	}

	// Free up the active slot and promote: this drives PromoteNext, the
	// exact path under test.
	q2.SetMaxActiveJobs(2)

	q2.mu.RLock()
	promoted := q2.byID[jobB.ID]
	q2.mu.RUnlock()
	if promoted.Status != constants.StatusDownloading {
		t.Fatalf("fixture guard: jobB.Status = %v after SetMaxActiveJobs, want StatusDownloading (not promoted)", promoted.Status)
	}

	if got := promoted.TotalBytes(); got != wantBytes {
		t.Errorf("TotalBytes after promotion = %d, want %d", got, wantBytes)
	}
	if got := promoted.NumFiles(); got != 2 {
		t.Errorf("NumFiles after promotion = %d, want 2", got)
	}
	if got := promoted.NumArticles(); got != 6 {
		t.Errorf("NumArticles after promotion = %d, want 6", got)
	}
}
