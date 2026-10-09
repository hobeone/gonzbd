package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	dispatchstore "github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
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

// stopHeldDispatcher stops d in the order Application.Shutdown uses: Pause, so
// the ticker launches nothing new, Yielded for each listed job, then Stop. It
// yields every listed job, where Shutdown yields only the states whose worker
// stopped cleanly. holdingRunner never yields, so a job no test hands off or
// removes still holds its launch claim at cleanup, and a bare Stop waits for it
// up to perJobTimeout (3s) per job within Stop's overall budget. A yield before
// the Pause can be undone: the ticker launches the job again and the runner
// takes the claim back.
func stopHeldDispatcher(d *dispatch.Dispatcher) {
	d.Pause()
	for _, row := range d.List() {
		_ = d.Yielded(row.ID) // ErrNotFound for a job gone from the registry; ignored
	}
	_ = d.Stop()
}

// repairingJob builds a one-file job whose attempt stands at Repairing. Its
// file is Complete, as a job's are once it leaves Fetching.
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
	if err := j.MarkFileComplete(0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
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

// gatedStage reports which job it started, then returns ctx.Err() once its
// context is cancelled, or nil once the test closes finish.
type gatedStage struct {
	entered chan string
	finish  chan struct{}
}

func (gatedStage) Name() string { return "gated" }

func (s gatedStage) Run(ctx context.Context, j *postproc.Job) error {
	s.entered <- j.JobID()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.finish:
		return nil
	}
}

// heldRepairingApp starts an application whose post-processor runs only stage,
// and launches one Repairing job under a holdingRunner, so the job holds its
// launch claim and the post-processor has not been given it. The returned
// postproc.Job is what handing it over would enqueue.
func heldRepairingApp(t *testing.T, stage postproc.Stage) (*Application, *job.Job, *postproc.Job) {
	t.Helper()
	application, repo, _ := newLifecycleTestApp(t, WithPostProcStages([]postproc.Stage{stage}))
	// persistAndCommit and finalize derive their deadlines from app.ctx,
	// which only Start sets.
	application.ctx = t.Context()
	runner := holdingRunner{launched: make(chan string, 1)}
	d := dispatch.New(
		1, 1, 10*time.Millisecond, time.Now,
		&appWorkers{app: application},
		application.residency,
		dispatchstore.New(repo.DB(), nil),
		runner,
	)
	application.dispatcher = d
	application.pipeline.dispatcher = d

	j, hdr := repairingJob(t, application, "held")
	if err := d.Add(t.Context(), j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { stopHeldDispatcher(d) })
	if err := application.postProcessor.Start(t.Context()); err != nil {
		t.Fatalf("postProcessor.Start: %v", err)
	}
	// Registered after d.Stop, so it runs first and a stage the test left
	// blocked returns before the dispatcher stops.
	t.Cleanup(func() { _ = application.postProcessor.Stop() })

	select {
	case <-runner.launched:
	case <-time.After(10 * time.Second):
		t.Fatal("the Repairing job was never launched")
	}
	// A non-empty download directory: an empty one skips every stage.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "held.bin"), make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	return application, j, &postproc.Job{Job: j, DownloadDir: dir}
}

func awaitStage(t *testing.T, entered <-chan string) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the post-processing stage never started")
	}
}

func removeWithinBudget(t *testing.T, application *Application, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 1*time.Second)
	defer cancel()
	if err := application.RemoveJob(ctx, id, false); err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}
	if _, ok := application.dispatcher.Job(id); ok {
		t.Error("the job is still registered after RemoveJob returned")
	}
}

// TestRemoveJob_CancelsAJobThatReachesPostProcessingBetweenItsCancels: a job
// the runner hands to the post-processor while RemoveJob is between its two
// cancels is still stopped, and RemoveJob returns. Whichever cancel comes
// second has to see it, so the dispatcher's must come first: had the job been
// in post-processing when the abort looked, the abort would have left its
// claim to post-processing, and with the post-processing cancel already past,
// nothing would end the stage that claim waits on.
func TestRemoveJob_CancelsAJobThatReachesPostProcessingBetweenItsCancels(t *testing.T) {
	t.Parallel()
	stage := gatedStage{entered: make(chan string, 1), finish: make(chan struct{})}
	application, j, ppJob := heldRepairingApp(t, stage)
	application.removeCancelGapHook = func(string) {
		application.postProcessor.Process(ppJob)
		awaitStage(t, stage.entered)
	}
	removeWithinBudget(t, application, j.ID())
}

// TestRemoveJob_ReleasesAJobPostProcessingFinishesAfterTheAbort: when the abort
// leaves the claim to post-processing and the job then finishes rather than
// being cancelled, RemoveJob still returns. Two releases cover this, so no one
// mutation fails it: the finalizer's (persistAndCommit), and the abort of
// dispatcher.Remove's own cancel, which yields once post-processing has let
// the job go.
func TestRemoveJob_ReleasesAJobPostProcessingFinishesAfterTheAbort(t *testing.T) {
	t.Parallel()
	stage := gatedStage{entered: make(chan string, 1), finish: make(chan struct{})}
	application, j, ppJob := heldRepairingApp(t, stage)
	application.postProcessor.Process(ppJob)
	awaitStage(t, stage.entered)
	// Past the dispatcher cancel, whose abort found the job in
	// post-processing. Finishing the stage and waiting for the worker to let
	// go leaves the post-processing cancel nothing to take.
	application.removeCancelGapHook = func(string) {
		close(stage.finish)
		waitFor(t, func() bool { return !application.postProcessor.HasJob(j) })
	}
	removeWithinBudget(t, application, j.ID())
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
		dispatchstore.New(repo.DB(), nil),
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
	t.Cleanup(func() { stopHeldDispatcher(d) })

	select {
	case <-runner.launched:
	case <-time.After(10 * time.Second):
		t.Fatal("the Repairing job was never launched")
	}
	if application.postProcessor.HasJob(j) {
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
