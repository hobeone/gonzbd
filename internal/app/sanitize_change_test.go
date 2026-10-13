package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// TestRecordedFileName_SurvivesASanitizeChange pins that a file's recorded
// name is where both the verifier and a later registration look for it, after
// the sanitize options change (#905). A file named under the defaults keeps
// its spaces and diacritics; re-sanitizing the recorded name under
// replace_spaces_with and strip_diacritics would look for a different file,
// which the verifier reads as missing and refetches.
func TestRecordedFileName_SurvivesASanitizeChange(t *testing.T) {
	t.Parallel()
	const subject = "My Show Café.bin"
	application, j := newDurabilityTestAppSubjects(t, []string{subject}, 1)
	p := application.pipeline

	writeFixtureArticle(t, application, j.ID(), 0, 0)
	info, err := p.resolveFileInfo(j.ID(), 0)
	if err != nil {
		t.Fatalf("resolveFileInfo: %v", err)
	}
	written := info.Path()
	if info.Name != subject {
		t.Fatalf("writer named the file %q, want %q under the default options", info.Name, subject)
	}
	if _, err := os.Stat(written); err != nil {
		t.Fatalf("the writer's file: %v", err)
	}
	_, recorded := j.FileLocation(0)

	// The options a restart (or a settings change) hands the pipeline.
	p.mu.Lock()
	p.sanitize = fsutil.SanitizeOptions{ReplaceSpacesWith: "_", StripDiacritics: true}
	p.mu.Unlock()

	t.Run("verifier", func(t *testing.T) {
		loc, err := application.residency.locate(j.Name(), recorded)
		if err != nil {
			t.Fatalf("locate(%q): %v", recorded, err)
		}
		if loc.Path() != written {
			t.Errorf("verifier reads %q, want the writer's %q", loc.Path(), written)
		}
	})
	t.Run("re-registration", func(t *testing.T) {
		p.forgetFile(j.ID(), 0)
		if err := p.registerFile(j, 0); err != nil {
			t.Fatalf("registerFile: %v", err)
		}
		again, err := p.resolveFileInfo(j.ID(), 0)
		if err != nil {
			t.Fatalf("resolveFileInfo: %v", err)
		}
		if again.Path() != written {
			t.Errorf("re-registered at %q, want the writer's %q", again.Path(), written)
		}
	})
}

// TestVerifyJobFiles_ARefusedRecordedNameIsAFault pins that a recorded name
// jobFileLocation refuses is a verification fault on the retry path, even
// when the job directory is missing — where an open would have read the file
// as absent and dropped its rows.
func TestVerifyJobFiles_ARefusedRecordedNameIsAFault(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	f := newVerifyFixture(t)
	f.files[0].Filename = "../v.bin"
	f.resolve = func(fn string) (jobFile, error) {
		return application.pipeline.jobFileLocation("no such job", fn)
	}
	res, err := f.run(t, t.Context(), f.rows, true)
	want, _ := application.pipeline.jobFileLocation("no such job", "../v.bin")
	assertVerifyFault(t, res, err, want.Path())
	if !errors.Is(err, fsutil.ErrNotOneComponent) {
		t.Errorf("err = %v, want it to wrap ErrNotOneComponent", err)
	}
}

// TestRegisterFile_ARefusedRecordedNameFailsTheRegistration pins that a
// recorded name jobFileLocation refuses fails registerFile and caches
// nothing, so no write is ever handed a path out of the job directory.
func TestRegisterFile_ARefusedRecordedNameFailsTheRegistration(t *testing.T) {
	t.Parallel()
	for _, recorded := range []string{"../x.bin", "a/b.bin"} {
		t.Run(recorded, func(t *testing.T) {
			t.Parallel()
			application, j := newDurabilityTestApp(t, 1, 1)
			p := application.pipeline
			if err := j.RecordFileFilename(0, j.Name(), recorded); err != nil {
				t.Fatalf("RecordFileFilename: %v", err)
			}
			if err := p.registerFile(j, 0); !errors.Is(err, fsutil.ErrNotOneComponent) {
				t.Errorf("registerFile = %v, want an error wrapping ErrNotOneComponent", err)
			}
			if info, err := p.resolveFileInfo(j.ID(), 0); err == nil {
				t.Errorf("registerFile cached %q for a refused name", info.Path())
			}
		})
	}
}

// TestPeek_ARefusedRecordedNameIsNotOpened pins that the peek of a file the
// verifier finished skips a recorded name jobFileLocation refuses rather than
// opening it: here "../outside.rar" names a flagged archive beside the job
// directory, which would block the job if it were read.
func TestPeek_ARefusedRecordedNameIsNotOpened(t *testing.T) {
	t.Parallel()
	data := unpackFixture(t, "single_rar5.rar")
	a := newPeekApp(t, unwanted.ActionPause, onlyTxt, false, []peekFile{{"release.part1.rar", data}})
	dir := filepath.Join(a.config.GetGeneral().DownloadDir, a.j.Name())
	if err := os.Rename(filepath.Join(dir, "release.part1.rar"), filepath.Join(dir, "..", "outside.rar")); err != nil {
		t.Fatal(err)
	}
	a.pipeline.forgetFile(a.j.ID(), 0) // as for a file finished at hydration
	if err := a.j.RecordFileFilename(0, a.j.Name(), "../outside.rar"); err != nil {
		t.Fatalf("RecordFileFilename: %v", err)
	}

	if msg := a.peekResumedFile(a.j, 0); msg != "" {
		t.Errorf("peek = %q, want no message", msg)
	}
	if got := a.state(t); got != unwanted.StateNone {
		t.Errorf("Unwanted = %d, want none: the peek read an archive outside the job directory", got)
	}
}

// TestReloadDownloadOptions_NamesNewFilesWithTheNewSanitizeOptions pins that
// a sanitize change applied through ReloadDownloadOptions reaches the next
// file the pipeline names, without a restart.
func TestReloadDownloadOptions_NamesNewFilesWithTheNewSanitizeOptions(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestAppSubjects(t, []string{"My Show.bin"}, 1)

	d := application.config.GetDownloads()
	d.ReplaceSpacesWith = "_"
	application.ReloadDownloadOptions(d)

	if err := application.pipeline.registerFile(j, 0); err != nil {
		t.Fatalf("registerFile: %v", err)
	}
	if _, got := j.FileLocation(0); got != "My_Show.bin" {
		t.Errorf("recorded filename = %q, want %q under replace_spaces_with=_", got, "My_Show.bin")
	}
}
