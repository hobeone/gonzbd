package assembler

import (
	"errors"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

func TestRejectedOffsetIsNotCountedAndIsReported(t *testing.T) {
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
	if a.handleSuccessArticle(f, req) {
		t.Error("an out-of-range article was counted toward the file's part total; " +
			"a hostile server can finalize a file by sending offsets it knows will be rejected")
	}
	if len(rejected) != 1 || rejected[0] != 7 {
		t.Errorf("OnArticleRejected calls = %v, want [7] — the article is not written, not "+
			"acked failed, and still Emitted, so it is never re-dispatched and its bytes "+
			"never charged against par2's recovery budget", rejected)
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
		a.opts.OnWriteFault = func(string, int, int32, *storagefault.Fault) { faults++ }
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
		a.opts.OnWriteFault = func(_ string, _ int, _ int32, f *storagefault.Fault) { gotFault = f }
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
