package checkpoint

import (
	"context"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

type noopStore struct{}

func (noopStore) SaveBatch(context.Context, []job.Checkpoint) error { return nil }

// BenchmarkPrune_NotInFlight measures what the #561 wait costs the removal path
// in the case every removal that does not land inside a checkpoint write takes:
// the job is not being written, so Prune does its bookkeeping and returns.
func BenchmarkPrune_NotInFlight(b *testing.B) {
	c := New(noopStore{}, time.Hour, nil)
	c.Mark(job.New("other", "O", job.PolicyFromPP(3)))
	for b.Loop() {
		c.Prune("a")
	}
}

// BenchmarkPrune_InFlightCompleted measures the other case with the write
// already finished: the id is in flight, so Prune takes flushMu uncontended.
func BenchmarkPrune_InFlightCompleted(b *testing.B) {
	c := New(noopStore{}, time.Hour, nil)
	for b.Loop() {
		b.StopTimer()
		c.mu.Lock()
		c.inFlight["a"] = job.New("a", "A", job.PolicyFromPP(3))
		c.mu.Unlock()
		b.StartTimer()
		c.Prune("a")
	}
}
