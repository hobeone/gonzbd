package postproc

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// callbackLog records which jobs each of OnJobDone and OnJobCancelled saw.
type callbackLog struct {
	mu        sync.Mutex
	done      []string
	cancelled []string
}

func (l *callbackLog) options(stages ...Stage) Options {
	return Options{
		Stages: stages,
		OnJobDone: func(j *Job) {
			l.mu.Lock()
			l.done = append(l.done, j.JobID())
			l.mu.Unlock()
		},
		OnJobCancelled: func(j *Job) {
			l.mu.Lock()
			l.cancelled = append(l.cancelled, j.JobID())
			l.mu.Unlock()
		},
	}
}

func (l *callbackLog) snapshot() (done, cancelled []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.done...), append([]string(nil), l.cancelled...)
}

func count(ids []string, id string) int {
	n := 0
	for _, x := range ids {
		if x == id {
			n++
		}
	}
	return n
}

// TestCancel_QueuedJobFiresOnJobCancelled: a job Cancel takes out of the queue
// is handed back through OnJobCancelled before Cancel returns, and never
// reaches OnJobDone.
func TestCancel_QueuedJobFiresOnJobCancelled(t *testing.T) {
	block := make(chan struct{})
	blocker := &recordStage{name: "blocker", block: block, started: make(chan struct{})}
	var log callbackLog
	p := startProcessor(t, log.options(blocker))

	p.Process(makeJob(t, "first"))
	<-blocker.started
	second := makeJob(t, "second")
	p.Process(second)

	if !p.CancelJob(second.Job) {
		t.Fatal("CancelJob(second) = false for a queued job")
	}
	if _, cancelled := log.snapshot(); count(cancelled, "second") != 1 {
		t.Fatalf("OnJobCancelled saw %v when Cancel returned, want second exactly once", cancelled)
	}

	close(block)
	waitUntil(t, func() bool {
		done, _ := log.snapshot()
		return count(done, "first") == 1
	}, 2*time.Second, "first to finish")
	done, cancelled := log.snapshot()
	if count(done, "second") != 0 {
		t.Errorf("OnJobDone saw %v, want no call for the cancelled job", done)
	}
	if count(cancelled, "first") != 0 || count(cancelled, "second") != 1 {
		t.Errorf("OnJobCancelled saw %v, want only second, once", cancelled)
	}
}

// heldStage returns only after its context is cancelled AND the test releases
// it, recording when it has returned, so a test can tell whether a callback
// ran while the stage was still running.
type heldStage struct {
	started  chan struct{}
	release  chan struct{}
	returned atomic.Bool
}

func (s *heldStage) Name() string { return "held" }

func (s *heldStage) Run(ctx context.Context, _ *Job) error {
	close(s.started)
	<-ctx.Done()
	<-s.release
	s.returned.Store(true)
	return ctx.Err()
}

// TestCancel_InFlightJobFiresOnJobCancelledAfterStageReturns: a running job
// Cancel interrupts is handed back through OnJobCancelled only once its stage
// has returned, and never reaches OnJobDone. The callback releases the job's
// launch claim, and that claim is what keeps the job's files from being torn
// down while a stage still uses them.
func TestCancel_InFlightJobFiresOnJobCancelledAfterStageReturns(t *testing.T) {
	stage := &heldStage{started: make(chan struct{}), release: make(chan struct{})}
	var sawReturned atomic.Bool
	fired := make(chan struct{})
	var once sync.Once
	var doneCalled atomic.Bool
	p := startProcessor(t, Options{
		Stages:    []Stage{stage},
		OnJobDone: func(*Job) { doneCalled.Store(true) },
		OnJobCancelled: func(*Job) {
			sawReturned.Store(stage.returned.Load())
			once.Do(func() { close(fired) })
		},
	})

	// Registered after startProcessor's Stop, so it runs first: a failing test
	// must still let the held stage return, or Stop waits on it forever.
	var releaseOnce sync.Once
	releaseStage := func() { releaseOnce.Do(func() { close(stage.release) }) }
	t.Cleanup(releaseStage)

	running := makeJob(t, "running")
	p.Process(running)
	<-stage.started
	if !p.CancelJob(running.Job) {
		t.Fatal("CancelJob(running) = false for an in-flight job")
	}

	select {
	case <-fired:
		t.Fatal("OnJobCancelled fired while the cancelled job's stage was still running")
	case <-time.After(200 * time.Millisecond):
	}

	releaseStage()
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("OnJobCancelled never fired for a job cancelled mid-processing")
	}
	if !sawReturned.Load() {
		t.Error("OnJobCancelled ran before the stage returned")
	}
	if doneCalled.Load() {
		t.Error("OnJobDone fired for a job cancelled mid-processing")
	}
}

// TestOnJobCancelled_NotFiredForACompletedJob: a job that runs to completion
// goes through OnJobDone only.
func TestOnJobCancelled_NotFiredForACompletedJob(t *testing.T) {
	var log callbackLog
	p := startProcessor(t, log.options(newRecordStage("a")))
	p.Process(makeJob(t, "ok"))
	waitUntil(t, func() bool {
		done, _ := log.snapshot()
		return count(done, "ok") == 1
	}, 2*time.Second, "ok to finish")
	if _, cancelled := log.snapshot(); len(cancelled) != 0 {
		t.Errorf("OnJobCancelled saw %v for a completed job", cancelled)
	}
}

// TestCancelJob_AnotherInstanceUnderTheSameID: CancelJob for one instance
// neither takes a queued job nor interrupts an in-flight one that is another
// instance under the same ID.
func TestCancelJob_AnotherInstanceUnderTheSameID(t *testing.T) {
	block := make(chan struct{})
	blocker := &recordStage{name: "blocker", block: block, started: make(chan struct{})}
	defer close(block)
	var log callbackLog
	p := startProcessor(t, log.options(blocker))

	running := makeJob(t, "running")
	p.Process(running)
	<-blocker.started
	queued := makeJob(t, "queued")
	p.Process(queued)

	if other := newQueueJob(t, running.JobID(), 0); p.CancelJob(other) {
		t.Error("CancelJob = true for another instance under the in-flight job's ID")
	}
	if other := newQueueJob(t, queued.JobID(), 0); p.CancelJob(other) {
		t.Error("CancelJob = true for another instance under the queued job's ID")
	}
	if !p.HasJob(queued.Job) {
		t.Error("the queued job left the queue after CancelJob for another instance under its ID")
	}
	if _, cancelled := log.snapshot(); len(cancelled) != 0 {
		t.Errorf("OnJobCancelled saw %v after CancelJob for other instances", cancelled)
	}
}

// TestCancel_UnknownJobFiresNothing: a CancelJob that finds nothing reports
// false and hands nothing back.
func TestCancel_UnknownJobFiresNothing(t *testing.T) {
	var log callbackLog
	p := startProcessor(t, log.options(newRecordStage("a")))
	if p.CancelJob(makeJob(t, "nope").Job) {
		t.Error("CancelJob(nope) = true for a job the processor never saw")
	}
	if p.CancelJob(nil) {
		t.Error("CancelJob(nil) = true")
	}
	if done, cancelled := log.snapshot(); len(done)+len(cancelled) != 0 {
		t.Errorf("callbacks fired for an unknown ID: done=%v cancelled=%v", done, cancelled)
	}
}
