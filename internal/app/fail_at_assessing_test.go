package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	dispatchstore "github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/storagefault"
	"github.com/hobeone/gonzbd/internal/types"
)

// assessingTee records every launch and runs the real appRunner for a launch
// at Assessing, so the job's Assessing worker is runAssess while its Fetching
// worker is a recorder that never reports.
type assessingTee struct {
	rec  *stateRecorder
	next dispatch.Runner
}

func (r assessingTee) Run(ctx context.Context, id string, s job.State) {
	r.rec.Run(ctx, id, s)
	if s == job.Assessing {
		r.next.Run(ctx, id, s)
	}
}

// interceptReporter forwards every report to the dispatcher, calling before
// first for a report from Assessing.
type interceptReporter struct {
	reporter
	before func(j *job.Job)
}

func (r interceptReporter) AdvanceFrom(j *job.Job, from, next job.State) error {
	if from == job.Assessing {
		r.before(j)
	}
	return r.reporter.AdvanceFrom(j, from, next)
}

// signalReporter forwards every report to the dispatcher and, once the
// dispatcher has returned, notes it on reported, dropping the note when the
// channel is full. The dispatcher's row has changed by then, so a test waiting
// for a worker's report can look at the row when it is told one was made.
type signalReporter struct {
	reporter
	reported chan struct{}
}

func (r signalReporter) note() {
	select {
	case r.reported <- struct{}{}:
	default:
	}
}

func (r signalReporter) FinishedJob(j *job.Job, o job.Outcome) error {
	defer r.note()
	return r.reporter.FinishedJob(j, o)
}

func (r signalReporter) Yielded(id string) error {
	defer r.note()
	return r.reporter.Yielded(id)
}

func (r signalReporter) YieldedJob(j *job.Job) error {
	defer r.note()
	return r.reporter.YieldedJob(j)
}

func (r signalReporter) AdvanceFrom(j *job.Job, from, next job.State) error {
	defer r.note()
	return r.reporter.AdvanceFrom(j, from, next)
}

// TestSignalReporter_NotesEachReportAfterForwardingIt: the note is what wakes
// awaitRow, and the row it re-reads has changed only if the dispatcher has
// already seen the report. No integration test forces that wake-up, because the
// row is usually settled by awaitRow's first read.
func TestSignalReporter_NotesEachReportAfterForwardingIt(t *testing.T) {
	t.Parallel()
	rec := &reportRecorder{}
	reported := make(chan struct{}, 1)
	r := signalReporter{reporter: rec, reported: reported}
	j := job.New("sig", "sig", job.Policy{})
	for i, report := range []func() error{
		func() error { return r.FinishedJob(j, job.OutcomeFailed) },
		func() error { return r.Yielded(j.ID()) },
		func() error { return r.YieldedJob(j) },
		func() error { return r.AdvanceFrom(j, job.Assessing, job.Extracting) },
	} {
		if err := report(); err != nil {
			t.Fatalf("report %d: %v", i, err)
		}
		select {
		case <-reported:
		default:
			t.Fatalf("report %d was not noted", i)
		}
		if got := rec.calls(); got != i+1 {
			t.Fatalf("after report %d the dispatcher side has seen %d reports, want %d", i, got, i+1)
		}
	}
}

// failAtAssessingFixture is one complete job launched at Fetching on a real
// dispatcher the test ticks by hand. finalized receives each job the finalizer
// starts, and the finalizer then waits for release.
type failAtAssessingFixture struct {
	app       *Application
	d         *dispatch.Dispatcher
	runner    *stateRecorder
	finalized chan *postproc.Job
	release   func()
	j         *job.Job
	// reported receives a note for each report the application's runner makes.
	reported chan struct{}
}

func newFailAtAssessingFixture(t *testing.T) *failAtAssessingFixture {
	t.Helper()
	stage := gatedStage{entered: make(chan string, 4), finish: make(chan struct{})}
	application, repo, _ := newLifecycleTestApp(t, WithPostProcStages([]postproc.Stage{stage}))
	application.ctx = t.Context()
	application.closeJobHandlesHook = func(context.Context, string) error { return nil }
	runner := &stateRecorder{}
	d := dispatch.New(2, 1, time.Hour, time.Now, &appWorkers{app: application},
		application.residency, dispatchstore.New(repo.DB(), nil), assessingTee{rec: runner, next: application.runner})
	restoreDispatcher(t, d)
	application.dispatcher = d
	application.pipeline.dispatcher = d
	reported := make(chan struct{}, 16)
	application.runner.report = signalReporter{reporter: d, reported: reported}

	finalized := make(chan *postproc.Job, 4)
	finRelease := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(finRelease) }) }
	application.finalizeHook = func(pj *postproc.Job) {
		finalized <- pj
		<-finRelease
	}

	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "assess.bin",
		Bytes:    100,
		Articles: []nzb.Article{{ID: "a0@t", Bytes: 100, Number: 1}},
	}}}
	j, hdr, err := BuildIngestJob(application.config, parsed, "assess.nzb", types.FetchOptions{NzbName: "assess"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := j.MarkFileComplete(0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}
	if err := d.Add(t.Context(), j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	dir := filepath.Join(application.config.GetGeneral().DownloadDir, j.Name())
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assess.bin"), make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := application.postProcessor.Start(t.Context()); err != nil {
		t.Fatalf("postProcessor.Start: %v", err)
	}
	t.Cleanup(func() { _ = application.postProcessor.Stop() })
	t.Cleanup(func() { close(stage.finish) })
	t.Cleanup(release)

	d.Tick(t.Context()) // begins the attempt at Fetching
	d.Tick(t.Context()) // grants the lease and launches the Fetching recorder
	if got := runner.ran(j.ID()); !slices.Equal(got, []job.State{job.Fetching}) {
		t.Fatalf("precondition: runs after launch = %v, want [Fetching]", got)
	}
	return &failAtAssessingFixture{app: application, d: d, runner: runner, finalized: finalized, release: release, j: j, reported: reported}
}

func assessFault() *storagefault.Fault {
	return &storagefault.Fault{Op: "sync", Path: "/data/assess.bin", Err: syscall.EROFS, Permanent: true}
}

// awaitHandOff returns the job the finalizer started, failing if none starts.
func (f *failAtAssessingFixture) awaitHandOff(t *testing.T) *postproc.Job {
	t.Helper()
	select {
	case pj := <-f.finalized:
		return pj
	case <-time.After(10 * time.Second):
		t.Fatal("the job was never handed to post-processing")
		return nil
	}
}

// awaitRow returns once the job's row satisfies cond. The row is read first and
// again after each report the application's runner makes, which is the only
// thing that moves it here, so the wait ends on the report and not on a clock.
func (f *failAtAssessingFixture) awaitRow(t *testing.T, what string, cond func(dispatch.Row) bool) {
	t.Helper()
	for {
		if row, ok := f.d.Row(f.j.ID()); ok && cond(row) {
			return
		}
		lrAwait(t, what, f.reported)
	}
}

// requireNotAdmitted fails unless the job is unadmitted and its row reads want.
func (f *failAtAssessingFixture) requireNotAdmitted(t *testing.T, when string, want job.StateView, running bool) {
	t.Helper()
	if f.app.postProcAdmissions.has(f.j) {
		t.Fatalf("%s: the job was admitted to post-processing; its Assessing worker must hand it over", when)
	}
	row, ok := f.d.Row(f.j.ID())
	if !ok {
		t.Fatalf("%s: the job left the dispatcher", when)
	}
	if row.View.State != want.State || row.View.Next != want.Next || row.View.Running != running {
		t.Fatalf("%s: row = {State:%v Next:%v Running:%v}, want {State:%v Next:%v Running:%v}",
			when, row.View.State, row.View.Next, row.View.Running, want.State, want.Next, running)
	}
}

// requireHandedOffFromAssessing checks the run the Assessing worker handed
// over: it carries the fault's reason, and the worker settled the job Failed
// at Assessing rather than reporting its verdict.
func (f *failAtAssessingFixture) requireHandedOffFromAssessing(t *testing.T, pj *postproc.Job, fault *storagefault.Fault) {
	t.Helper()
	if pj.Job != f.j {
		t.Fatalf("handed over instance %p, want %p", pj.Job, f.j)
	}
	if want := faultReason(fault); pj.FailMsg != want {
		t.Errorf("handed-over FailMsg = %q, want %q", pj.FailMsg, want)
	}
	f.awaitRow(t, "the Assessing worker's report", func(row dispatch.Row) bool {
		return row.View.Outcome.IsSettled() || row.View.Next != job.StateUnset
	})
	row, _ := f.d.Row(f.j.ID())
	if row.View.State != job.Assessing || row.View.Next != job.StateUnset || row.View.Outcome != job.OutcomeFailed {
		t.Errorf("after the hand-off row = {State:%v Next:%v Outcome:%v}, want settled Failed at Assessing with no next",
			row.View.State, row.View.Next, row.View.Outcome)
	}
}

// TestFail_AtAssessingWithALiveWorker_DefersToTheWorkersExit: a permanent
// storage fault reaches a job whose Assessing worker is running. Fail must not
// admit it to post-processing while that worker runs, or the finalize tears
// the job down under it; the worker hands the job over when it exits.
func TestFail_AtAssessingWithALiveWorker_DefersToTheWorkersExit(t *testing.T) {
	t.Parallel()
	f := newFailAtAssessingFixture(t)
	entered := make(chan string, 1)
	resume := make(chan struct{})
	var once sync.Once
	resumeWorker := func() { once.Do(func() { close(resume) }) }
	t.Cleanup(resumeWorker)
	f.app.assessHook = func(id string) {
		entered <- id
		<-resume
	}

	if err := f.d.AdvanceFrom(f.j, job.Fetching, job.Assessing); err != nil {
		t.Fatalf("AdvanceFrom: %v", err)
	}
	f.d.Tick(t.Context()) // launches runAssess, which waits in assessHook
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the Assessing worker never started")
	}

	fault := assessFault()
	f.app.Fail(f.j, fault)
	f.requireNotAdmitted(t, "Fail with the Assessing worker live", job.StateView{State: job.Assessing}, true)

	resumeWorker()
	f.requireHandedOffFromAssessing(t, f.awaitHandOff(t), fault)
}

// TestFail_WithAssessingPending_DefersToTheWorkerTheTickLaunches: Fail lands
// after the download-complete report and before the tick that launches the
// Assessing worker. Admitting the job there let that tick launch runAssess for
// a job already in post-processing; the worker it launches hands it over.
func TestFail_WithAssessingPending_DefersToTheWorkerTheTickLaunches(t *testing.T) {
	t.Parallel()
	f := newFailAtAssessingFixture(t)
	if err := f.d.AdvanceFrom(f.j, job.Fetching, job.Assessing); err != nil {
		t.Fatalf("AdvanceFrom: %v", err)
	}

	fault := assessFault()
	f.app.Fail(f.j, fault)
	f.requireNotAdmitted(t, "Fail with Assessing pending", job.StateView{State: job.Fetching, Next: job.Assessing}, false)

	f.d.Tick(t.Context())
	if got := f.runner.ran(f.j.ID()); !slices.Equal(got, []job.State{job.Fetching, job.Assessing}) {
		t.Fatalf("runs = %v, want [Fetching Assessing]", got)
	}
	f.requireHandedOffFromAssessing(t, f.awaitHandOff(t), fault)
}

// TestFail_OnACompleteJobAtFetching_DefersToAssessing: a job whose last file
// is complete has a download-complete report coming. Fail reads the job before
// that report, and an admission made then would have the report move the
// admitted job to Assessing, so Fail defers to the worker that report brings.
func TestFail_OnACompleteJobAtFetching_DefersToAssessing(t *testing.T) {
	t.Parallel()
	f := newFailAtAssessingFixture(t)

	fault := assessFault()
	f.app.Fail(f.j, fault)
	f.requireNotAdmitted(t, "Fail on a complete job at Fetching", job.StateView{State: job.Fetching}, true)

	if err := f.d.AdvanceFrom(f.j, job.Fetching, job.Assessing); err != nil {
		t.Fatalf("the download-complete report after Fail: %v", err)
	}
	f.d.Tick(t.Context())
	f.requireHandedOffFromAssessing(t, f.awaitHandOff(t), fault)
}

// TestFail_BetweenTheVerdictAndItsReport_IsHandedOffAfterTheReport: Fail
// lands after the Assessing worker last looked for a deferred reason and
// before its verdict is recorded. The job still reads Assessing, so Fail
// defers; the worker hands it over once the report is made.
func TestFail_BetweenTheVerdictAndItsReport_IsHandedOffAfterTheReport(t *testing.T) {
	t.Parallel()
	f := newFailAtAssessingFixture(t)
	fault := assessFault()
	var admittedBeforeReport atomic.Bool
	f.app.runner.report = interceptReporter{reporter: f.d, before: func(j *job.Job) {
		f.app.Fail(j, fault)
		admittedBeforeReport.Store(f.app.postProcAdmissions.has(j))
	}}

	if err := f.d.AdvanceFrom(f.j, job.Fetching, job.Assessing); err != nil {
		t.Fatalf("AdvanceFrom: %v", err)
	}
	f.d.Tick(t.Context())
	pj := f.awaitHandOff(t)
	if admittedBeforeReport.Load() {
		t.Errorf("Fail admitted the job while its Assessing worker had not yet reported")
	}
	if want := faultReason(fault); pj.FailMsg != want {
		t.Errorf("handed-over FailMsg = %q, want %q", pj.FailMsg, want)
	}
}
