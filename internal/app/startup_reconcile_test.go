package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// TestReconcileBeforeFirstTick_StopsOnACancelledContext pins that the step
// gives up on a cancelled startup before it reaches a queued job.
func TestReconcileBeforeFirstTick_StopsOnACancelledContext(t *testing.T) {
	t.Parallel()
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
	restoreQueue(t, application)
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
	}); err != nil {
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
	restoreQueue(t, application)

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
	}); err != nil {
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

// TestStart_LeavesAJobRestoredPausedAtFetchingUnverified pins that no startup
// pass that finishes inside Start hydrates a paused job: the job leaves Start
// with the restore pass's seed and nothing more. A background pass that
// finished after Start returned would not be caught here.
func TestStart_LeavesAJobRestoredPausedAtFetchingUnverified(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	j := pauseAtFetching(t, env, env.newApp(t), "paused")
	a2 := env.newApp(t)
	a2.start(t)
	j2 := a2.registered(t, j.ID())
	if !j2.HasProgress() {
		t.Fatal("the restored job has no progress after Start")
	}
	if j2.AllVerified() {
		t.Error("the restored paused job was verified during Start")
	}
	if j2.Resident() {
		t.Error("the restored paused job is resident after Start")
	}
	row, ok := a2.dispatcher.Row(j.ID())
	if !ok {
		t.Fatal("the job is not registered")
	}
	if want := row.Header.Bytes - 2*lrArtLen; row.RemainingBytes != want {
		t.Errorf("remaining = %d of %d, want %d estimated from the 2 recorded articles", row.RemainingBytes, row.Header.Bytes, want)
	}
}

// TestStart_FilesAnOwedJobInsideStart pins the sweep's place in
// reconcileBeforeFirstTick: a restored job owed an unwanted-extension filing is
// filed inside Start, before the first tick, over a real restarted queue. The
// paused job is there to show that an owed filing inside Start leaves an
// unrelated paused job paused (the IntentPause assertion).
func TestStart_FilesAnOwedJobInsideStart(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a1 := env.newApp(t)
	paused := pauseAtFetching(t, env, a1, "paused")
	owed := a1.addJob(t, "owed", 2, 1)
	a1.dispatcher.Tick(t.Context())
	a1.dispatcher.Tick(t.Context())
	if st := owed.State().State; st != job.Fetching {
		t.Fatalf("fixture: the owed job is at %v, want Fetching", st)
	}
	if moved, err := a1.dispatcher.BlockUnwanted(owed, false); err != nil || !moved {
		t.Fatalf("BlockUnwanted = (%v, %v)", moved, err)
	}
	a1.dispatcher.Tick(t.Context()) // persists the block

	a2 := env.newApp(t)
	a2.start(t)

	owed2 := a2.registered(t, owed.ID())
	if !a2.postProcAdmissions.has(owed2) {
		t.Error("the owed filing was not made inside Start")
	}
	paused2 := a2.registered(t, paused.ID())
	if paused2.Intent() != job.IntentPause {
		t.Errorf("the paused job's intent is %v after Start, want IntentPause", paused2.Intent())
	}
}

// TestFileOwedUnwantedFailures_FailedHydrationIsLoggedAndFiled pins the
// sweep's handling of a job it cannot load: the hydration's error is logged
// at Warn, and the job is filed anyway rather than left to run.
func TestFileOwedUnwantedFailures_FailedHydrationIsLoggedAndFiled(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a1 := env.newApp(t)
	owed := a1.addJob(t, "owed", 2, 1)
	a1.dispatcher.Tick(t.Context())
	a1.dispatcher.Tick(t.Context())
	if st := owed.State().State; st != job.Fetching {
		t.Fatalf("fixture: the owed job is at %v, want Fetching", st)
	}
	if moved, err := a1.dispatcher.BlockUnwanted(owed, false); err != nil || !moved {
		t.Fatalf("BlockUnwanted = (%v, %v)", moved, err)
	}
	a1.dispatcher.Tick(t.Context()) // persists the block
	path, err := manifestPath(env.admin, owed.ID())
	if err != nil {
		t.Fatalf("manifestPath: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the manifest: %v", err)
	}

	logs := &syncBuffer{}
	a2 := env.newApp(t, func(app *Application) {
		app.log = slog.New(slog.NewTextHandler(logs, nil))
	})
	if err := a2.fileOwedUnwantedFailures(t.Context()); err != nil {
		t.Fatalf("fileOwedUnwantedFailures: %v", err)
	}

	owed2 := a2.registered(t, owed.ID())
	if !a2.postProcAdmissions.has(owed2) {
		t.Error("a job whose hydration failed was not filed")
	}
	if got := logs.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "it is filed without its verified files") {
		t.Errorf("the failed hydration was not logged at Warn; log:\n%s", got)
	}
}

// TestFileOwedUnwantedFailures_StorageFaultThenUserResumeStillFiles pins that
// a storage fault in the startup hydration of a job the fail action blocked
// does not turn the owed filing into an approval choice (#1033). The fault
// parks the job with a "resume to retry" reason; the user's resume, made after
// the fault has cleared, must not approve the block, and the job is filed
// Failed with the unwanted-extension message rather than downloaded.
//
// Not parallel: preadAt is a package-level seam.
func TestFileOwedUnwantedFailures_StorageFaultThenUserResumeStillFiles(t *testing.T) {
	env := newLREnv(t)
	a1 := env.newApp(t)
	owed := a1.addJob(t, "owed", 3, 2)
	env.writeFileA(t, owed, 3, 0, 1)
	a1.recordFileA(t, owed.ID(), false, 0, 1)
	a1.dispatcher.Tick(t.Context())
	a1.dispatcher.Tick(t.Context())
	if st := owed.State().State; st != job.Fetching {
		t.Fatalf("fixture: the owed job is at %v, want Fetching", st)
	}
	if moved, err := a1.dispatcher.BlockUnwanted(owed, false); err != nil || !moved {
		t.Fatalf("BlockUnwanted = (%v, %v)", moved, err)
	}
	a1.dispatcher.Tick(t.Context()) // persists the block

	// The startup hydration's read-back of file A's second article fails.
	var failing atomic.Bool
	failing.Store(true)
	var faults atomic.Int32
	prev := preadAt
	preadAt = func(f *os.File, b []byte, off int64) (int, error) {
		if off == lrArtLen && failing.Load() {
			faults.Add(1)
			return 0, &os.PathError{Op: "pread", Path: f.Name(), Err: syscall.EIO}
		}
		return prev(f, b, off)
	}
	t.Cleanup(func() { preadAt = prev })

	a2 := env.newApp(t)
	a2.start(t)
	if faults.Load() == 0 {
		t.Fatal("fixture: the startup hydration never hit the injected fault")
	}

	// The fault clears, and the user resumes the job as the API's per-job
	// resume does. The job may already be on its way out, filed: cancelled
	// by its finalize (the intent is latched), or gone.
	failing.Store(false)
	if err := a2.dispatcher.ResumeJobByUser(owed.ID()); err != nil &&
		!errors.Is(err, dispatch.ErrNotFound) && !errors.Is(err, job.ErrIntentLatched) {
		t.Fatalf("ResumeJobByUser: %v", err)
	}
	a2.ReevaluateStalls()
	if row, ok := a2.dispatcher.Row(owed.ID()); ok && row.Header.Unwanted == unwanted.StateApproved {
		t.Errorf("the user's resume after a storage fault approved a job the fail action blocked")
	}

	if status := a2.lrWaitFinalized(t, owed.ID()); status != string(constants.StatusFailed) {
		t.Errorf("history status = %q, want %q", status, constants.StatusFailed)
	}
	e, err := a2.repo.Get(t.Context(), owed.ID())
	if err != nil {
		t.Fatalf("history Get: %v", err)
	}
	if !strings.HasPrefix(e.FailMessage, unwantedFailPrefix) {
		t.Errorf("fail message = %q, want the unwanted-extension message", e.FailMessage)
	}
	if e.Unwanted != unwanted.StateBlocked {
		t.Errorf("history unwanted_ext = %d, want %d (blocked), so Retry asks for approval", e.Unwanted, unwanted.StateBlocked)
	}
	if _, ok := a2.dispatcher.Job(owed.ID()); ok {
		t.Error("the filed job is still registered")
	}
}
