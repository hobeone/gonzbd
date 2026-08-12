package durability

import (
	"context"
	"log/slog"
	"slices"
	"testing"
)

// truncTarget is a Truncator that records the bound it was asked for.
//
// Everything else is inert: the barrier's job here is to decide WHAT to
// truncate to, and a real file would let a wrong bound be masked by the
// writer's own refusal to grow.
type truncTarget struct {
	drained  []WrittenArticle
	artCount int
	bound    int64
	called   bool
}

func (s *truncTarget) Files() []int32 { return []int32{0} }
func (s *truncTarget) Drain(context.Context, int32) ([]WrittenArticle, error) {
	return s.drained, nil
}
func (s *truncTarget) Sync(context.Context, int32) error { return nil }
func (s *truncTarget) Stat(int32) (int64, int64, error)  { return 5000, 1, nil }
func (s *truncTarget) ArticleCount(int32) int            { return s.artCount }
func (s *truncTarget) FileLocalOrdinal(_ int32, a int32) (int, bool) {
	if int(a) >= s.artCount {
		return 0, false
	}
	return int(a), true
}
func (s *truncTarget) Truncate(_ context.Context, _ int32, bound int64) error {
	s.called, s.bound = true, bound
	return nil
}

// TestFinalizeFile_TruncatesToTheHighestDurableFactEnd is the pin for the
// completion truncate bound, and it is the test whose absence would let the
// #342/#350 family back in unnoticed. Task 3 deleted the only end-to-end pin on
// this bound in the same commit that broke it, and every gate stayed green.
//
// The fixture is built so that the three candidate bounds are three DIFFERENT
// numbers, because two of them are wrong in ways that a fixture where they
// coincide could never tell apart:
//
//	this run's high-water mark  = 400   (only article 3 was drained this run)
//	FileExtent.VerifiedTo       = 200   (the gapless prefix stops at the hole)
//	highest durable fact end    = 500   <- the correct answer
//
// Article 2 permanently failed, so it never decoded and never wrote a fact.
// That hole is the whole point: it stalls the gapless prefix at 200 while
// articles 3 and 4 sit above it with real bytes on disk. Truncating to
// VerifiedTo would discard them — on a 40 GB file with one failed article near
// the start that is almost the entire download, and it destroys precisely the
// blocks par2 would repair from. Truncating to this run's high-water mark
// discards article 4, which an earlier run wrote.
func TestFinalizeFile_TruncatesToTheHighestDurableFactEnd(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	facts := NewSQLiteFactLog(db)
	exts := NewSQLiteExtentStore(db)

	// Class A: articles 0, 1, 3, 4 decoded. Article 2 permanently failed, so
	// it has no fact — by design, Class A records what was decoded.
	if err := facts.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, HasCRC: true},
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, HasCRC: true},
		{FileIdx: 0, ArtIdx: 3, Offset: 300, Length: 100, HasCRC: true},
		{FileIdx: 0, ArtIdx: 4, Offset: 400, Length: 100, HasCRC: true},
	}); err != nil {
		t.Fatal(err)
	}

	// Earlier runs made 0, 1 and 4 durable. Note 4 is ABOVE the hole.
	prior := NewBitmap(5)
	prior.Set(0)
	prior.Set(1)
	prior.Set(4)
	if err := exts.Commit(ctx, "job-1", []FileExtent{{FileIdx: 0, Durable: prior}}); err != nil {
		t.Fatal(err)
	}

	// This run wrote only article 3, so its high-water mark is 400.
	tgt := &truncTarget{
		artCount: 5,
		drained:  []WrittenArticle{{FileIdx: 0, ArtIdx: 3, Offset: 300, Length: 100}},
	}
	ack := &recordingAcker{}
	b := NewBarrier(facts, exts, ack, &recordingStall{}, slog.New(slog.DiscardHandler))

	if err := b.FinalizeFile(ctx, "job-1", 0, tgt); err != nil {
		t.Fatalf("FinalizeFile: %v", err)
	}

	if !tgt.called {
		t.Fatal("FinalizeFile did not truncate at all; pre-allocation's trailing zeros survive and par2 reports a healthy file as damaged")
	}
	if tgt.bound != 500 {
		switch tgt.bound {
		case 200:
			t.Fatalf("truncated to %d — that is FileExtent.VerifiedTo, the GAPLESS PREFIX, "+
				"which stalls at the hole left by failed article 2. Articles 3 and 4 are on "+
				"disk above it and would be destroyed, including the blocks par2 repairs from", tgt.bound)
		case 400:
			t.Fatalf("truncated to %d — that is this run's high-water mark. Article 4 was "+
				"written by an earlier run and would be discarded (#342/#350)", tgt.bound)
		default:
			t.Fatalf("truncated to %d, want 500 (highest end offset among durable facts)", tgt.bound)
		}
	}

	// The committed extent must still carry the un-truncated quantities, so a
	// later resume is not misled about the gapless prefix.
	got, err := exts.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("Load returned %d extents, want 1", len(got))
	}
	if got[0].VerifiedTo != 200 {
		t.Errorf("VerifiedTo = %d, want 200 — the gapless prefix must still stop at the hole", got[0].VerifiedTo)
	}
	var acked []int32
	for _, p := range ack.proofs {
		acked = append(acked, p.Articles()...)
	}
	if !slicesContains(acked, 3) {
		t.Errorf("article 3 was not acked; AckDurable saw %v", acked)
	}
}

// TestFinalizeFile_NoDurableFactsDoesNotTruncate pins the empty case. A file
// with nothing durable has no extent to trim to, and truncating to zero would
// destroy whatever is on disk on the strength of an absent record — the
// over-claiming direction S1 forbids.
func TestFinalizeFile_NoDurableFactsDoesNotTruncate(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	facts := NewSQLiteFactLog(db)
	exts := NewSQLiteExtentStore(db)

	tgt := &truncTarget{artCount: 4}
	b := NewBarrier(facts, exts, &recordingAcker{}, &recordingStall{}, slog.New(slog.DiscardHandler))
	if err := b.FinalizeFile(ctx, "job-1", 0, tgt); err != nil {
		t.Fatalf("FinalizeFile: %v", err)
	}
	if tgt.called {
		t.Errorf("truncated to %d with no durable facts; an absent record is not evidence of an empty file", tgt.bound)
	}
}

func slicesContains(s []int32, v int32) bool {
	return slices.Contains(s, v)
}

// TestDurableExtent_DoesNotStopAtAHole is the direct pin on the one line that
// separates durableExtent from gaplessPrefix. They read the same facts and
// must answer differently: the prefix walk stops at the first gap because a
// CRC anchor cannot be proven past it, while this walk must NOT, because the
// bytes above the gap are real bytes on disk and truncating them away is the
// data loss the bound exists to prevent.
func TestDurableExtent_DoesNotStopAtAHole(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	facts := NewSQLiteFactLog(db)
	if err := facts.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
		{FileIdx: 0, ArtIdx: 3, Offset: 300, Length: 100},
	}); err != nil {
		t.Fatal(err)
	}
	b := NewBarrier(facts, NewSQLiteExtentStore(db), &recordingAcker{}, &recordingStall{},
		slog.New(slog.DiscardHandler))

	durable := NewBitmap(4)
	durable.Set(0)
	durable.Set(3)
	tgt := &truncTarget{artCount: 4}

	got, err := b.durableExtent(ctx, "job-1", 0, durable, tgt)
	if err != nil {
		t.Fatal(err)
	}
	if got != 400 {
		t.Errorf("durableExtent = %d, want 400 — a walk that stopped at the hole "+
			"below article 3 would report 100 and truncate its bytes away", got)
	}
}

// TestDurableExtent_IgnoresFactsThatAreNotDurable pins the other half: a fact
// exists for every article that ever DECODED, whether or not its bytes reached
// stable storage. Counting a non-durable fact would extend the file over a
// range no fsync covered, which is the over-claim direction S1 forbids.
func TestDurableExtent_IgnoresFactsThatAreNotDurable(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	facts := NewSQLiteFactLog(db)
	if err := facts.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 900},
	}); err != nil {
		t.Fatal(err)
	}
	b := NewBarrier(facts, NewSQLiteExtentStore(db), &recordingAcker{}, &recordingStall{},
		slog.New(slog.DiscardHandler))

	durable := NewBitmap(2)
	durable.Set(0) // article 1 decoded but never reached disk

	got, err := b.durableExtent(ctx, "job-1", 0, durable, &truncTarget{artCount: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got != 100 {
		t.Errorf("durableExtent = %d, want 100 — article 1 has a fact but no fsync "+
			"covered it, so extending the file to 1000 claims bytes nothing wrote", got)
	}
}
