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

// TestReevaluateStall_LeavesAVerifyFaultUntilTheJobIsResumed pins the dispatch
// on stallKind directly. A re-evaluation of a verification fault on a job that
// is still paused (the interval's, a queue-wide resume's, or another job's
// resume) leaves it paused with its reason; once the job itself is resumed,
// the re-evaluation lifts it.
func TestReevaluateStall_LeavesAVerifyFaultUntilTheJobIsResumed(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 2)
	application.parkVerifyFault(j, storagefault.Classify("verify", "/mnt/dl/a.bin", syscall.EIO))
	if j.Intent() != job.IntentPause || !application.weParked(j.ID()) {
		t.Fatal("parkVerifyFault did not park an unpaused job as this application's")
	}

	application.reevaluateStall(j.ID())

	if j.Intent() != job.IntentPause {
		t.Error("a re-evaluation resumed a job parked on a verification fault that nobody resumed")
	}
	if application.StallReason(j.ID()).Reason == "" {
		t.Error("a re-evaluation cleared the reason of a job still parked on a verification fault; " +
			"the job would sit paused with nothing saying why")
	}

	if err := application.dispatcher.ResumeJobByUser(j.ID()); err != nil {
		t.Fatalf("ResumeJobByUser: %v", err)
	}
	application.reevaluateStall(j.ID())

	if got := application.StallReason(j.ID()).Reason; got != "" {
		t.Errorf("stall reason %q survived the re-evaluation after the user resumed the job", got)
	}
}

// TestReevaluateStall_TimerStillResumesAWriteFault is the other half: Stall's
// retryable fault is still lifted by the timer, so a full disk the user
// clears gets the job moving without a resume.
func TestReevaluateStall_TimerStillResumesAWriteFault(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 2)
	application.Stall(j, storagefault.Classify("write", "/mnt/dl/a.bin", syscall.ENOSPC))
	if j.Intent() != job.IntentPause {
		t.Fatal("Stall did not pause the job")
	}

	application.reevaluateStall(j.ID())

	if j.Intent() == job.IntentPause {
		t.Error("the timed re-evaluation left a job parked on a write fault paused")
	}
	if got := application.StallReason(j.ID()).Reason; got != "" {
		t.Errorf("stall reason %q survived the timed re-evaluation that resumed the job", got)
	}
}

// TestReevaluateStall_VerifyFaultOnAPausedJobKeepsItsReason pins the case of a
// job that was already paused when its hydration faulted, reached through
// Dispatcher.SetName's LoadProgress on a paused job: the pause is not claimed.
// A re-evaluation while the job is still paused keeps the reason: clearing it
// would leave the job paused at its seeded estimate (0 % when the record holds
// nothing) with nothing saying why, and nothing would re-verify it. The re-evaluation after the user resumes the job clears it.
func TestReevaluateStall_VerifyFaultOnAPausedJobKeepsItsReason(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 2)
	if err := application.dispatcher.PauseJob(j.ID()); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	application.parkVerifyFault(j, storagefault.Classify("verify", "/mnt/dl/a.bin", syscall.EIO))
	if application.weParked(j.ID()) {
		t.Fatal("parkVerifyFault claimed a pause that predates it")
	}

	application.reevaluateStall(j.ID())
	if application.StallReason(j.ID()).Reason == "" {
		t.Error("a re-evaluation cleared the reason of a job that is still paused")
	}
	if j.Intent() != job.IntentPause {
		t.Error("the re-evaluation undid a pause it did not make")
	}

	if err := application.dispatcher.ResumeJobByUser(j.ID()); err != nil {
		t.Fatalf("ResumeJobByUser: %v", err)
	}
	application.reevaluateStall(j.ID())
	if got := application.StallReason(j.ID()).Reason; got != "" {
		t.Errorf("stall reason %q survived the re-evaluation after the user resumed the job", got)
	}
}

// TestLooseRecord_LastingVerificationFaultStaysParked pins that a verification
// fault that does not clear keeps the job parked until the job is resumed
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

	// A re-evaluation asked for without resuming this job (a queue-wide
	// resume, or a resume of another job) leaves it parked too.
	a2.ReevaluateStalls()
	time.Sleep(30 * recheck)
	if got := passes.Load(); got != 1 {
		t.Fatalf("read-back passes = %d after a re-evaluation that resumed no job, want 1", got)
	}
	if !parked() {
		t.Fatalf("job not parked with its reason after a re-evaluation that resumed no job: "+
			"intent %v, reason %q", j2.Intent(), a2.StallReason(j.ID()).Reason)
	}

	// The operator's resume of the job, as the API's per-job resume makes it,
	// re-verifies: one more pass, which faults and parks the job again, and
	// then nothing more.
	resume := func() {
		t.Helper()
		if err := a2.dispatcher.ResumeJobByUser(j.ID()); err != nil {
			t.Fatalf("ResumeJobByUser: %v", err)
		}
		a2.ReevaluateStalls()
	}
	resume()
	lrWaitFor(t, "a resume of the job to re-verify it", func() bool { return passes.Load() >= 2 })
	lrWaitFor(t, "the re-verification's fault to park the job again", parked)
	time.Sleep(30 * recheck)
	if got := passes.Load(); got != 2 {
		t.Fatalf("read-back passes = %d after one resume of the job, want 2", got)
	}

	failing.Store(false)
	resume()
	lrWaitFor(t, "a resume after the fault cleared to verify and attach the job", func() bool {
		return articleDone(j2, 0) && articleDone(j2, 1)
	})
	if got := a2.StallReason(j.ID()).Reason; got != "" {
		t.Errorf("stall reason %q survived a resume whose verification succeeded", got)
	}
}

// TestStallReason_FollowsTheLatestFaultsKind: a second fault on a job that is
// already parked replaces the record's kind and fault together, so the
// surfaced prefix and stallKindOf both follow the latest fault, in either
// direction.
func TestStallReason_FollowsTheLatestFaultsKind(t *testing.T) {
	t.Parallel()
	first := storagefault.Classify("write", "/mnt/dl/a.bin", syscall.ENOSPC)
	second := storagefault.Classify("verify", "/mnt/dl/b.bin", syscall.EIO)
	tests := []struct {
		name     string
		a, b     func(app *Application, j *job.Job, f *storagefault.Fault)
		wantKind stallKind
		want     string
	}{
		{"retryable then verification", (*Application).Stall, (*Application).parkVerifyFault, stallVerify, "Stalled (resume to retry): "},
		{"verification then retryable", (*Application).parkVerifyFault, (*Application).Stall, stallRetryable, "Stalled: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			application, j := newDurabilityTestApp(t, 1, 2)
			tt.a(application, j, first)
			tt.b(application, j, second)

			if got := application.stallKindOf(j.ID()); got != tt.wantKind {
				t.Errorf("stallKindOf = %v, want %v", got, tt.wantKind)
			}
			if got, want := application.StallReason(j.ID()).Reason, tt.want+second.Error(); got != want {
				t.Errorf("stall reason = %q, want %q", got, want)
			}
		})
	}
}

// TestStallReason_SaysWhetherAResumeIsNeeded pins the surfaced wording per
// kind: a verification fault stays parked until the job itself is resumed, so
// its reason must say so, while a retryable fault recovers on its own and
// keeps the plain prefix. The fault text follows the prefix in both.
func TestStallReason_SaysWhetherAResumeIsNeeded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		park func(a *Application, j *job.Job, f *storagefault.Fault)
		want string
	}{
		{"verification fault", (*Application).parkVerifyFault, "Stalled (resume to retry): "},
		{"retryable fault", (*Application).Stall, "Stalled: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			application, j := newDurabilityTestApp(t, 1, 2)
			f := storagefault.Classify("verify", "/mnt/dl/a.bin", syscall.EIO)
			tt.park(application, j, f)

			if got, want := application.StallReason(j.ID()).Reason, tt.want+f.Error(); got != want {
				t.Errorf("stall reason = %q, want %q", got, want)
			}
		})
	}
}
