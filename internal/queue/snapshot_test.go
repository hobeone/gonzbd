package queue

import (
	"errors"
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

	snap, err := q.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
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

	sJob.progress.done[0] = true
	if job.progress.done[0] {
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

	snap, err := q.SnapshotJob(jobID)
	if err != nil {
		t.Fatalf("SnapshotJob: %v", err)
	}
	if snap.ID != jobID {
		t.Errorf("snapshot ID = %q, want %q", snap.ID, jobID)
	}

	if _, err := q.SnapshotJob("non-existent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SnapshotJob for non-existent ID: got err=%v, want ErrNotFound", err)
	}
}

// TestSnapshotJob_ArtIdxIsolation verifies that a cloned job's Progress is an
// independent deep copy, isolated from the original — Manifest, by contrast,
// is now legitimately shared by reference (it's immutable after
// construction), so this test mutates via Progress's own state rather than
// through a pointer returned by the messageID index, proving isolation on
// the half of the split that's actually meant to be isolated.
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

	// Force the original's manifest to build its messageIDIndex.
	origIdx, ok := job.manifest.articleIndexByID("art-001")
	if !ok {
		t.Fatal("articleIndexByID returned false on original job")
	}

	// Take a snapshot — Manifest is shared (immutable, safe to alias), but
	// Progress must be an independent deep copy.
	snap, err := q.SnapshotJob("idx-test")
	if err != nil {
		t.Fatalf("SnapshotJob: %v", err)
	}
	if snap.Manifest() != job.manifest {
		t.Error("clone's Manifest should be the same shared pointer as the original's")
	}

	cloneIdx, ok := snap.Manifest().articleIndexByID("art-001")
	if !ok || cloneIdx != origIdx {
		t.Fatal("articleIndexByID returned inconsistent index on cloned job's shared manifest")
	}

	// Mutate the clone's Progress directly.
	snap.progress.done[cloneIdx] = true

	// Verify the original's Progress is unaffected.
	if job.progress.done[origIdx] {
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
