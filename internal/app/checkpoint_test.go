package app_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
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

	cfg := testConfig(
		downloadDir,
		completeDir,
		adminDir,
		config.ServerConfig{
			Name:   "mock",
			Host:   mock.host,
			Port:   mock.port,
			Enable: true,
		},
	)

	application, err := app.New(cfg, nil, app.WithCheckpointInterval(interval))
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
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "checkpoint-test"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("queue.NewJob: %v", err)
	}

	return application, job, adminDir
}

// TestCheckpointFires_AfterMutation verifies that when the queue has unsaved
// mutations (article/file state changes), the periodic ticker saves within
// the configured interval.
func TestCheckpointFires_AfterMutation(t *testing.T) {
	const checkInterval = 50 * time.Millisecond
	application, job, adminDir := makeCheckpointApp(t, checkInterval)

	ctx, cancel := context.WithCancel(t.Context())
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
	// 50ms: above typical OS mtime resolution and OS scheduling jitter under
	// -race, reducing false flakes from async queue mutations racing the
	// stabilization window.
	const checkInterval = 50 * time.Millisecond
	application, job, adminDir := makeCheckpointApp(t, checkInterval)

	ctx, cancel := context.WithCancel(t.Context())
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

	// Wait for the mtime to stabilize. The download pipeline runs
	// asynchronously and may mark articles as failed (dirtying the queue)
	// after our manual MarkArticleDone. We wait until the mtime hasn't
	// changed for several checkpoint intervals, confirming all async
	// mutations have been checkpointed and the queue is truly clean.
	var stableMtime time.Time
	stabilizeDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(stabilizeDeadline) {
		info, err := os.Stat(indexPath)
		if err != nil {
			t.Fatalf("stat during stabilize: %v", err)
		}
		if info.ModTime().Equal(stableMtime) {
			// mtime unchanged — might be stable. Check again after
			// several intervals.
			time.Sleep(5 * checkInterval)
			info2, err := os.Stat(indexPath)
			if err != nil {
				t.Fatalf("stat after stabilize wait: %v", err)
			}
			if info2.ModTime().Equal(stableMtime) {
				break // Stable!
			}
		}
		stableMtime = info.ModTime()
		time.Sleep(checkInterval)
	}
	if stableMtime.IsZero() {
		t.Fatal("mtime never stabilized")
	}

	// Now that the queue is truly clean (no mtime change for 5+ intervals),
	// wait several more intervals and verify it stays unchanged.
	// Wait 10 intervals: the ticker fires every checkInterval; giving it
	// 10 cycles ensures any late background mutation is checkpointed before
	// we assert the mtime is unchanged.
	time.Sleep(10 * checkInterval)

	info2, err := os.Stat(indexPath)
	if err != nil {
		t.Fatalf("stat after clean wait: %v", err)
	}
	if !info2.ModTime().Equal(stableMtime) {
		t.Errorf("queue index was re-written on a clean queue: mtime changed from %v to %v",
			stableMtime, info2.ModTime())
	}
}
