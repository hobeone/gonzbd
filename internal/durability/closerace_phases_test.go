package durability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"syscall"
	"testing"
)

// lateCloseTarget reports two open files and closes one of them at a chosen
// phase, answering every later operation on it with ErrFileNotOpen.
//
// Two files rather than one, because the cost of getting this wrong is not
// only the closed file: the whole checkpoint was discarded, so the file that
// drained and fsynced successfully lost its ack as well. A single-file fixture
// cannot observe that.
type lateCloseTarget struct {
	// closeAt is the phase at which file 0 becomes closed: "sync" or "stat".
	closeAt   string
	closed    bool
	synced    []int32
	confirmed []int32
}

func (s *lateCloseTarget) Files() []int32    { return []int32{0, 1} }
func (s *lateCloseTarget) Path(int32) string { return "/downloads/a.bin" }

func (s *lateCloseTarget) Drain(_ context.Context, idx int32) ([]WrittenArticle, error) {
	return []WrittenArticle{{FileIdx: idx, ArtIdx: idx, Offset: 0, Length: 10}}, nil
}

func (s *lateCloseTarget) Sync(_ context.Context, idx int32) error {
	if idx == 0 && s.closeAt == "sync" {
		s.closed = true
		return fmt.Errorf("assembler: job x file 0 is not open: %w", ErrFileNotOpen)
	}
	if idx == 0 && s.closeAt == "stat" {
		s.closed = true
	}
	s.synced = append(s.synced, idx)
	return nil
}

func (s *lateCloseTarget) Stat(idx int32) (int64, int64, error) {
	if idx == 0 && s.closed {
		return 0, 0, fmt.Errorf("assembler: job x file 0 is not open: %w", ErrFileNotOpen)
	}
	return 100, 1, nil
}

func (s *lateCloseTarget) ArticleCount(int32) int { return 2 }
func (s *lateCloseTarget) FileLocalOrdinal(_ int32, a int32) (int, bool) {
	return int(a), int(a) < 2
}
func (s *lateCloseTarget) Confirm(_ context.Context, idx int32) {
	s.confirmed = append(s.confirmed, idx)
}

// TestBarrier_ACloseAfterTheDrainDropsOnlyThatFile pins the pairing between
// the two collections a dropped file has to leave.
//
// Phase 2 deleted the file from `drained` and left it in `files`, so phase 3
// went on to Stat it, was told the same thing one phase later, and — with
// nothing there recognising the sentinel — classified it as a storage fault.
// The healthy job was parked, and Run returned before committing anything, so
// file 1 lost the ack it had genuinely earned: it had drained and fsynced
// successfully before the race touched file 0 at all.
//
// The "stat" arm is the phase-3 entry to the same race: the close lands
// between the fsync and the stat, which is a window buildExtent owns rather
// than Run.
func TestBarrier_ACloseAfterTheDrainDropsOnlyThatFile(t *testing.T) {
	for _, closeAt := range []string{"sync", "stat"} {
		t.Run("closed at "+closeAt, func(t *testing.T) {
			stall := &recordingStall{}
			ack := &recordingAcker{}
			db := openTestDB(t)
			b := NewBarrier(NewSQLiteFactLog(db), NewSQLiteExtentStore(db),
				ack, stall, slog.New(slog.DiscardHandler))

			tgt := &lateCloseTarget{closeAt: closeAt}
			err := b.Run(context.Background(), "job-1", tgt)

			// Grounding: the race must have happened, or every assertion below
			// is true for a reason unrelated to the defect.
			if !tgt.closed {
				t.Fatal("file 0 was never closed, so this run did not exercise the race")
			}
			if err != nil {
				t.Errorf("Run returned %v; a file closed underneath the barrier leaves "+
					"nothing to checkpoint for THAT file, and nothing at all to surface", err)
			}
			if len(stall.stalled) != 0 {
				t.Errorf("the job was stalled with %q — a deliberate close was classified as "+
					"a storage fault one phase after the one that recognises it", stall.stalled[0])
			}
			if len(stall.failed) != 0 {
				t.Errorf("the job was FAILED over a closed file: %v", stall.failed[0])
			}

			// The point of the pairing: the file that DID drain and fsync must
			// still be acked. Discarding the whole run was the expensive half.
			if len(ack.proofs) == 0 {
				t.Fatal("nothing was acked; the whole checkpoint was discarded because " +
					"one of its two files was closed underneath it")
			}
			var acked []int32
			for _, p := range ack.proofs {
				acked = append(acked, p.Articles()...)
			}
			if len(acked) != 1 || acked[0] != 1 {
				t.Errorf("acked = %v, want exactly article 1 — file 1 earned its ack and "+
					"file 0's bytes were never proven by a completed fsync of this run", acked)
			}

			// The dropped file must leave the run's file set, not merely lose
			// its drain report. Confirm is what RELEASES a writer's retained
			// report, and a file still in the set gets one — so its articles
			// are dropped from the re-report R12 makes the next drain whole
			// with, having been neither committed nor acked.
			if slices.Contains(tgt.confirmed, 0) {
				t.Errorf("confirmed = %v — the closed file's drain report was released "+
					"even though nothing was committed or acked for it, so those articles "+
					"are lost from the next drain's re-report", tgt.confirmed)
			}
			if !slices.Contains(tgt.confirmed, 1) {
				t.Errorf("confirmed = %v, want file 1 released: its cycle landed", tgt.confirmed)
			}
		})
	}
}

// finalizeCloseTarget answers one chosen operation with ErrFileNotOpen, as the
// worker does when the file was closed while a finalize was in flight.
type finalizeCloseTarget struct {
	closeOn   string // "sync", "stat", or "truncate"
	truncated bool
}

func (s *finalizeCloseTarget) Files() []int32    { return []int32{0} }
func (s *finalizeCloseTarget) Path(int32) string { return "/downloads/a.bin" }

func (s *finalizeCloseTarget) Drain(context.Context, int32) ([]WrittenArticle, error) {
	return []WrittenArticle{{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 10}}, nil
}

func (s *finalizeCloseTarget) Sync(context.Context, int32) error {
	if s.closeOn == "sync" {
		return fmt.Errorf("assembler: job x file 0 is not open: %w", ErrFileNotOpen)
	}
	return nil
}

func (s *finalizeCloseTarget) Stat(int32) (int64, int64, error) {
	if s.closeOn == "stat" {
		return 0, 0, fmt.Errorf("assembler: job x file 0 is not open: %w", ErrFileNotOpen)
	}
	return 100, 1, nil
}

func (s *finalizeCloseTarget) Truncate(context.Context, int32, int64) error {
	s.truncated = true
	if s.closeOn == "truncate" {
		return fmt.Errorf("assembler: job x file 0 is not open: %w", ErrFileNotOpen)
	}
	return nil
}

func (s *finalizeCloseTarget) ArticleCount(int32) int { return 1 }
func (s *finalizeCloseTarget) FileLocalOrdinal(_ int32, a int32) (int, bool) {
	return int(a), a == 0
}
func (s *finalizeCloseTarget) Confirm(context.Context, int32) {}

// TestFinalizeFile_HonoursTheCloseSentinelAtEveryStep pins the rule FinalizeFile
// applied to its Drain and to nothing else.
//
// A close landing one statement later was classified as a retryable storage
// fault, and the consequences compound rather than merely mislead. The job is
// paused naming a healthy disk; finalizeCompletedFile deliberately keeps the
// fd on a failing finalize, so it is retained for a file the assembler has
// already closed; reevaluateStall retries forever against a handle that cannot
// come back; and MarkFileComplete never runs, so the job is wedged across
// restarts.
//
// Each operation gets its own arm because each is a separate call site, and
// pinning one says nothing about the others — which is exactly how three of
// the four came to be missing.
func TestFinalizeFile_HonoursTheCloseSentinelAtEveryStep(t *testing.T) {
	for _, closeOn := range []string{"sync", "stat", "truncate"} {
		t.Run("closed on "+closeOn, func(t *testing.T) {
			stall := &recordingStall{}
			db := openTestDB(t)
			fl := NewSQLiteFactLog(db)
			if err := fl.Append(context.Background(), "job-1", []ArticleFact{
				{ArtIdx: 0, FileIdx: 0, Offset: 0, Length: 10, CRC32: 1},
			}); err != nil {
				t.Fatal(err)
			}
			b := NewBarrier(fl, NewSQLiteExtentStore(db),
				&recordingAcker{}, stall, slog.New(slog.DiscardHandler))

			tgt := &finalizeCloseTarget{closeOn: closeOn}
			err := b.FinalizeFile(context.Background(), "job-1", 0, tgt)

			if closeOn == "truncate" && !tgt.truncated {
				t.Fatal("the truncate was never attempted, so this arm proves nothing " +
					"about how its failure is treated")
			}
			if err != nil {
				t.Errorf("FinalizeFile returned %v; the file was closed deliberately and "+
					"was drained and synced on the way out, so there is nothing to surface", err)
			}
			if len(stall.stalled) != 0 {
				t.Errorf("the job was stalled with %q — a deliberate close was classified as "+
					"a storage fault, which parks a healthy job, retains an fd for a file "+
					"the assembler has closed, and leaves the retry running forever",
					stall.stalled[0])
			}
			if len(stall.failed) != 0 {
				t.Errorf("the job was FAILED over a closed file: %v", stall.failed[0])
			}
		})
	}
}

// TestBarrier_ARoutedFaultSaysSo pins the marker the application layer reads to
// tell a fault this package dispatched from one it merely surfaced.
//
// The caller used to infer it from the error's shape — "the chain contains a
// *storagefault.Fault" — which was sound only while routeFault was the one
// thing that let a fault escape. The SyncTarget boundary now mints its own,
// and one of those read as already-handled was silently swallowed.
func TestBarrier_ARoutedFaultSaysSo(t *testing.T) {
	stall := &recordingStall{}
	db := openTestDB(t)
	b := NewBarrier(NewSQLiteFactLog(db), NewSQLiteExtentStore(db),
		&recordingAcker{}, stall, slog.New(slog.DiscardHandler))

	tgt := &fakeTarget{written: map[int32][]WrittenArticle{0: {}}, syncErr: syscall.ENOSPC, artCount: 4}
	err := b.Run(context.Background(), "job-1", tgt)

	if len(stall.stalled) == 0 {
		t.Fatal("the fixture's fault was not routed, so this test cannot observe the marker")
	}
	if !errors.Is(err, ErrFaultRouted) {
		t.Errorf("err = %v, want it to carry ErrFaultRouted; without the marker the "+
			"caller cannot tell a dispatched fault from one nothing has surfaced", err)
	}
}
