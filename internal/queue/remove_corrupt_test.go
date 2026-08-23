package queue

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
)

// countDurabilityRows returns how many durable runs and failed-article rows the
// database still holds for a job.
func countDurabilityRows(t *testing.T, db *sql.DB, jobID string) (runs, failed int) {
	t.Helper()
	if err := db.QueryRow(`SELECT COUNT(*) FROM durable_runs WHERE job_id = ?`, jobID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM failed_articles WHERE job_id = ?`, jobID).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	return runs, failed
}

// TestGet_CorruptManifestTakesTheDurabilityRowsWithTheJob pins the self-healing
// removal path's sweep.
//
// Get removes a job whose manifest it cannot read, and that removal is the end
// of the job: there is no queue row left, and no history entry is created, so
// nothing later arrives to collect anything. durable_runs and failed_articles
// are keyed by job ID with no foreign key to jobs, so before this they simply
// stayed — one set per corrupted job, forever.
//
// Dropping them is safe here specifically because the manifest is what is
// gone. Every durable_runs and failed_articles row is keyed by a global
// article index that only a manifest can interpret, so there is nothing left
// to read them against. That
// is why this is not done in Remove generally: Remove also runs on the
// queue-to-history transition, where a FAILED job's runs are retained on
// purpose for its retry.
func TestGet_CorruptManifestTakesTheDurabilityRowsWithTheJob(t *testing.T) {
	store, dir, db := setupResidencyTestStoreWithDB(t)
	ctx := context.Background()
	job := makeMultiFileJob(t, "corrupt-manifest", 1, 2)
	// Resident: Get only reads the manifest for a job in a downloading phase,
	// so a queued fixture would never reach the branch under test.
	job.Status = constants.StatusDownloading
	if err := store.Add(ctx, job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := durability.NewSQLiteRunStore(db).Commit(ctx, job.ID, []durability.DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordFailedArticles(ctx, job.ID, []int32{1}); err != nil {
		t.Fatal(err)
	}
	if r, f := countDurabilityRows(t, db, job.ID); r != 1 || f != 1 {
		t.Fatalf("fixture wrote %d runs and %d failed rows, want 1 and 1; the test would "+
			"pass vacuously against rows that were never there", r, f)
	}

	// Corrupt the manifest so Get self-heals.
	manifest := filepath.Join(dir, "manifests", job.ID+".json.gz")
	if err := os.WriteFile(manifest, []byte("not gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, job.ID); err == nil {
		t.Fatal("Get returned nil error for an unreadable manifest")
	}

	r, f := countDurabilityRows(t, db, job.ID)
	if r != 0 || f != 0 {
		t.Errorf("after the self-healing removal the job still has %d runs and %d failed "+
			"rows; no queue row and no history entry remain, so nothing will ever collect "+
			"them and they accumulate one set per corrupted job", r, f)
	}
}

// TestRemoveCorrupt_SweepsEvenWhenTheJobRowIsAlreadyGone covers the helper
// directly, including the branch a corrupt-manifest Get cannot reach.
//
// Remove returns ErrNotFound when there is no jobs row, and removeCorrupt
// ignores that deliberately: the rows it is here to collect are keyed by job
// ID alone and outlive the row entirely, so stopping on "no such job" would
// skip the only cleanup this path performs. A prior partial removal — the
// jobs row deleted, the sweep interrupted — is exactly the state that leaves
// them stranded, and it is the state a retry of this helper has to fix.
func TestRemoveCorrupt_SweepsEvenWhenTheJobRowIsAlreadyGone(t *testing.T) {
	store, _, db := setupResidencyTestStoreWithDB(t)
	ctx := context.Background()

	if err := durability.NewSQLiteRunStore(db).Commit(ctx, "ghost-job", []durability.DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordFailedArticles(ctx, "ghost-job", []int32{1}); err != nil {
		t.Fatal(err)
	}
	if r, f := countDurabilityRows(t, db, "ghost-job"); r != 1 || f != 1 {
		t.Fatalf("fixture wrote %d runs and %d failed rows, want 1 and 1", r, f)
	}

	store.removeCorrupt(ctx, "ghost-job")

	if r, f := countDurabilityRows(t, db, "ghost-job"); r != 0 || f != 0 {
		t.Errorf("removeCorrupt left %d runs and %d failed rows behind for a job with no "+
			"queue row; Remove's ErrNotFound must not stop the sweep, because these rows "+
			"are keyed by job ID alone and are precisely what outlives the row", r, f)
	}
}

// TestRemoveCorrupt_ReportsAFailedSweepRatherThanPanicking covers the error
// branch of the sweep.
//
// A DELETE that fails leaves rows behind, which is a leak rather than a
// correctness failure — the caller is already returning an error about the
// corruption that brought it here, and there is nothing useful to do with a
// second one. It must still not be silent (A2), and it must not take the
// process down: this runs on the load path at startup, where a panic would
// turn one unreadable manifest into a daemon that cannot boot.
func TestRemoveCorrupt_ReportsAFailedSweepRatherThanPanicking(t *testing.T) {
	store, _, db := setupResidencyTestStoreWithDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	store.removeCorrupt(context.Background(), "any-job")
}
