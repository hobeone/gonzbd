package assembler

import (
	"errors"
	"slices"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestDrainAndClose_ReturnsTheRolledBackArticles pins the half of this that is
// about ARTICLES rather than about the fault.
//
// A failed Drain rolls back everything after the failing write into w.faulted,
// and takeFaulted is its only consumer. Without a releaseFaulted here the set
// died with the writer: the articles kept their Emitted bits, so
// ForEachUnfinishedArticle skipped them and only a restart's ClearAllEmitted
// recovered them, and partsWritten kept counting them — leaving the file that
// many parts closer to TotalParts with nothing on disk behind them.
func TestDrainAndClose_ReturnsTheRolledBackArticles(t *testing.T) {
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
			JobID: "job", FileIdx: 0, ArtIdx: int32(i), //nolint:gosec // G115: tiny test index
			MessageID: string(rune('a' + i)), Offset: int64(i) * 4, Data: []byte("AAAA"),
		}) {
			t.Fatalf("article %d was not accepted, so the fixture never buffered it", i)
		}
	}

	// The device fills up between the accepts and the close.
	f.w.writeAt = func([]byte, int64) (int, error) { return 0, syscall.ENOSPC }

	_ = a.drainAndClose(f)

	slices.Sort(rolledBack)
	if !slices.Equal(rolledBack, []int32{0, 1}) {
		t.Errorf("rolled-back articles = %v, want [0 1] — an article reported by nobody "+
			"keeps its Emitted bit and is never re-dispatched", rolledBack)
	}
	if f.w.parts() != 0 {
		t.Errorf("partsWritten = %d, want 0 — the file is left that many parts closer "+
			"to TotalParts with nothing on disk behind them", f.w.parts())
	}
}

// TestDrainAndClose_ReportsTheFailureToItsCaller covers the other half. The
// fault is REPORTED rather than routed — see drainAndClose's doc for why
// routing it out-of-band is wrong on all three callers — so the return value
// is the entire mechanism by which a close-time failure is not silent.
func TestDrainAndClose_ReportsTheFailureToItsCaller(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()

	t.Run("a failing drain", func(t *testing.T) {
		f := newHelperFile(t, dir, "drain.dat", 0)
		f.w.wc = newWriteCache(1 << 20)
		if !a.handleSuccessArticle(f, WriteRequest{
			JobID: "job", FileIdx: 0, MessageID: "a", Offset: 0, Data: []byte("AAAA"),
		}) {
			t.Fatal("the article was not accepted, so the fixture never buffered it")
		}
		f.w.writeAt = func([]byte, int64) (int, error) { return 0, syscall.ENOSPC }

		err := a.drainAndClose(f)

		var fault *storagefault.Fault
		if !errors.As(err, &fault) {
			t.Fatalf("drainAndClose() = %v, want a *storagefault.Fault — a full device "+
				"at close was invisible to CloseFile's callers", err)
		}
		if fault.Op != "write" {
			t.Errorf("fault op = %q, want %q — relabelling discards which syscall "+
				"actually failed, which is what makes the reason actionable (R27)",
				fault.Op, "write")
		}
	})

	t.Run("a failing sync", func(t *testing.T) {
		f := newHelperFile(t, dir, "sync.dat", 0)
		f.w.syncFile = func() error { return syscall.EIO }

		err := a.drainAndClose(f)

		var fault *storagefault.Fault
		if !errors.As(err, &fault) {
			t.Fatalf("drainAndClose() = %v, want a *storagefault.Fault — an fsync that "+
				"fails at close means the drained bytes are not durable", err)
		}
		if fault.Op != "sync" {
			t.Errorf("fault op = %q, want %q", fault.Op, "sync")
		}
	})

	t.Run("a failing close", func(t *testing.T) {
		f := newHelperFile(t, dir, "close.dat", 0)
		// Injected through the seam rather than by pre-closing the handle.
		// Pre-closing makes Sync fail one line earlier, so the arm under test
		// is never reached — and Go's poll.FD answers an already-closed
		// handle with os.ErrClosed, not EBADF, so it would not even be the
		// permanent errno such a fixture appears to be arranging.
		f.w.closeFile = func() error { return syscall.EIO }

		err := a.drainAndClose(f)

		var fault *storagefault.Fault
		if !errors.As(err, &fault) {
			t.Fatalf("drainAndClose() = %v, want a *storagefault.Fault — on "+
				"network-backed mounts the close is frequently where a deferred "+
				"write error first surfaces", err)
		}
		if fault.Op != "close" {
			t.Errorf("fault op = %q, want %q", fault.Op, "close")
		}
	})
}

// TestDrainAndClose_PrefersAPermanentFaultOverTheFirstOne is the R20 case.
//
// ext4 mounted errors=remount-ro — the Debian default. The drain's WriteAt
// returns ENOSPC, which storagefault classifies RETRYABLE (it is deliberately
// absent from permanentErrnos); the kernel then remounts read-only and the
// close returns EROFS, which is permanent. Reporting the first one alone
// describes the condition as something waiting can clear, when it cannot —
// the caller sees a retryable fault and the job is merely stalled, to be
// re-faulted every interval forever against a filesystem that will never
// accept a write again.
func TestDrainAndClose_PrefersAPermanentFaultOverTheFirstOne(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()

	f := newHelperFile(t, dir, "remount.dat", 0)
	f.w.wc = newWriteCache(1 << 20)
	if !a.handleSuccessArticle(f, WriteRequest{
		JobID: "job", FileIdx: 0, MessageID: "a", Offset: 0, Data: []byte("AAAA"),
	}) {
		t.Fatal("the article was not accepted, so the fixture never buffered it")
	}
	f.w.writeAt = func([]byte, int64) (int, error) { return 0, syscall.ENOSPC }
	f.w.closeFile = func() error { return syscall.EROFS }

	err := a.drainAndClose(f)

	var fault *storagefault.Fault
	if !errors.As(err, &fault) {
		t.Fatalf("drainAndClose() = %v, want a *storagefault.Fault", err)
	}
	if !fault.Permanent {
		t.Errorf("fault = %+v, want the PERMANENT one — the retryable ENOSPC arrived "+
			"first, but only the EROFS behind it preserves R20's permanent → Fail "+
			"routing; reported as retryable the job stalls and is re-faulted forever",
			fault)
	}
}

// TestCloseFile_ReportsACloseTimeFailure pins the wiring, which is the part of
// this change that a caller actually observes.
//
// handleSyncOp's opClose arm used to leave the reply error nil, so CloseFile
// reported success for a close whose Drain, Sync or Close had failed. Both
// production callers — finalizeCompletedFile's defer and retryFinalize — log
// at Debug and continue, so the file was marked complete and fed to
// DirectUnpack and post-processing while its bytes were not all on disk. Every
// other fallible op in that switch answers its caller.
func TestCloseFile_ReportsACloseTimeFailure(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()

	wc := newWriteCache(1 << 20)
	f := newHelperFile(t, dir, "reported.dat", 0)
	f.w.wc = wc
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}

	f.w.syncFile = func() error { return syscall.EIO }

	op := &syncOp{kind: opClose, jobID: "job", fileIdx: 0, reply: make(chan syncReply, 1)}
	a.handleSyncOp(op, open, wc)

	r := <-op.reply
	if r.err == nil {
		t.Fatal("opClose answered nil for a close whose fsync failed. CloseFile then " +
			"reports success, and the file is marked complete and handed to " +
			"DirectUnpack and post-processing with bytes that are not durable")
	}
	var fault *storagefault.Fault
	if !errors.As(r.err, &fault) || fault.Op != "sync" {
		t.Errorf("reply err = %v, want a *storagefault.Fault naming the sync", r.err)
	}
}
