package app_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// removeBudget bounds each RemoveJob call below dispatcher.Remove's own 30 s,
// so a stranded launch claim fails the test in seconds rather than minutes.
const removeBudget = 5 * time.Second

// cancelHonouringStage stands in for an unpack or a move: it reports which job
// it started, then returns once its context is cancelled and, if release is
// non-nil, once the test closes release.
type cancelHonouringStage struct {
	entered chan string
	release chan struct{}
}

func (cancelHonouringStage) Name() string { return "cancel-honouring" }

func (s cancelHonouringStage) Run(ctx context.Context, j *postproc.Job) error {
	s.entered <- j.JobID()
	<-ctx.Done()
	if s.release != nil {
		<-s.release
	}
	return ctx.Err()
}

// startPostProcApp seeds one completed job per id in state, each with a
// non-empty download directory (an empty one skips every stage), and starts
// an application whose only post-processing stage is stage.
func startPostProcApp(t *testing.T, state job.State, stage postproc.Stage, ids ...string) *app.Application {
	t.Helper()
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)
	for _, id := range ids {
		name := "pp-" + id
		seedCompletedJob(t, repo, adminDir, id, name, state)
		dir := filepath.Join(downloadDir, name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "recovery.bin"), make([]byte, 100), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := testConfig(downloadDir, completeDir, adminDir, nntptest.New(t).ServerConfig("pp", 1))
	a, err := app.New(cfg, repo, app.WithPostProcStages([]postproc.Stage{stage}))
	if err != nil {
		t.Fatal(err)
	}
	_, cancel := startAppAndDrain(t, a)
	t.Cleanup(func() { cancel(); _ = a.Shutdown() })
	return a
}

func awaitEntered(t *testing.T, entered <-chan string) string {
	t.Helper()
	select {
	case id := <-entered:
		return id
	case <-time.After(10 * time.Second):
		t.Fatal("no post-processing stage started")
		return ""
	}
}

func removeWithin(t *testing.T, a *app.Application, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), removeBudget)
	defer cancel()
	if err := a.RemoveJob(ctx, id, false); err != nil {
		t.Fatalf("RemoveJob(%s): %v", id, err)
	}
	if _, ok := a.Dispatcher().Row(id); ok {
		t.Errorf("job %s is still registered after RemoveJob returned", id)
	}
}

// TestRemoveJob_ReleasesARunningPostProcessingJob: removing a job whose
// extraction or finalization is running returns once the stage has stopped,
// rather than waiting out dispatcher.Remove's budget on a launch claim that
// nothing releases.
func TestRemoveJob_ReleasesARunningPostProcessingJob(t *testing.T) {
	for _, state := range []job.State{job.Extracting, job.Finalizing} {
		t.Run(state.String(), func(t *testing.T) {
			t.Parallel()
			const id = "feedface00584a01"
			stage := cancelHonouringStage{entered: make(chan string, 1)}
			a := startPostProcApp(t, state, stage, id)
			awaitEntered(t, stage.entered)
			removeWithin(t, a, id)
		})
	}
}

// TestRemoveJob_ReleasesAQueuedPostProcessingJob: removing a job still waiting
// in the post-processing queue behind another returns promptly too.
func TestRemoveJob_ReleasesAQueuedPostProcessingJob(t *testing.T) {
	t.Parallel()
	ids := []string{"feedface00584b01", "feedface00584b02"}
	stage := cancelHonouringStage{entered: make(chan string, len(ids))}
	a := startPostProcApp(t, job.Extracting, stage, ids...)
	running := awaitEntered(t, stage.entered)
	queued := ids[0]
	if running == queued {
		queued = ids[1]
	}
	if !waitUntil(10*time.Second, func() bool { return a.PostProcessorHas(queued) }) {
		t.Fatalf("job %s never reached the post-processing queue", queued)
	}
	removeWithin(t, a, queued)
}

// TestRemoveJob_WaitsForTheCancelledStageToStop: RemoveJob does not return,
// and so does not tear the job down, while the cancelled stage is still
// running. The launch claim is what holds it back, so it must not be released
// before the stage returns.
func TestRemoveJob_WaitsForTheCancelledStageToStop(t *testing.T) {
	t.Parallel()
	const id = "feedface00584c01"
	stage := cancelHonouringStage{entered: make(chan string, 1), release: make(chan struct{})}
	a := startPostProcApp(t, job.Extracting, stage, id)
	// Registered after startPostProcApp's Shutdown, so it runs first: a failing
	// test must still let the held stage return.
	var releaseOnce sync.Once
	releaseStage := func() { releaseOnce.Do(func() { close(stage.release) }) }
	t.Cleanup(releaseStage)
	awaitEntered(t, stage.entered)

	ctx, cancel := context.WithTimeout(t.Context(), removeBudget)
	defer cancel()
	removed := make(chan error, 1)
	go func() { removed <- a.RemoveJob(ctx, id, false) }()

	select {
	case err := <-removed:
		t.Fatalf("RemoveJob returned (err = %v) while the cancelled stage was still running", err)
	case <-time.After(300 * time.Millisecond):
	}
	releaseStage()
	select {
	case err := <-removed:
		if err != nil {
			t.Fatalf("RemoveJob: %v", err)
		}
	case <-time.After(removeBudget + time.Second):
		t.Fatal("RemoveJob did not return after the stage stopped")
	}
}
