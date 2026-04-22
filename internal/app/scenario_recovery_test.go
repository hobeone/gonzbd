package app_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/app"
	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/constants"
	"github.com/hobeone/sabnzbd-go/internal/history"
	"github.com/hobeone/sabnzbd-go/internal/nntp/nntptest"
	"github.com/hobeone/sabnzbd-go/internal/postproc"
	"github.com/hobeone/sabnzbd-go/internal/queue"
)

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
	seeded := &queue.Job{
		ID:     jobID,
		Name:   "recovery",
		Status: constants.StatusQueued,
		Files: []queue.JobFile{{
			Subject:  "recovery.bin",
			Complete: true,
			Articles: []queue.JobArticle{{ID: "a@t", Done: true, Bytes: 100}},
			Bytes:    100,
		}},
		TotalBytes:     100,
		RemainingBytes: 0,
		PostProc:       true,
	}
	if err := seed.Add(seeded); err != nil {
		t.Fatalf("seed.Add: %v", err)
	}
	if err := seed.Save(filepath.Join(adminDir, "queue")); err != nil {
		t.Fatalf("seed.Save: %v", err)
	}

	db, err := history.Open(filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	cfg := app.Config{
		DownloadDir: downloadDir,
		CompleteDir: completeDir,
		AdminDir:    adminDir,
		CacheLimit:  1 << 20,
		// The job is already complete; the downloader should never
		// actually be asked to fetch anything. Provide a scripted
		// server purely to satisfy app.New's at-least-one-server
		// requirement.
		Servers: []config.ServerConfig{nntptest.New(t).ServerConfig("recovery", 1)},
	}

	a, err := app.New(cfg, repo, app.WithPostProcStages([]postproc.Stage{noOpStage{}}))
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = a.Shutdown()
	})
	if err := a.Start(ctx); err != nil {
		t.Fatalf("app.Start: %v", err)
	}
	go drainAny(ctx, a.FileComplete())
	go drainAny(ctx, a.JobComplete())
	go drainAny(ctx, a.PostProcComplete())

	if !waitUntil(10*time.Second, func() bool {
		_, err := repo.Get(context.Background(), jobID)
		return err == nil
	}) {
		t.Fatalf("timeout waiting for job %s to reach history after recovery", jobID)
	}

	if snap := a.Queue().SnapshotJob(jobID); snap != nil {
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
