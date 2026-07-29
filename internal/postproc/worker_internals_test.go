package postproc

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Direct unit tests for the worker loop (run) and the per-job stage driver
// (processJob). Both are exercised indirectly by the Start/Process tests, but
// their shutdown and skip branches are easier to pin by calling them directly
// with a controlled context.

// runInBackground calls p.run() on its own goroutine and returns a channel
// closed when it returns, so tests can assert termination without hanging the
// suite if the loop fails to exit.
func runInBackground(p *PostProcessor) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.run()
	}()
	return done
}

func TestRun_ReturnsWhenWorkerContextAlreadyCancelled(t *testing.T) {
	p := New(Options{})

	ctx, cancel := context.WithCancel(t.Context())
	p.workerCtx, p.workerCancel = ctx, cancel
	cancel()

	select {
	case <-runInBackground(p):
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return with an already-cancelled worker context; " +
			"the pre-check before blocking on the queue is not firing")
	}
}

func TestRun_ProcessesQueuedJobThenExitsOnCancel(t *testing.T) {
	stage := newRecordStage("s1")
	p := New(Options{Stages: []Stage{stage}})

	ctx, cancel := context.WithCancel(t.Context())
	p.workerCtx, p.workerCancel = ctx, cancel
	t.Cleanup(cancel)

	p.Process(makeJob(t, "queued-job"))

	done := runInBackground(p)
	waitUntil(t, func() bool { return stage.CallCount() == 1 }, 2*time.Second,
		"run to pick up and process the queued job")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after its worker context was cancelled")
	}
}

// A job that arrives already failed must skip every stage. The orchestrator
// records a "skipped" entry rather than running the pipeline against a job a
// prior step has given up on.
func TestProcessJob_SkipsAllStagesWhenJobAlreadyFailed(t *testing.T) {
	stage := newRecordStage("s1")
	p := New(Options{Stages: []Stage{stage}})

	job := makeJob(t, "already-failed")
	job.FailMsg = "download incomplete"

	p.processJob(t.Context(), job)

	if stage.CallCount() != 0 {
		t.Errorf("stage ran %d times on an already-failed job, want 0", stage.CallCount())
	}

	var skipped *StageLogEntry
	for i := range job.StageLog {
		if job.StageLog[i].Stage == "skipped" {
			skipped = &job.StageLog[i]
			break
		}
	}
	if skipped == nil {
		t.Fatalf("no %q stage log entry recorded; got stages %v", "skipped", stageNames(job.StageLog))
	}
	if !strings.Contains(strings.Join(skipped.Lines, " "), "download incomplete") {
		t.Errorf("skip entry does not carry the failure reason: %v", skipped.Lines)
	}
}

func TestProcessJob_RunsStagesForHealthyJob(t *testing.T) {
	first := newRecordStage("first")
	second := newRecordStage("second")
	p := New(Options{Stages: []Stage{first, second}})

	job := makeJob(t, "healthy")
	p.processJob(t.Context(), job)

	if first.CallCount() != 1 || second.CallCount() != 1 {
		t.Errorf("stage call counts = (%d, %d), want (1, 1)",
			first.CallCount(), second.CallCount())
	}
	if len(job.StageLog) == 0 {
		t.Error("no stage log entries recorded for a healthy job")
	}
}
