package app

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/job"
)

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
// to the post-processor once the wait ends.
func TestRemoveJob_DuringTheDirectUnpackWait_PostProcessingDoesNotRun(t *testing.T) {
	t.Parallel()
	stage := gatedStage{entered: make(chan string, 4), finish: make(chan struct{})}
	close(stage.finish)
	application, j := admittedApp(t, stage)
	id := j.ID()
	du := waitingDirectUnpack(t, application, id)

	application.maybeFinalize(id, "")
	removeWithinBudget(t, application, id)
	du.Abort()
	awaitAdmissionsEnded(t, application)

	assertNeverPostProcessed(t, application, stage)
}

// TestRemoveJob_InterruptsTheDirectUnpackWait: the removal itself ends the
// DirectUnpack wait, which duOrch.abortJob can no longer reach once the
// enqueue has collected the unpacker.
func TestRemoveJob_InterruptsTheDirectUnpackWait(t *testing.T) {
	t.Parallel()
	stage := gatedStage{entered: make(chan string, 4), finish: make(chan struct{})}
	close(stage.finish)
	application, j := admittedApp(t, stage)
	id := j.ID()
	waitingDirectUnpack(t, application, id)

	application.maybeFinalize(id, "")
	removeWithinBudget(t, application, id)
	awaitAdmissionsEnded(t, application)

	assertNeverPostProcessed(t, application, stage)
}

// TestEnqueuePostProc_AfterRemoveJob_DoesNotHandOver: an enqueue that looked
// the job up before a RemoveJob took it, and reaches its admission only after,
// does not hand it to the post-processor. The same check covers an enqueue
// with no DirectUnpack that a removal overtakes while it closes the job's
// handles.
func TestEnqueuePostProc_AfterRemoveJob_DoesNotHandOver(t *testing.T) {
	t.Parallel()
	stage := gatedStage{entered: make(chan string, 4), finish: make(chan struct{})}
	close(stage.finish)
	application, j := admittedApp(t, stage)
	id := j.ID()
	row, ok := application.dispatcher.Row(id)
	if !ok {
		t.Fatal("the job is not registered")
	}

	removeWithinBudget(t, application, id)
	application.enqueuePostProc(j, row.Header, "")
	awaitAdmissionsEnded(t, application)

	assertNeverPostProcessed(t, application, stage)
}

// handOverReason seals j's admission through a hand-over no removal refuses,
// and returns the run's FailMsg.
func handOverReason(a *postProcAdmissions, j *job.Job) string {
	var none jobTransitions
	msg, _ := a.beginHandOver(j, &none)
	a.endHandOver(j)
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

	if _, ok := a.beginHandOver(removed, &tr); ok {
		t.Error("beginHandOver handed over a removed instance")
	}
	if _, ok := a.beginHandOver(retry, &tr); !ok {
		t.Error("beginHandOver refused an instance no RemoveJob marked")
	}
	if _, ok := a.beginHandOver(job.New("absent", "absent", job.Policy{}), &tr); ok {
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

// TestPostProcAdmissions_WithdrawWaitsOutAHandOver: withdraw closes the
// removal channel at once, but returns only once a hand-over in progress
// ends, so the PostProcessor.Cancel after it finds the job. A job the
// post-processor finishes first ends the hand-over through its release.
func TestPostProcAdmissions_WithdrawWaitsOutAHandOver(t *testing.T) {
	t.Parallel()
	for _, end := range []string{"endHandOver", "release"} {
		t.Run(end, func(t *testing.T) {
			t.Parallel()
			var a postProcAdmissions
			var tr jobTransitions
			j := job.New("handing", "handing", job.Policy{})
			a.admit(j, "")
			removal := a.removal(j)
			if _, ok := a.beginHandOver(j, &tr); !ok {
				t.Fatal("beginHandOver refused")
			}

			done, returned := returnsWithin(50*time.Millisecond, func() { a.withdraw(j) })
			if returned {
				t.Fatal("withdraw returned while the hand-over was in progress")
			}
			select {
			case <-removal:
			case <-time.After(5 * time.Second):
				t.Fatal("withdraw did not close the removal channel")
			}
			if end == "endHandOver" {
				a.endHandOver(j)
			} else {
				a.release(j)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("withdraw did not return after %s", end)
			}
		})
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
	if a.removal(absent) != nil {
		t.Error("removal of a job that is not admitted is not nil")
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
