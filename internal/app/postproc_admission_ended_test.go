package app

import (
	"runtime"
	"testing"
	"time"
	"weak"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// TestEnqueue_AfterAFinalizeThatCouldNotRemoveTheJob_AdmitsNoSecondRun: a
// finalizer whose dispatcher removal fails leaves the job registered after its
// post-processing admission has ended. A late enqueue of that same instance
// must not start a second post-processing run, whose finalize would file the
// job in history a second time.
func TestEnqueue_AfterAFinalizeThatCouldNotRemoveTheJob_AdmitsNoSecondRun(t *testing.T) {
	t.Parallel()
	application, j, _ := newAppWithCustomDispatchStore(t, 2)
	application.ctx = t.Context()

	if got := application.postProcAdmissions.admit(j, ""); got != admitted {
		t.Fatalf("first admit = %v, want admitted", got)
	}
	application.finalizer.finalize(&postproc.Job{Job: j})
	if _, held := application.dispatcher.Job(j.ID()); !held {
		t.Fatal("the job left the dispatcher, so this would not test one still registered")
	}
	historyEntry(t, application, j.ID())

	application.enqueuePostProc(j, dispatch.Header{Name: j.Name()}, "")
	if n := admissionsHeld(&application.postProcAdmissions); n != 0 {
		t.Errorf("admissions held after a late enqueue = %d, want 0: the finalized instance was admitted to a second run", n)
	}
}

// TestEnqueue_AfterACancelledRun_AdmitsNoSecondRun: an instance whose
// post-processing a RemoveJob cancelled stays registered when that removal
// fails, and a late enqueue of it must not run its post-processing after all.
func TestEnqueue_AfterACancelledRun_AdmitsNoSecondRun(t *testing.T) {
	t.Parallel()
	application, j, _ := newAppWithCustomDispatchStore(t, 0)
	application.ctx = t.Context()

	application.postProcAdmissions.admit(j, "")
	application.finalizer.cancelled(&postproc.Job{Job: j})
	if _, held := application.dispatcher.Job(j.ID()); !held {
		t.Fatal("the job left the dispatcher, so this would not test one still registered")
	}

	application.enqueuePostProc(j, dispatch.Header{Name: j.Name()}, "")
	if n := admissionsHeld(&application.postProcAdmissions); n != 0 {
		t.Errorf("admissions held after a late enqueue = %d, want 0: the cancelled instance was admitted to a second run", n)
	}
}

// TestPostProcAdmissions_ForgetEndedDropsOnlyItsKey: the cleanup a collected
// job runs drops that job's record and no other.
func TestPostProcAdmissions_ForgetEndedDropsOnlyItsKey(t *testing.T) {
	t.Parallel()
	var a postProcAdmissions
	kept := job.New("same-id", "kept", job.Policy{})
	dropped := job.New("same-id", "dropped", job.Policy{})
	a.release(kept)
	a.release(dropped)

	a.forgetEnded(weak.Make(dropped))
	if got := a.admit(kept, ""); got != refusedEnded {
		t.Errorf("admit(kept) = %v after another job's record was dropped, want refusedEnded", got)
	}
	if got := a.admit(dropped, ""); got != admitted {
		t.Errorf("admit(dropped) = %v after its record was dropped, want admitted", got)
	}
}

// TestPostProcAdmissions_EndedForgetsACollectedJob: the record of ended
// admissions does not outlive the job, so it does not grow with every job ever
// post-processed.
func TestPostProcAdmissions_EndedForgetsACollectedJob(t *testing.T) {
	t.Parallel()
	var a postProcAdmissions
	j := job.New("gone", "gone", job.Policy{})
	a.admit(j, "")
	a.release(j)
	a.release(j)
	a.mu.Lock()
	n := len(a.ended)
	a.mu.Unlock()
	if n != 1 {
		t.Fatalf("ended holds %d entries after releasing one job twice, want 1", n)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		runtime.GC()
		a.mu.Lock()
		n := len(a.ended)
		a.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d ended admissions remain after the job was collected", n)
		}
		runtime.Gosched()
	}
}
