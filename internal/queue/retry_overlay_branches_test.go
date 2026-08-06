package queue

import (
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
)

// TestRetainedMatchesManifest_RejectsOutOfOrderIndexes pins the file-index
// check independently of the article-count one.
//
// RestoreRetryProgress indexes job.progress.files by RetainedFile.FileIndex,
// so a row claiming an index other than its own position would write one
// file's progress onto another — or panic out of range. HistoryFileProgress
// orders by file_index, so this only arises when rows are missing or
// duplicated, which is exactly when guessing is worst.
func TestRetainedMatchesManifest_RejectsOutOfOrderIndexes(t *testing.T) {
	j := makeMultiFileJob(t, "guard", 2, 3)
	m := j.manifest

	if !retainedMatchesManifest([]RetainedFile{
		{FileIndex: 0, ArticleCount: 3},
		{FileIndex: 1, ArticleCount: 3},
	}, m) {
		t.Error("well-formed rows were rejected")
	}
	// Right count of rows, right article counts, wrong positions.
	if retainedMatchesManifest([]RetainedFile{
		{FileIndex: 1, ArticleCount: 3},
		{FileIndex: 0, ArticleCount: 3},
	}, m) {
		t.Error("rows whose FileIndex does not match their position were accepted")
	}
	// A gap: file 1 never got a row, so file 2's landed at position 1.
	if retainedMatchesManifest([]RetainedFile{
		{FileIndex: 0, ArticleCount: 3},
		{FileIndex: 2, ArticleCount: 3},
	}, m) {
		t.Error("rows with a gap in file_index were accepted")
	}
	if retainedMatchesManifest([]RetainedFile{{FileIndex: 0, ArticleCount: 3}}, m) {
		t.Error("too few rows were accepted")
	}
}

// TestRestoreRetryProgress_NoProgressOrManifest pins that a job missing
// either residency tier is reported as not-applied rather than panicking.
// RestoreRetryProgress runs on a job built outside the queue, so it cannot
// rely on Add's residency repair having happened yet.
func TestRestoreRetryProgress_NoProgressOrManifest(t *testing.T) {
	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	store := NewSQLiteStore(repo.DB(), dir, repo)

	if applied, err := store.RestoreRetryProgress(t.Context(), nil); applied || err != nil {
		t.Errorf("nil job: applied=%v err=%v, want false/nil", applied, err)
	}
	if applied, err := store.RestoreRetryProgress(t.Context(), &Job{ID: "bare000000000001"}); applied || err != nil {
		t.Errorf("bare job: applied=%v err=%v, want false/nil", applied, err)
	}
}

// TestRestoreRetryProgress_RestoresCompleteAndDeferredFiles pins the two
// per-file flags the overlay carries besides the article bitmap.
//
// Complete short-circuits to marking every article of the file done, and
// Deferred marks a par2 recovery volume the job chose not to download. A
// retry that lost Deferred would fetch volumes on-demand par2 exists to
// avoid; one that lost Complete would refetch a file already assembled.
func TestRestoreRetryProgress_RestoresCompleteAndDeferredFiles(t *testing.T) {
	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	store := NewSQLiteStore(repo.DB(), dir, repo)
	q := New(WithStore(store))

	job := makeMultiFileJob(t, "flags", 2, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q.PromoteNext(t.Context())
	if err := q.MarkArticlesDone(job.ID, []string{articleID(0, 0), articleID(0, 1)}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}
	if err := q.MarkFileComplete(job.ID, 0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}
	job.progress.files[1].Fetch = FetchIfNeeded
	job.progress.files[0].Filename = "assembled.bin"
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.MoveToHistory(t.Context(), job, history.Entry{
		NzoID: job.ID, Name: job.Name, Status: string(constants.StatusFailed),
	}); err != nil {
		t.Fatalf("MoveToHistory: %v", err)
	}

	rebuilt := makeMultiFileJob(t, "flags", 2, 2)
	rebuilt.ID = job.ID
	applied, err := store.RestoreRetryProgress(t.Context(), rebuilt)
	if err != nil {
		t.Fatalf("RestoreRetryProgress: %v", err)
	}
	if !applied {
		t.Fatal("overlay refused for a matching manifest")
	}
	p := rebuilt.Progress()
	if !p.FileComplete(0) {
		t.Error("file 0 lost its Complete flag; the retry will reassemble it")
	}
	if !p.ArticleDone(0) || !p.ArticleDone(1) {
		t.Error("a complete file's articles should all come back done")
	}
	if p.FileFetchPolicy(1) != FetchIfNeeded {
		t.Error("file 1 lost its Deferred flag; on-demand par2 would fetch it anyway")
	}
	if p.FileFilename(0) != "assembled.bin" {
		t.Errorf("FileFilename(0) = %q, want the resolved name carried over", p.FileFilename(0))
	}
}

// TestRestoreRetryProgress_QueryFailurePropagates pins that a broken read is
// an error rather than a silent "nothing retained". The two are opposite
// instructions: one says re-download the job, the other says the database is
// unusable, and conflating them would quietly refetch everything.
func TestRestoreRetryProgress_QueryFailurePropagates(t *testing.T) {
	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	repo := history.NewRepository(db)
	store := NewSQLiteStore(repo.DB(), dir, repo)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	job := makeMultiFileJob(t, "broken", 1, 2)
	applied, err := store.RestoreRetryProgress(t.Context(), job)
	if err == nil {
		t.Fatal("a failed query was reported as success")
	}
	if applied {
		t.Error("applied is true despite the error")
	}
}
