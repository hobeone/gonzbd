package assembler

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// An article's Done ack is a claim that no later run can check: once the queue
// holds the bit, ForEachUnfinishedArticle skips the article forever and
// ResetForRetry does not clear it, because it clears done only where failed is
// set. So the ack has to wait for the bytes (#355).
//
// Two settings make a test on this surface vacuous by omission, and both are
// easy to lose by copying a neighbouring test:
//
//   - makeOpts leaves WriteCacheBytes at zero, which disables the cache. With
//     no cache there is no deferred write and no bug to reach.
//   - makeOpts wires no MarkArticles* callbacks, which makes recordPendingDone
//     a no-op and flush an early return, so every assertion about acks holds
//     trivially.
//
// newAckFixture sets both, which is why every test here goes through it. That
// is necessary rather than sufficient: a test can set both and still not
// distinguish the fixed code from the broken code —
// TestCompletedFileAcksEveryArticleExactlyOnce is one, and says so. What
// settles whether a test is a pin is reverting the fix and watching it fail,
// never the fixture it was built on.

// ackRecorder captures the batched acks the assembler hands to the queue.
type ackRecorder struct {
	done   []int32
	failed []int32
}

// opts returns Options wired to record acks, with the write cache at the given
// size. Zero disables coalescing, which is the uncached path.
func (r *ackRecorder) opts(cacheBytes int64) Options {
	return Options{
		FileInfo: func(string, int) (FileInfo, error) { return FileInfo{}, nil },
		MarkArticlesDoneByIdx: func(_ string, idxs []int32) error {
			r.done = append(r.done, idxs...)
			return nil
		},
		MarkArticlesFailedByIdx: func(_ string, idxs []int32) ([]int32, error) {
			r.failed = append(r.failed, idxs...)
			return nil, nil
		},
		WriteCacheBytes: cacheBytes,
	}
}

// ackFixture is an assembler exercised through the worker's own helpers on the
// test goroutine. It is deliberately never Started: those helpers are
// single-goroutine by contract, so calling them directly needs no locking and
// no wall-clock waiting. A negative assertion driven by the 250ms flush ticker
// would be the shape this repository already has flake history with.
type ackFixture struct {
	a    *Assembler
	rec  *ackRecorder
	wc   *writeCache
	f    *openFile
	key  fileKey
	open map[fileKey]*openFile
}

// newAckFixture builds an assembler, a write cache of the given size, and one
// open file. When closed is true the file's handle is closed first, which makes
// every WriteAt on it fail — the injection resume_crc_test.go established.
func newAckFixture(t *testing.T, cacheBytes int64, closed bool) *ackFixture {
	t.Helper()
	rec := &ackRecorder{}
	key := fileKey{jobID: "job1", fileIdx: 0}
	path := filepath.Join(t.TempDir(), "out.bin")
	fh, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if closed {
		if err := fh.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	} else {
		t.Cleanup(func() { _ = fh.Close() })
	}
	f := &openFile{
		handle:   fh,
		info:     FileInfo{Path: path},
		key:      key,
		seenDone: make(map[string]int64),
		crcValid: true,
	}
	return &ackFixture{
		a:    New(rec.opts(cacheBytes), nil),
		rec:  rec,
		wc:   newWriteCache(cacheBytes),
		f:    f,
		key:  key,
		open: map[fileKey]*openFile{key: f},
	}
}

// settle flushes the pending batches and returns every ack seen so far, sorted
// so assertions do not depend on map iteration order.
func (x *ackFixture) settle() (done, failed []int32) {
	x.a.flush()
	done = slices.Clone(x.rec.done)
	failed = slices.Clone(x.rec.failed)
	slices.Sort(done)
	slices.Sort(failed)
	return done, failed
}

// accept pushes one successful article through the accept path.
func (x *ackFixture) accept(t *testing.T, artIdx int32, offset int64, data []byte) bool {
	t.Helper()
	return x.a.handleSuccessArticle(x.f, articleReq(x.key, artIdx, offset, data), x.wc, x.open, x.key)
}

func articleReq(key fileKey, artIdx int32, offset int64, data []byte) WriteRequest {
	return WriteRequest{
		JobID:     key.jobID,
		FileIdx:   key.fileIdx,
		ArtIdx:    artIdx,
		MessageID: fmt.Sprintf("msg%d", artIdx),
		Offset:    offset,
		Data:      data,
		CRC:       uint32(artIdx) + 1,
	}
}

// TestBufferedArticleIsNotAckedUntilItsBytesLand is the crash case. It needs no
// I/O error: the flush timer and the queue's checkpoint run on schedules with
// no relation to the write cache, so an ack published while the bytes are
// buffered can be persisted and then lost.
func TestBufferedArticleIsNotAckedUntilItsBytesLand(t *testing.T) {
	x := newAckFixture(t, 1<<20, false)

	// Well under contiguousRunSize, so nothing is written yet.
	if !x.accept(t, 0, 0, []byte("buffered bytes")) {
		t.Fatal("handleSuccessArticle returned false for a fresh article")
	}

	done, failed := x.settle()
	if len(done) != 0 {
		t.Errorf("article %v acked Done while still buffered; a crash here loses "+
			"the bytes and keeps the ack, and no later run re-dispatches it (#355)", done)
	}
	if len(failed) != 0 {
		t.Errorf("article %v acked Failed while merely buffered; nothing failed", failed)
	}

	// Draining is what actually puts the bytes on disk.
	x.a.drainCacheForFile(x.wc, x.f, x.key)

	done, failed = x.settle()
	if !slices.Equal(done, []int32{0}) {
		t.Errorf("after the drain wrote the bytes, done acks = %v, want [0]", done)
	}
	if len(failed) != 0 {
		t.Errorf("successful drain produced failure acks %v", failed)
	}
}

// TestFailedCoalescedRunFailsEveryArticleInTheRun pins the case #355's Scope
// section wrongly cleared. buildContiguousRun coalesces every article
// contiguous with the write cursor into one buffer and pools the originals
// before flushRun attempts the write, so a single failing WriteAt loses all of
// them — not just the one whose arrival triggered the flush.
func TestFailedCoalescedRunFailsEveryArticleInTheRun(t *testing.T) {
	x := newAckFixture(t, 8<<20, true) // closed handle: the run's WriteAt fails

	// Six contiguous articles totalling more than contiguousRunSize, so the
	// last one triggers a coalesced flush covering all six.
	const (
		artCount = 6
		artSize  = 100_000
	)
	if artCount*artSize <= contiguousRunSize {
		t.Fatalf("fixture does not reach the coalescing threshold: %d <= %d",
			artCount*artSize, contiguousRunSize)
	}
	for i := range int32(artCount) {
		if !x.accept(t, i, int64(i)*artSize, make([]byte, artSize)) {
			t.Fatalf("handleSuccessArticle returned false for article %d", i)
		}
	}

	done, failed := x.settle()
	want := []int32{0, 1, 2, 3, 4, 5}
	if !slices.Equal(failed, want) {
		t.Errorf("failed acks = %v, want %v.\n"+
			"Every article in a coalesced run loses its bytes when the run's "+
			"WriteAt fails: they were merged into one buffer and pooled before "+
			"the write. Acking only the triggering article leaves the rest "+
			"marked Done with no bytes and no way to fetch them again (#355).", failed, want)
	}
	if len(done) != 0 {
		t.Errorf("done acks = %v after the run failed; the file holds none of "+
			"those bytes", done)
	}
	if x.f.crcValid {
		t.Error("crcValid survived a failed coalesced run")
	}
}

// TestFailedRunClearsSeenDone pins the half of the failure path that is not an
// ack. seenDone means "accepted and counted toward TotalParts", so leaving a
// lost article there lets the duplicate branch re-emit a Done for bytes the
// file does not have.
func TestFailedRunClearsSeenDone(t *testing.T) {
	x := newAckFixture(t, 8<<20, true)

	for i := range int32(6) {
		x.accept(t, i, int64(i)*100_000, make([]byte, 100_000))
	}

	if len(x.f.seenDone) != 0 {
		t.Errorf("seenDone still holds %d articles after their run failed; a "+
			"later duplicate would be re-acked Done over bytes that are not "+
			"on disk", len(x.f.seenDone))
	}
	if len(x.f.seenFailed) != 6 {
		t.Errorf("seenFailed holds %d articles, want 6", len(x.f.seenFailed))
	}
}

// TestDuplicateSuccessWhileBufferedIsNotReAcked covers the guard on the
// duplicate branch's re-emit. seenDone means the article was accepted, not that
// the file holds it, so re-emitting a Done there would restore exactly the
// premature ack this change removes — for an article whose first copy is still
// sitting in memory.
func TestDuplicateSuccessWhileBufferedIsNotReAcked(t *testing.T) {
	x := newAckFixture(t, 1<<20, false)

	if !x.accept(t, 0, 0, []byte("first copy")) {
		t.Fatal("first copy was not accepted")
	}
	// A second copy of the same Message-ID, while the first is still buffered.
	if x.accept(t, 0, 0, []byte("second copy")) {
		t.Error("a duplicate must not be counted toward TotalParts")
	}

	if done, _ := x.settle(); len(done) != 0 {
		t.Errorf("duplicate re-emitted a Done ack (%v) while the first copy's "+
			"bytes were still buffered", done)
	}

	// Once the bytes are on disk the re-emit is correct again: it exists so a
	// dropped flush can be recovered.
	x.a.drainCacheForFile(x.wc, x.f, x.key)
	if done, _ := x.settle(); !slices.Equal(done, []int32{0}) {
		t.Errorf("done acks = %v after the drain, want [0]", done)
	}

	x.accept(t, 0, 0, []byte("third copy"))
	if done, _ := x.settle(); !slices.Equal(done, []int32{0, 0}) {
		t.Errorf("done acks = %v; a duplicate arriving after the bytes landed "+
			"must still re-emit, so a dropped flush can be recovered", done)
	}
}

// TestDuplicateAtADifferentOffsetIsNotReAcked is the same guard against a
// duplicate that does not agree with the first copy about where the article
// belongs.
//
// The dedup is keyed on the Message-ID, so a second copy reporting a different
// yEnc offset still reaches the duplicate branch. Asking the cache about *that*
// offset answers a question about a slot the first copy never occupied — it is
// empty, so the guard would read "already written" and re-emit a Done for bytes
// still sitting in memory. seenDone records the offset the article was accepted
// at so the lookup asks about the right one.
func TestDuplicateAtADifferentOffsetIsNotReAcked(t *testing.T) {
	x := newAckFixture(t, 1<<20, false)

	if !x.accept(t, 0, 0, []byte("first copy")) {
		t.Fatal("first copy was not accepted")
	}

	// Same Message-ID, different offset, while the first copy is buffered.
	dup := articleReq(x.key, 0, 4096, []byte("same article, other offset"))
	if x.a.handleSuccessArticle(x.f, dup, x.wc, x.open, x.key) {
		t.Error("a duplicate must not be counted toward TotalParts")
	}

	if done, _ := x.settle(); len(done) != 0 {
		t.Errorf("done acks = %v; the guard asked the cache about offset 4096, "+
			"which the first copy never occupied, and read the empty slot as "+
			"'already written' while its bytes were still buffered", done)
	}
}

// TestFatalAfterBufferedSuccessDoesNotOutraceTheDoneAck covers the cross-state
// case: an article received successfully and then reported fatal.
//
// This used to resolve as Done by a timing accident. Both acks landed in one
// flush, which publishes the Done batch before the Failed batch, and markDone
// and markFailed are first-writer-wins. Deferring the Done ack removes that
// accident, so the failure has to be suppressed explicitly or it wins and
// charges a file that is complete on disk as damaged.
func TestFatalAfterBufferedSuccessDoesNotOutraceTheDoneAck(t *testing.T) {
	x := newAckFixture(t, 1<<20, false)

	if !x.accept(t, 0, 0, []byte("bytes that are fine")) {
		t.Fatal("article was not accepted")
	}

	fatal := articleReq(x.key, 0, 0, nil)
	fatal.FatalErr = errors.New("article unavailable on all servers")
	x.a.handleFatalArticle(x.f, fatal)

	if _, failed := x.settle(); len(failed) != 0 {
		t.Errorf("failed acks = %v for an article whose bytes are buffered and "+
			"about to be written; markFailed is first-writer-wins, so this "+
			"permanently marks a complete file damaged", failed)
	}

	x.a.drainCacheForFile(x.wc, x.f, x.key)

	done, failed := x.settle()
	if !slices.Equal(done, []int32{0}) {
		t.Errorf("done acks = %v after the bytes landed, want [0]", done)
	}
	if len(failed) != 0 {
		t.Errorf("failed acks = %v after a successful write", failed)
	}
}

// TestFailedDrainedWriteFailsTheArticle covers the two deferred sites the issue
// names. They reach writeCachedArticles by different routes — one from file
// completion, one from memory pressure — so one fixture cannot stand in for
// both.
func TestFailedDrainedWriteFailsTheArticle(t *testing.T) {
	tests := []struct {
		name  string
		drain func(x *ackFixture)
	}{
		{
			name:  "completion drain",
			drain: func(x *ackFixture) { x.a.drainCacheForFile(x.wc, x.f, x.key) },
		},
		{
			name:  "pressure flush",
			drain: func(x *ackFixture) { x.a.flushPressure(x.wc, x.open) },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			x := newAckFixture(t, 1<<20, true) // closed handle: the drained write fails

			x.accept(t, 3, 0, []byte("bytes that never land"))
			if done, _ := x.settle(); len(done) != 0 {
				t.Fatalf("article acked Done at accept time: %v", done)
			}

			tc.drain(x)

			done, failed := x.settle()
			if !slices.Equal(failed, []int32{3}) {
				t.Errorf("failed acks = %v, want [3]; a drained write that fails "+
					"must leave the article re-fetchable, not Done", failed)
			}
			if len(done) != 0 {
				t.Errorf("done acks = %v after the drained write failed", done)
			}
		})
	}
}

// TestDisplacedArticleIsFailed covers buffer's duplicate-offset branch. It is
// near-unreachable behind the seen-set dedup, but under a deferred ack an
// article silently evicted from the cache is one the queue never hears about
// again in either direction.
func TestDisplacedArticleIsFailed(t *testing.T) {
	x := newAckFixture(t, 1<<20, false)

	// Two different Message-IDs claiming the same offset. Upstream dedup keys
	// on the Message-ID, so it does not catch this; the cache's own
	// replacement branch is what sees it.
	if !x.accept(t, 7, 0, []byte("first")) {
		t.Fatal("first article was not accepted")
	}
	if !x.accept(t, 8, 0, []byte("second")) {
		t.Fatal("second article was not accepted")
	}

	done, failed := x.settle()
	if !slices.Equal(failed, []int32{7}) {
		t.Errorf("failed acks = %v, want [7]; the displaced article's bytes were "+
			"dropped and nothing else will write them, so leaving it unacked "+
			"would strand it in neither state", failed)
	}
	if len(done) != 0 {
		t.Errorf("done acks = %v while the surviving article is still buffered", done)
	}
	if x.f.crcValid {
		t.Error("crcValid survived a displacement; the evicted article's CRC " +
			"part was recorded at accept time and describes a length the " +
			"replacement does not have to match")
	}
	if _, still := x.f.seenDone["msg7"]; still {
		t.Error("the displaced article is still in seenDone")
	}
}

// TestWriteOutcomesSettleTheirAcks pins each outcome to the ack it owes. Go
// does not check a switch over a named type for exhaustiveness and this
// repository enables no linter that does, so this table is the guard the type
// itself cannot provide.
func TestWriteOutcomesSettleTheirAcks(t *testing.T) {
	tests := []struct {
		name        string
		cacheBytes  int64
		closed      bool
		wantOutcome writeOutcome
		wantDone    []int32
		wantFailed  []int32
	}{
		{
			name:        "uncached write lands",
			wantOutcome: outcomeDurable,
			wantDone:    []int32{0},
		},
		{
			name:        "uncached write fails",
			closed:      true,
			wantOutcome: outcomeFailed,
			wantFailed:  []int32{0},
		},
		{
			name:        "buffered write defers both acks",
			cacheBytes:  1 << 20,
			wantOutcome: outcomeDeferred,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The outcome value and the acks it settles are checked on
			// separate fixtures: writeArticleOrBuffer consumes the request's
			// buffer, so one request cannot feed both calls.
			x := newAckFixture(t, tc.cacheBytes, tc.closed)
			req := articleReq(x.key, 0, 0, []byte("payload"))
			if got := x.a.writeArticleOrBuffer(x.f, x.key, req, x.wc, x.open); got != tc.wantOutcome {
				t.Fatalf("outcome = %v, want %v", got, tc.wantOutcome)
			}

			y := newAckFixture(t, tc.cacheBytes, tc.closed)
			y.a.writeAndSettle(y.f, y.key, articleReq(y.key, 0, 0, []byte("payload")), y.wc, y.open)

			done, failed := y.settle()
			if !slices.Equal(done, tc.wantDone) {
				t.Errorf("done acks = %v, want %v", done, tc.wantDone)
			}
			if !slices.Equal(failed, tc.wantFailed) {
				t.Errorf("failed acks = %v, want %v", failed, tc.wantFailed)
			}
		})
	}
}

// TestDrainedArticlesForUnknownFileAreFailed covers the two branches that fire
// when the cache holds articles for a file that is no longer open.
//
// Production is not believed to reach them: every removal from the open-file
// map is paired with a forget or a drain in the same call, with no yield to the
// request channel in between. Nothing enforces that pairing, which is why both
// sites still log. A unit test reaches them by building the two structures
// independently, which is the only way to check what happens if it is broken.
//
// What happens matters more than it used to. These branches used to drop only
// the bytes; now they would drop the articles' acks with them, and an article
// acked neither Done nor Failed is one no later run re-dispatches.
func TestDrainedArticlesForUnknownFileAreFailed(t *testing.T) {
	tests := []struct {
		name  string
		flush func(a *Assembler, wc *writeCache, open map[fileKey]*openFile)
	}{
		{
			name: "pressure flush",
			flush: func(a *Assembler, wc *writeCache, open map[fileKey]*openFile) {
				a.flushPressure(wc, open)
			},
		},
		{
			name: "shutdown flush",
			flush: func(a *Assembler, wc *writeCache, open map[fileKey]*openFile) {
				a.flushWriteCache(wc, open)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			x := newAckFixture(t, 1<<20, false)

			cached, _ := x.wc.buffer(x.key, bufferedArticle{
				offset: 0,
				data:   []byte("orphaned"),
				id:     articleID{msgID: "msg5", artIdx: 5},
			})
			if !cached {
				t.Fatal("article was not buffered")
			}

			// The pairing broken deliberately: buffered articles, no open file.
			tc.flush(x.a, x.wc, map[fileKey]*openFile{})

			done, failed := x.settle()
			if !slices.Equal(failed, []int32{5}) {
				t.Errorf("failed acks = %v, want [5]; an article whose bytes are "+
					"discarded must stay re-fetchable rather than being left "+
					"unacked forever", failed)
			}
			if len(done) != 0 {
				t.Errorf("done acks = %v for an article that was never written", done)
			}
		})
	}
}

// TestBufferedReportsUnknownKey covers the lookup that tells an article still
// waiting in the cache from one already written and acked.
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

// TestCompletedFileAcksEveryArticleExactlyOnce is a regression guard, not a pin
// on #355: it passes against the unfixed code too, since acking at accept time
// also acks each article exactly once. What it catches is a fix that acks in
// both places, and it covers the ordering the unit tests above cannot —
// finalizeFile drains before the flush that publishes the acks, so deferring
// them does not delay a file's completion past its own callback.
func TestCompletedFileAcksEveryArticleExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	const parts = 4
	path := registerFile(t, dir, files, "job1", 0, parts)

	var rec ackRecorder
	opts := makeOpts(dir, files)
	opts.WriteCacheBytes = 1 << 20
	completed := make(chan struct{}, 1)
	opts.MarkArticlesDoneByIdx = func(_ string, idxs []int32) error {
		rec.done = append(rec.done, idxs...)
		return nil
	}
	opts.MarkArticlesFailedByIdx = func(_ string, idxs []int32) ([]int32, error) {
		rec.failed = append(rec.failed, idxs...)
		return nil, nil
	}
	opts.OnFileComplete = func(string, int, uint32) { completed <- struct{}{} }
	opts.DoneFlushInterval = -1 // no timer; completion and Stop are the only flushes

	a := startAssembler(t, opts)
	for i := range int32(parts) {
		req := articleReq(fileKey{jobID: "job1", fileIdx: 0}, i, int64(i)*4, []byte("abcd"))
		if err := a.WriteArticle(t.Context(), req); err != nil {
			t.Fatalf("WriteArticle %d: %v", i, err)
		}
	}

	<-completed
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	got := slices.Clone(rec.done)
	slices.Sort(got)
	if want := []int32{0, 1, 2, 3}; !slices.Equal(got, want) {
		t.Errorf("done acks = %v, want %v exactly once each", got, want)
	}
	if len(rec.failed) != 0 {
		t.Errorf("failure acks %v on a clean run", rec.failed)
	}
	if got := readFile(t, path); len(got) != parts*4 {
		t.Errorf("file is %d bytes, want %d", len(got), parts*4)
	}
}
