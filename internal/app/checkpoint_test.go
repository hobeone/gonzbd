package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// makeCheckpointApp builds a minimal Application whose queue has one
// article-bearing job. The checkpoint interval is set to interval so
// tests don't need to wait 30 s.
func makeCheckpointApp(t *testing.T, interval time.Duration) (*app.Application, *job.Job, dispatch.Header, *history.Repository) {
	t.Helper()
	// Store-backed, matching production: app.New only builds a queue Store
	// when it is given a live repo.
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
	j, hdr := buildTestJob(t, cfg, parsed, types.FetchOptions{NzbName: "checkpoint-test"})

	return application, j, hdr, repo
}

// TestCheckpointFires_AfterMutation verifies that when the checkpointer has unsaved
// mutations, the periodic ticker saves within the configured interval.
func TestCheckpointFires_AfterMutation(t *testing.T) {
	t.Parallel()
	const checkInterval = 10 * time.Millisecond
	application, j, hdr, _ := makeCheckpointApp(t, checkInterval)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer application.Shutdown() //nolint:errcheck

	// Add the job so the dispatcher has something to track.
	if err := application.Dispatcher().Add(j, hdr); err != nil {
		t.Fatalf("Dispatcher.Add: %v", err)
	}

	// Dirty the checkpointer.
	application.Checkpointer().Mark(j)

	// Watch the dirty flag on checkpointer.
	deadline := time.Now().Add(20 * checkInterval)
	for time.Now().Before(deadline) {
		if application.Checkpointer().DirtyCount() == 0 {
			return // checkpoint fired and the save landed
		}
		time.Sleep(checkInterval / 2)
	}
	t.Errorf("the checkpointer was still dirty %v after a mutation; no checkpoint ran", 20*checkInterval)
}

// TestCheckpointSkips_WhenClean verifies that when no mutations happen after
// a save the periodic ticker does not re-write the job row.
func TestCheckpointSkips_WhenClean(t *testing.T) {
	t.Parallel()
	const checkInterval = 10 * time.Millisecond
	application, j, hdr, repo := makeCheckpointApp(t, checkInterval)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer application.Shutdown() //nolint:errcheck

	// Pause before adding: mock 430s every article, so an unpaused job fails
	// and leaves the queue for history — taking with it the row this test needs to watch.
	application.Dispatcher().Pause()
	if err := application.Dispatcher().Add(j, hdr); err != nil {
		t.Fatalf("Dispatcher.Add: %v", err)
	}
	application.Checkpointer().Mark(j)

	// Wait for quiescence and initial row persistence.
	quiesceDeadline := time.Now().Add(5 * time.Second)
	clean := 0
	for time.Now().Before(quiesceDeadline) && clean < 5 {
		var s int
		rowExists := repo.DB().QueryRowContext(t.Context(), `SELECT state FROM dispatch_jobs WHERE id = ?`, j.ID()).Scan(&s) == nil
		if !rowExists || application.Checkpointer().DirtyCount() != 0 {
			clean = 0
		} else {
			clean++
		}
		time.Sleep(checkInterval)
	}
	if clean < 5 {
		t.Fatal("the checkpointer never went quiet, so a skipped checkpoint cannot be distinguished from a busy one")
	}

	// Plant a value only a checkpoint would overwrite. Checkpoint writes to
	// dispatch_jobs table, so if the ticker saves while clean the sentinel disappears.
	const sentinelState = 99
	res, err := repo.DB().ExecContext(t.Context(),
		`UPDATE dispatch_jobs SET state = ? WHERE id = ?`, sentinelState, j.ID())
	if err != nil {
		t.Fatalf("plant sentinel: %v", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		t.Fatalf("plant sentinel: 0 rows updated for job %s", j.ID())
	}

	time.Sleep(10 * checkInterval)

	var got int
	if err := repo.DB().QueryRowContext(t.Context(),
		`SELECT state FROM dispatch_jobs WHERE id = ?`, j.ID()).Scan(&got); err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if got != sentinelState {
		t.Errorf("the job row was rewritten on a clean queue (state = %d, want sentinel %d); checkpoint is saving every tick instead of only when dirty", got, sentinelState)
	}
}
