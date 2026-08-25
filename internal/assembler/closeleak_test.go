package assembler

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"testing"
)

// The faulted set at Close, and the branches that exist so it cannot be
// dropped in silence.
//
// Most tests here install the set by hand. That is not a shortcut around a
// reachable state: on every path but one the set is empty at both call sites,
// because each producer is drained before the worker returns to its select
// loop. Those pin the CONTRACT — if a future change adds a producer that
// nothing drains, the articles are reported rather than lost.
//
// The exception is the cancel arm's KeepFiles branch, where a FAILED drain
// rolls articles back into the set and the arm deliberately skips the
// releaseFaulted that would empty it again. That one is reachable, and
// TestDispatchRequest_CancelKeepingFilesReportsAFailedDrainWithoutTripping
// reaches it for real rather than installing anything.

// TestFileWriter_CloseHandsBackTheUnroutedFaultedSet is the pin on the return
// value itself.
//
// An article left in w.faulted when the writer goes away is neither Done, nor
// Failed, nor Outstanding: its Emitted bit is still set from dispatch and
// ForEachUnfinishedArticle skips a set Emitted bit, so nothing re-dispatches it
// until a restart clears the bits. Close is the last moment anything can be
// told about it.
func TestFileWriter_CloseHandsBackTheUnroutedFaultedSet(t *testing.T) {
	w := newTestFileWriter(t)
	w.admitAccepted(1)
	w.fail(articleID{msgID: "a1", artIdx: 1})

	leaked, err := w.Close()
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if len(leaked) != 1 || leaked[0].id.msgID != "a1" {
		t.Fatalf("Close() leaked = %v, want a1 — an article the writer takes to the "+
			"grave keeps its Emitted bit and is never re-dispatched", leaked)
	}
}

// TestFileWriter_CloseTakesTheFaultedSetRatherThanReadingIt pins the half that
// a "return w.faulted" implementation would get wrong.
//
// Each set must be routed exactly once. If Close returned the slice without
// clearing it, a caller that both routes the return value and reaches for the
// writer again would report the same article twice, and the second report
// clears an Emitted bit a later dispatch legitimately set.
func TestFileWriter_CloseTakesTheFaultedSetRatherThanReadingIt(t *testing.T) {
	w := newTestFileWriter(t)
	w.admitAccepted(2)
	w.fail(articleID{msgID: "a2", artIdx: 2})

	// The error is not the subject here — the test above pins it — and this
	// one asserts only that the SET is taken.
	if leaked, _ := w.Close(); len(leaked) != 1 {
		t.Fatalf("Close() leaked = %v, want one article", leaked)
	}
	if again := w.takeFaulted(); len(again) != 0 {
		t.Errorf("takeFaulted() = %v after Close, want empty — Close must TAKE the set, "+
			"or an article can be routed twice", again)
	}
}

// TestRouteFaulted_SplitsTheSetByDisposition pins the routing function
// directly, on a set holding both dispositions at once.
//
// The tests below reach it through drainAndClose with a single article, so
// each covers one arm in isolation. A real rolled-back set is not homogeneous
// — a coalesced run whose write failed can carry a displaced article alongside
// articles that were merely never attempted — and the two must not be routed
// to the same place.
func TestRouteFaulted_SplitsTheSetByDisposition(t *testing.T) {
	a := newHelperAssembler()
	var unwritten []int32
	var rejected []int32
	a.opts.OnArticlesUnwritten = func(_ string, _ int, arts []int32) {
		unwritten = append(unwritten, arts...)
	}
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		rejected = append(rejected, artIdx)
	}

	a.routeFaulted([]faultedArticle{
		{id: articleID{msgID: "n1", artIdx: 1}},
		{id: articleID{msgID: "d2", artIdx: 2}, displaced: true},
		{id: articleID{msgID: "n3", artIdx: 3}},
	}, "job", 0, "/tmp/target.dat")

	if len(unwritten) != 2 || unwritten[0] != 1 || unwritten[1] != 3 {
		t.Errorf("unwritten = %v, want [1 3] — an article that was never attempted "+
			"returns to Outstanding and is re-fetched", unwritten)
	}
	if len(rejected) != 1 || rejected[0] != 2 {
		t.Errorf("rejected = %v, want [2] — a displaced article cannot be re-fetched "+
			"without reproducing the collision that displaced it", rejected)
	}
}

// TestRouteFaulted_IgnoresAnEmptySet pins the degenerate input, which is the
// case that actually runs: the set is empty at both Close call sites on every
// reachable path, so this is the branch production takes.
func TestRouteFaulted_IgnoresAnEmptySet(t *testing.T) {
	a := newHelperAssembler()
	a.opts.OnArticlesUnwritten = func(_ string, _ int, arts []int32) {
		t.Errorf("OnArticlesUnwritten called with %v for an empty set", arts)
	}
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		t.Errorf("OnArticleRejected called with %d for an empty set", artIdx)
	}

	a.routeFaulted(nil, "job", 0, "/tmp/target.dat")
}

// TestDrainAndClose_RoutesAFaultedSetThatSurvivedTheDrain covers the tripwire
// branch in drainAndClose.
//
// The releaseFaulted inside drainAndClose runs before Sync and Close, and
// neither of those can append, so this branch is unreachable through public
// behaviour. It exists because Close is where the writer stops existing: on
// this path the file is being closed normally and its articles are still
// wanted, so they are routed rather than dropped.
func TestDrainAndClose_RoutesAFaultedSetThatSurvivedTheDrain(t *testing.T) {
	a := newHelperAssembler()
	var routed []int32
	a.opts.OnArticlesUnwritten = func(_ string, _ int, arts []int32) {
		routed = append(routed, arts...)
	}

	f := newHelperFile(t, t.TempDir(), "close.dat", 0)
	// Installed through the syncFile seam, which is the ONLY way to reach the
	// branch under test. drainAndClose runs releaseFaulted before Sync, so a
	// set installed up front is drained and routed by that call instead, and
	// the assertion below then passes without the Close arm ever executing —
	// which is exactly what it did before this was corrected.
	f.w.syncFile = func() error {
		f.w.faulted = []faultedArticle{{id: articleID{msgID: "a3", artIdx: 3}}}
		return nil
	}

	if err := a.drainAndClose(f); err != nil {
		t.Fatalf("drainAndClose() error = %v", err)
	}

	if len(routed) != 1 || routed[0] != 3 {
		t.Errorf("routed = %v, want [3] — an article still in the faulted set when the "+
			"writer is closed has to be returned to Outstanding, or it is stranded for "+
			"the life of the process", routed)
	}
}

// TestDrainAndClose_RoutesADisplacedArticleAsPermanentlyFailed pins the other
// arm of the same disposition, because the two are not interchangeable.
//
// A displaced article must NOT go back to Outstanding: re-fetching it
// reproduces the collision, and the re-fetched copy displaces the article that
// displaced it. It is resolved permanently failed instead.
func TestDrainAndClose_RoutesADisplacedArticleAsPermanentlyFailed(t *testing.T) {
	a := newHelperAssembler()
	var unwritten []int32
	var rejected []int32
	a.opts.OnArticlesUnwritten = func(_ string, _ int, arts []int32) {
		unwritten = append(unwritten, arts...)
	}
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		rejected = append(rejected, artIdx)
	}

	f := newHelperFile(t, t.TempDir(), "close-displaced.dat", 0)
	// Through the syncFile seam, for the reason the test above records: an
	// up-front set is consumed by drainAndClose's own releaseFaulted and never
	// reaches the Close arm.
	f.w.syncFile = func() error {
		f.w.faulted = []faultedArticle{{id: articleID{msgID: "a4", artIdx: 4}, displaced: true}}
		return nil
	}

	if err := a.drainAndClose(f); err != nil {
		t.Fatalf("drainAndClose() error = %v", err)
	}

	if len(rejected) != 1 || rejected[0] != 4 {
		t.Errorf("rejected = %v, want [4] — a displaced article is resolved permanently "+
			"failed", rejected)
	}
	if len(unwritten) != 0 {
		t.Errorf("unwritten = %v, want none — returning a displaced article to "+
			"Outstanding produces a ping-pong that never settles", unwritten)
	}
}

// TestDispatchRequest_CancelDropsTheFaultedSetButReportsIt covers the OTHER
// Close call site, which had no test at all: the branch is reachable only
// through the cancel control message, and the coverage profile showed it never
// executed.
//
// The two dispositions are deliberately different and neither implies the
// other. drainAndClose routes its set — that file is closing normally and its
// articles are still wanted. The cancel arm must NOT: its cache is forgotten
// and the job is leaving the queue, so returning the articles to Outstanding
// re-dispatches work for a job that is going away. That holds under both
// dispositions — keeping a removed job's bytes does not make its articles
// wanted again — though this test exercises DeleteFiles, where the file is
// unlinked as well.
//
// What it owes instead is a report. At Error here: with no drain error the
// set is empty on every reachable path, so a non-empty one means a producer
// was added that nothing drains. The KeepFiles path can populate it
// legitimately (a failed Drain rolls articles back and the arm skips the
// releaseFaulted that would empty it again), and reports at Warn instead.
func TestDispatchRequest_CancelDropsTheFaultedSetButReportsIt(t *testing.T) {
	var logs bytes.Buffer
	a := newHelperAssembler()
	a.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError}))
	a.opts.OnArticlesUnwritten = func(_ string, _ int, arts []int32) {
		t.Errorf("OnArticlesUnwritten(%v) on the cancel path — the job is leaving the "+
			"queue, so returning its articles to Outstanding re-dispatches work for a "+
			"job that is going away", arts)
	}
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		t.Errorf("OnArticleRejected(%d) on the cancel path", artIdx)
	}

	f := newHelperFile(t, t.TempDir(), "cancelled.dat", 0)
	f.w.faulted = []faultedArticle{{id: articleID{msgID: "a5", artIdx: 5}}}
	key := f.w.key
	open := map[fileKey]*openFile{key: f}
	completed := map[fileKey]struct{}{}

	a.dispatchRequest(
		// ackCh is what marks this a control message; CancelJob always sets
		// one, and the worker discriminates on it rather than on the sentinel
		// alone so that no request built outside the package can pose as one.
		//
		// disposition must be stated because this builds the request by hand
		// instead of going through CancelJob, and its zero value is KeepFiles
		// — the non-destructive default the exported API wants, which is the
		// opposite of what the os.Stat below asserts.
		WriteRequest{
			FileIdx:     fileIdxCancelJob,
			MessageID:   key.jobID,
			ackCh:       make(chan error, 1),
			disposition: DeleteFiles,
		},
		open, completed, map[string]struct{}{}, newWriteCache(0))

	if _, still := open[key]; still {
		t.Error("the cancelled job's file is still in the open map")
	}
	if _, err := os.Stat(f.info.Path); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%s) = %v, want not-exist — a cancelled job's partial file "+
			"must be removed", f.info.Path, err)
	}
	if got := f.w.takeFaulted(); len(got) != 0 {
		t.Errorf("takeFaulted() = %v after the cancel — Close must TAKE the set, or a "+
			"later reader routes an article this drop already accounted for", got)
	}
	if !strings.Contains(logs.String(), "artidxs") || !strings.Contains(logs.String(), "5") {
		t.Errorf("the dropped articles were not reported by index; log was:\n%s\n"+
			"this arm DROPS the set, so the log line is the only record of which "+
			"articles were stranded", logs.String())
	}
}

// TestDispatchRequest_CancelKeepingFilesStillDropsTheFaultedSet is the
// KeepFiles half of the test above, and it is the reason the keep branch calls
// f.w.Drain rather than a.drainAndClose.
//
// drainAndClose calls releaseFaulted and routes whatever Close returns, which
// would fire both callbacks below. That is right for its own caller, where the
// file is going to post-processing and its articles are still wanted. It is
// wrong here for the same reason it is wrong on the DeleteFiles path: the job
// has left the queue, so returning its articles to Outstanding re-dispatches
// work for a job that is going away. Keeping the bytes does not change that.
//
// Without this test the substitution is invisible — every other KeepFiles
// assertion passes with drainAndClose in place.
func TestDispatchRequest_CancelKeepingFilesStillDropsTheFaultedSet(t *testing.T) {
	a := newHelperAssembler()
	a.opts.OnArticlesUnwritten = func(_ string, _ int, arts []int32) {
		t.Errorf("OnArticlesUnwritten(%v) on the KeepFiles cancel path — the job is "+
			"leaving the queue whether or not its bytes are kept, so returning its "+
			"articles to Outstanding re-dispatches work for a job that is going away", arts)
	}
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		t.Errorf("OnArticleRejected(%d) on the KeepFiles cancel path", artIdx)
	}

	f := newHelperFile(t, t.TempDir(), "kept.dat", 0)
	f.w.faulted = []faultedArticle{{id: articleID{msgID: "a5", artIdx: 5}}}
	key := f.w.key
	open := map[fileKey]*openFile{key: f}

	a.dispatchRequest(
		WriteRequest{
			FileIdx:     fileIdxCancelJob,
			MessageID:   key.jobID,
			ackCh:       make(chan error, 1),
			disposition: KeepFiles,
		},
		open, map[fileKey]struct{}{}, map[string]struct{}{}, newWriteCache(0))

	if _, still := open[key]; still {
		t.Error("the cancelled job's file is still in the open map under KeepFiles — " +
			"the handle must be released whatever happens to the bytes")
	}
	if _, err := os.Stat(f.info.Path); err != nil {
		t.Errorf("os.Stat(%s) = %v, want the file to exist — KeepFiles suppresses the "+
			"unlink, and this is the assertion that separates it from DeleteFiles",
			f.info.Path, err)
	}
	if got := f.w.takeFaulted(); len(got) != 0 {
		t.Errorf("takeFaulted() = %v after the cancel — Close must TAKE the set, or a "+
			"later reader routes an article this drop already accounted for", got)
	}
}

// TestDispatchRequest_CancelKeepingFilesReportsAFailedDrainWithoutTripping is
// the one test in this file that reaches a non-empty faulted set for real
// instead of installing one.
//
// The KeepFiles branch drains before closing. When that drain fails, Drain
// calls w.fail on every article it did not attempt, which appends to
// w.faulted, and the arm deliberately skips the releaseFaulted that would
// empty it again. So the set is legitimately non-empty at Close on this path —
// the only path where that is true.
//
// The tripwire must therefore not fire. Its Error line accuses a future change
// of adding "a producer that nothing drains"; here the producer is the drain
// this very branch performs, and naming that as the cause would send the next
// reader hunting a defect that does not exist.
func TestDispatchRequest_CancelKeepingFilesReportsAFailedDrainWithoutTripping(t *testing.T) {
	var logs bytes.Buffer
	dir := t.TempDir()
	a := newHelperAssembler()
	// Level=Error, so the buffer collects the tripwire and nothing else. The
	// branch under test logs at Warn, which is filtered out — an empty buffer
	// is the assertion.
	a.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError}))
	a.opts.OnArticlesUnwritten = func(_ string, _ int, arts []int32) {
		t.Errorf("OnArticlesUnwritten(%v) after a failed drain on the KeepFiles cancel "+
			"path — the job has left the queue, so these articles must be dropped "+
			"rather than returned to Outstanding", arts)
	}
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		t.Errorf("OnArticleRejected(%d) after a failed drain on the cancel path", artIdx)
	}

	wc := newWriteCache(1 << 20)
	f := newHelperFile(t, dir, "kept_drain_fault.dat", 0)
	f.w.wc = wc
	key := f.w.key
	open := map[fileKey]*openFile{key: f}

	// Buffer a real article, then fault the writer so the drain that follows
	// cannot land it.
	if !a.handleSuccessArticle(f, WriteRequest{
		JobID: "job", FileIdx: 0, ArtIdx: 0, MessageID: "a", Offset: 0, Data: []byte("AAAA"),
	}) {
		t.Fatal("the article was not accepted, so the fixture never buffered it")
	}
	f.w.writeAt = func([]byte, int64) (int, error) { return 0, syscall.ENOSPC }

	a.dispatchRequest(
		WriteRequest{
			FileIdx:     fileIdxCancelJob,
			MessageID:   key.jobID,
			ackCh:       make(chan error, 1),
			disposition: KeepFiles,
		},
		open, map[fileKey]struct{}{}, map[string]struct{}{}, wc)

	if _, err := os.Stat(f.info.Path); err != nil {
		t.Errorf("the kept file is gone after a failed drain (%v) — a drain that could "+
			"not land its bytes is not a reason to destroy the ones already there", err)
	}
	if logs.Len() != 0 {
		t.Errorf("the tripwire fired at Error after a failed drain:\n%s\n"+
			"a failed drain is a KNOWN producer of the faulted set here, so reporting "+
			"it as a producer nothing drains names the wrong cause and sends the next "+
			"reader after a defect that is not there", logs.String())
	}
}
