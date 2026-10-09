package postproc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/job"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newQueueJob builds a real *job.Job (with a valid Manifest/Progress)
// for testing.
func newQueueJob(t *testing.T, id string, pp int) *job.Job {
	t.Helper()
	j := job.New(id, id, job.PolicyFromPP(pp))
	m := job.NewManifest(nil)
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	return j
}

// makeJob creates a minimal Job backed by a job.Job for use in tests.
func makeJob(t *testing.T, name string) *Job {
	t.Helper()
	return &Job{
		Job: newQueueJob(t, name, 3), // Repair + Unpack + Delete (production default)
		PP:  3,
	}
}

// recordStage is a mock Stage that appends its name + job name to a shared
// log each time Run is called.
type recordStage struct {
	name      string
	mu        sync.Mutex
	calls     []string                                  // "<stageName>/<jobName>"
	returnErr error                                     // if non-nil, returned from Run
	block     chan struct{}                             // if non-nil, Run blocks until this is closed
	startOnce sync.Once                                 // ensures started channel is closed only once
	started   chan struct{}                             // if non-nil, closed when Run starts executing
	runFn     func(ctx context.Context, job *Job) error // if non-nil, called instead of returning returnErr
}

func newRecordStage(name string) *recordStage { return &recordStage{name: name} }

func (s *recordStage) Name() string { return s.name }

func (s *recordStage) Run(ctx context.Context, job *Job) error {
	if s.started != nil {
		s.startOnce.Do(func() {
			close(s.started)
		})
	}
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			s.mu.Lock()
			s.calls = append(s.calls, s.name+"/cancelled")
			s.mu.Unlock()
			return ctx.Err()
		}
	}
	s.mu.Lock()
	s.calls = append(s.calls, s.name+"/"+job.Name())
	s.mu.Unlock()
	if s.runFn != nil {
		return s.runFn(ctx, job)
	}
	return s.returnErr
}

func (s *recordStage) CallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *recordStage) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	copy(out, s.calls)
	return out
}

// startProcessor is a test helper that creates and starts a PostProcessor,
// and registers a t.Cleanup that calls Stop.
func startProcessor(t *testing.T, opts Options) *PostProcessor {
	t.Helper()
	p := New(opts)
	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := p.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return p
}

// waitUntil polls cond every pollInterval until it returns true or the
// deadline is reached.
func waitUntil(t *testing.T, cond func() bool, deadline time.Duration, msg string) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// Test 1: Stages run in registered order for a single job.
func TestStagesRunInOrder(t *testing.T) {
	var orderMu sync.Mutex
	var order []string
	makeOrderStage := func(name string) Stage {
		return &orderCapture{name: name, order: &order, mu: &orderMu}
	}

	var doneMu sync.Mutex
	var done []string
	p := startProcessor(t, Options{
		Stages: []Stage{makeOrderStage("A"), makeOrderStage("B"), makeOrderStage("C")},
		OnJobDone: func(j *Job) {
			doneMu.Lock()
			for _, e := range j.StageLog {
				done = append(done, e.Stage)
			}
			doneMu.Unlock()
		},
	})

	job := makeJob(t, "myjob")
	p.Process(job)

	waitUntil(t, func() bool {
		doneMu.Lock()
		defer doneMu.Unlock()
		return len(done) == 5 // download + A + B + C + summary
	}, 2*time.Second, "job to complete")

	orderMu.Lock()
	defer orderMu.Unlock()
	want := []string{"A", "B", "C"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i, v := range want {
		if order[i] != v {
			t.Errorf("order[%d] = %q, want %q", i, order[i], v)
		}
	}
}

// orderCapture is a lightweight stage for ordering tests.
type orderCapture struct {
	name  string
	order *[]string
	mu    *sync.Mutex
}

func (o *orderCapture) Name() string { return o.name }
func (o *orderCapture) Run(_ context.Context, _ *Job) error {
	o.mu.Lock()
	*o.order = append(*o.order, o.name)
	o.mu.Unlock()
	return nil
}

// Test 4: Pause halts processing; Resume continues without losing jobs.
// Test 5: Stop during in-flight stage: stage receives cancelled ctx; Stop
// returns only after worker exits.
func TestStopDuringInFlightStage(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{})
	blocker := &recordStage{name: "blocker", block: block, started: started}

	p := New(Options{Stages: []Stage{blocker}})
	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	job := makeJob(t, "blocking-job")
	p.Process(job)

	// Wait until the blocker stage is actually executing.
	select {
	case <-blocker.started:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for blocker stage to start")
	}

	stopDone := make(chan struct{})
	go func() {
		//nolint:errcheck // Stop error is intentionally ignored in test teardown goroutine
		p.Stop()
		close(stopDone)
	}()

	// Stop should unblock once the ctx propagates to the stage.
	select {
	case <-stopDone:
		// Good — Stop returned.
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return within 3 seconds")
	}

	// The stage must have seen the cancellation.
	calls := blocker.Calls()
	if len(calls) == 0 {
		t.Error("blocker stage was never called")
	}
}

// Test 6: Stage returning an error does NOT abort the pipeline; subsequent
// stages still run (they self-gate via ParError/UnpackError flags).
// This matches Python's behavior where ALL stages run even on failure.
func TestStageErrorContinuesPipeline(t *testing.T) {
	errStage := &recordStage{name: "fail", returnErr: errors.New("boom")}
	nextStage := newRecordStage("next")

	var wg sync.WaitGroup
	wg.Add(1)
	var capturedLog []StageLogEntry

	p := startProcessor(t, Options{
		Stages: []Stage{errStage, nextStage},
		OnJobDone: func(j *Job) {
			capturedLog = append(capturedLog, j.StageLog...)
			wg.Done()
		},
	})

	p.Process(makeJob(t, "erring-job"))
	wg.Wait()

	// download + fail + next + summary = 4
	if len(capturedLog) != 4 {
		t.Fatalf("StageLog has %d entries, want 4; entries: %v",
			len(capturedLog), stageNames(capturedLog))
	}
	if capturedLog[1].Err == nil {
		t.Error("fail stage log entry should have Err set")
	}
	// The "next" stage should STILL RUN (not be skipped) because the
	// pipeline no longer aborts on stage error.
	if capturedLog[2].Stage != "next" {
		t.Errorf("expected stage name 'next', got %q", capturedLog[2].Stage)
	}
	if nextStage.CallCount() != 1 {
		t.Errorf("next stage called %d times, want 1 (should run despite prior failure)", nextStage.CallCount())
	}
	if capturedLog[2].Err != nil {
		t.Errorf("next stage should not have error, got: %v", capturedLog[2].Err)
	}
}

// TestScriptRunsAfterRepairFailure verifies that the script stage runs even
// when the repair stage fails. This is critical for Sonarr/Radarr integration.
func TestScriptRunsAfterRepairFailure(t *testing.T) {
	repair := &recordStage{name: "repair", returnErr: errors.New("par2 failed")}
	script := newRecordStage("script")

	var wg sync.WaitGroup
	wg.Add(1)
	var capturedLog []StageLogEntry

	p := startProcessor(t, Options{
		Stages: []Stage{repair, script},
		OnJobDone: func(j *Job) {
			capturedLog = append(capturedLog, j.StageLog...)
			wg.Done()
		},
	})

	p.Process(makeJob(t, "repair-fail-job"))
	wg.Wait()

	// The script stage must have run even though repair failed.
	if script.CallCount() != 1 {
		t.Errorf("script stage called %d times, want 1 (must run on repair failure)", script.CallCount())
	}

	// Verify repair error is recorded.
	found := false
	for _, e := range capturedLog {
		if e.Stage == "repair" && e.Err != nil {
			found = true
		}
	}
	if !found {
		t.Error("expected repair stage error in StageLog")
	}
}

// TestScriptRunsAfterUnpackFailure verifies the script stage runs when
// unpack fails, receiving the appropriate error state.
func TestScriptRunsAfterUnpackFailure(t *testing.T) {
	repair := newRecordStage("repair")
	unpackStg := &recordStage{name: "unpack", returnErr: errors.New("extraction failed")}
	script := newRecordStage("script")

	var wg sync.WaitGroup
	wg.Add(1)

	p := startProcessor(t, Options{
		Stages: []Stage{repair, unpackStg, script},
		OnJobDone: func(j *Job) {
			wg.Done()
		},
	})

	p.Process(makeJob(t, "unpack-fail-job"))
	wg.Wait()

	if repair.CallCount() != 1 {
		t.Errorf("repair called %d times, want 1", repair.CallCount())
	}
	if unpackStg.CallCount() != 1 {
		t.Errorf("unpack called %d times, want 1", unpackStg.CallCount())
	}
	if script.CallCount() != 1 {
		t.Errorf("script called %d times, want 1 (must run after unpack failure)", script.CallCount())
	}
}

// TestAllStagesRunOnError verifies the full pipeline behavior: all stages
// run even when multiple stages fail. Each failed stage's error is recorded.
func TestAllStagesRunOnError(t *testing.T) {
	s1 := &recordStage{name: "quickcheck", returnErr: errors.New("qc fail")}
	s2 := &recordStage{name: "repair", returnErr: errors.New("par2 fail")}
	s3 := newRecordStage("deobfuscate")
	s4 := newRecordStage("sort")
	s5 := newRecordStage("finalize")
	s6 := newRecordStage("script")

	var wg sync.WaitGroup
	wg.Add(1)
	var capturedLog []StageLogEntry

	p := startProcessor(t, Options{
		Stages: []Stage{s1, s2, s3, s4, s5, s6},
		OnJobDone: func(j *Job) {
			capturedLog = append(capturedLog, j.StageLog...)
			wg.Done()
		},
	})

	p.Process(makeJob(t, "multi-fail"))
	wg.Wait()

	// All 6 stages should have run despite failures.
	for _, name := range []string{"quickcheck", "repair", "deobfuscate", "sort", "finalize", "script"} {
		found := false
		for _, e := range capturedLog {
			if e.Stage == name {
				found = true
			}
		}
		if !found {
			t.Errorf("stage %q not found in StageLog", name)
		}
	}

	// Verify error stages have Err set.
	for _, e := range capturedLog {
		switch e.Stage {
		case "quickcheck":
			if e.Err == nil {
				t.Error("quickcheck should have error")
			}
		case "repair":
			if e.Err == nil {
				t.Error("repair should have error")
			}
		case "deobfuscate", "sort", "finalize", "script":
			if e.Err != nil {
				t.Errorf("stage %q should not have error, got: %v", e.Stage, e.Err)
			}
		}
	}
}

// stageNames returns a slice of stage names from a StageLog for diagnostics.
func stageNames(log []StageLogEntry) []string {
	names := make([]string, len(log))
	for i, e := range log {
		names[i] = e.Stage
	}
	return names
}

// Test 7: Cancel on a queued-but-not-started job removes it from the queue.
func TestCancelQueuedJob(t *testing.T) {
	block := make(chan struct{})
	blocker := &recordStage{name: "blocker", block: block}

	var wg sync.WaitGroup
	wg.Add(1)

	p := startProcessor(t, Options{
		Stages: []Stage{blocker},
		OnJobDone: func(_ *Job) {
			wg.Done()
		},
	})

	// First job blocks the worker.
	first := makeJob(t, "first")
	p.Process(first)

	// Wait for worker to pick up first job.
	waitUntil(t, func() bool {
		p.busyMu.Lock()
		b := p.busy
		p.busyMu.Unlock()
		return b
	}, 2*time.Second, "worker to be busy on first job")

	// Enqueue a second job — it will wait in the queue.
	second := makeJob(t, "second")
	p.Process(second)

	// Cancel second before it starts.
	removed := p.CancelJob(second.Job)
	if !removed {
		t.Error("Cancel returned false, expected true")
	}

	// Unblock first job.
	close(block)
	wg.Wait()

	// Only first job should have been processed via OnJobDone.
	hist := p.History()
	found := false
	for _, j := range hist {
		if j.JobID() == "second" && len(j.StageLog) > 0 {
			found = true
		}
	}
	if found {
		t.Error("cancelled job appears to have been processed")
	}
}

// TestCancelInProgressJob verifies that Cancel actually aborts a job that is
// currently executing a stage, not just one still sitting in the pending
// queue. Before this fix, Cancel only removed pending jobs; an in-progress
// job's stage kept running to completion (holding open files / subprocesses
// in its job directory) even though the caller (e.g. app.RemoveJob) may
// concurrently delete that directory. The cancelled job must also not be
// finalized via OnJobDone; it is handed back through OnJobCancelled instead
// (TestCancel_InFlightJobFiresOnJobCancelledAfterStageReturns).
func TestCancelInProgressJob(t *testing.T) {
	block := make(chan struct{}) // never closed: only ctx cancellation should unblock the stage
	blocker := &recordStage{name: "blocker", block: block}

	var onJobDoneCalled atomic.Bool
	p := startProcessor(t, Options{
		Stages: []Stage{blocker},
		OnJobDone: func(_ *Job) {
			onJobDoneCalled.Store(true)
		},
	})

	job := makeJob(t, "running")
	p.Process(job)

	waitUntil(t, func() bool {
		p.busyMu.Lock()
		b := p.busy && p.currentJob == job
		p.busyMu.Unlock()
		return b
	}, 2*time.Second, "worker to be busy on running job")

	removed := p.CancelJob(job.Job)
	if !removed {
		t.Error("Cancel returned false for in-progress job, want true")
	}

	// The blocker stage must observe ctx cancellation and return promptly,
	// without the test ever closing `block`.
	waitUntil(t, func() bool {
		return blocker.CallCount() > 0
	}, 2*time.Second, "blocker stage to observe cancellation and return")

	calls := blocker.Calls()
	if len(calls) != 1 || calls[0] != "blocker/cancelled" {
		t.Errorf("blocker calls = %v, want [blocker/cancelled]", calls)
	}

	waitUntil(t, func() bool {
		p.busyMu.Lock()
		busy := p.busy
		p.busyMu.Unlock()
		return !busy
	}, 2*time.Second, "worker to become idle after cancelled job")

	if onJobDoneCalled.Load() {
		t.Error("OnJobDone fired for a job cancelled mid-processing")
	}
}

// Test 8: OnJobDone fires exactly once per job with full StageLog.
func TestOnJobDoneFiredOnce(t *testing.T) {
	s1 := newRecordStage("a")
	s2 := newRecordStage("b")

	var mu sync.Mutex
	firings := make(map[string]int)
	logs := make(map[string][]StageLogEntry)

	var wg sync.WaitGroup
	wg.Add(2)

	p := startProcessor(t, Options{
		Stages: []Stage{s1, s2},
		OnJobDone: func(j *Job) {
			mu.Lock()
			firings[j.JobID()]++
			logs[j.JobID()] = append([]StageLogEntry{}, j.StageLog...)
			mu.Unlock()
			wg.Done()
		},
	})

	p.Process(makeJob(t, "j1"))
	p.Process(makeJob(t, "j2"))

	wg.Wait()

	mu.Lock()
	defer mu.Unlock()

	for _, id := range []string{"j1", "j2"} {
		if firings[id] != 1 {
			t.Errorf("OnJobDone fired %d times for %s, want 1", firings[id], id)
		}
		if len(logs[id]) != 4 { // download + a + b + summary
			t.Errorf("job %s StageLog has %d entries, want 4", id, len(logs[id]))
		}
	}
}

// TestNoGoroutineLeak verifies that no goroutines remain after Stop returns.
func TestNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	p := New(Options{})
	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Wait for the worker goroutine to actually start, so there is something to
	// reap (otherwise the leak check would pass trivially).
	waitUntil(t, func() bool { return runtime.NumGoroutine() > before }, 2*time.Second, "worker goroutine to start")

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Goroutine teardown after Stop is asynchronous; poll until the count
	// returns to baseline rather than sleeping a fixed amount.
	waitUntil(t, func() bool { return runtime.NumGoroutine() <= before }, 2*time.Second,
		"goroutines to return to baseline after Stop")
}

// ---------------------------------------------------------------------------
// ppQueue unit tests
// ---------------------------------------------------------------------------

func TestPPQueueOrdering(t *testing.T) {
	q := newPPQueue()
	names := []string{"j1", "j2", "j3"}
	for _, n := range names {
		q.Push(&Job{Job: newQueueJob(t, n, 0)})
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	for _, want := range names {
		job, ok := q.Pop(ctx, nil)
		if !ok {
			t.Fatalf("Pop returned false, want job %q", want)
		}
		if job.Name() != want {
			t.Errorf("got job %q, want %q", job.Name(), want)
		}
	}
}

func TestPPQueueCancel(t *testing.T) {
	q := newPPQueue()
	a := newQueueJob(t, "a", 0)
	q.Push(&Job{Job: a})
	q.Push(&Job{Job: newQueueJob(t, "b", 0)})
	if _, ok := q.CancelJob(a); !ok {
		t.Error("CancelJob(a) = false, want true")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	job, ok := q.Pop(ctx, nil)
	if !ok || job.JobID() != "b" {
		t.Errorf("expected 'b', got ok=%v job=%v", ok, job)
	}

	if _, ok := q.CancelJob(newQueueJob(t, "does-not-exist", 0)); ok {
		t.Error("CancelJob of a job never queued returned true")
	}
}

func TestPPQueuePopCancelledCtx(t *testing.T) {
	q := newPPQueue()
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // already done

	_, ok := q.Pop(ctx, nil)
	if ok {
		t.Error("Pop with cancelled ctx returned ok=true")
	}
}

func TestEmptyMethod(t *testing.T) {
	p := startProcessor(t, Options{})
	waitUntil(t, p.Empty, 2*time.Second, "processor to be idle")
	if !p.Empty() {
		t.Error("Empty() = false on idle processor")
	}
}

// ---------- TestPipeline_ConcurrentJobSubmission ----------

// TestPipeline_ConcurrentJobSubmission submits 10 jobs concurrently from
// separate goroutines and verifies that all 10 complete without panics or
// data races. This validates that the single-worker ppQueue handles
// concurrent Process() calls safely.
func TestPipeline_ConcurrentJobSubmission(t *testing.T) {
	stage := newRecordStage("s")

	const n = 10
	var doneCount atomic.Int32
	var wg sync.WaitGroup
	wg.Add(n)

	p := startProcessor(t, Options{
		Stages: []Stage{stage},
		OnJobDone: func(_ *Job) {
			doneCount.Add(1)
			wg.Done()
		},
	})

	// Submit n jobs concurrently.
	var submitWg sync.WaitGroup
	for i := range n {
		submitWg.Go(func() {
			name := fmt.Sprintf("concurrent-%d", i)
			p.Process(makeJob(t, name))
		})
	}
	submitWg.Wait()

	// Wait for all jobs to complete.
	waitUntil(t, func() bool {
		return doneCount.Load() == n
	}, 5*time.Second, "all concurrent jobs to complete")

	wg.Wait()

	if got := doneCount.Load(); got != n {
		t.Errorf("completed %d jobs, want %d", got, n)
	}
	if got := stage.CallCount(); got != n {
		t.Errorf("stage called %d times, want %d", got, n)
	}
}

// ---------- PP enforcement ----------

func TestPPEnforcement_PP0SkipsRepairAndUnpack(t *testing.T) {
	repair := newRecordStage("repair")
	unpack := newRecordStage("unpack")
	finalize := newRecordStage("finalize")

	var wg sync.WaitGroup
	wg.Add(1)
	p := startProcessor(t, Options{
		Stages:    []Stage{repair, unpack, finalize},
		OnJobDone: func(_ *Job) { wg.Done() },
	})

	job := &Job{Job: newQueueJob(t, "pp0", 0), PP: 0}
	p.Process(job)
	wg.Wait()

	if repair.CallCount() != 0 {
		t.Errorf("repair ran %d times with PP=0, want 0", repair.CallCount())
	}
	if unpack.CallCount() != 0 {
		t.Errorf("unpack ran %d times with PP=0, want 0", unpack.CallCount())
	}
	if finalize.CallCount() != 1 {
		t.Errorf("finalize ran %d times with PP=0, want 1", finalize.CallCount())
	}
}

func TestPPEnforcement_PP1SkipsUnpack(t *testing.T) {
	repair := newRecordStage("repair")
	unpack := newRecordStage("unpack")
	finalize := newRecordStage("finalize")

	var wg sync.WaitGroup
	wg.Add(1)
	p := startProcessor(t, Options{
		Stages:    []Stage{repair, unpack, finalize},
		OnJobDone: func(_ *Job) { wg.Done() },
	})

	job := &Job{Job: newQueueJob(t, "pp1", 1), PP: 1}
	p.Process(job)
	wg.Wait()

	if repair.CallCount() != 1 {
		t.Errorf("repair ran %d times with PP=1, want 1", repair.CallCount())
	}
	if unpack.CallCount() != 0 {
		t.Errorf("unpack ran %d times with PP=1, want 0", unpack.CallCount())
	}
	if finalize.CallCount() != 1 {
		t.Errorf("finalize ran %d times with PP=1, want 1", finalize.CallCount())
	}
}

// TestPPEnforcement_PP2RunsRepairAndUnpack verifies that PP=2 enables both
// repair AND unpack stages (delete implies unpack implies repair).
func TestPPEnforcement_PP2RunsRepairAndUnpack(t *testing.T) {
	quickcheck := newRecordStage("quickcheck")
	repair := newRecordStage("repair")
	unpack := newRecordStage("unpack")
	deobfuscate := newRecordStage("deobfuscate")
	sort := newRecordStage("sort")
	finalize := newRecordStage("finalize")
	script := newRecordStage("script")

	var wg sync.WaitGroup
	wg.Add(1)
	p := startProcessor(t, Options{
		Stages:    []Stage{quickcheck, repair, unpack, deobfuscate, sort, finalize, script},
		OnJobDone: func(_ *Job) { wg.Done() },
	})

	job := &Job{Job: newQueueJob(t, "pp2", 2), PP: 2}
	p.Process(job)
	wg.Wait()

	if quickcheck.CallCount() != 1 {
		t.Errorf("quickcheck ran %d times with PP=2, want 1", quickcheck.CallCount())
	}
	if repair.CallCount() != 1 {
		t.Errorf("repair ran %d times with PP=2, want 1", repair.CallCount())
	}
	if unpack.CallCount() != 1 {
		t.Errorf("unpack ran %d times with PP=2, want 1", unpack.CallCount())
	}
	// Non-gated stages always run.
	if deobfuscate.CallCount() != 1 {
		t.Errorf("deobfuscate ran %d times with PP=2, want 1", deobfuscate.CallCount())
	}
	if sort.CallCount() != 1 {
		t.Errorf("sort ran %d times with PP=2, want 1", sort.CallCount())
	}
	if finalize.CallCount() != 1 {
		t.Errorf("finalize ran %d times with PP=2, want 1", finalize.CallCount())
	}
	if script.CallCount() != 1 {
		t.Errorf("script ran %d times with PP=2, want 1", script.CallCount())
	}
}

func TestPPEnforcement_PP3RunsAll(t *testing.T) {
	repair := newRecordStage("repair")
	unpack := newRecordStage("unpack")
	finalize := newRecordStage("finalize")

	var wg sync.WaitGroup
	wg.Add(1)
	p := startProcessor(t, Options{
		Stages:    []Stage{repair, unpack, finalize},
		OnJobDone: func(_ *Job) { wg.Done() },
	})

	job := &Job{Job: newQueueJob(t, "pp3", 3), PP: 3}
	p.Process(job)
	wg.Wait()

	if repair.CallCount() != 1 {
		t.Errorf("repair ran %d times with PP=3, want 1", repair.CallCount())
	}
	if unpack.CallCount() != 1 {
		t.Errorf("unpack ran %d times with PP=3, want 1", unpack.CallCount())
	}
	if finalize.CallCount() != 1 {
		t.Errorf("finalize ran %d times with PP=3, want 1", finalize.CallCount())
	}
}

// TestPPEnforcement_PP0_NonGatedStagesAlwaysRun verifies that deobfuscate,
// sort, finalize, and script stages always run regardless of PP level.
func TestPPEnforcement_PP0_NonGatedStagesAlwaysRun(t *testing.T) {
	quickcheck := newRecordStage("quickcheck")
	repair := newRecordStage("repair")
	unpack := newRecordStage("unpack")
	deobfuscate := newRecordStage("deobfuscate")
	sort := newRecordStage("sort")
	finalize := newRecordStage("finalize")
	script := newRecordStage("script")
	extCleanup := newRecordStage("extension_cleanup")
	sampleCleanup := newRecordStage("sample_cleanup")

	var wg sync.WaitGroup
	wg.Add(1)
	p := startProcessor(t, Options{
		Stages: []Stage{
			quickcheck, repair, unpack,
			deobfuscate, sort,
			extCleanup, sampleCleanup,
			finalize, script,
		},
		OnJobDone: func(_ *Job) { wg.Done() },
	})

	job := &Job{Job: newQueueJob(t, "pp0-full", 0), PP: 0}
	p.Process(job)
	wg.Wait()

	// PP-gated stages should be skipped.
	if quickcheck.CallCount() != 0 {
		t.Errorf("quickcheck ran %d times with PP=0, want 0", quickcheck.CallCount())
	}
	if repair.CallCount() != 0 {
		t.Errorf("repair ran %d times with PP=0, want 0", repair.CallCount())
	}
	if unpack.CallCount() != 0 {
		t.Errorf("unpack ran %d times with PP=0, want 0", unpack.CallCount())
	}
	// Non-gated stages must ALWAYS run.
	for _, tc := range []struct {
		name  string
		stage *recordStage
	}{
		{"deobfuscate", deobfuscate},
		{"sort", sort},
		{"extension_cleanup", extCleanup},
		{"sample_cleanup", sampleCleanup},
		{"finalize", finalize},
		{"script", script},
	} {
		if tc.stage.CallCount() != 1 {
			t.Errorf("%s ran %d times with PP=0, want 1 (non-gated stages always run)", tc.name, tc.stage.CallCount())
		}
	}
}

func TestShouldSkipForPP(t *testing.T) {
	tests := []struct {
		stage string
		pp    int
		want  bool
	}{
		{"quickcheck", 0, true},
		{"quickcheck", 1, false},
		{"repair", 0, true},
		{"repair", 1, false},
		{"repair", 2, false},
		{"unpack", 0, true},
		{"unpack", 1, true},
		{"unpack", 2, false},
		{"unpack", 3, false},
		{"finalize", 0, false},
		{"script", 0, false},
		{"deobfuscate", 0, false},
		{"sort", 0, false},
		{"extension_cleanup", 0, false},
		{"sample_cleanup", 0, false},
		{"recover_par2_names", 0, false},
		{"par2_cleanup", 0, false},
	}
	for _, tt := range tests {
		got := shouldSkipForPP(tt.stage, tt.pp)
		if got != tt.want {
			t.Errorf("shouldSkipForPP(%q, %d) = %v, want %v", tt.stage, tt.pp, got, tt.want)
		}
	}
}

// TestQuickCheckPassedSkipsRepair verifies that when QuickCheck comes back clean
// by the quickcheck stage, the repair stage is skipped in the pipeline.
func TestQuickCheckPassedSkipsRepair(t *testing.T) {
	t.Parallel()
	quickcheck := newRecordStage("quickcheck")
	quickcheck.runFn = func(_ context.Context, job *Job) error {
		// Simulate QuickCheck setting the flag.
		job.QuickCheck = QuickCheckClean
		return nil
	}
	repair := newRecordStage("repair")
	finalize := newRecordStage("finalize")

	var doneMu sync.Mutex
	var doneJobs []string
	p := startProcessor(t, Options{
		Stages: []Stage{quickcheck, repair, finalize},
		OnJobDone: func(j *Job) {
			doneMu.Lock()
			doneJobs = append(doneJobs, j.JobID())
			doneMu.Unlock()
		},
	})

	job := makeJob(t, "qc-skip")
	p.Process(job)

	waitUntil(t, func() bool {
		doneMu.Lock()
		defer doneMu.Unlock()
		return len(doneJobs) == 1
	}, 2*time.Second, "job to complete")

	if quickcheck.CallCount() != 1 {
		t.Fatalf("quickcheck ran %d times, want 1", quickcheck.CallCount())
	}
	// The repair stage still runs through the pipeline; it's the real
	// RepairStage.Run that checks job.QuickCheck. Our mock just records.
	if repair.CallCount() != 1 {
		t.Fatalf("repair ran %d times, want 1", repair.CallCount())
	}
	if finalize.CallCount() != 1 {
		t.Fatalf("finalize ran %d times, want 1", finalize.CallCount())
	}
	// The outcome should be recorded on the job.
	if job.QuickCheck != QuickCheckClean {
		t.Errorf("QuickCheck = %s after the quickcheck stage, want clean", job.QuickCheck)
	}
}

// TestScriptCanFail_True verifies that a non-zero script exit sets job.FailMsg
// and records an error on the script stage when ScriptCanFail is true, while
// the pipeline completes both stages.
func TestScriptCanFail_True(t *testing.T) {
	t.Parallel()
	scriptDir := t.TempDir()
	writeScript(t, filepath.Join(scriptDir, "test.sh"), []byte("#!/bin/sh\nexit 1\n"))

	script := NewScriptStage(scriptDir, t.TempDir(), "test", "", "")
	script.SetScriptCanFail(true)
	if !script.scriptCanFail.Load() {
		t.Error("ScriptCanFail should be true")
	}

	finalize := newRecordStage("finalize")

	var doneMu sync.Mutex
	var doneJob *Job
	p := startProcessor(t, Options{
		Stages: []Stage{finalize, script},
		OnJobDone: func(j *Job) {
			doneMu.Lock()
			doneJob = j
			doneMu.Unlock()
		},
	})

	job := makeJob(t, "script-can-fail")
	job.Script = "test.sh"
	p.Process(job)

	waitUntil(t, func() bool {
		doneMu.Lock()
		defer doneMu.Unlock()
		return doneJob != nil
	}, 2*time.Second, "job to complete")

	// The script stage errored with ScriptCanFail=true and set FailMsg, while
	// the pipeline still ran finalize and completed.
	if finalize.CallCount() != 1 {
		t.Errorf("finalize ran %d times, want 1", finalize.CallCount())
	}
	doneMu.Lock()
	gotFailMsg := doneJob.FailMsg
	doneMu.Unlock()
	if gotFailMsg == "" {
		t.Error("expected job.FailMsg to be set when ScriptCanFail=true and script exits non-zero")
	}
}

// ---------------------------------------------------------------------------
// L11: Empty job pre-check tests
// ---------------------------------------------------------------------------

// TestPreCheck_EmptyDir verifies that a job with an empty download directory
// is rejected before any stages run, with FailMsg set.
func TestPreCheck_EmptyDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir() // exists but empty

	stage := newRecordStage("repair")
	var wg sync.WaitGroup
	wg.Add(1)
	var capturedJob *Job

	p := startProcessor(t, Options{
		Stages: []Stage{stage},
		OnJobDone: func(j *Job) {
			capturedJob = j
			wg.Done()
		},
	})

	job := &Job{
		Job:         newQueueJob(t, "empty", 3),
		PP:          3,
		DownloadDir: dir,
	}
	p.Process(job)
	wg.Wait()

	if stage.CallCount() != 0 {
		t.Errorf("stage ran %d times, want 0 (empty dir should skip)", stage.CallCount())
	}
	if capturedJob.FailMsg == "" {
		t.Error("FailMsg should be set for empty directory")
	}
	// StageLog should have download + pre-check + summary.
	foundPreCheck := false
	for _, e := range capturedJob.StageLog {
		if e.Stage == "pre-check" {
			foundPreCheck = true
		}
	}
	if !foundPreCheck {
		t.Error("expected 'pre-check' stage in StageLog")
	}
}

// TestPreCheck_MissingDir verifies that a job with a non-existent download
// directory is rejected with FailMsg mentioning "unavailable".
func TestPreCheck_MissingDir(t *testing.T) {
	t.Parallel()

	stage := newRecordStage("repair")
	var wg sync.WaitGroup
	wg.Add(1)
	var capturedJob *Job

	p := startProcessor(t, Options{
		Stages: []Stage{stage},
		OnJobDone: func(j *Job) {
			capturedJob = j
			wg.Done()
		},
	})

	job := &Job{
		Job:         newQueueJob(t, "missing", 3),
		PP:          3,
		DownloadDir: "/nonexistent/path/xyz",
	}
	p.Process(job)
	wg.Wait()

	if stage.CallCount() != 0 {
		t.Errorf("stage ran %d times, want 0 (missing dir should skip)", stage.CallCount())
	}
	if capturedJob.FailMsg == "" {
		t.Error("FailMsg should be set for missing directory")
	}
}

// TestPreCheck_UnsetDirSkipsGuard verifies that when DownloadDir is unset
// (empty string), the pre-check guard is skipped and stages run normally.
// This supports unit tests and dry-run pipelines that don't need a real dir.
func TestPreCheck_UnsetDirSkipsGuard(t *testing.T) {
	t.Parallel()

	stage := newRecordStage("repair")
	var wg sync.WaitGroup
	wg.Add(1)

	p := startProcessor(t, Options{
		Stages:    []Stage{stage},
		OnJobDone: func(_ *Job) { wg.Done() },
	})

	job := &Job{
		Job: newQueueJob(t, "no-dir", 3),
		PP:  3,
		// DownloadDir deliberately empty
	}
	p.Process(job)
	wg.Wait()

	if stage.CallCount() != 1 {
		t.Errorf("stage ran %d times, want 1 (unset DownloadDir should not trigger pre-check)", stage.CallCount())
	}
}

type mockPPStage struct {
	name    string
	runFunc func(ctx context.Context, job *Job) error
}

func (m *mockPPStage) Name() string                            { return m.name }
func (m *mockPPStage) Run(ctx context.Context, job *Job) error { return m.runFunc(ctx, job) }

func TestPostProc_HelperMethods(t *testing.T) {
	t.Parallel()

	t.Run("buildPreambleLog", func(t *testing.T) {
		now := time.Now()
		j := job.New("job1", "TestJob", job.Policy{})
		m := job.NewManifest(nil)
		if err := j.AttachContent(m); err != nil {
			t.Fatalf("AttachContent: %v", err)
		}
		if err := j.MarkJobStarted(now); err != nil {
			t.Fatalf("MarkJobStarted: %v", err)
		}
		if err := j.MarkDownloadFinished(now.Add(5 * time.Second)); err != nil {
			t.Fatalf("MarkDownloadFinished: %v", err)
		}
		job := &Job{
			Job:         j,
			DownloadDir: t.TempDir(),
		}

		// Write a dummy file to the download directory to populate the download file list
		dummyFile := filepath.Join(job.DownloadDir, "file.txt")
		if err := os.WriteFile(dummyFile, []byte("test"), 0o644); err != nil {
			t.Fatal(err)
		}

		// Case 1: No Direct Unpack data
		entries := buildPreambleLog(job)
		if len(entries) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(entries))
		}
		if entries[0].Stage != "download" {
			t.Errorf("expected stage 'download', got %q", entries[0].Stage)
		}
		if entries[0].Elapsed != 5*time.Second {
			t.Errorf("expected elapsed 5s, got %v", entries[0].Elapsed)
		}

		// Case 2: With Direct Unpack data
		job.DirectUnpackSets = map[string]directunpack.SuccessSet{
			"set1": {
				ExtractedFiles: []string{filepath.Join(job.DownloadDir, "extracted.txt")},
				RarParts:       []string{"part1.rar", "part2.rar"},
			},
		}
		job.DirectUnpackFailures = map[string]directunpack.FailedSet{
			"set2": {Reason: "password error"},
		}

		entries = buildPreambleLog(job)
		if len(entries) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(entries))
		}
		if entries[1].Stage != "direct unpack" {
			t.Errorf("expected stage 'direct unpack', got %q", entries[1].Stage)
		}
		// Verify lines content contains the sets/failures information
		linesStr := strings.Join(entries[1].Lines, "\n")
		if !strings.Contains(linesStr, `✓ Set "set1": extracted 1 file(s) from 2 volume(s)`) {
			t.Errorf("expected line showing set1 success, got lines: %v", entries[1].Lines)
		}
		if !strings.Contains(linesStr, `Error: Set "set2" failed → password error (will retry in normal unpack)`) {
			t.Errorf("expected line showing set2 failure, got lines: %v", entries[1].Lines)
		}
	})

	t.Run("buildSummaryEntry", func(t *testing.T) {
		now := time.Now()
		job := &Job{
			Job: newQueueJob(t, "job1", 0),
			StageLog: []StageLogEntry{
				{
					Stage:   "repair",
					Started: now,
					Elapsed: 1500 * time.Millisecond,
					Err:     nil,
				},
				{
					Stage:   "unpack",
					Started: now.Add(2 * time.Second),
					Elapsed: 2500 * time.Millisecond,
					Err:     fmt.Errorf("some unpack error"),
				},
			},
			FailMsg:  "Failed",
			FinalDir: "/final/destination/dir",
		}

		entry := buildSummaryEntry(job)
		if entry.Stage != "summary" {
			t.Errorf("expected stage 'summary', got %q", entry.Stage)
		}
		if entry.Elapsed != 4500*time.Millisecond {
			t.Errorf("expected total duration 4.5s, got %v", entry.Elapsed)
		}

		linesStr := strings.Join(entry.Lines, "\n")
		if !strings.Contains(linesStr, "Pipeline Failed in 4.5s → /final/destination/dir") {
			t.Errorf("expected header line, got: %v", entry.Lines)
		}
		if !strings.Contains(linesStr, "✓ repair (1.5s)") {
			t.Errorf("expected repair line, got: %v", entry.Lines)
		}
		if !strings.Contains(linesStr, "✗ unpack (2.5s — some unpack error)") {
			t.Errorf("expected unpack line, got: %v", entry.Lines)
		}
	})

	t.Run("runStage", func(t *testing.T) {
		p := &PostProcessor{
			log: slog.Default(),
		}

		job := &Job{
			Job: newQueueJob(t, "job1", 0), // PP=0 skips repair
			PP:  0,
		}

		mockStage := &mockPPStage{
			name: "repair",
			runFunc: func(ctx context.Context, job *Job) error {
				return nil
			},
		}

		// 1. Skip check
		entry, abort := p.runStage(t.Context(), mockStage, job)
		if abort {
			t.Error("expected abort to be false when skipping stage")
		}
		if len(entry.Lines) == 0 || !strings.Contains(entry.Lines[0], "Skipped: PP=0") {
			t.Errorf("expected skipped log entry, got %v", entry)
		}

		// 2. Normal success run
		job.PP = 3 // PP=3 runs repair
		var stageRan bool
		mockStage.runFunc = func(ctx context.Context, job *Job) error {
			stageRan = true
			job.OutputLines = append(job.OutputLines, "stage output line")
			return nil
		}

		entry, abort = p.runStage(t.Context(), mockStage, job)
		if abort {
			t.Error("expected abort to be false")
		}
		if !stageRan {
			t.Error("expected stage.Run to be executed")
		}
		if entry.Err != nil {
			t.Errorf("expected no error, got %v", entry.Err)
		}
		if len(entry.Lines) != 1 || entry.Lines[0] != "stage output line" {
			t.Errorf("expected stage output line in entry lines, got %v", entry.Lines)
		}

		// 3. Normal error run
		mockStage.runFunc = func(ctx context.Context, job *Job) error {
			return fmt.Errorf("stage execution failed")
		}
		entry, abort = p.runStage(t.Context(), mockStage, job)
		if abort {
			t.Error("expected abort to be false on error")
		}
		if entry.Err == nil || entry.Err.Error() != "stage execution failed" {
			t.Errorf("expected error in entry, got %v", entry.Err)
		}

		// 4. Abort on cancelled context
		ctx, cancel := context.WithCancel(t.Context())
		cancel() // Cancel immediately

		mockStage.runFunc = func(ctx context.Context, job *Job) error {
			return nil
		}
		_, abort = p.runStage(ctx, mockStage, job)
		if !abort {
			t.Error("expected abort to be true when context is cancelled")
		}
	})
}

// ---------------------------------------------------------------------------
// buildSummaryEntry — missing boundary cases
// ---------------------------------------------------------------------------

// TestBuildSummaryEntry_EmptyStageLog verifies that an empty StageLog produces
// a valid summary with zero duration, no panic, and "Completed" status.
func TestBuildSummaryEntry_EmptyStageLog(t *testing.T) {
	t.Parallel()
	job := &Job{
		Job:      newQueueJob(t, "empty-log", 0),
		StageLog: []StageLogEntry{}, // no stages ran
	}

	entry := buildSummaryEntry(job)

	if entry.Stage != "summary" {
		t.Errorf("Stage = %q; want %q", entry.Stage, "summary")
	}
	if entry.Elapsed != 0 {
		t.Errorf("Elapsed = %v; want 0 for empty StageLog", entry.Elapsed)
	}
	// No FailMsg, no ParError, no UnpackError → should say "Completed".
	if len(entry.Lines) == 0 {
		t.Fatal("Lines should not be empty")
	}
	header := entry.Lines[0]
	if !strings.Contains(header, "Completed") {
		t.Errorf("header = %q; want 'Completed' for clean job", header)
	}
}

// TestBuildSummaryEntry_AllSuccess verifies that a job with all successful
// stages and a FailMsg of "" produces a "Completed" header.
func TestBuildSummaryEntry_AllSuccess(t *testing.T) {
	t.Parallel()
	now := time.Now()
	job := &Job{
		Job:      newQueueJob(t, "all-ok", 0),
		FinalDir: "/output/done",
		StageLog: []StageLogEntry{
			{Stage: "repair", Started: now, Elapsed: time.Second, Err: nil},
			{Stage: "unpack", Started: now.Add(time.Second), Elapsed: 2 * time.Second, Err: nil},
		},
	}

	entry := buildSummaryEntry(job)

	if entry.Elapsed != 3*time.Second {
		t.Errorf("Elapsed = %v; want 3s", entry.Elapsed)
	}
	linesStr := strings.Join(entry.Lines, "\n")
	if !strings.Contains(linesStr, "Completed") {
		t.Errorf("expected 'Completed' in summary, got: %v", entry.Lines)
	}
	if !strings.Contains(linesStr, "/output/done") {
		t.Errorf("expected FinalDir in summary header, got: %v", entry.Lines)
	}
	// Verify both stage lines use the success symbol.
	if !strings.Contains(linesStr, "✓ repair") {
		t.Errorf("expected '✓ repair' in summary, got: %v", entry.Lines)
	}
	if !strings.Contains(linesStr, "✓ unpack") {
		t.Errorf("expected '✓ unpack' in summary, got: %v", entry.Lines)
	}
}

// TestPreCheck_AlreadyDeliveredPerJobFinalDir verifies #767: when DownloadDir
// is missing (because FinalizeStage already moved the job before a crash) and
// a per-job FinalDir exists and is non-empty, processJob sets
// DownloadDir = FinalDir, skips stages up to and including finalize, runs
// script with its normal failure semantics, and completes with FailMsg == "".
// With a flat layout (FlatLayout == true), FinalDir is shared across jobs so
// non-empty proves nothing and the job is still filed Failed.
func TestPreCheck_AlreadyDeliveredPerJobFinalDir(t *testing.T) {
	t.Parallel()

	t.Run("per-job FinalDir skips finalize, runs script, and completes", func(t *testing.T) {
		t.Parallel()

		missingDownloadDir := filepath.Join(t.TempDir(), "missing-download-dir")
		finalDir := filepath.Join(t.TempDir(), "complete", "movies", "MyRelease")
		if err := os.MkdirAll(finalDir, 0o750); err != nil {
			t.Fatalf("mkdir finalDir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(finalDir, "movie.mkv"), []byte("video"), 0o600); err != nil {
			t.Fatalf("write movie.mkv: %v", err)
		}

		scriptDir := t.TempDir()
		statusFile := filepath.Join(scriptDir, "status.txt")
		dirFile := filepath.Join(scriptDir, "dir.txt")
		writeScript(t, filepath.Join(scriptDir, "notify.sh"),
			[]byte("#!/bin/sh\necho \"$SAB_PP_STATUS\" > "+statusFile+"\necho \"$SAB_FINAL_PROCESSING_DIR\" > "+dirFile+"\n"))

		qjob := newQueueJob(t, "delivered-per-job", 3)
		qjob.SetName("MyRelease")
		if err := qjob.RecordDownload("news.example", 2048); err != nil {
			t.Fatalf("RecordDownload: %v", err)
		}
		job := &Job{
			Job:         qjob,
			Filename:    "MyRelease.nzb",
			PP:          3,
			DownloadDir: missingDownloadDir,
			FinalDir:    finalDir,
			FlatLayout:  false,
			Script:      "notify.sh",
		}

		pp := New(Options{
			Stages: []Stage{
				NewFinalizeStage(),
				NewScriptStage(scriptDir, filepath.Dir(finalDir), "test", "", ""),
			},
		})
		pp.processJob(t.Context(), job)

		if job.FailMsg != "" {
			t.Errorf("job.FailMsg = %q, want empty", job.FailMsg)
		}
		if job.DownloadDir != finalDir {
			t.Errorf("job.DownloadDir = %q, want FinalDir %q", job.DownloadDir, finalDir)
		}

		var sawFinalize, sawScript, sawMovieLine, sawServersLine bool
		for _, entry := range job.StageLog {
			switch entry.Stage {
			case "download":
				for _, line := range entry.Lines {
					if strings.Contains(line, "Error reading download dir") {
						t.Errorf("download StageLog contains read error on delivered job: %q", line)
					}
					if strings.Contains(line, "movie.mkv") {
						sawMovieLine = true
					}
					if strings.Contains(line, "Servers: news.example") {
						sawServersLine = true
					}
				}
			case "finalize":
				sawFinalize = true
			case "script":
				sawScript = true
			}
		}
		if !sawMovieLine {
			t.Errorf("download StageLog missing delivered file movie.mkv; StageLog = %+v", job.StageLog)
		}
		if !sawServersLine {
			t.Errorf("download StageLog missing ServerStats line; StageLog = %+v", job.StageLog)
		}
		if sawFinalize {
			t.Errorf("finalize stage ran on already-delivered job; StageLog = %+v", job.StageLog)
		}
		if !sawScript {
			t.Errorf("script stage did not run on already-delivered job; StageLog = %+v", job.StageLog)
		}
		if summary := buildSummaryEntry(job); len(summary.Lines) == 0 || !strings.HasPrefix(summary.Lines[0], "Pipeline Completed") {
			t.Errorf("summary header = %v, want prefix %q", summary.Lines, "Pipeline Completed")
		}

		gotStatus, err := os.ReadFile(statusFile)
		if err != nil {
			t.Fatalf("read script status output: %v", err)
		}
		if strings.TrimSpace(string(gotStatus)) != "0" {
			t.Errorf("script SAB_PP_STATUS = %q, want %q", strings.TrimSpace(string(gotStatus)), "0")
		}
		gotDir, err := os.ReadFile(dirFile)
		if err != nil {
			t.Fatalf("read script dir output: %v", err)
		}
		if strings.TrimSpace(string(gotDir)) != finalDir {
			t.Errorf("script SAB_FINAL_PROCESSING_DIR = %q, want FinalDir %q", strings.TrimSpace(string(gotDir)), finalDir)
		}
	})

	t.Run("per-job FinalDir preserves script failure semantics", func(t *testing.T) {
		t.Parallel()

		missingDownloadDir := filepath.Join(t.TempDir(), "missing-download-dir")
		finalDir := filepath.Join(t.TempDir(), "complete", "movies", "MyRelease")
		if err := os.MkdirAll(finalDir, 0o750); err != nil {
			t.Fatalf("mkdir finalDir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(finalDir, "movie.mkv"), []byte("video"), 0o600); err != nil {
			t.Fatalf("write movie.mkv: %v", err)
		}

		scriptDir := t.TempDir()
		writeScript(t, filepath.Join(scriptDir, "fail.sh"), []byte("#!/bin/sh\nexit 5\n"))

		qjob := newQueueJob(t, "delivered-script-fail", 3)
		qjob.SetName("MyRelease")
		job := &Job{
			Job:         qjob,
			Filename:    "MyRelease.nzb",
			PP:          3,
			DownloadDir: missingDownloadDir,
			FinalDir:    finalDir,
			Script:      "fail.sh",
		}

		scriptStage := NewScriptStage(scriptDir, filepath.Dir(finalDir), "test", "", "")
		scriptStage.SetScriptCanFail(true)
		pp := New(Options{
			Stages: []Stage{
				NewFinalizeStage(),
				scriptStage,
			},
		})
		pp.processJob(t.Context(), job)

		if !strings.Contains(job.FailMsg, "Script fail.sh failed") {
			t.Errorf("job.FailMsg = %q, want script failure message", job.FailMsg)
		}
		if job.DownloadDir != finalDir {
			t.Errorf("job.DownloadDir = %q, want FinalDir %q", job.DownloadDir, finalDir)
		}
	})

	t.Run("flat layout still files Failed", func(t *testing.T) {
		t.Parallel()

		missingDownloadDir := filepath.Join(t.TempDir(), "missing-download-dir")
		flatFinalDir := filepath.Join(t.TempDir(), "complete", "movies")
		if err := os.MkdirAll(flatFinalDir, 0o750); err != nil {
			t.Fatalf("mkdir flatFinalDir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(flatFinalDir, "other_job.mkv"), []byte("video"), 0o600); err != nil {
			t.Fatalf("write other_job.mkv: %v", err)
		}

		scriptDir := t.TempDir()
		ranFile := filepath.Join(scriptDir, "ran.txt")
		writeScript(t, filepath.Join(scriptDir, "notify.sh"),
			[]byte("#!/bin/sh\necho ran > "+ranFile+"\n"))

		qjob := newQueueJob(t, "delivered-flat", 3)
		qjob.SetName("MyRelease")
		job := &Job{
			Job:         qjob,
			Filename:    "MyRelease.nzb",
			PP:          3,
			DownloadDir: missingDownloadDir,
			FinalDir:    flatFinalDir,
			FlatLayout:  true,
			Script:      "notify.sh",
		}

		pp := New(Options{
			Stages: []Stage{
				NewFinalizeStage(),
				NewScriptStage(scriptDir, filepath.Dir(flatFinalDir), "test", "", ""),
			},
		})
		pp.processJob(t.Context(), job)

		if !strings.Contains(job.FailMsg, "download directory unavailable") {
			t.Errorf("job.FailMsg = %q, want download directory unavailable error", job.FailMsg)
		}
		if len(job.StageLog) != 2 || job.StageLog[0].Stage != "download" || job.StageLog[1].Stage != "pre-check" {
			t.Errorf("job.StageLog = %+v, want [download, pre-check]", job.StageLog)
		}
		if job.DownloadDir != missingDownloadDir {
			t.Errorf("job.DownloadDir = %q, want original %q", job.DownloadDir, missingDownloadDir)
		}
		if _, err := os.Stat(ranFile); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("script ran on flat-layout missing-DownloadDir job (stat err = %v)", err)
		}
	})

	t.Run("empty existing DownloadDir still files Failed even with non-empty FinalDir", func(t *testing.T) {
		t.Parallel()

		emptyDownloadDir := t.TempDir()
		finalDir := filepath.Join(t.TempDir(), "complete", "movies", "MyRelease")
		if err := os.MkdirAll(finalDir, 0o750); err != nil {
			t.Fatalf("mkdir finalDir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(finalDir, "stale.mkv"), []byte("video"), 0o600); err != nil {
			t.Fatalf("write stale.mkv: %v", err)
		}

		qjob := newQueueJob(t, "empty-existing-dl", 3)
		qjob.SetName("MyRelease")
		job := &Job{
			Job:         qjob,
			Filename:    "MyRelease.nzb",
			PP:          3,
			DownloadDir: emptyDownloadDir,
			FinalDir:    finalDir,
		}

		pp := New(Options{
			Stages: []Stage{NewFinalizeStage()},
		})
		pp.processJob(t.Context(), job)

		if job.FailMsg != "download directory is empty" {
			t.Errorf("job.FailMsg = %q, want %q", job.FailMsg, "download directory is empty")
		}
		if len(job.StageLog) != 2 || job.StageLog[0].Stage != "download" || job.StageLog[1].Stage != "pre-check" {
			t.Errorf("job.StageLog = %+v, want [download, pre-check]", job.StageLog)
		}
		if job.DownloadDir != emptyDownloadDir {
			t.Errorf("job.DownloadDir = %q, want %q", job.DownloadDir, emptyDownloadDir)
		}
	})

	t.Run("non-ENOENT ReadDir error on DownloadDir still files Failed even with non-empty FinalDir", func(t *testing.T) {
		t.Parallel()

		downloadDirAsFile := filepath.Join(t.TempDir(), "download-is-a-file")
		if err := os.WriteFile(downloadDirAsFile, []byte("not-a-dir"), 0o600); err != nil {
			t.Fatalf("write downloadDirAsFile: %v", err)
		}
		finalDir := filepath.Join(t.TempDir(), "complete", "movies", "MyRelease")
		if err := os.MkdirAll(finalDir, 0o750); err != nil {
			t.Fatalf("mkdir finalDir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(finalDir, "stale.mkv"), []byte("video"), 0o600); err != nil {
			t.Fatalf("write stale.mkv: %v", err)
		}

		qjob := newQueueJob(t, "enotdir-dl", 3)
		qjob.SetName("MyRelease")
		job := &Job{
			Job:         qjob,
			Filename:    "MyRelease.nzb",
			PP:          3,
			DownloadDir: downloadDirAsFile,
			FinalDir:    finalDir,
		}

		pp := New(Options{
			Stages: []Stage{NewFinalizeStage()},
		})
		pp.processJob(t.Context(), job)

		if !strings.HasPrefix(job.FailMsg, "download directory unavailable:") {
			t.Errorf("job.FailMsg = %q, want prefix %q", job.FailMsg, "download directory unavailable:")
		}
		if len(job.StageLog) != 2 || job.StageLog[0].Stage != "download" || job.StageLog[1].Stage != "pre-check" {
			t.Errorf("job.StageLog = %+v, want [download, pre-check]", job.StageLog)
		}
		if job.DownloadDir != downloadDirAsFile {
			t.Errorf("job.DownloadDir = %q, want %q", job.DownloadDir, downloadDirAsFile)
		}
	})

	t.Run("FailMsg-preset job does not redirect missing DownloadDir to FinalDir and records download preamble", func(t *testing.T) {
		t.Parallel()

		missingDownloadDir := filepath.Join(t.TempDir(), "missing-download-dir")
		finalDir := filepath.Join(t.TempDir(), "complete", "movies", "MyRelease")
		if err := os.MkdirAll(finalDir, 0o750); err != nil {
			t.Fatalf("mkdir finalDir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(finalDir, "stale.mkv"), []byte("video"), 0o600); err != nil {
			t.Fatalf("write stale.mkv: %v", err)
		}

		qjob := newQueueJob(t, "failmsg-preset-dl", 3)
		qjob.SetName("MyRelease")
		job := &Job{
			Job:         qjob,
			Filename:    "MyRelease.nzb",
			PP:          3,
			DownloadDir: missingDownloadDir,
			FinalDir:    finalDir,
			FailMsg:     "Aborted, cannot be completed",
		}

		pp := New(Options{
			Stages: []Stage{NewFinalizeStage()},
		})
		pp.processJob(t.Context(), job)

		if job.FailMsg != "Aborted, cannot be completed" {
			t.Errorf("job.FailMsg = %q, want %q", job.FailMsg, "Aborted, cannot be completed")
		}
		if job.DownloadDir != missingDownloadDir {
			t.Errorf("job.DownloadDir = %q, want original %q", job.DownloadDir, missingDownloadDir)
		}
		if len(job.StageLog) != 2 || job.StageLog[0].Stage != "download" || job.StageLog[1].Stage != "skipped" {
			t.Errorf("job.StageLog = %+v, want [download, skipped]", job.StageLog)
		}
	})

	t.Run("alreadyDelivered and stagesAfterFinalize helpers", func(t *testing.T) {
		t.Parallel()

		emptyFinal := t.TempDir()
		populatedFinal := t.TempDir()
		if err := os.WriteFile(filepath.Join(populatedFinal, "a.bin"), []byte("x"), 0o600); err != nil {
			t.Fatalf("write a.bin: %v", err)
		}

		for _, tc := range []struct {
			name string
			job  Job
			want bool
		}{
			{name: "unset FinalDir", job: Job{}, want: false},
			{name: "missing FinalDir", job: Job{FinalDir: filepath.Join(t.TempDir(), "missing")}, want: false},
			{name: "empty FinalDir", job: Job{FinalDir: emptyFinal}, want: false},
			{name: "flat layout with populated FinalDir", job: Job{FinalDir: populatedFinal, FlatLayout: true}, want: false},
			{name: "per-job layout with populated FinalDir", job: Job{FinalDir: populatedFinal, FlatLayout: false}, want: true},
		} {
			if got := tc.job.alreadyDelivered(); got != tc.want {
				t.Errorf("%s: alreadyDelivered() = %v, want %v", tc.name, got, tc.want)
			}
		}

		s1 := newRecordStage("repair")
		s2 := NewFinalizeStage()
		s3 := newRecordStage("script")
		if got := stagesAfterFinalize([]Stage{s1, s2, s3}); len(got) != 1 || got[0].Name() != "script" {
			t.Errorf("stagesAfterFinalize([repair, finalize, script]) = %v, want [script]", got)
		}
		if got := stagesAfterFinalize([]Stage{s1, s2}); len(got) != 0 {
			t.Errorf("stagesAfterFinalize([repair, finalize]) = %v, want empty", got)
		}
		if got := stagesAfterFinalize([]Stage{s1, s3}); got != nil {
			t.Errorf("stagesAfterFinalize([repair, script]) = %v, want nil", got)
		}
	})
}
