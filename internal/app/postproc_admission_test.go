package app

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// admittedApp is heldRepairingApp with a download directory at the path
// enqueuePostProc derives, so a job it hands over runs its stages rather
// than failing the empty-directory pre-check.
func admittedApp(t *testing.T, stage gatedStage) (*Application, *job.Job) {
	t.Helper()
	application, j, _ := heldRepairingApp(t, stage)
	dir := filepath.Join(application.config.GetGeneral().DownloadDir, j.Name())
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "held.bin"), make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	return application, j
}

// awaitFinalized waits until the job has left the dispatcher and the
// post-processor has nothing queued or running.
func awaitFinalized(t *testing.T, application *Application, id string) {
	t.Helper()
	waitFor(t, func() bool {
		_, registered := application.dispatcher.Job(id)
		return !registered && application.postProcessor.Empty()
	})
}

// admissionsHeld counts the admissions a has not ended.
func admissionsHeld(a *postProcAdmissions) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.jobs)
}

// historyEntry reads id's history entry.
func historyEntry(t *testing.T, application *Application, id string) *history.Entry {
	t.Helper()
	entry, err := application.historyRepo.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("history Get: %v", err)
	}
	return entry
}

// stageLogLines returns every line of the entry's stage log whose stage is
// named stage.
func stageLogLines(t *testing.T, entry *history.Entry, stage string) []string {
	t.Helper()
	var log []struct {
		Stage string
		Lines []string
	}
	if err := json.Unmarshal([]byte(entry.StageLog), &log); err != nil {
		t.Fatalf("decode StageLog: %v", err)
	}
	var lines []string
	for _, e := range log {
		if e.Stage == stage {
			lines = append(lines, e.Lines...)
		}
	}
	return lines
}

// TestFail_WhilePostProcessing_IsNotedAndLeavesTheStatusToTheStages: a
// permanent storage fault on a job whose stages are running does not hand the
// job to the post-processor again. Its reason arrived too late for the stages
// to act on, so the history entry reports what the stages did and carries the
// reason as a warning.
func TestFail_WhilePostProcessing_IsNotedAndLeavesTheStatusToTheStages(t *testing.T) {
	t.Parallel()
	stage := gatedStage{entered: make(chan string, 4), finish: make(chan struct{})}
	application, j := admittedApp(t, stage)
	id := j.ID()

	application.maybeFinalize(id, "")
	awaitStage(t, stage.entered)

	fault := storagefault.Classify("write", "/mnt/ro/held.bin", syscall.EROFS)
	application.Fail(id, fault)
	close(stage.finish)
	awaitFinalized(t, application, id)

	if n := len(application.postProcessor.History()); n != 1 {
		t.Errorf("the post-processor finished %d copies of the job, want 1", n)
	}
	entry := historyEntry(t, application, id)
	if entry.Status != "Completed" || entry.FailMessage != "" {
		t.Errorf("history Status, FailMessage = %q, %q, want Completed with no message", entry.Status, entry.FailMessage)
	}
	warnings := stageLogLines(t, entry, "warnings")
	if len(warnings) != 1 || !strings.Contains(warnings[0], "Failed: "+fault.Error()) {
		t.Errorf("warnings stage lines = %q, want one naming the fault", warnings)
	}
	waitFor(t, func() bool { return admissionsHeld(&application.postProcAdmissions) == 0 })
}

// TestRemoveJob_EndsThePostProcessingAdmission: a job removed while its
// post-processing runs does not keep its admission.
func TestRemoveJob_EndsThePostProcessingAdmission(t *testing.T) {
	t.Parallel()
	stage := gatedStage{entered: make(chan string, 4), finish: make(chan struct{})}
	application, j := admittedApp(t, stage)

	application.maybeFinalize(j.ID(), "")
	awaitStage(t, stage.entered)
	removeWithinBudget(t, application, j.ID())
	waitFor(t, func() bool { return admissionsHeld(&application.postProcAdmissions) == 0 })
}

// TestJobFinalizerCancelled_EndsTheAdmissionWithoutADispatcher: the admission
// is ended even where there is no dispatcher to release a claim on, and a
// callback carrying no job is ignored.
func TestJobFinalizerCancelled_EndsTheAdmissionWithoutADispatcher(t *testing.T) {
	t.Parallel()
	application := &Application{}
	f := newJobFinalizer(application)
	j := job.New("no-dispatcher", "no-dispatcher", job.Policy{})
	application.postProcAdmissions.admit(j, "")

	f.cancelled(nil)
	f.cancelled(&postproc.Job{})
	if n := admissionsHeld(&application.postProcAdmissions); n != 1 {
		t.Fatalf("admissions held after job-less callbacks = %d, want 1", n)
	}
	f.cancelled(&postproc.Job{Job: j})
	if n := admissionsHeld(&application.postProcAdmissions); n != 0 {
		t.Errorf("admissions held after cancelled = %d, want 0", n)
	}
}

// TestRunPostProc_DuringTheFinalizerTail_EnqueuesNoSecondCopy: once the worker
// has cleared its busy marker the post-processor no longer reports the job,
// but its finalizer has not yet taken it out of the dispatcher. A launch that
// lands then must not hand the job over again.
func TestRunPostProc_DuringTheFinalizerTail_EnqueuesNoSecondCopy(t *testing.T) {
	t.Parallel()
	stage := gatedStage{entered: make(chan string, 4), finish: make(chan struct{})}
	application, j := admittedApp(t, stage)
	id := j.ID()

	// Only the first finalize waits, so a second copy fails the assertions
	// below rather than hanging the test.
	inTail := make(chan struct{})
	leaveTail := make(chan struct{})
	var finalizes atomic.Int32
	application.finalizeHook = func(*postproc.Job) {
		if finalizes.Add(1) == 1 {
			close(inTail)
			<-leaveTail
		}
	}

	application.maybeFinalize(id, "")
	awaitStage(t, stage.entered)
	close(stage.finish)
	<-inTail
	if application.postProcessor.Has(id) {
		t.Fatal("the post-processor still reports the job inside OnJobDone, so this would not test the tail")
	}

	newAppRunner(application).runPostProc(t.Context(), id, job.Repairing)
	queuedAgain := application.postProcessor.Has(id)
	close(leaveTail)
	awaitFinalized(t, application, id)

	if queuedAgain {
		t.Error("a launch during the finalizer's tail handed the job to the post-processor again")
	}
	if n := len(application.postProcessor.History()); n != 1 {
		t.Errorf("the post-processor finished %d copies of the job, want 1", n)
	}
}

// TestEnqueue_DuringTheDirectUnpackWait_EnqueuesNoSecondCopy: while the
// admitted enqueue waits for the job's DirectUnpack, the post-processor has not
// been given the job. A launch or a Fail then must not hand it over, and the
// Fail's reason, arriving before the run's reason is sealed, must reach the
// admitted run before its stages and fail its history entry.
func TestEnqueue_DuringTheDirectUnpackWait_EnqueuesNoSecondCopy(t *testing.T) {
	t.Parallel()
	stage := gatedStage{entered: make(chan string, 4), finish: make(chan struct{})}
	close(stage.finish)
	application, j := admittedApp(t, stage)
	id := j.ID()

	// A DirectUnpacker given the first of two volumes waits for the second.
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

	application.maybeFinalize(id, "")
	fault := storagefault.Classify("write", "/mnt/ro/held.bin", syscall.EROFS)
	application.Fail(id, fault)
	newAppRunner(application).runPostProc(t.Context(), id, job.Repairing)
	handedOver := application.postProcessor.Has(id)

	du.Abort()
	awaitFinalized(t, application, id)

	if handedOver {
		t.Error("the job was handed to the post-processor during its DirectUnpack wait")
	}
	if n := len(application.postProcessor.History()); n != 1 {
		t.Errorf("the post-processor finished %d copies of the job, want 1", n)
	}
	if n := len(stage.entered); n != 0 {
		t.Errorf("the stages ran %d times for a job the Fail had already failed, want 0", n)
	}
	entry := historyEntry(t, application, id)
	if want := "Failed: " + fault.Error(); entry.Status != "Failed" || entry.FailMessage != want {
		t.Errorf("history Status, FailMessage = %q, %q, want Failed, %q", entry.Status, entry.FailMessage, want)
	}
}

// TestPostProcAdmissions_AdmitsEachInstanceOnce: an instance is admitted once
// until its admission is released, and a second instance under the same ID
// is a different job with an admission of its own.
func TestPostProcAdmissions_AdmitsEachInstanceOnce(t *testing.T) {
	t.Parallel()
	var a postProcAdmissions
	first := job.New("same-id", "first", job.Policy{})
	retry := job.New("same-id", "retry", job.Policy{})

	if got := a.admit(first, ""); got != admitted {
		t.Fatalf("first admit(first) = %v, want admitted", got)
	}
	if got := a.admit(first, ""); got != refused {
		t.Errorf("second admit(first) = %v, want refused", got)
	}
	if got := a.admit(retry, ""); got != admitted {
		t.Errorf("admit(retry) under first's ID = %v, want admitted", got)
	}
	a.release(first)
	if got := a.admit(retry, ""); got != refused {
		t.Errorf("admit(retry) after releasing first = %v, want refused", got)
	}
	if got := a.admit(first, ""); got != admitted {
		t.Errorf("admit(first) after its release = %v, want admitted", got)
	}
}

// TestPostProcAdmissions_HasIsPerInstance: has is the downloader's HandedOff,
// so it answers for the instance, and stops answering once the admission ends.
func TestPostProcAdmissions_HasIsPerInstance(t *testing.T) {
	t.Parallel()
	var a postProcAdmissions
	first := job.New("same-id", "first", job.Policy{})
	retry := job.New("same-id", "retry", job.Policy{})

	if a.has(first) {
		t.Fatal("has(first) = true before any admission")
	}
	a.admit(first, "")
	if !a.has(first) {
		t.Error("has(first) = false for an admitted instance; the downloader would dispatch it")
	}
	if a.has(retry) {
		t.Error("has(retry) = true for another instance under the same ID; a retry would never download")
	}
	a.release(first)
	if a.has(first) {
		t.Error("has(first) = true after its admission ended")
	}
}

// TestPostProcAdmissions_KeepsTheFirstFailureReason: a refused call's reason
// becomes the run's only while the admission has none and is not sealed; any
// other new reason is noted once.
func TestPostProcAdmissions_KeepsTheFirstFailureReason(t *testing.T) {
	t.Parallel()
	var a postProcAdmissions
	j := job.New("reasons", "reasons", job.Policy{})
	a.admit(j, "")
	if got := a.admit(j, "first"); got != refusedReasonKept {
		t.Errorf("admit with a first reason = %v, want refusedReasonKept", got)
	}
	if got := a.admit(j, "first"); got != refused {
		t.Errorf("admit repeating the kept reason = %v, want refused", got)
	}
	if got := a.admit(j, "second"); got != refusedReasonNoted {
		t.Errorf("admit with a second reason = %v, want refusedReasonNoted", got)
	}
	if got := a.admit(j, "second"); got != refused {
		t.Errorf("admit repeating a noted reason = %v, want refused", got)
	}
	if got := handOverReason(&a, j); got != "first" {
		t.Errorf("hand-over reason = %q, want %q", got, "first")
	}
	if got := a.notes(j); len(got) != 1 || got[0] != "second" {
		t.Errorf("notes = %q, want [second]", got)
	}

	sealed := job.New("sealed", "sealed", job.Policy{})
	a.admit(sealed, "")
	if got := handOverReason(&a, sealed); got != "" {
		t.Errorf("hand-over reason = %q, want empty", got)
	}
	if got := a.admit(sealed, "late"); got != refusedReasonNoted {
		t.Errorf("admit with a reason after seal = %v, want refusedReasonNoted", got)
	}
	if got := handOverReason(&a, sealed); got != "" {
		t.Errorf("hand-over reason after a late reason = %q, want empty", got)
	}
	if got := a.notes(sealed); len(got) != 1 || got[0] != "late" {
		t.Errorf("notes after a late reason = %q, want [late]", got)
	}
	if got := a.notes(job.New("absent", "absent", job.Policy{})); got != nil {
		t.Errorf("notes of an unadmitted job = %q, want nil", got)
	}
}

// TestWithFailureNotes_LeavesThePostProcessorsJobAlone: notes are added to a
// copy's stage log, never to the post-processor's own job, and change no
// field buildHistoryEntry derives the status from.
func TestWithFailureNotes_LeavesThePostProcessorsJobAlone(t *testing.T) {
	t.Parallel()
	ppJob := &postproc.Job{StageLog: []postproc.StageLogEntry{{Stage: "repair"}}}
	if got := withFailureNotes(ppJob, nil); got != ppJob {
		t.Error("withFailureNotes without notes did not return its argument")
	}
	got := withFailureNotes(ppJob, []string{"late"})
	if len(ppJob.StageLog) != 1 {
		t.Errorf("the original's StageLog has %d entries, want 1", len(ppJob.StageLog))
	}
	if n := len(got.StageLog); n != 2 || got.StageLog[1].Stage != "warnings" ||
		len(got.StageLog[1].Lines) != 1 || !strings.HasSuffix(got.StageLog[1].Lines[0], "late") {
		t.Errorf("copy's StageLog = %+v, want the original plus one warnings entry naming the note", got.StageLog)
	}
	if got.FailMsg != "" {
		t.Errorf("copy's FailMsg = %q, want empty", got.FailMsg)
	}
}
