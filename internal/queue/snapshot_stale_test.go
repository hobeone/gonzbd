package queue

import (
	"testing"
)

// TestSnapshotJob_ServesTheResumeCorrectionNotTheStoredBelief pins the read
// path against the state a resume disproved.
//
// hydrateSnapshot rebuilds a non-resident job's progress from the store
// unconditionally, and the per-job API endpoint serves the result.
// Application.Stall calls SnapshotJob immediately after Queue.Pause, so an
// operator polling to diagnose a stall reads this clone at exactly the moment
// they are looking — and a clone built from pre-correction state shows a file
// as complete whose bytes the resume proved are not all there.
//
// What closes it is that the correction reaches the STORE before the eviction
// can strand it: ReplaceFromRuns persists a clearing correction before it
// returns, so the row this path re-reads is the corrected one. This test holds
// that end-to-end rather than trusting the two halves separately, because the
// bug is precisely that they were separate.
//
// The ARTICLE half of the correction is structural now — the resume deletes
// the file's runs, and the queue derives resolution from what is left — so the
// assertions below cover both: the derived articles and the job_files columns
// that only this persist can carry.
func TestSnapshotJob_ServesTheResumeCorrectionNotTheStoredBelief(t *testing.T) {
	const perFile = 2
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	j := makeMultiFileJob(t, "snap-stale", 2, perFile)
	j.ID = "snap-stale"
	if err := q.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}
	job := jobByID(t, q, "snap-stale")

	// A previous run downloaded and completed both files, on disk.
	ackDoneIdx(t, q, "snap-stale", 0, 1, 2, 3)
	for _, fi := range []int{0, 1} {
		if err := q.MarkFileComplete("snap-stale", fi); err != nil {
			t.Fatalf("MarkFileComplete(%d): %v", fi, err)
		}
	}
	if err := q.store.Update(t.Context(), job); err != nil {
		t.Fatalf("seed the stored row: %v", err)
	}

	// This start's gate disproves file 0.
	discardRuns(t, q, "snap-stale", 0)
	if err := q.ReplaceFromRuns("snap-stale", []int32{0, 1}, fileRunsOf(1, perFile, 0, 1)); err != nil {
		t.Fatalf("ReplaceFromRuns: %v", err)
	}
	// Grounding: the correction must have landed in memory, or the snapshot
	// below agrees for a reason unrelated to the read path.
	if job.progress.FileComplete(0) {
		t.Fatal("the correction never cleared file 0's Complete flag in memory")
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
	p := snap.Progress()
	if p.FileComplete(0) {
		t.Error("the snapshot served a file the resume disproved as complete. " +
			"This clone is what the per-job endpoint returns and what Stall reads " +
			"straight after pausing, so the operator diagnosing the stall is shown " +
			"the belief the resume had already corrected")
	}
	if p.ArticleDone(0) {
		t.Error("the snapshot served an article whose run the resume discarded as downloaded")
	}
	if !p.FileComplete(1) || !p.ArticleDone(perFile) {
		t.Error("the snapshot lost the file the resume confirmed; the correction is " +
			"scoped to the file that was disproved")
	}
}
