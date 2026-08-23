package durability

import (
	"context"
	"log/slog"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// truncTarget records the bound FinalizeFile chose.
//
// Its Stat reports 5000 — a pre-allocated size far above anything the fixtures
// record — so a bound that came out wrong is never masked by the writer's own
// refusal to grow a file. The barrier's job here is to decide WHAT to truncate
// to, and that is all this fixture observes.
type truncTarget struct {
	confirmed []int32
	drained   []WrittenArticle
	bound     int64
	called    bool
}

func (s *truncTarget) Files() []int32    { return []int32{0} }
func (s *truncTarget) Path(int32) string { return "/tmp/trunc-target.bin" }
func (s *truncTarget) Drain(context.Context, int32) ([]WrittenArticle, error) {
	return s.drained, nil
}
func (s *truncTarget) Sync(context.Context, int32) error { return nil }
func (s *truncTarget) Stat(int32) (int64, error)         { return 5000, nil }
func (s *truncTarget) Truncate(_ context.Context, _ int32, bound int64) error {
	s.called, s.bound = true, bound
	return nil
}
func (s *truncTarget) Confirm(_ context.Context, idx int32) { s.confirmed = append(s.confirmed, idx) }

// TestFinalizeFile_TruncatesToTheHighestRecordedEnd is the pin for the
// completion truncate bound, and it is the test whose absence would let the
// #342/#350 family back in unnoticed.
//
// The bound is max(Offset+Length) over everything the record will hold once
// this call commits: the file's STORED runs, plus the articles this drain just
// made durable. The fixture is built so the two plausible wrong answers are
// three different numbers:
//
//	this run's high-water mark   = 400   (only article 3 was drained this run)
//	the gapless prefix from 0    = 200   (it stops at the permanent hole)
//	the highest recorded end     = 500   <- the correct answer
//
// Article 2 permanently failed, so nothing was ever written for it. That hole
// is the whole point: a bound that stops at it cuts the file to 200 while
// articles 3 and 4 sit above with real bytes on disk — on a 40 GB file with
// one failed article near the start that is almost the entire download, and it
// destroys precisely the blocks par2 would repair from. A bound taken from
// this run's high-water mark instead discards article 4, which an earlier run
// wrote and which is on disk in the STORED runs rather than in this drain.
//
// A run record spans a hole with no special case: the hole is simply a gap
// between two rows, and the maximum is taken over both.
func TestFinalizeFile_TruncatesToTheHighestRecordedEnd(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))

	// What earlier runs recorded: articles 0 and 1 tile [0,200), and article 4
	// sits above the hole at [400,500).
	if err := rs.Commit(ctx, "job-1", []DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x11},
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: 0x22},
		{FileIdx: 0, ArtIdx: 4, Offset: 400, Length: 100, CRC32: 0x44},
	}); err != nil {
		t.Fatal(err)
	}

	// This run drained only article 3, whose end is 400.
	tgt := &truncTarget{
		drained: []WrittenArticle{{FileIdx: 0, ArtIdx: 3, Offset: 300, Length: 100, CRC32: 0x33}},
	}
	b := NewBarrier(rs, &recordingAcker{}, &recordingStall{}, slog.New(slog.DiscardHandler))
	if _, err := b.FinalizeFile(ctx, "job-1", 0, tgt); err != nil {
		t.Fatalf("FinalizeFile: %v", err)
	}

	if !tgt.called {
		t.Fatal("no truncate happened; a completed file keeps pre-allocation's trailing " +
			"zeros, and par2's QuickCheck gates on an exact size match so the file is " +
			"never relocated")
	}
	if tgt.bound != 500 {
		t.Errorf("truncated to %d, want 500 — the highest end offset over the stored "+
			"runs and this drain. 400 is this run's high-water mark and discards "+
			"article 4; 200 stops at the permanent hole and discards both", tgt.bound)
	}
}

// TestFinalizeFile_NothingRecordedDoesNotTruncate pins the bound == 0 branch.
//
// A file with no stored run and nothing drained has no recorded end, and
// truncating to 0 would destroy whatever is on disk. S6 only ever SHRINKS, so
// declining to shrink is always the safe direction; leaving pre-allocation's
// zeros is a visible, repairable cost.
func TestFinalizeFile_NothingRecordedDoesNotTruncate(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))
	tgt := &truncTarget{}

	b := NewBarrier(rs, &recordingAcker{}, &recordingStall{}, slog.New(slog.DiscardHandler))
	if _, err := b.FinalizeFile(ctx, "job-1", 0, tgt); err != nil {
		t.Fatalf("FinalizeFile: %v", err)
	}
	if tgt.called {
		t.Errorf("truncated to %d with nothing recorded; a bound of 0 cuts the file "+
			"to nothing", tgt.bound)
	}
}

// faultyTruncTarget injects a failure into one of FinalizeFile's storage calls.
type faultyTruncTarget struct {
	truncTarget
	drainErr, syncErr, truncErr error
	syncCalls                   int
}

func (s *faultyTruncTarget) Drain(context.Context, int32) ([]WrittenArticle, error) {
	if s.drainErr != nil {
		return nil, s.drainErr
	}
	return s.drained, nil
}

func (s *faultyTruncTarget) Sync(context.Context, int32) error {
	s.syncCalls++
	return s.syncErr
}

func (s *faultyTruncTarget) Truncate(ctx context.Context, idx int32, bound int64) error {
	if s.truncErr != nil {
		return s.truncErr
	}
	return s.truncTarget.Truncate(ctx, idx, bound)
}

// TestFinalizeFile_StorageFaultsStallRatherThanFailArticles pins A1 across
// FinalizeFile's three storage calls. A full disk or a wedged mount resolves
// against STORAGE: the job stalls with a surfaced reason and its articles stay
// Outstanding. Recording any of these against the articles instead would burn
// their retry budget, inflate the job's failed-byte count and degrade its
// reported health (R21) — from a condition the user often fixes in seconds.
//
// It also pins that a fault records nothing: R7 requires a failed barrier to
// leave the stored runs wholly intact.
func TestFinalizeFile_StorageFaultsStallRatherThanFailArticles(t *testing.T) {
	drained := []WrittenArticle{{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x11}}

	for _, tc := range []struct {
		name   string
		target func() *faultyTruncTarget
	}{
		{"drain", func() *faultyTruncTarget {
			return &faultyTruncTarget{drainErr: syscall.ENOSPC}
		}},
		{"sync", func() *faultyTruncTarget {
			return &faultyTruncTarget{truncTarget: truncTarget{drained: drained}, syncErr: syscall.EIO}
		}},
		{"truncate", func() *faultyTruncTarget {
			return &faultyTruncTarget{truncTarget: truncTarget{drained: drained}, truncErr: syscall.EIO}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rs := NewSQLiteRunStore(openTestDB(t))
			ack := &recordingAcker{}
			stall := &recordingStall{}
			b := NewBarrier(rs, ack, stall, slog.New(slog.DiscardHandler))

			if _, err := b.FinalizeFile(ctx, "job-1", 0, tc.target()); err == nil {
				t.Fatal("FinalizeFile returned nil on a storage fault")
			}
			routed := append(append([]*storagefault.Fault{}, stall.stalled...), stall.failed...)
			if len(routed) == 0 {
				t.Fatal("the fault never reached Stallable, so the job has no surfaced reason " +
					"and the user sees an unexplained halt")
			}
			// R27: the reason has to name the file, or the user is told a
			// disk failed without being told which one.
			if got := routed[0].Path; got != "/tmp/trunc-target.bin" {
				t.Errorf("routed fault Path = %q, want the target's path", got)
			}
			if len(ack.proofs) != 0 {
				t.Error("articles were acked despite a storage fault; a failed barrier must ack nothing (R7)")
			}
			got, err := rs.ForJob(ctx, "job-1")
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Errorf("a failed FinalizeFile recorded %d runs; it must leave the record intact (R7)", len(got))
			}
		})
	}
}
