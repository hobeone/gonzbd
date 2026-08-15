package assembler

import (
	"context"
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestWaitEnded pins the split that decides whether a timeout parks a job.
//
// Both cases arrive as the same expired context, and conflating them is what
// let an ordinary shutdown park a healthy job: the checkpoint budget expires
// on a queue with many open files, the barrier classified the resulting
// context error as storage, and the pause was persisted by the final queue
// save with nothing left to undo it.
//
// Called directly because the two arms cannot both be reached through submit
// in one test — one needs a live caller context and a fired inner bound, the
// other needs the reverse — and because the discriminator is a single
// condition whose inversion is invisible from outside.
func TestWaitEnded(t *testing.T) {
	a := &Assembler{opts: Options{FileInfo: func(string, int) (FileInfo, error) {
		return FileInfo{Path: "/downloads/a.bin"}, nil
	}}}
	tgt := &jobSyncTarget{a: a, jobID: "job-1"}

	t.Run("the caller stopped waiting", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := tgt.waitEnded(ctx, syncOp{kind: opSync, fileIdx: 0})

		if !errors.Is(err, durability.ErrTargetUnavailable) {
			t.Errorf("err = %v, want ErrTargetUnavailable: the caller's own deadline "+
				"ending says nothing about the device", err)
		}
		if _, isFault := errors.AsType[*storagefault.Fault](err); isFault {
			t.Error("a shutdown budget expiring was reported as a storage fault, which " +
				"parks a healthy job with a reason no operator action clears")
		}
	})

	t.Run("our own bound expired", func(t *testing.T) {
		err := tgt.waitEnded(context.Background(), syncOp{kind: opSync, fileIdx: 0})

		if errors.Is(err, durability.ErrTargetUnavailable) {
			t.Fatalf("err = %v, want a storage fault: the worker is parked in a syscall "+
				"against a mount that is not answering, and a job left running against "+
				"one sits at N%% with nothing surfaced (A2)", err)
		}
		f, ok := errors.AsType[*storagefault.Fault](err)
		if !ok {
			t.Fatalf("err = %v (%T), want a *storagefault.Fault", err, err)
		}
		if f.Op != "sync" {
			t.Errorf("op = %q, want the operation that timed out", f.Op)
		}
		if f.Path != "" {
			t.Errorf("path = %q, want empty: resolving one calls back into the component "+
				"that just failed to answer, and Barrier.raise fills it in instead", f.Path)
		}
		if f.Permanent {
			t.Errorf("fault = %v, want retryable: a mount that stops answering usually "+
				"comes back", f)
		}
	})

	t.Run("unavailable wraps rather than replaces", func(t *testing.T) {
		cause := errors.New("some cause")
		err := tgt.unavailable(cause)
		if !errors.Is(err, cause) || !errors.Is(err, durability.ErrTargetUnavailable) {
			t.Errorf("err = %v, want both the sentinel and the cause preserved", err)
		}
	})
}

// TestSyncOpKindString keeps every operation named, because the name is what
// an operator reads as the failing syscall in a stall reason. An unnamed kind
// would surface as "unknown", which is the reason R27 exists to prevent.
func TestSyncOpKindString(t *testing.T) {
	for _, tc := range []struct {
		kind syncOpKind
		want string
	}{
		{opFiles, "list"},
		{opDrain, "write"},
		{opSync, "sync"},
		{opStat, "stat"},
		{opTruncate, "truncate"},
		{opClose, "close"},
		{opJobs, "list"},
		{opConfirm, "confirm"},
		{syncOpKind(99), "unknown"},
	} {
		if got := tc.kind.String(); got != tc.want {
			t.Errorf("syncOpKind(%d).String() = %q, want %q", tc.kind, got, tc.want)
		}
	}
}
