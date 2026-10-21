package app_test

import (
	"testing"
	"time"
)

// TestScenario_DecodeError verifies that a job with corrupted articles (decode
// errors) correctly moves to history as "Failed" instead of getting stuck in
// the active queue.
func TestScenario_DecodeError(t *testing.T) {
	t.Parallel()
	h := newScenarioHarness(t)

	// Started first: Start refuses a job ingested before it. The job's one
	// article is corrupt on the server from the moment it is added.
	h.Start()
	job := h.AddSimpleJobServing("decode-error", []byte("dummy"), []byte("this is not yenc"))

	// Wait for the job to complete or fail.
	// If the fix is NOT present, this will timeout because the article is neither Done nor Failed.
	if !h.WaitForPostProc(job.ID(), 5*time.Second) {
		t.Fatalf("timeout waiting for job completion/failure")
	}

	if !h.WaitForHistory(job.ID(), 2*time.Second) {
		t.Fatalf("job did not reach history")
	}

	if h.QueueContains(job.ID()) {
		t.Errorf("job still in active queue after failure")
	}

	hist, err := h.repo.Get(h.ctx, job.ID())
	if err != nil {
		t.Fatalf("history missing job: %v", err)
	}

	if hist.Status != "Failed" {
		t.Errorf("history status = %q, want Failed", hist.Status)
	}
}

// TestScenario_DecodeJunkRecovery verifies that the decoder can recover from
// server-injected junk or poster introductions before the =ybegin line.
func TestScenario_DecodeJunkRecovery(t *testing.T) {
	t.Parallel()
	h := newScenarioHarness(t)

	// 1. Prepare valid yEnc article
	payload := []byte("hello world")
	validBody := yencSinglePart("test.bin", payload)

	// 2. Add leading junk
	junkBody := append([]byte("X-Antivirus: scan ok\r\n\r\n"), validBody...)

	// 3. Start, then add a job whose article the server serves with the junk.
	// Started first: Start refuses a job ingested before it.
	h.Start()
	job := h.AddSimpleJobServing("junk-recovery", payload, junkBody)

	// Wait for the job to complete.
	if !h.WaitForPostProc(job.ID(), 5*time.Second) {
		t.Fatalf("timeout waiting for job completion")
	}

	if !h.WaitForHistory(job.ID(), 2*time.Second) {
		t.Fatalf("job did not reach history")
	}

	hist, err := h.repo.Get(h.ctx, job.ID())
	if err != nil {
		t.Fatalf("history missing job: %v", err)
	}

	if hist.Status != "Completed" {
		t.Errorf("history status = %q, want Completed (junk recovery failed)", hist.Status)
	}
}
