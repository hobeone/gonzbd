package checkpoint

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

type recordingStore struct{ batches [][]job.Checkpoint }

func (s *recordingStore) SaveBatch(_ context.Context, cps []job.Checkpoint) error {
	cp := make([]job.Checkpoint, len(cps))
	copy(cp, cps)
	s.batches = append(s.batches, cp)
	return nil
}

// TestCheckpointer_CoalescesMarksIntoOneBatch pins the reason this type exists:
// six single-job writers in internal/queue each closed a read-after-write
// window against one transition, and every one of them was a second writer of
// state the periodic save already owned (Rule 2).
func TestCheckpointer_CoalescesMarksIntoOneBatch(t *testing.T) {
	st := &recordingStore{}
	c := New(st, time.Hour, nil) // never fires on its own; Flush drives it

	a := job.New("a", "A", job.PolicyFromPP(3))
	b := job.New("b", "B", job.PolicyFromPP(3))

	c.Mark(a)
	c.Mark(a) // same job twice must not produce two rows
	c.Mark(b)

	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(st.batches) != 1 {
		t.Fatalf("batches = %d, want 1", len(st.batches))
	}
	if got := len(st.batches[0]); got != 2 {
		t.Fatalf("batch size = %d, want 2 (a and b, a coalesced)", got)
	}

	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	if len(st.batches) != 1 {
		t.Fatalf("a second Flush with nothing marked must not write; batches = %d", len(st.batches))
	}
}

// TestCheckpointer_FlushIsSynchronous pins the surviving read-after-write
// window. workset.go:453 persisted a cleared Complete/CRC before re-hydration
// could re-read the stale row; §10.1's banner keeps resumeAllJobs, which
// reaches it via ReplaceFromRuns, so that window survives the swap and Flush
// is what closes it.
func TestCheckpointer_FlushIsSynchronous(t *testing.T) {
	st := &recordingStore{}
	c := New(st, time.Hour, nil)

	j := job.New("a", "A", job.PolicyFromPP(3))
	c.Mark(j)

	if len(st.batches) != 0 {
		t.Fatal("Mark must not write; only Flush and the ticker write")
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(st.batches) != 1 {
		t.Fatalf("Flush must write synchronously; batches = %d", len(st.batches))
	}
}

type failingStore struct {
	recordingStore
	failsLeft  int
	beforeFail func()
}

var errSaveBatchFailed = errors.New("checkpoint: simulated SaveBatch failure")

func (s *failingStore) SaveBatch(ctx context.Context, cps []job.Checkpoint) error {
	if s.failsLeft > 0 {
		s.failsLeft--
		if s.beforeFail != nil {
			s.beforeFail()
		}
		return errSaveBatchFailed
	}
	return s.recordingStore.SaveBatch(ctx, cps)
}

func TestCheckpointer_DirtyCountAndRun(t *testing.T) {
	st := &recordingStore{}
	// Use an hour interval so the ticker cannot fire and race with shutdown;
	// the flush tested below must come strictly from ctx.Done().
	c := New(st, time.Hour, nil)

	j := job.New("a", "A", job.PolicyFromPP(3))
	c.Mark(j)
	if got := c.DirtyCount(); got != 1 {
		t.Fatalf("DirtyCount = %d, want 1", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.Run(ctx)
	}()

	// Give the goroutine a moment to start and block in select, then cancel.
	time.Sleep(10 * time.Millisecond)
	cancel()

	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(st.batches) != 1 {
		t.Fatalf("batches = %d, want 1 (flushed on ctx.Done)", len(st.batches))
	}
	if c.DirtyCount() != 0 {
		t.Fatalf("DirtyCount after Run cancelled = %d, want 0", c.DirtyCount())
	}
}

// TestCheckpointer_FailedFlushLeavesJobsMarked pins that a failed SaveBatch
// re-merges dirty jobs back into the dirty set so that a subsequent flush
// actually persists them.
func TestCheckpointer_FailedFlushLeavesJobsMarked(t *testing.T) {
	st := &failingStore{failsLeft: 1}
	c := New(st, time.Hour, nil)

	a := job.New("a", "A", job.PolicyFromPP(3))
	b := job.New("b", "B", job.PolicyFromPP(3))
	c.Mark(a)
	c.Mark(b)

	if err := c.Flush(context.Background()); !errors.Is(err, errSaveBatchFailed) {
		t.Fatalf("Flush: got %v, want errSaveBatchFailed", err)
	}
	if len(st.batches) != 0 {
		t.Fatalf("a failed SaveBatch must not have recorded a batch; got %d", len(st.batches))
	}
	if got := c.DirtyCount(); got != 2 {
		t.Fatalf("DirtyCount after failed Flush = %d, want 2", got)
	}

	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("retry Flush: %v", err)
	}
	if len(st.batches) != 1 {
		t.Fatalf("retry Flush must write exactly one batch; got %d", len(st.batches))
	}
	if got := len(st.batches[0]); got != 2 {
		t.Fatalf("retry batch size = %d, want 2 (a and b survived the failed Flush)", got)
	}
	if got := c.DirtyCount(); got != 0 {
		t.Fatalf("DirtyCount after successful retry Flush = %d, want 0", got)
	}
}

// TestCheckpointer_MarkDuringFailedFlushIsNotLost pins that a job marked WHILE
// SaveBatch is in flight survives alongside the re-merged jobs from the failed batch.
func TestCheckpointer_MarkDuringFailedFlushIsNotLost(t *testing.T) {
	a := job.New("a", "A", job.PolicyFromPP(3))
	b := job.New("b", "B", job.PolicyFromPP(3))

	st := &failingStore{failsLeft: 1}
	c := New(st, time.Hour, nil)
	st.beforeFail = func() { c.Mark(b) }

	c.Mark(a)
	if err := c.Flush(context.Background()); !errors.Is(err, errSaveBatchFailed) {
		t.Fatalf("Flush: got %v, want errSaveBatchFailed", err)
	}
	if got := c.DirtyCount(); got != 2 {
		t.Fatalf("DirtyCount = %d, want 2", got)
	}

	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("retry Flush: %v", err)
	}
	if len(st.batches) != 1 {
		t.Fatalf("retry Flush must write exactly one batch; got %d", len(st.batches))
	}
	if got := len(st.batches[0]); got != 2 {
		t.Fatalf("retry batch size = %d, want 2 (a re-merged, b marked meanwhile)", got)
	}
}

// TestCheckpointer_RemarkDuringFailedFlushNotOverwrittenWithStale pins that a
// job re-marked during an in-flight flush is not overwritten with its older stale
// version during re-merge.
func TestCheckpointer_RemarkDuringFailedFlushNotOverwrittenWithStale(t *testing.T) {
	a1 := job.New("a", "Original", job.PolicyFromPP(3))
	a2 := job.New("a", "Updated", job.PolicyFromPP(3))
	if err := a2.SetIntent(job.IntentPause); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}

	st := &failingStore{failsLeft: 1}
	c := New(st, time.Hour, nil)
	st.beforeFail = func() { c.Mark(a2) }

	c.Mark(a1)
	if err := c.Flush(context.Background()); !errors.Is(err, errSaveBatchFailed) {
		t.Fatalf("Flush: got %v, want errSaveBatchFailed", err)
	}

	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("retry Flush: %v", err)
	}
	if len(st.batches) != 1 {
		t.Fatalf("retry Flush must write exactly one batch; got %d", len(st.batches))
	}
	if got := len(st.batches[0]); got != 1 {
		t.Fatalf("retry batch size = %d, want 1", got)
	}
	if got := st.batches[0][0].Intent; got != job.IntentPause {
		t.Fatalf("flushed job intent = %v, want %v (a2 should not be overwritten by stale a1)", got, job.IntentPause)
	}
}
