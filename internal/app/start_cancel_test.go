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

// Start must remain retryable after a failure, and a retry must not be
// prevented by the cancellation added for the leak fix.
func TestStart_RetryableAfterFailure(t *testing.T) {
	application := newFailingStartApp(t, errors.New("downloader boom"))

	if err := application.Start(t.Context()); err == nil {
		t.Fatal("first Start unexpectedly succeeded")
	}
	if application.started.Load() {
		t.Error("started should be reset to false after a failed Start")
	}

	// A second attempt must not be rejected with ErrAlreadyStarted, and must
	// install a fresh, live context rather than reusing the cancelled one.
	failedCtx := application.ctx
	err := application.Start(t.Context())
	if errors.Is(err, ErrAlreadyStarted) {
		t.Fatal("Start is documented as retryable but returned ErrAlreadyStarted")
	}
	if application.ctx == failedCtx {
		t.Error("retry reused the cancelled context from the failed attempt")
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
