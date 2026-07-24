package app_test

import (
	"context"
	"os"
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

func newSeedQueue(t *testing.T, repo *history.Repository, adminDir string) *queue.Queue {
	t.Helper()
	store := queue.NewSQLiteStore(repo.DB(), filepath.Join(adminDir, "queue"), repo)
	return queue.New(queue.WithStore(store))
}

func loadTestQueue(t *testing.T, repo *history.Repository, adminDir string) *queue.Queue {
	t.Helper()
	store := queue.NewSQLiteStore(repo.DB(), filepath.Join(adminDir, "queue"), repo)
	q, err := queue.Load(filepath.Join(adminDir, "queue"), queue.WithStore(store))
	if err != nil {
		t.Fatalf("loadTestQueue: %v", err)
	}
	return q
}

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
func setupTestDirsAndRepo(t *testing.T) (adminDir, downloadDir, completeDir string, repo *history.Repository) {
	t.Helper()
	adminDir = t.TempDir()
	downloadDir = t.TempDir()
	completeDir = t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return adminDir, downloadDir, completeDir, history.NewRepository(db)
}

func startAppAndDrain(t *testing.T, a *app.Application) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	if err := a.Start(ctx); err != nil {
		t.Fatalf("app.Start: %v", err)
	}
	go drainAny(ctx, a.JobComplete())
	go drainAny(ctx, a.PostProcComplete())
	return ctx, cancel
}

func waitForHistoryAndQueueCleanup(t *testing.T, repo *history.Repository, a *app.Application, jobID string) {
	t.Helper()
	if !waitUntil(10*time.Second, func() bool {
		_, err := repo.Get(t.Context(), jobID)
		return err == nil
	}) {
		t.Fatalf("timeout waiting for job %s to reach history after recovery", jobID)
	}
	if !waitUntil(2*time.Second, func() bool {
		return a.Queue().SnapshotJob(jobID) == nil
	}) {
		snap := a.Queue().SnapshotJob(jobID)
		t.Fatalf("job %s still in active queue after recovery (status=%q)", jobID, snap.Status)
	}
}

func TestRecovery_PostProcTrueOnRestart(t *testing.T) {
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)
	const jobID = "recover0-00000001"

	seed := newSeedQueue(t, repo, adminDir)
	seedCompletedJob(t, seed, jobID, "recovery", true)
	if err := seed.Save(filepath.Join(adminDir, "queue")); err != nil {
		t.Fatalf("seed.Save: %v", err)
	}

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

	_, cancel := startAppAndDrain(t, a)
	t.Cleanup(func() {
		cancel()
		if err := a.Shutdown(); err != nil {
			t.Errorf("a.Shutdown: %v", err)
		}
	})

	waitForHistoryAndQueueCleanup(t, repo, a, jobID)
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
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)
	const jobID = "recover0-00000002"

	seed := newSeedQueue(t, repo, adminDir)
	seedCompletedJob(t, seed, jobID, "recovery-dup", false)
	if err := seed.Save(filepath.Join(adminDir, "queue")); err != nil {
		t.Fatalf("seed.Save: %v", err)
	}

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

	_, cancel := startAppAndDrain(t, a)
	t.Cleanup(func() {
		cancel()
		if err := a.Shutdown(); err != nil {
			t.Errorf("a.Shutdown: %v", err)
		}
	})

	if a.Queue().SnapshotJob(jobID) != nil {
		t.Errorf("job %s was not removed from queue on startup despite being in history", jobID)
	}
}

// TestRecovery_CrashBetweenMultiStoreWrites verifies crash recovery across multi-store
// write boundaries both in queue persistence (queue.Save) and during job finalization
// (persistAndCommit queue→history transition).
//
// 1) Queue multi-store save crash: In queue.Save, jobs/<id>.json.gz files are written
// first and queue.json.gz second. If a crash occurs after writing jobs/<new_id>.json.gz
// but before updating queue.json.gz, calling queue.Load (via app.New) ignores the
// unreferenced job file and Prune() removes it, leaving the queue state consistent.
//
// 2) Finalization multi-store save crash: During persistAndCommit, history/jobs/<id>.json.gz
// is written first and historyRepo.Add(entry) is called second. If a crash occurs after
// writing history/jobs/<id>.json.gz but before historyRepo.Add, restarting app re-enqueues
// the job for post-processing and successfully re-completes the transition without data
// corruption or duplicate entries.
func TestRecovery_CrashBetweenMultiStoreWrites(t *testing.T) {
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)

	const (
		crashedJobID    = "recover0-00000003" // Part 1: Orphaned in queue/jobs, unreferenced in queue.json.gz
		transitionJobID = "recover0-00000004" // Part 2: Crashed during persistAndCommit after job write, before db write
	)

	seed := newSeedQueue(t, repo, adminDir)
	// Seed transitionJobID as a completed job with PostProc=true.
	seedCompletedJob(t, seed, transitionJobID, "recovery-transition", true)
	if err := seed.Save(filepath.Join(adminDir, "queue")); err != nil {
		t.Fatalf("seed.Save: %v", err)
	}

	// Part 1 Simulate crash in queue.Save: Write crashedJobID directly to queue/jobs/<id>.json.gz
	// without adding it to queue.json.gz index.
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "orphan.bin", Bytes: 100, Articles: []nzb.Article{{ID: "o@t", Bytes: 100, Number: 1}}},
	}}
	orphanJob, err := queue.NewJob(parsed, queue.AddOptions{Filename: "orphan.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	orphanJob.ID = crashedJobID
	orphanJob.Name = "orphan-job"
	orphanPath := filepath.Join(adminDir, "queue", "jobs", crashedJobID+".json.gz")
	if err := queue.SaveJob(orphanPath, orphanJob); err != nil {
		t.Fatalf("SaveJob orphan: %v", err)
	}

	// Part 2 Simulate crash in persistAndCommit: Write history/jobs/<id>.json.gz
	// before historyRepo.Add has run.
	histJobsDir := filepath.Join(adminDir, "history", "jobs")
	if err := os.MkdirAll(histJobsDir, 0o750); err != nil {
		t.Fatalf("MkdirAll histJobsDir: %v", err)
	}
	histJobPath := filepath.Join(histJobsDir, transitionJobID+".json.gz")
	if err := queue.SaveJob(histJobPath, seed.SnapshotJob(transitionJobID)); err != nil {
		t.Fatalf("SaveJob history: %v", err)
	}

	cfg := testConfig(
		downloadDir,
		completeDir,
		adminDir,
		nntptest.New(t).ServerConfig("recovery-multistore", 1),
	)

	a, err := app.New(cfg, repo, app.WithPostProcStages([]postproc.Stage{noOpStage{}}))
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	// Verify Part 1 immediately after app.New (which ran queue.Load and Prune):
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Errorf("orphaned job file %q should have been pruned by Load, stat err = %v", orphanPath, err)
	}
	if snap := a.Queue().SnapshotJob(crashedJobID); snap != nil {
		t.Errorf("orphaned job %s should not exist in loaded queue", crashedJobID)
	}

	_, cancel := startAppAndDrain(t, a)
	t.Cleanup(func() {
		cancel()
		if err := a.Shutdown(); err != nil {
			t.Errorf("a.Shutdown: %v", err)
		}
	})

	waitForHistoryAndQueueCleanup(t, repo, a, transitionJobID)

	// Verify history payload file is intact and valid (no data corruption).
	loadedHistJob, err := queue.LoadJob(histJobPath)
	if err != nil {
		t.Fatalf("LoadJob on history payload failed: %v", err)
	}
	if loadedHistJob.ID != transitionJobID {
		t.Errorf("loaded history job ID = %q, want %q", loadedHistJob.ID, transitionJobID)
	}

	// Verify no duplicate history entries were created.
	entries, err := repo.Search(t.Context(), history.SearchOptions{})
	if err != nil {
		t.Fatalf("repo.Search: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(entries))
	}
}
