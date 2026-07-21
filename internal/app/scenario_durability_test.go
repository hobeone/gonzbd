package app_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/queue"
)

// TestDurability_DoneMeansOnDisk verifies that an article flagged Done in
// the queue is actually on disk — i.e. the Done flag is set from the
// assembler write path, not from the downloader's NNTP-receive path.
func TestDurability_DoneMeansOnDisk(t *testing.T) {
	adminDir := t.TempDir()
	downloadDir := t.TempDir()
	completeDir := t.TempDir()

	server := nntptest.New(t)
	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("open history db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	cfg := testConfig(
		downloadDir,
		completeDir,
		adminDir,
		server.ServerConfig("durability", 1),
	)

	enteredHook := make(chan struct{}, 1)
	pauseCh := make(chan struct{})

	var rawApp *app.Application

	hook := func(jobID string, messageIDs []string) error {
		select {
		case enteredHook <- struct{}{}:
		default:
		}
		<-pauseCh
		return rawApp.Queue().MarkArticlesDone(jobID, messageIDs)
	}

	a, err := app.New(cfg, repo,
		app.WithPostProcStages([]postproc.Stage{noOpStage{}}),
		app.WithMarkArticlesDoneHook(hook),
	)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	rawApp = a

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancel()
		_ = a.Shutdown()
	})
	if err := a.Start(ctx); err != nil {
		t.Fatalf("app.Start: %v", err)
	}

	msgID := randomMsgID(t)
	rawPayload := []byte("durability test article payload - verifying bytes on stable storage")
	server.AddArticle(msgID, yencSinglePart("durable.bin", rawPayload))

	parsed := &nzb.NZB{
		Files: []nzb.File{{
			Subject:  `"durable.bin" yEnc (1/1)`,
			Articles: []nzb.Article{{ID: msgID, Bytes: len(rawPayload), Number: 1}},
			Bytes:    int64(len(rawPayload)),
		}},
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "durability-job"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := a.Queue().Add(job); err != nil {
		t.Fatalf("Queue.Add: %v", err)
	}

	// 1. Wait until assembler worker attempts to flush MarkArticlesDone
	select {
	case <-enteredHook:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for assembler to reach MarkArticlesDone hook")
	}

	// 2. While paused inside the assembler hook (before MarkArticlesDone runs):
	// Assert that article in active queue is NOT marked Done yet under B.6
	snap := a.Queue().SnapshotJob(job.ID)
	if snap == nil {
		t.Fatalf("job missing from queue")
	}
	if len(snap.Files) == 0 || len(snap.Files[0].Articles) == 0 {
		t.Fatalf("job files/articles empty")
	}

	artDoneBeforeHookFinish := snap.Files[0].Articles[0].Done
	if artDoneBeforeHookFinish {
		t.Errorf("FAIL: article is marked Done BEFORE MarkArticlesDone completed (downloader called MarkArticleDone prematurely!)")
	}

	// Save queue to disk to simulate on-disk queue state right before crash/flush
	if err := a.Queue().Save(filepath.Join(adminDir, "queue")); err != nil {
		t.Fatalf("Queue.Save: %v", err)
	}

	// Reload queue from disk into a fresh queue instance
	reloadedQ, err := queue.Load(filepath.Join(adminDir, "queue"))
	if err != nil {
		t.Fatalf("queue.Load: %v", err)
	}
	reloadedSnap := reloadedQ.SnapshotJob(job.ID)
	if reloadedSnap == nil {
		t.Fatalf("reloaded job missing")
	}
	if reloadedSnap.Files[0].Articles[0].Done {
		t.Errorf("FAIL: on-disk queue has article marked Done before assembler completed MarkArticlesDone!")
	}

	// 3. Unblock the assembler hook
	close(pauseCh)

	// Wait for job completion (reaches history)
	if !waitUntil(5*time.Second, func() bool {
		_, err := repo.Get(t.Context(), job.ID)
		return err == nil
	}) {
		t.Fatalf("timeout waiting for job to complete and reach history after unblocking hook")
	}

	// Verify file on disk using independent oracle
	filePath := filepath.Join(downloadDir, "durability-job", snap.Files[0].Filename)
	diskBytes, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("ReadFile on target file failed: %v", err)
	}
	if !bytes.Equal(diskBytes, rawPayload) {
		t.Errorf("content mismatch: got %q, want %q", diskBytes, rawPayload)
	}
}
