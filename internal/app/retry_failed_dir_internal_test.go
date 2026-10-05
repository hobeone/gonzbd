package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
)

// TestRestoreFailedDir covers each arrangement of the _FAILED_ directory and
// the job directory restoreFailedDir can find, and what it does with each.
func TestRestoreFailedDir(t *testing.T) {
	t.Parallel()
	const name = "job"
	for _, tc := range []struct {
		name string
		// recordFailed records the _FAILED_ path on the entry; otherwise the
		// entry records the job directory itself.
		recordFailed bool
		failed       string // "dir", "file" or "" for absent
		jobDir       bool
		wantErr      error
		wantMoved    bool
	}{
		{name: "no rename recorded", failed: "dir"},
		{name: "renamed and free", recordFailed: true, failed: "dir", wantMoved: true},
		{name: "both gone", recordFailed: true},
		{name: "job dir already exists", recordFailed: true, failed: "dir", jobDir: true, wantErr: errRetryDirConflict},
		{name: "failed dir gone, job dir exists", recordFailed: true, jobDir: true, wantErr: errRetryDirConflict},
		{name: "failed path is a file", recordFailed: true, failed: "file", wantErr: errRetryDirConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			downloadDir := t.TempDir()
			jobDir := filepath.Join(downloadDir, name)
			failedDir := filepath.Join(downloadDir, "_FAILED_"+name)
			switch tc.failed {
			case "dir":
				if err := os.Mkdir(failedDir, 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(failedDir, "kept"), []byte("kept"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(failedDir, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.jobDir {
				if err := os.Mkdir(jobDir, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			recorded := jobDir
			if tc.recordFailed {
				recorded = failedDir
			}
			from, err := restoreFailedDir(recorded, downloadDir, name)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("restoreFailedDir error = %v, want %v", err, tc.wantErr)
			}
			if !tc.wantMoved {
				if from != "" {
					t.Errorf("restoreFailedDir moved from %q, want nothing moved", from)
				}
				if tc.failed == "dir" {
					if _, err := os.Stat(filepath.Join(failedDir, "kept")); err != nil {
						t.Errorf("the _FAILED_ directory's contents moved: %v", err)
					}
				}
				return
			}
			if from != failedDir {
				t.Errorf("restoreFailedDir moved from %q, want %q", from, failedDir)
			}
			if got, err := os.ReadFile(filepath.Join(jobDir, "kept")); err != nil || string(got) != "kept" {
				t.Errorf("job directory holds %q (err %v), want the _FAILED_ directory's file", got, err)
			}

			if err := undoRestoreFailedDir(from, downloadDir, name); err != nil {
				t.Fatalf("undoRestoreFailedDir: %v", err)
			}
			if _, err := os.Stat(filepath.Join(failedDir, "kept")); err != nil {
				t.Errorf("undo left nothing at the _FAILED_ path: %v", err)
			}
			if _, err := os.Lstat(jobDir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("undo left the job directory in place (Lstat err %v)", err)
			}
		})
	}
}

// TestQueuedName: queuedName reports a name only while a job the dispatcher
// holds carries it, and nothing without a dispatcher.
func TestQueuedName(t *testing.T) {
	t.Parallel()
	if (&Application{}).queuedName("any") {
		t.Error("queuedName = true without a dispatcher")
	}
	a := newTestApplication(t)
	if a.queuedName("held") {
		t.Error("queuedName(held) = true before any job was added")
	}
	j := job.New("queuednamejob001", "held", job.Policy{})
	if err := a.dispatcher.Add(context.Background(), j, dispatch.Header{Name: "held"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !a.queuedName("held") {
		t.Error("queuedName(held) = false for a queued job of that name")
	}
	if a.queuedName("other") {
		t.Error("queuedName(other) = true, but no queued job has that name")
	}
}

// TestRenameWithin: renameWithin renames inside base, and refuses a base that
// does not exist and a path that leaves base.
func TestRenameWithin(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	from, to := filepath.Join(base, "a"), filepath.Join(base, "b")
	if err := os.Mkdir(from, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := renameWithin(base, from, to); err != nil {
		t.Fatalf("renameWithin: %v", err)
	}
	if _, err := os.Stat(to); err != nil {
		t.Errorf("nothing at %s after the rename: %v", to, err)
	}

	if err := renameWithin(filepath.Join(base, "missing"), to, from); err == nil {
		t.Error("renameWithin succeeded under a base that does not exist")
	}

	outside := filepath.Join(t.TempDir(), "escaped")
	if err := renameWithin(base, to, outside); err == nil {
		t.Error("renameWithin moved a directory outside its base")
	}
	if _, err := os.Stat(to); err != nil {
		t.Errorf("the refused rename moved %s: %v", to, err)
	}
}

// TestUndoRestoreFailedDir_NothingMoved: undoing a restore that moved nothing
// touches nothing.
func TestUndoRestoreFailedDir_NothingMoved(t *testing.T) {
	t.Parallel()
	downloadDir := t.TempDir()
	jobDir := filepath.Join(downloadDir, "job")
	if err := os.Mkdir(jobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := undoRestoreFailedDir("", downloadDir, "job"); err != nil {
		t.Fatalf("undoRestoreFailedDir: %v", err)
	}
	if _, err := os.Stat(jobDir); err != nil {
		t.Errorf("the job directory moved: %v", err)
	}
}
