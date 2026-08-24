package app

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/durability"
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
	application.noteJobBytes(job.ID, 4096) // bytes at risk: the precondition for the hazard

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
	application.noteJobBytes(job.ID, 4096) // bytes at risk: the precondition for the hazard

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
	application.noteJobBytes(job.ID, 4096) // bytes at risk: the precondition for the hazard

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
	application.noteJobBytes(job.ID, 4096) // bytes at risk: the precondition for the hazard

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
	application.noteJobBytes(job.ID, 4096) // bytes at risk: the precondition for the hazard
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
	application.noteJobBytes(job.ID, 4096) // bytes at risk: the precondition for the hazard

	unsafe := application.checkpointAllShare(t.Context(), time.Nanosecond)
	if _, ok := unsafe[job.ID]; !ok {
		t.Errorf("a job the sweep could not ack is absent from the unsafe set %v; the "+
			"reload would clear its emitted bits and re-fetch bytes already on disk", unsafe)
	}
}

// TestCheckpointJob_ReportsSafeWhenNothingWasWrittenSinceTheLastBarrier keeps
// the withheld set as narrow as the hazard.
//
// The skip is per JOB while the hazard is per ARTICLE. An article emitted and
// then cancelled by the old downloader's Stop was never written and is never
// acked, so withholding it strands it until a process restart — its file never
// completing and its job never finalizing. A job with an empty barrier
// accumulator has written nothing since its last successful barrier, so a clear
// can strand nothing, whatever happened to this checkpoint.
//
// Not exotic: perJobShare divides one budget among the jobs a sweep visits, so
// a queue with a few dozen open jobs gives each about 200ms and marks many of
// them unsafe on ordinary hardware.
func TestCheckpointJob_ReportsSafeWhenNothingWasWrittenSinceTheLastBarrier(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID, 0, 0)
	// Deliberately NO noteJobBytes: nothing is at risk.

	if got := application.pendingBytesFor(job.ID); got != 0 {
		t.Fatalf("fixture: %d bytes at risk, want 0 — this test is about the empty case", got)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if !application.checkpointJob(ctx, job.ID) {
		t.Error("a job with nothing written since its last successful barrier was " +
			"reported unsafe to clear. Withholding it strands articles that were " +
			"emitted and cancelled but never written, and those are never acked, " +
			"so the job cannot finalize until the process restarts")
	}
}

// TestJobsAtRisk_NamesOnlyTheJobsHoldingUnackedBytes covers the answer the
// sweep gives when it cannot enumerate jobs itself.
//
// A failed OpenJobIDs is not the transient listing error it looks like: submit
// bounds it with barrierOpTimeout, and in production it fails when a worker is
// parked in an fsync on a wedged mount — the state with the most unacked bytes.
// Answering "clear everything" there would let one timed-out listing undo the
// whole fix, so it answers from the accumulator instead.
func TestJobsAtRisk_NamesOnlyTheJobsHoldingUnackedBytes(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)

	if got := application.jobsAtRisk(); got != nil {
		t.Errorf("jobsAtRisk() = %v with an empty accumulator, want nil: no job has "+
			"written anything since its last barrier, so none needs withholding", got)
	}

	application.noteJobBytes(job.ID, 4096)
	// Inserted directly, not via noteJobBytes, which early-returns on n <= 0 and
	// would leave no entry at all — an assertion against an absent key passes
	// whether or not jobsAtRisk filters on n > 0.
	application.barrierMu.Lock()
	application.jobBarrierBytes["idle-job"] = 0
	application.barrierMu.Unlock()

	got := application.jobsAtRisk()
	if _, ok := got[job.ID]; !ok {
		t.Errorf("jobsAtRisk() = %v, want it to name %s, which holds unacked written bytes",
			got, job.ID)
	}
	if _, ok := got["idle-job"]; ok {
		t.Errorf("jobsAtRisk() = %v names a job with a zero entry; withholding it strands "+
			"articles that were emitted and cancelled but never written", got)
	}

	// A successful barrier settles the accumulator, so the job stops being at
	// risk without anything having to remember that it was.
	application.settleJobBytes(job.ID, 4096)
	// Empty rather than nil: the zero "idle-job" entry above is still present,
	// so the map is allocated and then filtered down to nothing. Either shape
	// means the same thing to ClearAllEmitted, which only does lookups.
	if got := application.jobsAtRisk(); len(got) != 0 {
		t.Errorf("jobsAtRisk() = %v after the window was settled, want no jobs", got)
	}
}

// blockingCommitStore parks a barrier inside Commit — phase 4, after the drain,
// the fsync and the stat — so a test can observe the accumulator at the one
// moment that matters: the run is genuinely in flight and nothing is durable.
type blockingCommitStore struct {
	durability.RunStore
	entered chan struct{}
	release chan struct{}
}

func (s blockingCommitStore) Commit(ctx context.Context, jobID string, arts []durability.DurableArticle) ([]durability.Collision, error) {
	close(s.entered)
	<-s.release
	return s.RunStore.Commit(ctx, jobID, arts)
}

// TestCheckpointJob_KeepsAJobAtRiskWhileItsBarrierIsInFlight is the pin for the
// window itself, observed through checkpointJob rather than the primitives.
//
// The unit tests below fix the CONTRACT of the read and the settle; this one
// fixes which of them checkpointJob calls, and when. That distinction is the
// whole defect: the old code's read was destructive, so between it and the
// restore-on-failure the job had no accumulator entry, and jobsAtRisk — which a
// reload consults when OpenJobIDs fails — could not name it.
//
// Nothing serialises the two. checkpointJob holds the per-job barrier mutex
// across its run, but the OpenJobIDs failure path never calls checkpointJob at
// all; it reads the accumulator directly, and barrierMu is a leaf.
func TestCheckpointJob_KeepsAJobAtRiskWhileItsBarrierIsInFlight(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 2)
	writeFixtureArticle(t, application, job.ID, 0, 0)

	// Grounding: with no open file the run returns before it ever reaches
	// Commit, and this test would pass without observing anything.
	if len(application.syncTargetFor(job.ID).Files()) == 0 {
		t.Fatal("the fixture has no open file, so the barrier never reaches Commit")
	}

	blocked := blockingCommitStore{
		RunStore: application.runs,
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	application.barrier = durability.NewBarrier(
		blocked, application.queue, application, slog.New(slog.DiscardHandler),
	)

	application.noteJobBytes(job.ID, 4096)

	done := make(chan bool, 1)
	go func() { done <- application.checkpointJob(t.Context(), job.ID) }()

	<-blocked.entered
	// The barrier is parked mid-run. Its bytes are on disk but not durable, and
	// this is precisely when a concurrent reload would consult jobsAtRisk.
	if _, ok := application.jobsAtRisk()[job.ID]; !ok {
		t.Error("a job whose barrier is in flight is absent from jobsAtRisk. A reload " +
			"whose OpenJobIDs failed here would clear its emitted bits, and if the " +
			"barrier then fails, bytes already on disk are re-fetched against a " +
			"changed server set — #417")
	}
	if application.nothingAtRisk(job.ID) {
		t.Error("a job whose barrier is in flight reported nothing at risk")
	}
	close(blocked.release)

	if safe := <-done; !safe {
		t.Fatal("the barrier failed; this test is about a run that succeeds")
	}
	// And the success really does retire it, or every reload after a healthy
	// checkpoint would stall the articles it is supposed to re-dispatch.
	if _, ok := application.jobsAtRisk()[job.ID]; ok {
		t.Error("a job whose barrier succeeded is still named at risk")
	}
}

// TestJobsAtRisk_NamesAJobWhoseBarrierIsStillInFlight closes the window
// CodeRabbit found on #428.
//
// The accumulator used to be read-and-cleared BEFORE barrier.Run and put back
// only if that run failed. Between those two points the job had no entry, so
// jobsAtRisk could not name it — and jobsAtRisk is exactly what a reload
// consults when OpenJobIDs fails, which is the wedged-mount case where a
// background barrier is most likely to be in flight and about to fail. The
// reload cleared the job's Emitted bits, the barrier then failed and restored
// the bytes too late, and #417 reproduced through a narrower door.
//
// Nothing serialised the two: checkpointJob holds the per-job barrier mutex
// across its run, but the OpenJobIDs failure path never calls checkpointJob at
// all — it reads the accumulator directly, and barrierMu is a leaf.
func TestJobsAtRisk_NamesAJobWhoseBarrierIsStillInFlight(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	application.noteJobBytes(job.ID, 4096)

	// The barrier reads its window and starts running. Nothing is durable yet.
	pending := application.pendingBytesFor(job.ID)
	if pending != 4096 {
		t.Fatalf("fixture: barrier read %d bytes, want 4096", pending)
	}

	if _, ok := application.jobsAtRisk()[job.ID]; !ok {
		t.Error("a job whose barrier is still in flight is absent from jobsAtRisk. A " +
			"reload whose OpenJobIDs failed would clear its emitted bits, and if that " +
			"barrier then fails its written bytes are re-fetched against a changed " +
			"server set — #417")
	}
	if application.nothingAtRisk(job.ID) {
		t.Error("a job whose barrier is still in flight reported nothing at risk")
	}

	// Only the run that earns it retires the window.
	application.settleJobBytes(job.ID, pending)
	if _, ok := application.jobsAtRisk()[job.ID]; ok {
		t.Error("a job whose barrier succeeded is still named at risk, so every reload " +
			"after a healthy checkpoint would stall the articles it must re-dispatch")
	}
}

// TestJobsAtRisk_KeepsAFailedBarriersBytesAtRisk is the other half: a barrier
// that does NOT succeed never calls settleJobBytes, so the figure stands.
//
// This replaces what restoreJobBytes used to pin. The property is the same —
// a failed barrier leaves the bytes at risk — but it now holds because nothing
// removed them, rather than because something put them back.
func TestJobsAtRisk_KeepsAFailedBarriersBytesAtRisk(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	application.noteJobBytes(job.ID, 4096)

	// A barrier reads its window and fails. settleJobBytes is not called.
	_ = application.pendingBytesFor(job.ID)

	if _, ok := application.jobsAtRisk()[job.ID]; !ok {
		t.Error("a job whose barrier failed is absent from jobsAtRisk; its written bytes " +
			"are unacked and a clear would offer them for re-fetch")
	}
	if got := application.pendingBytesFor(job.ID); got != 4096 {
		t.Errorf("pending = %d after a failed barrier, want 4096 — the figure is read as "+
			"reassurance and must not drop beside a last_barrier that did not move", got)
	}
}

// TestNothingAtRisk_TracksTheAccumulator pins the per-job predicate the unsafe
// arms narrow themselves with.
func TestNothingAtRisk_TracksTheAccumulator(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)

	if !application.nothingAtRisk(job.ID) {
		t.Error("a job that has written nothing was reported as having bytes at risk")
	}

	application.noteJobBytes(job.ID, 4096)
	if application.nothingAtRisk(job.ID) {
		t.Error("a job holding unacked written bytes was reported as having nothing at " +
			"risk, which would let a reload clear its emitted bits and re-fetch them")
	}

	// Reading the window is what a barrier does before it runs, and it must NOT
	// move this predicate — the run has not earned anything yet. That is what
	// keeps a barrier in flight, and a barrier that FAILED, from looking like
	// one that succeeded.
	pending := application.pendingBytesFor(job.ID)
	if application.nothingAtRisk(job.ID) {
		t.Error("reading the window cleared the at-risk verdict, so a barrier still in " +
			"flight — or one that failed — reads as a success")
	}

	// Only settling does, and only a successful barrier settles.
	application.settleJobBytes(job.ID, pending)
	if !application.nothingAtRisk(job.ID) {
		t.Error("a settled window still reports bytes at risk, which would stall the " +
			"articles a reload must re-dispatch")
	}
}

// TestCheckpointAllShare_ReportsNothingWhenEveryJobIsAcked is the other half:
// the ordinary reload must not withhold anything, or every settings change
// would stall the articles it was supposed to re-dispatch.
func TestCheckpointAllShare_ReportsNothingWhenEveryJobIsAcked(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID, 0, 0)
	application.noteJobBytes(job.ID, 4096) // bytes at risk: the precondition for the hazard

	unsafe := application.checkpointAllShare(t.Context(), reloadCheckpointTimeout)
	if len(unsafe) != 0 {
		t.Errorf("a healthy sweep reported %v as unsafe to clear; those articles would "+
			"not be re-dispatched until a later barrier", unsafe)
	}
}
