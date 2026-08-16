package assembler

import (
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestTransientWriteFault_DoesNotPermanentlyUndercountTheFile pins the pairing
// of partsWritten with the seenDone record the give-back is derived from. They
// used to be set at different moments, and the gap was permanent.
//
// FileWriter.admitAccepted now applies both in one method, so the pairing is
// structural and this test is a regression net rather than the thing that
// establishes it. The history is worth keeping because the failure was silent
// and permanent: a failing write decided the give-back from seenDone, which
// handleSuccessArticle recorded BEFORE attempting the write — but the
// increment lived in processRequest, after the accept, and processRequest
// returns before it on a failure. So the roll-back decremented a count that
// had never been applied. The article's retry then restored only one of the
// two, leaving the file permanently one part short: partsWritten >= TotalParts
// becomes unreachable, OnFileComplete never fires, MarkFileComplete never
// runs, and the job sits at 100% with zero outstanding articles across
// restarts.
//
// A transient ENOSPC that the user clears in ten seconds is enough.
func TestTransientWriteFault_DoesNotPermanentlyUndercountTheFile(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	a.opts.OnArticlesUnwritten = func(string, int, []int32) {}
	a.opts.OnWriteFault = func(string, int, *storagefault.Fault) {}

	wc := newWriteCache(0) // no coalescing: every accept is its own write
	f := newHelperFile(t, dir, "count.dat", 0)
	f.w.wc = wc
	f.info.TotalParts = 3
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}
	completed := map[fileKey]struct{}{}

	send := func(idx int32, msg string) {
		a.processRequest(WriteRequest{
			JobID: "job", FileIdx: 0, ArtIdx: idx, MessageID: msg,
			Offset: int64(idx) * 4, Data: []byte("AAAA"),
		}, open, completed, wc)
	}

	send(0, "a")
	send(1, "b")
	if f.w.parts() != 2 {
		t.Fatalf("partsWritten = %d, want 2; the fixture did not reach the state "+
			"under test", f.w.parts())
	}

	// The volume fills for exactly one article.
	f.w.writeAt = func([]byte, int64) (int, error) { return 0, syscall.ENOSPC }
	send(2, "c")
	if f.w.parts() != 2 {
		t.Errorf("partsWritten = %d after a failed write, want 2 — the failing article "+
			"was never counted, so decrementing for it takes away a part that one of "+
			"the SUCCESSFUL articles is holding", f.w.parts())
	}

	// The user clears the disk and the article is re-fetched.
	f.w.writeAt = f.w.handle.WriteAt
	send(2, "c")
	if f.w.parts() != 3 {
		t.Errorf("partsWritten = %d after the retry landed, want 3 — the file can now "+
			"never reach TotalParts, so OnFileComplete never fires and the job sits at "+
			"100%% with nothing outstanding, across restarts", f.w.parts())
	}
}
