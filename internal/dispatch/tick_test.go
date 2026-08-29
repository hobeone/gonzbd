package dispatch

import (
	"bytes"
	"context"
	"errors"
	"strings"
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

func TestStart_IsNotBlockingAndStopIsIdempotent(t *testing.T) {
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
