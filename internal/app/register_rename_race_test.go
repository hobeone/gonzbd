package app

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
)

// TestRegisterFile_RenameBetweenNameReadAndFilenameWrite pins #838: a rename
// that lands after registerFile read the job's name and before it recorded the
// file's filename must not leave the file under the old name. Either the
// rename is refused with dispatch.ErrJobStarted and the file is under the old
// name, or the rename is accepted and the file is under the new one.
func TestRegisterFile_RenameBetweenNameReadAndFilenameWrite(t *testing.T) {
	t.Parallel()
	a, id := renameFixture(t)
	j, ok := a.Dispatcher().Job(id)
	if !ok {
		t.Fatalf("job %s is gone", id)
	}
	p := helperPipeline(t, a.Dispatcher())

	var renamed string
	var renameErr error
	fired := false
	p.nameReadHook = func(*job.Job) {
		if fired {
			return
		}
		fired = true
		renamed, renameErr = a.RenameJob(id, "renamed")
	}

	if err := p.registerFile(j, 0); err != nil {
		t.Fatalf("registerFile: %v", err)
	}
	if !fired {
		t.Fatal("the hook never ran; the test exercises nothing")
	}
	info, err := p.resolveFileInfo(id, 0)
	if err != nil {
		t.Fatalf("resolveFileInfo: %v", err)
	}

	want := "first"
	switch {
	case errors.Is(renameErr, dispatch.ErrJobStarted):
	case renameErr == nil:
		want = renamed
	default:
		t.Fatalf("RenameJob = %v, want nil or ErrJobStarted", renameErr)
	}
	if got := nameOf(t, a, id); got != want || j.Name() != want {
		t.Errorf("job name: header %q, job %q; want %q", got, j.Name(), want)
	}
	if got := filepath.Base(info.Dir); got != want {
		t.Errorf("file registered under directory %q while the job is named %q (rename err %v): the job's files split across two directories", got, want, renameErr)
	}
	if got := j.Progress().FileFilename(0); got != info.Name {
		t.Errorf("recorded filename %q, want the registered %q", got, info.Name)
	}
}

// TestRegisterFile_ConcurrentRenameLeavesOneDirectory runs registerFile and
// RenameJob concurrently, with no hook, and requires the outcome the hooked
// test requires: the file is registered under the name the job ends with.
func TestRegisterFile_ConcurrentRenameLeavesOneDirectory(t *testing.T) {
	t.Parallel()
	for range 20 {
		a, id := renameFixture(t)
		j, ok := a.Dispatcher().Job(id)
		if !ok {
			t.Fatalf("job %s is gone", id)
		}
		p := helperPipeline(t, a.Dispatcher())

		var wg sync.WaitGroup
		var renameErr, registerErr error
		wg.Go(func() { _, renameErr = a.RenameJob(id, "renamed") })
		wg.Go(func() { registerErr = p.registerFile(j, 0) })
		wg.Wait()

		if registerErr != nil {
			t.Fatalf("registerFile: %v", registerErr)
		}
		if renameErr != nil && !errors.Is(renameErr, dispatch.ErrJobStarted) {
			t.Fatalf("RenameJob = %v, want nil or ErrJobStarted", renameErr)
		}
		info, err := p.resolveFileInfo(id, 0)
		if err != nil {
			t.Fatalf("resolveFileInfo: %v", err)
		}
		if got, want := filepath.Base(info.Dir), nameOf(t, a, id); got != want {
			t.Fatalf("file registered under directory %q while the job is named %q (rename err %v)", got, want, renameErr)
		}
	}
}

// TestRegisterFile_GivesUpAfterRenamesKeepWinning pins the bound on
// registerFile's retries: a rename landing in every attempt's window ends the
// call with an error, and nothing is recorded or registered for the file.
func TestRegisterFile_GivesUpAfterRenamesKeepWinning(t *testing.T) {
	t.Parallel()
	a, id := renameFixture(t)
	j, ok := a.Dispatcher().Job(id)
	if !ok {
		t.Fatalf("job %s is gone", id)
	}
	p := helperPipeline(t, a.Dispatcher())

	n := 0
	p.nameReadHook = func(*job.Job) {
		n++
		if _, err := a.RenameJob(id, "renamed"+string(rune('a'+n))); err != nil {
			t.Errorf("RenameJob in attempt %d: %v", n, err)
		}
	}

	err := p.registerFile(j, 0)
	if !errors.Is(err, job.ErrNameChanged) {
		t.Fatalf("registerFile = %v, want ErrNameChanged", err)
	}
	if n != registerNameAttempts {
		t.Errorf("registerFile made %d attempts, want %d", n, registerNameAttempts)
	}
	if _, err := p.resolveFileInfo(id, 0); err == nil {
		t.Error("a file whose filename was never recorded is registered with the assembler")
	}
	if got := j.Progress().FileFilename(0); got != "" {
		t.Errorf("recorded filename %q, want none", got)
	}
}

// TestRegisterFileAttempt_ReportsARenameInItsWindow pins one attempt's
// contract: a rename accepted inside its window is reported as
// job.ErrNameChanged with nothing recorded or registered, and the next
// attempt resolves the file under the new name.
func TestRegisterFileAttempt_ReportsARenameInItsWindow(t *testing.T) {
	t.Parallel()
	a, id := renameFixture(t)
	j, ok := a.Dispatcher().Job(id)
	if !ok {
		t.Fatalf("job %s is gone", id)
	}
	p := helperPipeline(t, a.Dispatcher())
	m := mustManifest(t, j)

	fired := false
	p.nameReadHook = func(*job.Job) {
		if fired {
			return
		}
		fired = true
		if _, err := a.RenameJob(id, "renamed"); err != nil {
			t.Errorf("RenameJob: %v", err)
		}
	}
	if err := p.registerFileAttempt(j, m, 0); !errors.Is(err, job.ErrNameChanged) {
		t.Fatalf("first attempt = %v, want ErrNameChanged", err)
	}
	if _, err := p.resolveFileInfo(id, 0); err == nil {
		t.Error("a refused attempt registered the file with the assembler")
	}
	if err := p.registerFileAttempt(j, m, 0); err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	info, err := p.resolveFileInfo(id, 0)
	if err != nil {
		t.Fatalf("resolveFileInfo: %v", err)
	}
	if got := filepath.Base(info.Dir); got != "renamed" {
		t.Errorf("second attempt registered the file under %q, want renamed", got)
	}
}
