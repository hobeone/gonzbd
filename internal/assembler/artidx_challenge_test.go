package assembler

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAssembler_ArtIdx_FallbackAndPopulated tests assembler behavior when
// MarkArticlesDoneByIdx / MarkArticlesFailedByIdx are provided vs unpopulated (nil).
func TestAssembler_ArtIdx_FallbackAndPopulated(t *testing.T) {
	t.Run("uses MarkArticlesDoneByIdx when populated", func(t *testing.T) {
		var doneIdxCount atomic.Int32
		var doneStrCount atomic.Int32

		opts := Options{
			FileInfo: func(jobID string, fileIdx int) (FileInfo, error) {
				return FileInfo{
					Path: "/tmp/fake", TotalParts: 2,
				}, nil
			},
			MarkArticlesDoneByIdx: func(jobID string, artIdxs []int32) error {
				doneIdxCount.Add(int32(len(artIdxs)))
				return nil
			},
			MarkArticlesDone: func(jobID string, msgIDs []string) error {
				doneStrCount.Add(int32(len(msgIDs)))
				return nil
			},
			DoneFlushInterval: 10 * time.Millisecond,
		}

		a := startAssembler(t, opts)

		_ = a.WriteArticle(t.Context(), WriteRequest{
			JobID: "j1", FileIdx: 0, ArtIdx: 0, MessageID: "msg0", Data: []byte("A"),
		})
		_ = a.WriteArticle(t.Context(), WriteRequest{
			JobID: "j1", FileIdx: 0, ArtIdx: 1, MessageID: "msg1", Data: []byte("B"),
		})

		time.Sleep(50 * time.Millisecond)
		_ = a.Stop()

		if doneIdxCount.Load() != 2 {
			t.Errorf("Expected 2 done by idx, got %d", doneIdxCount.Load())
		}
		if doneStrCount.Load() != 0 {
			t.Errorf("Expected 0 done by string (fallback should not trigger), got %d", doneStrCount.Load())
		}
	})

	t.Run("falls back to MarkArticlesDone when MarkArticlesDoneByIdx is nil", func(t *testing.T) {
		var doneIdxCount atomic.Int32
		var doneStrCount atomic.Int32

		opts := Options{
			FileInfo: func(jobID string, fileIdx int) (FileInfo, error) {
				return FileInfo{
					Path: "/tmp/fake", TotalParts: 2,
				}, nil
			},
			MarkArticlesDoneByIdx: nil, // hook explicitly nil (fallback mode)
			MarkArticlesDone: func(jobID string, msgIDs []string) error {
				doneStrCount.Add(int32(len(msgIDs)))
				return nil
			},
			DoneFlushInterval: 10 * time.Millisecond,
		}

		a := startAssembler(t, opts)

		_ = a.WriteArticle(t.Context(), WriteRequest{
			JobID: "j1", FileIdx: 0, ArtIdx: 0, MessageID: "msg0", Data: []byte("A"),
		})
		_ = a.WriteArticle(t.Context(), WriteRequest{
			JobID: "j1", FileIdx: 0, ArtIdx: 1, MessageID: "msg1", Data: []byte("B"),
		})

		time.Sleep(50 * time.Millisecond)
		_ = a.Stop()

		if doneIdxCount.Load() != 0 {
			t.Errorf("Expected 0 done by idx when hook is nil, got %d", doneIdxCount.Load())
		}
		if doneStrCount.Load() != 2 {
			t.Errorf("Expected 2 done by string fallback, got %d", doneStrCount.Load())
		}
	})
}

// TestAssembler_HighThroughputFlush stress-tests worker dispatch and flush under high concurrency.
func TestAssembler_HighThroughputFlush(t *testing.T) {
	var totalDone atomic.Int64
	opts := Options{
		FileInfo: func(jobID string, fileIdx int) (FileInfo, error) {
			return FileInfo{
				Path: fmt.Sprintf("/tmp/fake-%s-%d", jobID, fileIdx), TotalParts: 10000,
			}, nil
		},
		MarkArticlesDoneByIdx: func(jobID string, artIdxs []int32) error {
			totalDone.Add(int64(len(artIdxs)))
			return nil
		},
		DoneFlushInterval: 5 * time.Millisecond,
	}

	a := startAssembler(t, opts)

	const numWorkers = 10
	const requestsPerWorker = 500
	var wg sync.WaitGroup

	for w := range numWorkers {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := range requestsPerWorker {
				artIdx := int32(workerID*requestsPerWorker + i)
				req := WriteRequest{
					JobID:     fmt.Sprintf("job-%d", workerID%2),
					FileIdx:   0,
					ArtIdx:    artIdx,
					MessageID: fmt.Sprintf("msg-%d", artIdx),
					Offset:    int64(i * 10),
					Data:      []byte("0123456789"),
				}
				_ = a.WriteArticle(t.Context(), req)
			}
		}(w)
	}

	wg.Wait()
	_ = a.Stop()

	expectedTotal := int64(numWorkers * requestsPerWorker)
	if totalDone.Load() != expectedTotal {
		t.Errorf("Expected %d total done articles flushed, got %d", expectedTotal, totalDone.Load())
	}
}
