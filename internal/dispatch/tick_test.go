package dispatch

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/sched"
)

func TestTick_PromotesWithoutAKick(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Drain the kick Add queued, so only the tick itself can promote.
	select {
	case <-d.wake:
	default:
	}

	d.tick(context.Background())

	if !j.HasRun() {
		t.Fatal("job never started — the ticker alone must be sufficient for liveness; if this needs a kick, the kick has become a second owner (D-B7)")
	}
}

func TestTick_WalksInQueueOrder(t *testing.T) {
	// One lease, two jobs: the first in queue order must win it.
	d := newTestDispatcher(t, withCaps(1, 1))
	first := job.New("first", "n", job.Policy{})
	second := job.New("second", "n", job.Policy{})
	for _, j := range []*job.Job{first, second} {
		if err := d.Add(j, Header{}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	d.tick(context.Background())
	d.tick(context.Background())

	if !d.q.Render(first).Running {
		t.Error("first is not running — the head of the queue must win the only lease")
	}
	if d.q.Render(second).Running {
		t.Error("second is running — it must wait behind first for the only lease")
	}
}

// TestStop_IsIdempotent covers Stop only. An earlier name also claimed Start
// is non-blocking, which this body cannot observe and which is not true as
// written: Start calls restore(ctx) — and so Store.Load — synchronously
// before it launches run, so it returns only once the restore has finished.
// With the immediate fakeStore there is no window to detect either way.
func TestStop_IsIdempotent(t *testing.T) {
	d := newTestDispatcher(t)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := d.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := d.Stop(); err != nil {
		t.Fatalf("second Stop returned %v, want nil — Stop must be idempotent", err)
	}
}

func TestStart_TwiceIsAnError(t *testing.T) {
	d := newTestDispatcher(t)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })
	if err := d.Start(context.Background()); err == nil {
		t.Fatal("second Start returned nil, want an error — a second ticker breaks D-B7's single-goroutine premise, and two concurrent walks would need locking that this design does not have")
	}
}

// TestStart_AfterStopReturnsAnErrorRatherThanPanicking pins Stop's terminal
// contract: d.stop and d.done are created once, in New, and Stop closes both.
// A naive Start that only checked d.started would proceed after Stop clears
// it, launch a second `go d.run(ctx)`, and that goroutine would select the
// already-closed d.stop and return immediately — its deferred close(d.done)
// then panicking on an already-closed channel, with Start itself having
// already returned nil. This asserts the safe outcome instead: a distinct
// error, and no panic, which running under `go test` would catch regardless
// since an unrecovered panic in a spawned goroutine crashes the whole test
// binary.
func TestStart_AfterStopReturnsAnErrorRatherThanPanicking(t *testing.T) {
	d := newTestDispatcher(t)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := d.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if err := d.Start(context.Background()); err == nil {
		t.Fatal("Start after Stop returned nil, want an error — Stop is terminal and a Dispatcher must not be restarted")
	}

	// Give a wrongly-spawned second run goroutine a chance to panic before
	// the test process exits, so a regression fails this test rather than
	// crashing an unrelated one later in the same binary.
	time.Sleep(20 * time.Millisecond)
}

func TestStop_ParksHoldersRatherThanSettlingThem(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background()) // begin, then grant
	if !d.q.Render(j).Running {
		t.Fatal("setup: job is not running")
	}

	if err := d.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := d.q.Render(j).Outcome; got.IsSettled() {
		t.Errorf("Outcome = %v after Stop, want unsettled — a shutdown is not an outcome, and recording one would contradict what is on disk", got)
	}
	if j.HoldsLease() {
		t.Error("job still holds its lease after Stop — Stop must park every holder so the pools are returned")
	}
}

// TestStop_AfterAFailedStartReturnsRatherThanDeadlocking pins the failure
// mode a naive fix reintroduces: if Start's restore error leaves d.started
// true, a later Stop reads wasStarted true, closes d.stop, and blocks
// forever on <-d.done — because run never launched on this path, and
// nothing else closes d.done. A test that just called Stop() and asserted
// on its return value would hang the whole suite instead of failing, which
// is worse than not testing this at all. So Stop runs in its own goroutine
// and the test asserts on a channel with a bounded timeout: on timeout it
// fails with a message that names the deadlock instead of leaving the
// runner to time out the whole package with no diagnosis.
func TestStop_AfterAFailedStartReturnsRatherThanDeadlocking(t *testing.T) {
	wantErr := errors.New("boom")
	d := newTestDispatcher(t, withStore(&failingLoadStore{err: wantErr}))

	if err := d.Start(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Start() = %v, want an error wrapping %v", err, wantErr)
	}

	done := make(chan error, 1)
	go func() { done <- d.Stop() }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Stop() = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop deadlocked after a failed Start — d.started was left true with no run goroutine to close d.done")
	}
}

// TestTick_LogsAndSkipsAJobWhoseAdvanceErrors pins the branch a good-weather
// walk never reaches: q.Advance returning a real error, which tick must log
// and step past rather than let one job's failure abort the rest of the
// walk (Standing Design Rule 3's blast-radius argument, applied here rather
// than to an article).
//
// sched.Queue.Advance's error sources are all documented in
// internal/sched/advance.go as unreachable through a single Queue's own
// calls — moveTo's refused-Transition arm says so explicitly ("The
// refused-Transition path is unreachable through Advance's own
// preconditions"), and BeginAttempt's and Cross's guards can only disagree
// with what Advance just read if something outside that one q.mu-held call
// mutated the job in between, which a single well-behaved Queue never does.
// The one legitimate way to produce a real Advance error is therefore to
// misuse the API across TWO Queues sharing one Job: let d1's queue issue the
// job's lease and cross it into Production up to the boundary, then hand the
// same job to a second dispatcher (d2) whose queue never issued that lease.
// d2.tick's Advance call reaches the boundary-crossing arm, j.Cross succeeds
// (crossing is a property of the JOB, not of which queue is asking), and
// d2.q.reclaim(l) then fails sched's identity audit — pool.go's leasePool
// reclaim: "if !p.issued[l.ID()] { return fmt.Errorf(...ErrNotOutstanding...) }"
// — because d2's pool never issued that lease ID. That failure is
// deterministic and requires no goroutine, no sleep and no race: it is a
// single-threaded misuse of the public API, not a timing accident.
func TestTick_LogsAndSkipsAJobWhoseAdvanceErrors(t *testing.T) {
	j := job.New("j1", "n", job.Policy{})

	d1 := newTestDispatcher(t, withCaps(2, 2))
	if err := d1.Add(j, Header{}); err != nil {
		t.Fatalf("d1.Add: %v", err)
	}
	d1.tick(context.Background()) // branch 1: begins the attempt at Fetching
	d1.tick(context.Background()) // branch 2: d1's pool grants the lease
	if !j.HoldsLease() {
		t.Fatal("setup: job does not hold a lease after two ticks on d1")
	}
	if err := j.SetNext(job.Assessing); err != nil { // Fetching -> Assessing is a legal edge
		t.Fatalf("SetNext(Assessing): %v", err)
	}
	d1.tick(context.Background()) // branch 3, non-crossing: moveTo grants d1's slot and transitions
	if got := d1.q.Render(j).State; got != job.Assessing {
		t.Fatalf("setup: job is at %v, want Assessing", got)
	}
	if err := j.SetNext(job.Extracting); err != nil { // Assessing -> Extracting is the one byCross edge
		t.Fatalf("SetNext(Extracting): %v", err)
	}

	// A second, independent dispatcher — its queue has issued this job
	// nothing. Its tick reaches the crossing arm and fails the reclaim.
	var buf bytes.Buffer
	d2 := newTestDispatcher(t, withCaps(2, 2))
	d2.log = captureLogger(&buf)
	if err := d2.Add(j, Header{}); err != nil {
		t.Fatalf("d2.Add: %v", err)
	}
	// Registered AFTER the failing job, so it is reached only if the walk
	// carries on past the error. Without it the test observes the log and
	// nothing else, and tick's `continue` could be a `return` unnoticed.
	later := job.New("j2", "n", job.Policy{})
	if err := d2.Add(later, Header{}); err != nil {
		t.Fatalf("d2.Add(later): %v", err)
	}

	d2.tick(context.Background())

	out := buf.String()
	if !strings.Contains(out, "advance failed") {
		t.Fatalf("log output %q does not contain \"advance failed\" — tick's error branch did not run", out)
	}
	if !strings.Contains(out, "job_id=j1") {
		t.Errorf("log output %q does not name the failing job", out)
	}
	// The job itself DID cross — j.Cross mutates the job before d2 ever
	// attempts the reclaim that then fails — so it now reports Extracting
	// even though d2's tick logged an error for it.
	if got := j.State().State; got != job.Extracting {
		t.Errorf("job State = %v after the failed reclaim, want Extracting — Cross succeeds independently of which queue reclaims the lease", got)
	}
	if got := d2.q.Render(later).State; got != job.Fetching {
		t.Errorf("the job queued behind the failing one is at %v, want Fetching — tick must SKIP a job whose Advance errors and keep walking, not abandon the pass", got)
	}
}

func TestNew_PanicsOnEachNilPort(t *testing.T) {
	valid := func() (sched.Workers, Residency, Store, Runner) {
		return &stubWorkers{}, &fakeResidency{}, &fakeStore{}, &fakeRunner{}
	}
	tests := []struct {
		name string
		call func()
	}{
		{"nil Workers", func() {
			_, r, s, run := valid()
			New(1, 1, time.Hour, testClock, nil, r, s, run)
		}},
		{"nil Residency", func() {
			w, _, s, run := valid()
			New(1, 1, time.Hour, testClock, w, nil, s, run)
		}},
		{"nil Store", func() {
			w, r, _, run := valid()
			New(1, 1, time.Hour, testClock, w, r, nil, run)
		}},
		{"nil Runner", func() {
			w, r, s, _ := valid()
			New(1, 1, time.Hour, testClock, w, r, s, nil)
		}},
		{"non-positive tick", func() {
			w, r, s, run := valid()
			New(1, 1, 0, testClock, w, r, s, run)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("New did not panic")
				}
			}()
			tc.call()
		})
	}
}

func TestNew_BuildsAWorkingDispatcher(t *testing.T) {
	runner := &fakeRunner{}
	d := New(2, 2, time.Hour, testClock, &stubWorkers{}, &fakeResidency{}, &fakeStore{}, runner)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background())
	if !j.HasRun() {
		t.Fatal("job never started on a Dispatcher built by New")
	}
	if !runner.started(j.ID()) {
		t.Fatal("New's Runner was never invoked — launch is wired to a nil-checked field, not a live one")
	}
}

// TestRun_ExitsOnStopWithoutStart calls run directly rather than through
// Start, so the loop's own exit path is under a direct reference rather than
// only reachable through Start's wrapper.
func TestRun_ExitsOnStopWithoutStart(t *testing.T) {
	d := newTestDispatcher(t)
	go d.run(context.Background())

	close(d.stop)
	select {
	case <-d.done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not exit after d.stop was closed")
	}
}

// TestRun_AdvancesOnAWakeAndStopsOnContextCancel drives run's remaining two
// select arms directly: the d.wake case (via Add's kick, after the ticker
// itself has been given an interval too long to fire during the test) and
// ctx.Done (by cancelling the context Start was given). Both are exercised
// through the public Start/Stop surface rather than by waiting on the real
// ticker, so nothing here depends on d.tickEvery firing.
func TestRun_AdvancesOnAWakeAndStopsOnContextCancel(t *testing.T) {
	d := newTestDispatcher(t) // tickEvery is one hour — only the wake can drive this
	ctx, cancel := context.WithCancel(context.Background())
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil { // Add's kick wakes run
		t.Fatalf("Add: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for !j.HasRun() {
		if time.Now().After(deadline) {
			t.Fatal("job never started — the wake did not drive a tick")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case <-d.done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not exit after ctx was cancelled")
	}

	if err := d.Stop(); err != nil {
		t.Fatalf("Stop after ctx cancel: %v", err)
	}
}

// TestRestore_SurfacesTheStoreError pins restore's own contract directly:
// it must neither swallow a Load error nor fabricate one.
func TestRestore_SurfacesTheStoreError(t *testing.T) {
	wantErr := errors.New("boom")
	d := newTestDispatcher(t, withStore(&failingLoadStore{err: wantErr}))
	if err := d.restore(context.Background()); !errors.Is(err, wantErr) {
		t.Errorf("restore() = %v, want %v", err, wantErr)
	}

	d2 := newTestDispatcher(t)
	if err := d2.restore(context.Background()); err != nil {
		t.Errorf("restore() = %v, want nil for a Store whose Load succeeds", err)
	}
}

type failingLoadStore struct{ err error }

func (f *failingLoadStore) Load(context.Context) ([]Persisted, error) { return nil, f.err }
func (f *failingLoadStore) Save(context.Context, Persisted) error     { return nil }
func (f *failingLoadStore) Delete(context.Context, string) error      { return nil }

// blockingLoadStore's Load announces that it has been entered, waits to be
// released, and then fails. It is what lets a test hold Start inside restore
// — the unlocked window D-B9 forces, since restore does Store I/O — while a
// concurrent Stop runs to completion, with no sleep and no polling.
type blockingLoadStore struct {
	entered chan struct{}
	release chan struct{}
	err     error
}

func (b *blockingLoadStore) Load(context.Context) ([]Persisted, error) {
	close(b.entered)
	<-b.release
	return nil, b.err
}
func (b *blockingLoadStore) Save(context.Context, Persisted) error { return nil }
func (b *blockingLoadStore) Delete(context.Context, string) error  { return nil }

// TestStop_ConcurrentWithAFailingStartReturnsRatherThanDeadlocking pins the
// interleaving TestStop_AfterAFailedStart above cannot reach, because that
// one calls Start and Stop in sequence: there, Start has already cleared
// started before Stop reads it, so Stop takes its no-op path.
//
// The dangerous order is Stop landing WHILE Start is inside restore. Start
// claims started, releases d.mu (D-B9 forbids holding it across the Store
// I/O restore does), and in that window Stop reads wasStarted true, latches
// stopped, closes d.stop and blocks on <-d.done. If restore then fails,
// Start returns without ever spawning run — so run's deferred close(d.done)
// never happens and Stop waits forever. Start's error path must close d.done
// itself when it sees the stopped latch.
//
// Every step is driven by a channel, so nothing here depends on timing: the
// test waits for Load to be entered, then for Stop to close d.stop (which
// Stop does only after latching stopped under d.mu), and only then releases
// Load to fail. The final assertion is a bounded select rather than a plain
// receive so that a regression FAILS with a diagnosis instead of wedging the
// package until the go test timeout.
func TestStop_ConcurrentWithAFailingStartReturnsRatherThanDeadlocking(t *testing.T) {
	wantErr := errors.New("boom")
	st := &blockingLoadStore{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		err:     wantErr,
	}
	d := newTestDispatcher(t, withStore(st))

	startErr := make(chan error, 1)
	go func() { startErr <- d.Start(context.Background()) }()
	<-st.entered // Start has claimed started and is inside restore.

	stopErr := make(chan error, 1)
	go func() { stopErr <- d.Stop() }()
	<-d.stop // Stop has latched stopped and is now blocked on <-d.done.

	close(st.release) // Let restore fail.

	select {
	case err := <-stopErr:
		if err != nil {
			t.Errorf("Stop() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop deadlocked: a Stop that latched stopped while Start was inside restore is waiting on d.done, which Start's restore-failure path never closed")
	}

	if err := <-startErr; !errors.Is(err, wantErr) {
		t.Errorf("Start() = %v, want an error wrapping %v", err, wantErr)
	}
}

// blockingSaveStore holds a tick in flight. persistIfChanged is the last stage
// of the per-job walk and runs on the very first tick after Add — Advance
// opens the attempt at Fetching, which needsLease and does not yet hold one,
// so neither the residency nor the launch stage does anything — which makes
// Save the earliest deterministic place to park a tick inside run.
type blockingSaveStore struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingSaveStore) Load(context.Context) ([]Persisted, error) { return nil, nil }
func (b *blockingSaveStore) Delete(context.Context, string) error      { return nil }
func (b *blockingSaveStore) Save(context.Context, Persisted) error {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
	return nil
}

// TestStop_ConcurrentStopsBothWaitForTheInFlightTick pins that EVERY Stop
// caller waits for shutdown, not just the first.
//
// Stop reads wasStarted and clears started under d.mu. A second Stop landing
// while the first is still blocked on <-d.done therefore reads wasStarted
// FALSE, skips the wait entirely, and walks straight into the teardown sweep —
// q.Park, res.Evict, markNotResident, clearLaunched — while the background
// tick is still calling Advance, Hydrate and Runner.Run. It then returns nil
// to its caller, reporting a shutdown that has not happened.
//
// The pin is that the second Stop must still be blocked while the tick is
// parked. It cannot produce a false failure: once every caller waits on the
// same shutdown, no Stop can return before release is closed.
func TestStop_ConcurrentStopsBothWaitForTheInFlightTick(t *testing.T) {
	bs := &blockingSaveStore{entered: make(chan struct{}), release: make(chan struct{})}
	d := newTestDispatcher(t, withStore(bs))
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := d.Add(job.New("j1", "n", job.Policy{}), Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	<-bs.entered // a tick is now parked inside run, mid-walk

	first := make(chan error, 1)
	go func() { first <- d.Stop() }()
	<-d.stop // the first Stop has latched stopped and is waiting on d.done

	second := make(chan error, 1)
	go func() { second <- d.Stop() }()

	select {
	case err := <-second:
		t.Fatalf("a second Stop returned %v while a tick is still in flight — "+
			"it read wasStarted false, skipped <-d.done, and is tearing down "+
			"resources underneath a live tick", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(bs.release)
	for i, ch := range []chan error{first, second} {
		select {
		case err := <-ch:
			if err != nil {
				t.Errorf("Stop %d = %v, want nil", i+1, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Stop %d never returned after the tick was released", i+1)
		}
	}
}

// TestStart_StoppedDuringASucceedingRestoreDoesNotStart is the success-path
// twin of TestStop_ConcurrentWithAFailingStartReturnsRatherThanDeadlocking.
// That test covers restore FAILING while a Stop is latched; this one covers it
// succeeding, which took the opposite branch and checked nothing.
//
// Start claims started, releases d.mu (D-B9 forbids holding it across
// restore's Store I/O), and in that window a Stop latches stopped, closes
// d.stop and blocks on d.done. When restore then succeeds, Start used to
// launch run and return nil — reporting success for a dispatcher that is
// already stopped, and handing run a d.stop that is ALREADY closed while
// Add's kick has primed d.wake. Both cases of that select are then ready at
// once, and Go picks uniformly at random, so run could take the wake, tick,
// and launch workers after shutdown.
func TestStart_StoppedDuringASucceedingRestoreDoesNotStart(t *testing.T) {
	st := &blockingLoadStore{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		err:     nil, // restore SUCCEEDS — the branch the failing twin cannot reach
	}
	d := newTestDispatcher(t, withStore(st))

	startErr := make(chan error, 1)
	go func() { startErr <- d.Start(context.Background()) }()
	<-st.entered // Start has claimed started and is inside restore.

	stopErr := make(chan error, 1)
	go func() { stopErr <- d.Stop() }()
	<-d.stop // Stop has latched stopped and is now blocked on <-d.done.

	close(st.release) // Let restore succeed.

	select {
	case err := <-startErr:
		if err == nil {
			t.Error("Start() = nil after a Stop latched during restore — it " +
				"reported success for a dispatcher that is already stopped, and " +
				"launched run against an already-closed d.stop")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start never returned")
	}

	select {
	case err := <-stopErr:
		if err != nil {
			t.Errorf("Stop() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop deadlocked: nothing closed d.done on the succeeding-restore path")
	}
}

// countingStore records how many times a tick reached persistIfChanged.
type countingStore struct {
	mu    sync.Mutex
	saves int
}

func (c *countingStore) Load(context.Context) ([]Persisted, error) { return nil, nil }
func (c *countingStore) Delete(context.Context, string) error      { return nil }
func (c *countingStore) Save(context.Context, Persisted) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saves++
	return nil
}

// TestRun_ShutdownBeatsAPrimedWake pins that run never ticks once d.stop is
// closed, even when d.wake is primed at the same instant.
//
// A select with several ready cases picks one uniformly at random. With a
// closed d.stop AND a primed d.wake — the state Add's kick plus a concurrent
// Stop produces — the old loop had roughly even odds of taking the wake,
// ticking, and launching workers after shutdown.
//
// The repetition is the point: one trial proves nothing about a coin toss.
// Against the unprioritised loop this fails almost immediately; with shutdown
// checked first it cannot fail at all, because no ordering exists in which
// the tick runs.
func TestRun_ShutdownBeatsAPrimedWake(t *testing.T) {
	const trials = 200
	for range trials {
		cs := &countingStore{}
		d := newTestDispatcher(t, withStore(cs))
		if err := d.Add(job.New("j1", "n", job.Policy{}), Header{}); err != nil {
			t.Fatalf("Add: %v", err)
		}
		// Add primed d.wake. Close d.stop so both cases are ready before run
		// ever reaches its select.
		close(d.stop)

		done := make(chan struct{})
		go func() { defer close(done); d.run(context.Background()) }()
		<-done

		cs.mu.Lock()
		saves := cs.saves
		cs.mu.Unlock()
		if saves != 0 {
			t.Fatalf("run ticked after d.stop was closed (%d store writes) — "+
				"it took the primed wake instead of the shutdown, which is the "+
				"coin toss a multi-ready select makes", saves)
		}
	}
}

// TestStop_ParkErrorRecordsFirstError pins that Stop returns park errors and
// keeps the first error when multiple jobs fail to park.
func TestStop_ParkErrorRecordsFirstError(t *testing.T) {
	d := newTestDispatcher(t)
	j1 := job.New("j1", "Job 1", job.Policy{})
	_ = j1.BeginAttempt(testClock())
	j1.Grant(job.NewLease(9991))
	if err := d.Add(j1, Header{Name: "Job 1"}); err != nil {
		t.Fatalf("Add(j1): %v", err)
	}

	j2 := job.New("j2", "Job 2", job.Policy{})
	_ = j2.BeginAttempt(testClock())
	j2.Grant(job.NewLease(9992))
	if err := d.Add(j2, Header{Name: "Job 2"}); err != nil {
		t.Fatalf("Add(j2): %v", err)
	}

	err := d.Stop()
	if err == nil {
		t.Fatal("Stop() = nil, want park error on foreign lease")
	}
	if !strings.Contains(err.Error(), "park j1") {
		t.Errorf("Stop() error = %v, want to contain first error (park j1)", err)
	}
	if strings.Contains(err.Error(), "park j2") {
		t.Errorf("Stop() error = %v, should keep first error, not be overwritten by park j2", err)
	}
}
