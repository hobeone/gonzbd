package durability

import (
	"context"
	"errors"
	"log/slog"
	"syscall"
	"testing"
)

// factGapTarget is a Truncator whose file has a real size and whose Stat can
// be made to fail on a chosen call.
//
// The size is real rather than inert, because the pins over it are about a
// bound that sits BELOW the file: a stat that overstated the size would let a
// destructive bound look harmless.
type factGapTarget struct {
	confirmed []int32
	drained   []WrittenArticle
	size      int64

	bound  int64
	called bool

	statCalls   int
	failStatOn  int // 1-based call number to fail on; 0 never fails
	statFailErr error
}

func (s *factGapTarget) Files() []int32    { return []int32{0} }
func (s *factGapTarget) Path(int32) string { return "/downloads/movie/vol042.rar" }
func (s *factGapTarget) Drain(context.Context, int32) ([]WrittenArticle, error) {
	return s.drained, nil
}
func (s *factGapTarget) Sync(context.Context, int32) error { return nil }
func (s *factGapTarget) Stat(int32) (int64, error) {
	s.statCalls++
	if s.failStatOn != 0 && s.statCalls == s.failStatOn {
		return 0, s.statFailErr
	}
	return s.size, nil
}

func (s *factGapTarget) Truncate(_ context.Context, _ int32, bound int64) error {
	s.called, s.bound = true, bound
	return nil
}

// Confirm records the release so a test can tell a confirmed cycle from an
// abandoned one; the real writer drops its drain report here.
func (s *factGapTarget) Confirm(_ context.Context, idx int32) { s.confirmed = append(s.confirmed, idx) }

// TestFinalizeFile_PostTruncateStatFaultNamesTheFile pins the path carried on
// the fault raised by the stat that follows a truncate.
//
// Every other Classify call site in FinalizeFile passes t.Path(idx); this one
// passed "". R27 requires the surfaced stall reason to name the file or mount,
// and a reason reading `on stat ""` gives an operator nothing to act on.
//
// This file's other test went with the two-record design. It pinned a guard
// against a durable article the fact log did not name — a divergence that
// cannot be represented in a single record written after the fsync, so the
// guard and the class it defended against are both gone. This pin is not: the
// post-truncate stat survives, and it is the value both §3.3's overlap check
// and §3.4's resume gate are stated against.
func TestFinalizeFile_PostTruncateStatFaultNamesTheFile(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))

	// One article covering [0, 100) of a 200-byte file, so a truncate to 100
	// is both correct and reached — the stat under test only runs after one.
	tgt := &factGapTarget{
		size:        200,
		drained:     []WrittenArticle{{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100}},
		failStatOn:  1, // FinalizeFile stats exactly once, after the truncate
		statFailErr: syscall.EIO,
	}
	stall := &recordingStall{}
	b := NewBarrier(rs, &recordingAcker{}, stall, slog.New(slog.DiscardHandler))

	_, err := b.FinalizeFile(ctx, "job-1", 0, tgt)
	if err == nil {
		t.Fatal("FinalizeFile returned nil; the injected stat failure was not surfaced")
	}
	if !tgt.called {
		t.Fatal("no truncate happened, so the stat under test was never reached and this test proves nothing")
	}
	if len(stall.stalled) != 1 {
		t.Fatalf("Stall was called %d times, want 1", len(stall.stalled))
	}
	f := stall.stalled[0]
	if !errors.Is(f.Err, syscall.EIO) {
		t.Fatalf("stalled on %v, want the injected EIO", f.Err)
	}
	if f.Path != tgt.Path(0) {
		t.Fatalf("fault path = %q, want %q — an operator reading the stall reason "+
			"cannot tell which file or mount faulted", f.Path, tgt.Path(0))
	}
}
