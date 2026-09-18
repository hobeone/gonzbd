//go:build crash && linux

package crash

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// addFixture is small and throttled: the job must still be queued when the
// restarted daemon is asked for it, not already finished.
func addFixture() harnessOpts {
	return harnessOpts{
		Connections: 1,
		BodyDelay:   50 * time.Millisecond,
		Files:       []fileSpec{{Name: "payload.bin", Size: 4 << 20, PartSize: 128 << 10}},
	}
}

// TestSIGKILL_AddThenImmediateKill pins that a job whose mode=addfile returned
// 200 survives a SIGKILL sent the moment the response arrives. The 200 is the
// daemon's acknowledgement, so the job's queue row must already be durable
// when it is sent: startup rebuilds the queue from dispatch_jobs alone.
//
// It is a race against the dispatcher's tick, so one pass proves nothing; run
// it with -count of 20 or more.
func TestSIGKILL_AddThenImmediateKill(t *testing.T) {
	h := newHarness(t, addFixture())
	jobID := h.AddJob()
	h.Kill()

	var jobFiles int
	db := h.openDB()
	if err := db.QueryRow(`SELECT COUNT(*) FROM job_files WHERE job_id = ?`, jobID).Scan(&jobFiles); err != nil {
		t.Fatalf("count job_files for %s: %v", jobID, err)
	}
	_, manifestErr := os.Stat(filepath.Join(h.AdminDir, "queue", "manifests", jobID+".json.gz"))

	h.Restart()
	if _, ok := h.Slot(jobID); !ok {
		t.Fatalf("job %s is absent from the queue after a SIGKILL sent as its addfile 200 arrived; "+
			"at the kill it had %d job_files row(s) and manifest present=%v, now orphaned",
			jobID, jobFiles, manifestErr == nil)
	}
}

// TestSIGKILL_AddThenImmediateKill_PositiveControl is the same sequence with
// the kill delayed past the first tick. If it fails, the harness cannot see a
// restored job at all and the test above proves nothing.
func TestSIGKILL_AddThenImmediateKill_PositiveControl(t *testing.T) {
	h := newHarness(t, addFixture())
	jobID := h.AddJob()
	time.Sleep(1500 * time.Millisecond)
	h.Kill()
	h.Restart()
	if _, ok := h.Slot(jobID); !ok {
		t.Fatalf("job %s is absent after a kill 1.5s past its add; the harness cannot see a restored job", jobID)
	}
}
