package durability

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
)

// closedFileTarget reports one open file and then answers every operation on
// it with ErrFileNotOpen, as the worker does when the file was closed between
// the barrier's listing and its first call.
type closedFileTarget struct {
	drained int
}

func (s *closedFileTarget) Files() []int32    { return []int32{0} }
func (s *closedFileTarget) Path(int32) string { return "/downloads/a.bin" }
func (s *closedFileTarget) Drain(context.Context, int32) ([]WrittenArticle, error) {
	s.drained++
	return nil, fmt.Errorf("assembler: job x file 0 is not open: %w", ErrFileNotOpen)
}
func (s *closedFileTarget) Sync(context.Context, int32) error { return nil }
func (s *closedFileTarget) Stat(int32) (int64, error)         { return 100, nil }
func (s *closedFileTarget) Confirm(context.Context, int32)    {}

// TestBarrier_ADeliberatelyClosedFileDoesNotStallTheJob pins that losing a
// file to an ordinary close is not a storage fault.
//
// finalizeCompletedFile releases the per-job barrier mutex before its deferred
// CloseFile, so a checkpoint can hold the lock, take the file from Files(),
// have the close processed, and then be told the file is not open. The worker
// documents that answer as "a bookkeeping disagreement, not a storage fault
// ... blaming storage for it would be the A1 conflation in reverse" — and the
// barrier then classified it as one anyway. No errno matches, so R18 made it
// retryable, and the job was paused with a reason naming a device that is
// perfectly healthy and an operator action that does not exist.
//
// The same answer arrives from CancelJob and from CloseJobHandles on a job
// entering post-processing. All three are deliberate, and all three drain and
// sync before closing, so there is nothing left for this barrier to do.
func TestBarrier_ADeliberatelyClosedFileDoesNotStallTheJob(t *testing.T) {
	stall := &recordingStall{}
	b := NewBarrier(NewSQLiteRunStore(openTestDB(t)),
		&recordingAcker{}, stall, slog.New(slog.DiscardHandler))

	tgt := &closedFileTarget{}
	_, err := b.Run(context.Background(), "job-1", tgt)

	// Grounding: the fixture must actually have been asked, or "no stall" is
	// true for a reason unrelated to the defect.
	if tgt.drained == 0 {
		t.Fatal("the barrier never drained the file, so it cannot have seen the race")
	}
	if err != nil {
		t.Errorf("Run returned %v; a file closed underneath the barrier leaves nothing "+
			"to checkpoint, which is not a failure of this run", err)
	}
	if len(stall.stalled) != 0 {
		t.Fatalf("the job was stalled with %q. The file was closed deliberately — by a "+
			"completed finalize, a cancel, or a job entering post-processing — and each "+
			"of those drained and synced first. Pausing a healthy job here surfaces a "+
			"storage reason the operator cannot act on", stall.stalled[0])
	}
	if len(stall.failed) != 0 {
		t.Fatalf("the job was FAILED over a closed file: %v", stall.failed[0])
	}
}
