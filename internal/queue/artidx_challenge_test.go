package queue

import (
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

// TestArtIdx_EdgeCases stress-tests AckPermanentFailure and the ByIdx emitted
// markers with negative, zero, out-of-bounds, and valid ArtIdx values.
//
// The done-path equivalent (formerly MarkArticlesDoneByIdx) is not exercised
// here: its replacement, AckDurable, only accepts a durability.DurableProof,
// which has no exported constructor outside internal/durability (see
// ackhelpers_test.go). That is deliberate compiler-enforced scoping, not an
// oversight, so the "out-of-bounds indices are silently ignored" coverage for
// the done path no longer exists in this package.
func TestArtIdx_EdgeCases(t *testing.T) {
	q := New()
	q.PauseAll()

	job := makeJob(t, "artidx-edge", constants.NormalPriority)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add job: %v", err)
	}

	gotJob, err := q.Get(job.ID)
	if err != nil || gotJob == nil || !manifestResident(gotJob) {
		t.Fatalf("Get job failed: %v", err)
	}
	nArt := mustManifest(t, gotJob).NumArticles()
	if nArt < 2 {
		t.Fatalf("Expected at least 2 articles in test manifest, got %d", nArt)
	}

	t.Run("AckPermanentFailure out-of-bounds safe handling", func(t *testing.T) {
		invalidIdxs := []int32{-1, int32(nArt + 10)}
		err := q.AckPermanentFailure(job.ID, invalidIdxs)
		if err != nil {
			t.Errorf("AckPermanentFailure returned unexpected error: %v", err)
		}
	})

	t.Run("valid indices update progress correctly", func(t *testing.T) {
		ackDoneIdx(t, q, job.ID, 0)

		snap, err := q.Get(job.ID)
		if err != nil {
			t.Fatalf("Get job: %v", err)
		}
		if !snap.Progress().ArticleDone(0) {
			t.Errorf("Expected article 0 to be Done")
		}
		if snap.Progress().ArticlesResolved() != 1 {
			t.Errorf("Expected 1 resolved article, got %d", snap.Progress().ArticlesResolved())
		}
	})

	t.Run("MarkArticleEmittedByIdx and ClearArticleEmittedByIdx bounds checks", func(t *testing.T) {
		if err := q.MarkArticleEmittedByIdx(job.ID, -1); err == nil {
			t.Errorf("MarkArticleEmittedByIdx(-1) expected error, got nil")
		}
		if err := q.MarkArticleEmittedByIdx(job.ID, int32(nArt)); err == nil {
			t.Errorf("MarkArticleEmittedByIdx(nArt) expected error, got nil")
		}
		if err := q.ClearArticleEmittedByIdx(job.ID, -1); err == nil {
			t.Errorf("ClearArticleEmittedByIdx(-1) expected error, got nil")
		}
		if err := q.ClearArticleEmittedByIdx(job.ID, int32(nArt)); err == nil {
			t.Errorf("ClearArticleEmittedByIdx(nArt) expected error, got nil")
		}

		// Valid emitted and clear
		if err := q.MarkArticleEmittedByIdx(job.ID, 1); err != nil {
			t.Errorf("MarkArticleEmittedByIdx(1) failed: %v", err)
		}
		snap, _ := q.Get(job.ID)
		if !snap.Progress().ArticleEmitted(1) {
			t.Errorf("Expected article 1 to be Emitted")
		}

		if err := q.ClearArticleEmittedByIdx(job.ID, 1); err != nil {
			t.Errorf("ClearArticleEmittedByIdx(1) failed: %v", err)
		}
		snap, _ = q.Get(job.ID)
		if snap.Progress().ArticleEmitted(1) {
			t.Errorf("Expected article 1 to no longer be Emitted")
		}
	})
}

// TestArtIdx_ConcurrentStress performs high-concurrency operations on ArtIdx methods.
func TestArtIdx_ConcurrentStress(t *testing.T) {
	q := New()
	q.PauseAll()

	job := makeJob(t, "artidx-stress", constants.NormalPriority)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add job: %v", err)
	}

	gotJob, _ := q.Get(job.ID)
	nArt := mustManifest(t, gotJob).NumArticles()

	var wg sync.WaitGroup
	numWorkers := 20
	opsPerWorker := 200

	for w := range numWorkers {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := range opsPerWorker {
				idx := int32(rand.IntN(nArt+5) - 2) // includes -2, -1, 0..nArt-1, nArt, nArt+1, etc.
				switch i % 3 {
				case 0:
					_ = q.AckPermanentFailure(job.ID, []int32{idx})
				case 1:
					_ = q.MarkArticleEmittedByIdx(job.ID, idx)
				case 2:
					_ = q.ClearArticleEmittedByIdx(job.ID, idx)
				}
			}
		}(w)
	}

	wg.Wait()
}
