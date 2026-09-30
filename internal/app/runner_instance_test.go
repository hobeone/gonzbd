package app

import (
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
)

// retryUnderStaleID registers j1, removes it, and registers j2 under the same
// ID with an open attempt, standing in for a retry registered while a runner
// still holds j1. It returns both.
func retryUnderStaleID(t *testing.T, application *Application, id string) (j1, j2 *job.Job) {
	t.Helper()
	d := application.Dispatcher()
	j1 = job.New(id, "first", job.Policy{})
	if err := d.Add(t.Context(), j1, dispatch.Header{Name: "first"}); err != nil {
		t.Fatalf("Add(j1): %v", err)
	}
	if err := d.Remove(t.Context(), id); err != nil {
		t.Fatalf("Remove(j1): %v", err)
	}
	j2 = job.New(id, "second", job.Policy{})
	if err := d.Add(t.Context(), j2, dispatch.Header{Name: "second"}); err != nil {
		t.Fatalf("Add(j2): %v", err)
	}
	if err := j2.BeginAttempt(time.Now()); err != nil {
		t.Fatalf("BeginAttempt(j2): %v", err)
	}
	return j1, j2
}

// TestFailHopeless_LeavesALaterInstanceAlone: runAssess's hopeless verdict for
// an instance that has left the dispatcher must neither hand to
// post-processing nor settle a later instance registered under the same ID.
func TestFailHopeless_LeavesALaterInstanceAlone(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	r := newAppRunner(application)
	r.report = application.Dispatcher()
	const id = "feedface00656a01"
	j1, j2 := retryUnderStaleID(t, application, id)

	r.failHopeless(j1)

	if got, ok := application.Dispatcher().Job(id); !ok || got != j2 {
		t.Fatalf("dispatcher.Job(%s) = (%p, %v) after the first instance's hopeless verdict, want the second instance %p", id, got, ok, j2)
	}
	if application.postProcAdmissions.has(j2) {
		t.Error("the first instance's hopeless verdict admitted the second instance to post-processing")
	}
	if application.postProcAdmissions.has(j1) {
		t.Error("the first instance was admitted to post-processing after it left the dispatcher")
	}
	if s := j2.Snapshot(); s.State.Outcome.IsSettled() {
		t.Errorf("the first instance's hopeless verdict settled the second instance: outcome %v", s.State.Outcome)
	}
}

// TestFailHopeless_HandsOverAndSettlesItsOwnInstance: the verdict for the
// registered instance still hands it to post-processing and settles it.
func TestFailHopeless_HandsOverAndSettlesItsOwnInstance(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	r := newAppRunner(application)
	r.report = application.Dispatcher()
	const id = "feedface00656a02"
	_, j2 := retryUnderStaleID(t, application, id)

	r.failHopeless(j2)

	if !application.postProcAdmissions.has(j2) {
		t.Error("the hopeless verdict did not admit its own instance to post-processing")
	}
	if s := j2.Snapshot(); s.State.Outcome != job.OutcomeFailed {
		t.Errorf("outcome after the hopeless verdict = %v, want %v", s.State.Outcome, job.OutcomeFailed)
	}
}
