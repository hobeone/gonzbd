package durability

import (
	"context"
	"errors"
	"log/slog"
	"syscall"
	"testing"
)

// factGapTarget is a Truncator whose file is 200 bytes and whose Stat can be
// made to fail on a chosen call.
//
// The size is real rather than the inert 5000 truncTarget uses, because these
// pins are about a bound that sits BELOW the file: a stat that overstated the
// size would let a destructive bound look harmless.
type factGapTarget struct {
	confirmed []int32
	drained   []WrittenArticle
	artCount  int
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
func (s *factGapTarget) Stat(int32) (int64, int64, error) {
	s.statCalls++
	if s.failStatOn != 0 && s.statCalls == s.failStatOn {
		return 0, 0, s.statFailErr
	}
	return s.size, 1, nil
}
func (s *factGapTarget) ArticleCount(int32) int { return s.artCount }
func (s *factGapTarget) FileLocalOrdinal(_ int32, a int32) (int, bool) {
	if int(a) >= s.artCount {
		return 0, false
	}
	return int(a), true
}
func (s *factGapTarget) Truncate(_ context.Context, _ int32, bound int64) error {
	s.called, s.bound = true, bound
	return nil
}

// TestFinalizeFile_DoesNotTrimBelowADurableArticleWithNoFact pins the
// direction of the recovery guard that was missing.
//
// The guard at FinalizeFile compares the durable set against the fact log in
// ONE direction — a fact whose article is not durable — because that is the
// shape a retried finalize produces (#342/#350). The reverse is reachable too
// and is silent data loss: appendArticleFacts logs and swallows its error
// (R2 makes Class A independent of the write), so an article can reach disk,
// be drained, be fsynced, and earn a truthful durable bit while having NO
// Class A fact. Both bounds are then derived by walking facts, so neither sees
// it, and if it holds the file's top offset the truncate cuts its bytes away.
//
// The fixture makes the article that lacks a fact the HIGHEST one, because
// that is the only arrangement where the missing fact changes the bound. With
// the gap anywhere lower, the top article's own fact still produces the right
// answer and the defect is invisible — a fixture that could not tell the two
// apart would pass against the unfixed code.
//
//	article 0: fact [0, 100)   durable
//	article 1: NO FACT         durable, drained this run, occupies [100, 200)
//	file on disk: 200 bytes
//
// Unfixed, both walks return 100 and the file is cut to 100, destroying
// article 1 — which is simultaneously acked Done, so it is never re-fetched.
func TestFinalizeFile_DoesNotTrimBelowADurableArticleWithNoFact(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	facts := NewSQLiteFactLog(db)
	exts := NewSQLiteExtentStore(db)

	// Class A records article 0 only. Article 1's append failed.
	if err := facts.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
	}); err != nil {
		t.Fatal(err)
	}

	tgt := &factGapTarget{
		artCount: 2,
		size:     200,
		drained: []WrittenArticle{
			{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
			{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100},
		},
	}
	ack := &recordingAcker{}
	b := NewBarrier(facts, exts, ack, &recordingStall{}, slog.New(slog.DiscardHandler))

	if _, err := b.FinalizeFile(ctx, "job-1", 0, tgt); err != nil {
		t.Fatalf("FinalizeFile: %v", err)
	}

	// Grounding: the fixture must actually have reached the state under test.
	// If article 1 never earned a durable bit, the bound below is correct for
	// a reason that has nothing to do with this defect and the pin is inert.
	got, err := exts.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("Load returned %d extents, want 1", len(got))
	}
	if !got[0].Durable.Get(1) {
		t.Fatal("fixture never made article 1 durable, so this test cannot observe the defect it names")
	}

	// The real assertion: the file must not be cut below article 1's bytes.
	if tgt.called && tgt.bound < 200 {
		t.Fatalf("truncated to %d, cutting away article 1's bytes at [100, 200). "+
			"Article 1 has no Class A fact because its append failed, so both the durable "+
			"and the recorded bound walk right past it. It is acked Done in the same "+
			"FinalizeFile call, so the bytes are never re-fetched: a silently short file",
			tgt.bound)
	}
}

// TestFinalizeFile_PostTruncateStatFaultNamesTheFile pins the path carried on
// the fault raised by the re-stat that follows a truncate.
//
// Every other Classify call site in FinalizeFile passes t.Path(idx); this one
// passed "". R27 requires the surfaced stall reason to name the file or mount,
// and a reason reading `on stat ""` gives an operator nothing to act on.
func TestFinalizeFile_PostTruncateStatFaultNamesTheFile(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	facts := NewSQLiteFactLog(db)
	exts := NewSQLiteExtentStore(db)

	// One article covering [0, 100) of a 200-byte file, so a truncate to 100
	// is both correct and reached — the re-stat only runs after a truncate.
	if err := facts.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
	}); err != nil {
		t.Fatal(err)
	}

	tgt := &factGapTarget{
		artCount:    1,
		size:        200,
		drained:     []WrittenArticle{{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100}},
		failStatOn:  2, // 1 is buildExtent's; 2 is the post-truncate re-stat
		statFailErr: syscall.EIO,
	}
	stall := &recordingStall{}
	b := NewBarrier(facts, exts, &recordingAcker{}, stall, slog.New(slog.DiscardHandler))

	_, err := b.FinalizeFile(ctx, "job-1", 0, tgt)
	if err == nil {
		t.Fatal("FinalizeFile returned nil; the injected stat failure was not surfaced")
	}
	if !tgt.called {
		t.Fatal("no truncate happened, so the post-truncate re-stat was never reached and this test proves nothing")
	}
	if tgt.statCalls < 2 {
		t.Fatalf("Stat was called %d times; the post-truncate re-stat was not reached", tgt.statCalls)
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

// Confirm records the release so a test can tell a confirmed cycle from an
// abandoned one; the real writer drops its drain report here.
func (s *factGapTarget) Confirm(_ context.Context, idx int32) { s.confirmed = append(s.confirmed, idx) }
