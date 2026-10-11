package app

import (
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestReevaluateStall_TimerLeavesAVerifyFaultForTheOperator pins the dispatch
// on stallKind directly: the timed re-evaluation leaves a verification fault
// paused with its reason, and an explicit resume lifts it.
func TestReevaluateStall_TimerLeavesAVerifyFaultForTheOperator(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 2)
	application.parkVerifyFault(j.ID(), storagefault.Classify("verify", "/mnt/dl/a.bin", syscall.EIO))
	if j.Intent() != job.IntentPause || !application.weParked(j.ID()) {
		t.Fatal("parkVerifyFault did not park an unpaused job as this application's")
	}

	application.reevaluateStall(j.ID(), byTimer)

	if j.Intent() != job.IntentPause {
		t.Error("the timed re-evaluation resumed a job parked on a verification fault")
	}
	if application.StallReason(j.ID()).Reason == "" {
		t.Error("the timed re-evaluation cleared a verification fault's reason; the job " +
			"would sit paused with nothing saying why")
	}

	application.reevaluateStall(j.ID(), byResume)

	if j.Intent() == job.IntentPause {
		t.Error("an explicit resume left a job parked on a verification fault paused")
	}
	if got := application.StallReason(j.ID()).Reason; got != "" {
		t.Errorf("stall reason %q survived an explicit resume", got)
	}
}

// TestReevaluateStall_TimerStillResumesAWriteFault is the other half: Stall's
// retryable fault is still lifted by the timer, so a full disk the user
// clears gets the job moving without a resume.
func TestReevaluateStall_TimerStillResumesAWriteFault(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 2)
	application.Stall(j.ID(), storagefault.Classify("write", "/mnt/dl/a.bin", syscall.ENOSPC))
	if j.Intent() != job.IntentPause {
		t.Fatal("Stall did not pause the job")
	}

	application.reevaluateStall(j.ID(), byTimer)

	if j.Intent() == job.IntentPause {
		t.Error("the timed re-evaluation left a job parked on a write fault paused")
	}
	if got := application.StallReason(j.ID()).Reason; got != "" {
		t.Errorf("stall reason %q survived the timed re-evaluation that resumed the job", got)
	}
}

// TestReevaluateStall_VerifyFaultOnAPausedJobKeepsItsReason pins the startup
// verifier's case (verifyPausedJobs): the job was paused when the fault came,
// so the pause is not claimed. The timer leaves the reason shown; the user's
// resume clears it and leaves the job's intent to that resume.
func TestReevaluateStall_VerifyFaultOnAPausedJobKeepsItsReason(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 2)
	if err := application.dispatcher.PauseJob(j.ID()); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	application.parkVerifyFault(j.ID(), storagefault.Classify("verify", "/mnt/dl/a.bin", syscall.EIO))
	if application.weParked(j.ID()) {
		t.Fatal("parkVerifyFault claimed a pause that predates it")
	}

	application.reevaluateStall(j.ID(), byTimer)
	if application.StallReason(j.ID()).Reason == "" {
		t.Error("the timed re-evaluation cleared a verification fault's reason")
	}

	application.reevaluateStall(j.ID(), byResume)
	if got := application.StallReason(j.ID()).Reason; got != "" {
		t.Errorf("stall reason %q survived an explicit resume", got)
	}
	if j.Intent() != job.IntentPause {
		t.Error("the re-evaluation undid a pause it did not make")
	}
}

// TestLooseRecord_LastingVerificationFaultStaysParked pins that a verification
// fault that does not clear keeps the job parked until an explicit resume
// (#828). A timed re-evaluation that resumed it would have the tick hydrate
// the job again, on the dispatcher's single goroutine, and re-read every
// recorded row until it reached the same unreadable range, once per
// stallRecheckInterval for as long as the sector stays bad.
//
// It counts the reads of the failing row: each is one full read-back pass.
//
// Not parallel: preadAt is a package-level seam.
func TestLooseRecord_LastingVerificationFaultStaysParked(t *testing.T) {
	env := newLREnv(t)
	a1 := env.newApp(t)
	j := a1.addJob(t, "lasting", 3, 2)
	env.writeFileA(t, j, 3, 0, 1)
	a1.recordFileA(t, j.ID(), false, 0, 1)

	var failing atomic.Bool
	failing.Store(true)
	var passes atomic.Int32
	prev := preadAt
	preadAt = func(f *os.File, b []byte, off int64) (int, error) {
		if off == lrArtLen && failing.Load() {
			passes.Add(1)
			return 0, &os.PathError{Op: "pread", Path: f.Name(), Err: syscall.EIO}
		}
		return prev(f, b, off)
	}
	t.Cleanup(func() { preadAt = prev })

	const recheck = 10 * time.Millisecond
	a2 := env.newApp(t, func(a *Application) { a.stallRecheckInterval = recheck })
	a2.start(t)
	j2 := a2.registered(t, j.ID())

	path := env.filePath(j)
	parked := func() bool {
		return strings.Contains(a2.StallReason(j.ID()).Reason, path) && j2.Intent() == job.IntentPause
	}
	lrWaitFor(t, "the verification fault to park the job", parked)

	// Many re-evaluation intervals. Each one that resumed the job would cost
	// another pass.
	time.Sleep(30 * recheck)
	if got := passes.Load(); got != 1 {
		t.Fatalf("read-back passes = %d over %d re-evaluation intervals, want 1: the timed "+
			"re-evaluation resumed a job parked on a verification fault, and the tick re-read it", got, 30)
	}
	if !parked() {
		t.Fatalf("job not parked with its reason after the intervals: intent %v, reason %q",
			j2.Intent(), a2.StallReason(j.ID()).Reason)
	}

	// The operator's resume re-verifies: one more pass, which faults and parks
	// the job again, and then nothing more.
	a2.ReevaluateStalls()
	lrWaitFor(t, "an explicit resume to re-verify the job", func() bool { return passes.Load() >= 2 })
	lrWaitFor(t, "the re-verification's fault to park the job again", parked)
	time.Sleep(30 * recheck)
	if got := passes.Load(); got != 2 {
		t.Fatalf("read-back passes = %d after one explicit resume, want 2", got)
	}

	failing.Store(false)
	a2.ReevaluateStalls()
	lrWaitFor(t, "a resume after the fault cleared to verify and attach the job", func() bool {
		return articleDone(j2, 0) && articleDone(j2, 1)
	})
	if got := a2.StallReason(j.ID()).Reason; got != "" {
		t.Errorf("stall reason %q survived a resume whose verification succeeded", got)
	}
}
