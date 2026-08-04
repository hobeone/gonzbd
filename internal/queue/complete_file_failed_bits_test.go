package queue

import (
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
)

// completeFileWithFailedArticle builds a store-backed queue holding one
// 1-file, 3-article job in which article 1 failed and the file was then
// marked complete, and checkpoints it.
//
// That combination is the normal outcome of a partial download, not an edge
// case: internal/app/pipeline.go hands a permanently failed article to the
// assembler ("article permanently failed, handing to assembler"), which
// finishes the file with a gap in it and emits FileComplete. So any job with
// a missing article ends up with Complete set and a failed bit set on the
// same file.
func completeFileWithFailedArticle(t *testing.T) (*SQLiteStore, *history.Repository, *Job, string) {
	t.Helper()

	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	repo := history.NewRepository(db)
	store := NewSQLiteStore(repo.DB(), dir, repo)
	q := New(WithStore(store), WithStateDir(dir))

	job := makeMultiFileJob(t, "gapfile", 1, 3)
	job.Status = constants.StatusDownloading
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := q.MarkArticlesDone(job.ID, []string{articleID(0, 0), articleID(0, 2)}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}
	if _, err := q.MarkArticlesFailed(job.ID, []string{articleID(0, 1)}); err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
	}
	if err := q.MarkFileComplete(job.ID, 0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return store, repo, job, dir
}

// TestRestoreJobProgress_KeepsFailedBitsOnCompleteFile pins that a restart
// does not silently convert a failed article into a successful one.
//
// The complete-file branch used to mark every article of the file done and
// skip the articles_done bitmap entirely, so the failed bits were dropped on
// load. JobProgress.recompute derives failedBytes from that bitmap, which
// meant a restarted job reported FailedBytes() == 0 — and buildHistoryEntry
// feeds that to downloadCompleteness, recording a job with missing articles
// in history at 100% health.
func TestRestoreJobProgress_KeepsFailedBitsOnCompleteFile(t *testing.T) {
	store, _, job, dir := completeFileWithFailedArticle(t)

	wantFailedBytes := job.Progress().FailedBytes()
	if wantFailedBytes == 0 {
		t.Fatal("setup produced no failed bytes; the test would prove nothing")
	}

	reloaded, err := Load(dir, WithStore(store))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := reloaded.SnapshotJob(job.ID)
	if got == nil {
		t.Fatal("job missing after reload")
	}
	p := got.Progress()

	if !p.ArticleFailed(1) {
		t.Error("article 1 lost its failed bit across a restart")
	}
	if !p.ArticleDone(0) || !p.ArticleDone(2) {
		t.Error("the successful articles should still be done")
	}
	if !p.FileComplete(0) {
		t.Error("file 0 lost its Complete flag")
	}
	if p.FailedBytes() != wantFailedBytes {
		t.Errorf("FailedBytes = %d, want %d; history would record this job at the wrong health",
			p.FailedBytes(), wantFailedBytes)
	}
}

// TestRestoreRetryProgress_KeepsFailedBitsOnCompleteFile pins the same
// property on the retry path, where losing it is worse than a wrong number.
//
// ResetForRetry clears exactly those articles whose failed bit is set. If the
// overlay restores a complete file as all-done-none-failed, the reset finds
// nothing to do, the rebuilt job looks fully downloaded, and RetryHistoryJob
// sends it straight back to post-processing — which fails again for the same
// reason. Retry appears to run and changes nothing.
func TestRestoreRetryProgress_KeepsFailedBitsOnCompleteFile(t *testing.T) {
	store, _, job, _ := completeFileWithFailedArticle(t)

	if err := store.MoveToHistory(t.Context(), job, history.Entry{
		NzoID: job.ID, Name: job.Name, Status: string(constants.StatusFailed),
	}); err != nil {
		t.Fatalf("MoveToHistory: %v", err)
	}

	rebuilt := makeMultiFileJob(t, "gapfile", 1, 3)
	rebuilt.ID = job.ID
	applied, err := store.RestoreRetryProgress(t.Context(), rebuilt)
	if err != nil {
		t.Fatalf("RestoreRetryProgress: %v", err)
	}
	if !applied {
		t.Fatal("overlay refused for a manifest that matches")
	}

	if !rebuilt.Progress().ArticleFailed(1) {
		t.Fatal("article 1 came back unfailed, so ResetForRetry will not refetch it")
	}

	rebuilt.ResetForRetry()
	p := rebuilt.Progress()
	if p.ArticleDone(1) || p.ArticleFailed(1) {
		t.Errorf("after ResetForRetry article 1 is done=%v failed=%v, want neither — it must be refetched",
			p.ArticleDone(1), p.ArticleFailed(1))
	}
	if !p.ArticleDone(0) || !p.ArticleDone(2) {
		t.Error("the successful articles were reset too; the retry will refetch data it already has")
	}
	if p.FileComplete(0) {
		t.Error("file 0 is still Complete after a reset that cleared one of its articles")
	}
}
