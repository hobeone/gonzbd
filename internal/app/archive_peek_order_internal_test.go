package app

import (
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// A job the peek fails is filed only after the flagged file is marked
// complete: the history entry must retain that file as Complete (a retry
// reuses it), and the completion must not report the job as not resident.
// Driven through completeFinalizedFile, the path the pipeline uses.
func TestPeek_FailActionFilesTheJobAfterTheFileIsMarkedComplete(t *testing.T) {
	t.Parallel()
	a := newPeekApp(t, unwanted.ActionFail, onlyTxt, false,
		[]peekFile{{"release.part1.rar", unpackFixture(t, "single_rar5.rar")}, {"release.part2.rar", []byte("later")}})
	// The flagged file is not the job's last, so the job is not yet due at
	// Assessing and is handed to post-processing as soon as it is filed.
	// The manifest AddJob would have written, so the entry's file list does
	// not depend on the job still being resident when the finalizer reads it.
	if err := writeJobManifest(a.config.GetGeneral().AdminDir, a.j); err != nil {
		t.Fatalf("writeJobManifest: %v", err)
	}
	// Hold the completion between the peek and the mark until the job, if it
	// was filed in this window, has reached history: the entry would then be
	// written with file 0 still incomplete. Nothing waits when the job is not
	// filed yet, as it should not be.
	a.peekedHook = func() {
		if a.postProcAdmissions.has(a.j) {
			a.awaitHistory(t)
		}
	}
	// An error here is the job having been evicted before the mark
	// (job.ErrNotResident); reported, not fatal, so the history check below
	// still says what was recorded.
	if err := a.completeFinalizedFile(t.Context(), FileComplete{JobID: a.j.ID(), FileIdx: 0}); err != nil {
		t.Errorf("completeFinalizedFile: %v", err)
	}

	e := a.awaitHistory(t)
	files, err := a.repo.RetainedFiles(t.Context(), e.NzoID)
	if err != nil {
		t.Fatalf("RetainedFiles: %v", err)
	}
	if len(files) != 2 || !files[0].Complete || files[1].Complete {
		t.Fatalf("retained files = %+v, want file 0 Complete and file 1 not", files)
	}
}

// The filing is owed by the job's state, not by a message carried in one
// call: a completion whose mark fails (the job was evicted after the peek
// blocked it) leaves the job Blocked and running, and the redelivered
// completion, which finds the job already Blocked, files it.
func TestPeek_FailActionFilingSurvivesAnEvictionBeforeTheMark(t *testing.T) {
	t.Parallel()
	a := newPeekApp(t, unwanted.ActionFail, onlyTxt, false,
		[]peekFile{{"release.part1.rar", unpackFixture(t, "single_rar5.rar")}, {"release.part2.rar", []byte("later")}})
	if err := writeJobManifest(a.config.GetGeneral().AdminDir, a.j); err != nil {
		t.Fatalf("writeJobManifest: %v", err)
	}
	evict := true
	a.peekedHook = func() {
		if evict {
			evict = false
			a.j.Evict()
		}
	}
	fc := FileComplete{JobID: a.j.ID(), FileIdx: 0}
	if err := a.completeFinalizedFile(t.Context(), fc); !errors.Is(err, job.ErrNotResident) {
		t.Fatalf("first completion = %v, want job.ErrNotResident", err)
	}
	if got := a.state(t); got != unwanted.StateBlocked {
		t.Fatalf("Unwanted = %d, want blocked", got)
	}
	if a.postProcAdmissions.has(a.j) {
		t.Fatal("the job was filed although its mark failed")
	}

	if err := a.residency.Hydrate(t.Context(), a.j.ID()); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	a.complete(t, 0) // the redelivery

	e := a.awaitHistory(t)
	if e.Status != string(constants.StatusFailed) || e.FailMessage != unwantedFailPrefix {
		t.Errorf("entry = %q %q, want Failed %q", e.Status, e.FailMessage, unwantedFailPrefix)
	}
	if e.Unwanted != unwanted.StateBlocked {
		t.Errorf("history Unwanted = %d, want blocked", e.Unwanted)
	}
}

// A restored job that the fail action blocked but did not file is filed at
// startup, before it can run on; one the pause action blocked is held, not
// filed.
func TestReconcile_FilesAFailBlockedJobAndHoldsAPauseBlockedOne(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		pause  bool
		filed  bool
		intent job.Intent
	}{
		{"fail action", false, true, job.IntentRun},
		{"pause action", true, false, job.IntentPause},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a := newPeekApp(t, unwanted.ActionFail, onlyTxt, false,
				[]peekFile{{"release.part1.rar", unpackFixture(t, "single_rar5.rar")}, {"release.part2.rar", []byte("later")}})
			if err := writeJobManifest(a.config.GetGeneral().AdminDir, a.j); err != nil {
				t.Fatalf("writeJobManifest: %v", err)
			}
			// The persisted state between the move and the filing.
			if moved, err := a.dispatcher.BlockUnwanted(a.j, c.pause); err != nil || !moved {
				t.Fatalf("BlockUnwanted = (%v, %v)", moved, err)
			}
			if in := a.j.Intent(); in != c.intent {
				t.Fatalf("Intent = %v, want %v", in, c.intent)
			}
			if err := a.reconcileBeforeFirstTick(t.Context()); err != nil {
				t.Fatalf("reconcileBeforeFirstTick: %v", err)
			}
			if got := a.postProcAdmissions.has(a.j); got != c.filed {
				t.Fatalf("filed = %v, want %v", got, c.filed)
			}
			if c.filed {
				if e := a.awaitHistory(t); e.Status != string(constants.StatusFailed) {
					t.Errorf("Status = %q, want Failed", e.Status)
				}
			}
		})
	}
}

// A par2 file that cannot be parsed is skipped: the completion succeeds, the
// job is neither blocked nor failed, and it stays registered.
func TestPeek_CorruptPar2IsSkippedWithoutAffectingTheJob(t *testing.T) {
	t.Parallel()
	a := newPeekApp(t, unwanted.ActionPause, onlyTxt, false,
		[]peekFile{{"release.par2", par2Declaring("payload.exe")[:70]}})
	a.complete(t, 0)
	if got := a.state(t); got != unwanted.StateNone {
		t.Fatalf("Unwanted = %d, want none", got)
	}
	if in := a.j.Intent(); in != job.IntentRun {
		t.Errorf("Intent = %v, want IntentRun", in)
	}
}

// With no file info cached (the startup repair of a stranded finalize runs
// before the pipeline resolved any), the path comes from the filename the job
// recorded; with neither, the file is left alone.
func TestPeek_PathFallsBackToTheRecordedFilename(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		recorded bool
		want     unwanted.State
	}{
		{"filename recorded", true, unwanted.StateBlocked},
		{"nothing resolves the path", false, unwanted.StateNone},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a := newPeekApp(t, unwanted.ActionPause, onlyTxt, false,
				[]peekFile{{"release.part1.rar", unpackFixture(t, "single_rar5.rar")}})
			a.pipeline.forgetJob(a.j.ID())
			if c.recorded {
				if err := a.j.SetFileFilename(0, "release.part1.rar"); err != nil {
					t.Fatalf("SetFileFilename: %v", err)
				}
			}
			a.complete(t, 0)
			if got := a.state(t); got != c.want {
				t.Errorf("Unwanted = %d, want %d", got, c.want)
			}
		})
	}
}
