package queue

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

// TestArtIdx_EdgeCases stress-tests MarkArticlesDoneByIdx and MarkArticlesFailedByIdx
// with negative, zero, out-of-bounds, and valid ArtIdx values.
func TestArtIdx_EdgeCases(t *testing.T) {
	q := New()
	q.PauseAll()

	job := makeJob(t, "artidx-edge", constants.NormalPriority)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add job: %v", err)
	}

	gotJob, err := q.Get(job.ID)
	if err != nil || gotJob == nil || gotJob.Manifest() == nil {
		t.Fatalf("Get job failed: %v", err)
	}
	nArt := gotJob.Manifest().NumArticles()
	if nArt < 2 {
		t.Fatalf("Expected at least 2 articles in test manifest, got %d", nArt)
	}

	t.Run("negative and out-of-bounds indices are safely ignored", func(t *testing.T) {
		invalidIdxs := []int32{-1, -100, int32(nArt), int32(nArt + 50), math.MaxInt32, math.MinInt32}
		err := q.MarkArticlesDoneByIdx(job.ID, invalidIdxs)
		if err != nil {
			t.Errorf("MarkArticlesDoneByIdx returned unexpected error for out-of-bounds: %v", err)
		}

		// Verify no article was marked done
		snap, err := q.Get(job.ID)
		if err != nil {
			t.Fatalf("Get job: %v", err)
		}
		if snap.Progress().ArticlesResolved() != 0 {
			t.Errorf("Expected 0 resolved articles, got %d", snap.Progress().ArticlesResolved())
		}
	})

	t.Run("MarkArticlesFailedByIdx out-of-bounds safe handling", func(t *testing.T) {
		invalidIdxs := []int32{-1, int32(nArt + 10)}
		firstTime, err := q.MarkArticlesFailedByIdx(job.ID, invalidIdxs)
		if err != nil {
			t.Errorf("MarkArticlesFailedByIdx returned unexpected error: %v", err)
		}
		if len(firstTime) != 0 {
			t.Errorf("Expected len(firstTime) == 0 for invalid indices, got %v", firstTime)
		}
	})

	t.Run("valid indices update progress correctly", func(t *testing.T) {
		validIdxs := []int32{0}
		err := q.MarkArticlesDoneByIdx(job.ID, validIdxs)
		if err != nil {
			t.Fatalf("MarkArticlesDoneByIdx: %v", err)
		}

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
	nArt := gotJob.Manifest().NumArticles()

	var wg sync.WaitGroup
	numWorkers := 20
	opsPerWorker := 200

	for w := range numWorkers {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := range opsPerWorker {
				idx := int32(rand.IntN(nArt+5) - 2) // includes -2, -1, 0..nArt-1, nArt, nArt+1, etc.
				switch i % 4 {
				case 0:
					_ = q.MarkArticlesDoneByIdx(job.ID, []int32{idx})
				case 1:
					_, _ = q.MarkArticlesFailedByIdx(job.ID, []int32{idx})
				case 2:
					_ = q.MarkArticleEmittedByIdx(job.ID, idx)
				case 3:
					_ = q.ClearArticleEmittedByIdx(job.ID, idx)
				}
			}
		}(w)
	}

	wg.Wait()
}

// BenchmarkMarkArticlesDone_Comparison benchmarks string-based vs O(1) index-based article completion.
func BenchmarkMarkArticlesDone_Comparison(b *testing.B) {
	const totalArticles = 10000

	files := []JobFile{
		{
			Subject: "test.file.rar",
			Bytes:   totalArticles * 100,
			Articles: func() []JobArticle {
				arts := make([]JobArticle, totalArticles)
				for i := range totalArticles {
					arts[i] = JobArticle{
						ID:     fmt.Sprintf("msg-%d@test", i),
						Bytes:  100,
						Number: i + 1,
					}
				}
				return arts
			}(),
		},
	}

	manifest := newManifest(files)

	b.Run("StringMap_MarkArticlesDone", func(b *testing.B) {
		for i := range b.N {
			b.StopTimer()
			q := New()
			q.PauseAll()
			job := &Job{
				ID:       fmt.Sprintf("job-str-%d", i),
				manifest: manifest,
				progress: newJobProgress(manifest),
				Status:   constants.StatusDownloading,
			}
			q.byID[job.ID] = job
			q.jobs = []*Job{job}
			msgIDs := []string{fmt.Sprintf("msg-%d@test", i%totalArticles)}
			b.StartTimer()

			_ = q.MarkArticlesDone(job.ID, msgIDs)
		}
	})

	b.Run("O1Index_MarkArticlesDoneByIdx", func(b *testing.B) {
		for i := range b.N {
			b.StopTimer()
			q := New()
			q.PauseAll()
			job := &Job{
				ID:       fmt.Sprintf("job-idx-%d", i),
				manifest: manifest,
				progress: newJobProgress(manifest),
				Status:   constants.StatusDownloading,
			}
			q.byID[job.ID] = job
			q.jobs = []*Job{job}
			artIdxs := []int32{int32(i % totalArticles)}
			b.StartTimer()

			_ = q.MarkArticlesDoneByIdx(job.ID, artIdxs)
		}
	})
}
