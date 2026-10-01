package app

import (
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// assessingJob returns a job whose open attempt is at Assessing.
func assessingJob(t *testing.T, id string) *job.Job {
	t.Helper()
	j := job.New(id, id, job.Policy{})
	if err := j.BeginAttempt(time.Now()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	if err := j.SetNext(job.Assessing); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if err := j.Transition(job.Assessing); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	return j
}

// TestAwaitsAssessing pins which positions defer a hand-off by ID to the
// Assessing worker. A job with no content reads as incomplete, so at Fetching
// it does not.
func TestAwaitsAssessing(t *testing.T) {
	t.Parallel()
	fetching := func(t *testing.T) *job.Job {
		j := job.New("f", "f", job.Policy{})
		if err := j.BeginAttempt(time.Now()); err != nil {
			t.Fatalf("BeginAttempt: %v", err)
		}
		return j
	}
	cases := []struct {
		name string
		make func(t *testing.T) *job.Job
		want bool
	}{
		{"never run", func(*testing.T) *job.Job { return job.New("n", "n", job.Policy{}) }, false},
		{"incomplete at Fetching", fetching, false},
		{"Assessing recorded as next", func(t *testing.T) *job.Job {
			j := fetching(t)
			if err := j.SetNext(job.Assessing); err != nil {
				t.Fatalf("SetNext: %v", err)
			}
			return j
		}, true},
		{"at Assessing", func(t *testing.T) *job.Job { return assessingJob(t, "a") }, true},
		{"at Assessing with its verdict recorded", func(t *testing.T) *job.Job {
			j := assessingJob(t, "v")
			if err := j.SetNext(job.Extracting); err != nil {
				t.Fatalf("SetNext: %v", err)
			}
			return j
		}, false},
		{"settled at Assessing", func(t *testing.T) *job.Job {
			j := assessingJob(t, "s")
			if _, err := j.Finish(job.OutcomeFailed, time.Now()); err != nil {
				t.Fatalf("Finish: %v", err)
			}
			return j
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := awaitsAssessing(tc.make(t)); got != tc.want {
				t.Errorf("awaitsAssessing = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAdmitLocked_Outcomes pins the admission decision admit and
// admitUnlessAssessing share, call by call on one instance.
func TestAdmitLocked_Outcomes(t *testing.T) {
	t.Parallel()
	var a postProcAdmissions
	j := job.New("o", "o", job.Policy{})
	steps := []struct {
		failMsg string
		want    admitOutcome
	}{
		{"", admitted},
		{"", refused},
		{"first", refusedReasonKept},
		{"first", refused},
		{"second", refusedReasonNoted},
		{"second", refused},
	}
	for i, s := range steps {
		a.mu.Lock()
		got := a.admitLocked(j, s.failMsg)
		a.mu.Unlock()
		if got != s.want {
			t.Fatalf("call %d admitLocked(%q) = %v, want %v", i, s.failMsg, got, s.want)
		}
	}
	a.release(j)
	a.mu.Lock()
	got := a.admitLocked(j, "late")
	a.mu.Unlock()
	if got != refusedEnded {
		t.Errorf("admitLocked after release = %v, want refusedEnded", got)
	}
}

// TestAdmitUnlessAssessing_DefersToTheVisit: a hand-off by ID at Assessing is
// not admitted; its reasons wait, in order, for the worker's visit, which
// takes each once.
func TestAdmitUnlessAssessing_DefersToTheVisit(t *testing.T) {
	t.Parallel()
	var a postProcAdmissions
	j := assessingJob(t, "d")

	if got := a.admitUnlessAssessing(j, "first"); got != deferredToAssessing {
		t.Fatalf("admitUnlessAssessing before the worker = %v, want deferredToAssessing", got)
	}
	visit := a.beginAssess(j)
	if got := a.admitUnlessAssessing(j, "second"); got != deferredToAssessing {
		t.Fatalf("admitUnlessAssessing with the worker live = %v, want deferredToAssessing", got)
	}
	if a.has(j) {
		t.Fatal("a deferred hand-off admitted the job")
	}
	if got := a.takeDeferred(j); !slices.Equal(got, []string{"first", "second"}) {
		t.Errorf("takeDeferred = %q, want [first second]", got)
	}
	if got := a.admitUnlessAssessing(j, "third"); got != deferredToAssessing {
		t.Fatalf("admitUnlessAssessing after the worker looked = %v, want deferredToAssessing", got)
	}
	if got := a.endAssess(j, visit); !slices.Equal(got, []string{"third"}) {
		t.Errorf("endAssess = %q, want [third]", got)
	}
	if got := a.takeDeferred(j); got != nil {
		t.Errorf("takeDeferred after endAssess = %q, want none", got)
	}
}

// TestAdmitUnlessAssessing_AdmitsWhatIsNotAtAssessing: a job past its verdict
// is admitted as admit would, and an admitted one is not deferred.
func TestAdmitUnlessAssessing_AdmitsWhatIsNotAtAssessing(t *testing.T) {
	t.Parallel()
	var a postProcAdmissions
	j := assessingJob(t, "x")
	if err := j.SetNext(job.Extracting); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if got := a.admitUnlessAssessing(j, "fault"); got != admitted {
		t.Fatalf("admitUnlessAssessing past the verdict = %v, want admitted", got)
	}

	k := assessingJob(t, "y")
	if got := a.admit(k, ""); got != admitted {
		t.Fatalf("admit = %v, want admitted", got)
	}
	if got := a.admitUnlessAssessing(k, "fault"); got != refusedReasonKept {
		t.Errorf("admitUnlessAssessing on an admitted job at Assessing = %v, want refusedReasonKept", got)
	}
}

// TestAdmitUnlessAssessing_AVisitNoWorkerOwnsGoesWithItsJob: a reason
// deferred to an instance that is removed before its Assessing worker runs is
// not kept for the life of the process.
func TestAdmitUnlessAssessing_AVisitNoWorkerOwnsGoesWithItsJob(t *testing.T) {
	t.Parallel()
	var a postProcAdmissions
	if got := a.admitUnlessAssessing(assessingJob(t, "gone"), "fault"); got != deferredToAssessing {
		t.Fatalf("admitUnlessAssessing = %v, want deferredToAssessing", got)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		runtime.GC()
		a.mu.Lock()
		n := len(a.assessing)
		a.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d visits remain after their job was collected", n)
		}
		runtime.Gosched()
	}
}

// TestBeginAssess_AnEarlierVisitsEndTakesNothingDeferredToALaterOne: a visit
// still owned when the next worker begins belongs to a worker that has not
// reached endAssess. The reasons deferred to the next worker are its own.
func TestBeginAssess_AnEarlierVisitsEndTakesNothingDeferredToALaterOne(t *testing.T) {
	t.Parallel()
	var a postProcAdmissions
	j := assessingJob(t, "e")

	earlier := a.beginAssess(j)
	later := a.beginAssess(j)
	if got := a.admitUnlessAssessing(j, "fault"); got != deferredToAssessing {
		t.Fatalf("admitUnlessAssessing = %v, want deferredToAssessing", got)
	}
	if got := a.endAssess(j, earlier); got != nil {
		t.Errorf("the earlier visit's endAssess took %q, want none", got)
	}
	if got := a.admitUnlessAssessing(j, "second"); got != deferredToAssessing {
		t.Fatalf("admitUnlessAssessing after the earlier end = %v, want deferredToAssessing", got)
	}
	if got := a.endAssess(j, later); !slices.Equal(got, []string{"fault", "second"}) {
		t.Errorf("the later visit's endAssess = %q, want [fault second]", got)
	}
}
