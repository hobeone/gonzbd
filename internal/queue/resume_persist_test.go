package queue

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
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

// TestReplaceFromResume_SurvivesAReHydration pins the durability of the one
// correction that can LOSE ground.
//
// ReplaceFromResume is the authoritative sweep: where the recomputation
// disagrees with the stored bits, the recomputation wins. It applied that to
// the in-memory JobProgress and set a dirty flag, and nothing persisted it
// synchronously — the first save could be a whole checkpoint interval away.
//
// Every re-hydration in the queue re-reads job_files.articles_done
// unconditionally (PromoteNext, hydrateSnapshot, SetStatus), so any eviction
// and re-promotion inside that window refills the corrected progress from the
// pre-correction row and the disproven article is Done again. The file's
// Complete flag and AssembledCRC32 come back with it.
//
// This is not a race. resumeAllJobs calls ReplaceFromResume and then Stall on
// the same job whenever any OTHER file of that job faulted, and Stall pauses
// the job, which evicts the manifest with the correction still unsaved. The
// hazard was already understood for one caller — Queue.Retry persists before
// returning, citing this exact re-read — and was never generalized.
func TestReplaceFromResume_SurvivesAReHydration(t *testing.T) {
	const n = 4
	q := newTestQueueWithJob(t, "resume-persist", n)
	job := jobByID(t, q, "resume-persist")

	// A previous run recorded articles 0 and 1 as downloaded, and that belief
	// is on disk.
	ackDoneIdx(t, q, "resume-persist", 0, 1)
	if err := q.store.Update(t.Context(), job); err != nil {
		t.Fatalf("seed the stored row: %v", err)
	}

	// Grounding: the stored row really does carry article 1, so the assertion
	// below is about the correction rather than about an empty row.
	if err := q.store.RestoreJobProgress(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	if !job.progress.ArticleDone(1) {
		t.Fatal("fixture never persisted article 1 as done, so it cannot observe the correction being lost")
	}

	// This run's recomputation disproves article 1: the bytes on disk do not
	// match its recorded CRC. Article 0 still holds.
	bm := durability.NewBitmap(n)
	bm.Set(0)
	if err := q.ReplaceFromResume("resume-persist", []durability.FileExtent{{FileIdx: 0, Durable: bm}}); err != nil {
		t.Fatalf("ReplaceFromResume: %v", err)
	}
	if job.progress.ArticleDone(1) {
		t.Fatal("ReplaceFromResume did not clear the disproven article in memory")
	}

	// The re-hydration every promotion performs. Nothing saved the queue in
	// between, which is the whole point.
	if err := q.store.RestoreJobProgress(t.Context(), job); err != nil {
		t.Fatal(err)
	}

	if job.progress.ArticleDone(1) {
		t.Error("a re-hydration resurrected an article the resume disproved. " +
			"Its bytes do not match its recorded CRC, so the file will finalize " +
			"with a hole and never re-fetch them")
	}
	if !job.progress.ArticleDone(0) {
		t.Error("article 0 was still durable and must not have been cleared")
	}
}
