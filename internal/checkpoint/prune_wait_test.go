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
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(st.release) }) })
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

	release.Do(func() { close(st.release) })
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
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(st.release) }) })
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

	release.Do(func() { close(st.release) })
	if err := <-flushed; err != nil {
		t.Errorf("Flush: %v", err)
	}
}

// TestPrune_SecondDepartureOfOneJobAlsoWaits pins the case a single map could
// not answer: two departures of one job overlapping a flush that carries it.
//
// RemoveJob and the finalizer both prune, and nothing serialises them for one
// job — a removal landing on a job that is finalizing reaches both. If the
// second caller returns early it goes on to reclaim while the batch is still
// being written, which is the whole defect for the retry that follows.
func TestPrune_SecondDepartureOfOneJobAlsoWaits(t *testing.T) {
	t.Parallel()
	st := &blockingStore{entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(st.release) }) })
	c := New(st, time.Hour, nil)
	c.Mark(job.New("a", "A", job.PolicyFromPP(3)))

	flushed := make(chan error, 1)
	go func() { flushed <- c.Flush(context.Background()) }()
	<-st.entered

	first := make(chan struct{})
	go func() {
		c.Prune("a")
		close(first)
	}()
	waitUntilNotInFlight(t, c, "a") // the first prune has done its bookkeeping

	second := make(chan struct{})
	go func() {
		c.Prune("a")
		close(second)
	}()

	select {
	case <-second:
		t.Fatal("the second departure of this job returned while the flush carrying it " +
			"was still inside SaveBatch; it now reclaims rows the batch is about to " +
			"write, and a retry of the same id inherits the previous run's failure marks")
	case <-time.After(100 * time.Millisecond):
	}

	once.Do(func() { close(st.release) })
	<-first
	<-second
	if !st.writeReturned() {
		t.Error("a departure returned before the write completed")
	}
	if err := <-flushed; err != nil {
		t.Errorf("Flush: %v", err)
	}
}
