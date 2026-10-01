package durability

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// commitErrStore fails every commit with a plain error, the shape a SQLite
// failure has: not a *storagefault.Fault, and wrapping no errno.
type commitErrStore struct {
	runStore
	err error
}

func (c *commitErrStore) commit(context.Context, string, []DurableArticle) ([]Collision, error) {
	return nil, c.err
}

func commitFaultTarget() *fakeTarget {
	return &fakeTarget{
		written: map[int32][]WrittenArticle{0: {{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100}}},
		size:    100,
	}
}

// TestBarrier_Run_ACommitErrorStallsTheJob pins that a failed commit reaches
// Stallable like the drain, sync and stat failures before it. A commit error
// that only returned left the job downloading while no barrier could record
// anything it wrote.
//
// It also pins R27 for this fault specifically: Run's raise call used to pass
// "" for the path, since nothing before it ever learned the database's own —
// commit, unlike write/sync/stat, has no file index to resolve a path from.
// The stall reason must still name the database file, or an operator sees
// `on commit ""` and has nothing to act on.
func TestBarrier_Run_ACommitErrorStallsTheJob(t *testing.T) {
	t.Parallel()
	const dbPath = "/admin/history.db"
	boom := errors.New("database or disk is full")
	stall := &recordingStall{}
	ack := &recordingAcker{}
	tgt := commitFaultTarget()
	b := NewBarrier(&commitErrStore{runStore: NewStore(openTestDB(t), dbPath), err: boom}, ack, stall, testLogger(t))

	_, err := b.Run(context.Background(), "job-1", tgt)

	if len(stall.stalled) != 1 {
		t.Fatalf("stalled %d times, want 1 — a job whose commits fail keeps downloading "+
			"bytes no barrier can record", len(stall.stalled))
	}
	if f := stall.stalled[0]; f.Op != "commit" || !errors.Is(f, boom) {
		t.Errorf("fault = %v, want op commit wrapping the store's error", f)
	}
	if f := stall.stalled[0]; f.Path != dbPath {
		t.Errorf("fault path = %q, want %q — the store's own path (R27)", f.Path, dbPath)
	}
	if !strings.Contains(err.Error(), dbPath) {
		t.Errorf("Run = %v, want it to name %q", err, dbPath)
	}
	if len(stall.failed) != 0 {
		t.Errorf("failed %d times; an unrecognised commit error is retryable", len(stall.failed))
	}
	if !errors.Is(err, ErrFaultRouted) || !errors.Is(err, boom) {
		t.Errorf("Run = %v, want the routed fault wrapping the store's error", err)
	}
	if len(ack.proofs) != 0 {
		t.Errorf("acked %d proofs past a failed commit", len(ack.proofs))
	}
	if len(tgt.confirmed) != 0 {
		t.Errorf("confirmed %v; the drain reports must survive for the retry to re-report (R12)",
			tgt.confirmed)
	}
}

// TestBarrier_Run_ACommitAbandonedByItsCallerIsNotStalled pins the other half:
// a commit that failed once the caller's context ended says nothing about
// storage, so the job is not parked for it.
func TestBarrier_Run_ACommitAbandonedByItsCallerIsNotStalled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	stall := &recordingStall{}
	b := NewBarrier(&commitErrStore{runStore: NewStore(openTestDB(t), "/admin/history.db"), err: errors.New("interrupted")},
		&recordingAcker{}, stall, testLogger(t))
	tgt := commitFaultTarget()
	cancel()

	_, err := b.Run(ctx, "job-1", tgt)

	if len(stall.stalled)+len(stall.failed) != 0 {
		t.Errorf("stalled %v, failed %v — a caller that stopped waiting is not a storage "+
			"condition, and parking the job names a disk that did not fail",
			stall.stalled, stall.failed)
	}
	if !errors.Is(err, ErrTargetUnavailable) || !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v, want ErrTargetUnavailable naming the cancellation", err)
	}
	if errors.Is(err, ErrFaultRouted) {
		t.Error("a caller's cancellation was marked as a routed fault")
	}
}

// finalizeStoreFault fails one of the two store calls FinalizeFile makes — its
// read of the file's stored runs ("read") or its commit ("commit") — with a
// plain error, the shape a SQLite failure has. With cancel set it also ends
// the caller's context as the call fails, which is what a driver interrupted
// by that cancellation looks like from the barrier's side.
type finalizeStoreFault struct {
	runStore
	op     string
	err    error
	cancel context.CancelFunc
}

func (s *finalizeStoreFault) fail(op string) error {
	if s.op != op {
		return nil
	}
	if s.cancel != nil {
		s.cancel()
	}
	return s.err
}

func (s *finalizeStoreFault) commit(ctx context.Context, jobID string, arts []DurableArticle) ([]Collision, error) {
	if err := s.fail("commit"); err != nil {
		return nil, err
	}
	return s.runStore.commit(ctx, jobID, arts)
}

func (s *finalizeStoreFault) ForFile(ctx context.Context, jobID string, idx int32) ([]Run, error) {
	if err := s.fail("read"); err != nil {
		return nil, err
	}
	return s.runStore.ForFile(ctx, jobID, idx)
}

// TestBarrier_FinalizeFile_AStoreErrorStallsNamingTheStore pins that
// FinalizeFile's two store calls are routed as Run's commit is: to Stallable,
// with the operation that failed and the database's own path (R27).
//
// Before, both returned a plain error, and Application.routeFinalizeFailure
// classified it as a "finalize" fault on the completed file — pointing the
// operator at a download directory whose disk may be healthy.
func TestBarrier_FinalizeFile_AStoreErrorStallsNamingTheStore(t *testing.T) {
	t.Parallel()
	const dbPath = "/admin/history.db"
	for _, op := range []string{"read", "commit"} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			boom := errors.New("database or disk is full")
			store := NewStore(openTestDB(t), dbPath)
			stall := &recordingStall{}
			ack := &recordingAcker{}
			tgt := &truncTarget{drained: []WrittenArticle{{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100}}}
			b := NewBarrier(&finalizeStoreFault{runStore: store, op: op, err: boom}, ack, stall, testLogger(t))

			_, err := b.FinalizeFile(context.Background(), "job-1", 0, tgt)

			if len(stall.stalled) != 1 {
				t.Fatalf("stalled %d times, want 1 — the job is halted with no reason the "+
					"barrier chose", len(stall.stalled))
			}
			f := stall.stalled[0]
			if f.Op != op || !errors.Is(f, boom) {
				t.Errorf("fault = %v, want op %q wrapping the store's error", f, op)
			}
			if f.Path != dbPath {
				t.Errorf("fault path = %q, want %q — the store's own path, not the "+
					"completed file's (R27)", f.Path, dbPath)
			}
			if len(stall.failed) != 0 {
				t.Errorf("failed %d times; an unrecognised store error is retryable", len(stall.failed))
			}
			if !errors.Is(err, ErrFaultRouted) || !errors.Is(err, boom) {
				t.Errorf("FinalizeFile = %v, want the routed fault wrapping the store's error, "+
					"so routeFinalizeFailure does not stall the job a second time", err)
			}
			if len(ack.proofs) != 0 {
				t.Errorf("acked %d proofs past a failed %s", len(ack.proofs), op)
			}
			if len(tgt.confirmed) != 0 {
				t.Errorf("confirmed %v; the drain report must survive for the retry (R12)", tgt.confirmed)
			}
			if runs, _ := store.ForJob(context.Background(), "job-1"); len(runs) != 0 {
				t.Errorf("a failed finalize recorded %d runs (R7)", len(runs))
			}
		})
	}
}

// TestBarrier_FinalizeFile_AStoreCallAbandonedByItsCallerIsNotStalled pins the
// other half for FinalizeFile: a store call that failed once the caller's
// context ended says nothing about storage, so nothing is routed, and the
// error carries the sentinel routeFinalizeFailure answers without a stall.
func TestBarrier_FinalizeFile_AStoreCallAbandonedByItsCallerIsNotStalled(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"read", "commit"} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stall := &recordingStall{}
			ack := &recordingAcker{}
			tgt := &truncTarget{drained: []WrittenArticle{{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100}}}
			rs := &finalizeStoreFault{
				runStore: NewStore(openTestDB(t), "/admin/history.db"),
				op:       op, err: errors.New("interrupted"), cancel: cancel,
			}
			b := NewBarrier(rs, ack, stall, testLogger(t))

			_, err := b.FinalizeFile(ctx, "job-1", 0, tgt)

			if len(stall.stalled)+len(stall.failed) != 0 {
				t.Errorf("stalled %v, failed %v — a caller that stopped waiting is not a "+
					"storage condition", stall.stalled, stall.failed)
			}
			if !errors.Is(err, ErrTargetUnavailable) || !errors.Is(err, context.Canceled) {
				t.Errorf("FinalizeFile = %v, want ErrTargetUnavailable naming the cancellation", err)
			}
			if errors.Is(err, ErrFaultRouted) {
				t.Error("a caller's cancellation was marked as a routed fault")
			}
			if len(ack.proofs) != 0 || len(tgt.confirmed) != 0 {
				t.Errorf("acked %d proofs, confirmed %v past an abandoned %s",
					len(ack.proofs), tgt.confirmed, op)
			}
		})
	}
}
