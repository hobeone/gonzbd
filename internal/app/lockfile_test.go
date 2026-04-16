//go:build unix

package app

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAcquireLockfileRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sabnzbd.lock")

	lf, err := AcquireLockfile(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lockfile: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse pid %q: %v", string(data), err)
	}
	if pid != os.Getpid() {
		t.Errorf("pid in lockfile = %d; want %d", pid, os.Getpid())
	}

	if got := lf.Path(); got != path {
		t.Errorf("Path() = %q; want %q", got, path)
	}

	if err := lf.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := lf.Release(); err != nil {
		t.Errorf("second release: %v; want nil", err)
	}
}

func TestAcquireLockfileSecondFails(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sabnzbd.lock")

	first, err := AcquireLockfile(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	t.Cleanup(func() { _ = first.Release() })

	if _, err := AcquireLockfile(path); !errors.Is(err, ErrLocked) {
		t.Errorf("second acquire err = %v; want ErrLocked", err)
	}
}

func TestAcquireLockfileReacquiresAfterRelease(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sabnzbd.lock")

	first, err := AcquireLockfile(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	second, err := AcquireLockfile(path)
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	t.Cleanup(func() { _ = second.Release() })
}

func TestAcquireLockfileMissingDir(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing-dir", "sabnzbd.lock")

	_, err := AcquireLockfile(path)
	if err == nil {
		t.Fatal("expected error; got nil")
	}
	if errors.Is(err, ErrLocked) {
		t.Errorf("err = ErrLocked; want open failure")
	}
}
