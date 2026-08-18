package queue

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

func TestQueue_Snapshot(t *testing.T) {
	q := New()
	q.PauseAll()
	job := &Job{
		ID:     "test-job",
		Name:   "Test Job",
		Status: constants.StatusQueued,
		Meta: map[string][]string{
			"key1": {"val1"},
		},
		Groups: []string{"group1"},
	}
	job.manifest = newManifest([]JobFile{
		{
			Subject:  "file1",
			Articles: []JobArticle{{ID: "art1"}},
		},
	})
	job.progress = newJobProgress(job.manifest)
	job.progress.serverStats = map[string]int64{"server1": 100}
	_ = q.Add(job)

	snap := q.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot length = %d, want 1", len(snap))
	}

	// Verify deep copy of nested structures
	sJob := snap[0]
	if sJob.ID != job.ID {
		t.Errorf("snapshot job ID = %q, want %q", sJob.ID, job.ID)
	}

	// Mutate snapshot and verify original is unchanged
	sJob.Status = constants.StatusPaused
	if job.Status != constants.StatusQueued {
		t.Error("mutation to snapshot affected original job status")
	}

	sJob.progress.serverStats["server1"] = 200
	if job.progress.serverStats["server1"] != 100 {
		t.Error("mutation to snapshot map affected original server stats")
	}

	sJob.Meta["key1"][0] = "mutated"
	if job.Meta["key1"][0] != "val1" {
		t.Error("mutation to snapshot nested slice affected original meta")
	}

	sJob.Groups[0] = "mutated"
	if job.Groups[0] != "group1" {
		t.Error("mutation to snapshot slice affected original groups")
	}

	sJob.progress.done.Set(0)
	if job.progress.done.Get(0) {
		t.Error("mutation to snapshot nested structure affected original article state")
	}
}

func TestQueue_SnapshotJob(t *testing.T) {
	q := New()
	jobID := "test-id"
	job := &Job{ID: jobID, Name: "Test"}
	job.manifest = newManifest(nil)
	job.progress = newJobProgress(job.manifest)
	_ = q.Add(job)

	snap := q.SnapshotJob(jobID)
	if snap == nil {
		t.Fatal("SnapshotJob returned nil")
	}
	if snap.ID != jobID {
		t.Errorf("snapshot ID = %q, want %q", snap.ID, jobID)
	}

	if q.SnapshotJob("non-existent") != nil {
		t.Error("SnapshotJob returned non-nil for non-existent ID")
	}
}

// TestSnapshotJob_ArtIdxIsolation verifies that a cloned job's Progress is an
// independent deep copy, isolated from the original — Manifest, by contrast,
// is legitimately shared by reference, being immutable after construction.
// So this test mutates via Progress's own state, proving isolation on the
// half of the split that is actually meant to be isolated, and asserts
// pointer identity on the half that is meant to be shared.
func TestSnapshotJob_ArtIdxIsolation(t *testing.T) {
	q := New()
	job := &Job{ID: "idx-test", Name: "ArtIdx Isolation"}
	job.manifest = newManifest([]JobFile{
		{
			Subject: "file1",
			Articles: []JobArticle{
				{ID: "art-001"},
				{ID: "art-002"},
			},
		},
	})
	job.progress = newJobProgress(job.manifest)
	_ = q.Add(job)

	// Take a snapshot — Manifest is shared (immutable, safe to alias), but
	// Progress must be an independent deep copy.
	snap := q.SnapshotJob("idx-test")
	if snap == nil {
		t.Fatal("SnapshotJob returned nil")
	}
	if mustManifest(t, snap) != job.manifest {
		t.Error("clone's Manifest should be the same shared pointer as the original's")
	}

	// article 0 is "art-001", the fixture's first article.
	const idx = 0

	// Mutate the clone's Progress directly.
	snap.progress.done.Set(idx)

	// Verify the original's Progress is unaffected.
	if job.progress.done.Get(idx) {
		t.Error("mutation via clone's Progress affected original job's Progress — clone was not isolated")
	}
}

func TestCloneJobDirect(t *testing.T) {
	t.Parallel()
	job := &Job{
		ID:   "test-id",
		Name: "Test",
	}
	job.manifest = newManifest(nil)
	job.progress = newJobProgress(job.manifest)
	cloned := cloneJob(job)
	if cloned.ID != job.ID {
		t.Errorf("cloneJob ID mismatch: got %q, want %q", cloned.ID, job.ID)
	}
}

// Snapshot must not hydrate. It backs the queue listing, which is polled
// continuously and contains every queued and paused job — all non-resident
// once the active set is full — so a manifest read per job here is a disk
// read per job per poll. It also opened a window where a job removed
// between the clone and the read produced an error for an operation that
// had already succeeded.
//
// The reporting values a listing needs survive without it: the promoted
// scalars and JobProgress are resident for the life of the job. Callers
// that genuinely need file-level detail use SnapshotJob, which still
// hydrates for that one job.
func TestSnapshot_DoesNotHydrateNonResidentJobs(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	// Fill the single active slot so the job under test stays non-resident.
	filler := makeMultiFileJob(t, "snap-filler", 1, 1)
	if err := q.Add(filler); err != nil {
		t.Fatalf("Add filler: %v", err)
	}
	job := makeMultiFileJobWithPar2(t, "snap-nonresident", 2, 3)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	wantBytes, wantPar2 := job.TotalBytes(), job.RecoveryBytes()

	q.mu.RLock()
	live := q.byID[job.ID]
	nonResident := live.manifest == nil
	q.mu.RUnlock()
	if !nonResident {
		t.Fatal("fixture guard: job is resident, nothing is being tested")
	}

	var got *Job
	for _, cp := range q.Snapshot() {
		if cp.ID == job.ID {
			got = cp
		}
	}
	if got == nil {
		t.Fatal("job missing from Snapshot")
	}

	if manifestResident(got) {
		t.Error("Snapshot hydrated a non-resident job; that is a disk read per job on every queue poll")
	}
	// ...and the values a listing actually renders are still right.
	if got.TotalBytes() != wantBytes {
		t.Errorf("TotalBytes = %d, want %d", got.TotalBytes(), wantBytes)
	}
	if got.RecoveryBytes() != wantPar2 {
		t.Errorf("Par2Bytes = %d, want %d", got.RecoveryBytes(), wantPar2)
	}
	if got.Progress() == nil {
		t.Error("Progress is nil in a snapshot copy; reporting needs it and it is meant to be always resident")
	}
}

// TestHydrateSnapshot_AttachesManifestAndProgress calls hydrateSnapshot
// directly rather than through SnapshotJob, pinning the function's own
// contract: given a freshly cloned, non-resident Job, it attaches the
// on-disk manifest and leaves the clone's already-accurate JobProgress in
// place rather than rebuilding it (see the function's doc comment on why
// replacing it with a fresh newJobProgress was wrong — #287).
func TestHydrateSnapshot_AttachesManifestAndProgress(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	// Fill the single active slot first so job below starts non-resident.
	filler := makeMultiFileJob(t, "hydrate-direct-filler", 1, 1)
	if err := q.Add(filler); err != nil {
		t.Fatalf("Add filler: %v", err)
	}
	job := makeMultiFileJob(t, "hydrate-direct", 2, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// A sentinel value written through the progress tier, which — unlike
	// MarkArticlesDone — neither needs the manifest nor can fail on
	// residency (see SetPar2ReleaseReason's doc comment). It marks the
	// exact JobProgress object that must survive hydration unrebuilt.
	if err := q.SetPar2ReleaseReason(job.ID, "sentinel"); err != nil {
		t.Fatalf("SetPar2ReleaseReason: %v", err)
	}
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	q.mu.RLock()
	live := q.byID[job.ID]
	if live.manifest != nil {
		q.mu.RUnlock()
		t.Fatal("fixture guard: job is resident, hydrateSnapshot has nothing to load")
	}
	cp := cloneJob(live)
	q.mu.RUnlock()
	priorProgress := cp.progress
	if priorProgress == nil {
		t.Fatal("fixture guard: clone must carry the job's real progress, not nil")
	}

	var logBuf bytes.Buffer
	hydrateSnapshot(slog.New(slog.NewTextHandler(&logBuf, nil)), dir, store, cp)

	if !manifestResident(cp) {
		t.Fatalf("hydrateSnapshot did not attach a manifest; log: %q", logBuf.String())
	}
	if cp.hydrateErr != nil {
		t.Errorf("hydrateErr = %v, want nil on a clean hydration", cp.hydrateErr)
	}
	// The clone's own accurate progress must survive, not a freshly built
	// one: rebuilding it would discard the sentinel set above (#287).
	if cp.progress != priorProgress {
		t.Error("hydrateSnapshot replaced the clone's progress instead of attaching the manifest to the one it already had")
	}
	if got := cp.progress.Par2ReleaseReason(); got != "sentinel" {
		t.Errorf("Par2ReleaseReason = %q after hydration, want the sentinel to survive", got)
	}
}

// SnapshotJob is the counterpart: one job, on demand, where file-level
// detail is the point and a single read is affordable.
func TestSnapshotJob_StillHydrates(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	filler := makeMultiFileJob(t, "snapjob-filler", 1, 1)
	if err := q.Add(filler); err != nil {
		t.Fatalf("Add filler: %v", err)
	}
	job := makeMultiFileJob(t, "snapjob-nonresident", 2, 3)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	q.mu.RLock()
	nonResident := q.byID[job.ID].manifest == nil
	q.mu.RUnlock()
	if !nonResident {
		t.Fatal("fixture guard: job is resident, nothing is being tested")
	}

	snap := q.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("SnapshotJob returned nil")
	}
	if !manifestResident(snap) {
		t.Error("SnapshotJob did not hydrate; per-file detail (queueJobDetail, the postproc paths) depends on it")
	}
}
