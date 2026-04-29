package postproc

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/queue"
)

// ---------- Has ----------

func TestHas_CurrentJob(t *testing.T) {
	block := make(chan struct{})
	blocker := &recordStage{name: "blocker", block: block}
	defer close(block)

	p := startProcessor(t, Options{Stages: []Stage{blocker}})
	p.Process(makeJob(t, "running"))

	waitUntil(t, func() bool {
		p.busyMu.Lock()
		b := p.busy
		p.busyMu.Unlock()
		return b
	}, 2*time.Second, "worker busy")

	if !p.Has("running") {
		t.Error("Has('running') = false while job is being processed")
	}
}

func TestHas_QueuedJob(t *testing.T) {
	block := make(chan struct{})
	blocker := &recordStage{name: "blocker", block: block}
	defer close(block)

	p := startProcessor(t, Options{Stages: []Stage{blocker}})
	p.Process(makeJob(t, "first"))

	waitUntil(t, func() bool {
		p.busyMu.Lock()
		b := p.busy
		p.busyMu.Unlock()
		return b
	}, 2*time.Second, "worker busy")

	p.Process(makeJob(t, "queued"))
	if !p.Has("queued") {
		t.Error("Has('queued') = false for a queued job")
	}
}

func TestHas_NotPresent(t *testing.T) {
	p := startProcessor(t, Options{})
	time.Sleep(10 * time.Millisecond)
	if p.Has("nonexistent") {
		t.Error("Has('nonexistent') = true for non-existent job")
	}
}

// ---------- PushHead ----------

func TestPushHead_SetsFirstInQueue(t *testing.T) {
	q := newPPQueue()
	q.Push(&Job{Queue: &queue.Job{ID: "a"}})
	q.Push(&Job{Queue: &queue.Job{ID: "b"}})
	q.PushHead(&Job{Queue: &queue.Job{ID: "head"}})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	first, ok := q.Pop(ctx)
	if !ok || first.Queue.ID != "head" {
		t.Errorf("PushHead: first popped = %q, want 'head'", first.Queue.ID)
	}
}

func TestPPQueue_Len(t *testing.T) {
	q := newPPQueue()
	if q.Len() != 0 {
		t.Errorf("Len on empty = %d", q.Len())
	}
	q.Push(&Job{Queue: &queue.Job{ID: "a"}})
	q.Push(&Job{Queue: &queue.Job{ID: "b"}})
	if q.Len() != 2 {
		t.Errorf("Len after 2 pushes = %d", q.Len())
	}
}

func TestPPQueue_Has(t *testing.T) {
	q := newPPQueue()
	q.Push(&Job{Queue: &queue.Job{ID: "x"}})
	if !q.Has("x") {
		t.Error("Has('x') = false")
	}
	if q.Has("y") {
		t.Error("Has('y') = true for absent job")
	}
}

// ---------- StatusUpdater ----------

func TestStatusUpdater_CalledPerStage(t *testing.T) {
	var mu sync.Mutex
	statuses := make(map[constants.Status]bool)

	var wg sync.WaitGroup
	wg.Add(1)

	p := startProcessor(t, Options{
		Stages: []Stage{
			newRecordStage("repair"),
			newRecordStage("unpack"),
			newRecordStage("finalize"),
			newRecordStage("custom"),
		},
		StatusUpdater: func(_ string, s constants.Status) {
			mu.Lock()
			statuses[s] = true
			mu.Unlock()
		},
		OnJobDone: func(_ *Job) { wg.Done() },
	})

	p.Process(makeJob(t, "status-job"))
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	// Should have seen StatusRepairing, StatusExtracting, StatusMoving, StatusRunning
	for _, want := range []constants.Status{
		constants.StatusRepairing,
		constants.StatusExtracting,
		constants.StatusMoving,
		constants.StatusRunning,
	} {
		if !statuses[want] {
			t.Errorf("missing status %v", want)
		}
	}
}

// ---------- OnEmpty ----------

func TestOnEmpty_CalledWhenQueueDrains(t *testing.T) {
	var emptyCalls int
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)

	p := startProcessor(t, Options{
		Stages: []Stage{newRecordStage("s")},
		OnEmpty: func() {
			mu.Lock()
			emptyCalls++
			mu.Unlock()
		},
		OnJobDone: func(_ *Job) { wg.Done() },
	})

	p.Process(makeJob(t, "j1"))
	wg.Wait()
	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if emptyCalls != 1 {
		t.Errorf("OnEmpty fired %d times, want 1", emptyCalls)
	}
}

func TestOnEmpty_NotCalledWhenMoreJobs(t *testing.T) {
	var emptyCalls int
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(2)

	block := make(chan struct{})
	blocker := &recordStage{name: "blocker", block: block}

	p := startProcessor(t, Options{
		Stages: []Stage{blocker},
		OnEmpty: func() {
			mu.Lock()
			emptyCalls++
			mu.Unlock()
		},
		OnJobDone: func(_ *Job) { wg.Done() },
	})

	p.Process(makeJob(t, "j1"))
	p.Process(makeJob(t, "j2"))
	close(block)
	wg.Wait()
	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	// Only 1 OnEmpty call — queue drains after j2 is done.
	if emptyCalls != 1 {
		t.Errorf("OnEmpty fired %d times, want 1", emptyCalls)
	}
}

// ---------- History ----------

func TestHistory_DeepCopy(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	p := startProcessor(t, Options{
		Stages:    []Stage{newRecordStage("s")},
		OnJobDone: func(_ *Job) { wg.Done() },
	})

	p.Process(makeJob(t, "hist-job"))
	wg.Wait()

	h := p.History()
	if len(h) != 1 {
		t.Fatalf("History len = %d, want 1", len(h))
	}
	// StageLog IS deep-copied — mutating it should not affect internal state.
	h[0].StageLog = append(h[0].StageLog, StageLogEntry{Stage: "injected"})
	h2 := p.History()
	if len(h2[0].StageLog) != 1 {
		t.Errorf("StageLog mutation leaked: len = %d, want 1", len(h2[0].StageLog))
	}
}

func TestHistory_CapMaxEntries(t *testing.T) {
	// addHistory caps at 1000 entries. Process a few extra and verify.
	p := New(Options{})
	for i := 0; i < 1005; i++ {
		p.addHistory(&Job{Queue: &queue.Job{ID: "j"}})
	}
	h := p.History()
	if len(h) > 1000 {
		t.Errorf("History len = %d, want <= 1000", len(h))
	}
}

// ---------- Pause/Resume edge cases ----------

func TestPauseWhileIdle(t *testing.T) {
	p := startProcessor(t, Options{Stages: []Stage{newRecordStage("s")}})
	time.Sleep(10 * time.Millisecond)

	p.Pause()
	// Double pause is safe.
	p.Pause()

	// Resume while paused with no jobs.
	p.Resume()
	// Double resume is safe.
	p.Resume()

	// Enqueue job — should process normally.
	var wg sync.WaitGroup
	wg.Add(1)
	p2 := startProcessor(t, Options{
		Stages:    []Stage{newRecordStage("s")},
		OnJobDone: func(_ *Job) { wg.Done() },
	})
	p2.Pause()
	p2.Process(makeJob(t, "after-pause"))
	time.Sleep(30 * time.Millisecond)
	p2.Resume()
	wg.Wait()
}

// ---------- Stop while paused ----------

func TestStopWhilePaused(t *testing.T) {
	p := New(Options{Stages: []Stage{newRecordStage("s")}})
	_ = p.Start(context.Background())
	time.Sleep(10 * time.Millisecond)
	p.Pause()
	time.Sleep(10 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		_ = p.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop while paused did not return")
	}
}

// ---------- Context cancellation in processJob ----------

func TestProcessJob_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s1 := newRecordStage("first")
	s2 := newRecordStage("second")

	p := New(Options{Stages: []Stage{s1, s2}})
	_ = p.Start(ctx)

	// Cancel the context before the first stage runs.
	cancel()
	time.Sleep(20 * time.Millisecond)

	p.wg.Wait()

	// At most stage 1 should have run. Stage 2 should be skipped.
	if s2.CallCount() > 0 {
		t.Error("second stage should not run after context cancellation")
	}
}
