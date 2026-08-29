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
	valid := func() (sched.Workers, Residency, Store) {
		return &stubWorkers{}, &fakeResidency{}, &fakeStore{}
	}
	tests := []struct {
		name string
		call func()
	}{
		{"nil Workers", func() {
			_, r, s := valid()
			New(1, 1, time.Hour, testClock, nil, r, s)
		}},
		{"nil Residency", func() {
			w, _, s := valid()
			New(1, 1, time.Hour, testClock, w, nil, s)
		}},
		{"nil Store", func() {
			w, r, _ := valid()
			New(1, 1, time.Hour, testClock, w, r, nil)
		}},
		{"non-positive tick", func() {
			w, r, s := valid()
			New(1, 1, 0, testClock, w, r, s)
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
	d := New(2, 2, time.Hour, testClock, &stubWorkers{}, &fakeResidency{}, &fakeStore{})
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	if !j.HasRun() {
		t.Fatal("job never started on a Dispatcher built by New")
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

// TestRestore_SurfacesTheStoreError pins restore's own contract directly —
// Task 7 replaces the body, but until then it must neither swallow a Load
// error nor fabricate one.
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
