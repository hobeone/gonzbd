package queue

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

func TestQueue_Snapshot(t *testing.T) {
	q := New()
	job := &Job{
		ID:     "test-job",
		Name:   "Test Job",
		Status: constants.StatusQueued,
		ServerStats: map[string]int64{
			"server1": 100,
		},
		Meta: map[string][]string{
			"key1": {"val1"},
		},
		Groups: []string{"group1"},
		Files: []JobFile{
			{
				Subject: "file1",
				Articles: []JobArticle{
					{ID: "art1", Done: false},
				},
			},
		},
	}
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

	sJob.ServerStats["server1"] = 200
	if job.ServerStats["server1"] != 100 {
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

	sJob.Files[0].Articles[0].Done = true
	if job.Files[0].Articles[0].Done != false {
		t.Error("mutation to snapshot nested structure affected original article state")
	}
}

func TestQueue_SnapshotJob(t *testing.T) {
	q := New()
	jobID := "test-id"
	_ = q.Add(&Job{ID: jobID, Name: "Test"})

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

// TestSnapshotJob_ArtIdxIsolation verifies that the lazy article-by-ID
// index (artIdx) on a cloned job points into the clone's own Files slice,
// not the original job's. Before the fix, cloneJob shallow-copied artIdx,
// which held *JobArticle pointers into the original's slice.
func TestSnapshotJob_ArtIdxIsolation(t *testing.T) {
	q := New()
	job := &Job{
		ID:   "idx-test",
		Name: "ArtIdx Isolation",
		Files: []JobFile{
			{
				Subject: "file1",
				Articles: []JobArticle{
					{ID: "art-001", Done: false},
					{ID: "art-002", Done: false},
				},
			},
		},
	}
	_ = q.Add(job)

	// Force the original to build its artIdx.
	origArt := job.articleByID("art-001")
	if origArt == nil {
		t.Fatal("articleByID returned nil on original job")
	}

	// Take a snapshot — artIdx must be rebuilt independently.
	snap := q.SnapshotJob("idx-test")
	if snap == nil {
		t.Fatal("SnapshotJob returned nil")
	}

	// Access an article on the clone via articleByID.
	cloneArt := snap.articleByID("art-001")
	if cloneArt == nil {
		t.Fatal("articleByID returned nil on cloned job")
	}

	// Mutate the clone's article.
	cloneArt.Done = true

	// Verify the original is unchanged.
	if origArt.Done {
		t.Error("mutation via clone's articleByID affected original job — artIdx was not isolated")
	}
	if job.Files[0].Articles[0].Done {
		t.Error("mutation via clone's articleByID affected original job's Files slice")
	}
}

func TestCloneJobDirect(t *testing.T) {
	t.Parallel()
	job := &Job{
		ID:   "test-id",
		Name: "Test",
	}
	cloned := cloneJob(job)
	if cloned.ID != job.ID {
		t.Errorf("cloneJob ID mismatch: got %q, want %q", cloned.ID, job.ID)
	}
}
