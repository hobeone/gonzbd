//go:build crash && linux

package crash

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func lostAddFixture() harnessOpts {
	return harnessOpts{
		CheckpointBytes:    1 << 20,
		CheckpointInterval: time.Hour,
		WriteCacheBytes:    1 << 20,
		Connections:        1,
		BodyDelay:          50 * time.Millisecond,
	}
}

func manifestPathFor(adminDir, jobID string) string {
	return filepath.Join(adminDir, "queue", "manifests", jobID+".json.gz")
}

func jobFilesRowCount(t *testing.T, h *harness, jobID string) int {
	t.Helper()
	db := h.openDB()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM job_files WHERE job_id = ?`, jobID).Scan(&n); err != nil {
		t.Fatalf("count job_files for %s: %v", jobID, err)
	}
	return n
}

func TestSIGKILL_AddThenImmediateKill_PositiveControl(t *testing.T) {
	h := newHarness(t, lostAddFixture())
	jobID := h.AddJob()
	time.Sleep(1500 * time.Millisecond)
	h.Kill()
	h.Restart()

	slot, ok := h.Slot(jobID)
	if !ok {
		t.Fatalf("positive control: job %s absent after 1.5s grace before SIGKILL", jobID)
	}
	t.Logf("positive control: job %s survived with status %q", jobID, slot.Status)
}

func TestSIGKILL_AddThenImmediateKill(t *testing.T) {
	h := newHarness(t, lostAddFixture())
	jobID := h.AddJob()
	h.Kill()

	rowsBefore := jobFilesRowCount(t, h, jobID)
	manifestPath := manifestPathFor(h.AdminDir, jobID)
	_, manifestErrBefore := os.Stat(manifestPath)

	h.Restart()

	slot, ok := h.Slot(jobID)
	if !ok {
		t.Errorf("job %s is ABSENT from the queue after an immediate SIGKILL following AddJob", jobID)
	} else {
		t.Logf("job %s survived the kill and restart with status %q", jobID, slot.Status)
	}
	t.Logf("job_files rows for %s at kill time: %d", jobID, rowsBefore)
	if !ok && rowsBefore > 0 {
		t.Logf("ORPHAN: %d job_files row(s) remain with no queue entry", rowsBefore)
	}
	if !ok && manifestErrBefore == nil {
		t.Logf("ORPHAN: manifest %s remains with no queue entry", manifestPath)
	}
}
