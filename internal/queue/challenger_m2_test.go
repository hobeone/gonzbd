package queue_test

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

func TestQueue_WithMaxActiveJobs(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		input       int
		expectedMax int
	}{
		{input: 1, expectedMax: 1},
		{input: 4, expectedMax: 4},
		{input: 10, expectedMax: 10},
		{input: 1000, expectedMax: 1000},
		{input: 0, expectedMax: 4},  // Default 4 fallback
		{input: -1, expectedMax: 4}, // Default 4 fallback
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("WithMaxActiveJobs_%d", tc.input), func(t *testing.T) {
			q := queue.New(queue.WithMaxActiveJobs(tc.input))
			if got := q.ActiveSet().MaxActive(); got != tc.expectedMax {
				t.Fatalf("queue.New(WithMaxActiveJobs(%d)) MaxActive = %d, want %d", tc.input, got, tc.expectedMax)
			}
		})
	}
}

func TestQueue_SetMaxActiveJobsLimits(t *testing.T) {
	t.Parallel()

	q := queue.New(queue.WithMaxActiveJobs(4))
	if got := q.ActiveSet().MaxActive(); got != 4 {
		t.Fatalf("initial MaxActive = %d, want 4", got)
	}

	q.SetMaxActiveJobs(16)
	if got := q.ActiveSet().MaxActive(); got != 16 {
		t.Fatalf("after SetMaxActiveJobs(16), MaxActive = %d, want 16", got)
	}

	q.SetMaxActiveJobs(0) // should fallback to 4
	if got := q.ActiveSet().MaxActive(); got != 4 {
		t.Fatalf("after SetMaxActiveJobs(0), MaxActive = %d, want 4", got)
	}

	q.SetMaxActiveJobs(-10) // should fallback to 4
	if got := q.ActiveSet().MaxActive(); got != 4 {
		t.Fatalf("after SetMaxActiveJobs(-10), MaxActive = %d, want 4", got)
	}

	q.SetMaxActiveJobs(math.MaxInt)
	if got := q.ActiveSet().MaxActive(); got != math.MaxInt {
		t.Fatalf("after SetMaxActiveJobs(MaxInt), MaxActive = %d, want %d", got, math.MaxInt)
	}
}

func TestQueue_SetMaxActiveJobsConcurrentPromotionStress(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	q := queue.New(
		queue.WithStateDir(stateDir),
		queue.WithMaxActiveJobs(2),
	)

	// Populate queue with jobs
	for i := 1; i <= 20; i++ {
		parsed := &nzb.NZB{
			Files: []nzb.File{{
				Subject:  fmt.Sprintf("file%d.bin", i),
				Articles: []nzb.Article{{ID: fmt.Sprintf("art%d@test", i), Bytes: 1000, Number: 1}},
				Bytes:    1000,
			}},
		}
		job, err := queue.NewJob(parsed, queue.AddOptions{Name: fmt.Sprintf("job-%d", i)}, fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatalf("failed to create job: %v", err)
		}
		if err := q.Add(job); err != nil {
			t.Fatalf("failed to add job: %v", err)
		}
	}

	var wg sync.WaitGroup
	ctx := context.Background()

	// Goroutines updating MaxActiveJobs
	for range 5 {
		wg.Go(func() {
			for iter := range 50 {
				q.SetMaxActiveJobs((iter % 10) + 1)
			}
		})
	}

	// Goroutines triggering promote next
	for range 5 {
		wg.Go(func() {
			for range 50 {
				q.PromoteNext(ctx)
			}
		})
	}

	wg.Wait()
}
