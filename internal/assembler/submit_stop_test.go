package assembler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/durability"
)

// TestSubmit_ADeadlineFreeCallerIsReleasedByStop pins the case the reply
// select had no arm for.
//
// The SEND select always carried a stopCh case; the reply select did not. So a
// caller with no deadline that had already handed its request to the worker
// waited on op.reply forever once the worker parked in an fsync — and the
// deferred wg.Done never ran, so Stop's own wg.Wait blocked behind it and the
// process could not exit. One wedged mount took the whole daemon down rather
// than one job.
//
// SyncTarget.Drain and Sync are the deadline-free callers in practice: the
// interface hands them the caller's context, and a caller that supplies
// context.Background() is exactly what this reproduces.
func TestSubmit_ADeadlineFreeCallerIsReleasedByStop(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 1)

	release := make(chan struct{})
	var releaseOnce sync.Once
	unpark := func() { releaseOnce.Do(func() { close(release) }) }
	blocked := make(chan struct{}, 1)
	opts := makeOpts(dir, files)
	inner := opts.FileInfo
	opts.FileInfo = func(jobID string, fileIdx int) (FileInfo, error) {
		select {
		case blocked <- struct{}{}:
		default:
		}
		<-release // hold the worker, as a wedged mount would
		return inner(jobID, fileIdx)
	}

	a := New(opts, nil)
	if err := a.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(unpark)

	_ = a.WriteArticle(t.Context(), WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("AAAA"), MessageID: "m1",
	})
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never parked, so this test proves nothing")
	}

	// A deadline-free wait for a reply the parked worker will never send.
	tgt := &jobSyncTarget{a: a, jobID: "job1"}
	done := make(chan error, 1)
	go func() {
		//nolint:usetesting // a deadline-free context is exactly what is under test
		_, err := tgt.Drain(context.Background(), 0)
		done <- err
	}()

	// Give the submit time to get past the send and onto the reply wait, which
	// is the state with no arm for Stop.
	time.Sleep(200 * time.Millisecond)

	stopped := make(chan struct{})
	go func() {
		_ = a.Stop()
		close(stopped)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrAssemblerStopped) {
			t.Errorf("err = %v, want ErrAssemblerStopped", err)
		}
		// The boundary rule: a stopped assembler is not a storage condition,
		// so it must not reach storagefault.Classify — which defaults
		// everything it does not recognise to retryable and would park a
		// healthy job on a disk that did not fail.
		if !errors.Is(err, durability.ErrTargetUnavailable) {
			t.Errorf("err = %v, want it wrapped in durability.ErrTargetUnavailable so the "+
				"barrier does not classify an ordinary shutdown as a storage fault", err)
		}
	case <-time.After(3 * barrierOpTimeout):
		t.Fatal("the deadline-free caller was never released; its deferred wg.Done " +
			"cannot run, so Stop's wg.Wait blocks behind it and the process cannot exit")
	}

	// The worker itself is still parked, and Go cannot interrupt it, so Stop
	// cannot return until the mount answers. That is the intended division —
	// a wedged mount stalls the job, never the process — and it is why this
	// releases the worker before asserting Stop finishes. What is under test
	// is that the SUBMIT is not also holding wg.Wait open: without its stopCh
	// case it never returns even after the worker does, because the reply for
	// its op was consumed by nobody.
	unpark()
	select {
	case <-stopped:
	case <-time.After(3 * barrierOpTimeout):
		t.Fatal("Stop did not return: wg.Wait is blocked behind a submit that " +
			"nothing releases")
	}
}
