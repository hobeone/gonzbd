// Package checkpoint owns batched writes of job progress (per-file completion,
// CRC, and failed articles) to the database.
//
// It exists because internal/queue had SIX single-job writers beside its
// batched periodic save, each closing a read-after-write window against one
// transition (git grep -n 'store\.Update(' -- internal/queue/ ':!*_test.go'
// returned 6 lines before the swap). Five of those transitions are deleted by
// the swap; the sixth, ReplaceFromRuns' cleared Complete/CRC, survives because
// §10.1 keeps resumeAllJobs — and it is served here by Flush rather than by a
// second writer.
package checkpoint

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// Store is the persistence this package needs and no more.
type Store interface {
	SaveBatch(ctx context.Context, cps []job.Checkpoint) error
}

// Checkpointer batches job-state writes. Mark records that a job moved; the
// ticker and Flush are the only things that write.
type Checkpointer struct {
	store Store
	every time.Duration
	log   *slog.Logger

	mu    sync.Mutex
	dirty map[string]*job.Job
}

// New constructs a Checkpointer. every is the batch cadence.
func New(store Store, every time.Duration, log *slog.Logger) *Checkpointer {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Checkpointer{store: store, every: every, log: log, dirty: map[string]*job.Job{}}
}

// Mark records that a job's state has moved and should be written at the next
// batch. It is cheap and never writes: coalescing repeated marks for one job
// into one row is the whole point.
func (c *Checkpointer) Mark(j *job.Job) {
	c.mu.Lock()
	c.dirty[j.ID()] = j
	c.mu.Unlock()
}

// DirtyCount returns the number of jobs currently marked dirty awaiting flush.
func (c *Checkpointer) DirtyCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.dirty)
}

// Flush writes every marked job now and clears the set. It is synchronous
// because ReplaceFromRuns needs the row on disk before re-hydration can read
// it — the one read-after-write window the swap does not delete.
//
// A failed SaveBatch does not lose the jobs it was carrying: Flush swaps in a
// fresh map before writing so marks arriving during the write land in the new
// map, then on error re-merges the un-remarked jobs back into c.dirty.
func (c *Checkpointer) Flush(ctx context.Context) error {
	c.mu.Lock()
	if len(c.dirty) == 0 {
		c.mu.Unlock()
		return nil
	}
	batch := c.dirty
	c.dirty = make(map[string]*job.Job)
	c.mu.Unlock()

	cps := make([]job.Checkpoint, 0, len(batch))
	for _, j := range batch {
		cps = append(cps, j.Checkpoint())
	}

	if err := c.store.SaveBatch(ctx, cps); err != nil {
		c.mu.Lock()
		for id, j := range batch {
			if _, remarked := c.dirty[id]; !remarked {
				c.dirty[id] = j
			}
		}
		c.mu.Unlock()
		return err
	}
	return nil
}

// Run drives the periodic batch until ctx is cancelled, then flushes once more.
func (c *Checkpointer) Run(ctx context.Context) error {
	t := time.NewTicker(c.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return c.Flush(context.WithoutCancel(ctx))
		case <-t.C:
			if err := c.Flush(ctx); err != nil {
				c.log.Error("checkpoint flush failed", "error", err)
			}
		}
	}
}
