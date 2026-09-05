package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestDispatcherControlSurface_PerJobDoors pins the doors the API needs and
// did not have: a job pointer, and per-job pause/resume distinct from the
// queue-wide flag.
func TestDispatcherControlSurface_PerJobDoors(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("a", "Job A", job.PolicyFromPP(3))
	if err := d.Add(j, Header{Name: "Job A"}); err != nil {
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
	if err := d.Add(jA, Header{Name: "Job A"}); err != nil {
		t.Fatalf("Add(a): %v", err)
	}
	jB := job.New("b", "Job B", job.PolicyFromPP(3))
	if err := d.Add(jB, Header{Name: "Job B"}); err != nil {
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
	if err := dErr.Add(jErr, Header{Name: "Job E"}); err != nil {
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
	if err := d.Add(j, Header{Name: "Job 1"}); err != nil {
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
	if err := d.Add(j, Header{Name: "Job 1"}); err != nil {
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
	if err := d.Add(j, Header{Name: "Job 1"}); err != nil {
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

	workerCtxSeen := make(chan context.Context, 1)
	runner := &inspectingRunner{
		onRun: func(ctx context.Context, id string, state job.State) {
			workerCtxSeen <- ctx
		},
	}
	d := newTestDispatcher(t, withRunner(runner))

	if err := d.Start(parentCtx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer d.Stop() //nolint:errcheck

	j := job.New("j1", "Job 1", job.Policy{})
	if err := d.Add(j, Header{Name: "Job 1"}); err != nil {
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

func TestStop_PersistTimeoutBoundedByWaitCtx(t *testing.T) {
	t.Parallel()
	st := &deadlineRecordingStore{
		savedDeadline: make(chan time.Time, 1),
	}
	d := newTestDispatcher(t, withStore(st))
	d.SetStopTimeout(300 * time.Millisecond)

	j := job.New("j1", "Job 1", job.Policy{})
	if err := d.Add(j, Header{Name: "Job 1"}); err != nil {
		t.Fatalf("Add: %v", err)
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
	if remaining > 500*time.Millisecond {
		t.Fatalf("persist context timeout was %v, exceeding stopTimeout budget (must be bounded by waitCtx)", remaining)
	}
}

func TestStop_PerJobBudgetIsolation_SubsequentJobsNotStarved(t *testing.T) {
	res := &fakeResidency{}
	st := &fakeStore{}
	d := newTestDispatcher(t, withResidency(res), withStore(st))

	j1 := job.New("j1", "Job 1", job.Policy{})
	if err := d.Add(j1, Header{Name: "Job 1"}); err != nil {
		t.Fatalf("Add j1: %v", err)
	}
	j2 := job.New("j2", "Job 2", job.Policy{})
	if err := d.Add(j2, Header{Name: "Job 2"}); err != nil {
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
