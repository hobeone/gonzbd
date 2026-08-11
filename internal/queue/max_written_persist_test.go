package queue

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
)

// The assembler seeds a resumed file's decoded high-water mark from what was
// persisted, and truncates the completed file to it. If the figure does not
// survive a restart the mark starts at zero, and the truncate cuts the file
// down to whatever the new run happened to receive (#342).
//
// These tests run against the real SQLite store rather than a fake queue on
// purpose: what they certify is that migration 011 applies and that every
// query carrying per-file state carries the new column. A fake would pass
// while proving none of that.

// TestMaxWrittenSurvivesReload pins the job_files round trip.
func TestMaxWrittenSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	repo := history.NewRepository(db)
	store := NewSQLiteStore(repo.DB(), dir, repo)
	q := New(WithStore(store))

	job := makeMultiFileJob(t, "extents", 2, 3)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q.PromoteNext(context.Background())

	const (
		wantCursor = int64(4096)
		wantMax    = int64(9000) // above the cursor: a tail arrived out of order
	)
	if err := q.SetFileExtents(job.ID, 0, wantCursor, wantMax); err != nil {
		t.Fatalf("SetFileExtents: %v", err)
	}
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := Load(dir, WithStore(store))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	snap, err := reloaded.Get(job.ID)
	if err != nil {
		t.Fatalf("the job did not survive the reload: %v", err)
	}
	if got := snap.Progress().FileMaxWritten(0); got != wantMax {
		t.Errorf("FileMaxWritten after reload = %d, want %d — a resumed run would "+
			"rebuild the mark from zero and truncate the file to this run's extent",
			got, wantMax)
	}
	if got := snap.Progress().FileWriteCursor(0); got != wantCursor {
		t.Errorf("FileWriteCursor after reload = %d, want %d", got, wantCursor)
	}
}

// TestMaxWrittenSurvivesRetry pins the history_job_files round trip.
//
// A failed job's per-file progress is retained in a separate table so a retry
// resumes instead of refetching. A column present in job_files but absent
// there is read and silently dropped — on the one path where resume state
// matters most, since the file is definitely partial.
func TestMaxWrittenSurvivesRetry(t *testing.T) {
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
	if err := q.MarkArticlesDone(job.ID, []string{articleID(0, 0)}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}

	const wantMax = int64(9000)
	if err := q.SetFileExtents(job.ID, 0, 4096, wantMax); err != nil {
		t.Fatalf("SetFileExtents: %v", err)
	}
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	entry := history.Entry{NzoID: job.ID, Name: job.Name, Status: string(constants.StatusFailed)}
	if err := store.MoveToHistory(t.Context(), job, entry); err != nil {
		t.Fatalf("MoveToHistory: %v", err)
	}

	rebuilt := makeMultiFileJob(t, "retry", 2, 3)
	rebuilt.ID = job.ID
	applied, err := store.RestoreRetryProgress(t.Context(), rebuilt)
	if err != nil {
		t.Fatalf("RestoreRetryProgress: %v", err)
	}
	if !applied {
		t.Fatal("overlay refused for a manifest that matches the retained shape")
	}

	if got := rebuilt.Progress().FileMaxWritten(0); got != wantMax {
		t.Errorf("FileMaxWritten after retry = %d, want %d — the retained figure "+
			"was dropped, so the retry truncates away bytes the first run wrote",
			got, wantMax)
	}
}

// TestFileMaxWrittenOutOfRange pins the accessor's guards.
//
// It is read from the pipeline while building FileInfo for a file the queue may
// no longer hold, so an out-of-range index must return zero rather than panic
// on the assembler's single worker goroutine. Zero is the safe answer: it means
// "no persisted mark", which degrades to seeding from the write cursor alone.
func TestFileMaxWrittenOutOfRange(t *testing.T) {
	var nilProgress *JobProgress
	if got := nilProgress.FileMaxWritten(0); got != 0 {
		t.Errorf("nil JobProgress returned %d, want 0", got)
	}

	q := New()
	job := makeMultiFileJob(t, "range", 1, 3)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	p := q.SnapshotJob(job.ID).Progress()

	for _, fi := range []int{-1, 99} {
		if got := p.FileMaxWritten(fi); got != 0 {
			t.Errorf("FileMaxWritten(%d) = %d, want 0", fi, got)
		}
	}
}

// TestSetFileExtentsOnlyAdvances pins that neither figure walks backwards.
//
// The assembler reports from its own in-memory state, seeded from these values
// on resume. A job requeued without its retained progress starts from zero
// while the on-disk file may still hold more, so a stale report must not lower
// what is stored — the truncate would then cut below the file's real extent.
func TestSetFileExtentsOnlyAdvances(t *testing.T) {
	q := New()
	job := makeMultiFileJob(t, "advance", 1, 3)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q.PromoteNext(context.Background())

	if err := q.SetFileExtents(job.ID, 0, 4096, 9000); err != nil {
		t.Fatalf("SetFileExtents: %v", err)
	}
	if err := q.SetFileExtents(job.ID, 0, 10, 20); err != nil {
		t.Fatalf("SetFileExtents (regressing): %v", err)
	}

	p := q.SnapshotJob(job.ID).Progress()
	if got := p.FileMaxWritten(0); got != 9000 {
		t.Errorf("FileMaxWritten = %d after a lower report, want 9000", got)
	}
	if got := p.FileWriteCursor(0); got != 4096 {
		t.Errorf("FileWriteCursor = %d after a lower report, want 4096", got)
	}
}
