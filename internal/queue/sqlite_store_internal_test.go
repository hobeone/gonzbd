package queue

import (
	"path/filepath"
	"testing"

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
	jobArt.Progress().markDone(jobArt.Manifest(), 1) // Mark article index 1 as done
	encoded := encodeArticlesDone(jobArt, 0)
	if encoded == "" {
		t.Fatal("expected non-empty encoded articles done string")
	}

	jobArt2, _ := NewJob(parsed, AddOptions{Name: "job-arts-2"}, fsutil.SanitizeOptions{})
	decodeArticlesDone(encoded, jobArt2, 0)
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
	decodeArticlesDone("invalid-hex-string!!!", jobArt2, 0)
	decodeArticlesDone("", jobArt2, 0)
	decodeArticlesDone("00", nil, 0)
	decodeArticlesDone("00", jobArt2, 99)
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
