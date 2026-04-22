package app_test

import (
	"testing"
	"time"
)

// TestScenarioHarness_Smoke exercises the scenarioHarness end-to-end: one
// simple job, one article, a no-op post-processing chain. On success the job
// should leave the active queue and land in history with status "Completed".
func TestScenarioHarness_Smoke(t *testing.T) {
	h := newScenarioHarness(t)
	h.Start()

	job := h.AddSimpleJob("smoke", []byte("hello world"))

	if !h.WaitForPostProc(job.ID, 10*time.Second) {
		t.Fatalf("timeout waiting for post-proc completion of %s", job.ID)
	}
	if !h.WaitForHistory(job.ID, 2*time.Second) {
		t.Fatalf("job %s did not reach history", job.ID)
	}

	if h.QueueContains(job.ID) {
		t.Errorf("job %s still in active queue after post-proc", job.ID)
	}

	files, jobs, pps := h.Events()
	if len(files) == 0 {
		t.Errorf("no FileComplete events recorded")
	}
	if len(jobs) == 0 {
		t.Errorf("no JobComplete events recorded")
	}
	if len(pps) == 0 {
		t.Errorf("no PostProcComplete events recorded")
	}
}
