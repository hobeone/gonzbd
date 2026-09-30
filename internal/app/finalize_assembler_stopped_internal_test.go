package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/job"
)

// drainCompletionAfterStop drives a completion through handleFileComplete the
// way Shutdown reaches it for a job the tick has not evicted: the queue is
// paused, checkpoint runs R6's clean-shutdown barrier, the job's Fetching
// lease is yielded, the assembler stopped — its exit drain flushes and closes
// the file without trimming it — and only then does watchCompletions deliver
// the completion.
//
// checkpoint runs before the yield, as stopWorkers does: a yield parks the
// job's lease and kicks the tick, whose reconcileResidency evicts the job,
// and the barrier skips a job with no resident manifest. Without the
// checkpoint the file has no durable runs at all.
func drainCompletionAfterStop(t *testing.T, application *Application, j *job.Job, checkpoint bool) *lockedBuffer {
	t.Helper()
	logs := closeFaultLogs(application)
	application.dispatcher.Pause()
	if checkpoint {
		application.shutdownCheckpoint()
		if !j.Progress().ArticleDone(0) {
			t.Fatal("the shutdown checkpoint did not ack the article; this fixture needs " +
				"its runs committed")
		}
	}
	if err := application.dispatcher.Yielded(j.ID()); err != nil {
		t.Fatal(err)
	}
	if application.syncTargetFor(j.ID()) == nil {
		t.Fatal("the job has no sync target; this test is about a job still resident, which " +
			"takes the stopped-assembler return rather than the nil-target one")
	}
	if err := application.assembler.Stop(); err != nil {
		t.Fatal(err)
	}
	application.handleFileComplete(t.Context(), FileComplete{JobID: j.ID(), FileIdx: 0})
	return logs
}

// TestHandleFileComplete_ACompletionDrainedAfterTheAssemblerStopsIsWithheld
// pins the resident half of a shutdown drain.
//
// The assembler's exit drain flushes and closes every open file, but does not
// trim it. A completion that reaches finalizeCompletedFile after that, for a
// job still resident, has had no barrier trim it, so it must not be marked
// complete: the queue save persists the flag, and the next start ships the
// file with pre-allocation's trailing bytes. Withholding it is ordinary at
// shutdown, so it must not log an Error or park the job.
func TestHandleFileComplete_ACompletionDrainedAfterTheAssemblerStopsIsWithheld(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 1)
	writeShortArticle(t, application, j)
	path := application.filePathFor(j.ID(), 0)

	logs := drainCompletionAfterStop(t, application, j, false)

	if j.Progress().FileComplete(0) {
		t.Errorf("the file was marked complete at %d bytes after the assembler stopped; no "+
			"barrier trimmed it, and the queue save persists the flag", fileSize(t, path))
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("a completion drained at shutdown logged an error; logs:\n%s", logs.String())
	}
	if st, ok := application.recoveryFiles(j.ID())[0]; !ok || st != finalizePending {
		t.Errorf("recovery state = %v, want the file pending — the completion is owed a "+
			"finalize, not a delivery", application.recoveryFiles(j.ID()))
	}
	if got := application.StallReason(j.ID()).Reason; got != "" || application.weParked(j.ID()) {
		t.Errorf("stall reason = %q, parked = %v; want the job not stalled for a shutdown",
			got, application.weParked(j.ID()))
	}
}

// TestResume_ACompletionDrainedAfterTheAssemblerStopsIsRederived drives the
// same drain through the next start: the queue save, then the resume sweep
// that rebuilds the job from its durable runs.
//
// ReplaceFromRuns clears Complete only on a file whose Done bits it clears, so
// a flag persisted at shutdown survives the restart on a file still carrying
// its trailing bytes. Withheld, the flag is re-derived instead:
// completeStrandedFiles trims and completes a file its runs fully resolve, and
// a file with no runs keeps its article Outstanding to be fetched again.
func TestResume_ACompletionDrainedAfterTheAssemblerStopsIsRederived(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		checkpoint bool
	}{
		{"runs committed by the shutdown checkpoint", true},
		{"no runs committed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			application, j := newDurabilityTestApp(t, 1, 1)
			application.dispatcher.Tick(t.Context())
			writeShortArticle(t, application, j)
			path := application.filePathFor(j.ID(), 0)

			drainCompletionAfterStop(t, application, j, tc.checkpoint)
			restartSweep(t, application, j)

			complete := j.Progress().FileComplete(0)
			size := fileSize(t, path)
			if complete && size != shortArticleBytes {
				t.Fatalf("after the restart the file is Complete at %d bytes, want %d: the "+
					"flag the shutdown drain set survived on an untrimmed file", size, shortArticleBytes)
			}
			if tc.checkpoint && !complete {
				t.Errorf("the restart did not complete a file its durable runs fully resolve; "+
					"completeStrandedFiles should trim and complete it (size %d)", size)
			}
			if !tc.checkpoint {
				if complete {
					t.Error("a file with no durable runs is Complete after the restart")
				}
				if j.Progress().ArticleDone(0) {
					t.Error("an article no run covers is Done after the restart; it is never fetched again")
				}
			}
		})
	}
}

// TestWithholdUntrimmed_AnswersByWhetherTheJobIsQueued pins the decision both
// untrimmed returns share: a queued job's completion is withheld with the
// reason routeFinalizeFailure routes on, and a departed job's is not, because
// completeFinalizedFile refuses it and nothing would ever retry it.
func TestWithholdUntrimmed_AnswersByWhetherTheJobIsQueued(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 1)
	why := fmt.Errorf("the assembler stopped before it was trimmed: %w", assembler.ErrAssemblerStopped)

	err := application.withholdUntrimmed(j.ID(), 0, why)
	if !errors.Is(err, ErrNotFinalized) || !errors.Is(err, assembler.ErrAssemblerStopped) {
		t.Errorf("withholdUntrimmed = %v for a queued job, want ErrNotFinalized wrapping the "+
			"reason, so the completion is withheld and routed without a halt", err)
	}

	if err := application.dispatcher.Remove(t.Context(), j.ID()); err != nil {
		t.Fatal(err)
	}
	if err := application.withholdUntrimmed(j.ID(), 0, why); err != nil {
		t.Errorf("withholdUntrimmed = %v for a job that has left the queue, want nil; "+
			"routeFinalizeFailure records it pending for a job no retry can reach", err)
	}
}

// TestStopWorkers_TheShutdownBarrierCoversAJobTheYieldWouldEvict pins where
// R6's clean-shutdown barrier sits against the Fetching yield.
//
// A yield parks the job's lease and kicks the dispatcher's tick, and that
// tick's reconcileResidency evicts a job that no longer holds its lease.
// Nothing orders the tick after the barrier, so the hook runs one at the
// barrier's start — the tick winning that race. A barrier that runs after the
// yield then finds no sync target, skips the job, and everything written
// since the last checkpoint is fetched again on the next start.
func TestStopWorkers_TheShutdownBarrierCoversAJobTheYieldWouldEvict(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 1)
	// stopWorkers yields only after a downloader stopped cleanly. Set before
	// the ticks, whose launched Fetching worker reads it.
	application.mu.Lock()
	application.downloader = &mockCancelWakeDownloader{}
	application.mu.Unlock()
	// Two ticks: the first starts the attempt at Fetching, the second grants
	// its lease, which is what makes the dispatcher count the job resident
	// and a later tick evict it.
	application.dispatcher.Tick(t.Context())
	application.dispatcher.Tick(t.Context())
	if !j.HoldsLease() {
		t.Fatal("the job holds no lease, so no tick would evict it and this test asserts nothing")
	}
	writeShortArticle(t, application, j)
	if j.Progress().ArticleDone(0) {
		t.Fatal("the article is already acked before the shutdown barrier")
	}
	application.dispatcher.Pause()
	application.checkpointHook = func() { application.dispatcher.Tick(context.Background()) }

	application.stopWorkers(5*time.Second, nil, barrierOnStop)

	if !j.Progress().ArticleDone(0) {
		t.Error("the clean-shutdown barrier did not ack the job's written article; the " +
			"yield let the tick evict the job first, and the next start fetches it again")
	}
	if j.HoldsLease() {
		t.Error("the Fetching job still holds its lease after stopWorkers; the yield loop " +
			"after the barrier did not run")
	}
}

// restartSweep persists the queue as Shutdown's final flush does, and runs the
// next start's resume sweep over the job.
//
// The job stays resident rather than being evicted and re-hydrated: this
// fixture never wrote a manifest to disk, and the sweep reads the same
// progress either way, because the flush has just written it to the row that
// hydration re-applies.
func restartSweep(t *testing.T, application *Application, j *job.Job) {
	t.Helper()
	if err := application.checkpointer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if row, ok := application.dispatcher.Row(j.ID()); !ok || row.View.State != job.Fetching {
		t.Fatalf("row = %+v, want the job Fetching so the resume sweep covers it", row.View)
	}
	if err := application.resumeAllJobs(t.Context()); err != nil {
		t.Fatal(err)
	}
}
