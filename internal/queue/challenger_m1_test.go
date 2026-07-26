package queue

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
)

func TestChallenger_ConcurrentPromotionAndPauseResumeLoops(t *testing.T) {
	tempDir := t.TempDir()
	q := New(WithStateDir(tempDir))
	maxActive := 5
	q.SetMaxActiveJobs(maxActive)

	// Add 100 queued jobs
	const totalJobs = 100
	for i := range totalJobs {
		job := makeJob(t, fmt.Sprintf("job-%03d", i), constants.NormalPriority)
		job.Status = constants.StatusQueued
		if err := q.Add(job); err != nil {
			t.Fatalf("Add job %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	numWorkers := 8
	for w := range numWorkers {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}

				switch rand.IntN(4) {
				case 0:
					q.PromoteNext(context.Background())
				case 1:
					q.PauseAll()
					q.ResumeAll(context.Background())
				case 2:
					q.PromoteNext(context.Background())
				case 3:
					q.SetMaxActiveJobs(maxActive)
				}
				time.Sleep(time.Duration(rand.IntN(3)) * time.Millisecond)
			}
		}(w)
	}

	wg.Wait()

	activeLen := q.activeSet.Len()
	t.Logf("End of stress test: ActiveSet.Len()=%d, MaxActive=%d, Queue.Len()=%d", activeLen, maxActive, q.Len())
	if activeLen > maxActive {
		t.Errorf("INVARIANT VIOLATION: ActiveSet.Len() (%d) > MaxActive (%d)", activeLen, maxActive)
	}
}

func TestJob_MarkDownloadFinished_NilProgressGuard(t *testing.T) {
	job := &Job{
		ID:       "non-resident-job",
		Status:   constants.StatusQueued,
		progress: nil,
	}

	// Should not panic on non-resident job with progress == nil
	job.MarkDownloadFinished(time.Now())
}
