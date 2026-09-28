package app

import (
	"context"
	"testing"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// TestPersistAndCommit_LeavesALaterInstanceAlone: finalizing a job that left
// the dispatcher without a RemoveJob mark, after a new instance was registered
// under the same ID, must not cancel, park or deregister that new instance.
func TestPersistAndCommit_LeavesALaterInstanceAlone(t *testing.T) {
	app := newTestApplication(t)
	const id = "feedface00590a01"

	j1 := job.New(id, "first", job.Policy{})
	if err := app.dispatcher.Add(context.Background(), j1, dispatch.Header{Name: "first"}); err != nil {
		t.Fatalf("Add(j1): %v", err)
	}
	if err := app.dispatcher.Remove(context.Background(), id); err != nil {
		t.Fatalf("Remove(j1): %v", err)
	}
	j2 := job.New(id, "second", job.Policy{})
	if err := app.dispatcher.Add(context.Background(), j2, dispatch.Header{Name: "second"}); err != nil {
		t.Fatalf("Add(j2): %v", err)
	}

	ppJob := &postproc.Job{Job: j1}
	_ = app.finalizer.persistAndCommit(app.log, buildHistoryEntry(ppJob), ppJob)

	got, ok := app.dispatcher.Job(id)
	if !ok || got != j2 {
		t.Fatalf("dispatcher.Job(%s) = (%p, %v) after finalizing the first instance, want the second instance %p still registered", id, got, ok, j2)
	}
	if intent := j2.Intent(); intent == job.IntentCancel {
		t.Errorf("the second instance has intent %v: finalizing the first instance cancelled it", intent)
	}
}
