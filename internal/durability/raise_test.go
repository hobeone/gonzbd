package durability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestRaise pins the boundary rule directly, one branch at a time.
//
// The call sites exercise it end to end, but each reaches only one branch, and
// the branch that matters most is the one where doing nothing looks identical
// to doing the right thing: a non-storage condition must leave Stallable
// untouched. Asserting that from a call site means asserting the absence of an
// effect, which passes when raise is never reached at all.
func TestRaise(t *testing.T) {
	const path = "/downloads/a.bin"

	newBarrier := func(s *recordingStall) *Barrier {
		db := openTestDB(t)
		return NewBarrier(NewSQLiteRunStore(db),
			&recordingAcker{}, s, slog.New(slog.DiscardHandler))
	}

	t.Run("a nil error raises nothing", func(t *testing.T) {
		s := &recordingStall{}
		if err := newBarrier(s).raise("job-1", "sync", path, nil); err != nil {
			t.Errorf("raise(nil) = %v, want nil", err)
		}
		if len(s.stalled)+len(s.failed) != 0 {
			t.Error("a nil error reached Stallable")
		}
	})

	t.Run("a fault the target built keeps its own op", func(t *testing.T) {
		s := &recordingStall{}
		f := storagefault.Classify("stat", "/other/path", syscall.ENOSPC)
		err := newBarrier(s).raise("job-1", "sync", path, f)

		if !errors.Is(err, ErrFaultRouted) {
			t.Errorf("err = %v, want it marked routed", err)
		}
		if len(s.stalled) != 1 {
			t.Fatalf("stalled %d times, want 1", len(s.stalled))
		}
		if s.stalled[0].Op != "stat" || s.stalled[0].Path != "/other/path" {
			t.Errorf("fault = %v — relabelling it discards which syscall actually "+
				"failed, which is what makes the reason actionable (R27)", s.stalled[0])
		}
	})

	t.Run("a fault with no path of its own gets the caller's", func(t *testing.T) {
		s := &recordingStall{}
		// What the SyncTarget boundary mints: it cannot resolve a path
		// without calling back into the component that just failed to answer.
		f := storagefault.Classify("sync", "", errors.New("the worker did not answer"))
		newBarrier(s).raise("job-1", "sync", path, f)

		if len(s.stalled) != 1 {
			t.Fatalf("stalled %d times, want 1", len(s.stalled))
		}
		if s.stalled[0].Path != path {
			t.Errorf("path = %q, want %q — an operator told a download halted but not "+
				"which file has nothing to act on (R27)", s.stalled[0].Path, path)
		}
	})

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a deliberate close", fmt.Errorf("worker: %w", ErrFileNotOpen)},
		{"an unavailable target", fmt.Errorf("stopped: %w", ErrTargetUnavailable)},
	} {
		t.Run(tc.name+" is not a storage condition", func(t *testing.T) {
			s := &recordingStall{}
			err := newBarrier(s).raise("job-1", "sync", path, tc.err)

			if !errors.Is(err, tc.err) {
				t.Errorf("err = %v, want the cause preserved", err)
			}
			if errors.Is(err, ErrFaultRouted) {
				t.Error("a non-storage condition was marked as a routed fault")
			}
			if len(s.stalled) != 0 {
				t.Errorf("the job was parked with %q. storagefault.Classify defaults "+
					"everything it does not recognise to retryable, so classifying this "+
					"names a disk that did not fail and an action that does not exist",
					s.stalled[0])
			}
			if len(s.failed) != 0 {
				t.Errorf("the job was FAILED for a non-storage condition: %v", s.failed[0])
			}
		})
	}

	t.Run("a raw storage error is classified and routed", func(t *testing.T) {
		s := &recordingStall{}
		err := newBarrier(s).raise("job-1", "truncate", path, syscall.EROFS)

		if !errors.Is(err, ErrFaultRouted) {
			t.Errorf("err = %v, want it marked routed", err)
		}
		if len(s.failed) != 1 {
			t.Fatalf("failed %d times, want 1: EROFS is permanent and no waiting clears it",
				len(s.failed))
		}
		if s.failed[0].Op != "truncate" || s.failed[0].Path != path {
			t.Errorf("fault = %v, want it to name this call site's op and path", s.failed[0])
		}
	})

	// Grounding for the whole table: the barrier really does reach raise from
	// its own operations, so the branches above are not exercised only here.
	t.Run("the call sites reach it", func(t *testing.T) {
		s := &recordingStall{}
		tgt := &fakeTarget{written: map[int32][]WrittenArticle{0: {}}, syncErr: syscall.ENOSPC}
		if _, err := newBarrier(s).Run(context.Background(), "job-1", tgt); err == nil {
			t.Fatal("Run succeeded over a failing sync")
		}
		if len(s.stalled) != 1 {
			t.Errorf("stalled %d times, want 1", len(s.stalled))
		}
	})
}
