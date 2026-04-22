package app_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/app"
	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/nzb"
	"github.com/hobeone/sabnzbd-go/internal/queue"
)

// makeCheckpointApp builds a minimal Application whose queue has one
// article-bearing job. The checkpoint interval is set to interval so
// tests don't need to wait 30 s.
func makeCheckpointApp(t *testing.T, interval time.Duration) (*app.Application, *queue.Job, string) {
	t.Helper()
	downloadDir := t.TempDir()
	completeDir := t.TempDir()
	adminDir := t.TempDir()

	// Use empty mock so all articles return 430 — the job fails fast and
	// leaves a clean queue for checkpoint assertions.
	mock := startMockNNTP(t, map[string][]byte{})

	cfg := app.Config{
		DownloadDir:        downloadDir,
		CompleteDir:        completeDir,
		AdminDir:           adminDir,
		CacheLimit:         1 * 1024 * 1024,
		CheckpointInterval: interval,
		Servers: []config.ServerConfig{{
			Name:   "mock",
			Host:   mock.host,
			Port:   mock.port,
			Enable: true,
		}},
	}

	application, err := app.New(cfg, nil)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	parsed := &nzb.NZB{
		Files: []nzb.File{{
			Subject: "test.bin",
			Articles: []nzb.Article{
				{ID: "chk1@t", Bytes: 512, Number: 1},
				{ID: "chk2@t", Bytes: 512, Number: 2},
			},
			Bytes: 1024,
		}},
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "checkpoint-test"})
	if err != nil {
		t.Fatalf("queue.NewJob: %v", err)
	}

	return application, job, adminDir
}

// TestCheckpointFires_AfterMutation verifies that when the queue has unsaved
// mutations (article/file state changes), the periodic ticker saves within
// the configured interval.
func TestCheckpointFires_AfterMutation(t *testing.T) {
	const checkInterval = 20 * time.Millisecond
	application, job, adminDir := makeCheckpointApp(t, checkInterval)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer application.Shutdown() //nolint:errcheck

	// Add the job so the queue has something to save.
	if err := application.Queue().Add(job); err != nil {
		t.Fatalf("Queue.Add: %v", err)
	}

	// Dirty the queue manually (MarkArticleDone is one of the five mutators).
	if err := application.Queue().MarkArticleDone(job.ID, "chk1@t"); err != nil {
		t.Fatalf("MarkArticleDone: %v", err)
	}

	// The queue index file should appear within a few intervals.
	indexPath := filepath.Join(adminDir, "queue", "queue.json.gz")
	deadline := time.Now().Add(10 * checkInterval)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(indexPath); err == nil {
			// File exists — checkpoint fired.
			return
		}
		time.Sleep(checkInterval / 2)
	}
	t.Errorf("queue index file %s was not written within %v of a mutation", indexPath, deadline)
}

// TestCheckpointSkips_WhenClean verifies that when no mutations happen after
// a save the periodic ticker does not re-write the queue index.
func TestCheckpointSkips_WhenClean(t *testing.T) {
	const checkInterval = 20 * time.Millisecond
	application, job, adminDir := makeCheckpointApp(t, checkInterval)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer application.Shutdown() //nolint:errcheck

	// Add the job so there's something to save.
	if err := application.Queue().Add(job); err != nil {
		t.Fatalf("Queue.Add: %v", err)
	}

	// Dirty and force a save so we have a baseline mtime.
	if err := application.Queue().MarkArticleDone(job.ID, "chk1@t"); err != nil {
		t.Fatalf("MarkArticleDone: %v", err)
	}

	indexPath := filepath.Join(adminDir, "queue", "queue.json.gz")

	// Wait for the first checkpoint to land.
	deadline := time.Now().Add(10 * checkInterval)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(indexPath); err == nil {
			break
		}
		time.Sleep(checkInterval / 2)
	}
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("baseline checkpoint never wrote %s: %v", indexPath, err)
	}

	// Record the mtime after the baseline save. The queue is now clean.
	info1, err := os.Stat(indexPath)
	if err != nil {
		t.Fatalf("stat after baseline: %v", err)
	}
	mtime1 := info1.ModTime()

	// Wait several intervals without mutating; mtime must not change.
	time.Sleep(5 * checkInterval)

	info2, err := os.Stat(indexPath)
	if err != nil {
		t.Fatalf("stat after wait: %v", err)
	}
	if !info2.ModTime().Equal(mtime1) {
		t.Errorf("queue index was re-written on a clean queue: mtime changed from %v to %v",
			mtime1, info2.ModTime())
	}
}
