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
	dir := filepath.Join(adminDir, "queue")
	store := queue.NewSQLiteStore(repo.DB(), dir, repo)
	return queue.New(queue.WithStore(store), queue.WithStateDir(dir))
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
	startTime := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	finishTime := time.Now().Truncate(time.Second)
	_ = seed.MarkJobStarted(job.ID, startTime)
	ackDone(t, seed, job.ID, "a@t")
	if err := seed.MarkFileComplete(job.ID, 0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}
	_ = seed.MarkDownloadFinished(job.ID, finishTime)
	// Set PostProc directly (still a plain exported field), leaving Status
	// at Queued — this reproduces the exact crash-simulated inconsistency
	// under test: PostProc=true while Status never transitioned through
	// SetPostProcStarted (a real crash can strand this exact combination).
	job.PostProc = postProc
	seed.ResumeAll(context.Background())
}

// setupTestDirsAndRepo creates the admin, download and complete directories a
// recovery scenario needs, plus a history repository over a real SQLite file in
// the admin directory. Shared by every test in this file.
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

// recoveryLiveness bounds how long a recovery scenario waits for the
// application to finish a transition. It is a LIVENESS bound, not a
// performance assertion: these tests assert that a job reaches history, never
// that it gets there quickly, so the only job of this number is to fail
// instead of hanging when something is genuinely stuck.
//
// That makes over-provisioning free and under-provisioning expensive, and the
// old values were under-provisioned. TestCheckpoint_SurvivesCrashMidDownload
// failed on CI at 10.23s against a 10s bound, having passed 23 consecutive
// local runs including the whole package pinned to two cores and fifteen
// iterations pinned to one. The runner is simply slower than anything
// reproducible here — its internal/app run took 108s against 69s for the
// two-core pinned run — and a scenario that restarts the entire application
// twice and re-downloads an article had no headroom for that.
//
// A passing run pays nothing for the larger number, because waitUntil returns
// as soon as the condition holds.
//
// Other scenario waits in this package still use ad-hoc 2s/5s/10s literals.
// They are the same kind of bound and could adopt this constant; they are left
// alone here rather than swept, so that a future CI failure attributes to the
// wait it actually exceeded.
const recoveryLiveness = 30 * time.Second

func waitForHistoryAndQueueCleanup(t *testing.T, repo *history.Repository, a *app.Application, jobID string) {
	t.Helper()
	if !waitUntil(recoveryLiveness, func() bool {
		_, err := repo.Get(t.Context(), jobID)
		return err == nil
	}) {
		t.Fatalf("timeout waiting for job %s to reach history after recovery", jobID)
	}
	if !waitUntil(recoveryLiveness, func() bool {
		return a.Queue().SnapshotJob(jobID) == nil
	}) {
		snap := a.Queue().SnapshotJob(jobID)
		t.Fatalf("job %s still in active queue after recovery (status=%q)", jobID, snap.Status)
	}
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

	histEntry, err := repo.Get(t.Context(), jobID)
	if err != nil {
		t.Fatalf("repo.Get: %v", err)
	}
	if histEntry.DownloadTime <= 0 {
		t.Errorf("DownloadTime = %d, want > 0 after recovery", histEntry.DownloadTime)
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

// TestRecovery_CrashBetweenMultiStoreWrites verifies that a stale job
// document left in queue/jobs/ by the removed whole-queue JSON engine is
// pruned on startup, and that a job caught mid-transition to history
// re-completes without duplicating its entry.
//
// It used to cover a second crash window as well: persistAndCommit wrote
// history/jobs/<id>.json.gz before historyRepo.Add, so a crash between the
// two left an orphaned payload. #298 removed that write — MoveToHistory is
// now the first mutation persistAndCommit makes, and it is a single
// transaction — so the window is closed by construction and there is nothing
// left to simulate.
func TestRecovery_CrashBetweenMultiStoreWrites(t *testing.T) {
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)

	const (
		crashedJobID    = "recover0-00000003" // Orphaned in queue/jobs by the removed JSON engine
		transitionJobID = "recover0-00000004" // Caught mid-transition to history
	)

	seed := newSeedQueue(t, repo, adminDir)
	// Seed transitionJobID as a completed job with PostProc=true.
	seedCompletedJob(t, seed, transitionJobID, "recovery-transition", true)
	if err := seed.Save(filepath.Join(adminDir, "queue")); err != nil {
		t.Fatalf("seed.Save: %v", err)
	}

	// Leave a job document in queue/jobs/ that no live job claims. Pruning
	// is by filename against the active job set, so the contents are
	// irrelevant — which is just as well, since nothing writes this format
	// any more.
	orphanPath := filepath.Join(adminDir, "queue", "jobs", crashedJobID+".json.gz")
	if err := os.MkdirAll(filepath.Dir(orphanPath), 0o750); err != nil {
		t.Fatalf("MkdirAll queue/jobs: %v", err)
	}
	if err := os.WriteFile(orphanPath, []byte("stale job document"), 0o600); err != nil {
		t.Fatalf("write orphan: %v", err)
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

	// Verify no duplicate history entries were created.
	entries, err := repo.Search(t.Context(), history.SearchOptions{})
	if err != nil {
		t.Fatalf("repo.Search: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(entries))
	}
}
