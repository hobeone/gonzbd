package app

import (
	"context"
	"testing"
	"time"
)

// checkpointJob's return answers one question: does this job hold
// written-but-unacked articles that clearing Emitted would strand? It is NOT
// "did a barrier run" — one exit is not an ack and is still safe, and one
// looks safe and is not. #417.
//
// ReloadDownloader clears Emitted bits for every resident job after a
// best-effort checkpoint. An article the assembler wrote but no barrier acked
// then looks outstanding, is re-fetched from a server set the user has just
// changed, and — if the new set cannot serve it — is marked permanently failed
// while its bytes sit on disk. The inflated failedBytes can reach
// RepairNoCapacity/RepairBeyondCapacity, both Hopeless(), aborting a job whose
// file was never damaged.

// TestCheckpointJob_ReportsUnsafeWhenTheContextIsAlreadySpent is the case the
// issue is about: the reload's per-job budget expired, so the barrier never
// ran, and the job's written-but-unacked articles must not be cleared.
//
// A cancelled context reaches OpenFiles — checkpointJob takes the per-job
// mutex and resolves the sync target before consulting any context — so this
// exercises the same arm budget exhaustion does, without having to starve a
// real budget.
func TestCheckpointJob_ReportsUnsafeWhenTheContextIsAlreadySpent(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID, 0, 0)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if application.checkpointJob(ctx, job.ID) {
		t.Error("a checkpoint whose context was already spent reported the job safe to " +
			"clear. Its written articles are unacked, so ClearAllEmitted would offer " +
			"them for re-fetch against the server set the user just changed")
	}
}

// TestCheckpointJob_ReportsSafeAfterASuccessfulBarrier is the ordinary path:
// the articles are acked, so nothing a clear could strand remains.
func TestCheckpointJob_ReportsSafeAfterASuccessfulBarrier(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID, 0, 0)

	if !application.checkpointJob(t.Context(), job.ID) {
		t.Error("a successful checkpoint reported the job unsafe to clear, which would " +
			"hold its Emitted bits until a later barrier for no reason")
	}
}

// TestCheckpointJob_ReportsUnsafeWhenNoBarrierRan covers the nil-target exit.
//
// The job left the queue while the assembler still holds its file, so
// checkpointAll keeps listing it and no barrier can run for it. Its written
// bytes are unacked exactly as in the timeout case.
func TestCheckpointJob_ReportsUnsafeWhenNoBarrierRan(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID, 0, 0)

	if err := application.queue.Remove(job.ID); err != nil {
		t.Fatal(err)
	}
	if application.syncTargetFor(job.ID) != nil {
		t.Fatal("the fixture still has a sync target, so this asserts nothing reachable")
	}

	if application.checkpointJob(t.Context(), job.ID) {
		t.Error("a checkpoint that ran no barrier at all reported the job safe to clear")
	}
}

// TestCheckpointJob_ReportsSafeAfterTheAssemblerStopped pins the one error that
// means "genuinely nothing" rather than "unknown".
//
// ErrAssemblerStopped is the ordinary end of every process. Folding it into the
// unsafe arm would hold a job's Emitted bits until a restart on every clean
// shutdown — and per this change's own design an article whose data died with
// the old downloader is never acked, so that stall would not self-clear.
// finalizeCompletedFile cases it out for the same reason (durability.go:868).
func TestCheckpointJob_ReportsSafeAfterTheAssemblerStopped(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID, 0, 0)

	if err := application.assembler.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if !application.checkpointJob(t.Context(), job.ID) {
		t.Error("a stopped assembler reported the job unsafe to clear. That is the " +
			"ordinary end of every process, and treating it as unknown strands the " +
			"job's Emitted bits until a restart")
	}
}

// TestCheckpointJob_ReportsSafeWithoutABarrier keeps the barrier-less path
// clearing as it does today.
//
// Unreachable in production — app.barrier is set unless the history repo or its
// DB is nil (app.go:487-491), and cmd/gonzbd/main.go fails the whole start if
// history.Open errors. Pinned so the test-only path does not silently start
// stranding Emitted bits.
func TestCheckpointJob_ReportsSafeWithoutABarrier(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID, 0, 0)
	application.barrier = nil

	if !application.checkpointJob(t.Context(), job.ID) {
		t.Error("a barrier-less application reported a job unsafe to clear; with no " +
			"barrier nothing ever acks, so its articles would never be re-dispatched")
	}
}

// TestCheckpointAllShare_ReportsTheJobsItCouldNotProtect is what
// ReloadDownloader consumes. A job the sweep could not ack must appear in the
// returned set, or the reload has no way to tell a checkpoint that covered
// every job from one that covered none — which is #417 itself.
//
// The budget is exhausted rather than the parent context cancelled, and the
// difference is load-bearing: a cancelled parent fails at OpenJobIDs before any
// job is visited, which is the listing-failure early return and answers nil by
// design. Starving the per-job share is what drives a job through
// checkpointJob and out the unsafe arm — the issue's first named cause.
func TestCheckpointAllShare_ReportsTheJobsItCouldNotProtect(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID, 0, 0)

	unsafe := application.checkpointAllShare(t.Context(), time.Nanosecond)
	if _, ok := unsafe[job.ID]; !ok {
		t.Errorf("a job the sweep could not ack is absent from the unsafe set %v; the "+
			"reload would clear its emitted bits and re-fetch bytes already on disk", unsafe)
	}
}

// TestCheckpointAllShare_ReportsNothingWhenEveryJobIsAcked is the other half:
// the ordinary reload must not withhold anything, or every settings change
// would stall the articles it was supposed to re-dispatch.
func TestCheckpointAllShare_ReportsNothingWhenEveryJobIsAcked(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID, 0, 0)

	unsafe := application.checkpointAllShare(t.Context(), reloadCheckpointTimeout)
	if len(unsafe) != 0 {
		t.Errorf("a healthy sweep reported %v as unsafe to clear; those articles would "+
			"not be re-dispatched until a later barrier", unsafe)
	}
}
