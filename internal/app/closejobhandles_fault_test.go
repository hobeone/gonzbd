package app

import (
	"context"
	"errors"
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

// TestEnqueuePostProc_APermanentCloseFaultFailsTheRun: a permanent storage
// fault from the hand-off's handle close becomes the run's failure reason, so
// the stages skip and the job is filed Failed with it (R20). The fault is
// joined behind a retryable one, as the close arm joins one error per file.
func TestEnqueuePostProc_APermanentCloseFaultFailsTheRun(t *testing.T) {
	t.Parallel()
	retryable := storagefault.Classify("sync", "/mnt/dl/a.bin", syscall.ENOSPC)
	permanent := storagefault.Classify("close", "/mnt/dl/b.bin", syscall.EROFS)

	status, failMsg, runs := closeFaultRun(t, errors.Join(retryable, permanent))

	if want := "Failed: " + permanent.Error(); status != "Failed" || failMsg != want {
		t.Errorf("history Status, FailMessage = %q, %q, want Failed, %q", status, failMsg, want)
	}
	if runs != 0 {
		t.Errorf("the stages ran %d times for a run the close fault failed, want 0", runs)
	}
}

// TestEnqueuePostProc_ACloseTimeoutRunsTheStages: a close that does not finish
// within its budget is logged, and the run goes on as an ordinary one. So does
// a retryable fault.
func TestEnqueuePostProc_ACloseTimeoutRunsTheStages(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "retryable fault", err: storagefault.Classify("sync", "/mnt/dl/a.bin", syscall.ENOSPC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, failMsg, runs := closeFaultRun(t, tc.err)
			if status != "Completed" || failMsg != "" {
				t.Errorf("history Status, FailMessage = %q, %q, want Completed with no message", status, failMsg)
			}
			if runs != 1 {
				t.Errorf("the stages ran %d times, want 1", runs)
			}
		})
	}
}

// TestPermanentFaultIn finds a permanent fault wherever it sits in the tree.
func TestPermanentFaultIn(t *testing.T) {
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
		{name: "retryable alone", err: retryable, want: nil},
		{name: "permanent alone", err: permanent, want: permanent},
		{name: "wrapped", err: errors.Join(errors.New("ctx"), errors.Join(retryable, permanent)), want: permanent},
	} {
		if got := permanentFaultIn(tc.err); got != tc.want {
			t.Errorf("%s: permanentFaultIn = %v, want %v", tc.name, got, tc.want)
		}
	}
}
