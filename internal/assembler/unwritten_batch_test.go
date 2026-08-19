package assembler

import (
	"slices"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestDrainFailure_ReportsEveryRolledBackArticle pins the path where the
// BARRIER routes the fault and this package still owes the articles.
//
// A failed Drain rolls back everything after the write that failed, and that
// set never crosses the SyncTarget interface — the barrier gets an error and a
// list of what DID land, and nothing else. So the articles were left neither
// Done, nor Failed, nor Outstanding: their Emitted bits stayed set,
// ForEachUnfinishedArticle skipped them, and only a restart's ClearAllEmitted
// recovered them.
//
// The counts matter as much as the routing. partsWritten is incremented on
// ACCEPT, so a rolled-back article leaves the file one part closer to
// TotalParts with nothing behind it, and a later article can take it to
// completion — firing OnFileComplete over bytes that never reached WriteAt,
// with failedBytes unchanged, so the job reports 100% health.
func TestDrainFailure_ReportsEveryRolledBackArticle(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	var rolledBack []int32
	a.opts.OnArticlesUnwritten = func(_ string, _ int, artIdxs []int32) {
		rolledBack = append(rolledBack, artIdxs...)
	}
	var faults []*storagefault.Fault
	a.opts.OnWriteFault = func(_ string, _ int, f *storagefault.Fault) { faults = append(faults, f) }

	wc := newWriteCache(1 << 20)
	f := newHelperFile(t, dir, "job1_0.dat", 0)
	f.w.wc = wc
	f.info.TotalParts = 3
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}

	for i := range 2 {
		if !a.handleSuccessArticle(f, WriteRequest{
			JobID: "job", FileIdx: 0, ArtIdx: testArtIdx(i),
			MessageID: string(rune('a' + i)), Offset: int64(i) * 4, Data: []byte("AAAA"),
		}) {
			t.Fatalf("article %d was not accepted, so the fixture never buffered it", i)
		}
	}

	// The device fills up between the accepts and the drain.
	f.w.writeAt = func([]byte, int64) (int, error) { return 0, syscall.ENOSPC }

	// The worker's own handler, which is where the barrier's Drain lands.
	// Called directly because no worker is running, so nothing else touches
	// the writer this owns (X1).
	op := &syncOp{kind: opDrain, jobID: "job", fileIdx: 0, reply: make(chan syncReply, 1)}
	a.handleSyncOp(op, open, wc)
	if r := <-op.reply; r.err == nil {
		t.Fatal("the drain succeeded over a full device, so this test proves nothing")
	}

	slices.Sort(rolledBack)
	if !slices.Equal(rolledBack, []int32{0, 1}) {
		t.Errorf("rolled-back articles = %v, want [0 1] — this set never crosses the "+
			"SyncTarget interface, so the barrier that routes the fault cannot report "+
			"it: an article reported by nobody keeps its Emitted bit and is never "+
			"re-dispatched", rolledBack)
	}
	if len(faults) != 0 {
		t.Errorf("the drain path routed %d faults itself; the barrier routes this one, "+
			"and routing it twice parks the job on its own second report", len(faults))
	}
}

// TestDrainFailure_GivesBackThePartsItRolledBack is the counting half, driven
// through handleSuccessArticle so partsWritten moves the way the worker moves
// it.
func TestDrainFailure_GivesBackThePartsItRolledBack(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	var rolledBack []int32
	a.opts.OnArticlesUnwritten = func(_ string, _ int, artIdxs []int32) {
		rolledBack = append(rolledBack, artIdxs...)
	}

	f := newHelperFile(t, dir, "job1_0.dat", 0)
	f.w.wc = newWriteCache(1 << 20)
	f.info.TotalParts = 3

	for i := range 2 {
		if !a.handleSuccessArticle(f, WriteRequest{
			JobID: "job1", FileIdx: 0, ArtIdx: testArtIdx(i),
			MessageID: string(rune('a' + i)), Offset: int64(i) * 4, Data: []byte("AAAA"),
		}) {
			t.Fatalf("article %d was not accepted, so the fixture never counted it", i)
		}
	}
	if f.w.parts() != 2 {
		t.Fatalf("partsWritten = %d, want 2; the fixture did not reach the state under test",
			f.w.parts())
	}

	f.w.writeAt = func([]byte, int64) (int, error) { return 0, syscall.ENOSPC }
	if _, err := f.w.Drain(); err == nil {
		t.Fatal("Drain succeeded over a full device, so this test proves nothing")
	}
	a.releaseFaulted(f, "job1", 0)

	slices.Sort(rolledBack)
	if !slices.Equal(rolledBack, []int32{0, 1}) {
		t.Errorf("rolled-back articles = %v, want [0 1]", rolledBack)
	}
	if f.w.parts() != 0 {
		t.Errorf("partsWritten = %d, want 0 — the file is %d parts closer to TotalParts "+
			"with nothing on disk behind them, so a later article completes it over "+
			"bytes that never reached WriteAt while health reads 100%%",
			f.w.parts(), f.w.parts())
	}
}

// TestNoteArticlesUnwritten_IsSilentOnAnEmptySet keeps a no-op from being
// reported as an event.
//
// The callback reaches the queue, and a call with no articles would log a
// warning about a roll-back that did not happen — noise an operator has to
// read past on every successful drain of a file that has nothing buffered.
func TestNoteArticlesUnwritten_IsSilentOnAnEmptySet(t *testing.T) {
	a := newHelperAssembler()
	var calls int
	a.opts.OnArticlesUnwritten = func(string, int, []int32) { calls++ }

	a.noteArticlesUnwritten("job", 0, nil)
	a.noteArticlesUnwritten("job", 0, []int32{})
	if calls != 0 {
		t.Errorf("the callback ran %d times for an empty set", calls)
	}

	a.noteArticlesUnwritten("job", 0, []int32{4})
	if calls != 1 {
		t.Errorf("the callback ran %d times for one article, want 1", calls)
	}
}

// TestFailPermanent_KeepsTheArticleCounted pins the other half of the pair
// fail belongs to.
//
// A rejected article is resolved elsewhere and will never arrive again, so it
// must keep its count toward the file's part total — a file that stopped
// counting it could never reach TotalParts, and the job would sit at 100% with
// nothing outstanding. It must also not appear in the rolled-back set, which
// would clear an Emitted bit the ack is about to resolve.
func TestFailPermanent_KeepsTheArticleCounted(t *testing.T) {
	w := newTestFileWriter(t)
	w.seenDone["m1"] = struct{}{}

	w.failPermanent(articleID{msgID: "m1", artIdx: 1})

	if _, still := w.seenDone["m1"]; still {
		t.Error("a permanently refused article is still recorded as done; nothing wrote its bytes")
	}
	if _, failed := w.seenFailed["m1"]; !failed {
		t.Error("the refused article is not in seenFailed, so a redelivery would be " +
			"counted a second time and overshoot the file's part total")
	}
	if got := w.takeFaulted(); len(got) != 0 {
		t.Errorf("takeFaulted() = %v — a refused article must not be rolled back to "+
			"Outstanding: it is resolved, and re-dispatching it would fetch the same "+
			"unusable article forever", got)
	}

	// An empty Message-ID has nothing to key either set on, so it is a no-op
	// rather than a map entry under "".
	w.failPermanent(articleID{artIdx: 2})
	if _, bogus := w.seenFailed[""]; bogus {
		t.Error("an article with no Message-ID was recorded under the empty key")
	}
}
