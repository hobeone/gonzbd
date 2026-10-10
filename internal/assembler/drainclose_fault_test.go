package assembler

import (
	"errors"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestDrainAndClose_ReportsTheFailureToItsCaller pins that the fault is REPORTED rather than routed — see drainAndClose's doc for why
// routing it out-of-band is wrong on all three callers — so the return value
// is the entire mechanism by which a close-time failure is not silent.
func TestDrainAndClose_ReportsTheFailureToItsCaller(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()

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
// ext4 mounted errors=remount-ro — the Debian default. The fsync
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
	f.w.syncFile = func() error { return syscall.ENOSPC }
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
