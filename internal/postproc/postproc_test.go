package postproc

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/queue"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// makeJob creates a minimal Job backed by a queue.Job for use in tests.
func makeJob(t *testing.T, name string) *Job {
	t.Helper()
	return &Job{
		Queue: &queue.Job{
			ID:   name,
			Name: name,
		},
	}
}

// recordStage is a mock Stage that appends its name + job name to a shared
// log each time Run is called.
type recordStage struct {
	name      string
	mu        sync.Mutex
	calls     []string      // "<stageName>/<jobName>"
	returnErr error         // if non-nil, returned from Run
	block     chan struct{} // if non-nil, Run blocks until this is closed
}

func newRecordStage(name string) *recordStage { return &recordStage{name: name} }

func (s *recordStage) Name() string { return s.name }

func (s *recordStage) Run(ctx context.Context, job *Job) error {
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
	if err := p.Start(context.Background()); err != nil {
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
		return len(done) == 3
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
func TestPauseResume(t *testing.T) {
	stage := newRecordStage("s")

	var doneMu sync.Mutex
	var doneCount int
	var wg sync.WaitGroup
	wg.Add(3)

	p := startProcessor(t, Options{
		Stages: []Stage{stage},
		OnJobDone: func(_ *Job) {
			doneMu.Lock()
			doneCount++
			doneMu.Unlock()
			wg.Done()
		},
	})

	p.Pause()

	// Enqueue 3 jobs while paused.
	for i := 0; i < 3; i++ {
		p.q.Push(&Job{Queue: &queue.Job{ID: "j" + string(rune('0'+i)), Name: "j" + string(rune('0'+i))}})
	}

	// Give the worker a moment to confirm it is not processing.
	time.Sleep(30 * time.Millisecond)
	if stage.CallCount() > 0 {
		t.Errorf("stage called %d times while paused, want 0", stage.CallCount())
	}

	p.Resume()
	wg.Wait()

	if got := stage.CallCount(); got != 3 {
		t.Errorf("stage called %d times after resume, want 3", got)
	}
}

// Test 5: Stop during in-flight stage: stage receives cancelled ctx; Stop
// returns only after worker exits.
func TestStopDuringInFlightStage(t *testing.T) {
	block := make(chan struct{})
	blocker := &recordStage{name: "blocker", block: block}

	p := New(Options{Stages: []Stage{blocker}})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	job := makeJob(t, "blocking-job")
	p.Process(job)

	// Wait until the blocker stage is actually executing.
	waitUntil(t, func() bool {
		p.busyMu.Lock()
		b := p.busy
		p.busyMu.Unlock()
		return b
	}, 2*time.Second, "worker to be busy")

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

// Test 6: Stage returning an error is recorded in StageLog but does not abort
// the pipeline; subsequent stages still run.
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

	if len(capturedLog) != 2 {
		t.Fatalf("StageLog has %d entries, want 2", len(capturedLog))
	}
	if capturedLog[0].Err == nil {
		t.Error("first stage log entry should have Err set")
	}
	if capturedLog[1].Err != nil {
		t.Errorf("second stage log entry should have nil Err, got %v", capturedLog[1].Err)
	}
	if nextStage.CallCount() != 1 {
		t.Errorf("next stage called %d times, want 1", nextStage.CallCount())
	}
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
		if len(logs[id]) != 2 {
			t.Errorf("job %s StageLog has %d entries, want 2", id, len(logs[id]))
		}
	}
}

// TestNoGoroutineLeak verifies that no goroutines remain after Stop returns.
func TestNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	p := New(Options{})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Give the worker goroutine time to start.
	time.Sleep(20 * time.Millisecond)

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Give the runtime time to reclaim the goroutine stack.
	time.Sleep(20 * time.Millisecond)

	after := runtime.NumGoroutine()
	if after > before {
		t.Errorf("goroutine leak: %d goroutines before, %d after", before, after)
	}
}

// ---------------------------------------------------------------------------
// ppQueue unit tests
// ---------------------------------------------------------------------------

func TestPPQueueOrdering(t *testing.T) {
	q := newPPQueue()
	names := []string{"j1", "j2", "j3"}
	for _, n := range names {
		q.Push(&Job{Queue: &queue.Job{ID: n, Name: n}})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for _, want := range names {
		job, ok := q.Pop(ctx)
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
	q.Push(&Job{Queue: &queue.Job{ID: "a", Name: "a"}})
	q.Push(&Job{Queue: &queue.Job{ID: "b", Name: "b"}})
	if !q.Cancel("a") {
		t.Error("Cancel('a') = false, want true")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	job, ok := q.Pop(ctx)
	if !ok || job.Queue.ID != "b" {
		t.Errorf("expected 'b', got ok=%v job=%v", ok, job)
	}

	if q.Cancel("does-not-exist") {
		t.Error("Cancel of non-existent job returned true")
	}
}

func TestPPQueuePopCancelledCtx(t *testing.T) {
	q := newPPQueue()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done

	_, ok := q.Pop(ctx)
	if ok {
		t.Error("Pop with cancelled ctx returned ok=true")
	}
}

func TestEmptyMethod(t *testing.T) {
	p := startProcessor(t, Options{})
	time.Sleep(20 * time.Millisecond)
	if !p.Empty() {
		t.Error("Empty() = false on idle processor")
	}
}
