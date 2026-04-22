package assembler

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkBatchedDoneFlush measures the pathological tiny-articles path
// (articles small enough that the queue-lock acquisition dominates the
// per-article cost). With DoneFlushInterval: -1 the timer is disabled so
// Done-flushing happens once per file-complete, exercising the batching
// pure-play path. The fake MarkArticlesDone simulates a queue-write-lock
// acquisition by serialising through a mutex — this is the cost B.7
// amortises.
func BenchmarkBatchedDoneFlush(b *testing.B) {
	for _, parts := range []int{1, 16, 256} {
		b.Run(fmt.Sprintf("parts=%d", parts), func(b *testing.B) {
			benchBatched(b, parts)
		})
	}
}

func benchBatched(b *testing.B, partsPerFile int) {
	dir := b.TempDir()

	// Simulate queue write-lock contention: every MarkArticlesDone call
	// pays a fixed mutex acquisition cost. This is what batching avoids
	// — a firehose of per-article calls would each contend on this lock
	// against every RLock reader.
	var qmu sync.Mutex
	var batches atomic.Int64
	var articles atomic.Int64

	opts := Options{
		FileInfo: func(jobID string, fileIdx int) (FileInfo, error) {
			return FileInfo{
				Path:       filepath.Join(dir, fmt.Sprintf("%s_%d.dat", jobID, fileIdx)),
				TotalParts: partsPerFile,
			}, nil
		},
		MarkArticlesDone: func(_ string, ids []string) error {
			qmu.Lock()
			batches.Add(1)
			articles.Add(int64(len(ids)))
			// Simulate a non-trivial lock hold — not so long that the
			// benchmark becomes a sleep test, but enough that per-article
			// locking would be visibly worse than per-batch.
			time.Sleep(10 * time.Microsecond)
			qmu.Unlock()
			return nil
		},
		// -1 disables the periodic flush; flushes happen only on file
		// completion (batching pure-play).
		DoneFlushInterval: -1,
	}

	a := New(opts, nil)
	if err := a.Start(context.Background()); err != nil {
		b.Fatalf("Start: %v", err)
	}

	// 4-byte "articles" — the pathological tiny-article case.
	payload := []byte("AAAA")

	b.ResetTimer()
	for i := range b.N {
		jobID := fmt.Sprintf("j%d", i)
		for p := range partsPerFile {
			req := WriteRequest{
				JobID:     jobID,
				FileIdx:   0,
				MessageID: fmt.Sprintf("%s-%d", jobID, p),
				Offset:    int64(p * 4),
				Data:      append([]byte(nil), payload...),
			}
			if err := a.WriteArticle(context.Background(), req); err != nil {
				b.Fatalf("WriteArticle: %v", err)
			}
		}
	}
	if err := a.Stop(); err != nil {
		b.Fatalf("Stop: %v", err)
	}
	b.StopTimer()

	// Ratio should be ~partsPerFile: one batch per file, not one per article.
	if arts, bs := articles.Load(), batches.Load(); bs > 0 {
		b.ReportMetric(float64(arts)/float64(bs), "articles/batch")
	}
}

// BenchmarkTimerDrivenFlush exercises the ticker path with a short
// interval, to confirm the timer path doesn't starve articles/batch far
// below partsPerFile when files are multi-part.
func BenchmarkTimerDrivenFlush(b *testing.B) {
	dir := b.TempDir()
	const partsPerFile = 64

	var batches atomic.Int64
	var articles atomic.Int64

	opts := Options{
		FileInfo: func(jobID string, fileIdx int) (FileInfo, error) {
			return FileInfo{
				Path:       filepath.Join(dir, fmt.Sprintf("%s_%d.dat", jobID, fileIdx)),
				TotalParts: partsPerFile,
			}, nil
		},
		MarkArticlesDone: func(_ string, ids []string) error {
			batches.Add(1)
			articles.Add(int64(len(ids)))
			return nil
		},
		DoneFlushInterval: 5 * time.Millisecond,
	}

	a := New(opts, nil)
	if err := a.Start(context.Background()); err != nil {
		b.Fatalf("Start: %v", err)
	}
	payload := []byte("AAAA")

	b.ResetTimer()
	i := 0
	for b.Loop() {
		jobID := fmt.Sprintf("j%d", i)
		i++
		for p := range partsPerFile {
			req := WriteRequest{
				JobID:     jobID,
				FileIdx:   0,
				MessageID: fmt.Sprintf("%s-%d", jobID, p),
				Offset:    int64(p * 4),
				Data:      append([]byte(nil), payload...),
			}
			if err := a.WriteArticle(context.Background(), req); err != nil {
				b.Fatalf("WriteArticle: %v", err)
			}
		}
	}
	if err := a.Stop(); err != nil {
		b.Fatalf("Stop: %v", err)
	}
	b.StopTimer()

	if arts, bs := articles.Load(), batches.Load(); bs > 0 {
		b.ReportMetric(float64(arts)/float64(bs), "articles/batch")
	}
}
