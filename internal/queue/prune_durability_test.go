package queue

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
)

// addPruneJob puts a job in the jobs table -- the only thing Prune reads from
// it -- and returns its ID.
func addPruneJob(t *testing.T, store *SQLiteStore, id string) string {
	t.Helper()
	if err := store.Add(t.Context(), &Job{
		ID:       id,
		Filename: id + ".nzb",
		Name:     id,
		Category: "default",
		Priority: constants.NormalPriority,
		Status:   constants.StatusQueued,
		PP:       3,
		Added:    time.Now().Truncate(time.Second),
		AvgAge:   time.Now().Truncate(time.Second),
	}); err != nil {
		t.Fatalf("Add(%s): %v", id, err)
	}
	return id
}

// pruneFixture builds a store plus the two durability stores that write the
// tables Prune has to sweep, all against one database.
func pruneFixture(t *testing.T) (*SQLiteStore, *history.Repository, *durability.SQLiteFactLog, *durability.SQLiteExtentStore) {
	t.Helper()
	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	return NewSQLiteStore(repo.DB(), dir, repo), repo,
		durability.NewSQLiteFactLog(repo.DB()), durability.NewSQLiteExtentStore(repo.DB())
}

// seedDurability writes one Class A fact and one Class B extent for jobID.
func seedDurability(t *testing.T, fl *durability.SQLiteFactLog, es *durability.SQLiteExtentStore, jobID string) {
	t.Helper()
	ctx := context.Background()
	if err := fl.Append(ctx, jobID, []durability.ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x1234},
	}); err != nil {
		t.Fatalf("Append(%s): %v", jobID, err)
	}
	bm := durability.NewBitmap(1)
	bm.Set(0)
	if err := es.Commit(ctx, jobID, []durability.FileExtent{{
		FileIdx: 0, Durable: bm, BytesDurable: 100, Size: 100, ModTimeNs: 1,
	}}); err != nil {
		t.Fatalf("Commit(%s): %v", jobID, err)
	}
}

// hasDurability reports whether either table still holds a row for jobID.
func hasDurability(t *testing.T, fl *durability.SQLiteFactLog, es *durability.SQLiteExtentStore, jobID string) (facts, extents bool) {
	t.Helper()
	ctx := context.Background()
	f, err := fl.ForFile(ctx, jobID, 0)
	if err != nil {
		t.Fatalf("ForFile(%s): %v", jobID, err)
	}
	e, err := es.Load(ctx, jobID)
	if err != nil {
		t.Fatalf("Load(%s): %v", jobID, err)
	}
	return len(f) > 0, len(e) > 0
}

// TestPrune_SweepsDurabilityRowsOnlyForFullyDepartedJobs pins the backstop and
// the exception it must respect, in one fixture, because a green result on
// either arm alone says nothing.
//
// article_facts and file_extents are keyed by job ID with no foreign key to
// jobs, so nothing collects them implicitly. The ordinary deletions all run on
// a job's way out; what survives them is a crash in the window between the job
// leaving jobs and its rows being deleted, and after that no later pass looks.
//
// The FAILED arm is the half that matters. Those rows are retained
// deliberately: a retry bounds Barrier.FinalizeFile's truncate to the whole
// partial file using them, rather than to the few articles it re-fetches. The
// obvious "job_id NOT IN (SELECT id FROM jobs)" predicate destroys exactly
// that ground, and does it silently.
func TestPrune_SweepsDurabilityRowsOnlyForFullyDepartedJobs(t *testing.T) {
	ctx := context.Background()
	store, repo, fl, es := pruneFixture(t)

	live := addPruneJob(t, store, "job-live")
	for _, id := range []string{live, "job-failed", "job-done", "job-gone"} {
		seedDurability(t, fl, es, id)
	}
	// Two history entries that differ only in status, so the assertions below
	// isolate the status rule rather than "is it in history at all".
	for _, e := range []history.Entry{
		{NzoID: "job-failed", Name: "failed", Status: string(constants.StatusFailed)},
		{NzoID: "job-done", Name: "done", Status: string(constants.StatusCompleted)},
	} {
		if err := repo.Add(ctx, e); err != nil {
			t.Fatalf("history.Add(%s): %v", e.NzoID, err)
		}
	}

	// Grounding: every job must actually have rows before the sweep, or a
	// "swept" assertion passes on a fixture that never wrote them.
	for _, id := range []string{live, "job-failed", "job-done", "job-gone"} {
		if f, e := hasDurability(t, fl, es, id); !f || !e {
			t.Fatalf("fixture for %s has facts=%v extents=%v before Prune, want both true", id, f, e)
		}
	}

	if err := store.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	for _, tc := range []struct {
		jobID string
		want  bool
		why   string
	}{
		{live, true, "the job is still in the queue; sweeping it destroys a live download's resume ground"},
		{"job-failed", true, "the job is in history as FAILED, whose facts a retry uses to bound FinalizeFile's truncate to the whole partial file"},
		{"job-done", false, "the job completed, so nothing will ever resume against it"},
		{"job-gone", false, "the job is in neither jobs nor history, which is exactly the crash-window orphan this sweep exists for"},
	} {
		facts, extents := hasDurability(t, fl, es, tc.jobID)
		if facts != tc.want {
			t.Errorf("article_facts for %s present = %v, want %v — %s", tc.jobID, facts, tc.want, tc.why)
		}
		if extents != tc.want {
			t.Errorf("file_extents for %s present = %v, want %v — %s", tc.jobID, extents, tc.want, tc.why)
		}
	}
}

// TestPrune_SweepsWhenAKeyColumnHoldsNull pins the NULL trap in both
// subqueries.
//
// SQLite permits NULL in a TEXT PRIMARY KEY on a rowid table, so jobs.id and
// history.nzo_id can each hold one — not from this code, which always supplies
// a Go string, but from a hand-edited or partially-written database. A single
// NULL anywhere in a NOT IN subquery makes the predicate NULL for EVERY row
// and the delete then matches nothing: the sweep silently stops sweeping, with
// no error and nothing observable from outside. NOT EXISTS compares row by row
// and is unaffected.
//
// The failure direction is retention rather than loss, which is why it is
// worth a test: it would never surface as a bug, only as this code quietly
// never working.
//
// Both arms are needed and neither subsumes the other. The history NULL must
// carry status Failed to reach the subquery at all — a NULL behind the status
// filter is invisible, which is how the first draft of this test passed
// against a NOT IN implementation and pinned nothing.
func TestPrune_SweepsWhenAKeyColumnHoldsNull(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed string
	}{
		{
			name: "jobs.id",
			seed: `INSERT INTO jobs (id, filename, name, priority, status, pp, sort_key,
			                         time_added, md5, avg_age)
			       VALUES (NULL, 'x.nzb', 'x', 0, 'Queued', 3, 0, 0, '', 0)`,
		},
		{
			// Failed, not Completed: the status filter is applied inside the
			// subquery, so a NULL behind any other status never reaches it.
			name: "history.nzo_id",
			seed: `INSERT INTO history (nzo_id, name, status) VALUES (NULL, 'orphan', 'Failed')`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, _, fl, es := pruneFixture(t)
			seedDurability(t, fl, es, "job-gone")
			if _, err := store.db.ExecContext(ctx, tc.seed); err != nil {
				t.Fatalf("seed NULL key: %v", err)
			}

			if err := store.Prune(ctx); err != nil {
				t.Fatalf("Prune: %v", err)
			}

			if facts, extents := hasDurability(t, fl, es, "job-gone"); facts || extents {
				t.Errorf("a departed job kept facts=%v extents=%v after Prune. One NULL in "+
					"%s disabled the whole sweep, which is what NOT IN does and NOT EXISTS "+
					"does not", facts, extents, tc.name)
			}
		})
	}
}

// TestPruneDurabilityRows_SurfacesADatabaseFailure pins the one way this
// differs from everything else in Prune.
//
// The cleanDir calls above it discard their errors deliberately: a manifest
// that cannot be removed is a single orphaned file. A failing DELETE is the
// whole sweep not running, and Queue.persist propagates Prune's error, so
// returning it is what makes the next save retry instead of reporting a prune
// that did not happen.
func TestPruneDurabilityRows_SurfacesADatabaseFailure(t *testing.T) {
	store, _, _, _ := pruneFixture(t)
	if err := store.db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err := store.pruneDurabilityRows(context.Background())
	if err == nil {
		t.Fatal("pruneDurabilityRows reported success against a closed database; the sweep " +
			"silently did not run and the caller has no way to know")
	}
	if !strings.Contains(err.Error(), "prune durability rows") {
		t.Errorf("err = %q, want it to name the failing step", err)
	}
}
