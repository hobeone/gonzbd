package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/job"
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

	appNilDisp := &Application{}
	if _, ok := appNilDisp.lookupJob("test-job"); ok {
		t.Fatal("lookupJob must return false when dispatcher is nil")
	}
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
	// 1. Nil app should safely return without panic.
	wNil := &appWorkers{app: nil}
	wNil.Abort(job.New("job-nil", "Job Nil", job.Policy{}))

	// Nil job should safely return without panic.
	wNil.Abort(nil)

	// 2. App with nil downloader and nil dispatcher should safely return.
	wEmpty := &appWorkers{app: &Application{}}
	wEmpty.Abort(job.New("job-empty", "Job Empty", job.Policy{}))

	// 3. App with wired mock downloader and real dispatcher.
	app := newTestApplication(t)
	w := &appWorkers{app: app}

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
