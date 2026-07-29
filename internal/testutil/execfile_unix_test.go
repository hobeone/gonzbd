//go:build unix

package testutil

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
	r.mu.Unlock()

	// Mirror Fatalf's control flow rather than returning, which would let
	// WriteExecutable fall through to "return path" for a file it never
	// wrote -- the caller would then exec a missing file and report a
	// phantom exec failure instead of this write failure. Goexit is legal
	// here because the failure is recorded above rather than routed
	// through testing's FailNow, and sync.WaitGroup.Go documents that it
	// still decrements when f "completed ... abruptly using goexit".
	runtime.Goexit()
}

func (r *recordingTB) failures() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.errs...)
}

// TestRecordingTB_FatalfRecordsAndAborts pins the two properties the
// stress test below relies on: a WriteExecutable failure inside a worker
// goroutine is recorded, and it stops that worker instead of continuing
// with a path that was never written. Without the runtime.Goexit, the
// "unreachable" marker would be appended and the worker would go on to
// exec a nonexistent file.
func TestRecordingTB_FatalfRecordsAndAborts(t *testing.T) {
	t.Parallel()

	rec := &recordingTB{TB: t}

	var wg sync.WaitGroup
	wg.Go(func() {
		// /proc/<x> is not creatable, so MkdirAll fails and
		// WriteExecutable calls Fatalf on our stub.
		WriteExecutable(rec, "/proc/gonzbd-not-creatable/stub.sh", "#!/bin/sh\n")
		rec.Fatalf("unreachable: Fatalf returned instead of aborting the goroutine")
	})
	wg.Wait() // must not hang: wg.Go still calls Done after a Goexit.

	got := rec.failures()
	if len(got) != 1 {
		t.Fatalf("recorded %d failures, want exactly 1: %q", len(got), got)
	}
	if !strings.Contains(got[0], "mkdir") {
		t.Errorf("recorded failure = %q, want the mkdir failure", got[0])
	}
}

// TestWriteExecutable_ModeIsUmaskIndependent pins the documented 0o755
// mode against the process umask. OpenFile's perm argument is masked by
// the umask, so under umask 0o027 the file would come out 0o750, and
// under a umask carrying an execute bit it would come out non-executable
// -- silently defeating the point of the helper. CI happens to run with a
// permissive umask, so this sets a restrictive one explicitly rather than
// trusting the ambient value.
//
// No t.Parallel(): syscall.Umask is process-wide. Go runs serial tests to
// completion before releasing any parallel ones, and the Cleanup restores
// the old value before that barrier opens.
func TestWriteExecutable_ModeIsUmaskIndependent(t *testing.T) {
	old := syscall.Umask(0o027)
	t.Cleanup(func() { syscall.Umask(old) })

	path := WriteExecutable(t, filepath.Join(t.TempDir(), "stub.sh"), "#!/bin/sh\n")

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o755 {
		t.Errorf("perm = %04o under umask 0027, want 0755", perm)
	}
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
