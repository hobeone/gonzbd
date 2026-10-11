package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAcquireRunLock_ConflictsAcrossDifferentTMPDIRs(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init: %v\n%s", err, out)
	}

	t.Setenv("TMPDIR", t.TempDir())
	release, err := acquireRunLock(root)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()

	t.Setenv("TMPDIR", t.TempDir())
	if _, err := acquireRunLock(root); err == nil {
		t.Fatal("a second run with a different TMPDIR acquired the lock; the lock must not depend on TMPDIR")
	}
	if want := filepath.Join(root, ".git"); !strings.HasPrefix(runLockPath(root), want) {
		t.Errorf("lock path %q is not inside the tree's git dir %q", runLockPath(root), want)
	}
}
