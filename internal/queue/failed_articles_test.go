package queue

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// TestRecordAndClearFailedArticles_RoundTrip pins the pair the queue owns.
//
// AckPermanentFailure is the only writer and the two reversal sites are the
// only readers of the clear, so the round trip is the whole contract: what is
// recorded comes back, a repeat is a no-op rather than an error, and the clear
// removes exactly one job's rows.
func TestRecordAndClearFailedArticles_RoundTrip(t *testing.T) {
	store, _, _ := setupResidencyTestStoreWithDB(t)
	ctx := context.Background()

	if err := store.RecordFailedArticles(ctx, "job-a", []int32{3, 7}); err != nil {
		t.Fatalf("RecordFailedArticles: %v", err)
	}
	if err := store.RecordFailedArticles(ctx, "job-b", []int32{1}); err != nil {
		t.Fatalf("RecordFailedArticles(job-b): %v", err)
	}
	// Idempotent: AckPermanentFailure is reachable more than once for the same
	// article, and the row carries no value beyond its own existence.
	if err := store.RecordFailedArticles(ctx, "job-a", []int32{3}); err != nil {
		t.Fatalf("re-recording an article already failed: %v", err)
	}

	got, err := store.failedArticlesForJob(ctx, "job-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("job-a has %d failed articles, want 2 (%v)", len(got), got)
	}

	// An empty batch must not open a transaction it has nothing to write in.
	if err := store.RecordFailedArticles(ctx, "job-a", nil); err != nil {
		t.Errorf("an empty batch reported an error: %v", err)
	}

	if err := store.ClearFailedArticles(ctx, "job-a"); err != nil {
		t.Fatalf("ClearFailedArticles: %v", err)
	}
	if got, err := store.failedArticlesForJob(ctx, "job-a"); err != nil || len(got) != 0 {
		t.Errorf("job-a has %d failed articles after the clear (err %v), want 0", len(got), err)
	}
	// Scoped, and this is the half that matters: a clear that swept every job
	// would resurrect the permanently failed articles of every NON-resident
	// one as fetchable work, and the symptom is a silent re-download storm
	// rather than an error.
	if got, err := store.failedArticlesForJob(ctx, "job-b"); err != nil || len(got) != 1 {
		t.Errorf("job-b has %d failed articles after clearing job-a (err %v), want 1", len(got), err)
	}
}

// TestFailedArticleWrites_SurfaceADatabaseFailure pins that neither half
// swallows a write failure.
//
// The record's caller treats a failure as bounded — R10 makes losing a
// permanent failure cost one re-request — but it can only make that judgement
// if the error reaches it. The CLEAR's caller cannot: a stale row survives a
// retry and makes the next restart mark the retry's re-fetched article failed,
// so Queue.Retry rolls back on it.
func TestFailedArticleWrites_SurfaceADatabaseFailure(t *testing.T) {
	store, _, _ := setupResidencyTestStoreWithDB(t)
	if err := store.db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx := context.Background()

	err := store.RecordFailedArticles(ctx, "job-a", []int32{1})
	if err == nil {
		t.Error("RecordFailedArticles reported success against a closed database")
	} else if !strings.Contains(err.Error(), "job-a") {
		t.Errorf("err = %q, want it to name the job", err)
	}

	if err := store.ClearFailedArticles(ctx, "job-a"); err == nil {
		t.Error("ClearFailedArticles reported success against a closed database; a stale " +
			"row survives the retry and the next restart marks its re-fetched article failed")
	}
}

// TestResolutionForJob_SurfacesAReadFailure pins that an unreadable record is
// an error rather than "nothing is resolved".
//
// The difference is a whole job's worth of rework. Reading a failure as an
// empty resolution makes every article of the job Outstanding, so a restart
// re-downloads a job that is already on disk — and says nothing about why.
func TestResolutionForJob_SurfacesAReadFailure(t *testing.T) {
	store, _, _ := setupResidencyTestStoreWithDB(t)
	if err := store.db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, _, err := store.resolutionForJob(context.Background(), "job-a", 4); err == nil {
		t.Error("resolutionForJob reported success against a closed database; an " +
			"unreadable record would read as an unstarted job and re-download it whole")
	}
}

// TestScanAll_NamesTheFailingSweep pins the reason scanAll takes a `what`
// string at all: fillResolution runs two sweeps over two tables, and an error
// that named neither would leave a reader unable to tell which record could
// not be read.
func TestScanAll_NamesTheFailingSweep(t *testing.T) {
	store, _, _ := setupResidencyTestStoreWithDB(t)
	ctx := context.Background()

	// A row the scan function refuses, so the per-row branch is reached rather
	// than only the query one.
	if err := store.RecordFailedArticles(ctx, "job-a", []int32{1}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("scan refused")
	err := store.scanAll(ctx, `SELECT job_id FROM failed_articles`, "failed articles",
		func(*sql.Rows) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the scan failure", err)
	}
	if !strings.Contains(err.Error(), "failed articles") {
		t.Errorf("err = %q, want it to name the sweep that failed", err)
	}

	// And the query itself failing.
	if err := store.db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err = store.scanAll(ctx, `SELECT job_id FROM durable_runs`, "durable runs",
		func(*sql.Rows) error { return nil })
	if err == nil {
		t.Fatal("scanAll reported success against a closed database")
	}
	if !strings.Contains(err.Error(), "durable runs") {
		t.Errorf("err = %q, want it to name the sweep that failed", err)
	}
}
