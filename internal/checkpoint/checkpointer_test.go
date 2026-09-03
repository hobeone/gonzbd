package checkpoint

import (
	"context"
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
