package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dispatch"
	dispatchstore "github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
)

// seedCompletedJob builds a real *job.Job (one file, one article, 100
// bytes, fully downloaded and marked complete) and adds it to dispatch_jobs
// and writes its manifest.
func seedCompletedJob(t *testing.T, repo *history.Repository, adminDir, id, name string, state job.State) *job.Job {
	t.Helper()
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "recovery.bin", Bytes: 100, Articles: []nzb.Article{{ID: "a@t", Bytes: 100, Number: 1}}},
	}}
	cfg := &config.Config{}
	j, hdr := buildTestJob(t, cfg, parsed, types.FetchOptions{NzbName: name, JobID: id})
	_ = j.SetFileFilename(0, "recovery.bin")
	_ = j.BeginAttempt(time.Now())
	_ = j.MarkArticleDone(0, 100, "mock")
	_ = j.MarkFileComplete(0)

	if state != job.StateUnset && state != job.Fetching {
		switch state {
		case job.Assessing:
			if err := j.SetNext(job.Assessing); err != nil {
				t.Fatalf("SetNext Assessing: %v", err)
			}
			if err := j.Transition(job.Assessing); err != nil {
				t.Fatalf("Transition Assessing: %v", err)
			}
		case job.Repairing:
			if err := j.SetNext(job.Assessing); err != nil {
				t.Fatalf("SetNext Assessing: %v", err)
			}
			if err := j.Transition(job.Assessing); err != nil {
				t.Fatalf("Transition Assessing: %v", err)
			}
			if err := j.SetNext(job.Repairing); err != nil {
				t.Fatalf("SetNext Repairing: %v", err)
			}
			if err := j.Transition(job.Repairing); err != nil {
				t.Fatalf("Transition Repairing: %v", err)
			}
		case job.Extracting:
			if err := j.SetNext(job.Assessing); err != nil {
				t.Fatalf("SetNext Assessing: %v", err)
			}
			if err := j.Transition(job.Assessing); err != nil {
				t.Fatalf("Transition Assessing: %v", err)
			}
			if err := j.SetNext(job.Extracting); err != nil {
				t.Fatalf("SetNext Extracting: %v", err)
			}
			if _, err := j.Cross(job.Extracting); err != nil {
				t.Fatalf("Cross Extracting: %v", err)
			}
		case job.Finalizing:
			if err := j.SetNext(job.Assessing); err != nil {
				t.Fatalf("SetNext Assessing: %v", err)
			}
			if err := j.Transition(job.Assessing); err != nil {
				t.Fatalf("Transition Assessing: %v", err)
			}
			if err := j.SetNext(job.Extracting); err != nil {
				t.Fatalf("SetNext Extracting: %v", err)
			}
			if _, err := j.Cross(job.Extracting); err != nil {
				t.Fatalf("Cross Extracting: %v", err)
			}
			if err := j.SetNext(job.Finalizing); err != nil {
				t.Fatalf("SetNext Finalizing: %v", err)
			}
			if err := j.Transition(job.Finalizing); err != nil {
				t.Fatalf("Transition Finalizing: %v", err)
			}
		}
	}

	manifestDir := filepath.Join(adminDir, "queue", "manifests")
	if err := os.MkdirAll(manifestDir, 0o750); err != nil {
		t.Fatalf("mkdir manifests: %v", err)
	}
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := fsutil.WriteGzAtomicBytes(filepath.Join(manifestDir, j.ID()+".json.gz"), data); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	store := dispatchstore.New(repo.DB())
	cp := j.Checkpoint()
	p := dispatch.Persisted{
		ID:      j.ID(),
		SortKey: 1,
		Header:  hdr,
		Policy:  j.Policy(),
		State:   cp.State,
		Intent:  j.Intent(),
	}
	if err := store.Save(t.Context(), p); err != nil {
		t.Fatalf("store.Save: %v", err)
	}
	_, err = repo.DB().ExecContext(t.Context(),
		`INSERT INTO job_files (job_id, file_index, subject, date, bytes, complete, assembled_crc32, fetch_policy, filename)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID(), 0, "recovery.bin", 0, 100, 1, 0, int(job.FetchAlways), "recovery.bin",
	)
	if err != nil {
		t.Fatalf("insert job_files: %v", err)
	}
	return j
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
const recoveryLiveness = 30 * time.Second

func waitForHistoryAndQueueCleanup(t *testing.T, repo *history.Repository, a *app.Application, jobID string) {
	t.Helper()
	if !waitUntil(recoveryLiveness, func() bool {
		_, err := repo.Get(t.Context(), jobID)
		return err == nil
	}) {
		row, _ := a.Dispatcher().Row(jobID)
		var pStr string
		if j, ok := a.Dispatcher().Job(jobID); ok {
			p := j.Progress()
			pStr = fmt.Sprintf("resident=%v done=[%v,%v,%v] files=[%v,%v,%v]",
				j.Resident(),
				p.ArticleDone(0), p.ArticleDone(1), p.ArticleDone(2),
				p.FileComplete(0), p.FileComplete(1), p.FileComplete(2))
		}
		t.Fatalf("timeout waiting for job %s to reach history after recovery: row=%+v %s", jobID, row, pStr)
	}
	if !waitUntil(recoveryLiveness, func() bool {
		_, ok := a.Dispatcher().Row(jobID)
		return !ok
	}) {
		row, _ := a.Dispatcher().Row(jobID)
		t.Fatalf("job %s still in active queue after recovery (status=%q)", jobID, row.Status())
	}
}

// TestRecovery_PostProcTrueOnRestart verifies that Application.Start
// finalises a job whose post-processing state survived a crash.
func TestRecovery_PostProcTrueOnRestart(t *testing.T) {
	t.Parallel()
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)
	const jobID = "recover0-00000001"

	seedCompletedJob(t, repo, adminDir, jobID, "recovery", job.Repairing)

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
	t.Parallel()
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)
	const jobID = "recover0-00000002"

	seedCompletedJob(t, repo, adminDir, jobID, "recovery-dup", job.StateUnset)

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

	if _, ok := a.Dispatcher().Row(jobID); ok {
		t.Errorf("job %s was not removed from queue on startup despite being in history", jobID)
	}
}

func TestRecovery_CrashBetweenMultiStoreWrites(t *testing.T) {
	t.Parallel()
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)

	const transitionJobID = "recover0-00000004" // Caught mid-transition to history

	seedCompletedJob(t, repo, adminDir, transitionJobID, "recovery-transition", job.Repairing)

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
