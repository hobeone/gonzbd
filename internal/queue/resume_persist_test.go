package queue

import (
	"testing"
)

// jobByID reaches the live job for a test that needs to drive the store
// directly, the way PromoteNext and hydrateSnapshot do.
func jobByID(t *testing.T, q *Queue, jobID string) *Job {
	t.Helper()
	q.mu.RLock()
	defer q.mu.RUnlock()
	job, ok := q.byID[jobID]
	if !ok {
		t.Fatalf("job %s not in queue", jobID)
	}
	return job
}

// TestReplaceFromRuns_SurvivesAReHydration pins the durability of the one
// correction that can LOSE ground.
//
// Half of it is now structural. Article resolution is DERIVED from the
// durability record on every re-hydration, and durability.Resumer's gate
// deletes a disproved file's runs before the sweep ever reaches the queue — so
// there is no longer a second belief for a stale row to resurrect the articles
// from. That is what the two-record collapse bought here.
//
// The other half is not, and it is what this test is for. Complete and
// AssembledCRC32 live on job_files and nowhere else. ReplaceFromRuns clears
// them in MEMORY for a file whose articles it returned to Outstanding, and
// nothing else in the queue ever re-derives them — so unless the correction
// reaches the row before the job is evicted, the next promotion brings a
// Complete flag and a whole-file checksum back over a file the assembler has
// bytes left to write into.
//
// It is not a race. resumeAllJobs calls this and then Stall on the same job
// whenever any OTHER file of it faulted, and Stall pauses the job, which
// evicts the manifest with the correction still unsaved.
//
// Two files, because a fixture with one cannot tell "the correction landed"
// from "everything was cleared": file 0 is disproved and file 1 is untouched,
// and both directions are asserted.
func TestReplaceFromRuns_SurvivesAReHydration(t *testing.T) {
	const perFile = 2
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	j := makeMultiFileJob(t, "resume-persist", 2, perFile)
	j.ID = "resume-persist"
	if err := q.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}
	job := jobByID(t, q, "resume-persist")

	// A previous run downloaded and completed both files, and that is on
	// stable storage: the runs, the Complete flags and the assembled CRCs.
	ackDoneIdx(t, q, "resume-persist", 0, 1, 2, 3)
	for _, fi := range []int{0, 1} {
		if err := q.MarkFileComplete("resume-persist", fi); err != nil {
			t.Fatalf("MarkFileComplete(%d): %v", fi, err)
		}
		if err := q.SetFileCRC32("resume-persist", fi, 0xC0FFEE); err != nil {
			t.Fatalf("SetFileCRC32(%d): %v", fi, err)
		}
	}
	if err := q.store.Update(t.Context(), job); err != nil {
		t.Fatalf("seed the stored row: %v", err)
	}

	// Grounding: the stored state really does carry all four articles and
	// both flags, so the assertions below are about the correction rather
	// than about an empty row.
	if err := q.store.RestoreJobProgress(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		if !job.progress.ArticleDone(i) {
			t.Fatalf("fixture never persisted article %d as done, so it cannot observe "+
				"the correction being lost", i)
		}
	}
	if !job.progress.FileComplete(0) {
		t.Fatal("fixture never persisted file 0 as Complete")
	}

	// This start's gate finds file 0 shorter than its runs claim and discards
	// them; file 1 is intact and keeps its own.
	discardRuns(t, q, "resume-persist", 0)
	surviving := fileRunsOf(1, perFile, 0, 1)
	if err := q.ReplaceFromRuns("resume-persist", []int32{0, 1}, surviving); err != nil {
		t.Fatalf("ReplaceFromRuns: %v", err)
	}
	if job.progress.FileComplete(0) {
		t.Fatal("ReplaceFromRuns did not clear the disproved file's Complete flag in memory")
	}

	// The re-hydration every promotion performs. Nothing saved the queue in
	// between, which is the whole point.
	if err := q.store.RestoreJobProgress(t.Context(), job); err != nil {
		t.Fatal(err)
	}

	if job.progress.FileComplete(0) {
		t.Error("a re-hydration resurrected the Complete flag of a file the resume " +
			"disproved. Nothing else re-derives that flag, so the job goes to " +
			"post-processing over a file with a hole in it")
	}
	if got := job.progress.FileAssembledCRC32(0); got != 0 {
		t.Errorf("a re-hydration resurrected the assembled CRC (%#x) of a file that has "+
			"lost bytes; QuickCheck would trust it", got)
	}
	for i := range perFile {
		if job.progress.ArticleDone(i) {
			t.Errorf("article %d of the disproved file came back Done; its runs were "+
				"deleted, so nothing should be able to derive that", i)
		}
	}
	if !job.progress.FileComplete(1) {
		t.Error("the untouched file lost its Complete flag; the correction is scoped to " +
			"the file the resume disproved")
	}
	for i := perFile; i < 2*perFile; i++ {
		if !job.progress.ArticleDone(i) {
			t.Errorf("article %d of the untouched file was cleared", i)
		}
	}
}
