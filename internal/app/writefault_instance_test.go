package app

import (
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestStallAndFail_LeaveALaterInstanceAlone pins #914 at Stall and Fail: a
// storage fault of an attempt that has left the queue neither parks nor fails
// the attempt since registered under the same ID, and leaves the ID's stall
// record as that attempt's own faults made it.
func TestStallAndFail_LeaveALaterInstanceAlone(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	const id = "feedface00914a01"
	j1, j2 := retryUnderStaleID(t, application, id)

	application.Stall(j1, storagefault.Classify("sync", "/x", syscall.ENOSPC))
	if got := j2.Intent(); got != job.IntentRun {
		t.Errorf("a stale attempt's Stall left the later attempt at %v, want %v", got, job.IntentRun)
	}
	if got := application.StallReason(id).Reason; got != "" {
		t.Errorf("a stale attempt's Stall recorded %q under the later attempt's ID", got)
	}

	application.Fail(j1, storagefault.Classify("write", "/x", syscall.EROFS))
	if application.postProcAdmissions.has(j2) {
		t.Error("a stale attempt's Fail handed the later attempt to post-processing")
	}
	if row, _ := application.Dispatcher().Row(id); row.Header.FailReason != "" {
		t.Errorf("a stale attempt's Fail wrote %q as the later attempt's fail reason", row.Header.FailReason)
	}

	application.Stall(j2, storagefault.Classify("sync", "/x", syscall.ENOSPC))
	application.Fail(j1, storagefault.Classify("write", "/x", syscall.EROFS))
	if got := application.StallReason(id).Reason; got == "" {
		t.Error("a stale attempt's Fail cleared the later attempt's own stall")
	}
}

// TestHandleWriteFault_ActsOnTheAttemptRegisteredOnTheWorker pins #914 at
// handleWriteFault: the job is resolved when the fault is raised, so a retry
// registered under the same ID before the routing goroutine runs is neither
// parked nor failed.
func TestHandleWriteFault_ActsOnTheAttemptRegisteredOnTheWorker(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		fault *storagefault.Fault
	}{
		{"retryable", storagefault.Classify("sync", "/x", syscall.ENOSPC)},
		{"permanent", storagefault.Classify("write", "/x", syscall.EROFS)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			application := newTestApplication(t)
			const id = "feedface00914a02"
			d := application.Dispatcher()
			if err := d.Add(t.Context(), job.New(id, "first", job.Policy{}), dispatch.Header{Name: "first"}); err != nil {
				t.Fatalf("Add(first): %v", err)
			}
			retry := job.New(id, "second", job.Policy{})
			application.writeFaultRoutedHook = func(*job.Job) {
				if err := d.Remove(t.Context(), id); err != nil {
					t.Errorf("Remove(first): %v", err)
				}
				if err := d.Add(t.Context(), retry, dispatch.Header{Name: "second"}); err != nil {
					t.Errorf("Add(second): %v", err)
				}
			}

			application.handleWriteFault(id, 0, tc.fault)
			application.wg.Wait()

			if got := retry.Intent(); got != job.IntentRun {
				t.Errorf("the retry was left at %v, want %v", got, job.IntentRun)
			}
			if got := application.StallReason(id).Reason; got != "" {
				t.Errorf("the retry's ID carries the stall %q", got)
			}
			if application.postProcAdmissions.has(retry) {
				t.Error("the retry was handed to post-processing")
			}
			if row, _ := d.Row(id); row.Header.FailReason != "" {
				t.Errorf("the retry carries the fail reason %q", row.Header.FailReason)
			}
		})
	}
}
