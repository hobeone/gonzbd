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
// already finished: the id is in flight and its flush's channel is closed, so
// Prune does its bookkeeping and takes a completed channel.
//
// The closed channel is the point. Seeding inFlight alone leaves flushDone nil
// and Prune skips the receive entirely, which measures the branch that is not
// the one this benchmark names.
func BenchmarkPrune_InFlightCompleted(b *testing.B) {
	c := New(noopStore{}, time.Hour, nil)
	finished := make(chan struct{})
	close(finished)
	for b.Loop() {
		b.StopTimer()
		c.mu.Lock()
		c.inFlight["a"] = job.New("a", "A", job.PolicyFromPP(3))
		c.flushDone = finished
		c.mu.Unlock()
		b.StartTimer()
		c.Prune("a")
	}
}

// There is deliberately no benchmark for a wait on a LIVE flush. Its duration
// is the remainder of one SaveBatch, which is a property of the store and of
// the batch, not of Prune: any harness that ends the wait has to decide when to
// release the write, and it then measures that decision. The version this tree
// carried measured whichever of the two raced first, and releasing on a poll of
// inFlight measures the poll interval (67-132 us at 10 us). What Prune itself
// costs on that path is the receive measured by BenchmarkPrune_InFlightCompleted.
