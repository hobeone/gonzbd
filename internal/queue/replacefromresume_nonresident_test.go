package queue

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
)

// TestReplaceFromResume_CorrectsANonResidentJob pins the residency handling
// the startup sweep depends on.
//
// A PAUSED job is not resident, and it is exactly the job that needs this:
// Application.Stall leaves jobs Paused, and the sweep skipping them let #362
// survive in that branch — the disproven Done bits were never corrected,
// priorExtent ORs the stored bitmap as its base, so the next checkpoint
// re-committed them with a fresh matching stamp and the file finalized over a
// hole.
//
// Both halves are asserted: the correction lands, and the job's residency is
// unchanged afterwards. Leaving it hydrated would put a job the active set does
// not account for into the residency budget docs/queue-lifecycle.md exists to
// bound.
func TestReplaceFromResume_CorrectsANonResidentJob(t *testing.T) {
	q := newTestQueueWithJob(t, "job-1", 2)
	job := q.byID["job-1"]

	// Both articles believed done, as a previous process recorded them.
	both := durability.NewBitmap(2)
	both.Set(0)
	both.Set(1)
	if err := q.SeedFromExtents(job.ID, []durability.FileExtent{{FileIdx: 0, Durable: both}}); err != nil {
		t.Fatalf("SeedFromExtents: %v", err)
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

	// A resume that proved only article 0.
	proved := durability.NewBitmap(2)
	proved.Set(0)
	if err := q.ReplaceFromResume(job.ID, []durability.FileExtent{{FileIdx: 0, Durable: proved}}); err != nil {
		t.Fatalf("ReplaceFromResume: %v", err)
	}

	p := q.SnapshotJob(job.ID).Progress()
	if !p.ArticleDone(0) {
		t.Error("the article the resume PROVED was cleared; only what it disproved may be")
	}
	if p.ArticleDone(1) {
		t.Error("the disproven article is still Done. Nothing clears a bit in the extent " +
			"store, so the next checkpoint re-commits it with a fresh matching stamp and " +
			"the file finalizes over a hole (#362)")
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

// TestReplaceFromResume_RefusesWhatItCannotHydrate covers the two ways the
// hydration can fail to produce a job to correct.
//
// Both must return an error rather than silently doing nothing. The caller —
// the startup sweep — treats a failure as "the job re-fetches what it could not
// be told it has", which is the safe direction; reporting success would let it
// move on believing a correction landed that did not.
func TestReplaceFromResume_RefusesWhatItCannotHydrate(t *testing.T) {
	q := newTestQueueWithJob(t, "job-1", 2)

	bm := durability.NewBitmap(2)
	bm.Set(0)
	exts := []durability.FileExtent{{FileIdx: 0, Durable: bm}}

	t.Run("no such job", func(t *testing.T) {
		if err := q.ReplaceFromResume("never-added", exts); err == nil {
			t.Error("a correction for a job that is not in the queue reported success")
		}
	})

	t.Run("no extents is not an error", func(t *testing.T) {
		// The sweep omits a file it never resumed, and a job whose files were
		// all omitted yields an empty slice. That is silence, not a finding of
		// absence, and must not be reported as a failure.
		if err := q.ReplaceFromResume("job-1", nil); err != nil {
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
		if err := q.ReplaceFromResume("job-1", exts); err == nil {
			t.Error("a correction for a job whose manifest cannot be read reported " +
				"success; the sweep would move on believing it landed")
		}
	})
}
