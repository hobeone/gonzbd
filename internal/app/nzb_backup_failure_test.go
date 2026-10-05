package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stagingFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".staging") {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestWriteNZBBackup_ALinkFailureOtherThanAnExistingNameFailsAndLeavesNoStagingFile
// pins the failure path: a name the filesystem refuses for any reason but an
// existing file (here, one longer than NAME_MAX) fails the write instead of
// choosing another name, and the staged file does not outlive the call.
func TestWriteNZBBackup_ALinkFailureOtherThanAnExistingNameFailsAndLeavesNoStagingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := writeNZBBackup(dir, strings.Repeat("a", 300)+".nzb", []byte("<nzb>x</nzb>"), nil)
	if err == nil || !strings.Contains(err.Error(), "publish NZB backup") {
		t.Fatalf("writeNZBBackup = %v, want a publish error", err)
	}
	if left := stagingFiles(t, dir); len(left) != 0 {
		t.Errorf("staging files left behind: %v", left)
	}
}

// TestWriteNZBBackup_GivesUpAfterTheAttemptBound pins the exhaustion path: when
// every chosen name is taken by another writer before the link, the call fails
// after maxNZBBackupAttempts choices and leaves no staging file.
func TestWriteNZBBackup_GivesUpAfterTheAttemptBound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	calls := 0
	_, err := writeNZBBackup(dir, "Show.nzb", []byte("<nzb>x</nzb>"), func(name string) {
		calls++
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Errorf("setup: %v", err)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "no free NZB backup name") {
		t.Fatalf("writeNZBBackup = %v, want the exhaustion error", err)
	}
	if calls != maxNZBBackupAttempts {
		t.Errorf("%d names chosen, want %d", calls, maxNZBBackupAttempts)
	}
	if left := stagingFiles(t, dir); len(left) != 0 {
		t.Errorf("staging files left behind: %v", left)
	}
}

// TestWriteNZBBackup_APanickingHookLeavesNoStagingFile pins that the staged
// file is removed on a panic between staging and the link.
func TestWriteNZBBackup_APanickingHookLeavesNoStagingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	func() {
		defer func() { _ = recover() }()
		_, _ = writeNZBBackup(dir, "Show.nzb", []byte("<nzb>x</nzb>"), func(string) { panic("hook") })
	}()
	if left := stagingFiles(t, dir); len(left) != 0 {
		t.Errorf("staging files left behind: %v", left)
	}
}
