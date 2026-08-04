package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/queue"
)

// TestFinalize_KeepsJobInQueueWhenHistoryWriteFails pins the recovery
// behaviour of the queue→history transition: if the history write fails, the
// job stays in the queue so the next attempt can retry it.
//
// Dropping it instead would lose the job entirely — it is gone from the queue
// and never arrived in history. persistAndCommit's error path exists for
// exactly this, and nothing exercised it: finalize is only ever reached
// through the live pipeline, where the write does not fail.
//
// A duplicate nzo_id is the cheapest way to make MoveToHistory fail for real
// rather than through an injected fake; history.nzo_id carries a UNIQUE index
// (migration 001), and a finalize re-run after a crash between commit and
// Queue.Remove is how it happens in practice.
func TestFinalize_KeepsJobInQueueWhenHistoryWriteFails(t *testing.T) {
	adminDir := t.TempDir()
	cfg := testConfigInternal(t, adminDir)

	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	application, err := New(cfg, repo)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = application.Shutdown() })
	application.PauseDownloads()
	application.Queue().PauseAll()

	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "file.bin",
		Bytes:    1024,
		Articles: []nzb.Article{{ID: "fin1@t", Bytes: 1024, Number: 1}},
	}}}
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: "finalize.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	job.ID = "finalizeconflict"
	if err := application.Queue().Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Occupy the nzo_id so the history insert inside MoveToHistory violates
	// its unique index.
	if err := repo.Add(ctx, history.Entry{
		NzoID:     job.ID,
		Name:      "already-there",
		Status:    string(constants.StatusCompleted),
		Completed: time.Now(),
	}); err != nil {
		t.Fatalf("seed conflicting entry: %v", err)
	}

	newJobFinalizer(application).finalize(&postproc.Job{
		Queue:       application.Queue().SnapshotJob(job.ID),
		FinalDir:    t.TempDir(),
		DownloadDir: t.TempDir(),
	})

	if application.Queue().SnapshotJob(job.ID) == nil {
		t.Error("job was removed from the queue although its history write failed")
	}
	entry, err := repo.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if entry.Name != "already-there" {
		t.Errorf("the seeded entry was overwritten: name = %q", entry.Name)
	}
}

// testConfigInternal builds a minimal config for package-internal tests.
func testConfigInternal(t *testing.T, adminDir string) *config.Config {
	t.Helper()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.General.AdminDir = adminDir
	cfg.General.DownloadDir = t.TempDir()
	cfg.General.CompleteDir = t.TempDir()
	cfg.Servers = []config.ServerConfig{{
		Name: "mock", Host: "127.0.0.1", Port: 1119, Enable: false, Connections: 1,
	}}
	return cfg
}
