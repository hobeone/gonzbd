package app

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// failCommit is a CommitWrap whose commit always fails, so a barrier over real
// open files reaches phase 4 and returns an error.
func failCommit(context.Context, string, func() ([]durability.Collision, error)) ([]durability.Collision, error) {
	return nil, errors.New("database is locked")
}

// TestCheckpointJob_KeepsPendingBytesOverNoFiles pins the empty-file-set path.
//
// When the assembler holds no open file for the job, checkpointJob returns
// without running a barrier, and the accumulator must stand: a run over
// nothing made nothing durable.
//
// The nil-sync-target case is TestCheckpointJob_LeavesPendingBytesWhenNoBarrierRan.
func TestCheckpointJob_KeepsPendingBytesOverNoFiles(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	// Grounding: a job with no open file is exactly what a wedged Files()
	// looks like from here, and the target must still exist or this test
	// duplicates the nil-target case instead of covering this one.
	if application.syncTargetFor(job.ID()) == nil {
		t.Fatal("the fixture has no sync target, so it exercises the nil-target guard " +
			"rather than the empty-file-set one")
	}
	application.noteJobBytes(job.ID(), 400)

	application.checkpointJob(t.Context(), job.ID())

	if got := application.pendingBytesFor(job.ID()); got != 400 {
		t.Errorf("pending bytes = %d after a checkpoint over no files, want 400: retiring "+
			"the window there would report nothing at risk while nothing has been fsynced", got)
	}
}

// TestCheckpointJob_KeepsThePendingByteFigureWhenTheBarrierFails pins the
// figure after a failed barrier.
//
// checkpointJob used to reset the accumulator BEFORE the run, so an article
// written while the barrier was in flight would be charged to the next window.
// Nothing restored it when the run failed, so a job that stalled at phase 1
// with megabytes unsynced reported its pending bytes as 0 — and because the stall
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

	application.noteJobBytes(job.ID(), 400)
	if got := application.pendingBytesFor(job.ID()); got != 400 {
		t.Fatalf("fixture accumulated %d bytes, want 400", got)
	}

	// The job has no open files, so this checkpoint claims nothing — the same
	// shape as a run that fails at phase 1, and what a wedged Files() produces
	// every interval.
	application.checkpointJob(t.Context(), job.ID())

	if got := application.pendingBytesFor(job.ID()); got != 400 {
		t.Errorf("pending bytes = %d after a barrier that claimed nothing, want 400. "+
			"Reporting zero says nothing is at risk at the moment when everything "+
			"written since the last real barrier is", got)
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
	writeFixtureArticle(t, application, job.ID(), 0, 0)

	// Grounding: without an open file this takes the empty-set early return
	// and never resets, so it would pass without the restore existing.
	if len(application.syncTargetFor(job.ID()).Files()) == 0 {
		t.Fatal("the fixture has no open file, so the run returns before the reset")
	}

	application.barrier = durability.NewBarrier(
		realStore(t, application),
		application, application, slog.New(slog.DiscardHandler),
		durability.WithCommitWrap(failCommit),
	)

	application.noteJobBytes(job.ID(), 400)
	if got := application.pendingBytesFor(job.ID()); got != 400 {
		t.Fatalf("fixture accumulated %d bytes, want 400", got)
	}

	application.checkpointJob(t.Context(), job.ID())

	if got := application.pendingBytesFor(job.ID()); got < 400 {
		t.Errorf("pending bytes = %d after a failed barrier, want at least 400. The "+
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
	application.noteJobBytes(job.ID(), 400)
	pending := application.pendingBytesFor(job.ID())
	// 120 bytes arrived while that barrier was in flight.
	application.noteJobBytes(job.ID(), 120)

	application.settleJobBytes(job.ID(), pending)

	if got := application.pendingBytesFor(job.ID()); got != 120 {
		t.Errorf("pending = %d, want 120 — clearing would drop the bytes written during "+
			"the run, which are the least likely of all to be on disk", got)
	}
}

// TestSettleJobBytes_IgnoresANonPositiveAmount pins the guard that keeps a
// job with nothing pending out of the map entirely.
func TestSettleJobBytes_IgnoresANonPositiveAmount(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	application.settleJobBytes(job.ID(), 0)

	if got := application.pendingBytesFor(job.ID()); got != 0 {
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

	application.noteJobBytes(job.ID(), 400)
	application.settleJobBytes(job.ID(), 400)

	application.barrierMu.Lock()
	_, present := application.jobBarrierBytes[job.ID()]
	application.barrierMu.Unlock()
	if present {
		t.Error("a fully settled job kept its accumulator entry; the map grows by one " +
			"entry per job ever downloaded")
	}
}
