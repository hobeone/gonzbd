package queue

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
)

// failedJobInHistory builds a store-backed queue holding one 2-file,
// 3-article-per-file job, resolves article (0,0) successfully and (0,1) as a
// failure, then moves the job to history as Failed so its progress is
// retained. It returns the store and the original job.
//
// The two article states matter: articles_done packs done and failed as
// separate bitmaps, and a retry must refetch the failed one while leaving the
// succeeded one alone. A test that only marked articles done could not tell
// the two apart.
func failedJobInHistory(t *testing.T) (*SQLiteStore, *Job) {
	t.Helper()

	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	repo := history.NewRepository(db)
	store := NewSQLiteStore(repo.DB(), dir, repo)
	q := New(WithStore(store))

	job := makeMultiFileJob(t, "retry", 2, 3)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q.PromoteNext(context.Background())

	ackDone(t, q, job.ID, articleID(0, 0))
	ackFailed(t, q, job.ID, articleID(0, 1))
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	entry := history.Entry{NzoID: job.ID, Name: job.Name, Status: string(constants.StatusFailed)}
	if err := store.MoveToHistory(t.Context(), job, entry); err != nil {
		t.Fatalf("MoveToHistory: %v", err)
	}
	return store, job
}

// TestRestoreRetryProgress_AppliesAlignedOverlay pins that a rebuilt job
// picks up the retained per-article state: the article that succeeded stays
// done so a retry does not refetch it, and the one that failed comes back
// marked failed so ResetForRetry can clear it.
func TestRestoreRetryProgress_AppliesAlignedOverlay(t *testing.T) {
	store, orig := failedJobInHistory(t)

	rebuilt := makeMultiFileJob(t, "retry", 2, 3)
	rebuilt.ID = orig.ID

	applied, err := store.RestoreRetryProgress(t.Context(), rebuilt)
	if err != nil {
		t.Fatalf("RestoreRetryProgress: %v", err)
	}
	if !applied {
		t.Fatal("overlay refused for a manifest that matches the retained shape")
	}

	p := rebuilt.Progress()
	if !p.ArticleDone(0) || p.ArticleFailed(0) {
		t.Errorf("article 0: done=%v failed=%v, want done and not failed",
			p.ArticleDone(0), p.ArticleFailed(0))
	}
	if !p.ArticleFailed(1) {
		t.Error("article 1 lost its failed bit; ResetForRetry would not refetch it")
	}
	if p.ArticleDone(2) {
		t.Error("article 2 was never resolved but came back done")
	}
}

// TestRestoreRetryProgress_RefusesMisalignedOverlay pins that an overlay is
// refused when the rebuilt manifest does not match the shape the bitmap was
// written against.
//
// articles_done is indexed positionally, so applying it to a different
// manifest marks the wrong articles done — and a wrongly-done article is
// never requested, so the job would finish "successfully" with missing data.
// Refusing costs a full re-download; applying costs silent corruption.
func TestRestoreRetryProgress_RefusesMisalignedOverlay(t *testing.T) {
	store, orig := failedJobInHistory(t)

	// Same file count, different article count per file.
	rebuilt := makeMultiFileJob(t, "retry", 2, 4)
	rebuilt.ID = orig.ID

	applied, err := store.RestoreRetryProgress(t.Context(), rebuilt)
	if err != nil {
		t.Fatalf("RestoreRetryProgress: %v", err)
	}
	if applied {
		t.Fatal("overlay applied to a manifest with a different article count")
	}
	p := rebuilt.Progress()
	for i := range 8 {
		if p.ArticleDone(i) {
			t.Errorf("article %d marked done by a refused overlay", i)
		}
	}
}

// TestRestoreRetryProgress_NothingRetained pins that a job with no retained
// rows is reported as not-applied rather than as an error. A completed entry
// retains nothing by design, and an entry predating retention has nothing
// either; both are ordinary and mean "re-download from scratch".
func TestRestoreRetryProgress_NothingRetained(t *testing.T) {
	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	store := NewSQLiteStore(repo.DB(), dir, repo)

	job := makeMultiFileJob(t, "retry", 1, 2)

	applied, err := store.RestoreRetryProgress(t.Context(), job)
	if err != nil {
		t.Fatalf("RestoreRetryProgress: %v", err)
	}
	if applied {
		t.Error("reported an overlay applied when nothing was retained")
	}
}
