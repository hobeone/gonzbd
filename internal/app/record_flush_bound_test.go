package app

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
)

// deadlineStore records the deadline of the ctx each ApplyRecord is given.
type deadlineStore struct {
	mu       sync.Mutex
	deadline []time.Time // zero when the call's ctx had none
	called   chan struct{}
}

func (s *deadlineStore) ApplyRecord(ctx context.Context, _ []durability.RecordBatch) error {
	d, _ := ctx.Deadline()
	s.mu.Lock()
	s.deadline = append(s.deadline, d)
	first := len(s.deadline) == 1
	s.mu.Unlock()
	if first {
		close(s.called)
	}
	return nil
}

// TestRecorder_RunBoundsEachFlush: the periodic flush holds wmu while it
// writes, and the assembler's worker waits for wmu in handleFileUntrusted, so
// a store that never answers must not hold it past recorderFlushTimeout.
func TestRecorder_RunBoundsEachFlush(t *testing.T) {
	t.Parallel()
	st := &deadlineStore{called: make(chan struct{})}
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, slog.Default())
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan struct{})
	go func() { r.run(ctx, time.Millisecond); close(finished) }()
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Length: 10})
	select {
	case <-st.called:
	case <-time.After(30 * time.Second):
		t.Fatal("the periodic flush never reached the store")
	}
	latest := time.Now().Add(recorderFlushTimeout) // the flush began before this
	cancel()
	<-finished

	st.mu.Lock()
	d := st.deadline[0]
	st.mu.Unlock()
	if d.IsZero() {
		t.Fatal("the periodic flush wrote with a ctx that has no deadline: a stalled store would hold wmu, and with it the assembler's worker, indefinitely")
	}
	if d.After(latest) {
		t.Errorf("flush deadline %v is later than %v, the bound recorderFlushTimeout allows", d, latest)
	}
}
