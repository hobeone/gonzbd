package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// TestStart_RefusesAJobCarryingProgressBeforeTheRestorePass pins that Start
// fails, naming the job, when a registered job already holds a file table
// before the restore pass (Application.restoreProgress), and leaves that
// job's progress as it found it.
//
// This replaces the #523 pin, which registered a job carrying a failed
// article before Start and checked that Start did not clear it: Start then ran
// a reload-shaped ClearEmittedForReload sweep over every registered job. That
// fixture is now refused here, and a restored job reaches Start with no
// progress at all, the restore pass seeding no article bit, so no failed bit
// exists for such a sweep to clear before the job's first hydration.
func TestStart_RefusesAJobCarryingProgressBeforeTheRestorePass(t *testing.T) {
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
	restoreQueue(t, application)

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
	if err := j.MarkArticleFailed(0, job.WholeArticle); err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}

	application.PauseDownloads()
	application.Dispatcher().Pause()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err = application.Start(ctx)
	if err == nil {
		t.Cleanup(func() { _ = application.Shutdown() })
		t.Fatal("Start accepted a job that held a file table before the restore pass")
	}
	if !strings.Contains(err.Error(), "job1 already has a file table") {
		t.Errorf("Start = %v, want the restore pass's refusal naming job1", err)
	}
	if !j.Progress().ArticleFailed(0) || j.Progress().FailedBytes() != 512 {
		t.Error("a refused Start changed the job's failed article")
	}
}
