package app

import (
	"context"
	"slices"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// stallAdmissionOutcome is what one TestStall_Admission row observed: the
// stall record, the job's intent right after the fault was routed, and where
// the fault's reason went.
type stallAdmissionOutcome struct {
	stallReason string
	intent      job.Intent
	// admitted is whether the instance holds an admission once the fault
	// was routed.
	admitted bool
	// notes are the admission's notes, read while it is still held.
	notes []string
	// status and failMsg are the history entry's, for a row whose run
	// finished.
	status, failMsg string
}

// TestStall_Admission: a storage fault routed through Stall or
// parkVerifyFault parks a job that is not handed off, and is reported to the
// admitted run, with no stall record, once the job is admitted. Once the
// instance's admission has ended it is offered to no run. The instance is
// paused in every case. A record left behind would report a job in
// post-processing as stalled, and its re-evaluation would resume the ID,
// which a retry may hold by then (handOverStall).
func TestStall_Admission(t *testing.T) {
	t.Parallel()
	fault := storagefault.Classify("write", "/mnt/dl/a.bin", syscall.ENOSPC)
	reason := faultReason(fault)
	stall := func(app *Application, j *job.Job) { app.Stall(j, fault) }
	verify := func(app *Application, j *job.Job) { app.parkVerifyFault(j, fault) }

	// observe reads a row's outcome from an application whose run, if any,
	// has not finished.
	observe := func(application *Application, j *job.Job) stallAdmissionOutcome {
		return stallAdmissionOutcome{
			stallReason: application.StallReason(j.ID()).Reason,
			intent:      j.Intent(),
			admitted:    application.postProcAdmissions.has(j),
			notes:       application.postProcAdmissions.closeAndTakeNotes(j),
		}
	}

	// afterHopeless admits the job through finalizeHopeless, whose own reason
	// becomes the run's, and then routes the fault through route.
	afterHopeless := func(route func(*Application, *job.Job)) func(*testing.T) stallAdmissionOutcome {
		return func(t *testing.T) stallAdmissionOutcome {
			application, j := newDurabilityTestApp(t, 1, 2)
			application.finalizeHopeless(j.ID(), hopelessHealthGate)
			if !application.postProcAdmissions.has(j) {
				t.Fatal("precondition: the hopeless hand-over did not admit the job")
			}
			route(application, j)
			return observe(application, j)
		}
	}

	// afterEnded routes the fault through route once the instance's admission
	// has ended.
	afterEnded := func(route func(*Application, *job.Job)) func(*testing.T) stallAdmissionOutcome {
		return func(t *testing.T) stallAdmissionOutcome {
			application, j := newDurabilityTestApp(t, 1, 2)
			if got := application.postProcAdmissions.admit(j, ""); got != admitted {
				t.Fatalf("precondition: admit = %v, want admitted", got)
			}
			application.postProcAdmissions.release(j)
			route(application, j)
			return observe(application, j)
		}
	}

	// inTheWindow routes the fault through route between enqueuePostProc's
	// admit and its beginHandOver, from closeJobHandlesHook, as a fault from
	// a write in flight at the hand-over lands, and returns once the run has
	// been filed.
	inTheWindow := func(route func(*Application, *job.Job)) func(*testing.T) stallAdmissionOutcome {
		return func(t *testing.T) stallAdmissionOutcome {
			stage := gatedStage{entered: make(chan string, 4), finish: make(chan struct{})}
			close(stage.finish)
			application, j := admittedApp(t, stage)
			id := j.ID()
			var out stallAdmissionOutcome
			application.closeJobHandlesHook = func(context.Context, string) error {
				route(application, j)
				out.intent = j.Intent()
				return nil
			}
			application.maybeFinalizeJob(j, "")
			awaitFinalized(t, application, id)
			if n := len(stage.entered); n != 0 {
				t.Errorf("the stages ran %d times for a run the fault failed, want 0", n)
			}
			entry := historyEntry(t, application, id)
			out.stallReason = application.StallReason(id).Reason
			out.status, out.failMsg = entry.Status, entry.FailMessage
			return out
		}
	}

	for _, tc := range []struct {
		name string
		run  func(*testing.T) stallAdmissionOutcome
		// parked: the job has a stall record.
		parked bool
		// admitted: the instance holds an admission after the fault.
		admitted bool
		// noted: the fault's reason is one of the admission's notes.
		noted bool
		// failed: the run finished Failed with the fault's reason.
		failed bool
	}{
		{
			name: "before admission the job parks",
			run: func(t *testing.T) stallAdmissionOutcome {
				application, j := newDurabilityTestApp(t, 1, 2)
				stall(application, j)
				return observe(application, j)
			},
			parked: true,
		},
		{
			name:     "a write fault after an admission with its own reason is noted",
			run:      afterHopeless(stall),
			admitted: true,
			noted:    true,
		},
		{
			name:     "a verify fault after an admission with its own reason is noted",
			run:      afterHopeless(verify),
			admitted: true,
			noted:    true,
		},
		{
			name:   "a write fault between the admission and its seal fails the run",
			run:    inTheWindow(stall),
			failed: true,
		},
		{
			name:   "a verify fault between the admission and its seal fails the run",
			run:    inTheWindow(verify),
			failed: true,
		},
		{
			// The dispatcher still holds an instance whose admission ended
			// when the finalizer's removal failed.
			name: "a write fault after the admission ended reaches no run",
			run:  afterEnded(stall),
		},
		{
			name: "a verify fault after the admission ended reaches no run",
			run:  afterEnded(verify),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.run(t)
			if recorded := got.stallReason != ""; recorded != tc.parked {
				t.Errorf("StallReason = %q, want a record: %v", got.stallReason, tc.parked)
			}
			// Every row pauses, record or not: a refused verify fault relies on
			// the pause to stop reconcileResidency re-hydrating the job.
			if got.intent != job.IntentPause {
				t.Errorf("intent after the fault = %v, want IntentPause", got.intent)
			}
			if got.admitted != tc.admitted {
				t.Errorf("admitted after the fault = %v, want %v", got.admitted, tc.admitted)
			}
			if noted := slices.Contains(got.notes, reason); noted != tc.noted {
				t.Errorf("admission notes = %q, want %q among them: %v", got.notes, reason, tc.noted)
			}
			if tc.failed && (got.status != "Failed" || got.failMsg != reason) {
				t.Errorf("history Status, FailMessage = %q, %q, want Failed, %q", got.status, got.failMsg, reason)
			}
		})
	}
}
