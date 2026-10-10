package app

import (
	"slices"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

func stallFault() *storagefault.Fault {
	return &storagefault.Fault{Op: "sync", Path: "/data/assess.bin", Err: syscall.ENOSPC, Permanent: false}
}

// stall parks the fixture's job as a retryable storage fault does.
func (f *failAtAssessingFixture) stall(t *testing.T) {
	t.Helper()
	f.app.Stall(f.j.ID(), stallFault())
	if got := f.j.Intent(); got != job.IntentPause {
		t.Fatalf("precondition: intent after Stall = %v, want IntentPause", got)
	}
	if !f.app.weParked(f.j.ID()) {
		t.Fatal("precondition: Stall did not record the job as parked")
	}
}

// requireResumedForAssessing fails unless Fail left the job unadmitted, off
// the stalled list, and resumed, so the tick can launch the worker its reason
// waits for.
func (f *failAtAssessingFixture) requireResumedForAssessing(t *testing.T) {
	t.Helper()
	if f.app.postProcAdmissions.has(f.j) {
		t.Fatal("Fail admitted the job; its Assessing worker must hand it over")
	}
	if got := f.app.StallReason(f.j.ID()).Reason; got != "" {
		t.Errorf("after Fail the stall reason is %q, want none", got)
	}
	if got := f.j.Intent(); got != job.IntentRun {
		t.Fatalf("after Fail the job's intent is %v: Stall's pause was not lifted, so no Assessing worker launches and the job is never handed over", got)
	}
}

// tickUntilRan ticks until the runner has launched want, in order.
func (f *failAtAssessingFixture) tickUntilRan(t *testing.T, want []job.State) {
	t.Helper()
	for range 5 {
		if slices.Equal(f.runner.ran(f.j.ID()), want) {
			return
		}
		f.d.Tick(t.Context())
	}
	if got := f.runner.ran(f.j.ID()); !slices.Equal(got, want) {
		t.Fatalf("runs = %v, want %v", got, want)
	}
}

// TestFail_OnAStalledCompleteJobAtFetching_ResumesItForAssessing: Stall pauses
// a complete job at Fetching, then a permanent fault reaches Fail. Fail defers
// the reason to an Assessing worker, which the tick launches only for a job
// not paused, and clears the stall record the re-evaluation would have resumed
// it from; so Fail resumes the job itself.
func TestFail_OnAStalledCompleteJobAtFetching_ResumesItForAssessing(t *testing.T) {
	t.Parallel()
	f := newFailAtAssessingFixture(t)
	f.stall(t)

	fault := assessFault()
	f.app.Fail(f.j.ID(), fault)
	f.requireResumedForAssessing(t)

	// The relaunched Fetching worker reports the complete job, as runFetch does.
	f.tickUntilRan(t, []job.State{job.Fetching, job.Fetching})
	if reported, err := f.app.reportDownloadComplete(f.j, f.d); !reported || err != nil {
		t.Fatalf("the relaunched Fetching worker's report = (%v, %v), want (true, nil)", reported, err)
	}
	f.tickUntilRan(t, []job.State{job.Fetching, job.Fetching, job.Assessing})
	f.requireHandedOffFromAssessing(t, f.awaitHandOff(t), fault)
}

// TestFail_OnAStalledJobWithAssessingPending_ResumesItForAssessing: the
// download-complete report lands on a job Stall paused, recording Assessing as
// its next state, and then Fail defers to that Assessing worker.
func TestFail_OnAStalledJobWithAssessingPending_ResumesItForAssessing(t *testing.T) {
	t.Parallel()
	f := newFailAtAssessingFixture(t)
	f.stall(t)
	if err := f.d.AdvanceFrom(f.j, job.Fetching, job.Assessing); err != nil {
		t.Fatalf("the download-complete report on the paused job: %v", err)
	}

	fault := assessFault()
	f.app.Fail(f.j.ID(), fault)
	f.requireResumedForAssessing(t)

	f.tickUntilRan(t, []job.State{job.Fetching, job.Assessing})
	f.requireHandedOffFromAssessing(t, f.awaitHandOff(t), fault)
}

// TestFail_OnAUserPausedJob_DefersWithoutResumingIt: a pause the user made is
// not lifted by a deferred Fail, with or without a stall record Stall did not
// park. The reason waits for the user's resume.
func TestFail_OnAUserPausedJob_DefersWithoutResumingIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		unparkedRec bool
	}{
		{"no stall record", false},
		{"a stall record Stall did not park", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFailAtAssessingFixture(t)
			if err := f.d.PauseJob(f.j.ID()); err != nil {
				t.Fatalf("the user's pause: %v", err)
			}
			if tc.unparkedRec {
				f.app.noteStall(f.j.ID(), assessFault(), false)
			}

			f.app.Fail(f.j.ID(), assessFault())
			if f.app.postProcAdmissions.has(f.j) {
				t.Fatal("Fail admitted the job; its Assessing worker must hand it over")
			}
			if got := f.j.Intent(); got != job.IntentPause {
				t.Fatalf("after Fail the user-paused job's intent is %v, want IntentPause: a deferred Fail lifted the user's pause", got)
			}
			if got := f.app.postProcAdmissions.takeDeferred(f.j); !slices.Equal(got, []string{faultReason(assessFault())}) {
				t.Errorf("the reason waiting for the Assessing worker = %q, want the fault's", got)
			}
		})
	}
}

// TestMaybeFinalize_AnEmptyReasonAtAssessing_IsNoFailure: a hand-off by ID
// with no reason, deferred to the Assessing worker, is a plain hand-over, as
// admit reads "". The worker reports its verdict rather than settling the job
// Failed.
func TestMaybeFinalize_AnEmptyReasonAtAssessing_IsNoFailure(t *testing.T) {
	t.Parallel()
	f := newFailAtAssessingFixture(t)
	if err := f.d.AdvanceFrom(f.j, job.Fetching, job.Assessing); err != nil {
		t.Fatalf("AdvanceFrom: %v", err)
	}
	if deferred := f.app.maybeFinalize(f.j.ID(), ""); !deferred {
		t.Fatal("precondition: maybeFinalize did not defer a job with Assessing pending")
	}

	f.d.Tick(t.Context())
	waitFor(t, func() bool {
		row, ok := f.d.Row(f.j.ID())
		return ok && (row.View.Outcome.IsSettled() || row.View.State != job.Fetching && row.View.Next != job.StateUnset)
	})
	row, _ := f.d.Row(f.j.ID())
	if row.View.Outcome == job.OutcomeFailed {
		t.Fatalf("a hand-off with no reason settled the job Failed at %v; want its verdict reported", row.View.State)
	}
}
