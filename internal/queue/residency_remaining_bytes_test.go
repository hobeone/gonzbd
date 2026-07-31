package queue

import (
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/history"
)

// setupResidencyTestStore builds a fresh SQLiteStore backed by a real
// on-disk history DB, mirroring setupChallengerTestStore in
// challenger_m3_test.go. Package queue_test's setupTestStore
// (sqlite_store_test.go) is not reachable from package queue, so tests in
// this package construct the store directly the same way
// challenger_m3_test.go does.
func setupResidencyTestStore(t *testing.T) (*SQLiteStore, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "history.db")
	db, err := history.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	repo := history.NewRepository(db)
	store := NewSQLiteStore(repo.DB(), dir, repo)
	return store, dir
}

// TestTotalRemainingBytes_NonResidentJobsCounted reproduces issue #262: with
// more queued jobs than maxActive, every job beyond the active set is
// non-resident (job.progress == nil, per Queue.Add's de-hydration), and
// TotalRemainingBytes used to skip every one of them instead of falling back
// to a cached figure.
func TestTotalRemainingBytes_NonResidentJobsCounted(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	const nJobs = 4
	const bytesPerJob = int64(100_000) // makeMultiFileJob(t, name, 1, 1): one 100_000-byte article
	jobIDs := make([]string, 0, nJobs)
	for i := range nJobs {
		job := makeMultiFileJob(t, "queued-job-"+string(rune('A'+i)), 1, 1)
		if err := q.Add(job); err != nil {
			t.Fatalf("Add job %d: %v", i, err)
		}
		jobIDs = append(jobIDs, job.ID)
	}

	// Guard the fixture: with maxActive=1, exactly one job should have been
	// promoted to resident/Downloading; the rest must be non-resident
	// StatusQueued, or this test would pass vacuously.
	q.mu.RLock()
	var resident, nonResident int
	for _, id := range jobIDs {
		j := q.byID[id]
		if j.manifest != nil || j.progress != nil {
			resident++
		} else {
			nonResident++
		}
	}
	q.mu.RUnlock()
	if resident != 1 || nonResident != nJobs-1 {
		t.Fatalf("fixture not exercising the bug: resident=%d nonResident=%d, want 1 and %d", resident, nonResident, nJobs-1)
	}

	want := bytesPerJob * nJobs
	if got := q.TotalRemainingBytes(); got != want {
		t.Errorf("TotalRemainingBytes = %d, want %d (all %d jobs counted, none downloaded)", got, want, nJobs)
	}
}

// TestTotalRemainingBytes_RestartReconstructsNonResident builds a
// store-backed queue with more queued jobs than maxActive, completes some
// articles on a resident job, flushes to the store via Save, reloads a fresh
// Queue from the same directory via Loader.Load, and asserts
// TotalRemainingBytes is correct for the jobs that come back non-resident —
// without a real flush-and-reload, this test would not exercise
// Store.RemainingBytesByJob at all.
func TestTotalRemainingBytes_RestartReconstructsNonResident(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	// Job A: gets promoted (resident), we complete one of its two articles.
	jobA := makeMultiFileJob(t, "restart-active", 1, 2)
	if err := q.Add(jobA); err != nil {
		t.Fatalf("Add jobA: %v", err)
	}
	if err := q.MarkArticlesDone(jobA.ID, []string{articleID(0, 0)}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}

	// Jobs B, C: stay queued and non-resident (maxActive=1 already spent on A).
	jobB := makeMultiFileJob(t, "restart-queued-b", 1, 1)
	if err := q.Add(jobB); err != nil {
		t.Fatalf("Add jobB: %v", err)
	}
	jobC := makeMultiFileJob(t, "restart-queued-c", 1, 1)
	if err := q.Add(jobC); err != nil {
		t.Fatalf("Add jobC: %v", err)
	}

	q.mu.RLock()
	bResident := q.byID[jobB.ID].manifest != nil
	cResident := q.byID[jobC.ID].manifest != nil
	q.mu.RUnlock()
	if bResident || cResident {
		t.Fatal("fixture not exercising the bug: jobB/jobC still resident before save+reload")
	}

	// Flush to the store (saveStore path) so job_files reflects jobA's
	// completed article and jobB/jobC's untouched byte counts.
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload a fresh Queue from the same dir/store — this is the real
	// restart path under test (Loader.Load's store-backed branch). Reusing
	// the same *SQLiteStore is enough to force a true reload: q2 is a
	// brand-new Queue struct with an empty q2.byID, and Load's store-backed
	// branch always re-fetches every job from SQLite via store.List()
	// rather than reusing any in-memory state from q.
	q2, err := Load(dir, WithStore(store), WithMaxActiveJobs(1))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Guard: jobB and jobC must have come back non-resident (StatusQueued,
	// beyond maxActive), or this test is vacuous with respect to piece 3.
	q2.mu.RLock()
	bAfter, bOK := q2.byID[jobB.ID]
	cAfter, cOK := q2.byID[jobC.ID]
	q2.mu.RUnlock()
	if !bOK || !cOK {
		t.Fatalf("jobB/jobC missing after reload: bOK=%v cOK=%v", bOK, cOK)
	}
	if bAfter.manifest != nil || cAfter.manifest != nil {
		t.Fatal("fixture not exercising the bug: jobB/jobC resident after reload")
	}

	// jobA: 1 of 2 articles (100_000 bytes each) done → 100_000 remaining,
	// whichever job ended up resident/promoted after reload.
	// jobB, jobC: untouched, 1 article each → 100_000 remaining apiece.
	want := int64(100_000 + 100_000 + 100_000)
	if got := q2.TotalRemainingBytes(); got != want {
		t.Errorf("TotalRemainingBytes after reload = %d, want %d", got, want)
	}
}
