package app_test

import (
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
)

// TestScenario_OneShotDuplicateNeverPaused verifies that if we add the same
// NZB twice (simulating a one-shot re-run), the job is NOT added in a paused
// state, because one-shot mode implies the user explicitly wants to download
// this specific file right now.
func TestScenario_OneShotDuplicateNeverPaused(t *testing.T) {
	h := newScenarioHarness(t)
	h.Start()

	payload := []byte("oneshot content")

	// 1. Add the job the first time.
	job1 := h.AddOneShotJob("oneshot-job", payload, true)
	if !h.WaitForHistory(job1.ID(), 5*time.Second) {
		t.Fatalf("first download failed to reach history")
	}

	// 2. Add the SAME job again (duplicate).
	// This simulates the CLI's AddJob call with force=true.
	// We expect this to be Queued, not Paused.
	job2 := h.AddOneShotJob("oneshot-job", payload, true)

	row, ok := h.app.Dispatcher().Row(job2.ID())
	if !ok {
		t.Fatalf("failed to find job in dispatcher")
	}
	status := row.Status()

	// The real invariant: the duplicate must NOT be paused. It should be
	// Queued, but under -race the downloader goroutine may pick it up
	// before we check, transitioning it to Downloading — that's fine too.
	if status == constants.StatusPaused {
		t.Errorf("job was added as Paused (duplicate), but one-shot mode requires it to be active")
	}
}
