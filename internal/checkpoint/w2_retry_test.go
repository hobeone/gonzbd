package checkpoint

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// The W2 end-to-end model below is Agent B's, taken into this tree during the
// step-4 bake-off. It states the outcome the wait exists for, which the tests
// above pin only as separate halves: the wait, and the departure ordering.
//
// w2Store is a model of durability.Store for the one property W2 turns on: a
// failed mark is written only while the job has job_files, which Admit seeds
// and Reclaim takes. Without Prune's wait the sequence below re-seeds through
// the retry's Admit before the stale batch commits, and the model writes the
// previous run's mark onto the new one.

type w2Store struct {
	mu             sync.Mutex
	jobFiles       map[string]bool
	failedArticles map[string][]int
	inSaveBatch    chan struct{}
	releaseBatch   chan struct{}
}

func newW2Store() *w2Store {
	return &w2Store{
		jobFiles:       make(map[string]bool),
		failedArticles: make(map[string][]int),
		inSaveBatch:    make(chan struct{}),
		releaseBatch:   make(chan struct{}),
	}
}

func (s *w2Store) Admit(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobFiles[id] = true
}

func (s *w2Store) Reclaim(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.jobFiles, id)
	delete(s.failedArticles, id)
}

func (s *w2Store) FailedArticles(id string) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.failedArticles[id])
}

func (s *w2Store) SaveBatch(_ context.Context, cps []job.Checkpoint) error {
	select {
	case <-s.inSaveBatch:
	default:
		close(s.inSaveBatch)
	}
	<-s.releaseBatch

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cp := range cps {
		if s.jobFiles[cp.ID] {
			s.failedArticles[cp.ID] = append(s.failedArticles[cp.ID], 99)
		}
	}
	return nil
}

// TestCheckpointer_W2_RetryDoesNotInheritStaleMarks drives the whole W2
// sequence — Prune, Reclaim, the retry's Admit, then the stale batch commits —
// and asserts the retried run inherits nothing.
func TestCheckpointer_W2_RetryDoesNotInheritStaleMarks(t *testing.T) {
	st := newW2Store()
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(st.releaseBatch) }) })
	c := New(st, time.Hour, nil)

	// Initial run: admit job "a" and mark it dirty.
	st.Admit("a")
	a := job.New("a", "A", job.PolicyFromPP(3))
	c.Mark(a)

	flushDone := make(chan error, 1)
	go func() {
		flushDone <- c.Flush(context.Background())
	}()

	<-st.inSaveBatch

	// Departure sequence runs: Prune("a"), Reclaim("a"), then retry Admit("a").
	departureAndRetryDone := make(chan struct{})
	go func() {
		c.Prune("a")
		st.Reclaim("a")
		st.Admit("a") // retry arrives
		close(departureAndRetryDone)
	}()

	// Assert that while SaveBatch is held, departureAndRetry cannot complete
	// because Prune is waiting for Flush.
	select {
	case <-departureAndRetryDone:
		t.Fatal("departure and retry completed before in-flight SaveBatch was released")
	case <-time.After(50 * time.Millisecond):
		// Expected: blocked on Prune.
	}

	// Release SaveBatch.
	release.Do(func() { close(st.releaseBatch) })

	select {
	case <-departureAndRetryDone:
	case <-time.After(2 * time.Second):
		t.Fatal("departure and retry did not complete after SaveBatch release")
	}

	if err := <-flushDone; err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Stale failed marks from the old run must NOT survive onto the retried job!
	failed := st.FailedArticles("a")
	if len(failed) != 0 {
		t.Fatalf("W2: retried job inherited stale failed articles %v, want none", failed)
	}
}
