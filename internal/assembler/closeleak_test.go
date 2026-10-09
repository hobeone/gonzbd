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
// The tests here install the set by hand. That is not a shortcut around a
// reachable state: the set is empty at both call sites on every path, because
// each producer is drained before the worker returns to its select loop. They
// pin the CONTRACT — if a future change adds a producer that nothing drains,
// the articles are reported rather than lost.

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

// TestRouteFaulted_ReturnsTheWholeSetToOutstanding pins the routing function
// directly: every rolled-back article is returned to Outstanding, and none is
// resolved permanently failed, since a storage fault says nothing about an
// article's availability.
func TestRouteFaulted_ReturnsTheWholeSetToOutstanding(t *testing.T) {
	a := newHelperAssembler()
	var unwritten []int32
	a.opts.OnArticlesUnwritten = func(_ string, _ int, arts []int32) {
		unwritten = append(unwritten, arts...)
	}
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		t.Errorf("OnArticleRejected called with %d; a rolled-back article is re-fetched, not resolved", artIdx)
	}

	a.routeFaulted([]faultedArticle{
		{id: articleID{msgID: "n1", artIdx: 1}},
		{id: articleID{msgID: "n3", artIdx: 3}},
	}, "job", 0)

	if len(unwritten) != 2 || unwritten[0] != 1 || unwritten[1] != 3 {
		t.Errorf("unwritten = %v, want [1 3] — an article that was never attempted "+
			"returns to Outstanding and is re-fetched", unwritten)
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

	a.routeFaulted(nil, "job", 0)
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

// TestDispatchRequest_CancelDropsTheFaultedSetButReportsIt covers the OTHER
// Close call site, which had no test at all: the branch is reachable only
// through the cancel control message, and the coverage profile showed it never
// executed.
//
// The two dispositions are deliberately different and neither implies the
// other. drainAndClose routes its set — that file is closing normally and its
// articles are still wanted. The cancel arm must NOT: the job is leaving the
// queue, so returning the articles to Outstanding re-dispatches work for a job that is going away. That holds under both
// dispositions — keeping a removed job's bytes does not make its articles
// wanted again — though this test exercises DeleteFiles, where the file is
// unlinked as well.
//
// What it owes instead is a report, at Error: the set is empty on every
// reachable path, so a non-empty one means a producer was added that nothing
// drains.
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
		open, completed, map[string]struct{}{})

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
// KeepFiles half of the test above, and it is the reason the keep branch
// calls f.w.Close rather than a.drainAndClose.
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
		open, map[fileKey]struct{}{}, map[string]struct{}{})

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

// TestDispatchRequest_CancelKeepingFilesLogsAFailedClose pins that a close
// failure under KeepFiles is reported rather than discarded. The file survives
// the cancel, so the same error that is best-effort under DeleteFiles means a
// kept file may be missing bytes.
func TestDispatchRequest_CancelKeepingFilesLogsAFailedClose(t *testing.T) {
	var logs bytes.Buffer
	a := newHelperAssembler()
	a.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	f := newHelperFile(t, t.TempDir(), "kept_close_fault.dat", 0)
	f.w.closeFile = func() error { return syscall.EIO }
	key := f.w.key
	open := map[fileKey]*openFile{key: f}

	a.dispatchRequest(
		WriteRequest{
			FileIdx:     fileIdxCancelJob,
			MessageID:   key.jobID,
			ackCh:       make(chan error, 1),
			disposition: KeepFiles,
		},
		open, map[fileKey]struct{}{}, map[string]struct{}{})

	if !strings.Contains(logs.String(), "failed to close a cancelled job's file that is being kept") {
		t.Errorf("a failed close of a kept file was not reported; log was:\n%s", logs.String())
	}
	if _, err := os.Stat(f.info.Path); err != nil {
		t.Errorf("the kept file is gone after a failed close: %v", err)
	}
}
