package app_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/queue"
)

// seedCompletedJob builds a real *queue.Job (one file, one article, 100
// bytes, fully downloaded and marked complete) via queue.NewJob and adds it
// to seed — the only way to reach that state, rather than a parallel
// struct-literal construction path.
func seedCompletedJob(t *testing.T, seed *queue.Queue, id, name string, postProc bool) {
	t.Helper()
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "recovery.bin", Bytes: 100, Articles: []nzb.Article{{ID: "a@t", Bytes: 100, Number: 1}}},
	}}
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: name + ".nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	job.ID = id
	job.Name = name
	job.Status = constants.StatusQueued
	if err := seed.Add(job); err != nil {
		t.Fatalf("seed.Add: %v", err)
	}
	if err := seed.MarkArticlesDone(job.ID, []string{"a@t"}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}
	if err := seed.MarkFileComplete(job.ID, 0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}
	// Set PostProc directly (still a plain exported field), leaving Status
	// at Queued — this reproduces the exact crash-simulated inconsistency
	// under test: PostProc=true while Status never transitioned through
	// SetPostProcStarted (a real crash can strand this exact combination).
	job.PostProc = postProc
}

// TestRecovery_PostProcTrueOnRestart verifies that Application.Start
// finalises a job whose PostProc flag survived a crash.
//
// The crash is simulated by seeding the on-disk queue state directly:
// a fully-downloaded, all-complete job with PostProc=true. In a real
// crash this is the state left on disk when a process died after the
// completion path flipped the flag but before OnJobDone's history.Add
// + queue.Remove ran. Startup rescan must pick the job up and drive
// it through post-processing to history.
//
// Pre-B.1 this failed because the rescan routed through
// sendToPostProcessor → SetPostProcStarted, saw PostProc already true,
// and silently dropped the handoff — stranding the job forever.
func TestRecovery_PostProcTrueOnRestart(t *testing.T) {
	adminDir := t.TempDir()
	downloadDir := t.TempDir()
	completeDir := t.TempDir()

	const jobID = "recover0-00000001"

	// Seed the on-disk queue with a stranded PostProc=true job. Using
	// a throwaway Queue.Add + Queue.Save writes the index and per-job
	// file in the same layout the live Application produces.
	seed := queue.New()
	seedCompletedJob(t, seed, jobID, "recovery", true)
	if err := seed.Save(filepath.Join(adminDir, "queue")); err != nil {
		t.Fatalf("seed.Save: %v", err)
	}

	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	cfg := testConfig(
		downloadDir,
		completeDir,
		adminDir,
		nntptest.New(t).ServerConfig("recovery", 1),
	)

	a, err := app.New(cfg, repo, app.WithPostProcStages([]postproc.Stage{noOpStage{}}))
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancel()
		_ = a.Shutdown()
	})
	if err := a.Start(ctx); err != nil {
		t.Fatalf("app.Start: %v", err)
	}
	go drainAny(ctx, a.JobComplete())
	go drainAny(ctx, a.PostProcComplete())

	if !waitUntil(10*time.Second, func() bool {
		_, err := repo.Get(t.Context(), jobID)
		return err == nil
	}) {
		t.Fatalf("timeout waiting for job %s to reach history after recovery", jobID)
	}

	// Wait for the job to be fully removed from the active queue.
	// There's a brief window where it appears in history but hasn't
	// been cleaned from the queue yet (status=Running).
	if !waitUntil(2*time.Second, func() bool {
		return a.Queue().SnapshotJob(jobID) == nil
	}) {
		snap := a.Queue().SnapshotJob(jobID)
		t.Errorf("job %s still in active queue after recovery (status=%q)", jobID, snap.Status)
	}
}

// drainAny reads values from src until ctx is done or src is closed.
func drainAny[T any](ctx context.Context, src <-chan T) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-src:
			if !ok {
				return
			}
		}
	}
}

// waitUntil polls cond at ~20ms intervals.
func waitUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRecovery_DuplicateJobInHistory(t *testing.T) {
	adminDir := t.TempDir()
	downloadDir := t.TempDir()
	completeDir := t.TempDir()

	const jobID = "recover0-00000002"

	// Seed active queue with a completed job.
	seed := queue.New()
	seedCompletedJob(t, seed, jobID, "recovery-dup", false)
	if err := seed.Save(filepath.Join(adminDir, "queue")); err != nil {
		t.Fatalf("seed.Save: %v", err)
	}

	// Seed history with the same job.
	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	entry := history.Entry{
		NzoID:     jobID,
		Name:      "recovery-dup",
		Status:    "Completed",
		Completed: time.Now(),
	}
	if err := repo.Add(t.Context(), entry); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	cfg := testConfig(
		downloadDir,
		completeDir,
		adminDir,
		nntptest.New(t).ServerConfig("recovery-dup", 1),
	)

	a, err := app.New(cfg, repo)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancel()
		_ = a.Shutdown()
	})
	if err := a.Start(ctx); err != nil {
		t.Fatalf("app.Start: %v", err)
	}

	// Verify that the duplicate job has been removed from the queue immediately on start.
	if a.Queue().SnapshotJob(jobID) != nil {
		t.Errorf("job %s was not removed from queue on startup despite being in history", jobID)
	}
}
