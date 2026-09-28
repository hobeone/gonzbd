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
	"maps"
	"runtime"
	"sync"
	"time"
	"weak"

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

	flushMu  sync.Mutex
	mu       sync.Mutex
	dirty    map[string]*job.Job
	inFlight map[string]*job.Job
	// flushDone is non-nil exactly while a flush holds a batch, and is closed
	// when that flush's write has returned. flushing is that flush's batch.
	//
	// Both are written only by Flush, which is what makes a second Prune of
	// one job correct: inFlight answers "may a failing flush re-merge this?"
	// and Prune clears it, so it cannot also answer "is a flush writing this?"
	// for a caller that arrives while another Prune is already waiting.
	flushDone chan struct{}
	flushing  map[string]*job.Job
	// pruned holds the job instances Prune has taken and Unprune has not
	// given back; Mark refuses them. Keyed by instance, not ID, so a later
	// job under the same ID still marks; weakly, so the entry goes when the
	// job is collected, which is also when the last Mark of it becomes
	// impossible.
	pruned map[weak.Pointer[job.Job]]runtime.Cleanup
}

// New constructs a Checkpointer. every is the batch cadence.
func New(store Store, every time.Duration, log *slog.Logger) *Checkpointer {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Checkpointer{
		store:    store,
		every:    every,
		log:      log,
		dirty:    map[string]*job.Job{},
		inFlight: map[string]*job.Job{},
		pruned:   map[weak.Pointer[job.Job]]runtime.Cleanup{},
	}
}

// Mark records that a job's state has moved and should be written at the next
// batch. It is cheap and never writes: coalescing repeated marks for one job
// into one row is the whole point.
//
// A mark of an instance Prune has taken is dropped. A late result can mark an
// instance after its Prune — AckDurable, for one, looks the job up and marks
// it with nothing spanning a departure between the two — and left in, that
// mark would be written by whichever flush came next, where a retry that had
// re-seeded job_files under the same ID would satisfy SaveProgress's job_files
// gate for it.
func (c *Checkpointer) Mark(j *job.Job) {
	key := weak.Make(j)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, gone := c.pruned[key]; gone {
		return
	}
	c.dirty[j.ID()] = j
}

// Prune removes a job's ID from both the dirty set and any in-flight flush
// batch, refuses every later Mark of this instance of it, and does not return
// until a flush that was carrying the ID has finished writing. A departure
// reclaims the job's rows once Prune returns, so a batch still in the store at
// that moment would re-insert what the reclaim took (#561).
//
// The wait is the whole point, so it is not bounded: returning early would
// hand the caller exactly the state the wait exists to prevent. It only
// happens when the job is in flight, which is only for the duration of one
// SaveBatch.
//
// The refusal is what covers a Mark arriving AFTER this returns.
// durability.SaveProgress's job_files gate covers it only until a retry of the
// same ID re-seeds those rows, and a flush landing after that seed would write
// this instance's failed articles onto the retry. The refusal is keyed by
// instance, so the retry's own job still marks, and it lasts as long as the
// instance is reachable (see pruned).
func (c *Checkpointer) Prune(j *job.Job) {
	id := j.ID()
	key := weak.Make(j)
	c.mu.Lock()
	if _, ok := c.pruned[key]; !ok {
		c.pruned[key] = runtime.AddCleanup(j, c.forgetPruned, key)
	}
	delete(c.dirty, id)
	delete(c.inFlight, id)
	var done chan struct{}
	if _, carried := c.flushing[id]; carried {
		done = c.flushDone
	}
	c.mu.Unlock()
	if done != nil {
		<-done
	}
}

// Unprune withdraws Prune's refusal of j's marks, for a departure that gave
// up with j still registered. A checkpoint carries the job's whole state, so
// the next Mark of j also writes whatever the prune dropped from the dirty set.
func (c *Checkpointer) Unprune(j *job.Job) {
	key := weak.Make(j)
	c.mu.Lock()
	defer c.mu.Unlock()
	if cleanup, ok := c.pruned[key]; ok {
		cleanup.Stop()
		delete(c.pruned, key)
	}
}

func (c *Checkpointer) forgetPruned(key weak.Pointer[job.Job]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pruned, key)
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
// map, then on error re-merges the un-remarked, un-pruned jobs back into c.dirty.
func (c *Checkpointer) Flush(ctx context.Context) error {
	c.flushMu.Lock()
	defer c.flushMu.Unlock()

	c.mu.Lock()
	if len(c.dirty) == 0 {
		c.mu.Unlock()
		return nil
	}
	batch := c.dirty
	c.dirty = make(map[string]*job.Job)
	maps.Copy(c.inFlight, batch)
	done := make(chan struct{})
	c.flushDone = done
	c.flushing = batch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.flushDone = nil
		c.flushing = nil
		c.mu.Unlock()
		close(done)
	}()

	cps := make([]job.Checkpoint, 0, len(batch))
	for _, j := range batch {
		cps = append(cps, j.Checkpoint())
	}

	err := c.store.SaveBatch(ctx, cps)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		for id, j := range batch {
			if _, stillInFlight := c.inFlight[id]; stillInFlight {
				if _, remarked := c.dirty[id]; !remarked {
					c.dirty[id] = j
				}
			}
		}
	}
	for id := range batch {
		delete(c.inFlight, id)
	}
	return err
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
