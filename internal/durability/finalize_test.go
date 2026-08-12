package durability

import (
	"context"
	"log/slog"
	"slices"
	"syscall"
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

// TestFinalizeFile_ZeroArticleCountDoesNotEraseTheStoredExtent pins the
// erasure that a guard on Files() did not cover.
//
// Barrier.Run iterates Files(), so a target reporting none makes Run inert.
// FinalizeFile does not: it takes an explicit file index and goes straight to
// Drain and buildExtent. A target that cannot report an article count then
// reaches priorExtent with artCount == 0, which re-derives the STORED bitmap at
// zero width — and committing that replaces the real one. Every durable bit the
// job accumulated is gone, the next restart reads nothing as durable, and the
// whole job is re-downloaded. That is the ground L3 says a restart must not
// lose.
//
// The empty drain is load-bearing in the fixture. A drain carrying at least one
// article aborts earlier, on the FileLocalOrdinal lookup, so it never reaches
// the commit — which is why the hole needs a checkpoint over a file that took
// no writes this interval to be reachable. That is an ordinary event, not an
// exotic one.
func TestFinalizeFile_ZeroArticleCountDoesNotEraseTheStoredExtent(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	facts := NewSQLiteFactLog(db)
	exts := NewSQLiteExtentStore(db)

	// A previously committed extent with real durable bits.
	stored := NewBitmap(64)
	stored.Set(0)
	stored.Set(7)
	if err := exts.Commit(ctx, "job-1", []FileExtent{{
		FileIdx: 0, Durable: stored, BytesDurable: 200, Size: 4096,
	}}); err != nil {
		t.Fatal(err)
	}
	before, err := exts.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || before[0].Durable.Count() != 2 {
		t.Fatalf("fixture did not store two durable bits: %+v", before)
	}

	b := NewBarrier(facts, exts, &recordingAcker{}, &recordingStall{},
		slog.New(slog.DiscardHandler))

	// A target that cannot answer the manifest questions, with an EMPTY drain.
	tgt := &truncTarget{artCount: 0}

	if err := b.FinalizeFile(ctx, "job-1", 0, tgt); err == nil {
		t.Error("FinalizeFile accepted a target reporting zero articles; it must refuse " +
			"rather than commit an extent it cannot size")
	}

	after, err := exts.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("stored extent disappeared entirely: %+v", after)
	}
	if got := after[0].Durable.Count(); got != 2 {
		t.Errorf("stored durable bits = %d, want 2 — FinalizeFile committed a zero-width "+
			"bitmap over the stored one, so the next restart re-downloads the whole job", got)
	}
	if after[0].BytesDurable != 200 {
		t.Errorf("stored BytesDurable = %d, want 200", after[0].BytesDurable)
	}
}

// TestRun_ZeroArticleCountDoesNotEraseTheStoredExtent is the same claim for the
// other entry point, asserted at the same chokepoint rather than at Files().
// A SyncTarget that reports a file but cannot count its articles is the case;
// the assembler's adapter suppresses Files() for a nil ArticleMap, but the
// barrier must not depend on any particular target doing so.
func TestRun_ZeroArticleCountDoesNotEraseTheStoredExtent(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	exts := NewSQLiteExtentStore(db)

	stored := NewBitmap(64)
	stored.Set(3)
	if err := exts.Commit(ctx, "job-1", []FileExtent{{FileIdx: 0, Durable: stored}}); err != nil {
		t.Fatal(err)
	}

	b := NewBarrier(NewSQLiteFactLog(db), exts, &recordingAcker{}, &recordingStall{},
		slog.New(slog.DiscardHandler))

	if err := b.Run(ctx, "job-1", &truncTarget{artCount: 0}); err == nil {
		t.Error("Run accepted a target reporting zero articles for a file it listed")
	}

	after, err := exts.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Durable.Count() != 1 {
		t.Errorf("stored durable bits = %+v, want one bit preserved", after)
	}
}

// faultyTruncTarget injects a fault into one of FinalizeFile's storage calls.
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
// It also pins that a fault commits nothing: R7 requires a failed barrier to
// leave the previously committed cache wholly intact.
func TestFinalizeFile_StorageFaultsStallRatherThanFailArticles(t *testing.T) {
	drained := []WrittenArticle{{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100}}

	for _, tc := range []struct {
		name   string
		target func() *faultyTruncTarget
	}{
		{"drain", func() *faultyTruncTarget {
			return &faultyTruncTarget{truncTarget: truncTarget{artCount: 4}, drainErr: syscall.ENOSPC}
		}},
		{"sync", func() *faultyTruncTarget {
			return &faultyTruncTarget{truncTarget: truncTarget{artCount: 4, drained: drained}, syncErr: syscall.EIO}
		}},
		{"truncate", func() *faultyTruncTarget {
			return &faultyTruncTarget{truncTarget: truncTarget{artCount: 4, drained: drained}, truncErr: syscall.EIO}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := openTestDB(t)
			facts := NewSQLiteFactLog(db)
			if err := facts.Append(ctx, "job-1", []ArticleFact{
				{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
			}); err != nil {
				t.Fatal(err)
			}
			exts := NewSQLiteExtentStore(db)
			ack := &recordingAcker{}
			stall := &recordingStall{}
			b := NewBarrier(facts, exts, ack, stall, slog.New(slog.DiscardHandler))

			if err := b.FinalizeFile(ctx, "job-1", 0, tc.target()); err == nil {
				t.Fatal("FinalizeFile returned nil on a storage fault")
			}
			if len(stall.stalled)+len(stall.failed) == 0 {
				t.Error("the fault never reached Stallable, so the job has no surfaced reason " +
					"and the user sees an unexplained halt")
			}
			if len(ack.proofs) != 0 {
				t.Error("articles were acked despite a storage fault; a failed barrier must ack nothing (R7)")
			}
			got, err := exts.Load(ctx, "job-1")
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Errorf("a failed FinalizeFile committed %d extents; it must leave the prior cache intact (R7)", len(got))
			}
		})
	}
}
