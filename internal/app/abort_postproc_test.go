package app

import (
	"context"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	dispatchstore "github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// holdingRunner takes each launch and hands the job to nobody, so the launch
// claim stays held with no post-processing behind it. enqueuePostProc leaves
// a Repairing job in that shape while it waits on a direct unpack before
// handing the job to the post-processor.
type holdingRunner struct {
	launched chan string
}

func (r holdingRunner) Run(_ context.Context, id string, _ job.State) {
	r.launched <- id
}

// repairingJob builds a one-file job whose attempt stands at Repairing.
func repairingJob(t *testing.T, application *Application, name string) (*job.Job, dispatch.Header) {
	t.Helper()
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  name + ".bin",
		Bytes:    100,
		Articles: []nzb.Article{{ID: name + "0@t", Bytes: 100, Number: 1}},
	}}}
	j, hdr, err := BuildIngestJob(application.config, parsed, name+".nzb", types.FetchOptions{NzbName: name}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := j.BeginAttempt(time.Now()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	for _, s := range []job.State{job.Assessing, job.Repairing} {
		if err := j.SetNext(s); err != nil {
			t.Fatalf("SetNext(%s): %v", s, err)
		}
		if err := j.Transition(s); err != nil {
			t.Fatalf("Transition(%s): %v", s, err)
		}
	}
	return j, hdr
}

// TestRemoveJob_ReleasesARepairingJobPostProcessingDoesNotHold: a Repairing
// job that is launched but not in post-processing has nothing that will hand
// it back later, so the abort must release its launch claim itself, or
// RemoveJob waits out dispatcher.Remove's budget and fails.
func TestRemoveJob_ReleasesARepairingJobPostProcessingDoesNotHold(t *testing.T) {
	t.Parallel()
	application, repo, _ := newLifecycleTestApp(t)
	runner := holdingRunner{launched: make(chan string, 1)}
	d := dispatch.New(
		1, 1, 10*time.Millisecond, time.Now,
		&appWorkers{app: application},
		application.residency,
		dispatchstore.New(repo.DB()),
		runner,
	)
	application.dispatcher = d
	application.pipeline.dispatcher = d

	j, hdr := repairingJob(t, application, "unheld")
	if err := d.Add(t.Context(), j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	select {
	case <-runner.launched:
	case <-time.After(10 * time.Second):
		t.Fatal("the Repairing job was never launched")
	}
	if application.postProcessor.Has(j.ID()) {
		t.Fatal("post-processing holds the job, so this would not test an unheld one")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := application.RemoveJob(ctx, j.ID(), false); err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}
	if _, ok := d.Job(j.ID()); ok {
		t.Error("the job is still registered after RemoveJob returned")
	}
}
