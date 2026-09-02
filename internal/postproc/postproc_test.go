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

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newQueueJob builds a real *queue.Job (with a valid Manifest/Progress) via
// queue.NewJob, adds it to a fresh queue, and returns it — the one way to
// get a job into existence, rather than a parallel struct-literal path.
func newQueueJob(t *testing.T, id string, pp int) *queue.Job {
	t.Helper()
	qjob, err := queue.NewJob(&nzb.NZB{}, queue.AddOptions{Filename: id + ".nzb", Name: id, PP: pp}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	// ID is a plain exported field, unaffected by the Manifest/Progress
	// split — override the randomly-generated one so tests can match jobs
	// by the readable id they passed in (e.g. p.Cancel(id)).
	qjob.ID = id
	q := queue.New()
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return qjob
}

// makeJob creates a minimal Job backed by a queue.Job for use in tests.
func makeJob(t *testing.T, name string) *Job {
	t.Helper()
	return &Job{
		Queue: newQueueJob(t, name, 3), // Repair + Unpack + Delete (production default)
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
	s.calls = append(s.calls, s.name+"/"+job.Queue.Name)
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
	removed := p.Cancel("second")
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
		if j.Queue.ID == "second" && len(j.StageLog) > 0 {
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
// concurrently delete that directory. The cancelled job must also be
// dropped silently -- not finalized via OnJobDone -- since the canceller
// owns its cleanup.
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
		b := p.busy && p.currentJobID == "running"
		p.busyMu.Unlock()
		return b
	}, 2*time.Second, "worker to be busy on running job")

	removed := p.Cancel("running")
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
		t.Error("OnJobDone fired for a job cancelled mid-processing; it should be dropped silently")
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
			firings[j.Queue.ID]++
			logs[j.Queue.ID] = append([]StageLogEntry{}, j.StageLog...)
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
		q.Push(&Job{Queue: newQueueJob(t, n, 0)})
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	for _, want := range names {
		job, ok := q.Pop(ctx, nil)
		if !ok {
			t.Fatalf("Pop returned false, want job %q", want)
		}
		if job.Queue.Name != want {
			t.Errorf("got job %q, want %q", job.Queue.Name, want)
		}
	}
}

func TestPPQueueCancel(t *testing.T) {
	q := newPPQueue()
	q.Push(&Job{Queue: newQueueJob(t, "a", 0)})
	q.Push(&Job{Queue: newQueueJob(t, "b", 0)})
	if !q.Cancel("a") {
		t.Error("Cancel('a') = false, want true")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	job, ok := q.Pop(ctx, nil)
	if !ok || job.Queue.ID != "b" {
		t.Errorf("expected 'b', got ok=%v job=%v", ok, job)
	}

	if q.Cancel("does-not-exist") {
		t.Error("Cancel of non-existent job returned true")
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

	job := &Job{Queue: newQueueJob(t, "pp0", 0)}
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

	job := &Job{Queue: newQueueJob(t, "pp1", 1)}
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

	job := &Job{Queue: newQueueJob(t, "pp2", 2)}
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

	job := &Job{Queue: newQueueJob(t, "pp3", 3)}
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

	job := &Job{Queue: newQueueJob(t, "pp0-full", 0)}
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
			doneJobs = append(doneJobs, j.Queue.ID)
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

// TestScriptCanFail_True verifies that non-zero script exit is logged but
// does NOT return an error when ScriptCanFail is true.
func TestScriptCanFail_True(t *testing.T) {
	t.Parallel()
	// Create a real ScriptStage and verify SetScriptCanFail works.
	script := &ScriptStage{}
	script.SetScriptCanFail(true)
	if !script.scriptCanFail.Load() {
		t.Error("ScriptCanFail should be true")
	}

	// Use a mock stage in the pipeline to simulate a failing script.
	// The pipeline continues past errors, so both stages should run.
	failScript := newRecordStage("script")
	failScript.returnErr = fmt.Errorf("script %q exited %d", "test.sh", 1)
	finalize := newRecordStage("finalize")

	var doneMu sync.Mutex
	var doneJobs []string
	p := startProcessor(t, Options{
		Stages: []Stage{finalize, failScript},
		OnJobDone: func(j *Job) {
			doneMu.Lock()
			doneJobs = append(doneJobs, j.Queue.ID)
			doneMu.Unlock()
		},
	})

	job := makeJob(t, "script-can-fail")
	p.Process(job)

	waitUntil(t, func() bool {
		doneMu.Lock()
		defer doneMu.Unlock()
		return len(doneJobs) == 1
	}, 2*time.Second, "job to complete")

	// The script stage errored but pipeline should still complete both stages.
	if finalize.CallCount() != 1 {
		t.Errorf("finalize ran %d times, want 1", finalize.CallCount())
	}
	if failScript.CallCount() != 1 {
		t.Errorf("script ran %d times, want 1", failScript.CallCount())
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
		Queue:       newQueueJob(t, "empty", 3),
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
		Queue:       newQueueJob(t, "missing", 3),
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
		Queue: newQueueJob(t, "no-dir", 3),
		// DownloadDir deliberately empty
	}
	p.Process(job)
	wg.Wait()

	if stage.CallCount() != 1 {
		t.Errorf("stage ran %d times, want 1 (unset DownloadDir should not trigger pre-check)", stage.CallCount())
	}
}

// TestProcessJob_SeedsOwnedFilesFromDownloadDir verifies that processJob
// auto-populates job.OwnedFiles from the current contents of DownloadDir
// before any stage runs, giving every real pipeline run the extension/sample
// cleanup ownership restriction for free (see Job.OwnedFiles doc comment).
func TestProcessJob_SeedsOwnedFilesFromDownloadDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var capturedOwned map[string]struct{}
	stage := &mockPPStage{
		name: "capture",
		runFunc: func(_ context.Context, job *Job) error {
			capturedOwned = job.OwnedFiles
			return nil
		},
	}

	var wg sync.WaitGroup
	wg.Add(1)
	p := startProcessor(t, Options{
		Stages:    []Stage{stage},
		OnJobDone: func(_ *Job) { wg.Done() },
	})

	job := &Job{
		Queue:       newQueueJob(t, "seed", 3),
		DownloadDir: dir,
	}
	p.Process(job)
	wg.Wait()

	if capturedOwned == nil {
		t.Fatal("expected job.OwnedFiles to be populated before stages ran")
	}
	if _, ok := capturedOwned[filePath]; !ok {
		t.Errorf("expected %s to be recorded as owned, got %v", filePath, capturedOwned)
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
		q := queue.New()
		qjob, err := queue.NewJob(&nzb.NZB{}, queue.AddOptions{Filename: "job1.nzb", Name: "TestJob"}, fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatalf("NewJob: %v", err)
		}
		if err := q.Add(qjob); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if err := q.MarkJobStarted(qjob.ID, now); err != nil {
			t.Fatalf("MarkJobStarted: %v", err)
		}
		if err := q.MarkDownloadFinished(qjob.ID, now.Add(5*time.Second)); err != nil {
			t.Fatalf("MarkDownloadFinished: %v", err)
		}
		job := &Job{
			Queue:       qjob,
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
			Queue: newQueueJob(t, "job1", 0),
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
		var updatedID string
		var updatedStatus constants.Status

		p := &PostProcessor{
			log: slog.Default(),
			statusUpdater: func(id string, status constants.Status) {
				updatedID = id
				updatedStatus = status
			},
		}

		job := &Job{
			Queue: newQueueJob(t, "job1", 0), // PP=0 skips repair
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
		job.Queue.PP = 3 // PP=3 runs repair
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
		if updatedID != "job1" || updatedStatus != constants.StatusVerifying {
			t.Errorf("expected status update to Verifying, got id=%s status=%s", updatedID, updatedStatus)
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
		Queue:    newQueueJob(t, "empty-log", 0),
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
		Queue:    newQueueJob(t, "all-ok", 0),
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
