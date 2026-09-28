package app

import (
	"context"
	"testing"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// TestJobFinalizerCancelled_LeavesALaterInstanceAlone: a callback that arrives
// for a removed instance must not cancel the instance a retry has since
// registered under the same ID. A never-run job with cancel intent is evicted
// by the tick, so the retry would disappear.
func TestJobFinalizerCancelled_LeavesALaterInstanceAlone(t *testing.T) {
	app := newTestApplication(t)
	const id = "feedface00584d01"

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

	app.finalizer.cancelled(&postproc.Job{Job: j1})

	if got, ok := app.dispatcher.Job(id); !ok || got != j2 {
		t.Fatalf("dispatcher.Job(%s) = (%p, %v), want the second instance %p", id, got, ok, j2)
	}
	if intent := j2.Intent(); intent == job.IntentCancel {
		t.Errorf("the second instance has intent %v: a callback for the removed first instance cancelled it", intent)
	}
}
