package app

import (
	"testing"
	"time"

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
	// Hold the completion between the peek and the mark: a job filed in this
	// window would be handed to post-processing, and its history entry
	// written, with file 0 still incomplete.
	a.peekedHook = func() { time.Sleep(300 * time.Millisecond) }
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
