package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/dispatch"
	dispatchstore "github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
)

// postProcStates are the states a launched job hands to enqueuePostProc from.
// A dispatcher cancel interrupts only the first, so only there does
// appWorkers.Abort release the launch claim.
var postProcStates = []job.State{job.Repairing, job.Extracting, job.Finalizing}

// jobAt builds a one-file job whose attempt stands at state. Past Fetching its
// file is Complete, as a job's are once it leaves Fetching; at Fetching it is
// not, so the job's download is unfinished.
func jobAt(t *testing.T, application *Application, name string, state job.State) (*job.Job, dispatch.Header) {
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
	if state == job.Fetching {
		return j, hdr
	}
	if err := j.MarkFileComplete(0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}
	step := func(s job.State) {
		t.Helper()
		if err := j.SetNext(s); err != nil {
			t.Fatalf("SetNext(%s): %v", s, err)
		}
		if err := j.Transition(s); err != nil {
			t.Fatalf("Transition(%s): %v", s, err)
		}
	}
	step(job.Assessing)
	if state == job.Repairing {
		step(job.Repairing)
		return j, hdr
	}
	if err := j.SetNext(job.Extracting); err != nil {
		t.Fatalf("SetNext(Extracting): %v", err)
	}
	if _, err := j.Cross(job.Extracting); err != nil {
		t.Fatalf("Cross(Extracting): %v", err)
	}
	if state == job.Finalizing {
		step(job.Finalizing)
	}
	return j, hdr
}

// launchedAppAt is admittedApp with the job at state: launched under a
// holdingRunner, so it holds its launch claim and the post-processor has not
// been given it, with a non-empty download directory at the path
// enqueuePostProc derives.
func launchedAppAt(t *testing.T, stage postproc.Stage, state job.State) (*Application, *job.Job) {
	t.Helper()
	application, repo, _ := newLifecycleTestApp(t, WithPostProcStages([]postproc.Stage{stage}))
	application.ctx = t.Context()
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

	j, hdr := jobAt(t, application, "held", state)
	if err := d.Add(t.Context(), j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })
	if err := application.postProcessor.Start(t.Context()); err != nil {
		t.Fatalf("postProcessor.Start: %v", err)
	}
	t.Cleanup(func() { _ = application.postProcessor.Stop() })
	select {
	case <-runner.launched:
	case <-time.After(10 * time.Second):
		t.Fatalf("the %s job was never launched", state)
	}
	dir := filepath.Join(application.config.GetGeneral().DownloadDir, j.Name())
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "held.bin"), make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	return application, j
}

// finishedStage is a gatedStage that returns as soon as it starts.
func finishedStage() gatedStage {
	stage := gatedStage{entered: make(chan string, 4), finish: make(chan struct{})}
	close(stage.finish)
	return stage
}

// waitingDirectUnpack registers a DirectUnpacker for id that has the first of
// two volumes and waits for the second, which never arrives.
func waitingDirectUnpack(t *testing.T, application *Application, id string) *directunpack.DirectUnpacker {
	t.Helper()
	rarData, err := os.ReadFile("../unpack/testdata/multi_new.part01.rar")
	if err != nil {
		t.Fatalf("read test archive: %v", err)
	}
	duDir := t.TempDir()
	rarPath := filepath.Join(duDir, "multi_new.part01.rar")
	if err := os.WriteFile(rarPath, rarData, 0o600); err != nil {
		t.Fatal(err)
	}
	du := directunpack.New(slog.New(slog.DiscardHandler), id, duDir, t.TempDir(), directunpack.Options{})
	du.SetAllFilenames([]string{"multi_new.part01.rar", "multi_new.part02.rar"})
	du.Add(t.Context(), "multi_new.part01.rar", rarPath)
	t.Cleanup(du.Abort)
	application.duOrch.inject(id, du)
	return du
}

// awaitAdmissionsEnded waits up to five seconds for every post-processing
// admission to end.
func awaitAdmissionsEnded(t *testing.T, application *Application) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for admissionsHeld(&application.postProcAdmissions) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the job's post-processing admission never ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// assertNeverPostProcessed fails if the post-processor was given the job.
func assertNeverPostProcessed(t *testing.T, application *Application, stage gatedStage) {
	t.Helper()
	if !application.postProcessor.Empty() {
		t.Error("the post-processor holds the removed job")
	}
	if n := len(application.postProcessor.History()); n != 0 {
		t.Errorf("the post-processor ran %d copies of the removed job, want 0", n)
	}
	if n := len(stage.entered); n != 0 {
		t.Errorf("the stages ran %d times for the removed job, want 0", n)
	}
}

// TestRemoveJob_DuringTheDirectUnpackWait_PostProcessingDoesNotRun: a job
// removed while its admitted enqueue waits for its DirectUnpack is not handed
// to the post-processor, and RemoveJob returns with the job deregistered, in
// every state a launch hands to enqueuePostProc from.
func TestRemoveJob_DuringTheDirectUnpackWait_PostProcessingDoesNotRun(t *testing.T) {
	t.Parallel()
	for _, state := range postProcStates {
		t.Run(state.String(), func(t *testing.T) {
			t.Parallel()
			stage := finishedStage()
			application, j := launchedAppAt(t, stage, state)
			id := j.ID()
			waitingDirectUnpack(t, application, id)

			application.maybeFinalize(id, "")
			removeWithinBudget(t, application, id)
			awaitAdmissionsEnded(t, application)

			assertNeverPostProcessed(t, application, stage)
		})
	}
}

// TestRemoveJob_WaitsForTheDirectUnpackToStop: RemoveJob deletes nothing
// while the DirectUnpacker the enqueue collected still runs; duOrch.abortJob
// cannot reach it, so the removal waits for the enqueue's wait to abort it.
//
// It checks du.Done() from two hooks, because each pins a different way that
// guarantee can break and neither alone is both sound and complete:
//
//   - removeJobHook fires from RemoveJob's own goroutine, once withdraw
//     returns. It catches a withdraw that returns before du has stopped for
//     any reason reachable from RemoveJob's side, including a step ended
//     before the DirectUnpack wait ever begins. But withdraw returning is
//     not RemoveJob's next line: dispatcher.Cancel, postProcessor.Cancel and
//     checkpointer.Prune run first, and that is real time for the
//     DirectUnpack-wait goroutine to finish stopping du for real even when
//     its own endStep call ran before its own awaitDirectUnpackOrAbort call —
//     which is the one bug this signal cannot be trusted to catch, since
//     catching it is a race against unrelated work rather than against the
//     bug itself.
//   - directUnpackWaitEndHook fires from the DirectUnpack-wait goroutine
//     itself, immediately after its own endStep call — the same goroutine
//     that runs awaitDirectUnpackOrAbort, so du.Done() reflects only that
//     goroutine's two statements in whatever order they actually ran, with no
//     other goroutine's timing involved. That makes it deterministic for the
//     bug removeJobHook cannot reliably catch, but it says nothing about a
//     step ended earlier, outside this goroutine, before either statement
//     runs — removeJobHook is the only signal for that.
func TestRemoveJob_WaitsForTheDirectUnpackToStop(t *testing.T) {
	t.Parallel()
	stage := finishedStage()
	application, j := launchedAppAt(t, stage, job.Extracting)
	id := j.ID()
	du := waitingDirectUnpack(t, application, id)
	fromRemoveJob := make(chan bool, 1)
	application.removeJobHook = func(string) {
		select {
		case <-du.Done():
			fromRemoveJob <- false
		default:
			fromRemoveJob <- true
		}
	}
	fromWaitEnd := make(chan bool, 1)
	application.directUnpackWaitEndHook = func(string) {
		select {
		case <-du.Done():
			fromWaitEnd <- false
		default:
			fromWaitEnd <- true
		}
	}

	application.maybeFinalize(id, "")
	removeWithinBudget(t, application, id)
	select {
	case stillRunning := <-fromRemoveJob:
		if stillRunning {
			t.Error("RemoveJob went on to its cleanup while the DirectUnpacker was still running")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RemoveJob never reached its cleanup")
	}
	select {
	case stillRunning := <-fromWaitEnd:
		if stillRunning {
			t.Error("the wait step ended while the DirectUnpacker was still running")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the DirectUnpack wait step never ended")
	}
}

// TestEnqueuePostProc_RemovedBeforeItsHandOver_ReleasesTheJob: an enqueue with
// no DirectUnpack that a RemoveJob overtakes before its hand-over, as while it
// closes the job's handles, or that looked the job up before the removal and
// is admitted after it, runs nothing, and RemoveJob returns with the job
// deregistered. At Extracting and Finalizing nothing but the refused
// hand-over releases the launch claim dispatcher.Remove waits on.
func TestEnqueuePostProc_RemovedBeforeItsHandOver_ReleasesTheJob(t *testing.T) {
	t.Parallel()
	for _, state := range postProcStates {
		t.Run(state.String(), func(t *testing.T) {
			t.Parallel()
			stage := finishedStage()
			application, j := launchedAppAt(t, stage, state)
			id := j.ID()
			row, ok := application.dispatcher.Row(id)
			if !ok {
				t.Fatal("the job is not registered")
			}

			removed := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				removed <- application.RemoveJob(ctx, id, false)
			}()
			waitFor(t, func() bool { return application.transitions.wasRemoved(j) })
			application.enqueuePostProc(j, row.Header, "")

			if err := <-removed; err != nil {
				t.Fatalf("RemoveJob: %v", err)
			}
			if _, ok := application.dispatcher.Job(id); ok {
				t.Error("the job is still registered after RemoveJob returned")
			}
			awaitAdmissionsEnded(t, application)
			assertNeverPostProcessed(t, application, stage)
		})
	}
}

// TestRemoveJob_InterruptsTheDirectUnpackWait: the removal itself ends the
// DirectUnpack wait, which duOrch.abortJob can no longer reach once the
// enqueue has collected the unpacker.
func TestRemoveJob_InterruptsTheDirectUnpackWait(t *testing.T) {
	t.Parallel()
	stage := finishedStage()
	application, j := admittedApp(t, stage)
	id := j.ID()
	waitingDirectUnpack(t, application, id)

	application.maybeFinalize(id, "")
	removeWithinBudget(t, application, id)
	awaitAdmissionsEnded(t, application)

	assertNeverPostProcessed(t, application, stage)
}

// handOverReason seals j's admission through a hand-over no removal refuses,
// and returns the run's FailMsg.
func handOverReason(a *postProcAdmissions, j *job.Job) string {
	var none jobTransitions
	msg, token, _ := a.beginHandOver(j, &none)
	a.endStep(j, token)
	return msg
}

// TestPostProcAdmissions_HandOverRefusesARemovedInstance: the hand-over
// refuses an instance a RemoveJob marked and an instance that is not admitted,
// and hands over another instance under the same ID.
func TestPostProcAdmissions_HandOverRefusesARemovedInstance(t *testing.T) {
	t.Parallel()
	var a postProcAdmissions
	var tr jobTransitions
	removed := job.New("same-id", "removed", job.Policy{})
	retry := job.New("same-id", "retry", job.Policy{})
	a.admit(removed, "")
	a.admit(retry, "")
	tr.markRemoved(removed)

	if _, _, ok := a.beginHandOver(removed, &tr); ok {
		t.Error("beginHandOver handed over a removed instance")
	}
	if _, _, ok := a.beginHandOver(retry, &tr); !ok {
		t.Error("beginHandOver refused an instance no RemoveJob marked")
	}
	if _, _, ok := a.beginHandOver(job.New("absent", "absent", job.Policy{}), &tr); ok {
		t.Error("beginHandOver handed over an instance that is not admitted")
	}
}

// returnsWithin runs fn in a goroutine and reports whether it returned within
// d, with a channel that closes when it does.
func returnsWithin(d time.Duration, fn func()) (<-chan struct{}, bool) {
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
		return done, true
	case <-time.After(d):
		return done, false
	}
}

// TestPostProcAdmissions_WithdrawWaitsOutAStep: withdraw closes the removal
// channel at once but returns only once the enqueue's step in progress, its
// DirectUnpack wait or its hand-over, has ended, through endStep or, for a
// job the post-processor finished first, through release.
func TestPostProcAdmissions_WithdrawWaitsOutAStep(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"wait", "hand-over"} {
		for _, end := range []string{"endStep", "release"} {
			t.Run(step+"/"+end, func(t *testing.T) {
				t.Parallel()
				var a postProcAdmissions
				var tr jobTransitions
				j := job.New("stepping", "stepping", job.Policy{})
				a.admit(j, "")
				removal, token := a.beginWait(j)
				if step == "hand-over" {
					a.endStep(j, token)
					var ok bool
					if _, token, ok = a.beginHandOver(j, &tr); !ok {
						t.Fatal("beginHandOver refused")
					}
				}

				var ended atomic.Bool
				var endedFirst atomic.Bool
				done, _ := returnsWithin(0, func() {
					a.withdraw(j)
					endedFirst.Store(ended.Load())
				})
				select {
				case <-removal:
				case <-time.After(5 * time.Second):
					t.Fatal("withdraw did not close the removal channel")
				}
				// withdraw has taken the step's token under the lock by the
				// time the channel is closed; a withdraw that does not wait
				// returns within microseconds of it.
				select {
				case <-done:
					t.Fatalf("withdraw returned while the %s was in progress", step)
				case <-time.After(100 * time.Millisecond):
				}
				ended.Store(true)
				if end == "endStep" {
					a.endStep(j, token)
				} else {
					a.release(j)
				}
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatalf("withdraw did not return after %s", end)
				}
				if !endedFirst.Load() {
					t.Errorf("withdraw returned before the %s ended", step)
				}
			})
		}
	}
}

// TestPostProcAdmissions_AStaleTokenEndsNoStep: an enqueue's step token ends
// only its own step. After its admission was released and the instance
// admitted again, a late endStep with the old token must not release a
// withdraw waiting on the new admission's step.
func TestPostProcAdmissions_AStaleTokenEndsNoStep(t *testing.T) {
	t.Parallel()
	var a postProcAdmissions
	var tr jobTransitions
	j := job.New("stale", "stale", job.Policy{})
	a.admit(j, "")
	_, stale, _ := a.beginHandOver(j, &tr)
	a.release(j)
	a.admit(j, "")
	_, current := a.beginWait(j)

	done, _ := returnsWithin(0, func() { a.withdraw(j) })
	a.endStep(j, stale)
	select {
	case <-done:
		t.Fatal("a stale token ended the current step")
	case <-time.After(100 * time.Millisecond):
	}
	a.endStep(j, current)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("withdraw did not return after the current step ended")
	}
}

// TestPostProcAdmissions_WithdrawIsSafeToRepeat: a second withdraw of one
// admission, and a withdraw of a job that is not admitted, return at once.
func TestPostProcAdmissions_WithdrawIsSafeToRepeat(t *testing.T) {
	t.Parallel()
	var a postProcAdmissions
	j := job.New("twice", "twice", job.Policy{})
	a.admit(j, "")
	absent := job.New("absent", "absent", job.Policy{})
	for i, target := range []*job.Job{j, j, absent} {
		if _, ok := returnsWithin(5*time.Second, func() { a.withdraw(target) }); !ok {
			t.Fatalf("withdraw %d did not return", i)
		}
	}
	if removal, token := a.beginWait(absent); removal != nil || token != nil {
		t.Error("beginWait of a job that is not admitted returned a channel")
	}
}

// TestAwaitDirectUnpackOrAbort_RemovalAbortsAndContinues: a removal aborts
// the unpack and returns true, leaving the hand-over to refuse the job, where
// a shutdown returns false.
func TestAwaitDirectUnpackOrAbort_RemovalAbortsAndContinues(t *testing.T) {
	t.Parallel()
	f := newFakeDirectUnpack()
	removed := make(chan struct{})
	close(removed)
	var got bool
	if _, ok := returnsWithin(5*time.Second, func() { got = awaitDirectUnpackOrAbort(t.Context(), removed, f) }); !ok {
		t.Fatal("awaitDirectUnpackOrAbort did not return after the removal")
	}
	if !got {
		t.Error("awaitDirectUnpackOrAbort returned false on a removal, want true")
	}
	if !f.aborted.Load() {
		t.Error("the removal did not abort the unpack")
	}
}
