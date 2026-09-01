package queue

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
)

// lockProbeStore records whether q.mu was held at the moment the store's
// persistence methods were invoked.
//
// The embedded nil Store interface means any method this probe does not
// override panics if the code under test calls it — a deliberate tripwire, so
// the test fails loudly rather than silently exercising a different path.
type lockProbeStore struct {
	Store

	q *Queue

	// moveErr, when non-nil, is returned by MoveToHistory.
	moveErr error

	mu               sync.Mutex
	moveCalled       bool
	lockHeldDuringIO bool
}

func (s *lockProbeStore) Add(context.Context, *Job) error                 { return nil }
func (s *lockProbeStore) Update(context.Context, *Job) error              { return nil }
func (s *lockProbeStore) ShiftSortKey(context.Context, string, int) error { return nil }

func (s *lockProbeStore) MoveToHistory(context.Context, *Job, history.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.moveCalled = true
	// TryLock succeeds only if q.mu is free. Probing this way rather than
	// taking the lock outright keeps a regression from deadlocking the suite
	// for the full test timeout.
	if s.q.mu.TryLock() {
		s.q.mu.Unlock()
	} else {
		s.lockHeldDuringIO = true
	}
	return s.moveErr
}

// MoveToHistory opens a SQLite transaction spanning the active-job and history
// tables (see Store.MoveToHistory). Running it under q.mu -- the global queue
// mutex the downloader's dispatch loop contends on every pass -- means a slow
// or lock-contended database write stalls all dispatch.
//
// scripts/check_lock_io cannot catch this: it has no detector for Store or
// database/sql calls, and the os.Stat/os.Rename in the same function sit one
// frame below the caller that takes the lock, which its single-function AST
// walk cannot see.
func TestClaimFailure_DoesNotHoldQueueLockDuringIO(t *testing.T) {
	q := New(WithStateDir(t.TempDir()))
	probe := &lockProbeStore{q: q}
	q.store = probe

	// A job whose manifest does not exist on disk. PromoteNext's manifest read
	// fails, which is the claim-failure path under test.
	job := &Job{
		ID:     "job-corrupt",
		Name:   "corrupt-manifest",
		Status: constants.StatusQueued,
	}
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Add ends by calling PromoteNext, which drives the failure path.
	probe.mu.Lock()
	called, heldDuringIO := probe.moveCalled, probe.lockHeldDuringIO
	probe.mu.Unlock()

	if !called {
		t.Fatal("MoveToHistory was never called — the claim-failure path did not run, " +
			"so this test is not exercising what it claims")
	}
	if heldDuringIO {
		t.Error("q.mu was held while MoveToHistory ran: a SQLite transaction is " +
			"executing under the global queue lock, stalling downloader dispatch")
	}
}

// The failed job must still be evicted from the in-RAM queue regardless of how
// the I/O is sequenced, so hoisting the I/O out of the lock cannot silently
// drop the state change it was paired with.
func TestClaimFailure_RemovesJobFromQueue(t *testing.T) {
	q := New(WithStateDir(t.TempDir()))
	probe := &lockProbeStore{q: q}
	q.store = probe

	job := &Job{
		ID:     "job-corrupt",
		Name:   "corrupt-manifest",
		Status: constants.StatusQueued,
	}
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if _, err := q.liveJob("job-corrupt"); err == nil {
		t.Error("job with an unreadable manifest is still in the queue after promotion failed")
	}
	if n := q.Len(); n != 0 {
		t.Errorf("queue length = %d, want 0", n)
	}
}

// recordingHandler captures emitted log records for assertion.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler            { return h }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}

// hasErrAttr reports whether any record carries an "err" attribute matching
// target.
//
// Asserting on the specific error matters: PromoteNext already logs the
// claim failure itself at error level, so a bare "was anything logged at
// error level" check passes whether or not the store error is reported.
func (h *recordingHandler) hasErrAttr(target error) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		found := false
		r.Attrs(func(a slog.Attr) bool {
			if a.Key != "err" {
				return true
			}
			if err, ok := a.Value.Any().(error); ok && errors.Is(err, target) {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// A failed MoveToHistory leaves the job evicted from RAM but potentially still
// present in the store as Queued. Nothing can roll that back and no caller is
// positioned to act on the error, but the divergence must not be silent -- it
// is invisible until the next startup reconciles it.
func TestClaimFailure_LogsMoveToHistoryError(t *testing.T) {
	rec := &recordingHandler{}
	q := New(WithStateDir(t.TempDir()), WithLogger(slog.New(rec)))
	moveErr := errors.New("database is locked")
	probe := &lockProbeStore{q: q, moveErr: moveErr}
	q.store = probe

	job := &Job{
		ID:     "job-corrupt",
		Name:   "corrupt-manifest",
		Status: constants.StatusQueued,
	}
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	probe.mu.Lock()
	called := probe.moveCalled
	probe.mu.Unlock()
	if !called {
		t.Fatal("MoveToHistory was never called — the test is not exercising the failure path")
	}

	if !rec.hasErrAttr(moveErr) {
		t.Error("the MoveToHistory error was not logged: a job evicted from RAM " +
			"while still persisted as Queued is a silent store divergence")
	}
}
