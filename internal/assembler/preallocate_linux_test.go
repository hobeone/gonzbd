//go:build linux

package assembler

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPreallocateLinuxFallback(t *testing.T) {
	// Opening /proc/self/coredump_filter for writing.
	// fallocate on /proc files returns ENOTSUP/EOPNOTSUPP, forcing the fallback path.
	f, err := os.OpenFile("/proc/self/coredump_filter", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("Skipping test: cannot open coredump_filter: %v", err)
	}
	defer f.Close()

	// Call preallocateFile. It should fail fallocate with EOPNOTSUPP,
	// fall back to Truncate, which will succeed and return nil.
	err = preallocateFile(f, 1024)
	if err != nil {
		t.Errorf("expected no error from preallocating /proc file with fallback, got: %v", err)
	}
}

func TestPreallocateLinuxError(t *testing.T) {
	// A pipe does not support fallocate and returns ESPIPE (not EOPNOTSUPP),
	// which will directly return the error without falling back to Truncate.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	err = preallocateFile(w, 1024)
	if err == nil {
		t.Error("expected error from preallocating pipe, got nil")
	}
	if !errors.Is(err, unix.ESPIPE) {
		t.Errorf("expected ESPIPE error, got %v", err)
	}
}
