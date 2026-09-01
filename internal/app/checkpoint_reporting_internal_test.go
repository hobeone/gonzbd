package app

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// failingCommitStore is a RunStore whose Commit always fails, so a barrier
// over real open files reaches phase 4 and returns an error.
type failingCommitStore struct {
	durability.RunStore
}

func (failingCommitStore) Commit(context.Context, string, []durability.DurableArticle) ([]durability.Collision, error) {
	return nil, errors.New("database is locked")
}

// TestCheckpointJob_DoesNotStampABarrierOverNoFiles pins the other half of the
// stamp guard.
//
// SyncTarget.Files answers nil when OpenFiles times out on a wedged mount —
// deliberately, because the barrier has nothing useful to do with the error.
// Barrier.Run then iterates nothing, Commit returns early on an empty slice,
// len(acked)==0 returns nil, and checkpointJob read that nil as success and
// stamped last_barrier. The API reported a fresh timestamp every interval
// while nothing had been fsynced since the mount went away — the exact
// inversion R26 asks that figure to prevent.
//
// checkpointJob's own comment names this scenario as the defect it fixed, but
// the guard and its test cover only the nil-sync-target case of a job that
// left the queue. A wedged mount keeps its target: syncTargetFor goes through
// SnapshotJob, which hydrates a manifest from disk and does not touch the
// mount at all.
func TestCheckpointJob_DoesNotStampABarrierOverNoFiles(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	// Grounding: a job with no open file is exactly what a wedged Files()
	// looks like from here, and the target must still exist or this test
	// duplicates the nil-target case instead of covering this one.
	if application.syncTargetFor(job.ID) == nil {
		t.Fatal("the fixture has no sync target, so it exercises the nil-target guard " +
			"rather than the empty-file-set one")
	}
	if application.hasBarrierStamp(job.ID) {
		t.Fatal("the fixture was already stamped, so it cannot observe a new stamp")
	}

	application.checkpointJob(t.Context(), job.ID)

	if application.hasBarrierStamp(job.ID) {
		t.Error("a barrier that saw no files stamped last_barrier. On a wedged mount " +
			"Files() answers nil every interval, so the job reports a fresh barrier " +
			"timestamp forever while nothing has been fsynced")
	}
}

// TestCheckpointJob_KeepsThePendingByteFigureWhenTheBarrierFails pins the
// figure beside the stamp.
//
// checkpointJob used to reset the accumulator BEFORE the run, so an article
// written while the barrier was in flight would be charged to the next window.
// Nothing restored it when the run failed, so a job that stalled at phase 1
// with megabytes unsynced reported bytes_pending as 0 — and because the stall
// pauses it, nothing re-accumulated.
//
// The window is now retired by settleJobBytes on the success path, which keeps
// this property without the reset: a failed run simply never settles. The test
// is unchanged because the property it pins never was about the reset.
//
// The nil-target branch a few lines above declines to reset for exactly this
// reason: "two figures agreeing that nothing is at risk, at the moment when
// everything written since the last real barrier is". The failure branch did
// not apply the same rule.
func TestCheckpointJob_KeepsThePendingByteFigureWhenTheBarrierFails(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	application.noteJobBytes(job.ID, 400)
	if got := application.pendingBytesFor(job.ID); got != 400 {
		t.Fatalf("fixture accumulated %d bytes, want 400", got)
	}

	// The job has no open files, so this checkpoint claims nothing — the same
	// shape as a run that fails at phase 1, and what a wedged Files() produces
	// every interval.
	application.checkpointJob(t.Context(), job.ID)

	if got := application.pendingBytesFor(job.ID); got != 400 {
		t.Errorf("bytes_pending = %d after a barrier that claimed nothing, want 400. "+
			"Reporting zero beside a stale last_barrier says nothing is at risk at "+
			"the moment when everything written since the last real barrier is", got)
	}
}

// TestCheckpointJob_LeavesThePendingBytesWhenTheRunFails pins the figure on the
// path that actually reaches the settle: a barrier with real open files that
// fails at the commit.
//
// The sibling test above covers a run that claims nothing and returns early,
// never reaching the settle at all. This one gets as far as the commit and then
// fails, which is the case the arithmetic exists for — a job stalled at phase 1
// with megabytes unsynced reported zero bytes pending, and because the stall
// pauses it nothing re-accumulated.
func TestCheckpointJob_LeavesThePendingBytesWhenTheRunFails(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)
	writeFixtureArticle(t, application, job.ID, 0, 0)

	// Grounding: without an open file this takes the empty-set early return
	// and never resets, so it would pass without the restore existing.
	if len(application.syncTargetFor(job.ID).Files()) == 0 {
		t.Fatal("the fixture has no open file, so the run returns before the reset")
	}

	application.barrier = durability.NewBarrier(
		failingCommitStore{RunStore: application.runs},
		application.queue, application, slog.New(slog.DiscardHandler),
	)

	application.noteJobBytes(job.ID, 400)
	if got := application.pendingBytesFor(job.ID); got != 400 {
		t.Fatalf("fixture accumulated %d bytes, want 400", got)
	}

	application.checkpointJob(t.Context(), job.ID)

	if got := application.pendingBytesFor(job.ID); got < 400 {
		t.Errorf("bytes_pending = %d after a failed barrier, want at least 400. The "+
			"window was retired by a run that claimed nothing, so the figure reports "+
			"no bytes at risk while every byte written since the last real barrier "+
			"still is", got)
	}
}

// TestSettleJobBytes_SubtractsRatherThanClears pins the arithmetic, which is
// the only decision this helper makes.
//
// A barrier reads the accumulator before it runs, so articles written while it
// was in flight belong to the NEXT window. Clearing the entry on success would
// discard exactly those — the most recently written bytes, and the ones least
// likely to be on disk — so only the figure the barrier actually read comes off.
func TestSettleJobBytes_SubtractsRatherThanClears(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	// The window the barrier read before it started.
	application.noteJobBytes(job.ID, 400)
	pending := application.pendingBytesFor(job.ID)
	// 120 bytes arrived while that barrier was in flight.
	application.noteJobBytes(job.ID, 120)

	application.settleJobBytes(job.ID, pending)

	if got := application.pendingBytesFor(job.ID); got != 120 {
		t.Errorf("pending = %d, want 120 — clearing would drop the bytes written during "+
			"the run, which are the least likely of all to be on disk", got)
	}
}

// TestSettleJobBytes_IgnoresANonPositiveAmount pins the guard that keeps a
// job with nothing pending out of the map entirely.
func TestSettleJobBytes_IgnoresANonPositiveAmount(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	application.settleJobBytes(job.ID, 0)

	if got := application.pendingBytesFor(job.ID); got != 0 {
		t.Errorf("pending = %d after settling nothing, want 0", got)
	}
	if at := application.jobsAtRisk(); len(at) != 0 {
		t.Errorf("jobsAtRisk() = %v after settling nothing; the guard must not create "+
			"an entry for a job that never wrote", at)
	}
}

// TestSettleJobBytes_RemovesTheEntryWhenTheWindowIsFullyRetired keeps a settled
// job from lingering as a zero entry, which would leak one map entry per job
// ever downloaded.
func TestSettleJobBytes_RemovesTheEntryWhenTheWindowIsFullyRetired(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	application.noteJobBytes(job.ID, 400)
	application.settleJobBytes(job.ID, 400)

	application.barrierMu.Lock()
	_, present := application.jobBarrierBytes[job.ID]
	application.barrierMu.Unlock()
	if present {
		t.Error("a fully settled job kept its accumulator entry; the map grows by one " +
			"entry per job ever downloaded")
	}
}
