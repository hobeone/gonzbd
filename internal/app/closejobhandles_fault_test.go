package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// closeFaultRun hands a held job to post-processing with its handle close
// answering closeErr, and returns its history entry and how many times the
// stages ran.
func closeFaultRun(t *testing.T, closeErr error) (status, failMsg string, stageRuns int) {
	t.Helper()
	stage := gatedStage{entered: make(chan string, 4), finish: make(chan struct{})}
	close(stage.finish)
	application, j := admittedApp(t, stage)
	id := j.ID()
	application.closeJobHandlesHook = func(context.Context, string) error { return closeErr }

	application.maybeFinalize(id, "")
	awaitFinalized(t, application, id)

	entry := historyEntry(t, application, id)
	return entry.Status, entry.FailMessage, len(stage.entered)
}

// TestEnqueuePostProc_ACloseFaultFailsTheRun: any storage fault — permanent
// or retryable — from the hand-off's handle close becomes the run's failure
// reason, so the stages skip and the job is filed Failed with it. The close
// arm tombstones the handle either way, so nothing ever retries a fault
// observed here, and the lost bytes would otherwise reach par2/unrar as a
// hole.
func TestEnqueuePostProc_ACloseFaultFailsTheRun(t *testing.T) {
	t.Parallel()
	retryable := storagefault.Classify("sync", "/mnt/dl/a.bin", syscall.ENOSPC)
	permanent := storagefault.Classify("close", "/mnt/dl/b.bin", syscall.EROFS)
	for _, tc := range []struct {
		name string
		err  error
		want *storagefault.Fault
	}{
		{name: "permanent alone", err: permanent, want: permanent},
		{name: "retryable alone", err: retryable, want: retryable},
		{name: "joined, retryable first", err: errors.Join(retryable, permanent), want: retryable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, failMsg, runs := closeFaultRun(t, tc.err)
			if want := "Failed: " + tc.want.Error(); status != "Failed" || failMsg != want {
				t.Errorf("history Status, FailMessage = %q, %q, want Failed, %q", status, failMsg, want)
			}
			if runs != 0 {
				t.Errorf("the stages ran %d times for a run the close fault failed, want 0", runs)
			}
		})
	}
}

// TestEnqueuePostProc_ACloseFaultIsNotedBehindAnEarlierReason: a hand-off that
// already carries a failure reason before the close runs keeps that reason as
// the run's FailMsg — the admission's failMsg is set at the very first admit,
// before CloseJobHandles is even called — and the close fault is recorded
// as a second reason, which finalize adds to the stage log's "warnings" line
// rather than the status.
func TestEnqueuePostProc_ACloseFaultIsNotedBehindAnEarlierReason(t *testing.T) {
	t.Parallel()
	stage := gatedStage{entered: make(chan string, 4), finish: make(chan struct{})}
	close(stage.finish)
	application, j := admittedApp(t, stage)
	id := j.ID()
	fault := storagefault.Classify("close", "/mnt/dl/b.bin", syscall.EROFS)
	application.closeJobHandlesHook = func(context.Context, string) error { return fault }

	application.maybeFinalize(id, "Failed: earlier")
	awaitFinalized(t, application, id)

	entry := historyEntry(t, application, id)
	if want := "Failed: earlier"; entry.Status != "Failed" || entry.FailMessage != want {
		t.Errorf("history Status, FailMessage = %q, %q, want Failed, %q", entry.Status, entry.FailMessage, want)
	}
	if n := len(stage.entered); n != 0 {
		t.Errorf("the stages ran %d times for a job already failed before the close, want 0", n)
	}
	warnings := stageLogLines(t, entry, "warnings")
	if len(warnings) != 1 || !strings.Contains(warnings[0], "Failed: "+fault.Error()) {
		t.Errorf("warnings stage lines = %q, want one naming the close fault", warnings)
	}
}

// TestEnqueuePostProc_ACloseTimeoutRunsTheStages: a close that does not finish
// within its budget, with no fault observed, is logged, and the run goes on
// as an ordinary one.
func TestEnqueuePostProc_ACloseTimeoutRunsTheStages(t *testing.T) {
	t.Parallel()
	status, failMsg, runs := closeFaultRun(t, context.DeadlineExceeded)
	if status != "Completed" || failMsg != "" {
		t.Errorf("history Status, FailMessage = %q, %q, want Completed with no message", status, failMsg)
	}
	if runs != 1 {
		t.Errorf("the stages ran %d times, want 1", runs)
	}
}

// TestFaultIn finds a fault of either kind wherever it sits in the tree.
func TestFaultIn(t *testing.T) {
	t.Parallel()
	retryable := storagefault.Classify("sync", "/p", syscall.ENOSPC)
	permanent := storagefault.Classify("close", "/p", syscall.EROFS)
	for _, tc := range []struct {
		name string
		err  error
		want *storagefault.Fault
	}{
		{name: "nil", err: nil, want: nil},
		{name: "not a fault", err: context.DeadlineExceeded, want: nil},
		{name: "retryable alone", err: retryable, want: retryable},
		{name: "permanent alone", err: permanent, want: permanent},
		{name: "wrapped, retryable first", err: errors.Join(errors.New("ctx"), errors.Join(retryable, permanent)), want: retryable},
		{name: "wrapped, permanent first", err: errors.Join(errors.New("ctx"), errors.Join(permanent, retryable)), want: permanent},
		{name: "single-wrapped", err: fmt.Errorf("ctx: %w", permanent), want: permanent},
	} {
		if got := faultIn(tc.err); got != tc.want {
			t.Errorf("%s: faultIn = %v, want %v", tc.name, got, tc.want)
		}
	}
}
