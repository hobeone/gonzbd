package app

import (
	"context"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestStall_StillParksWhenTheProcessIsNotStopping is the half that keeps the
// guard above honest. Without it, "never park" satisfies the test.
func TestStall_StillParksWhenTheProcessIsNotStopping(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	if application.stopping.Load() {
		t.Fatal("the fixture is already stopping, so it cannot observe the running case")
	}

	application.Stall(job.ID(), storagefault.Classify("sync", "/d/a.bin", syscall.ETIMEDOUT))

	row, ok := application.dispatcher.Row(job.ID())
	if !ok {
		t.Fatal("row is missing")
	}
	if row.Status() != constants.StatusPaused {
		t.Fatal("a wedged mount left the job running; it sits at N% with no reason the " +
			"operator can act on (A2)")
	}
}

func TestStopAndJoin_WaitsForWgBeforePostProcAndReportsTimeout(t *testing.T) {
	t.Parallel()

	t.Run("waits for wg before stopping post-processor", func(t *testing.T) {
		t.Parallel()
		application, _ := newDurabilityTestApp(t, 1, 2)
		application.shutdownStepTimeout = 2 * time.Second
		ctx, cancel := context.WithCancel(context.Background())
		application.ctx, application.cancel = ctx, cancel
		t.Cleanup(cancel)

		var wgFinished, wgDoneWhenPostProcStopped atomic.Bool
		application.wg.Go(func() {
			<-application.ctx.Done()
			// Delay briefly after context cancellation so a reordered
			// implementation that invokes postProcessor.Stop before wg.Wait
			// almost always enters the hook while wgFinished is still false.
			time.Sleep(20 * time.Millisecond)
			wgFinished.Store(true)
		})

		application.postProcStopHook = func() error {
			wgDoneWhenPostProcStopped.Store(wgFinished.Load())
			return nil
		}

		if err := application.stopAndJoin(); err != nil {
			t.Fatalf("stopAndJoin unexpected error: %v", err)
		}
		if !wgDoneWhenPostProcStopped.Load() {
			t.Error("post-processor stopped before context cancellation and wg.Wait finished (in joinAndStop)")
		}
	})

	t.Run("reports error when wg.Wait times out", func(t *testing.T) {
		t.Parallel()
		var zeroApp Application
		if got := zeroApp.stepTimeout(); got != defaultShutdownStepTimeout {
			t.Errorf("zeroApp.stepTimeout() = %v, want %v", got, defaultShutdownStepTimeout)
		}

		appStuck, _, _ := newLifecycleTestApp(t)
		appStuck.shutdownStepTimeout = 10 * time.Millisecond
		unblock := make(chan struct{})
		t.Cleanup(func() { close(unblock) })
		appStuck.wg.Go(func() {
			<-unblock
		})
		if err := appStuck.stopAndJoin(); err == nil {
			t.Error("stopAndJoin returned nil error when wg.Wait timed out")
		}
	})
}
