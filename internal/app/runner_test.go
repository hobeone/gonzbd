package app

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

type reportRecorder struct {
	mu       sync.Mutex
	total    int
	outcomes map[string]job.Outcome
	advances map[string][2]job.State
	yields   map[string]int
	// yieldedJobs is each instance reported through YieldedJob, which
	// yields counts too.
	yieldedJobs []*job.Job
	// changed is closed, and cleared, by the next report, so awaitCalls
	// blocks on it rather than polling total.
	changed chan struct{}
}

// reportedLocked counts one report and wakes the awaitCalls waiting for it.
// The caller holds r.mu.
func (r *reportRecorder) reportedLocked() {
	r.total++
	if r.changed != nil {
		close(r.changed)
		r.changed = nil
	}
}

// awaitCalls blocks until the recorder has counted at least n reports. The
// runner reports from a goroutine Run starts, so the report is not made when
// Run returns; the wait ends on the report itself, and its budget is lrAwait's
// hang guard, not a bound on how long the report may take.
func (r *reportRecorder) awaitCalls(t *testing.T, n int) {
	t.Helper()
	for {
		r.mu.Lock()
		if r.total >= n {
			r.mu.Unlock()
			return
		}
		if r.changed == nil {
			r.changed = make(chan struct{})
		}
		ch := r.changed
		r.mu.Unlock()
		lrAwait(t, fmt.Sprintf("the runner's report %d", n), ch)
	}
}

func (r *reportRecorder) FinishedJob(j *job.Job, o job.Outcome) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reportedLocked()
	if r.outcomes == nil {
		r.outcomes = make(map[string]job.Outcome)
	}
	r.outcomes[j.ID()] = o
	return nil
}

func (r *reportRecorder) outcome(id string) job.Outcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outcomes[id]
}

func (r *reportRecorder) Yielded(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reportedLocked()
	if r.yields == nil {
		r.yields = make(map[string]int)
	}
	r.yields[id]++
	return nil
}

func (r *reportRecorder) YieldedJob(j *job.Job) error {
	r.mu.Lock()
	r.yieldedJobs = append(r.yieldedJobs, j)
	r.mu.Unlock()
	return r.Yielded(j.ID())
}

// yieldedInstances returns the instances reported through YieldedJob.
func (r *reportRecorder) yieldedInstances() []*job.Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.yieldedJobs)
}

// yielded returns how many times id was reported Yielded or YieldedJob.
func (r *reportRecorder) yielded(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.yields[id]
}

func (r *reportRecorder) AdvanceFrom(j *job.Job, from, next job.State) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reportedLocked()
	if r.advances == nil {
		r.advances = make(map[string][2]job.State)
	}
	r.advances[j.ID()] = [2]job.State{from, next}
	return nil
}

// advance returns the from and next of the AdvanceFrom reported for id.
func (r *reportRecorder) advance(id string) [2]job.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.advances[id]
}

func (r *reportRecorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total
}

// TestReportRecorder_WakesAwaitCallsOnEachReport: every report method closes
// and clears the channel a waiting awaitCalls holds. The runner tests do not
// force this: a report that lands before awaitCalls first looks needs no
// wake-up, so they pass without it.
func TestReportRecorder_WakesAwaitCallsOnEachReport(t *testing.T) {
	t.Parallel()
	j := job.New("rec", "rec", job.Policy{})
	for name, report := range map[string]func(*reportRecorder) error{
		"FinishedJob": func(r *reportRecorder) error { return r.FinishedJob(j, job.OutcomeFailed) },
		"Yielded":     func(r *reportRecorder) error { return r.Yielded(j.ID()) },
		"YieldedJob":  func(r *reportRecorder) error { return r.YieldedJob(j) },
		"AdvanceFrom": func(r *reportRecorder) error { return r.AdvanceFrom(j, job.Assessing, job.Extracting) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := &reportRecorder{}
			r.mu.Lock()
			r.changed = make(chan struct{})
			waiter := r.changed
			r.mu.Unlock()
			if err := report(r); err != nil {
				t.Fatalf("report: %v", err)
			}
			select {
			case <-waiter:
			default:
				t.Error("the report did not wake the waiter")
			}
			r.mu.Lock()
			cleared := r.changed == nil
			r.mu.Unlock()
			if !cleared {
				t.Error("the report left the waiter's channel installed")
			}
		})
	}
}

// TestAppRunner_ReturnsPromptly pins ports.go's hardest requirement: Run is
// called from the dispatcher's tick goroutine, so blocking there stalls every
// other job's advance -- not just this one's.
func TestAppRunner_ReturnsPromptly(t *testing.T) {
	t.Parallel()
	r := newTestApplication(t).runner

	done := make(chan struct{})
	go func() {
		r.Run(context.Background(), "abc123", job.Fetching)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run blocked for 2s; it must dispatch work and return")
	}
}

// TestAppRunner_EveryStateReportsExactlyOnce pins the resource contract: a
// state that returns without calling Finished or Yielded strands the job's
// lease and compute slot forever, because the Queue cannot tell 'holding and
// working' from 'holding and yielded'.
func TestAppRunner_EveryStateReportsExactlyOnce(t *testing.T) {
	t.Parallel()
	for _, st := range job.AllStates() {
		t.Run(st.String(), func(t *testing.T) {
			app := newTestApplication(t)
			rec := &reportRecorder{}
			r := newAppRunner(app)
			r.report = rec

			r.Run(context.Background(), "abc123", st)
			rec.awaitCalls(t, 1)

			if got := rec.calls(); got != 1 {
				t.Fatalf("state %s reported %d times, want exactly 1", st, got)
			}
		})
	}
}

// TestAppRunner_EveryStateDischargesCompletionContract pins Finding B9's core invariant:
// For every job.State:
// 1. When the application is stopping, every state yields immediately.
// 2. For an unregistered job, every state yields immediately.
// 3. For a registered job:
//   - Assessing discharges directly via report.AdvanceFrom, report.FinishedJob
//     or report.YieldedJob, as appRunner's item 2 lists.
//   - Default/unhandled states discharge directly via report.Yielded.
//   - Fetching hands off to downloader (dl.Wake).
//   - Repairing/Extracting/Finalizing hands off to postProcessor.
func TestAppRunner_EveryStateDischargesCompletionContract(t *testing.T) {
	t.Parallel()
	for _, st := range job.AllStates() {
		t.Run(st.String()+"/StoppingYieldsImmediately", func(t *testing.T) {
			app := newTestApplication(t)
			j := buildRunnerJob(t, app, []failMsgFile{{subject: "file.rar", bytes: 100}})
			app.stopping.Store(true)
			rec := &reportRecorder{}
			r := newAppRunner(app)
			r.report = rec

			r.Run(context.Background(), j.ID(), st)
			rec.awaitCalls(t, 1)
			if got := rec.calls(); got != 1 {
				t.Fatalf("state %s when stopping reported %d times, want exactly 1", st, got)
			}
		})

		t.Run(st.String()+"/RegisteredJobDischargesOrHandsOff", func(t *testing.T) {
			app := newTestApplication(t)
			rec := &reportRecorder{}
			r := newAppRunner(app)
			r.report = rec

			j := buildRunnerJob(t, app, []failMsgFile{{subject: "file.rar", bytes: 100}})

			switch st {
			case job.Assessing:
				r.Run(context.Background(), j.ID(), st)
				rec.awaitCalls(t, 1)
				if got := rec.calls(); got != 1 {
					t.Fatalf("Assessing state reported %d times, want exactly 1", got)
				}
			case job.Fetching:
				// Fetching hands off to downloader. Waking downloader is the handoff.
				r.Run(context.Background(), j.ID(), st)
				// Downstream handoff verified: when file completion arrives, handleFileComplete yields.
				if err := app.Dispatcher().Yielded(j.ID()); err != nil {
					t.Fatalf("expected job %s to be launched and yieldable: %v", j.ID(), err)
				}
			case job.Repairing, job.Extracting, job.Finalizing:
				// Postproc states hand off to postprocessor.
				r.Run(context.Background(), j.ID(), st)
				// Downstream handoff verified: postproc completion yields via jobFinalizer.
				if err := app.Dispatcher().Yielded(j.ID()); err != nil {
					t.Fatalf("expected job %s to be launched and yieldable: %v", j.ID(), err)
				}
			default:
				r.Run(context.Background(), j.ID(), st)
				rec.awaitCalls(t, 1)
				if got := rec.calls(); got != 1 {
					t.Fatalf("default state %s reported %d times, want exactly 1", st, got)
				}
			}
		})
	}
}

func TestAppRunner_DirectMethods(t *testing.T) {
	t.Parallel()
	app := newTestApplication(t)
	rec := &reportRecorder{}
	r := newAppRunner(app)
	r.report = rec

	r.runFetch(context.Background(), "unknown")
	r.runAssess(context.Background(), "unknown")
	r.runPostProc(context.Background(), "unknown", job.Repairing)

	if got := rec.calls(); got != 3 {
		t.Errorf("got %d calls, want 3", got)
	}
}

func buildRunnerJob(t *testing.T, app *Application, files []failMsgFile, failIdx ...int) *job.Job {
	t.Helper()
	parsed := &nzb.NZB{}
	for i, f := range files {
		parsed.Files = append(parsed.Files, nzb.File{
			Subject:  f.subject,
			Bytes:    f.bytes,
			Articles: []nzb.Article{{ID: fmt.Sprintf("a%d@t", i), Bytes: int(f.bytes), Number: 1}},
		})
	}
	j, hdr, err := BuildIngestJob(app.config, parsed, "t.nzb", types.FetchOptions{NzbName: "t.nzb"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	// Every call builds from "t.nzb"; the registry refuses a second job of one name.
	hdr.Name = hdr.Name + "-" + j.ID()
	j.SetName(hdr.Name)
	if err := app.Dispatcher().Add(context.Background(), j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	for _, i := range failIdx {
		ackFailed(t, app.Dispatcher(), j.ID(), fmt.Sprintf("a%d@t", i))
	}
	if err := j.BeginAttempt(time.Now()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	if err := j.Transition(job.Assessing); err != nil {
		t.Fatalf("Transition(Assessing): %v", err)
	}
	return j
}

func TestAppRunner_RunAssessBranches(t *testing.T) {
	t.Parallel()
	app := newTestApplication(t)
	rec := &reportRecorder{}
	r := newAppRunner(app)
	r.report = rec

	// Default branch in Run
	r.Run(context.Background(), "j_default", job.State(250))
	if got := rec.calls(); got != 1 {
		t.Fatalf("default state should yield once; got %d", got)
	}

	// 1. Hopeless job
	jHopeless := buildRunnerJob(t, app, []failMsgFile{
		{subject: "movie.rar", bytes: 100},
	}, 0)
	r.runAssess(context.Background(), jHopeless.ID())
	if got := rec.outcome(jHopeless.ID()); got != job.OutcomeFailed {
		t.Errorf("reported outcome for hopeless job = %v, want OutcomeFailed", got)
	}

	// 2. Repairable job
	jRepair := buildRunnerJob(t, app, []failMsgFile{
		{subject: "movie.rar", bytes: 100},
		{subject: "movie.vol01+02.par2", bytes: 200},
	}, 0)
	r.runAssess(context.Background(), jRepair.ID())
	if got, want := rec.advance(jRepair.ID()), [2]job.State{job.Assessing, job.Repairing}; got != want {
		t.Errorf("jRepair reported AdvanceFrom %v, want %v", got, want)
	}

	// 3. Deferred recovery volume release (maybeReleaseRecoveryVolumes == true)
	jDeferred := buildRunnerJob(t, app, []failMsgFile{
		{subject: "movie.rar", bytes: 100},
		{subject: "movie.vol01+02.par2", bytes: 200},
	})
	_ = jDeferred.SetFileFetchPolicy(1, job.FetchIfNeeded)
	// Releasing a deferred recovery volume is gated on Policy.Repair.
	// buildRunnerJob's default PP (0) resolves Policy.Repair == false, so
	// this subtest — which pins the release happening — needs it allowed.
	jDeferred.SetPolicy(job.PolicyFromPP(types.PPRepair))
	r.runAssess(context.Background(), jDeferred.ID())
	if got, want := rec.advance(jDeferred.ID()), [2]job.State{job.Assessing, job.Fetching}; got != want {
		t.Errorf("jDeferred reported AdvanceFrom %v, want %v", got, want)
	}

	// 4. Intact job
	jClean := buildRunnerJob(t, app, []failMsgFile{
		{subject: "movie.rar", bytes: 100},
	})
	r.runAssess(context.Background(), jClean.ID())
	if got, want := rec.advance(jClean.ID()), [2]job.State{job.Assessing, job.Extracting}; got != want {
		t.Errorf("jClean reported AdvanceFrom %v, want %v", got, want)
	}

	// 5. A cancelled assessment (shutdown) yields the job at Assessing: no
	// verdict, no release, and a reason deferred to it stays deferred
	// rather than being handed to post-processing (#1008).
	jCancelled := buildRunnerJob(t, app, []failMsgFile{
		{subject: "movie.rar", bytes: 100},
		{subject: "movie.vol01+02.par2", bytes: 200},
	})
	_ = jCancelled.SetFileFetchPolicy(1, job.FetchIfNeeded)
	jCancelled.SetPolicy(job.PolicyFromPP(types.PPRepair))
	if got := app.postProcAdmissions.admitUnlessAssessing(jCancelled, "deferred reason"); got != deferredToAssessing {
		t.Fatalf("admitUnlessAssessing = %v, want deferredToAssessing", got)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	r.runAssess(cancelled, jCancelled.ID())
	if got := rec.advance(jCancelled.ID()); got != ([2]job.State{}) {
		t.Errorf("jCancelled reported AdvanceFrom %v, want none", got)
	}
	if got := rec.yielded(jCancelled.ID()); got != 1 {
		t.Errorf("jCancelled yielded %d times, want 1", got)
	}
	if got := rec.yieldedInstances(); len(got) != 1 || got[0] != jCancelled {
		t.Errorf("YieldedJob instances = %v, want only jCancelled, not a yield by ID", got)
	}
	if p := jCancelled.Progress().FileFetchPolicy(1); p != job.FetchIfNeeded {
		t.Errorf("volume policy = %v, want FetchIfNeeded", p)
	}
	if got := app.postProcAdmissions.takeDeferred(jCancelled); len(got) != 1 || got[0] != "deferred reason" {
		t.Errorf("deferred reasons = %q, want the one deferred before the cancelled visit", got)
	}
}
