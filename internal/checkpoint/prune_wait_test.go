package checkpoint

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// blockingStore holds a SaveBatch open until released, and records whether the
// write had returned by the time something else asked.
type blockingStore struct {
	entered  chan struct{}
	release  chan struct{}
	mu       sync.Mutex
	returned bool
}

func (s *blockingStore) SaveBatch(context.Context, []job.Checkpoint) error {
	close(s.entered)
	<-s.release
	s.mu.Lock()
	s.returned = true
	s.mu.Unlock()
	return nil
}

func (s *blockingStore) writeReturned() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.returned
}

// TestPrune_WaitsForAFlushCarryingTheJob is the pin for the half of #561 the
// liveness guard cannot reach: a batch captured before a departure, committing
// after a RETRY of the same id has re-seeded job_files, passes the guard and
// writes the previous run's failure marks onto the new one.
//
// Prune returning only after the write completes is what makes a departure's
// Reclaim — which always follows its Prune — take those rows.
func TestPrune_WaitsForAFlushCarryingTheJob(t *testing.T) {
	t.Parallel()
	st := &blockingStore{entered: make(chan struct{}), release: make(chan struct{})}
	c := New(st, time.Hour, nil)
	c.Mark(job.New("a", "A", job.PolicyFromPP(3)))

	flushed := make(chan error, 1)
	go func() { flushed <- c.Flush(context.Background()) }()
	<-st.entered // the batch is with the store, and "a" is in flight

	pruned := make(chan struct{})
	go func() {
		c.Prune("a")
		close(pruned)
	}()
	// Prune has done its bookkeeping and everything after it is the wait.
	waitUntilNotInFlight(t, c, "a")

	select {
	case <-pruned:
		t.Fatal("Prune returned while the flush carrying the job was still inside " +
			"SaveBatch; the caller's Reclaim now runs against a batch that has not " +
			"committed, and the rows it deletes are re-inserted behind it")
	case <-time.After(200 * time.Millisecond):
	}

	close(st.release)
	<-pruned
	if !st.writeReturned() {
		t.Error("Prune returned before the write completed")
	}
	if err := <-flushed; err != nil {
		t.Errorf("Flush: %v", err)
	}
}

// TestPrune_DoesNotWaitForAFlushThatDoesNotCarryTheJob pins the other half of
// the condition: the wait costs a removal nothing when the job is not being
// written, which is every removal that does not land inside a checkpoint.
func TestPrune_DoesNotWaitForAFlushThatDoesNotCarryTheJob(t *testing.T) {
	t.Parallel()
	st := &blockingStore{entered: make(chan struct{}), release: make(chan struct{})}
	c := New(st, time.Hour, nil)
	c.Mark(job.New("a", "A", job.PolicyFromPP(3)))

	flushed := make(chan error, 1)
	go func() { flushed <- c.Flush(context.Background()) }()
	<-st.entered

	done := make(chan struct{})
	go func() {
		c.Prune("b") // not in this batch
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Prune blocked on a flush that was not carrying the job, so every " +
			"removal now waits for an unrelated checkpoint write")
	}

	close(st.release)
	if err := <-flushed; err != nil {
		t.Errorf("Flush: %v", err)
	}
}
