package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// TestStart_PreservesPersistedFailedArticlesAcrossStartup pins that Application.Start
// does not wipe out failed article state or refund failedBytes on registered jobs.
//
// Previously (#523), Application.Start executed a reload-shaped ClearEmittedForReload
// sweep over all registered jobs. For any resident job, that call invoked resetForReload,
// clearing the failed bit and refunding failedBytes for failed articles in incomplete files,
// undoing the failed_articles state restored from persistence and racing hydration.
func TestStart_PreservesPersistedFailedArticlesAcrossStartup(t *testing.T) {
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

	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject: "file.bin",
		Bytes:   1024,
		Articles: []nzb.Article{
			{ID: "art1@t", Bytes: 512, Number: 1},
			{ID: "art2@t", Bytes: 512, Number: 2},
		},
	}}}
	j, hdr, err := BuildIngestJob(application.config, parsed, "test.nzb", types.FetchOptions{JobID: "job1", NzbName: "test"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.Dispatcher().Add(context.Background(), j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Mark article 0 as failed so the job carries a failed article into startup.
	if err := j.MarkArticleFailed(0); err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}
	if !j.Progress().ArticleFailed(0) {
		t.Fatal("setup failed: article 0 is not marked failed")
	}
	if got := j.Progress().FailedBytes(); got != 512 {
		t.Fatalf("setup failed: FailedBytes = %d, want 512", got)
	}

	application.PauseDownloads()
	application.Dispatcher().Pause()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = application.Shutdown() })

	// The startup sequence must not un-fail articles or clear failed bits.
	if !j.Progress().ArticleFailed(0) {
		t.Errorf("article 0 failed bit was cleared across Start; ClearEmittedForReload wiped out the failure")
	}
	if got := j.Progress().FailedBytes(); got != 512 {
		t.Errorf("FailedBytes = %d across Start, want 512", got)
	}
}
