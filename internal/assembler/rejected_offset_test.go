package assembler

import (
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestRejectedOffsetIsFailedAndStillCompletesTheFile pins what a refused
// article does to the file it belongs to.
//
// It supersedes an assertion that a rejected article must NOT be counted
// toward the file's part total. That was written for a world where a rejection
// resolved the article nowhere: counting it then let a hostile server finalize
// a file by sending offsets it knew would be rejected. The article is now
// acked permanently failed through OnArticleRejected, and with that in place
// NOT counting it is the expensive half — partsWritten can never reach
// TotalParts, so OnFileComplete never fires, MarkFileComplete never runs, and
// the job sits at 100%% with zero outstanding articles across restarts.
//
// Counting it claims nothing about its bytes. The completion trim is bounded
// by the file's owned ranges (finish), a rejected article is never written so
// no range ever covers it,
// and its bytes are charged to failedBytes — so the file finishes with a hole
// par2 repairs from, which is exactly what a permanently failed article
// already does through handleFatalArticle.
func TestRejectedOffsetIsFailedAndStillCompletesTheFile(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	var rejected []int32
	a.opts.OnArticleRejected = func(_ Owner, _ int, artIdx int32, _ string, _ int64) {
		rejected = append(rejected, artIdx)
	}
	f := newHelperFile(t, dir, "hostile.dat", 1000)
	f.info.TotalParts = 2

	req := WriteRequest{
		Owner: ownerFor("job"), JobID: "job", FileIdx: 0, ArtIdx: 7, MessageID: "<evil@x>",
		Offset: 1 << 40, Data: []byte("x"),
	}
	a.handleSuccessArticle(f, req)
	if f.w.parts() != 1 {
		t.Error("a rejected article was not counted toward the file's part total; the " +
			"file can never reach TotalParts, so OnFileComplete never fires and the job " +
			"sits at 100% with nothing outstanding, across restarts")
	}
	if len(rejected) != 1 || rejected[0] != 7 {
		t.Errorf("OnArticleRejected calls = %v, want [7] — the article is not written, not "+
			"acked failed, and still Emitted, so it is never re-dispatched and its bytes "+
			"never charged against par2's recovery budget", rejected)
	}
	// Counted as FAILED, not as done: the seen-sets are what stop a redelivery
	// from taking the part total past TotalParts.
	if _, ok := f.w.seenFailed[7]; !ok {
		t.Error("the rejected article is not in seenFailed, so a redelivery would be " +
			"counted a second time and overshoot the file's part total")
	}
	if _, ok := f.w.seenDone[7]; ok {
		t.Error("a rejected article is recorded as done; nothing wrote its bytes")
	}
	// A redelivery is a new fetch and decode, so it arrives in its own buffer.
	// Reusing req.Data would release one slice twice (#574).
	req.Data = []byte("x")
	a.handleSuccessArticle(f, req)
	if f.w.parts() != 1 {
		t.Error("a redelivery of the rejected article was counted again")
	}
}

// TestRouteOutcome_SplitsArticleFaultsFromStorageFaults pins the A1 split at
// the one place that makes it, and pins both directions because either
// mistake is silent and expensive.
//
// A storage fault sent to OnArticleRejected marks a perfectly good article
// permanently failed and burns it against par2's recovery budget over a full
// disk the user fixes in seconds. A rejected article sent to OnWriteFault
// stalls the whole job on a healthy device, waiting for a condition that will
// never clear because there is no condition.
func TestRouteOutcome_SplitsArticleFaultsFromStorageFaults(t *testing.T) {
	dir := t.TempDir()
	req := WriteRequest{Owner: ownerFor("job"), JobID: "job", FileIdx: 0, ArtIdx: 3}

	t.Run("a rejected article goes to OnArticleRejected", func(t *testing.T) {
		a := newHelperAssembler()
		var rejReason string
		var faults, unwritten int
		a.opts.OnArticleRejected = func(_ Owner, _ int, _ int32, reason string, _ int64) { rejReason = reason }
		a.opts.OnWriteFault = func(Owner, int, *storagefault.Fault) { faults++ }
		a.opts.OnArticlesUnwritten = func(Owner, int, []int32) { unwritten++ }
		f := newHelperFile(t, dir, "reject.dat", 100)

		a.routeOutcome(f, req, outcome{disposition: dispositionRejected, reason: "negative offset", uncovered: WholeArticle})

		if rejReason != "negative offset" {
			t.Errorf("OnArticleRejected reason = %q, want %q", rejReason, "negative offset")
		}
		if faults != 0 || unwritten != 0 {
			t.Error("a rejected article was routed as a storage fault; the job stalls on a healthy disk")
		}
	})

	t.Run("a storage fault goes to OnWriteFault", func(t *testing.T) {
		a := newHelperAssembler()
		var rejected int
		var gotFault *storagefault.Fault
		var unwritten []int32
		a.opts.OnArticleRejected = func(Owner, int, int32, string, int64) { rejected++ }
		a.opts.OnWriteFault = func(_ Owner, _ int, f *storagefault.Fault) { gotFault = f }
		a.opts.OnArticlesUnwritten = func(_ Owner, _ int, arts []int32) { unwritten = append(unwritten, arts...) }
		f := newHelperFile(t, dir, "fault.dat", 100)

		a.routeOutcome(f, req, outcome{disposition: dispositionRolledBack, fault: syscall.ENOSPC})

		if gotFault == nil {
			t.Fatal("a storage fault reached no callback at all")
		}
		if !errors.Is(gotFault, syscall.ENOSPC) {
			t.Errorf("routed fault = %v, want it to wrap ENOSPC", gotFault)
		}
		if len(unwritten) != 1 || unwritten[0] != 3 {
			t.Errorf("OnArticlesUnwritten = %v, want [3]: the rolled-back article's Emitted bit "+
				"is still set, and nothing else clears it", unwritten)
		}
		if rejected != 0 {
			t.Error("a full disk was recorded against the article; its retry budget is burned " +
				"and its bytes count against par2's recovery budget")
		}
	})

	t.Run("a written article goes to OnArticleWritten with its length", func(t *testing.T) {
		a := newHelperAssembler()
		var gotN int64 = -1
		var rejected, faults int
		a.opts.OnArticleWritten = func(_ Owner, _ int, _ int32, _, n int64, _ uint32) { gotN = n }
		a.opts.OnArticleRejected = func(Owner, int, int32, string, int64) { rejected++ }
		a.opts.OnWriteFault = func(Owner, int, *storagefault.Fault) { faults++ }
		f := newHelperFile(t, dir, "written.dat", 100)

		a.routeOutcome(f, req, outcome{disposition: dispositionWritten, n: 42})

		if gotN != 42 {
			t.Errorf("OnArticleWritten n = %d, want 42", gotN)
		}
		if rejected != 0 || faults != 0 {
			t.Error("a written article was reported as refused or faulted")
		}
	})

	t.Run("a disposition acceptArticle never produces is refused", func(t *testing.T) {
		a := newHelperAssembler()
		f := newHelperFile(t, dir, "fetchfailed.dat", 100)
		for _, d := range []disposition{disposition(0), dispositionFetchFailed} {
			func() {
				defer func() {
					if recover() == nil {
						t.Errorf("routeOutcome accepted %s; an outcome no arm owns would be dropped silently", d)
					}
				}()
				a.routeOutcome(f, req, outcome{disposition: d})
			}()
		}
	})
}

// TestRejectedOffset_TheFileStillCompletes is the consequence the unit test
// above only implies, driven through the real worker.
//
// The reviewer's probe reported completions=0 with both articles resolved: the
// job sits at 100%, nothing is outstanding, and nothing will ever fire the
// completion that marks the file done. It survives a restart, because the
// article is resolved in the queue and so is never re-dispatched.
func TestRejectedOffset_TheFileStillCompletes(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 2)
	// A declared size, or the range check has nothing to reject against.
	fi := files["job1:0"]
	fi.ExpectedSize = 1000
	files["job1:0"] = fi
	opts := makeOpts(dir, files)

	completions := make(chan int, 4)
	opts.OnFileComplete = func(_ Owner, fileIdx int) { completions <- fileIdx }
	rejected := make(chan int32, 4)
	opts.OnArticleRejected = func(_ Owner, _ int, artIdx int32, _ string, _ int64) { rejected <- artIdx }

	a := startAssembler(t, opts)

	if err := writeArticle(t.Context(), a, WriteRequest{
		Owner: ownerFor("job1"), JobID: "job1", FileIdx: 0, ArtIdx: 0, MessageID: "<good@x>",
		Offset: 0, Data: []byte("AAAA"),
	}); err != nil {
		t.Fatal(err)
	}
	// A yEnc header claiming an offset far past the file's expected size.
	if err := writeArticle(t.Context(), a, WriteRequest{
		Owner: ownerFor("job1"), JobID: "job1", FileIdx: 0, ArtIdx: 1, MessageID: "<evil@x>",
		Offset: 1 << 20, Data: []byte("B"),
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-rejected:
		if got != 1 {
			t.Errorf("rejected article %d, want 1", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hostile article was never rejected, so this test proves nothing")
	}

	select {
	case got := <-completions:
		if got != 0 {
			t.Errorf("completed file %d, want 0", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the file never completed. Its rejected article is resolved permanently " +
			"failed and will never arrive again, so partsWritten can never reach " +
			"TotalParts: the job sits at 100% with nothing outstanding, across restarts")
	}
}
