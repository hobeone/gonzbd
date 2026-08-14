package queue

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// TestSnapshotJob_ServesTheResumeCorrectionNotTheStoredBelief pins the read
// path against the progress a resume disproved.
//
// hydrateSnapshot restores a non-resident job's progress from job_files
// unconditionally, and the per-job API endpoint serves the result.
// Application.Stall calls SnapshotJob immediately after Queue.Pause, so an
// operator polling to diagnose a stall reads this clone at exactly the moment
// they are looking — and a clone built from a pre-correction row shows an
// article as downloaded whose bytes the resume proved are not there.
//
// What closes it is that the correction reaches the STORE before the eviction
// can strand it: ReplaceFromResume persists a clearing correction before it
// returns, so the row this path re-reads is the corrected one. This test holds
// that end-to-end rather than trusting the two halves separately, because the
// bug is precisely that they were separate.
func TestSnapshotJob_ServesTheResumeCorrectionNotTheStoredBelief(t *testing.T) {
	const n = 4
	q := newTestQueueWithJob(t, "snap-stale", n)
	job := jobByID(t, q, "snap-stale")

	// A previous run recorded articles 0 and 1 as downloaded, on disk.
	ackDoneIdx(t, q, "snap-stale", 0, 1)
	if err := q.store.Update(t.Context(), job); err != nil {
		t.Fatalf("seed the stored row: %v", err)
	}

	// This run's recomputation disproves article 1.
	bm := durability.NewBitmap(n)
	bm.Set(0)
	if err := q.ReplaceFromResume("snap-stale", []durability.FileExtent{{FileIdx: 0, Durable: bm}}); err != nil {
		t.Fatalf("ReplaceFromResume: %v", err)
	}
	// Grounding: the correction must have landed in memory, or the snapshot
	// below agrees for a reason unrelated to the read path.
	if job.progress.ArticleDone(1) {
		t.Fatal("the correction never cleared article 1 in memory")
	}

	// Pausing evicts the manifest, which is the state hydrateSnapshot exists
	// for and the moment Application.Stall takes its snapshot.
	if err := q.Pause("snap-stale"); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	snap := q.SnapshotJob("snap-stale")
	if snap == nil {
		t.Fatal("SnapshotJob returned nil for a paused job")
	}
	if snap.Progress().ArticleDone(1) {
		t.Error("the snapshot served an article the resume disproved as downloaded. " +
			"This clone is what the per-job endpoint returns and what Stall reads " +
			"straight after pausing, so the operator diagnosing the stall is shown " +
			"the belief the resume had already corrected")
	}
	if !snap.Progress().ArticleDone(0) {
		t.Error("the snapshot lost article 0, which the resume confirmed")
	}
}
