package durability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"syscall"

	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.DiscardHandler)
}

// fakeTarget records the order of Drain/Sync/Stat calls so the ordering
// invariant S1 can be asserted rather than assumed.
type fakeTarget struct {
	confirmed []int32
	calls     []string
	files     []int32
	written   map[int32][]WrittenArticle
	drainErr  error
	syncErr   error
	statErr   error
	size      int64
	modTime   int64
	artCount  int
	noOrd     bool
}

func (f *fakeTarget) Files() []int32 {
	if f.files != nil {
		return f.files
	}
	return []int32{0}
}

// Path is the R27 stall-reason accessor. The fixture returns a fixed name so
// the fault-routing tests can assert the fault carries it rather than "".
func (f *fakeTarget) Path(fileIdx int32) string {
	return fmt.Sprintf("/downloads/job-1/file%d.bin", fileIdx)
}

func (f *fakeTarget) Drain(_ context.Context, fileIdx int32) ([]WrittenArticle, error) {
	f.calls = append(f.calls, "drain")
	if f.drainErr != nil {
		return nil, f.drainErr
	}
	return f.written[fileIdx], nil
}

func (f *fakeTarget) Sync(_ context.Context, _ int32) error {
	f.calls = append(f.calls, "sync")
	return f.syncErr
}

func (f *fakeTarget) Stat(_ int32) (int64, int64, error) {
	f.calls = append(f.calls, "stat")
	if f.statErr != nil {
		return 0, 0, f.statErr
	}
	return f.size, f.modTime, nil
}

func (f *fakeTarget) ArticleCount(_ int32) int { return f.artCount }

func (f *fakeTarget) FileLocalOrdinal(_ int32, artIdx int32) (int, bool) {
	if f.noOrd {
		return 0, false
	}
	return int(artIdx), true
}

var _ SyncTarget = (*fakeTarget)(nil)

type recordingAcker struct {
	proofs []DurableProof
	err    error
}

func (r *recordingAcker) AckDurable(p DurableProof) error {
	r.proofs = append(r.proofs, p)
	return r.err
}

type recordingStall struct {
	stalled []*storagefault.Fault
	failed  []*storagefault.Fault
}

func (r *recordingStall) Stall(_ string, f *storagefault.Fault) { r.stalled = append(r.stalled, f) }
func (r *recordingStall) Fail(_ string, f *storagefault.Fault)  { r.failed = append(r.failed, f) }

// newBarrierWithStores is the common wiring for the tests that need no
// error injection in the stores themselves.
func newBarrierWithStores(t *testing.T, es ExtentStore, ack Acker, stall Stallable) (*Barrier, *SQLiteFactLog) {
	t.Helper()
	fl := NewSQLiteFactLog(openTestDB(t))
	return NewBarrier(fl, es, ack, stall, testLogger(t)), fl
}

// TestBarrier_SyncPrecedesCommitAndAck is the pin for S1 and R9. If the
// commit or the ack happens before the fsync, this fails.
func TestBarrier_SyncPrecedesCommitAndAck(t *testing.T) {
	ctx := context.Background()
	tgt := &fakeTarget{
		written:  map[int32][]WrittenArticle{0: {{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100}}},
		size:     100,
		modTime:  42,
		artCount: 4,
	}
	ack := &recordingAcker{}
	b, _ := newBarrierWithStores(t, NewSQLiteExtentStore(openTestDB(t)), ack, &recordingStall{})

	if err := b.Run(ctx, "job-1", tgt); err != nil {
		t.Fatal(err)
	}
	want := []string{"drain", "sync", "stat"}
	if len(tgt.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", tgt.calls, want)
	}
	for i := range want {
		if tgt.calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v — S1 requires sync before any claim", tgt.calls, want)
		}
	}
	if len(ack.proofs) != 1 || len(ack.proofs[0].Articles()) != 1 {
		t.Fatalf("expected exactly one article acked, got %v", ack.proofs)
	}
	if ack.proofs[0].JobID() != "job-1" {
		t.Errorf("proof job = %q, want job-1", ack.proofs[0].JobID())
	}
}

// TestBarrier_SyncFailureAcksNothing pins R7: a failed barrier acks nothing
// and leaves the prior committed cache intact.
func TestBarrier_SyncFailureAcksNothing(t *testing.T) {
	ctx := context.Background()
	es := NewSQLiteExtentStore(openTestDB(t))
	prior := NewBitmap(4)
	prior.Set(0)
	if err := es.Commit(ctx, "job-1", []FileExtent{{FileIdx: 0, Durable: prior, VerifiedTo: 50}}); err != nil {
		t.Fatal(err)
	}

	tgt := &fakeTarget{
		written:  map[int32][]WrittenArticle{0: {{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100}}},
		syncErr:  syscall.ENOSPC,
		artCount: 4,
	}
	ack := &recordingAcker{}
	stall := &recordingStall{}
	b, _ := newBarrierWithStores(t, es, ack, stall)

	if err := b.Run(ctx, "job-1", tgt); err == nil {
		t.Fatal("Run returned nil after a failed sync")
	}
	if len(ack.proofs) != 0 {
		t.Errorf("acked %d proofs after a failed sync, want 0", len(ack.proofs))
	}
	got, err := es.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("loaded %d extents, want 1", len(got))
	}
	if got[0].VerifiedTo != 50 || got[0].Durable.Count() != 1 {
		t.Errorf("prior cache was disturbed by a failed barrier: %+v", got[0])
	}
	if len(stall.stalled) != 1 {
		t.Fatalf("stalled %d times, want 1 — ENOSPC is retryable", len(stall.stalled))
	}
	if got := stall.stalled[0].Path; got != "/downloads/job-1/file0.bin" {
		t.Errorf("stall fault Path = %q, want the target's path (R27)", got)
	}
	if len(stall.failed) != 0 {
		t.Errorf("failed the job on a retryable fault")
	}
}

// TestBarrier_PermanentFaultFailsRatherThanStalls pins R20.
func TestBarrier_PermanentFaultFailsRatherThanStalls(t *testing.T) {
	ctx := context.Background()
	tgt := &fakeTarget{written: map[int32][]WrittenArticle{0: {}}, syncErr: syscall.EROFS, artCount: 4}
	stall := &recordingStall{}
	b, _ := newBarrierWithStores(t, NewSQLiteExtentStore(openTestDB(t)), &recordingAcker{}, stall)

	if err := b.Run(ctx, "job-1", tgt); err == nil {
		t.Fatal("Run returned nil after EROFS")
	}
	if len(stall.failed) != 1 {
		t.Errorf("failed %d times, want 1 — EROFS is permanent", len(stall.failed))
	}
	if len(stall.stalled) != 0 {
		t.Errorf("stalled on a permanent fault")
	}
}

// TestBarrier_DrainAndStatFaultsRouteThroughA1 pins that the two phases
// either side of the fsync classify their own failures rather than
// returning a bare error, and that neither marks an article failed (A1).
func TestBarrier_DrainAndStatFaultsRouteThroughA1(t *testing.T) {
	tests := []struct {
		name    string
		tgt     *fakeTarget
		wantCal []string
	}{
		{
			name:    "drain fault stops before any sync",
			tgt:     &fakeTarget{drainErr: syscall.ENOSPC, artCount: 4},
			wantCal: []string{"drain"},
		},
		{
			name:    "stat fault stops after the sync",
			tgt:     &fakeTarget{written: map[int32][]WrittenArticle{0: {}}, statErr: syscall.ENOSPC, artCount: 4},
			wantCal: []string{"drain", "sync", "stat"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			stall := &recordingStall{}
			ack := &recordingAcker{}
			b, _ := newBarrierWithStores(t, NewSQLiteExtentStore(openTestDB(t)), ack, stall)

			err := b.Run(ctx, "job-1", tt.tgt)
			if err == nil {
				t.Fatal("Run returned nil after a storage fault")
			}
			if !errors.Is(err, syscall.ENOSPC) {
				t.Errorf("err = %v, want it to wrap ENOSPC", err)
			}
			if len(stall.stalled) != 1 {
				t.Fatalf("stalled %d times, want 1", len(stall.stalled))
			}
			// R27: a stall reason has to be actionable, and "ENOSPC on
			// sync" without a path does not say which volume filled up.
			// A job's files can live on different mounts, so the job name
			// does not identify the device either.
			if got := stall.stalled[0].Path; got != "/downloads/job-1/file0.bin" {
				t.Errorf("stall fault Path = %q, want the target's path — "+
					"the user is told a disk is full without being told which one", got)
			}
			if len(ack.proofs) != 0 {
				t.Errorf("acked after a storage fault")
			}
			if len(tt.tgt.calls) != len(tt.wantCal) {
				t.Fatalf("calls = %v, want %v", tt.tgt.calls, tt.wantCal)
			}
			for i := range tt.wantCal {
				if tt.tgt.calls[i] != tt.wantCal[i] {
					t.Fatalf("calls = %v, want %v", tt.tgt.calls, tt.wantCal)
				}
			}
		})
	}
}

// TestBarrier_VerifiedToAdvancesOnlyOverAGaplessPrefix pins that a hole
// stops the CRC anchor without stopping resume. This is the distinction
// that #311 and #353 collided over: the durable bitmap must record the
// article above the hole, while VerifiedTo must not advance past it.
//
// The two subtests exist because a hole has two shapes and one fixture
// cannot pin both gates. When the missing article HAS a Class A fact, the
// walk stops because that article is not durable; when it does not — it was
// never decoded, so nothing recorded its byte range — the walk stops only
// because the next fact does not abut the run so far. A fixture with only
// the first shape passes with the contiguity check deleted, which was
// observed, not reasoned.
func TestBarrier_VerifiedToAdvancesOnlyOverAGaplessPrefix(t *testing.T) {
	tests := []struct {
		name  string
		facts []ArticleFact
	}{
		{
			name: "the hole has a fact, so the durable gate stops the walk",
			facts: []ArticleFact{
				{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
				{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100},
				{FileIdx: 0, ArtIdx: 2, Offset: 200, Length: 100},
			},
		},
		{
			// Article 1 was never decoded, so Class A has no record of its
			// byte range. Only contiguity can stop the walk here: every
			// fact present is durable.
			name: "the hole has no fact, so only contiguity stops the walk",
			facts: []ArticleFact{
				{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
				{FileIdx: 0, ArtIdx: 2, Offset: 200, Length: 100},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			es := NewSQLiteExtentStore(openTestDB(t))
			tgt := &fakeTarget{
				written: map[int32][]WrittenArticle{0: {
					{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
					// article 1 at offset 100 is missing — the hole
					{FileIdx: 0, ArtIdx: 2, Offset: 200, Length: 100},
				}},
				size: 300, artCount: 4,
			}
			b, fl := newBarrierWithStores(t, es, &recordingAcker{}, &recordingStall{})
			if err := fl.Append(ctx, "job-1", tt.facts); err != nil {
				t.Fatal(err)
			}

			if err := b.Run(ctx, "job-1", tgt); err != nil {
				t.Fatal(err)
			}
			got, err := es.Load(ctx, "job-1")
			if err != nil {
				t.Fatal(err)
			}
			if got[0].VerifiedTo != 100 {
				t.Errorf("VerifiedTo = %d, want 100 — it must stop at the hole", got[0].VerifiedTo)
			}
			if !got[0].Durable.Get(2) {
				t.Error("article 2 is not durable, but its bytes were written and fsynced — resume must not be held back by the hole")
			}
			if got[0].BytesDurable != 200 {
				t.Errorf("BytesDurable = %d, want 200", got[0].BytesDurable)
			}
		})
	}
}

// TestBarrier_ReplayedDrainDoesNotInflateBytesDurable pins R12: at-least-once
// delivery is the contract, so a drain that re-reports an article a previous
// barrier already recorded must be absorbed by the apply. Set is idempotent;
// the byte accumulator is not, so the 0->1 transition guard is what makes the
// second barrier a no-op for the R26 "bytes durable" figure.
func TestBarrier_ReplayedDrainDoesNotInflateBytesDurable(t *testing.T) {
	ctx := context.Background()
	es := NewSQLiteExtentStore(openTestDB(t))
	tgt := &fakeTarget{
		written:  map[int32][]WrittenArticle{0: {{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100}}},
		size:     100,
		artCount: 4,
	}
	b, _ := newBarrierWithStores(t, es, &recordingAcker{}, &recordingStall{})

	for i := range 3 {
		if err := b.Run(ctx, "job-1", tgt); err != nil {
			t.Fatalf("barrier %d: %v", i, err)
		}
	}
	got, err := es.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].BytesDurable != 100 {
		t.Errorf("BytesDurable = %d after three replays of one article, want 100", got[0].BytesDurable)
	}
	if got[0].Durable.Count() != 1 {
		t.Errorf("Durable.Count() = %d, want 1", got[0].Durable.Count())
	}
}

// TestBarrier_PriorPaddingBitsDoNotInflateTheDurableCount pins the fix for
// the hazard carried from task 3: ExtentStore.Load rebuilds each bitmap at
// its full BYTE width, and Bitmap's tail-word mask cannot fire when the bit
// count is a multiple of 64, so padding bits in a damaged blob survive and
// over-report Count(). priorExtent has the real article count and Load does
// not, so re-deriving here is the only place that can mask them off.
func TestBarrier_PriorPaddingBitsDoNotInflateTheDurableCount(t *testing.T) {
	ctx := context.Background()
	es := NewSQLiteExtentStore(openTestDB(t))
	// A blob whose bits above the real article count are set — the shape a
	// damaged or externally-written record has.
	damaged := NewBitmap(64)
	damaged.Set(0)
	damaged.Set(40)
	if err := es.Commit(ctx, "job-1", []FileExtent{{FileIdx: 0, Durable: damaged}}); err != nil {
		t.Fatal(err)
	}

	tgt := &fakeTarget{written: map[int32][]WrittenArticle{0: {}}, artCount: 4}
	b, _ := newBarrierWithStores(t, es, &recordingAcker{}, &recordingStall{})
	if err := b.Run(ctx, "job-1", tgt); err != nil {
		t.Fatal(err)
	}
	got, err := es.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Durable.Count() != 1 {
		t.Errorf("Durable.Count() = %d, want 1 — bit 40 is above the 4 real articles and must not be claimed", got[0].Durable.Count())
	}
}

// TestBarrier_PriorBitmapWidensToTheArticleCount pins that a stored bitmap
// narrower than the file's article count is widened rather than rejected:
// the extra articles are simply not durable yet.
func TestBarrier_PriorBitmapWidensToTheArticleCount(t *testing.T) {
	ctx := context.Background()
	es := NewSQLiteExtentStore(openTestDB(t))
	narrow := NewBitmap(4)
	narrow.Set(1)
	if err := es.Commit(ctx, "job-1", []FileExtent{{FileIdx: 0, Durable: narrow}}); err != nil {
		t.Fatal(err)
	}

	tgt := &fakeTarget{
		written:  map[int32][]WrittenArticle{0: {{FileIdx: 0, ArtIdx: 100, Offset: 0, Length: 10}}},
		artCount: 130,
	}
	b, _ := newBarrierWithStores(t, es, &recordingAcker{}, &recordingStall{})
	if err := b.Run(ctx, "job-1", tgt); err != nil {
		t.Fatal(err)
	}
	got, err := es.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Durable.Get(1) {
		t.Error("the prior bit was lost by widening")
	}
	if !got[0].Durable.Get(100) {
		t.Error("article 100 is not recorded, so the bitmap was not widened to 130 bits")
	}
}

// TestBarrier_UnplaceableArticleIsAnError pins A2/R28: an article the target
// cannot place in its file is a bookkeeping defect, and must fail loudly
// rather than be dropped from the barrier's accounting.
func TestBarrier_UnplaceableArticleIsAnError(t *testing.T) {
	ctx := context.Background()
	es := NewSQLiteExtentStore(openTestDB(t))
	tgt := &fakeTarget{
		written:  map[int32][]WrittenArticle{0: {{FileIdx: 0, ArtIdx: 7, Offset: 0, Length: 10}}},
		artCount: 4,
		noOrd:    true,
	}
	ack := &recordingAcker{}
	stall := &recordingStall{}
	b, _ := newBarrierWithStores(t, es, ack, stall)

	err := b.Run(ctx, "job-1", tgt)
	if err == nil {
		t.Fatal("Run returned nil for an article with no file-local ordinal")
	}
	if len(ack.proofs) != 0 {
		t.Errorf("acked despite an unplaceable article")
	}
	if len(stall.stalled) != 0 || len(stall.failed) != 0 {
		t.Errorf("a bookkeeping defect was routed as a storage fault: stalled=%d failed=%d",
			len(stall.stalled), len(stall.failed))
	}
	if got, _ := es.Load(ctx, "job-1"); len(got) != 0 {
		t.Errorf("committed %d extents despite an unplaceable article", len(got))
	}
}

// errStore fails Commit so the ack-after-commit ordering can be pinned from
// the commit side as well as the sync side.
type errStore struct {
	ExtentStore
	err error
}

func (e *errStore) Commit(context.Context, string, []FileExtent) error { return e.err }

// TestBarrier_CommitFailureAcksNothing pins the second half of phase 4: the
// ack is downstream of the commit, so a commit that fails must ack nothing.
func TestBarrier_CommitFailureAcksNothing(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("commit exploded")
	es := &errStore{ExtentStore: NewSQLiteExtentStore(openTestDB(t)), err: boom}
	tgt := &fakeTarget{
		written:  map[int32][]WrittenArticle{0: {{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100}}},
		artCount: 4,
	}
	ack := &recordingAcker{}
	b, _ := newBarrierWithStores(t, es, ack, &recordingStall{})

	err := b.Run(ctx, "job-1", tgt)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the commit failure", err)
	}
	if len(ack.proofs) != 0 {
		t.Errorf("acked %d proofs after a failed commit, want 0", len(ack.proofs))
	}
}

// TestBarrier_NothingDrainedAcksNothing pins that an idle barrier still
// commits the (unchanged) cache but emits no proof — an empty proof would
// be an ack of nothing and pointless work for the queue.
func TestBarrier_NothingDrainedAcksNothing(t *testing.T) {
	ctx := context.Background()
	es := NewSQLiteExtentStore(openTestDB(t))
	tgt := &fakeTarget{written: map[int32][]WrittenArticle{0: {}}, size: 7, modTime: 9, artCount: 4}
	ack := &recordingAcker{}
	b, _ := newBarrierWithStores(t, es, ack, &recordingStall{})

	if err := b.Run(ctx, "job-1", tgt); err != nil {
		t.Fatal(err)
	}
	if len(ack.proofs) != 0 {
		t.Errorf("acked %d proofs with nothing drained, want 0", len(ack.proofs))
	}
	got, err := es.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Size != 7 || got[0].ModTimeNs != 9 {
		t.Errorf("stat stamp not committed: %+v", got)
	}
}

// TestBarrier_AckFailurePropagates pins that a rejected ack is returned
// rather than swallowed (A2).
func TestBarrier_AckFailurePropagates(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("queue rejected the proof")
	ack := &recordingAcker{err: boom}
	tgt := &fakeTarget{
		written:  map[int32][]WrittenArticle{0: {{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100}}},
		artCount: 4,
	}
	b, _ := newBarrierWithStores(t, NewSQLiteExtentStore(openTestDB(t)), ack, &recordingStall{})

	if err := b.Run(ctx, "job-1", tgt); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the ack failure", err)
	}
}

// errFactLog fails ForFile so gaplessPrefix's error path can be pinned:
// A2 forbids computing a CRC anchor from a fact log that could not be read.
type errFactLog struct {
	FactLog
	err error
}

func (e *errFactLog) ForFile(context.Context, string, int32) ([]ArticleFact, error) {
	return nil, e.err
}

// TestBarrier_FactLogReadFailureAbortsTheBarrier pins that an unreadable
// fact log stops the barrier rather than yielding VerifiedTo = 0, which
// would be a silently wrong CRC anchor committed as if it were derived.
func TestBarrier_FactLogReadFailureAbortsTheBarrier(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("fact log unreadable")
	es := NewSQLiteExtentStore(openTestDB(t))
	fl := &errFactLog{FactLog: NewSQLiteFactLog(openTestDB(t)), err: boom}
	ack := &recordingAcker{}
	b := NewBarrier(fl, es, ack, &recordingStall{}, testLogger(t))
	tgt := &fakeTarget{
		written:  map[int32][]WrittenArticle{0: {{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100}}},
		artCount: 4,
	}

	if err := b.Run(ctx, "job-1", tgt); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the fact-log failure", err)
	}
	if len(ack.proofs) != 0 {
		t.Errorf("acked despite an unreadable fact log")
	}
	if got, _ := es.Load(ctx, "job-1"); len(got) != 0 {
		t.Errorf("committed %d extents despite an unreadable fact log", len(got))
	}
}

// TestBarrier_MultipleFilesSyncAllBeforeAnyClaim pins R5's "fsync every open
// file of the job" ordering across more than one file: every sync must
// precede every stat, not merely the stat of its own file.
//
// It also pins that each file's extent is committed under its OWN file_idx.
// That is not a cosmetic field: file_extents is keyed on (job_id, file_idx)
// and the store INSERTs OR REPLACEs, so a barrier that leaves FileIdx at
// its zero value collapses every file of the job into one row. Each file but
// the last silently loses its durable bitmap, and the next resume re-fetches
// all of them — an L3 violation with no symptom until a restart.
//
// The drained articles are 5 and 1, in that order, so the proof's sort is
// pinned too rather than being satisfied by an already-ascending fixture.
func TestBarrier_MultipleFilesSyncAllBeforeAnyClaim(t *testing.T) {
	ctx := context.Background()
	es := NewSQLiteExtentStore(openTestDB(t))
	tgt := &fakeTarget{
		files: []int32{0, 1},
		written: map[int32][]WrittenArticle{
			0: {{FileIdx: 0, ArtIdx: 5, Offset: 0, Length: 10}},
			1: {{FileIdx: 1, ArtIdx: 1, Offset: 0, Length: 20}},
		},
		artCount: 8,
	}
	ack := &recordingAcker{}
	b, _ := newBarrierWithStores(t, es, ack, &recordingStall{})
	if err := b.Run(ctx, "job-1", tgt); err != nil {
		t.Fatal(err)
	}
	want := []string{"drain", "drain", "sync", "sync", "stat", "stat"}
	if len(tgt.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", tgt.calls, want)
	}
	for i := range want {
		if tgt.calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v — every sync precedes every claim", tgt.calls, want)
		}
	}
	if len(ack.proofs) != 1 {
		t.Fatalf("acked %d proofs, want 1 covering both files", len(ack.proofs))
	}
	arts := ack.proofs[0].Articles()
	if len(arts) != 2 || arts[0] != 1 || arts[1] != 5 {
		t.Errorf("proof articles = %v, want [1 5] — drained as 5 then 1, so the proof is sorted", arts)
	}

	got, err := es.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("loaded %d extents, want 2 — a shared file_idx collapses the job's cache into one row", len(got))
	}
	// Load orders by file_idx, so index i is file i.
	for i, wantBytes := range []int64{10, 20} {
		if got[i].FileIdx != int32(i) {
			t.Errorf("extent %d has FileIdx %d, want %d", i, got[i].FileIdx, i)
		}
		if got[i].BytesDurable != wantBytes {
			t.Errorf("file %d BytesDurable = %d, want %d", i, got[i].BytesDurable, wantBytes)
		}
	}
	if !got[0].Durable.Get(5) {
		t.Error("file 0 lost article 5's durable bit")
	}
	if !got[1].Durable.Get(1) {
		t.Error("file 1 lost article 1's durable bit")
	}
}

// TestBarrier_RecomputesThePrefixCRCRatherThanCarryingIt pins that the CRC
// anchor and its CRC always move together.
//
// PrefixCRC is defined as the CRC32 of [0, VerifiedTo), so a CRC that outlives
// the prefix it was computed over relabels one range's checksum as another's.
// This test previously pinned a carry-and-clear rule: keep the loaded CRC when
// VerifiedTo happens not to move, zero it otherwise. That rule was replaced by
// recomputing both from the same walk over Class A, which makes the
// relabelling unreachable instead of merely forbidden — and which is also what
// finally lets a download produce a whole-file CRC at all, since under the old
// rule the barrier could only ever CLEAR one.
//
// The stored CRC here is deliberately a value no combine can produce, so
// carrying it forward is distinguishable from recomputing it.
func TestBarrier_RecomputesThePrefixCRCRatherThanCarryingIt(t *testing.T) {
	tests := []struct {
		name        string
		drain       []WrittenArticle
		wantVerTo   int64
		wantHasCRC  bool
		wantCRCZero bool
	}{
		{
			name:      "VerifiedTo advances and the CRC follows it",
			drain:     []WrittenArticle{{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100}},
			wantVerTo: 200,
		},
		{
			name:      "VerifiedTo is unchanged and the CRC is still recomputed",
			drain:     nil,
			wantVerTo: 100,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			es := NewSQLiteExtentStore(openTestDB(t))
			prior := NewBitmap(4)
			prior.Set(0)
			if err := es.Commit(ctx, "job-1", []FileExtent{{
				FileIdx: 0, Durable: prior, VerifiedTo: 100,
				PrefixCRC: 0xDEADBEEF, HasPrefixCRC: true,
			}}); err != nil {
				t.Fatal(err)
			}

			b, fl := newBarrierWithStores(t, es, &recordingAcker{}, &recordingStall{})
			if err := fl.Append(ctx, "job-1", []ArticleFact{
				{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
				{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100},
			}); err != nil {
				t.Fatal(err)
			}
			tgt := &fakeTarget{
				written:  map[int32][]WrittenArticle{0: tt.drain},
				artCount: 4,
			}
			if err := b.Run(ctx, "job-1", tgt); err != nil {
				t.Fatal(err)
			}

			got, err := es.Load(ctx, "job-1")
			if err != nil {
				t.Fatal(err)
			}
			if got[0].VerifiedTo != tt.wantVerTo {
				t.Fatalf("VerifiedTo = %d, want %d", got[0].VerifiedTo, tt.wantVerTo)
			}
			if got[0].PrefixCRC == 0xDEADBEEF {
				t.Errorf("PrefixCRC = %#x — the stored value was carried across a "+
					"recomputed prefix instead of being derived from the facts that "+
					"cover [0,%d)", got[0].PrefixCRC, got[0].VerifiedTo)
			}
			// The facts covering the prefix carry a CRC of 0 in this fixture, so
			// the combine over them is 0. The point is that it is DERIVED: the
			// assertion above is what fails if the stored value survives.
			if got[0].PrefixCRC != 0 {
				t.Errorf("PrefixCRC = %#x, want the combine over this prefix's facts", got[0].PrefixCRC)
			}
			// This target reports a size far above the prefix, so no prefix here
			// is the whole file and the CRC is correctly unavailable (R23).
			if got[0].HasPrefixCRC {
				t.Errorf("HasPrefixCRC = true for a prefix of %d on a much larger file; "+
					"a partial prefix has no whole-file value to report", got[0].VerifiedTo)
			}
		})
	}
}

// --- Direct unit tests for the unexported helpers -------------------------
//
// The tests above exercise these through Run, which is the shape that
// matters. These pin the cases Run cannot easily reach: an argument
// combination the four phases never construct in one pass.

// TestRouteFault pins A1's dispatch table and that the fault is also
// returned, so a caller that ignores Stallable still cannot read a storage
// fault as success.
func TestRouteFault(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantStalled int
		wantFailed  int
	}{
		{"retryable stalls", syscall.ENOSPC, 1, 0},
		{"permanent fails", syscall.EROFS, 0, 1},
		{"unrecognised defaults to retryable", errors.New("weird"), 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stall := &recordingStall{}
			b := NewBarrier(nil, nil, nil, stall, testLogger(t))
			f := storagefault.Classify("sync", "/x", tt.err)

			got := b.routeFault("job-1", f)
			if !errors.Is(got, tt.err) {
				t.Errorf("routeFault returned %v, want it to wrap %v", got, tt.err)
			}
			if len(stall.stalled) != tt.wantStalled {
				t.Errorf("stalled %d, want %d", len(stall.stalled), tt.wantStalled)
			}
			if len(stall.failed) != tt.wantFailed {
				t.Errorf("failed %d, want %d", len(stall.failed), tt.wantFailed)
			}
		})
	}
}

// TestPriorExtent_NoStoredExtentYieldsAnEmptyBitmapOfTheRightWidth pins the
// first-barrier case: nothing is durable, and the bitmap is already wide
// enough for every article so the caller never has to resize mid-apply.
func TestPriorExtent_NoStoredExtentYieldsAnEmptyBitmapOfTheRightWidth(t *testing.T) {
	ctx := context.Background()
	b := NewBarrier(nil, NewSQLiteExtentStore(openTestDB(t)), nil, nil, testLogger(t))

	ext, err := b.priorExtent(ctx, "job-1", 3, 70)
	if err != nil {
		t.Fatal(err)
	}
	if ext.Durable.Len() != 70 {
		t.Errorf("bitmap width = %d, want 70", ext.Durable.Len())
	}
	if ext.Durable.Count() != 0 || ext.VerifiedTo != 0 || ext.BytesDurable != 0 {
		t.Errorf("a job with no committed extent claimed something: %+v", ext)
	}
}

// TestPriorExtent_LoadFailureIsReturned pins that an unreadable extent store
// aborts rather than being read as "nothing durable yet" — that reading
// would silently reset a job's progress to zero and re-download it whole.
func TestPriorExtent_LoadFailureIsReturned(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("extent store unreadable")
	b := NewBarrier(nil, &loadErrStore{err: boom}, nil, nil, testLogger(t))

	if _, err := b.priorExtent(ctx, "job-1", 0, 4); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the load failure", err)
	}
}

type loadErrStore struct {
	ExtentStore
	err error
}

func (l *loadErrStore) Load(context.Context, string) ([]FileExtent, error) { return nil, l.err }

// LoadFile fails the same way. priorExtent reads one file by key rather than
// filtering a whole-job load, so overriding only Load would leave the failure
// path this fake exists for unreachable — the embedded nil ExtentStore would
// be called instead and panic.
func (l *loadErrStore) LoadFile(context.Context, string, int32) (FileExtent, bool, error) {
	return FileExtent{}, false, l.err
}

// TestGaplessPrefix pins the walk's stopping conditions individually. The
// Run-level test covers the two hole shapes; these cover the boundaries a
// full barrier pass does not produce on its own.
func TestGaplessPrefix(t *testing.T) {
	tests := []struct {
		name    string
		facts   []ArticleFact
		durable []int
		want    int64
	}{
		{
			name: "no facts means no proven prefix",
			want: 0,
		},
		{
			name:    "a file whose first fact does not start at 0 proves nothing",
			facts:   []ArticleFact{{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100}},
			durable: []int{1},
			want:    0,
		},
		{
			name: "a fully durable contiguous run is proven end to end",
			facts: []ArticleFact{
				{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
				{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 50},
			},
			durable: []int{0, 1},
			want:    150,
		},
		{
			name: "an overlapping fact stops the walk rather than being tiled over",
			facts: []ArticleFact{
				{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
				{FileIdx: 0, ArtIdx: 1, Offset: 50, Length: 100},
			},
			durable: []int{0, 1},
			want:    100,
		},
		{
			name:    "the first article being non-durable stops the walk at 0",
			facts:   []ArticleFact{{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100}},
			durable: nil,
			want:    0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			fl := NewSQLiteFactLog(openTestDB(t))
			if len(tt.facts) > 0 {
				if err := fl.Append(ctx, "job-1", tt.facts); err != nil {
					t.Fatal(err)
				}
			}
			bm := NewBitmap(4)
			for _, i := range tt.durable {
				bm.Set(i)
			}
			// Read back through the real fact log rather than passing
			// tt.facts straight in: the walk relies on ForFile returning
			// rows ordered by Offset, and a slice handed over in table
			// order would satisfy the walk without that guarantee holding.
			stored, err := fl.ForFile(ctx, "job-1", 0)
			if err != nil {
				t.Fatal(err)
			}

			got, _ := gaplessPrefix(stored, 0, bm, &fakeTarget{artCount: 4})
			if got != tt.want {
				t.Errorf("gaplessPrefix = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestGaplessPrefix_UnplaceableFactStopsTheWalk pins that a fact whose
// article the target cannot place is treated as unproven rather than as
// durable. Unlike the same condition in buildExtent this is not an error:
// the fact log outlives a file's article set, so a stale fact is possible
// and the safe reading is S3's — no evidence means not proven.
func TestGaplessPrefix_UnplaceableFactStopsTheWalk(t *testing.T) {
	ctx := context.Background()
	fl := NewSQLiteFactLog(openTestDB(t))
	if err := fl.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
	}); err != nil {
		t.Fatal(err)
	}
	bm := NewBitmap(4)
	bm.Set(0)
	stored, err := fl.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}

	got, _ := gaplessPrefix(stored, 0, bm, &fakeTarget{artCount: 4, noOrd: true})
	if got != 0 {
		t.Errorf("gaplessPrefix = %d, want 0 — an unplaceable fact proves nothing", got)
	}
}

// TestBuildExtent_ReturnsTheArticlesToAckWithoutCommitting pins that phase 3
// is pure with respect to persistence: it derives the extent and names the
// articles, but nothing reaches the store until Run's phase 4 commits.
func TestBuildExtent_ReturnsTheArticlesToAckWithoutCommitting(t *testing.T) {
	ctx := context.Background()
	es := NewSQLiteExtentStore(openTestDB(t))
	b, _ := newBarrierWithStores(t, es, &recordingAcker{}, &recordingStall{})
	tgt := &fakeTarget{size: 300, modTime: 11, artCount: 4}

	ext, acked, err := b.buildExtent(ctx, "job-1", 0, []WrittenArticle{
		{FileIdx: 0, ArtIdx: 2, Offset: 200, Length: 100},
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
	}, nil, tgt)
	if err != nil {
		t.Fatal(err)
	}
	if len(acked) != 2 || acked[0] != 2 || acked[1] != 0 {
		t.Errorf("acked = %v, want [2 0] in drain order — Run is what sorts", acked)
	}
	if ext.BytesDurable != 200 || ext.Size != 300 || ext.ModTimeNs != 11 {
		t.Errorf("extent = %+v, want 200 bytes durable and the stat stamp", ext)
	}
	if got, _ := es.Load(ctx, "job-1"); len(got) != 0 {
		t.Errorf("buildExtent committed %d extents; only Run's phase 4 may commit", len(got))
	}
}

// Confirm records the release so a test can tell a confirmed cycle from an
// abandoned one; the real writer drops its drain report here.
func (s *fakeTarget) Confirm(_ context.Context, idx int32) { s.confirmed = append(s.confirmed, idx) }

// TestBarrier_ConfirmsOnlyAfterTheCommitAndAck pins where the drain report is
// released, which is the difference between a retried barrier that can recover
// and one that has nothing left to work with.
//
// Sync used to release it, so any failure after the fsync — a Stat timeout, a
// busy ExtentStore.Commit, an AckDurable answering ErrJobNotResident — lost the
// report while the bytes stayed on disk. Those articles were then never acked
// and never earned a durable bit, and a redelivery is dropped as a duplicate,
// so the file could not complete for the life of the handle.
func TestBarrier_ConfirmsOnlyAfterTheCommitAndAck(t *testing.T) {
	ctx := context.Background()
	drain := map[int32][]WrittenArticle{0: {{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100}}}

	t.Run("a successful cycle confirms", func(t *testing.T) {
		b, fl := newBarrierWithStores(t, NewSQLiteExtentStore(openTestDB(t)),
			&recordingAcker{}, &recordingStall{})
		if err := fl.Append(ctx, "job-1", []ArticleFact{
			{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
		}); err != nil {
			t.Fatal(err)
		}
		tgt := &fakeTarget{written: drain, artCount: 4}
		if err := b.Run(ctx, "job-1", tgt); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(tgt.confirmed) == 0 {
			t.Error("a cycle that committed and acked never confirmed; the report is " +
				"retained forever and every later checkpoint re-reports the whole file")
		}
	})

	t.Run("a failed ack does not confirm", func(t *testing.T) {
		b, fl := newBarrierWithStores(t, NewSQLiteExtentStore(openTestDB(t)),
			&recordingAcker{err: errors.New("job not resident")}, &recordingStall{})
		if err := fl.Append(ctx, "job-1", []ArticleFact{
			{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
		}); err != nil {
			t.Fatal(err)
		}
		tgt := &fakeTarget{written: drain, artCount: 4}
		if err := b.Run(ctx, "job-1", tgt); err == nil {
			t.Fatal("Run reported success while the ack failed")
		}
		if len(tgt.confirmed) != 0 {
			t.Errorf("the report was confirmed for file %v despite the ack failing. "+
				"The retry drains nothing, so those articles are never acked and the "+
				"file cannot complete for the life of the handle", tgt.confirmed)
		}
	})
}

// TestConfirmAll_ReleasesEveryFileOfTheJob pins that the release covers the
// whole job rather than the file that happened to be last.
//
// Run drains and syncs every open file before it builds anything, so a cycle
// that lands has made claims about all of them. Confirming only some would
// leave the rest re-reporting forever, and buildExtent would redo their whole
// article set on every later checkpoint.
func TestConfirmAll_ReleasesEveryFileOfTheJob(t *testing.T) {
	b, _ := newBarrierWithStores(t, NewSQLiteExtentStore(openTestDB(t)),
		&recordingAcker{}, &recordingStall{})
	tgt := &fakeTarget{artCount: 4}

	b.confirmAll(context.Background(), []int32{0, 1, 2}, tgt)

	if len(tgt.confirmed) != 3 {
		t.Fatalf("confirmed %v, want all three files", tgt.confirmed)
	}
	for i, idx := range []int32{0, 1, 2} {
		if tgt.confirmed[i] != idx {
			t.Errorf("confirmed[%d] = %d, want %d", i, tgt.confirmed[i], idx)
		}
	}
}
