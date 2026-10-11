package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAcquireRunLock_RefusesASecondRunInTheSameTree(t *testing.T) {
	root := t.TempDir()
	release, err := acquireRunLock(root)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	_, err = acquireRunLock(root)
	if err == nil {
		t.Fatal("second acquire in the same tree succeeded; two runs would mutate each other's source")
	}
	if !strings.Contains(err.Error(), runLockPath(root)) {
		t.Errorf("refusal %q does not name the lock path %q", err, runLockPath(root))
	}

	release()
	release2, err := acquireRunLock(root)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()
}

func TestAcquireRunLock_DifferentTreesDoNotBlockEachOther(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	relA, err := acquireRunLock(a)
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	defer relA()
	relB, err := acquireRunLock(b)
	if err != nil {
		t.Fatalf("acquire b while a is held: %v (sibling worktrees must not serialize)", err)
	}
	relB()
}

func TestRunSpec_RefusesWhileAnotherRunHoldsTheTree(t *testing.T) {
	root := t.TempDir()
	release, err := acquireRunLock(root)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	var code int
	_, errOut := captureOutput(t, func() {
		code = runSpec(root, filepath.Join(root, "unread.spec"), runOpts{})
	})
	if code == 0 {
		t.Fatal("runSpec exited 0 while another run held the tree")
	}
	if !strings.Contains(errOut, "another mutate run is active") || !strings.Contains(errOut, runLockPath(root)) {
		t.Errorf("stderr does not explain the refusal and name the lock:\n%s", errOut)
	}
}

func TestRunCheck_IgnoresTheRunLock(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "target.go"), "package p\n\nconst V = 1\n")
	spec := filepath.Join(root, "t.spec")
	mustWrite(t, spec, "pkg .\n\n[m]\nfile target.go\n--- anchor\nconst V = 1\n--- replace\nconst V = 2\n--- end\n")

	release, err := acquireRunLock(root)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	var code int
	captureOutput(t, func() { code = runCheck(root, []string{spec}, []string{spec}) })
	if code != 0 {
		t.Errorf("runCheck = %d while the tree is locked, want 0: -check only reads", code)
	}
}
