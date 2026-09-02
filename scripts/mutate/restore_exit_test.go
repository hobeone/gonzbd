package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fatalRestoring exits the process, so it is exercised in a child rather than
// asserted about. The re-exec idiom is the standard way to test an os.Exit
// path: the parent runs this same test binary with a marker in the
// environment, and the child takes the branch below.
//
// What is being pinned is that the file on disk is the ORIGINAL after the exit
// — the deferred restore in run cannot run through os.Exit, so an early exit
// that does not restore for itself leaves the working tree mutated with the
// only good copy in a temp dir nobody was told about.
const restoringChildEnv = "MUTATE_TEST_FATAL_RESTORING"

func TestFatalRestoring_PutsTheFileBackBeforeExiting(t *testing.T) {
	if os.Getenv(restoringChildEnv) == "1" {
		runFatalRestoringChild()
		return
	}

	path := filepath.Join(t.TempDir(), "target.go")
	const originalBody = "package p\n\nconst Original = 1\n"
	mustWrite(t, path, originalBody)

	// G204: the binary is this test's own, and the only operand is a -run
	// pattern written here.
	cmd := exec.Command(os.Args[0], "-test.run", "^TestFatalRestoring_PutsTheFileBackBeforeExiting$") //nolint:gosec // G204: re-exec of the test binary itself
	cmd.Env = append(os.Environ(), restoringChildEnv+"=1", "MUTATE_TEST_PATH="+path)
	out, err := cmd.CombinedOutput()

	// The child must die: fatalRestoring restores and then exits non-zero.
	if err == nil {
		t.Fatalf("the child exited 0; fatalRestoring must still terminate.\n%s", out)
	}

	got, rerr := os.ReadFile(path) //nolint:gosec // G304: path is this test's own temp file
	if rerr != nil {
		t.Fatalf("read back: %v", rerr)
	}
	if string(got) != originalBody {
		t.Errorf("file after the exit = %q, want the original %q; the working tree was left mutated", got, originalBody)
	}
}

// runFatalRestoringChild reproduces run's own sequence — back up, write the
// mutation, then leave through fatalRestoring — which is what any early exit
// between the write and the verdict performs.
//
// It calls writeBackup rather than placing the copy itself, because restore
// removes filepath.Dir(backup) on the way out: a backup beside the target
// takes the target with it, and the pairing of the two is part of what this
// exercises.
func runFatalRestoringChild() {
	path := os.Getenv("MUTATE_TEST_PATH")
	original, err := os.ReadFile(path) //nolint:gosec // G304: path comes from the parent test
	if err != nil {
		fatal("child could not read the target: %v", err)
	}
	backup, err := writeBackup(path, original)
	if err != nil {
		fatal("child could not back up: %v", err)
	}
	if werr := writeFile(path, []byte("package p\n\nconst Mutated = 2\n"), 0o600); werr != nil {
		fatal("child could not write the mutation: %v", werr)
	}
	fatalRestoring(path, backup, original, "simulated launch failure")
}
