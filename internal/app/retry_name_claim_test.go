package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// seedFailedRetry files a Failed history entry named name, with its NZB
// backup, and leaves its bytes in a _FAILED_ directory holding "retained", as
// the finalize stage does. It returns the entry's job ID and the two
// directories.
func seedFailedRetry(t *testing.T, application *Application, repo *history.Repository, name string) (id, jobDir, failedDir string) {
	t.Helper()
	nzbDir := filepath.Join(application.config.GetGeneral().AdminDir, "nzb")
	if err := os.MkdirAll(nzbDir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	id = "feedfacecafe0726"
	backup := name + ".nzb.gz"
	rawNZB := []byte(fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="test" date="1700000000" subject="%s.bin yEnc (1/1)">
    <groups><group>alt.binaries.test</group></groups>
    <segments><segment bytes="100" number="1">%s-0@t</segment></segments>
  </file>
</nzb>`, name, name))
	if err := fsutil.WriteGzAtomicBytes(filepath.Join(nzbDir, backup), rawNZB); err != nil {
		t.Fatalf("WriteGzAtomicBytes: %v", err)
	}
	downloadDir := application.downloadDir()
	jobDir = filepath.Join(downloadDir, name)
	failedDir = postproc.FailedDir(jobDir)
	if err := os.MkdirAll(failedDir, 0o750); err != nil {
		t.Fatalf("MkdirAll failed dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(failedDir, "retained"), []byte("retained"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     id,
		Name:      name,
		NzbName:   name + ".nzb",
		NZBBackup: backup,
		Category:  "*",
		Status:    "Failed",
		Path:      failedDir,
		Completed: time.Now(),
	}, nil); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	return id, jobDir, failedDir
}

// TestRetryHistoryJob_NoJobCanTakeItsNameAfterTheRestore pins that a retry
// holds its name from before its _FAILED_ directory is moved back: a job that
// tries to register the name in the window between the restore and the retry's
// own registration (the hook runs exactly there) is refused, writes nothing
// into the restored directory, and the retry succeeds and keeps its bytes.
func TestRetryHistoryJob_NoJobCanTakeItsNameAfterTheRestore(t *testing.T) {
	t.Parallel()
	application, repo := newNameRaceApp(t)
	const name = "name-claim"
	id, jobDir, failedDir := seedFailedRetry(t, application, repo, name)

	other, ho, _ := buildNamedIngestJob(t, application, name, "other")
	var otherErr error
	application.retryRegisteringHook = func(string) {
		if _, err := os.Stat(filepath.Join(jobDir, "retained")); err != nil {
			t.Errorf("setup: the hook ran before the restore: %v", err)
		}
		otherErr = application.dispatcher.Add(t.Context(), other, ho)
		if otherErr == nil {
			// What an ingest that had chosen the name earlier would do next.
			if err := os.WriteFile(filepath.Join(jobDir, "early"), []byte("early"), 0o600); err != nil {
				t.Errorf("WriteFile: %v", err)
			}
		}
	}
	if err := application.RetryHistoryJob(t.Context(), id); err != nil {
		t.Errorf("RetryHistoryJob = %v, want the retry to keep its name", err)
	}
	if otherErr == nil {
		if _, err := os.Stat(filepath.Join(jobDir, "early")); err != nil {
			t.Errorf("the other job's early file is no longer in its directory: %v", err)
		}
	}
	if !errors.Is(otherErr, dispatch.ErrJobNameTaken) {
		t.Errorf("a job registering the name during the retry = %v, want ErrJobNameTaken", otherErr)
	}
	if got, err := os.ReadFile(filepath.Join(jobDir, "retained")); err != nil || string(got) != "retained" {
		t.Errorf("the retry's bytes are not at %s: %q, %v", jobDir, got, err)
	}
	if _, err := os.Lstat(failedDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the _FAILED_ directory is still there (Lstat err %v)", err)
	}
	if names := queueNames(application); len(names) != 1 || names[id] != name {
		t.Errorf("queue = %v, want only the retried job at %s", names, name)
	}
}

// TestRetryHistoryJob_AbortedRetryReturnsItsDirectoryAndItsName pins the
// unwinding: a retry that aborts after the restore moves the directory back
// to the _FAILED_ path and releases the name, so a later job can take it.
func TestRetryHistoryJob_AbortedRetryReturnsItsDirectoryAndItsName(t *testing.T) {
	t.Parallel()
	application, repo := newNameRaceApp(t)
	const name = "name-claim-abort"
	id, jobDir, failedDir := seedFailedRetry(t, application, repo, name)

	errAbort := errors.New("prepare refused")
	err := application.retryHistoryJob(t.Context(), id, false, func(*job.Job) error { return errAbort })
	if !errors.Is(err, errAbort) {
		t.Fatalf("retryHistoryJob = %v, want the prepare error", err)
	}
	if got, err := os.ReadFile(filepath.Join(failedDir, "retained")); err != nil || string(got) != "retained" {
		t.Errorf("the directory was not moved back to %s: %q, %v", failedDir, got, err)
	}
	if _, err := os.Lstat(jobDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the restored directory is still at %s (Lstat err %v)", jobDir, err)
	}
	release, err := application.dispatcher.ReserveName("someone-else", name)
	if err != nil {
		t.Fatalf("the aborted retry still holds its name: %v", err)
	}
	release()
}

// TestRetryHistoryJob_RefusesANameAQueuedJobHasBeforeMovingAnything pins that
// a queued job of the retry's name refuses it with errRetryDirConflict before
// the directory moves.
func TestRetryHistoryJob_RefusesANameAQueuedJobHasBeforeMovingAnything(t *testing.T) {
	t.Parallel()
	application, repo := newNameRaceApp(t)
	const name = "name-claim-held"
	id, jobDir, failedDir := seedFailedRetry(t, application, repo, name)

	held, hh, _ := buildNamedIngestJob(t, application, name, "held")
	if err := application.dispatcher.Add(t.Context(), held, hh); err != nil {
		t.Fatalf("Add(held): %v", err)
	}
	err := application.RetryHistoryJob(t.Context(), id)
	if !errors.Is(err, errRetryDirConflict) || !errors.Is(err, dispatch.ErrJobNameTaken) {
		t.Fatalf("RetryHistoryJob = %v, want errRetryDirConflict wrapping ErrJobNameTaken", err)
	}
	if _, err := os.Stat(filepath.Join(failedDir, "retained")); err != nil {
		t.Errorf("the refused retry moved the _FAILED_ directory: %v", err)
	}
	if _, err := os.Lstat(jobDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused retry created %s (Lstat err %v)", jobDir, err)
	}
	if _, err := repo.Get(t.Context(), id); err != nil {
		t.Errorf("the refused retry lost its history entry: %v", err)
	}
}

// TestAddJob_ChoosesAnotherNameWhileARetryHoldsItsName pins that an ingest of
// the retry's name, arriving while the retry has reserved it and nothing is on
// disk under it yet, takes the next suffix rather than failing: the registry
// refuses the reserved name, and the chooser does not offer it again.
func TestAddJob_ChoosesAnotherNameWhileARetryHoldsItsName(t *testing.T) {
	t.Parallel()
	application, _ := newNameRaceApp(t)
	release, err := application.dispatcher.ReserveName("retrying", "reserved")
	if err != nil {
		t.Fatalf("ReserveName: %v", err)
	}
	defer release()

	j, hdr, raw := buildNamedIngestJob(t, application, "reserved", "added")
	if err := application.AddJob(t.Context(), j, hdr, raw, false); err != nil {
		t.Fatalf("AddJob under a reserved name = %v, want it to take the next name", err)
	}
	if names := queueNames(application); names[j.ID()] != "reserved.1" {
		t.Errorf("queue = %v, want the added job at reserved.1", names)
	}
}
