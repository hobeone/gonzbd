package app_test

import (
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/queue"
)

// TestScenario_DecodeError verifies that a job with corrupted articles (decode
// errors) correctly moves to history as "Failed" instead of getting stuck in
// the active queue.
func TestScenario_DecodeError(t *testing.T) {
	h := newScenarioHarness(t)
	h.Start()

	// 1. Add a job
	job := h.AddSimpleJob("decode-error", []byte("dummy"))

	// 2. Identify the article ID and make it corrupt on the server
	msgID := ""
	h.app.Queue().ForEachUnfinishedArticle(func(ua queue.UnfinishedArticle) bool {
		if ua.JobID == job.ID {
			msgID = ua.MessageID
			return false
		}
		return true
	})
	h.server.AddArticle(msgID, []byte("this is not yenc"))

	// Wait for the job to complete or fail.
	// If the fix is NOT present, this will timeout because the article is neither Done nor Failed.
	if !h.WaitForPostProc(job.ID, 5*time.Second) {
		t.Fatalf("timeout waiting for job completion/failure")
	}

	if !h.WaitForHistory(job.ID, 2*time.Second) {
		t.Fatalf("job did not reach history")
	}

	if h.QueueContains(job.ID) {
		t.Errorf("job still in active queue after failure")
	}

	hist, err := h.repo.Get(h.ctx, job.ID)
	if err != nil {
		t.Fatalf("history missing job: %v", err)
	}

	if hist.Status != "Failed" {
		t.Errorf("history status = %q, want Failed", hist.Status)
	}
}
