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
// Counting it claims nothing about its bytes. The truncate bound is derived
// from Class A, a rejected article decoded no fact and earns no durable bit,
// and its bytes are charged to failedBytes — so the file finishes with a hole
// par2 repairs from, which is exactly what a permanently failed article
// already does through handleFatalArticle.
func TestRejectedOffsetIsFailedAndStillCompletesTheFile(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	var rejected []int32
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		rejected = append(rejected, artIdx)
	}
	f := newHelperFile(t, dir, "hostile.dat", 1000)
	f.info.TotalParts = 2

	req := WriteRequest{
		JobID: "job", FileIdx: 0, ArtIdx: 7, MessageID: "<evil@x>",
		Offset: 1 << 40, Data: []byte("x"),
	}
	if !a.handleSuccessArticle(f, req) {
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
	if a.handleSuccessArticle(f, req) {
		t.Error("a redelivery of the rejected article was counted again")
	}
}

// TestRouteAcceptFailure_SplitsArticleFaultsFromStorageFaults pins the A1
// split at the one place that makes it, and pins both directions because
// either mistake is silent and expensive.
//
// A storage fault sent to OnArticleRejected marks a perfectly good article
// permanently failed and burns it against par2's recovery budget over a full
// disk the user fixes in seconds. A rejected article sent to OnWriteFault
// stalls the whole job on a healthy device, waiting for a condition that will
// never clear because there is no condition.
func TestRouteAcceptFailure_SplitsArticleFaultsFromStorageFaults(t *testing.T) {
	dir := t.TempDir()
	req := WriteRequest{JobID: "job", FileIdx: 0, ArtIdx: 3}

	t.Run("a rejected article goes to OnArticleRejected", func(t *testing.T) {
		a := newHelperAssembler()
		var rejReason string
		var faults int
		a.opts.OnArticleRejected = func(_ string, _ int, _ int32, reason string) { rejReason = reason }
		a.opts.OnWriteFault = func(string, int, *storagefault.Fault) { faults++ }
		f := newHelperFile(t, dir, "reject.dat", 100)

		a.routeAcceptFailure(f, req, &rejectedArticleError{reason: "negative offset"})

		if rejReason != "negative offset" {
			t.Errorf("OnArticleRejected reason = %q, want %q", rejReason, "negative offset")
		}
		if faults != 0 {
			t.Error("a rejected article was routed as a storage fault; the job stalls on a healthy disk")
		}
	})

	t.Run("a storage fault goes to OnWriteFault", func(t *testing.T) {
		a := newHelperAssembler()
		var rejected int
		var gotFault *storagefault.Fault
		a.opts.OnArticleRejected = func(string, int, int32, string) { rejected++ }
		a.opts.OnWriteFault = func(_ string, _ int, f *storagefault.Fault) { gotFault = f }
		f := newHelperFile(t, dir, "fault.dat", 100)

		a.routeAcceptFailure(f, req, syscall.ENOSPC)

		if gotFault == nil {
			t.Fatal("a storage fault reached no callback at all")
		}
		if !errors.Is(gotFault, syscall.ENOSPC) {
			t.Errorf("routed fault = %v, want it to wrap ENOSPC", gotFault)
		}
		if rejected != 0 {
			t.Error("a full disk was recorded against the article; its retry budget is burned " +
				"and its bytes count against par2's recovery budget")
		}
	})
}

// TestRejectedArticleError_NamesTheReason pins that the reason survives into
// the error text. It is what the operator sees in the log line above the ack,
// and an error that rendered as a bare "article rejected" would leave a
// permanently failed article with no recorded cause.
func TestRejectedArticleError_NamesTheReason(t *testing.T) {
	err := &rejectedArticleError{reason: "write extends past declared file size"}
	if got, want := err.Error(), "assembler: article rejected: write extends past declared file size"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
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
	opts.OnFileComplete = func(_ string, fileIdx int) { completions <- fileIdx }
	rejected := make(chan int32, 4)
	opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) { rejected <- artIdx }

	a := startAssembler(t, opts)

	if err := writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, MessageID: "<good@x>",
		Offset: 0, Data: []byte("AAAA"),
	}); err != nil {
		t.Fatal(err)
	}
	// A yEnc header claiming an offset far past the file's expected size.
	if err := writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 1, MessageID: "<evil@x>",
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
