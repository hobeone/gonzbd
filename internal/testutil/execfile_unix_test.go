//go:build unix

package testutil

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

// recordingTB adapts a *testing.T for use from a non-test goroutine.
// testing.TB.Fatalf routes to FailNow, which the testing package
// documents as illegal outside the goroutine running the test, so the
// stress test below cannot hand its *testing.T to WriteExecutable
// directly. Passing this stub instead keeps the stress test pinned to
// the real exported helper rather than to a copy of its internals.
type recordingTB struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *recordingTB) failures() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.errs...)
}

// TestWriteExecutable_NoETXTBSYUnderConcurrentForks is the regression
// pin for issue #239. Many goroutines concurrently write a stub and
// exec it, so that each goroutine's fork+exec overlaps the others'
// write windows -- the exact interleaving that leaked a write
// descriptor into an unrelated child and produced
//
//	fork/exec .../mock-unrar: text file busy
//
// on CI. With the ForkLock hold removed from withoutConcurrentFork this
// test fails reliably; with it in place, no exec may fail with ETXTBSY.
func TestWriteExecutable_NoETXTBSYUnderConcurrentForks(t *testing.T) {
	t.Parallel()
	skipIfNoShell(t)

	const (
		workers    = 12
		iterations = 60
	)

	dir := t.TempDir()
	rec := &recordingTB{TB: t}
	execErrs := make(chan error, workers*iterations)

	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range iterations {
				path := filepath.Join(dir, fmt.Sprintf("stub-%d-%d", w, i))
				WriteExecutable(rec, path, "#!/bin/sh\nexit 0\n")
				if err := exec.Command(path).Run(); err != nil {
					execErrs <- fmt.Errorf("exec %s: %w", filepath.Base(path), err)
				}
			}
		})
	}
	wg.Wait()
	close(execErrs)

	for _, msg := range rec.failures() {
		t.Errorf("WriteExecutable failed: %s", msg)
	}

	var etxtbsy, other int
	for err := range execErrs {
		if errors.Is(err, syscall.ETXTBSY) {
			etxtbsy++
		} else {
			other++
		}
		t.Errorf("exec failed: %v", err)
	}
	if etxtbsy > 0 {
		t.Errorf("got %d ETXTBSY failures across %d write+exec pairs; "+
			"the ForkLock hold in withoutConcurrentFork is not excluding "+
			"concurrent forks (golang/go#22315)", etxtbsy, workers*iterations)
	}
	if other > 0 {
		t.Errorf("got %d non-ETXTBSY exec failures", other)
	}
}
