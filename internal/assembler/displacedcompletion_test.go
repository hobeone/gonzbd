package assembler

import (
	"bytes"
	"testing"
)

// TestDisplacement_FileStillReachesTotalParts is the defect #386 named: a file
// that suffered a cache-resident collision could never complete.
//
// Nothing drove a file to completion after a displacement before this, which is
// how it survived. The existing collision tests all stop at the rollback and
// assert the count in isolation, and a count one short of TotalParts looks
// unremarkable in isolation — it is only wrong relative to a total none of them
// supplied.
//
// The three assertions are made together on purpose. "OnFileComplete fired" on
// its own would also pass if the arriving article were counted twice, which is
// a different bug with the same symptom, so the part total and the loser's
// disposition are pinned alongside it.
func TestDisplacement_FileStillReachesTotalParts(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	var rejected []int32
	var unwritten []int32
	fired := 0
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		rejected = append(rejected, artIdx)
	}
	a.opts.OnArticlesUnwritten = func(_ string, _ int, idxs []int32) {
		unwritten = append(unwritten, idxs...)
	}
	a.opts.OnFileComplete = func(_ string, _ int) { fired++ }

	wc := newWriteCache(1 << 20)
	f := newHelperFile(t, dir, "wedge.dat", 0)
	f.w.wc = wc
	f.info.TotalParts = 2
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}
	completed := map[fileKey]struct{}{}

	for _, art := range []struct {
		msg string
		idx int32
	}{{"x", 0}, {"y", 1}} {
		a.processRequest(WriteRequest{
			JobID: "job", FileIdx: 0, ArtIdx: art.idx, MessageID: art.msg,
			Offset: 0, Data: []byte("XXXX"),
		}, open, completed, wc)
	}

	if got := f.w.parts(); got != f.info.TotalParts {
		t.Errorf("parts = %d, TotalParts = %d: the file is short by %d and no article "+
			"can supply the difference — x is resolved and will not arrive again",
			got, f.info.TotalParts, f.info.TotalParts-got)
	}
	if fired != 1 {
		t.Errorf("OnFileComplete fired %d times, want 1: without it MarkFileComplete "+
			"never runs and the job sits at 100%% with nothing outstanding", fired)
	}
	if len(rejected) != 1 || rejected[0] != 0 {
		t.Errorf("rejected = %v, want [0]: the displaced article must be resolved, "+
			"since re-fetching it reproduces the collision", rejected)
	}
	if len(unwritten) != 0 {
		t.Errorf("unwritten = %v, want none: returning the loser to Outstanding is the "+
			"ping-pong this disposition exists to avoid", unwritten)
	}
}

// TestDisplacement_StaleOwnerIsCountedNotMerelyKept is why the accounting goes
// through admitPermanentFailure rather than failPermanent.
//
// failPermanent KEEPS a part. That is sufficient where acceptArticle calls it,
// because admitAccepted ran a statement earlier and the article provably holds
// one. A displaced incumbent has no such guarantee: acceptedAt entries are
// never removed, so an article whose write faulted keeps owning its offset
// after fail has taken its part and its seenDone entry away. The next article
// at that offset displaces an owner holding nothing, and a primitive that only
// keeps parts would keep nothing.
func TestDisplacement_StaleOwnerIsCountedNotMerelyKept(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(1<<20))
	first := articleID{msgID: "a", artIdx: 0}
	second := articleID{msgID: "b", artIdx: 1}

	w.admitAccepted("a")
	if err := w.Accept(first, 0, append([]byte(nil), bytes.Repeat([]byte{'A'}, 32)...)); err != nil {
		t.Fatalf("accept first: %v", err)
	}

	// The write-fault rollback: the part and the seenDone entry go, the
	// acceptedAt ownership stays. Asserted rather than assumed, because the
	// whole case rests on those two moving apart.
	w.fail(first)
	_ = w.takeFaulted()
	if w.parts() != 0 {
		t.Fatalf("parts = %d after the rollback, want 0; the fixture did not roll the "+
			"article back, so it proves nothing about a stale owner", w.parts())
	}
	if owner, taken := w.acceptedAt[0]; !taken || owner.id != first {
		t.Fatalf("acceptedAt[0] = %+v (taken=%v), want the rolled-back article: the "+
			"fixture did not produce a stale owner", owner.id, taken)
	}

	w.admitAccepted("b")
	if err := w.Accept(second, 0, append([]byte(nil), bytes.Repeat([]byte{'B'}, 32)...)); err != nil {
		t.Fatalf("accept second: %v", err)
	}

	if got := w.parts(); got != 2 {
		t.Errorf("parts = %d, want 2: the stale owner was resolved permanently failed "+
			"while holding no part, so it had to be COUNTED for one rather than left "+
			"as it was", got)
	}
}

// TestDisplacement_RedeliveryIsNotCountedTwice pins the seen-set half.
//
// Recording the loser in seenFailed is what lets a later copy of it be
// recognised. Without that record it sits in neither set, which
// handleLateDuplicate reads as "never accounted for" and reports permanently
// failed a second time, and which handleSuccessArticle reads as a fresh
// article and counts again.
func TestDisplacement_RedeliveryIsNotCountedTwice(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	var rejected []int32
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		rejected = append(rejected, artIdx)
	}

	wc := newWriteCache(1 << 20)
	f := newHelperFile(t, dir, "redeliver.dat", 0)
	f.w.wc = wc
	f.info.TotalParts = 2
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}
	completed := map[fileKey]struct{}{}

	send := func(msg string, idx int32) {
		a.processRequest(WriteRequest{
			JobID: "job", FileIdx: 0, ArtIdx: idx, MessageID: msg,
			Offset: 0, Data: []byte("XXXX"),
		}, open, completed, wc)
	}

	send("x", 0)
	send("y", 1)
	if _, failed := f.w.seenFailed["x"]; !failed {
		t.Fatalf("x is not in seenFailed after the displacement; the fixture did not " +
			"reach the disposition this test is about")
	}

	send("x", 0) // the loser comes back

	if got := f.w.parts(); got != 2 {
		t.Errorf("parts = %d, want 2: the redelivery was counted again, carrying the "+
			"file past TotalParts", got)
	}
	if len(rejected) != 1 {
		t.Errorf("rejected = %v, want one entry: the redelivery was reported "+
			"permanently failed a second time for an article already resolved",
			rejected)
	}
}

// TestDisplacement_UntrackedRedeliveryCannotFinalizeAShortFile is the failure
// mode that keeping the part opens up when nothing records the article.
//
// handleSuccessArticle's two dedup arms are both gated on a non-empty
// Message-ID, so before resolvedUntracked existed an untracked article had no
// duplicate handling at all. Every redelivery ran the whole accept path, took
// another part and displaced whoever owned the offset by then, so the count
// climbed one per copy — and with a third segment still outstanding it reached
// TotalParts and finalized a file that was genuinely short.
//
// The give-back this replaced concealed it rather than preventing it: the
// re-collision handed back the part the redelivery had just taken, so the
// count oscillated and the file never completed at all. Trading a visible
// stall for a silently truncated file is the wrong direction, which is why the
// part is kept AND the disposition recorded rather than either alone.
//
// Three segments, not two, because with TotalParts=2 the file legitimately
// completes on the collision itself and the premature finalize is
// indistinguishable from the correct one.
func TestDisplacement_UntrackedRedeliveryCannotFinalizeAShortFile(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	fired := 0
	a.opts.OnFileComplete = func(_ string, _ int) { fired++ }
	a.opts.OnArticleRejected = func(_ string, _ int, _ int32, _ string) {}

	wc := newWriteCache(1 << 20)
	f := newHelperFile(t, dir, "short.dat", 0)
	f.w.wc = wc
	f.info.TotalParts = 3 // the third segment never arrives
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}
	completed := map[fileKey]struct{}{}

	send := func(idx int32) {
		a.processRequest(WriteRequest{
			JobID: "job", FileIdx: 0, ArtIdx: idx, MessageID: "",
			Offset: 0, Data: []byte("XXXX"),
		}, open, completed, wc)
	}

	send(1)
	send(2) // collides with 1, displacing it
	if got := f.w.parts(); got != 2 {
		t.Fatalf("parts = %d after the collision, want 2; the fixture is not in the "+
			"state this test is about", got)
	}

	send(1) // the displaced article comes back

	if got := f.w.parts(); got != 2 {
		t.Errorf("parts = %d after the redelivery, want 2: the article is already "+
			"counted and already resolved, so a second copy must add nothing", got)
	}
	if fired != 0 {
		t.Errorf("OnFileComplete fired %d times: the file has two of three segments, "+
			"and finalizing it here truncates it silently at whatever the barrier "+
			"has recorded", fired)
	}
}

// TestDisplacement_UntrackedGuardLeavesTheFailureBudgetIntact covers the reason
// failDisplaced skips an article with no Message-ID rather than handling it.
//
// admitPermanentFailure deduplicates on its argument, and an empty key is a
// legal one. Recording the displacement under it would spend that single slot,
// and the next genuinely different untracked article to fail would be waved
// through as a duplicate and never counted — the same shortfall #386 fixed,
// arriving from the other direction.
//
// There is no code change to revert here, so the red check is the mutation
// described in the commit: record the empty key in failDisplaced and watch this
// fail.
func TestDisplacement_UntrackedGuardLeavesTheFailureBudgetIntact(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(1<<20))
	first := articleID{msgID: "", artIdx: 1}
	second := articleID{msgID: "", artIdx: 2}

	w.admitAccepted(first.msgID)
	if err := w.Accept(first, 0, append([]byte(nil), bytes.Repeat([]byte{'A'}, 32)...)); err != nil {
		t.Fatalf("accept first: %v", err)
	}
	w.admitAccepted(second.msgID)
	if err := w.Accept(second, 0, append([]byte(nil), bytes.Repeat([]byte{'B'}, 32)...)); err != nil {
		t.Fatalf("accept second: %v", err)
	}
	_ = w.takeFaulted()

	// A third untracked article, unrelated to the collision, fails outright.
	if !w.admitPermanentFailure("") {
		t.Error("a distinct untracked article was refused as a duplicate: the " +
			"displacement spent the empty key's single dedup slot, so this article " +
			"is never counted and the file cannot reach TotalParts")
	}
	if got := w.parts(); got != 3 {
		t.Errorf("parts = %d, want 3: two colliding articles plus the one that failed "+
			"on its own, each of which is a segment the file waits for", got)
	}
}
