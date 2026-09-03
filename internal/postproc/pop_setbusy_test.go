package postproc

import (
	"context"
	"testing"

	"github.com/hobeone/gonzbd/internal/queue"
)

// TestPopJob_EmptyQueueReturnsFalseWhenWorkerContextDone pins popJob's
// early-exit path: with nothing pushed and the worker context already
// cancelled, ppQueue.Pop returns immediately with ok == false, and popJob
// must propagate that as (nil, nil, false) without touching busy state.
//
// This was previously unreferenced by any test even though it is
// PostProcessor.run's only way of noticing shutdown — check_test_alignment
// flagged it as pre-existing debt in a file this branch merely touched.
func TestPopJob_EmptyQueueReturnsFalseWhenWorkerContextDone(t *testing.T) {
	t.Parallel()

	p := New(Options{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p.workerCtx = ctx

	job, jobCtx, ok := p.popJob()
	if ok {
		t.Fatal("popJob returned ok=true on an empty queue with a cancelled worker context")
	}
	if job != nil {
		t.Errorf("popJob returned a non-nil job on failure: %+v", job)
	}
	if jobCtx != nil {
		t.Error("popJob returned a non-nil context on failure")
	}
	p.busyMu.Lock()
	busy := p.busy
	p.busyMu.Unlock()
	if busy {
		t.Error("popJob must not set busy when it returns ok=false")
	}
}

// TestPopJob_DequeuesAndMarksBusy pins the success path: a pushed job is
// returned, a jobCtx derived from workerCtx comes back independently
// cancellable, and busy/currentJobID/currentJobCancel are all set before
// popJob returns — the atomicity setBusyWithJob's own doc comment claims
// ("so Has()/Empty() never see the intermediate state").
func TestPopJob_DequeuesAndMarksBusy(t *testing.T) {
	t.Parallel()

	p := New(Options{})
	p.workerCtx, p.workerCancel = context.WithCancel(t.Context())
	t.Cleanup(p.workerCancel)

	const jobID = "pop-job-busy"
	want := &Job{Queue: &queue.Job{ID: jobID}}
	p.q.Push(want)

	got, jobCtx, ok := p.popJob()
	if !ok {
		t.Fatal("popJob returned ok=false for a queue with one job pushed")
	}
	if got != want {
		t.Errorf("popJob returned %+v, want the pushed job %+v", got, want)
	}
	if jobCtx == nil {
		t.Fatal("popJob returned a nil jobCtx on success")
	}
	if jobCtx.Err() != nil {
		t.Error("the fresh jobCtx must not already be done")
	}

	p.busyMu.Lock()
	busy, currentID, currentCancel := p.busy, p.currentJobID, p.currentJobCancel
	p.busyMu.Unlock()
	if !busy {
		t.Error("popJob must leave busy=true after a successful pop")
	}
	if currentID != jobID {
		t.Errorf("currentJobID = %q, want %q", currentID, jobID)
	}
	if currentCancel == nil {
		t.Fatal("currentJobCancel is nil after a successful pop")
	}

	// jobCtx is independently cancellable: cancelling it must not cancel
	// workerCtx, and cancelling workerCtx (via t.Cleanup above) is not what
	// this asserts — instead, calling the cancel func popJob stored must
	// cancel exactly the returned jobCtx.
	currentCancel()
	if jobCtx.Err() == nil {
		t.Error("calling the stored currentJobCancel did not cancel the jobCtx popJob returned")
	}
	if p.workerCtx.Err() != nil {
		t.Error("cancelling jobCtx must not cancel workerCtx")
	}
}

// TestSetBusyWithJob_MutatesAllThreeFieldsAtomically pins what
// setBusyWithJob actually mutates: exactly busy, currentJobID and
// currentJobCancel, read back through the same busyMu it documents itself
// as using. It is a plain three-field setter with no branching, but it is
// the sole writer of state Has/Empty/Cancel all read (see their doc
// comments), so a defect here — e.g. one field silently left stale — is not
// otherwise pinned anywhere.
func TestSetBusyWithJob_MutatesAllThreeFieldsAtomically(t *testing.T) {
	t.Parallel()

	p := New(Options{})

	cancelCalls := 0
	cancel := context.CancelFunc(func() { cancelCalls++ })

	p.setBusyWithJob(true, "job-a", cancel)
	p.busyMu.Lock()
	if !p.busy {
		t.Error("busy = false, want true after setBusyWithJob(true, ...)")
	}
	if p.currentJobID != "job-a" {
		t.Errorf("currentJobID = %q, want %q", p.currentJobID, "job-a")
	}
	if p.currentJobCancel == nil {
		t.Fatal("currentJobCancel is nil after setBusyWithJob with a non-nil cancel func")
	}
	p.busyMu.Unlock()

	// The stored cancel func must be the one passed in, not a copy or a
	// wrapper: calling it must run the original closure exactly once.
	p.currentJobCancel()
	if cancelCalls != 1 {
		t.Errorf("stored currentJobCancel ran %d times, want exactly 1", cancelCalls)
	}

	// Clearing busy must clear all three fields together, matching how
	// run() calls it: setBusyWithJob(false, "", nil) after each job.
	p.setBusyWithJob(false, "", nil)
	p.busyMu.Lock()
	defer p.busyMu.Unlock()
	if p.busy {
		t.Error("busy = true, want false after setBusyWithJob(false, \"\", nil)")
	}
	if p.currentJobID != "" {
		t.Errorf("currentJobID = %q, want empty after clearing", p.currentJobID)
	}
	if p.currentJobCancel != nil {
		t.Error("currentJobCancel must be nil after clearing")
	}
}

// TestSetBusyWithJob_ObservableThroughHasAndEmpty pins the reason
// setBusyWithJob exists at all (its own doc comment: "so Has and Cancel can
// observe the in-flight job... without racing the busy flag") by driving it
// through PostProcessor.Has and Empty rather than only reading the raw
// fields, so a change that broke that contract while leaving the fields
// themselves correct would still be caught.
func TestSetBusyWithJob_ObservableThroughHasAndEmpty(t *testing.T) {
	t.Parallel()

	p := New(Options{})
	const jobID = "observable-busy"

	if p.Has(jobID) {
		t.Fatal("fixture guard: Has must be false before setBusyWithJob runs")
	}
	if !p.Empty() {
		t.Fatal("fixture guard: Empty must be true before setBusyWithJob runs")
	}

	p.setBusyWithJob(true, jobID, func() {})
	if !p.Has(jobID) {
		t.Error("Has(jobID) = false while setBusyWithJob marked it current and busy")
	}
	if p.Empty() {
		t.Error("Empty() = true while busy is set")
	}

	p.setBusyWithJob(false, "", nil)
	if p.Has(jobID) {
		t.Error("Has(jobID) = true after setBusyWithJob cleared the current job")
	}
	if !p.Empty() {
		t.Error("Empty() = false after setBusyWithJob cleared busy and the queue is empty")
	}
}

// TestEmpty_QueueNonEmptyReturnsFalseWithoutBusy pins Empty's early-return
// branch: a job sitting in the queue makes Empty false on its own, with
// busy left at its New(Options{}) zero value (false). Every other Empty
// test in this package drives the busy path (queue empty, busy
// true/false); none pushes a job, so this branch (len(jobs) != 0 ->
// empty = false, return, before busyMu is ever touched) was previously
// unreferenced -- check_test_alignment's coverage gate flagged it as
// pre-existing debt in a file this branch merely touched.
func TestEmpty_QueueNonEmptyReturnsFalseWithoutBusy(t *testing.T) {
	t.Parallel()

	p := New(Options{})
	p.q.Push(&Job{Queue: newQueueJob(t, "queued", 0)})

	if p.Empty() {
		t.Error("Empty() = true with a job queued and busy=false, want false")
	}
}
