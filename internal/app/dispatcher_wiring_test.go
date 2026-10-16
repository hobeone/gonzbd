package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// TestApplicationConstructsAWiredDispatcher pins that app.New produces a
// dispatcher with both ports satisfied. dispatch.New panics on a nil Residency
// or Runner, so this test failing to panic IS the assertion.
func TestApplicationConstructsAWiredDispatcher(t *testing.T) {
	t.Parallel()
	app := newTestApplication(t)
	if app.Dispatcher() == nil {
		t.Fatal("app.New must construct a Dispatcher")
	}
	if app.Config() == nil {
		t.Fatal("app.Config() must not be nil")
	}

	w := &appWorkers{app: app}
	w.Abort(job.New("test-job", "Test Job", job.Policy{}))
}

type mockCancelWakeDownloader struct {
	mu        sync.Mutex
	cancelled []string
	woken     int
}

func (m *mockCancelWakeDownloader) Start(context.Context) error                      { return nil }
func (m *mockCancelWakeDownloader) Stop() error                                      { return nil }
func (m *mockCancelWakeDownloader) Completions() <-chan *downloader.ArticleResult    { return nil }
func (m *mockCancelWakeDownloader) SetSpeedLimit(int64)                              {}
func (m *mockCancelWakeDownloader) SetDispatchOptions(int, int, bool, time.Duration) {}
func (m *mockCancelWakeDownloader) UnblockServer(string) bool                        { return true }
func (m *mockCancelWakeDownloader) Pause()                                           {}
func (m *mockCancelWakeDownloader) Resume()                                          {}
func (m *mockCancelWakeDownloader) DisconnectAll()                                   {}
func (m *mockCancelWakeDownloader) CancelJob(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cancelled = append(m.cancelled, id)
}
func (m *mockCancelWakeDownloader) Wake() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.woken++
}

func TestAppWorkers_Abort(t *testing.T) {
	t.Parallel()
	app := newTestApplication(t)
	w := &appWorkers{app: app}

	// A nil job should safely return without panic.
	w.Abort(nil)

	// A wired mock downloader and a real dispatcher.

	j := job.New("job-abort", "Test Job", job.PolicyFromPP(3))
	if err := app.dispatcher.Add(context.Background(), j, dispatch.Header{Name: "Test Job"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	mockDL := &mockCancelWakeDownloader{}
	app.mu.Lock()
	app.downloader = mockDL
	app.mu.Unlock()

	w.Abort(j)

	mockDL.mu.Lock()
	cancelledLen := len(mockDL.cancelled)
	var cancelledID string
	if cancelledLen > 0 {
		cancelledID = mockDL.cancelled[0]
	}
	wokenCount := mockDL.woken
	mockDL.mu.Unlock()

	if cancelledLen != 1 || cancelledID != "job-abort" {
		t.Errorf("CancelJob called with %v, want [job-abort]", mockDL.cancelled)
	}
	if wokenCount != 1 {
		t.Errorf("Wake called %d times, want 1", wokenCount)
	}
}

func TestAppWorkers_Abort_DelayedGoroutine_DoesNotDisruptNewAttempt(t *testing.T) {
	t.Parallel()
	app := newTestApplication(t)
	w := &appWorkers{app: app}

	j1 := job.New("job-reuse", "First Attempt", job.Policy{})
	if err := app.dispatcher.Add(context.Background(), j1, dispatch.Header{Name: "First Attempt"}); err != nil {
		t.Fatalf("Add(j1): %v", err)
	}

	// Remove j1 from dispatcher, simulating j1 being removed / completed.
	if err := app.dispatcher.Remove(context.Background(), "job-reuse"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Register a new attempt under the same ID.
	j2 := job.New("job-reuse", "Second Attempt", job.Policy{})
	if err := app.dispatcher.Add(context.Background(), j2, dispatch.Header{Name: "Second Attempt"}); err != nil {
		t.Fatalf("Add(j2): %v", err)
	}

	// Simulate delayed Abort call from old attempt j1.
	w.Abort(j1)

	// Negative-observation window: allow the async goroutine spawned by Abort
	// to execute its YieldedJob attempt. Since j1 != j2, it must not disrupt j2.
	time.Sleep(50 * time.Millisecond)

	// Dispatcher still holds j2 under "job-reuse".
	got, ok := app.lookupJob("job-reuse")
	if !ok || got != j2 {
		t.Fatalf("lookupJob returned (%v, %v), want (%v, true)", got, ok, j2)
	}
}

func TestApplication_GetJob(t *testing.T) {
	t.Parallel()
	app := newTestApplication(t)

	// Before job is added or when nonexistent ID is queried, GetJob returns nil.
	if got := app.GetJob("nonexistent"); got != nil {
		t.Fatalf("GetJob(nonexistent) = %v, want nil", got)
	}

	j := job.New("job-get", "Test Job", job.Policy{})
	if err := app.dispatcher.Add(t.Context(), j, dispatch.Header{Name: "Test Job"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if got := app.GetJob("job-get"); got != j {
		t.Fatalf("GetJob(job-get) = %v, want %v", got, j)
	}

	// Nil dispatcher safely returns nil.
	var nilApp *Application
	if got := nilApp.GetJob("job-get"); got != nil {
		t.Fatalf("nilApp.GetJob = %v, want nil", got)
	}
}

func TestApplication_HasPostProcJob(t *testing.T) {
	t.Parallel()
	app := newTestApplication(t)

	j := job.New("job-pp", "Test Job", job.Policy{})

	// When job is not in post-processor, HasPostProcJob returns false.
	if app.HasPostProcJob(j) {
		t.Fatal("HasPostProcJob = true for job not in post-processor")
	}

	// Nil postProcessor safely returns false.
	var nilApp *Application
	if nilApp.HasPostProcJob(j) {
		t.Fatal("nilApp.HasPostProcJob = true, want false")
	}

	// When job is pushed to post-processor queue, HasPostProcJob returns true.
	app.postProcessor.Process(&postproc.Job{Job: j})
	if !app.HasPostProcJob(j) {
		t.Fatal("HasPostProcJob = false for job in post-processor")
	}
}
