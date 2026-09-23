package checkpoint

import (
	"context"
	"sync"
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

// BenchmarkPrune_InFlightActive is Agent B's shape, taken during the bake-off:
// a real flush goroutine running concurrently, so the measurement includes the
// scheduling a live wait actually costs rather than a receive on an already
// closed channel.
func BenchmarkPrune_InFlightActive(b *testing.B) {
	for b.Loop() {
		b.StopTimer()
		st := &releasableStore{released: make(chan struct{})}
		c := New(st, time.Hour, nil)
		c.Mark(job.New("a", "A", job.PolicyFromPP(3)))
		flushed := make(chan struct{})
		go func() { _ = c.Flush(context.Background()); close(flushed) }()
		<-st.entered()
		go func() { close(st.released) }()
		b.StartTimer()
		c.Prune("a")
		b.StopTimer()
		<-flushed
		b.StartTimer()
	}
}

// releasableStore blocks one SaveBatch until released.
type releasableStore struct {
	once     sync.Once
	in       chan struct{}
	released chan struct{}
}

func (s *releasableStore) entered() chan struct{} {
	s.once.Do(func() { s.in = make(chan struct{}) })
	return s.in
}

func (s *releasableStore) SaveBatch(context.Context, []job.Checkpoint) error {
	close(s.entered())
	<-s.released
	return nil
}
