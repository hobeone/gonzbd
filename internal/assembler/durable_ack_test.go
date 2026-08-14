package assembler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// An article's outcome is no longer an ack (X2): the assembler has no ack
// authority at all, and durability.Barrier is the only component that tells
// the queue anything, from Drain's return value. Where a test on this file
// used to assert "article X was marked Done", it now asserts "article X
// appears in FileWriter.Drain's return". Where it asserted "article X was
// marked Failed", it now asserts "article X is ABSENT from Drain" — and,
// where the fault is directly reachable, that a *storagefault.Fault came
// back. See filewriter_test.go for the base pins this file specializes.

// TestFailedCoalescedRunFailsEveryArticleInTheRun pins the case #355's Scope
// section wrongly cleared, translated for FileWriter.Drain as the evidence
// surface: buildContiguousRun coalesces every article contiguous with the
// write cursor into one buffer and pools the originals before flushRun
// attempts the write, so a single failing WriteAt loses all of them — not
// just the one whose arrival triggered the flush. None of the six may appear
// once the writer's evidence is collected.
func TestFailedCoalescedRunFailsEveryArticleInTheRun(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(8<<20))
	_ = w.handle.Close() // every WriteAt from here on fails

	const (
		artCount = 6
		artSize  = 100_000
	)
	if artCount*artSize <= contiguousRunSize {
		t.Fatalf("fixture does not reach the coalescing threshold: %d <= %d",
			artCount*artSize, contiguousRunSize)
	}

	var lastErr error
	for i := range int32(artCount) {
		lastErr = w.Accept(articleID{msgID: fmt.Sprintf("msg%d", i), artIdx: i}, int64(i)*artSize, make([]byte, artSize))
	}
	if lastErr == nil {
		t.Fatal("the run-triggering Accept returned nil error against a closed handle")
	}

	if got := w.writtenSoFar(); len(got) != 0 {
		t.Errorf("writtenSoFar = %v after the run's WriteAt failed; every article in "+
			"a coalesced run loses its bytes together — acking only the triggering "+
			"article would leave the rest reported Written with no bytes on disk (#355)", got)
	}

	got, err := w.Drain(context.Background())
	if err != nil {
		t.Fatalf("Drain after the failed run: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Drain = %v, want empty; the coalesced run's articles were already "+
			"cleared from the cache when the run was built, so nothing is left to drain", got)
	}
}

// TestFailedCoalescedRun_RetryIsWrittenNotDiscarded pins the half of the
// failure path that is not an ack, end to end.
//
// seenDone means "accepted and counted toward TotalParts". flushRun's failure
// branch calls w.fail on every part of the run, and fail() BOTH inserts into
// seenFailed AND deletes from seenDone. The delete is the load-bearing half:
// without it handleSuccessArticle's duplicate branch discards the retry
// (assembler.go, the `if _, dup := w.seenDone[...]` arm) instead of writing it,
// so a coalesced run that hit ENOSPC leaves the file permanently short, never
// acked and never failed. Silent without par2.
//
// # Why this test drives the Assembler and not the FileWriter
//
// It replaces a version that called w.Accept on a bare FileWriter and asserted
// len(w.seenDone) == 0. That assertion held UNCONDITIONALLY: seenDone is only
// ever written by handleSuccessArticle, so a bare writer's map is empty no
// matter what fail() does. Deleting the seenDone clear left the whole suite
// green. Reaching the map at all requires the real Assembler path.
//
// The assertion is also deliberately the BYTES, not the maps. Asserting
// "seenDone is empty" re-tests fail() against itself; asserting that a retry
// actually lands on disk is the property the maps exist to produce, and it is
// what fails when the clear is dropped.
func TestFailedCoalescedRun_RetryIsWrittenNotDiscarded(t *testing.T) {
	// 6 x 100_000 = 600_000 bytes, over contiguousRunSize (512 KiB), so the
	// sixth Accept coalesces all six into ONE run and flushes it. Five would
	// stay buffered and never reach flushRun at all.
	const (
		artCount = 6
		artSize  = 100_000
	)

	a := newHelperAssembler()
	dir := t.TempDir()
	path := filepath.Join(dir, "coalesced.dat")
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = fh.Close() })

	key := fileKey{jobID: "job", fileIdx: 0}
	w := newFileWriter(fh, path, key, newWriteCache(8<<20))
	f := &openFile{
		w:    w,
		info: FileInfo{Path: path, ExpectedSize: artCount * artSize},
		key:  key,
	}

	failWrites := true
	w.writeAt = func(p []byte, off int64) (int, error) {
		if failWrites {
			return 0, syscall.ENOSPC
		}
		return fh.WriteAt(p, off)
	}

	// Each article is filled with a distinct byte, so a short or mis-ordered
	// file is distinguishable from a merely zero-filled one.
	body := func(i int) []byte {
		b := make([]byte, artSize)
		for j := range b {
			b[j] = byte('A' + i)
		}
		return b
	}
	send := func(i int) {
		a.handleSuccessArticle(f, WriteRequest{
			JobID:     "job",
			MessageID: fmt.Sprintf("msg%d", i),
			ArtIdx:    int32(i), //nolint:gosec // G115: tiny test index
			Offset:    int64(i) * artSize,
			Data:      body(i),
		})
	}

	// Phase 1 — the whole coalesced run hits ENOSPC.
	for i := range artCount {
		send(i)
	}
	if len(w.seenDone) != 0 {
		t.Errorf("seenDone still holds %d articles after their run failed; a retry "+
			"would be discarded as a duplicate over bytes that are not on disk",
			len(w.seenDone))
	}
	if len(w.seenFailed) != artCount {
		t.Errorf("seenFailed holds %d articles, want %d", len(w.seenFailed), artCount)
	}

	// Phase 2 — the device recovers and every article is re-delivered.
	failWrites = false
	for i := range artCount {
		send(i)
	}
	if _, err := w.Drain(context.Background()); err != nil {
		t.Fatalf("Drain after the retries: %v", err)
	}

	// Phase 3 — the bytes are the assertion.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != artCount*artSize {
		t.Fatalf("file is %d bytes, want %d; the retried articles were discarded as "+
			"duplicates and their bytes never reached disk", len(got), artCount*artSize)
	}
	for i := range artCount {
		want := byte('A' + i)
		seg := got[i*artSize : (i+1)*artSize]
		for j, b := range seg {
			if b != want {
				t.Fatalf("article %d byte %d = %q, want %q; the retry did not write",
					i, j, b, want)
			}
		}
	}
}

// TestDuplicateSuccessWhileBufferedIsNotReAcked translates the old ack-timing
// pin: since this package no longer acks anything, "not re-acked" becomes "a
// duplicate does not add a second entry to what Drain reports." The dedup
// decision lives in handleSuccessArticle, keyed on the FileWriter's own
// seenDone map (R12), and fires the same way whether the first copy's bytes
// have reached disk yet or not — there is no ack whose timing could race any
// more.
func TestDuplicateSuccessWhileBufferedIsNotReAcked(t *testing.T) {
	a := newHelperAssembler()
	dir := t.TempDir()
	f := newHelperFile(t, dir, "dup.dat", 0)
	f.w.wc = newWriteCache(1 << 20) // cache enabled: the first copy stays buffered

	req := WriteRequest{JobID: "job", MessageID: "msg0", ArtIdx: 0, Offset: 0, Data: []byte("first copy")}
	if !a.handleSuccessArticle(f, req) {
		t.Fatal("first copy was not accepted")
	}
	if got := f.w.writtenSoFar(); len(got) != 0 {
		t.Fatalf("writtenSoFar = %v before any drain, want empty (bytes still buffered)", got)
	}

	dup := WriteRequest{JobID: "job", MessageID: "msg0", ArtIdx: 0, Offset: 0, Data: []byte("second copy")}
	if a.handleSuccessArticle(f, dup) {
		t.Error("a duplicate must not be counted toward TotalParts")
	}

	got, err := f.w.Drain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("Drain = %v, want exactly one entry — the duplicate must not have "+
			"queued a second write for bytes the first copy already claims", got)
	}
}

// TestDuplicateAtADifferentOffsetIsNotReAcked is the same guard against a
// duplicate that does not agree with the first copy about where the article
// belongs. The dedup is keyed on the Message-ID alone, so a second copy
// reporting a different yEnc offset still hits the duplicate branch and must
// not queue a second write.
func TestDuplicateAtADifferentOffsetIsNotReAcked(t *testing.T) {
	a := newHelperAssembler()
	dir := t.TempDir()
	f := newHelperFile(t, dir, "dupoffset.dat", 0)
	f.w.wc = newWriteCache(1 << 20)

	req := WriteRequest{JobID: "job", MessageID: "msg0", ArtIdx: 0, Offset: 0, Data: []byte("first copy")}
	if !a.handleSuccessArticle(f, req) {
		t.Fatal("first copy was not accepted")
	}

	// Same Message-ID, different offset, while the first copy is buffered.
	dup := WriteRequest{JobID: "job", MessageID: "msg0", ArtIdx: 0, Offset: 4096, Data: []byte("same article, other offset")}
	if a.handleSuccessArticle(f, dup) {
		t.Error("a duplicate must not be counted toward TotalParts")
	}

	got, err := f.w.Drain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("Drain = %v, want exactly one entry; the duplicate's mismatched "+
			"offset must not queue a second write", got)
	}
}

// TestDisplacedArticleIsFailed covers buffer's duplicate-offset branch. It is
// near-unreachable behind the seen-set dedup, but an article silently
// evicted from the cache is one that must not appear in Drain — nothing else
// will ever write its bytes.
func TestDisplacedArticleIsFailed(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(1<<20))

	// Two different Message-IDs claiming the same offset. Upstream dedup keys
	// on the Message-ID, so it does not catch this; the cache's own
	// replacement branch is what sees it.
	if err := w.Accept(articleID{msgID: "msg7", artIdx: 7}, 0, []byte("first")); err != nil {
		t.Fatalf("first article: %v", err)
	}
	if err := w.Accept(articleID{msgID: "msg8", artIdx: 8}, 0, []byte("second")); err != nil {
		t.Fatalf("second article: %v", err)
	}

	got, err := w.Drain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if a.ArtIdx == 7 {
			t.Error("the displaced article (7) appears in Drain; its bytes were " +
				"dropped when article 8 replaced it at the same offset")
		}
	}
	if len(got) != 1 || got[0].ArtIdx != 8 {
		t.Errorf("Drain = %v, want exactly the surviving article (8)", got)
	}
	if _, failed := w.seenFailed["msg7"]; !failed {
		t.Error("the displaced article was not recorded in seenFailed")
	}
	if _, still := w.seenDone["msg7"]; still {
		t.Error("the displaced article is still in seenDone")
	}
}

// TestZeroLengthArticleIsNotBuffered guards a hang rather than a wrong
// answer. buildContiguousRun scans from the write cursor and advances by the
// length of the article it finds there. A zero-length article at that offset
// never advances it, so the loop would run forever. Kept as-is: this is pure
// write-cache behaviour, untouched by the ack removal.
func TestZeroLengthArticleIsNotBuffered(t *testing.T) {
	wc := newWriteCache(1 << 20)
	key := fileKey{jobID: "job1", fileIdx: 0}

	cached, displaced := wc.buffer(key, bufferedArticle{offset: 0, data: nil})
	if cached {
		t.Error("a zero-length article was buffered; it cannot advance the " +
			"write cursor, so buildContiguousRun would never terminate")
	}
	if len(displaced) != 0 {
		t.Errorf("displaced = %v for an article that was never taken", displaced)
	}

	// End to end: it takes the inline path through Accept and is reported.
	w := newTestFileWriter(t, withCacheBytes(1<<20))
	if err := w.Accept(articleID{msgID: "msg0", artIdx: 0}, 0, nil); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	got, err := w.Drain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ArtIdx != 0 {
		t.Errorf("Drain = %v, want one entry for article 0 — the inline write is "+
			"a no-op but the article is still reported Written", got)
	}
}

// TestBuildContiguousRunStopsAtAZeroLengthArticle covers the second guard.
// buffer refuses to cache such an article, so only direct construction can
// put one in the map — which is what this does, since the cost of the scan
// not stopping is a hung worker rather than a wrong result. Kept as-is.
func TestBuildContiguousRunStopsAtAZeroLengthArticle(t *testing.T) {
	wc := newWriteCache(1 << 20)
	fb := &fileBuf{articles: make(map[int64]bufferedArticle)}
	fb.articles[0] = bufferedArticle{offset: 0, data: []byte{}}

	done := make(chan *flushRun, 1)
	go func() { done <- wc.buildContiguousRun(fb, 1) }()

	select {
	case run := <-done:
		if run != nil {
			t.Errorf("run = %+v, want nil; a zero-length article cannot start a run", run)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("buildContiguousRun did not return: the scan cannot advance past " +
			"a zero-length article at the write cursor")
	}
}

// TestBufferedReportsUnknownKey covers the lookup that tells an article still
// waiting in the cache from one already written. Kept as-is — pure
// write-cache behaviour, and still exercised directly by helpers_test.go's
// relievePressure fixture.
func TestBufferedReportsUnknownKey(t *testing.T) {
	wc := newWriteCache(1 << 20)
	key := fileKey{jobID: "job1", fileIdx: 0}

	if wc.buffered(key, 0) {
		t.Error("buffered reported true for a file with no cache entry")
	}
	wc.buffer(key, bufferedArticle{offset: 0, data: []byte("x")})
	if !wc.buffered(key, 0) {
		t.Error("buffered reported false for an article sitting in the cache")
	}
	if wc.buffered(key, 64) {
		t.Error("buffered reported true for an offset holding no article")
	}
}

// TestSyncTargetDrainReportsUntilTheCycleIsConfirmed pins WHEN Drain stops
// re-reporting an article: on the Sync that covers it, and not before.
//
// It replaces a pin that asserted the first Drain was the last — that every
// article appeared once and never again. That was the wrong boundary. A
// barrier that drains and then fails its Sync has claimed nothing, so an
// article dropped at that point is never acked by anyone; for a COMPLETED
// file the retry then drains nothing and FinalizeFile trims to a durable
// extent that sits below bytes genuinely on disk. R12 makes at-least-once the
// contract precisely so the report can survive that.
func TestSyncTargetDrainReportsUntilTheCycleIsConfirmed(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	const parts = 4
	// TotalParts is one more than what's written, so the file stays open and
	// SyncTargetFor can still reach it through the barrier's own path.
	registerFile(t, dir, files, "job1", 0, parts+1)

	opts := makeOpts(dir, files)
	opts.WriteCacheBytes = 1 << 20
	a := startAssembler(t, opts)

	for i := range int32(parts) {
		req := WriteRequest{
			JobID: "job1", FileIdx: 0, ArtIdx: i,
			MessageID: fmt.Sprintf("msg%d", i), Offset: int64(i) * 4, Data: []byte("abcd"),
		}
		if err := a.WriteArticle(t.Context(), req); err != nil {
			t.Fatalf("WriteArticle %d: %v", i, err)
		}
	}

	target := a.SyncTargetFor("job1", oneFileMap{n: 8})
	waitUntil(t, func() bool { return len(target.Files()) == 1 }, 2*time.Second, "file 0 to open")

	first, err := target.Drain(t.Context(), 0)
	if err != nil {
		t.Fatalf("first Drain: %v", err)
	}
	if len(first) != parts {
		t.Fatalf("first Drain = %v, want %d articles", first, parts)
	}

	// No Sync in between, so nothing has confirmed the first report and the
	// barrier is entitled to see it again.
	second, err := target.Drain(t.Context(), 0)
	if err != nil {
		t.Fatalf("second Drain: %v", err)
	}
	if len(second) != parts {
		t.Errorf("second Drain = %v, want the same %d articles — no Sync confirmed the "+
			"first report, so dropping it strands articles no ack will ever reach", second, parts)
	}

	if err := target.Sync(t.Context(), 0); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// A successful fsync is NOT the release point. The barrier's commit and
	// ack still follow it and can still fail, so the report has to survive
	// until Confirm says the whole cycle landed.
	third, err := target.Drain(t.Context(), 0)
	if err != nil {
		t.Fatalf("third Drain: %v", err)
	}
	if len(third) != parts {
		t.Errorf("third Drain = %v, want the same %d articles — the fsync landed but "+
			"nothing confirmed the commit or the ack, and a failure between them would "+
			"leave the retry with nothing to re-report", third, parts)
	}

	target.Confirm(t.Context(), 0)
	fourth, err := target.Drain(t.Context(), 0)
	if err != nil {
		t.Fatalf("fourth Drain: %v", err)
	}
	if len(fourth) != 0 {
		t.Errorf("fourth Drain = %v, want empty — the cycle was confirmed, so retaining "+
			"the report past it grows it without bound", fourth)
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestRetryAfterFailedWriteLandsOnDisk translates
// TestFailArticleMovesTheArticleOutOfSeenDone. The dedup lives in
// handleSuccessArticle, keyed on the FileWriter's seenDone/seenFailed maps.
// A write failure must move the article out of seenDone so its retry is not
// silently discarded as an already-accepted duplicate — it has no bytes on
// disk to be a duplicate of.
func TestRetryAfterFailedWriteLandsOnDisk(t *testing.T) {
	a := newHelperAssembler()
	dir := t.TempDir()
	f := newHelperFile(t, dir, "retry.dat", 0)

	req := WriteRequest{JobID: "job", MessageID: "msg0", ArtIdx: 0, Offset: 0, Data: []byte("first")}
	if !a.handleSuccessArticle(f, req) {
		t.Fatal("article was not accepted")
	}
	if _, ok := f.w.seenDone["msg0"]; !ok {
		t.Fatal("precondition: accepting the article should record it in seenDone")
	}

	// Force the write to fail directly, mirroring what a storage fault does.
	f.w.fail(articleID{msgID: "msg0", artIdx: 0})
	if _, stillDone := f.w.seenDone["msg0"]; stillDone {
		t.Error("the failed article is still in seenDone; its retry would take " +
			"the discard branch and never be written")
	}
	if _, failed := f.w.seenFailed["msg0"]; !failed {
		t.Error("the failed article was not recorded in seenFailed")
	}

	// The consequence the move exists for: the retry's bytes reach the file.
	retry := WriteRequest{JobID: "job", MessageID: "msg0", ArtIdx: 0, Offset: 0, Data: []byte("retry")}
	if a.handleSuccessArticle(f, retry) {
		t.Error("the retry was counted toward TotalParts a second time")
	}
	got, err := os.ReadFile(f.info.Path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "retry" {
		t.Errorf("file holds %q, want %q; the retry was discarded instead of "+
			"written, which is what leaving the seenDone entry causes", got, "retry")
	}
}

// TestFailWithoutAMessageIDIsANoOp translates
// TestFailArticleWithoutAMessageIDStillAcks. There is no ack for the queue
// to hear any more, so the only live claim left is that fail() tolerates an
// empty Message-ID — no dedup key to record — without touching the seen-set
// maps.
func TestFailWithoutAMessageIDIsANoOp(t *testing.T) {
	w := newTestFileWriter(t)
	w.fail(articleID{msgID: "", artIdx: 3})
	if len(w.seenFailed) != 0 {
		t.Errorf("seenFailed = %v; an empty Message-ID is not a dedup key", w.seenFailed)
	}
}

// TestRelievePressureForUnknownFileIsSkippedSafely covers the branch taken
// when the cache holds articles for a file relievePressure cannot find in
// the open map — a pairing nothing enforces (see writeCache's doc comment).
// Production is not believed to reach it; this exercises what happens if the
// pairing is ever broken. Before #355 this dropped only bytes; after it, and
// now again after the ack removal, it still only drops bytes — there is no
// ack left to lose alongside them — so the residual claim is that the worker
// does not panic on the missing *openFile and the pressure is actually
// relieved.
func TestRelievePressureForUnknownFileIsSkippedSafely(t *testing.T) {
	a := newHelperAssembler()
	wc := newWriteCache(10) // tiny limit so pressure trips immediately
	key := fileKey{jobID: "job1", fileIdx: 0}

	wc.buffer(key, bufferedArticle{
		offset: 0,
		data:   []byte("orphaned bytes exceeding the limit"),
		id:     articleID{msgID: "msg5", artIdx: 5},
	})
	if !wc.pressure() {
		t.Fatal("fixture did not put the cache under pressure; the test would pass vacuously")
	}

	// The pairing broken deliberately: buffered articles, no open file.
	a.relievePressure(wc, map[fileKey]*openFile{})

	if wc.pressure() {
		t.Error("relievePressure did not drain the orphaned entry")
	}
}

// TestFatalAfterAWrittenArticleCannotRetractIt replaces
// TestFatalAfterBufferedSuccessDoesNotOutraceTheDoneAck, which the plan
// authorised deleting because "the ordering it guards is gone once one
// component acks".
//
// That is true, but it was asserted rather than shown, so this shows it. The
// old race was a Done ack racing a Failed ack for the same article, with the
// loser's claim winning. Both acks are gone from this package: a success is
// reported only by appearing in Drain, and a permanent failure produces no
// report here at all — it goes to Queue.AckPermanentFailure from the pipeline.
// With one reporter and one direction, there is no second claim to outrace.
//
// What that means concretely is the assertion below: a FatalErr arriving after
// an article's bytes have landed cannot retract the Written report. It could
// not be demonstrated by inspection, because "nothing acks" is a claim about
// absence — so it is demonstrated by driving both events through the real
// worker in the losing order and reading what the barrier would see.
func TestFatalAfterAWrittenArticleCannotRetractIt(t *testing.T) {
	dir := t.TempDir()
	files := map[string]FileInfo{}
	registerFile(t, dir, files, "job1", 0, 1000) // never completes
	a := startAssembler(t, makeOpts(dir, files))

	// The article's bytes land first.
	if err := a.WriteArticle(t.Context(), WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, MessageID: "dup@x",
		Offset: 0, Data: []byte("abcd"),
	}); err != nil {
		t.Fatal(err)
	}
	// Then a permanent failure arrives for the SAME article — the order that
	// used to let a Failed ack overwrite a Done one.
	if err := a.WriteArticle(t.Context(), WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, MessageID: "dup@x",
		FatalErr: errFatalProbe,
	}); err != nil {
		t.Fatal(err)
	}

	written, err := a.SyncTargetFor("job1", oneFileMap{n: 1}).Drain(t.Context(), 0)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	var found bool
	for _, w := range written {
		if w.ArtIdx == 0 {
			found = true
		}
	}
	if !found {
		t.Error("article 0 is absent from Drain after a later FatalErr — a permanent " +
			"failure retracted bytes that are on disk, which is the ordering inversion " +
			"the deleted test guarded")
	}
}

var errFatalProbe = errors.New("permanent article failure (test)")

// TestSync_ActuallyIssuesTheFsyncAndKeepsTheReport is S1's
// syscall-level pin, and its scope is deliberately narrow.
//
// # What it pins
//
// That the fsync is ON THE PATH, and that the reported-set is discarded only
// AFTER it returns nil. Before this test, deleting w.handle.Sync() outright
// left the whole suite green — crash suite included — so the one syscall the
// entire durability design rests on was silently deletable.
//
// # What it does NOT pin, and what a reader must not conclude
//
// It does not pin fsync-to-platter. No unprivileged test can: dirty page cache
// survives process death, so a SIGKILL cannot distinguish a synced file from an
// unsynced one, and POSIX_FADV_DONTNEED / drop_caches both skip dirty pages.
// Real coverage needs a device-mapper log-writes or flakey target, which needs
// root. See docs/durability-contract.md, Accepted limitations #5, and #363.
//
// So this is "the program calls fsync in the right order", not "the bytes are
// on the platter". The second claim remains unverified in this repository.
func TestSync_ActuallyIssuesTheFsyncAndKeepsTheReport(t *testing.T) {
	w := newTestFileWriter(t)

	var syncs int
	var reportedAtSync int
	w.syncFile = func() error {
		syncs++
		// Captured DURING the syscall: if Sync cleared the set first, the
		// ordering bug would be invisible to an after-the-fact assertion.
		reportedAtSync = len(w.reported)
		return nil
	}

	if err := w.Accept(articleID{msgID: "m0", artIdx: 0}, 0, []byte("hello")); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if _, err := w.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(w.reported) == 0 {
		t.Fatal("nothing was reported before Sync; the fixture proves nothing")
	}

	if err := w.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if syncs != 1 {
		t.Errorf("fsync issued %d times, want exactly 1; the durability of every "+
			"ack rests on this syscall being on the path", syncs)
	}
	if reportedAtSync == 0 {
		t.Error("the reported set was already empty when fsync was called; it must be " +
			"discarded only AFTER a successful fsync, or a failed sync loses the report")
	}
	if len(w.reported) == 0 {
		t.Error("the reported set was released by the fsync. The barrier's commit and " +
			"ack follow it, so a failure between them would leave the retry with " +
			"nothing to re-report; Confirm is the release point")
	}
	w.Confirm()
	if len(w.reported) != 0 {
		t.Errorf("reported set still holds %d after Confirm, want 0", len(w.reported))
	}

	// A FAILING fsync must keep the report, or the next Drain has nothing to
	// re-report and a retried finalize truncates below bytes that are on disk.
	if err := w.Accept(articleID{msgID: "m1", artIdx: 1}, 5, []byte("world")); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if _, err := w.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	w.syncFile = func() error { return syscall.EIO }
	if err := w.Sync(context.Background()); err == nil {
		t.Fatal("Sync returned nil despite a failing fsync")
	}
	if len(w.reported) == 0 {
		t.Error("a FAILED fsync discarded the reported set; the next Drain would " +
			"re-report nothing and FinalizeFile would trim below real bytes")
	}
}

// TestNewFileWriter_BindsSyncFileToTheRealHandle closes the one level of hole
// the syncFile seam still had.
//
// TestSync_ActuallyIssuesTheFsyncAndKeepsTheReport pins that Sync
// CALLS w.syncFile in the right order. It says nothing about what syncFile
// holds, because that test installs its own stub. So rebinding
// `w.syncFile = handle.Sync` to `func() error { return nil }` in newFileWriter
// compiled and left the ENTIRE suite green — the same "delete the fsync and
// nothing notices" hole that motivated the seam, one level down.
//
// This is a problem the writeAt seam does not have. Its default binding is
// pinned implicitly by every test that writes through and asserts bytes on
// disk. A successful fsync has no observable effect at all, so nothing pins
// syncFile's binding implicitly and it has to be asserted directly.
//
// The observable used here is failure, not success: with the handle closed, a
// syncFile bound to the real *os.File returns ErrClosed, while any stand-in
// that ignores the handle returns nil. That distinguishes the binding without
// needing to observe a successful fsync, which is not observable.
func TestNewFileWriter_BindsSyncFileToTheRealHandle(t *testing.T) {
	w := newTestFileWriter(t)

	// Control: while the handle is open the real fsync succeeds. Without this,
	// a syncFile that ALWAYS errored would satisfy the assertion below.
	if err := w.syncFile(); err != nil {
		t.Fatalf("syncFile on an open handle = %v, want nil", err)
	}

	if err := w.handle.Close(); err != nil {
		t.Fatalf("close handle: %v", err)
	}
	if err := w.syncFile(); err == nil {
		t.Error("syncFile returned nil after its file handle was closed; it is not " +
			"bound to the real *os.File, so the fsync every ack depends on is not " +
			"actually being issued")
	}
}
