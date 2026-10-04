package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// TestDispatcherControlSurface_PerJobDoors pins the doors the API needs and
// did not have: a job pointer, and per-job pause/resume distinct from the
// queue-wide flag.
func TestDispatcherControlSurface_PerJobDoors(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("a", "Job A", job.PolicyFromPP(3))
	if err := d.Add(context.Background(), j, Header{Name: "Job A"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, ok := d.Job("a")
	if !ok || got.ID() != "a" {
		t.Fatalf("Job(a) = %v, %v; want the job", got, ok)
	}

	if err := d.PauseJob("a"); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	if in := j.Intent(); in != job.IntentPause {
		t.Fatalf("Intent = %v, want IntentPause", in)
	}

	// Per-job pause must NOT set the queue-wide flag. Conflating the two is
	// what ToSABnzbd's WaitReason.IsPause() routing exists to survive, and a
	// control surface that sets both makes that distinction unobservable.
	if d.Paused() {
		t.Fatal("PauseJob must not set the queue-wide pause flag")
	}

	if err := d.ResumeJob("a"); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	if in := j.Intent(); in != job.IntentRun {
		t.Fatalf("Intent = %v, want IntentRun", in)
	}

	if _, ok := d.Job("nope"); ok {
		t.Fatal("Job of an unknown id must report not-found")
	}
	if err := d.PauseJob("nope"); err == nil {
		t.Fatal("PauseJob of an unknown id must error")
	}
	if err := d.ResumeJob("nope"); err == nil {
		t.Fatal("ResumeJob of an unknown id must error")
	}
	if err := j.SetIntent(job.IntentCancel); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if err := d.ResumeJob("a"); err == nil {
		t.Fatal("ResumeJob on cancelled job must error")
	}
}

// TestSetName_RefusesUnsafeOrTakenNames pins the registry's half of the
// job-name invariant: a job's name becomes its download directory
// (DownloadDir/<name>), so it must be one path component that no other
// registered job has.
func TestSetName_RefusesUnsafeOrTakenNames(t *testing.T) {
	d := newTestDispatcher(t)
	for _, id := range []string{"a", "b"} {
		if err := d.Add(context.Background(), job.New(id, "Job "+id, job.PolicyFromPP(3)), Header{Name: "Job " + id}); err != nil {
			t.Fatalf("Add(%s): %v", id, err)
		}
	}
	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "/abs", "x\x00y", "Job b"} {
		if err := d.SetName("a", name); !errors.Is(err, ErrInvalidJobName) {
			t.Errorf("SetName(a, %q) = %v, want ErrInvalidJobName", name, err)
		}
		if row, _ := d.Row("a"); row.Header.Name != "Job a" {
			t.Fatalf("after a refused SetName(a, %q) the name is %q", name, row.Header.Name)
		}
	}
	if err := d.SetName("a", "Renamed"); err != nil {
		t.Fatalf("SetName(a, Renamed) = %v", err)
	}
	if err := d.SetName("a", "Renamed"); err != nil {
		t.Errorf("SetName to the job's own name = %v, want nil", err)
	}
}

// TestSetName_RefusesAJobThatHasStarted pins the registry's refusal to rename
// a job whose download has begun: its name is its download directory and a
// rename moves nothing. A job that has only begun an attempt (which the tick
// does for any ungated job) or whose articles all failed is still renamable.
func TestSetName_RefusesAJobThatHasStarted(t *testing.T) {
	d := newTestDispatcher(t)
	manifest := func() *job.Manifest {
		return job.NewManifest([]job.JobFile{{
			Subject: "f", Bytes: 200,
			Articles: []job.JobArticle{{ID: "a1", Bytes: 100, Number: 1}, {ID: "a2", Bytes: 100, Number: 2}},
		}})
	}
	add := func(id string, prep func(*job.Job)) *job.Job {
		j := job.New(id, "Job "+id, job.PolicyFromPP(3))
		if err := j.AttachContent(manifest()); err != nil {
			t.Fatalf("AttachContent(%s): %v", id, err)
		}
		prep(j)
		if err := d.Add(context.Background(), j, Header{Name: "Job " + id}); err != nil {
			t.Fatalf("Add(%s): %v", id, err)
		}
		return j
	}
	stamped := add("stamped", func(j *job.Job) {
		if err := j.MarkJobStarted(time.Now()); err != nil {
			t.Fatalf("MarkJobStarted: %v", err)
		}
	})
	retried := add("retried", func(j *job.Job) { // done article, stamps cleared as ResetForRetry leaves them
		if err := j.MarkArticleDone(0, 100, "s"); err != nil {
			t.Fatalf("MarkArticleDone: %v", err)
		}
	})
	add("attempt", func(j *job.Job) {
		if err := j.BeginAttempt(testClock()); err != nil {
			t.Fatalf("BeginAttempt: %v", err)
		}
	})
	add("failed", func(j *job.Job) {
		if err := j.MarkArticleFailed(0); err != nil {
			t.Fatalf("MarkArticleFailed: %v", err)
		}
	})

	for _, tc := range []struct {
		id string
		j  *job.Job
	}{{"stamped", stamped}, {"retried", retried}} {
		err := d.SetName(tc.id, "Other "+tc.id)
		if !errors.Is(err, ErrJobStarted) || errors.Is(err, ErrInvalidJobName) {
			t.Errorf("SetName on %s = %v, want ErrJobStarted and not ErrInvalidJobName", tc.id, err)
		}
		if row, _ := d.Row(tc.id); row.Header.Name != "Job "+tc.id || tc.j.Name() != "Job "+tc.id {
			t.Errorf("a refused rename changed the name: header %q, job %q", row.Header.Name, tc.j.Name())
		}
	}
	for _, id := range []string{"attempt", "failed"} {
		if err := d.SetName(id, "Renamed "+id); err != nil {
			t.Errorf("SetName on %s, which fetched nothing = %v, want nil", id, err)
		}
	}
}

// TestAdd_RefusesANameAnotherJobHas pins the registration half of the name
// invariant: two callers that each chose a name the queue did not yet hold
// cannot both register under it, because Add checks under the same d.mu span
// that inserts the job. A refused Add leaves no entry and writes no row.
func TestAdd_RefusesANameAnotherJobHas(t *testing.T) {
	st := &fakeStore{}
	d := newTestDispatcher(t, withStore(st))
	if err := d.Add(context.Background(), job.New("a", "Same", job.PolicyFromPP(3)), Header{Name: "Same"}); err != nil {
		t.Fatalf("Add(a): %v", err)
	}
	err := d.Add(context.Background(), job.New("b", "Same", job.PolicyFromPP(3)), Header{Name: "Same"})
	if !errors.Is(err, ErrJobNameTaken) || !errors.Is(err, ErrInvalidJobName) {
		t.Fatalf("Add(b) under a's name = %v, want ErrJobNameTaken wrapping ErrInvalidJobName", err)
	}
	if _, ok := d.Row("b"); ok {
		t.Fatal("a refused Add left b registered")
	}
	st.mu.Lock()
	_, wrote := st.rows["b"]
	st.mu.Unlock()
	if wrote {
		t.Fatal("a refused Add wrote b's queue row")
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			id := fmt.Sprintf("c%d", i)
			errs[i] = d.Add(context.Background(), job.New(id, "Raced", job.PolicyFromPP(3)), Header{Name: "Raced"})
		})
	}
	wg.Wait()
	admitted := 0
	for i, err := range errs {
		switch {
		case err == nil:
			admitted++
		case !errors.Is(err, ErrJobNameTaken):
			t.Errorf("concurrent Add c%d = %v, want nil or ErrJobNameTaken", i, err)
		}
	}
	if admitted != 1 {
		t.Fatalf("%d of %d concurrent Adds under one name were admitted, want exactly 1", admitted, n)
	}
}

// TestResumeJobByUser_ApprovesABlockedJob pins the approval half of the
// unwanted-extension pause: the user's resume moves a blocked job to
// approved, and the next tick persists it with the intent, while an
// application resume (ResumeJob) leaves the block standing.
func TestResumeJobByUser_ApprovesABlockedJob(t *testing.T) {
	st := &fakeStore{}
	d := newTestDispatcher(t, withStore(st))
	j := job.New("a", "Job A", job.PolicyFromPP(3))
	if err := j.SetIntent(job.IntentPause); err != nil {
		t.Fatal(err)
	}
	if err := d.Add(context.Background(), j, Header{Name: "Job A", Unwanted: unwanted.StateBlocked}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// A plain resume is refused outright: only ResumeJobByUser unblocks.
	// This is the order a resume racing a retry produces, where the API's
	// own check ran before the retry registered the job blocked.
	if err := d.ResumeJob("a"); !errors.Is(err, ErrUnwantedBlocked) {
		t.Fatalf("ResumeJob on a blocked job = %v, want ErrUnwantedBlocked", err)
	}
	if in := j.Intent(); in != job.IntentPause {
		t.Fatalf("after a refused resume Intent = %v, want IntentPause: the block was bypassed", in)
	}
	if row, _ := d.Row("a"); row.Header.Unwanted != unwanted.StateBlocked {
		t.Fatalf("after an application resume Unwanted = %d, want blocked (%d)", row.Header.Unwanted, unwanted.StateBlocked)
	}

	if err := d.ResumeJobByUser("a"); err != nil {
		t.Fatalf("ResumeJobByUser: %v", err)
	}
	if in := j.Intent(); in != job.IntentRun {
		t.Fatalf("Intent = %v, want IntentRun", in)
	}
	if row, _ := d.Row("a"); row.Header.Unwanted != unwanted.StateApproved {
		t.Fatalf("after the user's resume Unwanted = %d, want approved (%d)", row.Header.Unwanted, unwanted.StateApproved)
	}
	d.tick(context.Background())
	if p, ok := st.row("a"); !ok || p.Header.Unwanted != unwanted.StateApproved {
		t.Fatalf("persisted Unwanted = %d (row %v), want approved", p.Header.Unwanted, ok)
	}

	if err := d.ResumeJobByUser("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ResumeJobByUser of an unknown id = %v, want ErrNotFound", err)
	}
}

// TestResumeJobByUser_LeavesOtherStatesAlone pins that only a blocked job is
// approved: a job the check never blocked stays StateNone.
func TestResumeJobByUser_LeavesOtherStatesAlone(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("a", "Job A", job.PolicyFromPP(3))
	if err := d.Add(context.Background(), j, Header{Name: "Job A"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := d.ResumeJobByUser("a"); err != nil {
		t.Fatalf("ResumeJobByUser: %v", err)
	}
	if row, _ := d.Row("a"); row.Header.Unwanted != unwanted.StateNone {
		t.Fatalf("Unwanted = %d, want none", row.Header.Unwanted)
	}
	if err := j.SetIntent(job.IntentCancel); err != nil {
		t.Fatal(err)
	}
	if err := d.ResumeJobByUser("a"); err == nil {
		t.Fatal("ResumeJobByUser on a cancelled job must error")
	}
}

// TestDispatcherRemove_IsIdempotentAndReturnsResources pins that Remove gives
// back what the job held. A removed job that keeps its lease or slot strands
// pool capacity for the life of the process, and nothing later reclaims it --
// the tick only walks registered jobs.
func TestDispatcherRemove_IsIdempotentAndReturnsResources(t *testing.T) {
	st := &fakeStore{}
	w := &stubWorkers{}
	d := newTestDispatcher(t, withCaps(1, 1), withStore(st), withWorkers(w))
	w.onAbort = func(id string) {
		go func() {
			_ = d.Yielded(id)
		}()
	}
	jA := job.New("a", "Job A", job.PolicyFromPP(3))
	if err := d.Add(context.Background(), jA, Header{Name: "Job A"}); err != nil {
		t.Fatalf("Add(a): %v", err)
	}
	jB := job.New("b", "Job B", job.PolicyFromPP(3))
	if err := d.Add(context.Background(), jB, Header{Name: "Job B"}); err != nil {
		t.Fatalf("Add(b): %v", err)
	}

	// Tick to grant lease to job A (capacity 1). Two ticks: first opens attempt, second grants lease.
	d.tick(context.Background())
	d.tick(context.Background())
	if !jA.HoldsLease() {
		t.Fatal("precondition: job A must hold the lease")
	}
	if jB.HoldsLease() {
		t.Fatal("precondition: job B must not hold lease when capacity is 1")
	}

	if err := d.Remove(context.Background(), "a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !st.deleted("a") {
		t.Fatal("Remove must delete the job from the store")
	}
	if _, ok := d.Job("a"); ok {
		t.Fatal("Remove must deregister the job")
	}
	if err := d.Remove(context.Background(), "a"); err == nil {
		t.Fatal("Remove of an already-removed job must error, not silently succeed")
	}

	// Next tick must advance job B since A returned its lease on Remove.
	d.tick(context.Background())
	if !jB.HoldsLease() {
		t.Fatal("Remove failed to return lease capacity: job B did not acquire lease on next tick")
	}

	stErr := &fakeStore{delErr: errors.New("disk is angry")}
	dErr := newTestDispatcher(t, withStore(stErr))
	jErr := job.New("e", "Job E", job.PolicyFromPP(3))
	if err := dErr.Add(context.Background(), jErr, Header{Name: "Job E"}); err != nil {
		t.Fatalf("Add(e): %v", err)
	}
	if err := dErr.Remove(context.Background(), "e"); err == nil {
		t.Fatal("Remove must error if store.Delete fails")
	}
	if _, ok := dErr.Job("e"); !ok {
		t.Fatal("a Remove that failed must leave the job registered")
	}
}

type blockingRunner struct {
	runCalled     chan struct{}
	releaseWorker chan struct{}
	d             *Dispatcher
}

func (r *blockingRunner) Run(_ context.Context, id string, _ job.State) {
	close(r.runCalled)
	go func() {
		<-r.releaseWorker
		_ = r.d.Yielded(id)
	}()
}

func TestRemove_WaitsForActiveWorkerBeforeEvicting(t *testing.T) {
	res := &fakeResidency{}
	runner := &blockingRunner{
		runCalled:     make(chan struct{}),
		releaseWorker: make(chan struct{}),
	}
	d := newTestDispatcher(t, withResidency(res), withRunner(runner))
	runner.d = d

	j := job.New("j1", "Job 1", job.Policy{})
	if err := d.Add(context.Background(), j, Header{Name: "Job 1"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Two ticks: first opens attempt, second grants lease, hydrates manifest, and launches worker.
	d.tick(context.Background())
	d.tick(context.Background())

	select {
	case <-runner.runCalled:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for worker to launch")
	}

	if !res.resident("j1") {
		t.Fatal("precondition: manifest must be hydrated while worker is running")
	}

	// First verify retry contract with a timed-out context.
	timeoutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := d.Remove(timeoutCtx, "j1"); err == nil {
		t.Fatal("Remove with expired context must error")
	}
	if _, ok := d.Job("j1"); !ok {
		t.Fatal("job must remain registered after failed Remove")
	}
	if !res.resident("j1") {
		t.Fatal("manifest must remain resident after failed Remove")
	}

	removeDone := make(chan error, 1)
	go func() {
		removeDone <- d.Remove(context.Background(), "j1")
	}()

	// Assert that while releaseWorker is not closed, Remove does not complete
	// and manifest is NOT evicted.
	select {
	case err := <-removeDone:
		t.Fatalf("Remove completed prematurely with %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if !res.resident("j1") {
		t.Fatal("manifest was evicted while worker was still active")
	}

	// Close releaseWorker to let the worker call Yielded and finish.
	close(runner.releaseWorker)

	select {
	case err := <-removeDone:
		if err != nil {
			t.Fatalf("Remove returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Remove timed out waiting for worker exit")
	}

	if res.resident("j1") {
		t.Fatal("manifest was not evicted after Remove completed")
	}
	if _, ok := d.Job("j1"); ok {
		t.Fatal("job must be deregistered after Remove completed")
	}
}

func TestStop_WaitsForActiveWorkersBeforeEviction(t *testing.T) {
	res := &fakeResidency{}
	runner := &blockingRunner{
		runCalled:     make(chan struct{}),
		releaseWorker: make(chan struct{}),
	}
	d := newTestDispatcher(t, withResidency(res), withRunner(runner))
	runner.d = d

	j := job.New("j1", "Job 1", job.Policy{})
	if err := d.Add(context.Background(), j, Header{Name: "Job 1"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Two ticks: first opens attempt, second grants lease, hydrates manifest, and launches worker.
	d.tick(context.Background())
	d.tick(context.Background())

	select {
	case <-runner.runCalled:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for worker to launch")
	}

	if !res.resident("j1") {
		t.Fatal("precondition: manifest must be hydrated while worker is running")
	}

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- d.Stop()
	}()

	// Assert that while releaseWorker is not closed, Stop does not complete
	// and manifest remains hydrated (not evicted).
	select {
	case err := <-stopDone:
		t.Fatalf("Stop completed prematurely with %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if !res.resident("j1") {
		t.Fatal("manifest was evicted while worker was still active")
	}

	// Close releaseWorker to let the worker call Yielded and finish.
	close(runner.releaseWorker)

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop timed out waiting for worker exit")
	}

	if res.resident("j1") {
		t.Fatal("manifest was not evicted after Stop completed")
	}
}

func TestStop_WorkerTimeout_SkipsEvictionAndAggregatesErrors(t *testing.T) {
	res := &fakeResidency{}
	runner := &blockingRunner{
		runCalled:     make(chan struct{}),
		releaseWorker: make(chan struct{}),
	}
	d := newTestDispatcher(t, withResidency(res), withRunner(runner))
	runner.d = d
	d.stopTimeout = 50 * time.Millisecond

	j := job.New("j1", "Job 1", job.Policy{})
	if err := d.Add(context.Background(), j, Header{Name: "Job 1"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	d.tick(context.Background())
	d.tick(context.Background())

	select {
	case <-runner.runCalled:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for worker to launch")
	}

	// Stop without closing releaseWorker: waitLaunched must time out.
	err := d.Stop()
	if err == nil {
		t.Fatal("expected Stop to return error on worker timeout, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "wait worker j1") {
		t.Fatalf("expected error mentioning wait worker j1, got: %v", err)
	}

	// Invariant: manifest must NOT be evicted and scheduler resources must NOT be parked
	// under a live in-flight worker.
	if !res.resident("j1") {
		t.Fatal("manifest was evicted despite worker timeout; live worker's manifest was pulled!")
	}
	if !d.q.Render(j).Holds {
		t.Fatal("job was parked despite worker timeout; live worker's lease was revoked!")
	}

	// Cleanup worker goroutine to avoid leaking into other tests.
	close(runner.releaseWorker)
}

type inspectingRunner struct {
	onRun func(ctx context.Context, id string, state job.State)
}

func (r *inspectingRunner) Run(ctx context.Context, id string, state job.State) {
	if r.onRun != nil {
		r.onRun(ctx, id, state)
	}
}

func TestStart_PropagatesContextCancellation(t *testing.T) {
	parentCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var d *Dispatcher
	workerCtxSeen := make(chan context.Context, 1)
	runner := &inspectingRunner{
		onRun: func(ctx context.Context, id string, state job.State) {
			workerCtxSeen <- ctx
			go func() {
				<-ctx.Done()
				_ = d.Yielded(id)
			}()
		},
	}
	d = newTestDispatcher(t, withRunner(runner))

	if err := d.Start(parentCtx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := d.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	j := job.New("j1", "Job 1", job.Policy{})
	if err := d.Add(context.Background(), j, Header{Name: "Job 1"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	var wCtx context.Context
outer:
	for range 50 {
		d.kick()
		select {
		case wCtx = <-workerCtxSeen:
			break outer
		case <-time.After(10 * time.Millisecond):
		}
	}
	if wCtx == nil {
		t.Fatal("worker was not launched")
	}

	// Verify worker context is not cancelled yet.
	select {
	case <-wCtx.Done():
		t.Fatal("worker context should not be cancelled yet")
	default:
	}

	// Cancel parent context.
	cancel()

	// Verify worker context receives cancellation.
	select {
	case <-wCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("worker context did not receive cancellation from parent context")
	}
}

func TestSetStopTimeout(t *testing.T) {
	t.Parallel()
	d := newTestDispatcher(t)
	d.SetStopTimeout(50 * time.Millisecond)
	d.mu.Lock()
	got := d.stopTimeout
	d.mu.Unlock()
	if got != 50*time.Millisecond {
		t.Fatalf("stopTimeout = %v, want 50ms", got)
	}
}

// TestNew_DefaultStopTimeoutIsFractionOfStepBudget pins that a fresh Dispatcher
// initializes stopTimeout to 10s (matching finalizer's 10s budget and comfortably
// within the 15s waitBounded step timeout), ensuring Stop has guaranteed margin
// to complete before waitBounded abandons it.
func TestNew_DefaultStopTimeoutIsFractionOfStepBudget(t *testing.T) {
	t.Parallel()
	d := newTestDispatcher(t)
	d.mu.Lock()
	got := d.stopTimeout
	d.mu.Unlock()
	if got != 10*time.Second {
		t.Fatalf("default stopTimeout = %v, want 10s (must fit inside 15s step budget)", got)
	}
}

type deadlineRecordingStore struct {
	fakeStore
	savedDeadline chan time.Time
}

func (s *deadlineRecordingStore) Save(ctx context.Context, p Persisted) error {
	if dl, ok := ctx.Deadline(); ok {
		select {
		case s.savedDeadline <- dl:
		default:
		}
	}
	return s.fakeStore.Save(ctx, p)
}

// TestStop_PersistTimeoutIsolatedFromStopCtx pins that persist during Stop receives
// an isolated per-job deadline (perJobPersistWait = 2s) via context.WithoutCancel(stopCtx),
// ensuring that expired stopCtx or earlier drain timeouts cannot starve persistence.
func TestStop_PersistTimeoutIsolatedFromStopCtx(t *testing.T) {
	t.Parallel()
	st := &deadlineRecordingStore{
		savedDeadline: make(chan time.Time, 1),
	}
	d := newTestDispatcher(t, withStore(st))
	d.SetStopTimeout(300 * time.Millisecond)

	j := job.New("j1", "Job 1", job.Policy{})
	if err := d.Add(context.Background(), j, Header{Name: "Job 1"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := d.PauseJob("j1"); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}

	if err := d.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	var dl time.Time
	select {
	case dl = <-st.savedDeadline:
	default:
		t.Fatal("expected Save to be called during Stop")
	}

	remaining := time.Until(dl)
	if remaining < 1*time.Second || remaining > 3*time.Second {
		t.Fatalf("persist context deadline remaining was %v, want ~2s (perJobPersistWait isolated from stopTimeout)", remaining)
	}
}

func TestStop_PerJobBudgetIsolation_SubsequentJobsNotStarved(t *testing.T) {
	res := &fakeResidency{}
	st := &fakeStore{}
	d := newTestDispatcher(t, withResidency(res), withStore(st))

	j1 := job.New("j1", "Job 1", job.Policy{})
	if err := d.Add(context.Background(), j1, Header{Name: "Job 1"}); err != nil {
		t.Fatalf("Add j1: %v", err)
	}
	j2 := job.New("j2", "Job 2", job.Policy{})
	if err := d.Add(context.Background(), j2, Header{Name: "Job 2"}); err != nil {
		t.Fatalf("Add j2: %v", err)
	}

	// Mark both jobs resident and hydrate them in fakeResidency.
	d.markResident("j1")
	_ = res.Hydrate(context.Background(), "j1")
	d.markResident("j2")
	_ = res.Hydrate(context.Background(), "j2")

	occupyEntered := make(chan struct{})
	occupyRelease := make(chan struct{})
	defer close(occupyRelease)

	// Hold occupancy on j1 across Stop().
	go func() {
		_ = d.Occupy(context.Background(), "j1", func(ctx context.Context) {
			close(occupyEntered)
			<-occupyRelease
		})
	}()

	<-occupyEntered

	// Set a total stopTimeout of 5s. With per-job isolation, j1 will time out on its
	// per-job budget, leaving remaining budget for j2 to be processed.
	d.SetStopTimeout(5 * time.Second)

	stopErr := d.Stop()
	if stopErr == nil {
		t.Fatal("Stop() returned nil, want error for j1 wait live timeout")
	}
	if !strings.Contains(stopErr.Error(), "wait live j1") {
		t.Fatalf("Stop() error = %v, want error mentioning wait live j1", stopErr)
	}

	// Invariants for j1 (occupied, timed out):
	// Skipped Park and Evict to avoid race/panics under active occupancy.
	if !d.isResident("j1") {
		t.Error("isResident(j1) = false, want true (Stop must skip markNotResident for j1 on timeout)")
	}
	if !res.resident("j1") {
		t.Error("res.resident(j1) = false, want true (Stop must skip Evict for j1 on timeout)")
	}

	// Invariants for j2 (unoccupied, must NOT be starved):
	// - was parked
	// - had changes persisted to store
	// - was evicted from residency
	// - was marked not resident
	if d.q.Render(j2).Holds {
		t.Error("j2 was not parked (Render(j2).Holds is true)")
	}
	if _, ok := st.row("j2"); !ok {
		t.Error("j2 changes were not persisted to store")
	}
	if res.resident("j2") {
		t.Error("res.resident(j2) = true, want false (j2 must be evicted)")
	}
	if d.isResident("j2") {
		t.Error("isResident(j2) = true, want false (j2 must be marked not resident)")
	}
}

func TestStop_PersistErrorAggregatedIntoStopErr(t *testing.T) {
	st := &fakeStore{}
	d := newTestDispatcher(t, withStore(st))

	j := job.New("j1", "Job 1", job.Policy{})
	if err := d.Add(context.Background(), j, Header{Name: "Job 1"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := d.PauseJob("j1"); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	st.saveErr = errors.New("simulated disk full")

	err := d.Stop()
	if err == nil {
		t.Fatal("Stop() returned nil, want error for persist failure")
	}
	if !strings.Contains(err.Error(), "persist") {
		t.Fatalf("Stop() error = %v, want error containing %q", err, "persist")
	}
}

type contextAwareStore struct {
	fakeStore
}

func (s *contextAwareStore) Save(ctx context.Context, p Persisted) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.fakeStore.Save(ctx, p)
}

func TestStop_TailJobPersistedWhenEarlierJobsExhaustStopTimeout(t *testing.T) {
	st := &contextAwareStore{}
	d := newTestDispatcher(t, withStore(st))

	j1 := job.New("j1", "Job 1", job.Policy{})
	if err := d.Add(context.Background(), j1, Header{Name: "Job 1"}); err != nil {
		t.Fatalf("Add j1: %v", err)
	}
	j2 := job.New("j2", "Job 2", job.Policy{})
	if err := d.Add(context.Background(), j2, Header{Name: "Job 2"}); err != nil {
		t.Fatalf("Add j2: %v", err)
	}

	occupyEntered := make(chan struct{})
	occupyRelease := make(chan struct{})
	defer close(occupyRelease)

	// Hold occupancy on j1 across Stop().
	go func() {
		_ = d.Occupy(context.Background(), "j1", func(ctx context.Context) {
			close(occupyEntered)
			<-occupyRelease
		})
	}()

	<-occupyEntered

	// Short stopTimeout (50ms) so j1 exhaustively burns the stopCtx budget.
	d.SetStopTimeout(50 * time.Millisecond)

	stopErr := d.Stop()
	if stopErr == nil {
		t.Fatal("Stop() returned nil, want error for j1 wait live timeout")
	}
	if !strings.Contains(stopErr.Error(), "wait live j1") {
		t.Fatalf("Stop() error = %v, want error mentioning wait live j1", stopErr)
	}

	// Invariant: j2 (the tail job) MUST be persisted despite stopCtx expiration.
	if _, ok := st.row("j2"); !ok {
		t.Error("j2 was not persisted: tail job was starved by earlier timeout")
	}
}
