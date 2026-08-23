package queue

import (
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
)

// storeRoundTrip persists j through a real SQLite store and reads it back as
// a restarted daemon would: Add, checkpoint via Save, then Load into a fresh
// Queue and take a hydrated snapshot.
//
// This is the channel that replaced SaveJob/LoadJob. Those existed only for
// the history retry payload, and #298 replaced that with the NZB backup plus
// retained per-file progress — so a test that wants "does this job's state
// survive being written down" has to go through the store, which is the only
// thing that writes a live job down at all.
//
// The job is forced to StatusDownloading because SQLiteStore.Get restores
// per-file progress only for a job in a resident status. A job left at the
// NewJob default of StatusQueued comes back with zeroed progress and every
// assertion about article state then passes or fails for the wrong reason.
//
// The durability record is written explicitly, because Save does not write it.
// Article resolution is DERIVED from durable_runs and failed_articles rather
// than re-serialised into a queue column on every update, so a job whose
// progress was only ever mutated in memory persists no resolution at all —
// which is exactly what production does when no barrier has run. Callers
// arrive here having marked articles on a store-less queue, so this is the
// first point at which there is a store to record against.
func storeRoundTrip(t *testing.T, j *Job) *Job {
	t.Helper()

	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	repo := history.NewRepository(db)
	store := NewSQLiteStore(repo.DB(), dir, repo)

	j.Status = constants.StatusDownloading
	q := New(WithStore(store), WithStateDir(dir))
	if err := q.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}
	recordDurability(t, store, j)
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(dir, WithStore(store))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := loaded.SnapshotJob(j.ID)
	if got == nil {
		t.Fatalf("job %s missing from the reloaded queue", j.ID)
	}
	return got
}
