package testutil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func skipIfNoShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script tests not supported on Windows")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("skipping: /bin/sh not available: %v", err)
	}
}

func TestWriteExecutable(t *testing.T) {
	t.Parallel()
	skipIfNoShell(t)

	// A nested path exercises the parent-directory creation.
	path := filepath.Join(t.TempDir(), "nested", "dir", "stub.sh")
	got := WriteExecutable(t, path, "#!/bin/sh\nexit 7\n")

	if got != path {
		t.Errorf("WriteExecutable returned %q, want %q", got, path)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if want := "#!/bin/sh\nexit 7\n"; string(content) != want {
		t.Errorf("content = %q, want %q", content, want)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o755 {
		t.Errorf("perm = %04o, want 0755", perm)
	}

	// The written file must actually be executable, and its exit status
	// must survive -- this is what every caller depends on.
	err = exec.Command(path).Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("exec: got err %v, want *exec.ExitError", err)
	}
	if code := exitErr.ExitCode(); code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
}

// TestWriteAndClose_OpenError covers the open-failure branch. It targets
// writeAndClose directly rather than WriteExecutable, because the latter
// reports failure via t.Fatalf and so cannot be observed from a passing
// test.
func TestWriteAndClose_OpenError(t *testing.T) {
	t.Parallel()

	// A directory can never be opened O_WRONLY, so this fails with EISDIR.
	dirAsTarget := filepath.Join(t.TempDir(), "iam-a-dir")
	if err := os.Mkdir(dirAsTarget, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	err := writeAndClose(dirAsTarget, "#!/bin/sh\n", os.OpenFile)
	if err == nil {
		t.Fatal("writeAndClose on a directory returned nil, want an error")
	}
	if !errors.Is(err, syscall.EISDIR) {
		t.Errorf("err = %v, want EISDIR", err)
	}
}

// TestWriteAndClose_WriteError covers the write-failure branch, which a
// healthy filesystem will not produce on its own. The opener is passed in
// per call, so nothing is shared and the test is safe to parallelise.
func TestWriteAndClose_WriteError(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "stub.sh")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("pre-create: %v", err)
	}

	// A read-only descriptor makes the write fail with EBADF while
	// leaving the close path reachable.
	readOnly := func(name string, _ int, _ os.FileMode) (*os.File, error) {
		return os.Open(name) //nolint:gosec // G304: test-controlled path under t.TempDir()
	}

	err := writeAndClose(path, "#!/bin/sh\n", readOnly)
	if err == nil {
		t.Fatal("writeAndClose with a read-only descriptor returned nil, want an error")
	}
	if !errors.Is(err, syscall.EBADF) {
		t.Errorf("err = %v, want EBADF", err)
	}
}

func TestWriteExecutable_TruncatesExistingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "stub.sh")
	WriteExecutable(t, path, "#!/bin/sh\n# a much longer first version\n")
	WriteExecutable(t, path, "#!/bin/sh\n")

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if want := "#!/bin/sh\n"; string(content) != want {
		t.Errorf("content = %q, want %q -- O_TRUNC did not take effect", content, want)
	}
}
