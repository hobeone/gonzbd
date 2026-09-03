package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// newLifecycleTestApp builds a real Application (SQLite-backed dispatcher and
// history, matching production) over temp directories, without calling
// Start — the goroutine-driven helpers under test here take their own ctx
// argument and only need app.dispatcher/app.config/app.log/app.historyRepo
// wired, which New already does.
func newLifecycleTestApp(t *testing.T, opts ...func(*Application)) (*Application, *history.Repository, string) {
	t.Helper()
	adminDir := t.TempDir()
	cfg := testConfig(t.TempDir(), t.TempDir(), adminDir, config.ServerConfig{
		Name: "mock", Host: "127.0.0.1", Port: 1119, Enable: false,
	})
	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	application, err := New(cfg, repo, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return application, repo, adminDir
}

// ---------- deleteHistoryEntries ----------

// TestDeleteHistoryEntries_EmptyIsNoOp pins the guard: an empty slice
// returns (0, nil) immediately, without touching historyRepo — checked here
// by calling it on a zero-value Application whose historyRepo is nil, which
// would panic if the empty check were ever removed or reordered.
func TestDeleteHistoryEntries_EmptyIsNoOp(t *testing.T) {
	t.Parallel()
	application := &Application{}
	n, err := application.deleteHistoryEntries(t.Context(), nil)
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
}

// TestDeleteHistoryEntries_RemovesRowAndBackup pins the normal path: both
// the history row and the NZB backup it owns go together.
func TestDeleteHistoryEntries_RemovesRowAndBackup(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newLifecycleTestApp(t)
	nzbDir := filepath.Join(adminDir, "nzb")
	if err := os.MkdirAll(nzbDir, 0o750); err != nil {
		t.Fatalf("mkdir nzb: %v", err)
	}
	backupPath := filepath.Join(nzbDir, "job.nzb.gz")
	if err := os.WriteFile(backupPath, []byte("gz placeholder"), 0o600); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	entry := history.Entry{NzoID: "deleteentry00001", Name: "job", Status: "Failed", NZBBackup: "job.nzb.gz"}
	if err := repo.Add(t.Context(), entry); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	n, err := application.deleteHistoryEntries(t.Context(), []history.Entry{entry})
	if err != nil {
		t.Fatalf("deleteHistoryEntries: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Errorf("backup survived deletion, stat err = %v", err)
	}
	if _, err := repo.Get(t.Context(), entry.NzoID); err == nil {
		t.Error("history row survived deletion")
	}
}

// TestDeleteHistoryEntries_ToleratesMissingBackup pins that an entry naming
// a backup file that is no longer on disk still deletes cleanly — the
// backup was never required for the download to have completed, so its
// absence must not block removing the row.
func TestDeleteHistoryEntries_ToleratesMissingBackup(t *testing.T) {
	t.Parallel()
	application, repo, _ := newLifecycleTestApp(t)
	entry := history.Entry{NzoID: "deleteentry00002", Name: "job2", Status: "Failed", NZBBackup: "gone.nzb.gz"}
	if err := repo.Add(t.Context(), entry); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	n, err := application.deleteHistoryEntries(t.Context(), []history.Entry{entry})
	if err != nil {
		t.Fatalf("deleteHistoryEntries with missing backup: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
}

// TestDeleteHistoryEntries_PropagatesRepoError pins that a failure deleting
// the rows (here: the database already closed) surfaces to the caller
// rather than being reported as a silent success.
func TestDeleteHistoryEntries_PropagatesRepoError(t *testing.T) {
	t.Parallel()
	adminDir := t.TempDir()
	cfg := testConfig(t.TempDir(), t.TempDir(), adminDir, config.ServerConfig{
		Name: "mock", Host: "127.0.0.1", Port: 1119, Enable: false,
	})
	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	repo := history.NewRepository(db)
	application, err := New(cfg, repo)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	entry := history.Entry{NzoID: "deleteentry00003", Name: "job3", Status: "Failed"}
	if err := repo.Add(t.Context(), entry); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	if _, err := application.deleteHistoryEntries(t.Context(), []history.Entry{entry}); err == nil {
		t.Fatal("deleteHistoryEntries succeeded against a closed database, want an error")
	}
}

// ---------- runCheckpoint ----------

// TestRunCheckpoint_SavesWhenDirty pins that the ticker persists the checkpoint
// once it is dirty, driving the loop directly rather than through Start so
// the effect (DirtyCount transitioning to 0) is attributable to
// runCheckpoint alone.
func TestRunCheckpoint_SavesWhenDirty(t *testing.T) {
	t.Parallel()
	const interval = 20 * time.Millisecond
	application, _, _ := newLifecycleTestApp(t)

	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "f.bin",
		Bytes:    100,
		Articles: []nzb.Article{{ID: "a@t", Bytes: 100, Number: 1}},
	}}}
	j, hdr, err := BuildIngestJob(application.config, parsed, "f.nzb", types.FetchOptions{NzbName: "checkpoint-job"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.AddJob(t.Context(), j, hdr, nil, false); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		application.runCheckpoint(ctx, interval)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	application.checkpointer.Mark(j)
	if application.checkpointer.DirtyCount() == 0 {
		t.Fatal("fixture guard: checkpointer not dirty right after Mark")
	}

	deadline := time.Now().Add(20 * interval)
	for time.Now().Before(deadline) {
		if application.checkpointer.DirtyCount() == 0 {
			return // runCheckpoint saved and cleared the dirty set
		}
		time.Sleep(interval / 2)
	}
	t.Errorf("checkpointer still dirty after %v; runCheckpoint never saved", 20*interval)
}

// TestRunCheckpoint_SkipsWhenClean pins the self-gate: the ticker must not
// write when nothing is dirty.
func TestRunCheckpoint_SkipsWhenClean(t *testing.T) {
	t.Parallel()
	const interval = 5 * time.Millisecond
	application, _, _ := newLifecycleTestApp(t)

	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "f.bin",
		Bytes:    100,
		Articles: []nzb.Article{{ID: "a@t", Bytes: 100, Number: 1}},
	}}}
	j, hdr, err := BuildIngestJob(application.config, parsed, "f.nzb", types.FetchOptions{NzbName: "checkpoint-clean-job"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.AddJob(t.Context(), j, hdr, nil, false); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	// Initial flush to ensure clean state
	if err := application.checkpointer.Flush(t.Context()); err != nil {
		t.Fatalf("initial Flush: %v", err)
	}
	if application.checkpointer.DirtyCount() != 0 {
		t.Fatal("fixture guard: checkpointer dirty right after Flush")
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		application.runCheckpoint(ctx, interval)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	time.Sleep(10 * interval)
	if application.checkpointer.DirtyCount() != 0 {
		t.Errorf("checkpointer became dirty unexpectedly: %d", application.checkpointer.DirtyCount())
	}
}

// TestRunCheckpoint_ExitsOnContextCancel pins that runCheckpoint returns
// promptly once ctx is cancelled, rather than only on the next tick or
// never — Shutdown's wg.Wait depends on this to unblock.
func TestRunCheckpoint_ExitsOnContextCancel(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)

	// An interval far longer than the test timeout: if the goroutine only
	// exited on a tick rather than on ctx.Done, this test would time out.
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		application.runCheckpoint(ctx, time.Hour)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runCheckpoint did not return after context cancellation")
	}
}

// ---------- watchCompletions ----------

// newWatchCompletionsTestApp builds an Application whose dispatcher holds one job
// with two independent files, so marking file 0 complete never completes
// the job itself — keeping handleFileComplete's finalize path (postproc,
// history) out of scope for these tests, which target only the
// watchCompletions dispatch loop.
func newWatchCompletionsTestApp(t *testing.T) (*Application, *job.Job) {
	t.Helper()
	return newWatchCompletionsTestAppN(t, 2)
}

// newWatchCompletionsTestAppN is newWatchCompletionsTestApp generalized to
// nFiles independent single-article files, none of which ever completes the
// job as a whole (there is always at least one other file left pending).
func newWatchCompletionsTestAppN(t *testing.T, nFiles int) (*Application, *job.Job) {
	t.Helper()
	application, _, _ := newLifecycleTestApp(t)

	parsed := &nzb.NZB{}
	for i := range nFiles {
		parsed.Files = append(parsed.Files, nzb.File{
			Subject:  fmt.Sprintf("file-%d.bin", i),
			Bytes:    100,
			Articles: []nzb.Article{{ID: fmt.Sprintf("art-%d@t", i), Bytes: 100, Number: 1}},
		})
	}
	j, hdr, err := BuildIngestJob(application.config, parsed, "watch.nzb", types.FetchOptions{NzbName: "watch-completions-job"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.AddJob(t.Context(), j, hdr, nil, false); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	return application, j
}

// waitForFileComplete polls the dispatcher for fileIdx's completion within a
// bounded deadline.
func waitForFileComplete(t *testing.T, application *Application, jobID string, fileIdx int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := application.Dispatcher().Job(jobID); ok && j.Progress() != nil && j.Progress().FileComplete(fileIdx) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("file %d never marked complete", fileIdx)
}

// TestWatchCompletions_DispatchesToHandleFileComplete pins the loop's normal
// path: a completion event sent on internalFileComplete is applied to the
// job while the goroutine is running.
func TestWatchCompletions_DispatchesToHandleFileComplete(t *testing.T) {
	t.Parallel()
	application, j := newWatchCompletionsTestApp(t)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		application.watchCompletions(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	application.internalFileComplete <- FileComplete{JobID: j.ID(), FileIdx: 0}

	waitForFileComplete(t, application, j.ID(), 0)

	cancel()
	<-done
	if dj, ok := application.Dispatcher().Job(j.ID()); ok && dj.Progress().FileComplete(1) {
		t.Error("file 1 reported complete; only file 0's event was sent")
	}
}

// TestWatchCompletions_DrainsPendingOnContextCancel pins the shutdown
// contract documented on Shutdown/handleFileComplete: events already queued
// when ctx is cancelled must still be applied via drainCompletions before
// the goroutine returns, not dropped.
func TestWatchCompletions_DrainsPendingOnContextCancel(t *testing.T) {
	t.Parallel()
	const nEvents = 6
	application, j := newWatchCompletionsTestAppN(t, nEvents)

	ctx, cancel := context.WithCancel(t.Context())
	// Buffer all the events and cancel before the loop ever starts, so the
	// first iteration's select sees ctx.Done and a full backlog ready.
	for i := range nEvents {
		application.internalFileComplete <- FileComplete{JobID: j.ID(), FileIdx: i}
	}
	cancel()

	done := make(chan struct{})
	go func() {
		application.watchCompletions(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchCompletions did not return after context cancellation")
	}

	for i := range nEvents {
		if !j.Progress().FileComplete(i) {
			t.Errorf("file %d: pending completion was dropped instead of drained on shutdown", i)
		}
	}
}
