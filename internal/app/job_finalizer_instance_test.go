package app

import (
	"context"
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// TestPersistAndCommit_LeavesALaterInstanceAlone: finalizing a job that left
// the dispatcher without a RemoveJob mark, after a new instance was registered
// under the same ID, must not cancel, park or deregister that new instance,
// and must not file the earlier run in history under the shared ID.
func TestPersistAndCommit_LeavesALaterInstanceAlone(t *testing.T) {
	app, repo, _ := newLifecycleTestApp(t)
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
	err := app.finalizer.persistAndCommit(app.log, buildHistoryEntry(ppJob), ppJob)
	if !errors.Is(err, errFinalizedJobSuperseded) {
		t.Errorf("persistAndCommit = %v, want errFinalizedJobSuperseded", err)
	}

	got, ok := app.dispatcher.Job(id)
	if !ok || got != j2 {
		t.Fatalf("dispatcher.Job(%s) = (%p, %v) after finalizing the first instance, want the second instance %p still registered", id, got, ok, j2)
	}
	if intent := j2.Intent(); intent == job.IntentCancel {
		t.Errorf("the second instance has intent %v: finalizing the first instance cancelled it", intent)
	}
	if _, err := repo.Get(context.Background(), id); !errors.Is(err, history.ErrNotFound) {
		t.Errorf("history.Get(%s) err = %v, want ErrNotFound: the first instance's run was filed under the second's ID", id, err)
	}
}
