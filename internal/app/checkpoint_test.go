package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// makeCheckpointApp builds a minimal Application whose queue has one
// article-bearing job. The checkpoint interval is set to interval so
// tests don't need to wait 30 s.
func makeCheckpointApp(t *testing.T, interval time.Duration) (*app.Application, *queue.Job, *history.Repository) {
	t.Helper()
	// Store-backed, matching production: app.New only builds a queue Store
	// when it is given a live repo, and the store-less path it used to take
	// ran the whole-queue JSON engine removed in #266.
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)

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

	application, err := app.New(cfg, repo, app.WithCheckpointInterval(interval))
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

	return application, job, repo
}

// TestCheckpointFires_AfterMutation verifies that when the queue has unsaved
// mutations (article/file state changes), the periodic ticker saves within
// the configured interval.
func TestCheckpointFires_AfterMutation(t *testing.T) {
	const checkInterval = 50 * time.Millisecond
	application, job, _ := makeCheckpointApp(t, checkInterval)

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

	// Dirty the queue. RecordDownload is progress-tier, so it works whether
	// or not the job happens to have been promoted to resident yet —
	// MarkArticleDone would race PromoteNext and fail with ErrJobNotResident.
	if err := application.Queue().RecordDownload(job.ID, "mock", 512); err != nil {
		t.Fatalf("RecordDownload: %v", err)
	}

	// Watch the dirty flag rather than the filesystem. runCheckpoint saves
	// only when the queue is dirty, and Save clears the flag, so a
	// dirty→clean transition is the checkpoint firing — exactly, with no
	// dependence on mtime resolution or on which file the store happens to
	// write. The queue index this used to stat was the JSON engine's, removed
	// in #266.
	deadline := time.Now().Add(20 * checkInterval)
	for time.Now().Before(deadline) {
		if !application.Queue().IsDirty() {
			return // checkpoint fired and the save landed
		}
		time.Sleep(checkInterval / 2)
	}
	t.Errorf("the queue was still dirty %v after a mutation; no checkpoint ran", 20*checkInterval)
}

// TestCheckpointSkips_WhenClean verifies that when no mutations happen after
// a save the periodic ticker does not re-write the queue index.
func TestCheckpointSkips_WhenClean(t *testing.T) {
	const checkInterval = 50 * time.Millisecond
	application, job, repo := makeCheckpointApp(t, checkInterval)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer application.Shutdown() //nolint:errcheck

	// Pause before adding: makeCheckpointApp's mock 430s every article, so an
	// unpaused job fails and leaves the queue for history — taking with it
	// the row this test needs to watch.
	application.Queue().PauseAll()
	if err := application.Queue().Add(job); err != nil {
		t.Fatalf("Queue.Add: %v", err)
	}
	// RecordDownload rather than MarkArticleDone: a paused job is
	// non-resident, and the manifest-tier mutators correctly refuse it
	// (#261). Per-server byte counts live in progress, which is always
	// resident, so this dirties the queue in any residency state.
	if err := application.Queue().RecordDownload(job.ID, "mock", 512); err != nil {
		t.Fatalf("RecordDownload: %v", err)
	}

	// Wait for quiescence. The download pipeline is still failing articles
	// in the background, each of which legitimately dirties the queue, so
	// "clean" means clean and staying clean — five consecutive observations.
	// This replaces a stabilization loop over the index file's mtime; the
	// index belonged to the JSON engine removed in #266, and the dirty flag
	// is the exact signal the checkpoint itself reads.
	quiesceDeadline := time.Now().Add(5 * time.Second)
	clean := 0
	for time.Now().Before(quiesceDeadline) && clean < 5 {
		if application.Queue().IsDirty() {
			clean = 0
		} else {
			clean++
		}
		time.Sleep(checkInterval)
	}
	if clean < 5 {
		t.Fatal("the queue never went quiet, so a skipped checkpoint cannot be distinguished from a busy one")
	}

	// Plant a value only a checkpoint would overwrite. Save's UpdateBatch
	// rewrites every job row, so if the ticker saves while the queue is
	// clean the sentinel disappears — an exact write-detector, where the old
	// test could only compare mtimes and hope.
	const sentinel = "sentinel-must-survive-a-clean-tick"
	if _, err := repo.DB().ExecContext(t.Context(),
		`UPDATE jobs SET name = ? WHERE id = ?`, sentinel, job.ID); err != nil {
		t.Fatalf("plant sentinel: %v", err)
	}

	time.Sleep(10 * checkInterval)

	var got string
	if err := repo.DB().QueryRowContext(t.Context(),
		`SELECT name FROM jobs WHERE id = ?`, job.ID).Scan(&got); err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if got != sentinel {
		t.Errorf("the job row was rewritten on a clean queue (name = %q, want the sentinel); runCheckpoint is saving every tick instead of only when dirty", got)
	}
}
