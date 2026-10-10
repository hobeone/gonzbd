package assembler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestOpenJobIDs_IsBoundedWhileTheWorkerIsBlocked pins that OpenJobIDs, like
// OpenFiles, Stat and CloseFile, wraps the caller's context in
// barrierOpTimeout, so a worker blocked in an fsync cannot make a caller with a
// long-lived context wait forever.
//
// The worker is blocked here inside the FileInfo resolver rather than inside a
// real fsync: what is under test is the WAIT for the worker's reply, and the
// helper cannot tell which syscall is holding it up.
func TestOpenJobIDs_IsBoundedWhileTheWorkerIsBlocked(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 1)

	release := make(chan struct{})
	blocked := make(chan struct{}, 1)
	opts := makeOpts(dir, files)
	opts.BarrierOpTimeout = 20 * time.Millisecond
	inner := opts.FileInfo
	opts.FileInfo = func(jobID string, fileIdx int) (FileInfo, error) {
		select {
		case blocked <- struct{}{}:
		default:
		}
		<-release // hold the worker goroutine, as a wedged mount would
		return inner(jobID, fileIdx)
	}

	a := startAssembler(t, opts)
	t.Cleanup(func() { close(release) })

	// Park the worker inside the resolver.
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("AAAA"), MessageID: "m1",
	})
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never entered the resolver, so it is not blocked and this test proves nothing")
	}

	type result struct {
		err error
		dur time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		//nolint:usetesting // a deadline-free context is exactly what is under test
		_, err := a.OpenJobIDs(context.Background())
		done <- result{err: err, dur: time.Since(start)}
	}()

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("OpenJobIDs succeeded while the worker was blocked; it cannot have asked it anything")
		}
		// A bare context error is what this used to return, and it is what
		// made the barrier park a healthy job: storagefault.Classify matches
		// no errno for one and defaults to retryable, so an ordinary shutdown
		// looked identical to a dying disk. OUR bound expiring IS a storage
		// condition — the worker is parked in a syscall — so it is named as
		// one here rather than left to be guessed downstream.
		if errors.Is(got.err, durability.ErrTargetUnavailable) {
			t.Errorf("err = %v, want a storage fault: our own bound expiring is evidence "+
				"about the device, not about the caller", got.err)
		}
		f, ok := errors.AsType[*storagefault.Fault](got.err)
		if !ok {
			t.Fatalf("err = %v (%T), want a *storagefault.Fault", got.err, got.err)
		}
		if f.Permanent {
			t.Errorf("fault = %v, want retryable: a mount that stops answering usually comes back", f)
		}
		if !errors.Is(got.err, errWorkerUnresponsive) {
			t.Errorf("err = %v, want it to name the unresponsive worker", got.err)
		}
		if got.dur > 3*a.BarrierOpTimeout() {
			t.Errorf("returned after %v, want roughly %v", got.dur, a.BarrierOpTimeout())
		}
	case <-time.After(3 * a.BarrierOpTimeout()):
		t.Fatal("OpenJobIDs did not return while the worker was blocked")
	}
}
