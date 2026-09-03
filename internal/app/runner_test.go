package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

type reportRecorder struct {
	mu    sync.Mutex
	total int
}

func (r *reportRecorder) Finished(_ string, _ job.Outcome) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total++
	return nil
}

func (r *reportRecorder) Yielded(_ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total++
	return nil
}

func (r *reportRecorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total
}

func waitFor(t *testing.T, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

// TestAppRunner_ReturnsPromptly pins ports.go's hardest requirement: Run is
// called from the dispatcher's tick goroutine, so blocking there stalls every
// other job's advance -- not just this one's.
func TestAppRunner_ReturnsPromptly(t *testing.T) {
	r := newAppRunner(newTestApplication(t))

	done := make(chan struct{})
	go func() {
		r.Run(context.Background(), "abc123", job.Fetching)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run blocked for 2s; it must dispatch work and return")
	}
}

// TestAppRunner_EveryStateReportsExactlyOnce pins the resource contract: a
// state that returns without calling Finished or Yielded strands the job's
// lease and compute slot forever, because the Queue cannot tell 'holding and
// working' from 'holding and yielded'.
func TestAppRunner_EveryStateReportsExactlyOnce(t *testing.T) {
	for _, st := range job.AllStates() {
		t.Run(st.String(), func(t *testing.T) {
			app := newTestApplication(t)
			rec := &reportRecorder{}
			r := newAppRunner(app)
			r.report = rec

			r.Run(context.Background(), "abc123", st)
			waitFor(t, func() bool { return rec.calls() == 1 })

			if got := rec.calls(); got != 1 {
				t.Fatalf("state %s reported %d times, want exactly 1", st, got)
			}
		})
	}
}
