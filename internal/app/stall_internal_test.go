package app

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/storagefault"
	"github.com/hobeone/gonzbd/internal/types"
)

func testFault(op string) *storagefault.Fault {
	return &storagefault.Fault{Op: op, Path: "/data/x.bin", Err: syscall.ENOSPC}
}

// TestStallReason_ReportsNothingForAJobThatIsNotParked pins the polarity the
// API depends on: an empty reason means "not stalled", so a job that never
// stalled must not produce one.
func TestStallReason_ReportsNothingForAJobThatIsNotParked(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	if got := application.StallReason("nobody"); got.Reason != "" || !got.Since.IsZero() {
		t.Errorf("StallReason = %+v for an unknown job, want the zero value", got)
	}
}

// TestReevaluateStall_ForgetsAJobThatHasLeftTheQueue pins the disposition for a
// job removed while parked. Queue.Resume reports it as not found, and keeping
// the record would re-evaluate a job that no longer exists on every interval,
// forever.
func TestReevaluateStall_ForgetsAJobThatHasLeftTheQueue(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	application.noteStall("gone", testFault("write"), true)

	application.reevaluateStall("gone")

	if got := application.stalledJobIDs(); len(got) != 0 {
		t.Errorf("stalledJobIDs = %v, want empty for a job the queue does not have", got)
	}
}

// TestReevaluateStalls_CoversEveryParkedJob pins that the sweep is a sweep. A
// loop that stopped at the first job would leave every other stalled download
// parked behind one wedged mount.
func TestReevaluateStalls_CoversEveryParkedJob(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	application.noteStall("gone-a", testFault("write"), true)
	application.noteStall("gone-b", testFault("write"), true)

	application.reevaluateStalls(t.Context())

	if got := application.stalledJobIDs(); len(got) != 0 {
		t.Errorf("stalledJobIDs = %v, want empty — a sweep that stopped at the first job "+
			"leaves the rest parked behind it", got)
	}
}

// TestReevaluateStalls_StopsOnACancelledContext pins that the sweep honours
// cancellation, so shutdown never waits on it.
func TestReevaluateStalls_StopsOnACancelledContext(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	application.noteStall("gone-a", testFault("write"), true)
	application.noteStall("gone-b", testFault("write"), true)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	application.reevaluateStalls(ctx)

	if got := application.stalledJobIDs(); len(got) != 2 {
		t.Errorf("stalledJobIDs = %v, want both still parked — the sweep ran anyway on a "+
			"cancelled context", got)
	}
}

// TestReevaluateStalls_KickIsDeliveredOnceAndNeverBlocks pins the property the
// API handler depends on. ReevaluateStalls is called from an HTTP request, so
// the call must return immediately however many times it is made.
func TestReevaluateStalls_KickIsDeliveredOnceAndNeverBlocks(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			application.ReevaluateStalls()
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ReevaluateStalls blocked; an HTTP handler would hang behind the stall loop")
	}
	if n := len(application.stallKick); n != 1 {
		t.Errorf("queued kicks = %d, want 1 — a re-evaluation already queued does the same "+
			"work, so a second is not a lost request", n)
	}
}

// TestRunStallRecheck_ReevaluatesStallsOnItsTicker pins the R19 seam itself:
// it drives the real loop and asserts the job actually leaves Paused, which
// only happens if the ticker arm reaches reevaluateStalls.
func TestRunStallRecheck_ReevaluatesStallsOnItsTicker(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t, WithStallRecheckInterval(10*time.Millisecond))
	job := addStallTestJob(t, application, "ticker-job")
	application.Stall(job.ID(), testFault("write"))
	if row, ok := application.dispatcher.Row(job.ID()); !ok || row.Status() != constants.StatusPaused {
		t.Fatalf("status = %v after Stall, want Paused; the fixture is not parked", row.Status())
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go application.runStallRecheck(ctx)

	waitForResumed(t, application, job.ID(),
		"the stall loop's ticker never reached reevaluateStalls; R19's interval "+
			"half is wired to nothing and a parked job waits for a restart")
}

// TestRunStallRecheck_ReevaluatesStallsOnAKick pins the other arm — R19's "on
// user action" — through the same real loop, so that ReevaluateStalls is shown
// to reach reevaluateStalls rather than only to increment something.
func TestRunStallRecheck_ReevaluatesStallsOnAKick(t *testing.T) {
	t.Parallel()
	// An interval far longer than the test, so only the kick can explain a
	// resumed job.
	application, _, _ := newLifecycleTestApp(t, WithStallRecheckInterval(time.Hour))
	job := addStallTestJob(t, application, "kick-job")
	application.Stall(job.ID(), testFault("write"))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go application.runStallRecheck(ctx)

	application.ReevaluateStalls()

	waitForResumed(t, application, job.ID(),
		"ReevaluateStalls did not reach the stall loop; resuming a job from the API "+
			"leaves it parked until the interval or a restart")
}

// addStallTestJob adds a one-file job to the application's queue.
func addStallTestJob(t *testing.T, application *Application, name string) *job.Job {
	t.Helper()
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  name + ".bin",
		Bytes:    100,
		Articles: []nzb.Article{{ID: name + "0@t", Bytes: 100, Number: 1}},
	}}}
	j, hdr, err := BuildIngestJob(application.config, parsed, name+".nzb", types.FetchOptions{NzbName: name}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.Dispatcher().Add(context.Background(), j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return j
}

// waitForResumed blocks until the job is off Paused, or fails with why.
func waitForResumed(t *testing.T, application *Application, jobID, why string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		row, ok := application.dispatcher.Row(jobID)
		if ok && row.Status() != constants.StatusPaused {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(why)
}

// TestSetStallReasonLocked_CreatesTheRecordItNeeds: setStallReasonLocked may
// be the FIRST thing that parks a job, so a helper that only updated an
// existing record would silently drop the reason and leave the job off the
// stalled list, unreachable by any re-evaluation.
func TestSetStallReasonLocked_CreatesTheRecordItNeeds(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)

	application.stallMu.Lock()
	application.setStallReasonLocked("job-1", "Stalled: first", true)
	application.setStallReasonLocked("job-1", "Stalled: second", true)
	rec := application.stalls["job-1"]
	application.stallMu.Unlock()

	if rec == nil {
		t.Fatal("no record was created, so the job is not on the stalled list and no " +
			"re-evaluation will ever visit it")
	}
	if rec.reason != "Stalled: second" {
		t.Errorf("reason = %q, want the newer text", rec.reason)
	}
}
