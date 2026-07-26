package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// fakeTB wraps testing.TB to intercept cleanup handler registration and error reporting,
// allowing unit testing of test helper functions like AssertNoFDLeaks.
type fakeTB struct {
	testing.TB
	cleanups []func()
	errors   []string
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Cleanup(fn func()) {
	f.cleanups = append(f.cleanups, fn)
}

func (f *fakeTB) Errorf(format string, args ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

func (f *fakeTB) runCleanups() {
	// Run cleanups in LIFO order like Go's testing package.
	for _, fn := range slices.Backward(f.cleanups) {
		fn()
	}
}

func TestAssertNoFDLeaks_DetectsLeak(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("FD leak detection is supported only on Linux")
	}

	dir := t.TempDir()
	fake := &fakeTB{TB: t}

	AssertNoFDLeaks(fake, dir)

	// Open a file inside dir without closing it.
	filePath := filepath.Join(dir, "leaked.txt")
	f, err := os.Create(filePath)
	if err != nil {
		t.Fatalf("os.Create: %v", err)
	}
	defer f.Close() // Ensure cleanup after test assertion finishes.

	// Trigger the cleanup handler registered by AssertNoFDLeaks.
	fake.runCleanups()

	if len(fake.errors) == 0 {
		t.Fatalf("expected leak detection error for unclosed file %q, got none", filePath)
	}
	if !strings.Contains(fake.errors[0], filePath) {
		t.Errorf("expected error message to contain %q, got: %v", filePath, fake.errors[0])
	}
}

func TestAssertNoFDLeaks_NoLeak(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("FD leak detection is supported only on Linux")
	}

	dir := t.TempDir()
	fake := &fakeTB{TB: t}

	AssertNoFDLeaks(fake, dir)

	// Open and cleanly close a file inside dir.
	filePath := filepath.Join(dir, "clean.txt")
	f, err := os.Create(filePath)
	if err != nil {
		t.Fatalf("os.Create: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("f.Close: %v", err)
	}

	// Trigger the cleanup handler registered by AssertNoFDLeaks.
	fake.runCleanups()

	if len(fake.errors) > 0 {
		t.Fatalf("expected no leak errors, got: %v", fake.errors)
	}
}

func TestIsRegularOrDirPath(t *testing.T) {
	if isRegularOrDirPath("socket:[12345]") {
		t.Error("socket should be false")
	}
	if isRegularOrDirPath("/dev/null") {
		t.Error("/dev/null should be false")
	}
	if !isRegularOrDirPath("/tmp/some/file.txt") {
		t.Error("/tmp/some/file.txt should be true")
	}
}
