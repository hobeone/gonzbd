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
