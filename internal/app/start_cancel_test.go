package app

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/history"
)

// newFailingStartApp builds an Application whose downloader fails to start,
// driving one of Application.Start's error returns.
func newFailingStartApp(t *testing.T, startErr error) *Application {
	t.Helper()

	admin := t.TempDir()
	cfg := testConfig(t.TempDir(), t.TempDir(), admin)

	db, err := history.Open(t.Context(), filepath.Join(admin, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	fd := newFakeDownloader()
	fd.startErr = startErr

	application, err := New(cfg, history.NewRepository(db), WithDownloader(fd))
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	return application
}

// Start creates app.ctx via context.WithCancel and stores the cancel func on
// the struct. Its failure defer resets started to false, and Shutdown returns
// early when started is false -- so on any of Start's error returns nothing
// ever invokes app.cancel, and the context node stays registered on the parent
// until the parent itself is cancelled.
//
// This matters because Start's own doc comment commits to the error path being
// retryable, and each retry overwrites app.cancel, discarding the previous one
// entirely. Repeated failed starts therefore accumulate context nodes that can
// never be cancelled.
func TestStart_CancelsContextOnFailure(t *testing.T) {
	wantErr := errors.New("downloader boom")
	application := newFailingStartApp(t, wantErr)

	if err := application.Start(t.Context()); !errors.Is(err, wantErr) {
		t.Fatalf("Start error = %v, want %v", err, wantErr)
	}

	if application.ctx == nil {
		t.Fatal("app.ctx is nil; the test is not reaching the code path it claims to cover")
	}

	select {
	case <-application.ctx.Done():
		// Cancelled as it should be.
	default:
		t.Error("app.ctx is still live after Start failed: cancel was never invoked, " +
			"so the context node leaks on the parent until the parent is cancelled")
	}
}

// A failed Start must leave the object clean rather than half-armed: started
// back to false, so nothing downstream that gates on it (Shutdown,
// ReloadDownloader, StatusInfo) mistakes a failed startup for a live one.
//
// This deliberately does not assert that a second Start succeeds. Start is not
// retryable — see its doc comment — and no caller retries; both production
// call sites surface the error and exit. An earlier version of this test did
// assert retry, which happened to pass only because the injected failure lands
// at the downloader, upstream of PostProcessor.Start. A failure after that
// point cannot be retried at all, since postproc has no reset for its own
// started flag.
func TestStart_LeavesCleanStateAfterFailure(t *testing.T) {
	application := newFailingStartApp(t, errors.New("downloader boom"))

	if err := application.Start(t.Context()); err == nil {
		t.Fatal("first Start unexpectedly succeeded")
	}
	if application.started.Load() {
		t.Error("started should be reset to false after a failed Start, or a " +
			"failed startup looks live to every caller that gates on it")
	}
}

// Shutdown's early return on !started is what makes the leak possible; pin it
// so the fix cannot be quietly undone by relaxing that guard instead.
func TestShutdown_NoOpsWhenStartFailed(t *testing.T) {
	application := newFailingStartApp(t, errors.New("downloader boom"))

	if err := application.Start(t.Context()); err == nil {
		t.Fatal("Start unexpectedly succeeded")
	}
	if err := application.Shutdown(); err != nil {
		t.Errorf("Shutdown after a failed Start = %v, want nil", err)
	}
	// The context must be cancelled by Start's own cleanup, not by Shutdown.
	if application.ctx.Err() == nil {
		t.Error("context still live: Start's failure path did not cancel it, and " +
			"Shutdown cannot compensate because its started guard returns early")
	}
}
