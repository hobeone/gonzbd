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

// failingStore fails its first N calls to SaveBatch, then delegates to a
// recordingStore for every call after — enough to observe both "the write
// failed" and "a later successful write actually carries the lost rows".
//
// beforeFail, if set, runs on a failing call before the error is returned —
// this is what lets a test land a Mark truly INSIDE Flush's failing
// SaveBatch call, between the map swap and the post-error re-merge, rather
// than only after Flush has already returned. Without it, a Mark issued
// after Flush returns can never distinguish a merging re-merge from one that
// overwrites the dirty set outright: by the time Mark runs, the re-merge has
// already finished either way.
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

	// A second Flush with nothing marked since the first must not write: the
	// dirty set is cleared by the first Flush, and Flush's early return on an
	// empty set is what makes that hold.
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

// TestCheckpointer_FailedFlushLeavesJobsMarked pins the fix for a real bug:
// Flush used to clear the dirty set unconditionally before calling
// SaveBatch, so a failed write lost those jobs permanently — Run logs the
// error and loops, the next tick sees an empty set, and a job that had just
// settled would never be retried. A Flush that fails must re-mark every job
// it was carrying, and a Flush that then succeeds must actually write them.
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

	// The failed write must not have lost a and b: a second Flush, this time
	// against a store that succeeds, has to carry both.
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("retry Flush: %v", err)
	}
	if len(st.batches) != 1 {
		t.Fatalf("retry Flush must write exactly one batch; got %d", len(st.batches))
	}
	if got := len(st.batches[0]); got != 2 {
		t.Fatalf("retry batch size = %d, want 2 (a and b survived the failed Flush)", got)
	}
}

// TestCheckpointer_MarkDuringFailedFlushIsNotLost pins the narrower half of
// the same fix: a DIFFERENT job marked WHILE SaveBatch is failing — after
// Flush's map swap has already emptied c.dirty, but before Flush's
// post-error re-merge runs — must survive alongside the re-merged ones
// rather than being dropped by a re-merge that overwrites the dirty set
// outright instead of merging into it. beforeFail is what lands the Mark in
// that exact window; a Mark issued only after Flush returns cannot tell the
// two implementations apart, because by then the re-merge has already run.
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
