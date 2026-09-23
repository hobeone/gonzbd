package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/checkpoint"
	"github.com/hobeone/gonzbd/internal/job"
)

// heldSaveBatchStore keeps one SaveBatch open until released.
type heldSaveBatchStore struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *heldSaveBatchStore) SaveBatch(context.Context, []job.Checkpoint) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return nil
}

// TestRemoveJob_WaitsForACheckpointThatIsWritingTheJob pins the ordering the
// reclaim rule depends on: the departure's Prune comes before its Reclaim, and
// Prune does not return while a flush is still writing the job.
//
// Without it the reclaim runs against a batch that has not committed. The
// liveness guard refuses that batch's inserts for as long as the job stays
// departed — but a retry of the same id re-seeds job_files, and then the guard
// passes and the previous run's failure marks land on the new one.
func TestRemoveJob_WaitsForACheckpointThatIsWritingTheJob(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 2)
	st := &heldSaveBatchStore{entered: make(chan struct{}), release: make(chan struct{})}
	application.checkpointer = checkpoint.New(st, time.Hour, application.log)
	application.checkpointer.Mark(j)

	flushed := make(chan error, 1)
	go func() { flushed <- application.checkpointer.Flush(context.Background()) }()
	<-st.entered

	removed := make(chan error, 1)
	go func() { removed <- application.RemoveJob(context.Background(), j.ID(), false) }()

	select {
	case err := <-removed:
		t.Fatalf("RemoveJob returned (%v) while a checkpoint was still writing this job; "+
			"its reclaim deletes rows the in-flight batch is about to re-insert", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(st.release)
	if err := <-removed; err != nil {
		t.Errorf("RemoveJob: %v", err)
	}
	if err := <-flushed; err != nil {
		t.Errorf("Flush: %v", err)
	}
}
