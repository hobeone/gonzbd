package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/history"
)

// TestRetryHistoryJob_UnwritableManifestDirLeavesTheEntryRetryable pins what a
// retry does when its queue manifest cannot be written: it reports why, and it
// leaves the job exactly where it was — a FAILED history entry with its NZB
// backup, not in the queue — so the user can retry again once the directory
// is fixed.
//
// A regular file where the manifest directory belongs makes MkdirAll fail
// with ENOTDIR, and touches nothing the rest of the retry needs: the history
// database lives directly under the admin directory.
func TestRetryHistoryJob_UnwritableManifestDirLeavesTheEntryRetryable(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newLifecycleTestApp(t)

	const jobID = "feedface0000beef"
	const nzbBackup = "retry-unwritable.nzb.gz"
	writeRetryNZBBackup(t, adminDir, nzbBackup, retryFixtureNZB(1))
	if err := repo.Add(t.Context(), history.Entry{
		NzoID: jobID, Name: "retry-unwritable", NzbName: "retry-unwritable.nzb",
		NZBBackup: nzbBackup, Category: "*", Status: "Failed", Completed: time.Now(),
	}); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	mdir := manifestDir(adminDir)
	if err := os.MkdirAll(filepath.Dir(mdir), 0o750); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(mdir), err)
	}
	if err := os.WriteFile(mdir, nil, 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", mdir, err)
	}

	err := application.RetryHistoryJob(t.Context(), jobID)
	if err == nil {
		t.Fatal("RetryHistoryJob returned nil although its manifest directory cannot be created")
	}
	if !strings.Contains(err.Error(), "mkdir manifests") {
		t.Errorf("error = %q, want it to name the failed mkdir; the user is told the retry "+
			"failed but not why", err)
	}

	entry, gErr := repo.Get(t.Context(), jobID)
	if gErr != nil {
		t.Fatalf("history entry is gone after a retry that never entered the queue: %v", gErr)
	}
	if entry.Status != "Failed" {
		t.Errorf("history status = %q, want Failed; the job can no longer be retried", entry.Status)
	}
	if _, held := application.dispatcher.Job(jobID); held {
		t.Error("the dispatcher holds a job whose manifest was never written; the first " +
			"eviction settles it Failed permanently")
	}
	if _, err := os.Stat(filepath.Join(adminDir, "nzb", nzbBackup)); err != nil {
		t.Errorf("NZB backup was removed although the history entry still owns it: %v", err)
	}
}
