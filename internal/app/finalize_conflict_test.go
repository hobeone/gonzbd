package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
)

// TestFinalize_RemovesJobFromQueueWhenHistoryWriteFails pins that when the
// history write fails during finalization, the job is still removed from the
// dispatcher and queue teardown completes so that the job does not leak
// permanently into the active registry.
func TestFinalize_RemovesJobFromQueueWhenHistoryWriteFails(t *testing.T) {
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
	application.Dispatcher().Pause()

	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "file.bin",
		Bytes:    1024,
		Articles: []nzb.Article{{ID: "fin1@t", Bytes: 1024, Number: 1}},
	}}}
	job, hdr, err := BuildIngestJob(application.config, parsed, "finalize.nzb", types.FetchOptions{JobID: "finalizeconflict"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.Dispatcher().Add(job, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Occupy the nzo_id so the history insert inside MoveToHistory violates
	// its unique index.
	if err := repo.Add(ctx, history.Entry{
		NzoID:     job.ID(),
		Name:      "already-there",
		Status:    string(constants.StatusCompleted),
		Completed: time.Now(),
	}); err != nil {
		t.Fatalf("seed conflicting entry: %v", err)
	}

	newJobFinalizer(application).finalize(&postproc.Job{
		Job:         job,
		FinalDir:    t.TempDir(),
		DownloadDir: t.TempDir(),
	})

	if _, ok := application.Dispatcher().Job(job.ID()); ok {
		t.Error("job remained in the queue although its history write failed; expected teardown to complete")
	}
	entry, err := repo.Get(ctx, job.ID())
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
