package dispatch

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

type p1BlockingSaveStore struct {
	fakeStore
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (s *p1BlockingSaveStore) Save(ctx context.Context, p Persisted) error {
	if s.calls.Add(1) == 1 {
		close(s.entered)
		<-s.release
	}
	return s.fakeStore.Save(ctx, p)
}

func TestAdd_PersistFailureUnwindsWithoutTickTouchingJob(t *testing.T) {
	wantErr := errors.New("simulated sqlite disk full")
	st := &p1BlockingSaveStore{
		saveErr: wantErr,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	res := &fakeResidency{}
	runner := &fakeRunner{}
	d := newTestDispatcher(t, withStore(st), withResidency(res), withRunner(runner))

	j := job.New("unwritten-1", "Unwritten Job", job.Policy{})
	addErrCh := make(chan error, 1)
	go func() {
		addErrCh <- d.Add(t.Context(), j, Header{Name: "Unwritten Job"})
	}()

	// Wait until Add has registered j and blocked inside persistIfChanged's Save
	// (which holds d.storeMu).
	<-st.entered

	// Drive multiple ticks while Save is blocked. Because snapshotOrder filters
	// on d.written, none of these ticks may call Advance (BeginAttempt / grantFor),
	// reconcileResidency (Hydrate), launch, or persistIfChanged on j.
	// Run in a goroutine because if snapshotOrder's d.written filter is removed,
	// tick() advances j and then blocks on d.storeMu inside its own persistIfChanged.
	var releaseOnce sync.Once
	releaseSave := func() { releaseOnce.Do(func() { close(st.release) }) }
	t.Cleanup(releaseSave)

	ticksDone := make(chan struct{})
	go func() {
		defer close(ticksDone)
		for range 3 {
			d.tick(t.Context())
		}
	}()

	select {
	case <-ticksDone:
	case <-time.After(50 * time.Millisecond):
		// A tick entered persistIfChanged and blocked on d.storeMu while Save
		// was still in flight; release Save so the tick can finish and our
		// state/lease/residency assertions below report what the tick did.
		releaseSave()
		<-ticksDone
	}

	if snap := j.Snapshot(); snap.State.State != job.StateUnset {
		t.Fatalf("job state during blocked Save = %v, want StateUnset (tick must not advance unwritten job)", snap.State.State)
	}
	if j.HoldsLease() {
		t.Fatal("job holds a scheduler lease during blocked Save; tick must not grant leases to an unwritten job")
	}
	if res.resident("unwritten-1") || d.isResident("unwritten-1") {
		t.Fatal("job was hydrated during blocked Save; tick must not hydrate an unwritten job")
	}
	if runner.started("unwritten-1") {
		t.Fatal("worker was launched during blocked Save")
	}

	// Now release Save (which returns wantErr) and wait for Add to unwind.
	releaseSave()
	err := <-addErrCh
	if !errors.Is(err, wantErr) {
		t.Fatalf("Add error = %v, want %v", err, wantErr)
	}

	// Drive another tick after the failed Add to confirm nothing remained.
	d.tick(t.Context())

	if snap := j.Snapshot(); snap.State.State != job.StateUnset {
		t.Fatalf("job state after failed Add = %v, want StateUnset", snap.State.State)
	}
	if j.HoldsLease() {
		t.Fatal("job holds a scheduler lease after failed Add")
	}
	if res.resident("unwritten-1") || d.isResident("unwritten-1") {
		t.Fatal("job is resident after failed Add")
	}
	if runner.started("unwritten-1") {
		t.Fatal("worker was launched after failed Add")
	}
	if got := st.saveCount(); got != 0 {
		t.Fatalf("saved row count after failed Add = %d, want 0", got)
	}
	if rows := d.List(); len(rows) != 0 {
		t.Fatalf("List() after failed Add = %v, want empty", rows)
	}
	if _, ok := d.Job("unwritten-1"); ok {
		t.Fatal("Job(unwritten-1) still present in registry after failed Add")
	}
}

func TestAdd_PostPersistKickWakesTickAfterBlockedSave(t *testing.T) {
	st := &p1BlockingSaveStore{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	runner := &fakeRunner{}
	// newTestDispatcher uses a 1-hour ticker interval, so the background run loop
	// only advances within our 250ms timeout when woken via d.wake (d.kick()).
	d := newTestDispatcher(t, withStore(st), withRunner(runner))
	runner.d = d
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = d.Yielded("delayed-save-1")
		_ = d.Stop()
	})

	var releaseOnce sync.Once
	releaseSave := func() { releaseOnce.Do(func() { close(st.release) }) }
	t.Cleanup(releaseSave)

	j := job.New("delayed-save-1", "Delayed Save Job", job.Policy{})
	if err := j.BeginAttempt(testClock()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	addErrCh := make(chan error, 1)
	go func() {
		addErrCh <- d.Add(t.Context(), j, Header{Name: "Delayed Save Job"})
	}()

	<-st.entered
	// Wait briefly for the background run loop to consume register's kick while
	// Save is still blocked and j is still unwritten.
	waitDeadline := time.Now().Add(50 * time.Millisecond)
	for len(d.wake) > 0 && time.Now().Before(waitDeadline) {
		time.Sleep(time.Millisecond)
	}
	if runner.started("delayed-save-1") {
		releaseSave()
		t.Fatal("worker launched while unwritten")
	}

	// Release Save; Add marks j written and must call d.kick() so the run loop
	// wakes immediately instead of waiting for the 1-hour ticker.
	releaseSave()
	if err := <-addErrCh; err != nil {
		t.Fatalf("Add: %v", err)
	}

	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		if runner.started("delayed-save-1") {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("job was not launched promptly after Save completed; Add did not kick after persisting")
}
