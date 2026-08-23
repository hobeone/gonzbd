package queue

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

// TestReplaceFromRuns_CorrectsANonResidentJob pins the residency handling
// the startup sweep depends on.
//
// A PAUSED job is not resident, and it is exactly the job that needs this:
// Application.Stall leaves jobs Paused, and the sweep skipping them let #362
// survive in that branch — the disproven Done bits were never corrected, so
// the next checkpoint re-recorded them and the file finalized over a hole.
//
// Both halves are asserted: the correction lands, and the job's residency is
// unchanged afterwards. Leaving it hydrated would put a job the active set does
// not account for into the residency budget docs/queue-lifecycle.md exists to
// bound.
func TestReplaceFromRuns_CorrectsANonResidentJob(t *testing.T) {
	q := newTestQueueWithJob(t, "job-1", 2)
	job := q.byID["job-1"]

	// Both articles believed done, as a previous process recorded them.
	if err := q.SeedFromRuns(job.ID, runsOf(0, 1)); err != nil {
		t.Fatalf("SeedFromRuns: %v", err)
	}
	if err := q.Pause(job.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	// Grounding: the job must really be non-resident, or this exercises the
	// path it was already on.
	q.mu.Lock()
	live := q.byID[job.ID]
	resident := live != nil && live.manifest != nil
	q.mu.Unlock()
	if resident {
		t.Fatal("the paused job is still resident, so this cannot observe the hydration")
	}
	if got := q.SnapshotJob(job.ID); got == nil || got.Status != constants.StatusPaused {
		t.Fatal("the fixture is not paused")
	}

	// A resume whose gate left only article 0's run standing.
	if err := q.ReplaceFromRuns(job.ID, []int32{0}, runsOf(0)); err != nil {
		t.Fatalf("ReplaceFromRuns: %v", err)
	}

	p := q.SnapshotJob(job.ID).Progress()
	if !p.ArticleDone(0) {
		t.Error("the article the resume PROVED was cleared; only what it disproved may be")
	}
	if p.ArticleDone(1) {
		t.Error("the article whose run the resume discarded is still Done. Nothing else " +
			"corrects it, so the next checkpoint re-records it and the file finalizes " +
			"over a hole (#362)")
	}

	// Residency restored: hydrating for the correction must not leave the job
	// resident, or a paused queue's manifests all stay in memory after a sweep.
	q.mu.Lock()
	live = q.byID[job.ID]
	stillResident := live != nil && live.manifest != nil
	q.mu.Unlock()
	if stillResident {
		t.Error("the job was left hydrated. The sweep touches every paused job at " +
			"startup, so this is one manifest per paused job held for the life of the " +
			"process, outside the budget the active set enforces")
	}
}

// TestReplaceFromRuns_RefusesWhatItCannotHydrate covers the two ways the
// hydration can fail to produce a job to correct.
//
// Both must return an error rather than silently doing nothing. The caller —
// the startup sweep — treats a failure as "the job re-fetches what it could not
// be told it has", which is the safe direction; reporting success would let it
// move on believing a correction landed that did not.
func TestReplaceFromRuns_RefusesWhatItCannotHydrate(t *testing.T) {
	q := newTestQueueWithJob(t, "job-1", 2)

	swept := []int32{0}
	proved := runsOf(0)

	t.Run("no such job", func(t *testing.T) {
		if err := q.ReplaceFromRuns("never-added", swept, proved); err == nil {
			t.Error("a correction for a job that is not in the queue reported success")
		}
	})

	t.Run("no swept files is not an error", func(t *testing.T) {
		// The sweep omits a file it never resumed, and a job whose files were
		// all omitted yields an empty slice. That is silence, not a finding of
		// absence, and must not be reported as a failure.
		if err := q.ReplaceFromRuns("job-1", nil, nil); err != nil {
			t.Errorf("an empty correction was reported as a failure: %v", err)
		}
	})

	t.Run("an unreadable manifest", func(t *testing.T) {
		if err := q.Pause("job-1"); err != nil {
			t.Fatalf("Pause: %v", err)
		}
		// The manifest the hydration would read is gone, which is the state a
		// crash between the queue write and the manifest write leaves.
		if err := os.RemoveAll(filepath.Join(q.stateDir, "manifests")); err != nil {
			t.Fatal(err)
		}
		if err := q.ReplaceFromRuns("job-1", swept, proved); err == nil {
			t.Error("a correction for a job whose manifest cannot be read reported " +
				"success; the sweep would move on believing it landed")
		}
	})
}
