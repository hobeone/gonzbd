package queue

import (
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
)

func TestSQLiteStore_EncodeDecodeArticlesDone(t *testing.T) {
	parsed := &nzb.NZB{
		Files: []nzb.File{
			{
				Subject:  "test_file",
				Articles: []nzb.Article{{ID: "art1", Number: 1}, {ID: "art2", Number: 2}, {ID: "art3", Number: 3}},
				Bytes:    300,
			},
		},
	}
	jobArt, err := NewJob(parsed, AddOptions{Name: "job-arts"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	jobArt.Progress().markDone(mustManifest(t, jobArt), 1) // Mark article index 1 as done
	encoded := encodeArticlesDone(jobArt, 0)
	if encoded == "" {
		t.Fatal("expected non-empty encoded articles done string")
	}

	jobArt2, _ := NewJob(parsed, AddOptions{Name: "job-arts-2"}, fsutil.SanitizeOptions{})
	decodeTestStore(t).decodeArticlesDone(encoded, jobArt2, 0)
	if !jobArt2.Progress().ArticleDone(1) || jobArt2.Progress().ArticleDone(0) || jobArt2.Progress().ArticleDone(2) {
		t.Errorf("decodeArticlesDone failed to restore article bitmap: %v", jobArt2.progress.done)
	}

	// Test boundary and invalid input branches for 100% function coverage
	if encodeArticlesDone(nil, 0) != "" {
		t.Error("expected empty string for nil job in encodeArticlesDone")
	}
	if encodeArticlesDone(jobArt, 99) != "" {
		t.Error("expected empty string for out-of-bounds fileIdx in encodeArticlesDone")
	}
	decodeTestStore(t).decodeArticlesDone("invalid-hex-string!!!", jobArt2, 0)
	decodeTestStore(t).decodeArticlesDone("", jobArt2, 0)
	decodeTestStore(t).decodeArticlesDone("00", nil, 0)
	decodeTestStore(t).decodeArticlesDone("00", jobArt2, 99)
}

func TestSQLiteStore_UpdateTx(t *testing.T) {
	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	store := NewSQLiteStore(repo.DB(), dir, repo)

	ctx := t.Context()
	parsed := &nzb.NZB{
		Files: []nzb.File{
			{Subject: "file0.bin", Bytes: 100, Articles: []nzb.Article{{ID: "a0@t", Bytes: 100, Number: 1}}},
		},
	}
	job, err := NewJob(parsed, AddOptions{Name: "updatetx-job"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	job.Status = constants.StatusDownloading
	if err := store.Add(ctx, job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	job.Name = "updatetx-job-renamed"
	job.progress.files[0].Complete = true
	job.progress.files[0].BytesDownloaded = 100
	job.progress.files[0].WriteCursor = 100
	job.progress.files[0].Filename = "file0.bin"
	job.progress.files[0].AssembledCRC32 = 0xdeadbeef

	if err := store.updateTx(ctx, store.db, job); err != nil {
		t.Fatalf("updateTx: %v", err)
	}

	got, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "updatetx-job-renamed" {
		t.Errorf("job name not persisted by updateTx: got %q", got.Name)
	}
	if !got.Progress().FileComplete(0) {
		t.Error("file completion not persisted by updateTx")
	}
	if got.Progress().FileBytesDownloaded(0) != 100 || got.Progress().FileWriteCursor(0) != 100 {
		t.Errorf("file byte counters not persisted by updateTx: bytesDownloaded=%d writeCursor=%d",
			got.Progress().FileBytesDownloaded(0), got.Progress().FileWriteCursor(0))
	}

	// updateTx on a job that was never Add()ed affects zero rows in the jobs
	// UPDATE and must surface that as ErrNotFound rather than silently no-op.
	missing, err := NewJob(parsed, AddOptions{Name: "never-added"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob missing: %v", err)
	}
	if err := store.updateTx(ctx, store.db, missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for never-added job, got %v", err)
	}
}

func TestSQLiteStore_ResequenceTx(t *testing.T) {
	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	store := NewSQLiteStore(repo.DB(), dir, repo)

	ctx := t.Context()
	parsed := &nzb.NZB{
		Files: []nzb.File{
			{
				Subject:  "f1",
				Articles: []nzb.Article{{ID: "a1", Number: 1}},
				Bytes:    100,
			},
		},
	}
	j1, _ := NewJob(parsed, AddOptions{Name: "j1"}, fsutil.SanitizeOptions{})
	j2, _ := NewJob(parsed, AddOptions{Name: "j2"}, fsutil.SanitizeOptions{})
	if err := store.Add(ctx, j1); err != nil {
		t.Fatalf("Add 1: %v", err)
	}
	if err := store.Add(ctx, j2); err != nil {
		t.Fatalf("Add 2: %v", err)
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := store.resequenceTx(ctx, tx); err != nil {
		t.Fatalf("resequenceTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// decodeTestStore returns a store carrying only a logger. decodeArticlesDone
// reads no database state, so the rest of SQLiteStore is not needed to
// exercise it.
func decodeTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	return &SQLiteStore{log: slog.Default()}
}
