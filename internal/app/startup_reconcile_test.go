package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// TestReconcileBeforeFirstTick_StopsOnACancelledContext pins that the step
// gives up on a cancelled startup before it reaches a queued job, and that an
// application with no dispatcher has nothing to reconcile.
func TestReconcileBeforeFirstTick_StopsOnACancelledContext(t *testing.T) {
	t.Parallel()
	if err := (&Application{}).reconcileBeforeFirstTick(t.Context()); err != nil {
		t.Errorf("no dispatcher: err = %v, want nil", err)
	}

	adminDir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	application, err := New(testConfigInternal(t, adminDir), repo)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject: "file.bin", Bytes: 1024,
		Articles: []nzb.Article{{ID: "cancelled1@t", Bytes: 1024, Number: 1}},
	}}}
	j, hdr, err := BuildIngestJob(application.config, parsed, "cancelled.nzb",
		types.FetchOptions{JobID: "reconcilecancel1", NzbName: "cancelled"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.Dispatcher().Add(t.Context(), j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Filed in history, so a reconcile that reached the job would act on it:
	// drop it, or, since its lookup runs on the cancelled context and fails,
	// pause it.
	if err := repo.Add(t.Context(), history.Entry{
		NzoID: j.ID(), Name: "cancelled",
		Status: string(constants.StatusCompleted), Completed: time.Now(),
	}, nil); err != nil {
		t.Fatalf("seed history entry: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := application.reconcileBeforeFirstTick(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: err = %v, want context.Canceled", err)
	}
	if _, ok := application.Dispatcher().Job(j.ID()); !ok {
		t.Fatal("a cancelled reconcile dropped the job")
	}
	if got := j.Intent(); got != job.IntentRun {
		t.Errorf("intent = %s after a cancelled reconcile, want %s: the step reached the job",
			got, job.IntentRun)
	}
}

// TestStart_RetentionDoesNotDefeatCrashReconciliation pins that enabling
// history retention leaves the startup duplicate-removal working.
//
// Start reconciles a crash between the history commit and Dispatcher.Remove: a
// completed job still sitting in the queue is looked up in history, and if
// the entry is there the stale queue entry is dropped. That lookup is the
// only evidence the job already finished. A retention sweep that runs first
// can delete the entry out from under it, at which point Get returns
// ErrNotFound and the job stays queued — post-processed and re-filed a second
// time.
//
// The trigger is ordinary: the crash left an entry behind, the daemon stayed
// down past the retention threshold, and the operator restarted it.
func TestStart_RetentionDoesNotDefeatCrashReconciliation(t *testing.T) {
	adminDir := t.TempDir()
	cfg := testConfigInternal(t, adminDir)
	// Short enough that the 30-day-old entry below is expired.
	cfg.General.HistoryRetentionDays = 1

	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	application, err := New(cfg, repo)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// A completed job still in the queue — the state a crash between the
	// history commit and Dispatcher.Remove leaves behind.
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "file.bin",
		Bytes:    1024,
		Articles: []nzb.Article{{ID: "rec1@t", Bytes: 1024, Number: 1}},
	}}}
	j, hdr, err := BuildIngestJob(application.config, parsed, "reconcile.nzb", types.FetchOptions{JobID: "reconcilestale01", NzbName: "reconcile"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.Dispatcher().Add(context.Background(), j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// IsComplete keys on the per-file Complete flag, which the assembler
	// sets, not on article state.
	if err := j.MarkFileComplete(0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}
	if !j.IsComplete() {
		t.Fatal("setup did not produce a completed job in the queue")
	}

	// Its history entry, old enough that retention wants it gone.
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     j.ID(),
		Name:      "reconcile",
		Status:    string(constants.StatusCompleted),
		Completed: time.Now().AddDate(0, 0, -30),
	}, nil); err != nil {
		t.Fatalf("seed history entry: %v", err)
	}

	application.PauseDownloads()
	application.Dispatcher().Pause()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = application.Shutdown() })

	if _, ok := application.Dispatcher().Row(j.ID()); ok {
		t.Error("the completed job is still queued: retention deleted its history entry " +
			"before reconciliation could look it up, so it will be post-processed again")
	}
}
