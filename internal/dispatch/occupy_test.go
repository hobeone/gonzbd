package dispatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

func TestOccupy_BasicLifecycleAndIsOccupied(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "test", job.Policy{})
	if err := d.Add(j, Header{Name: "test"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if d.IsOccupied("j1") {
		t.Fatal("IsOccupied reported true before Occupy")
	}

	occupiedRan := false
	err := d.Occupy(context.Background(), "j1", func(ctx context.Context) {
		occupiedRan = true
		if !d.IsOccupied("j1") {
			t.Error("IsOccupied reported false inside Occupy")
		}
		tok, ok := ctx.Value(occupyContextKey{}).(occupyToken)
		if !ok || tok.id != "j1" || tok.token == nil {
			t.Errorf("ctx value for occupyContextKey = %v, %v; want occupyToken with id=%q, true", tok, ok, "j1")
		}
	})
	if err != nil {
		t.Fatalf("Occupy: %v", err)
	}
	if !occupiedRan {
		t.Fatal("Occupy callback did not run")
	}

	if d.IsOccupied("j1") {
		t.Fatal("IsOccupied reported true after Occupy finished")
	}
}

func TestOccupy_ReturnsNotFoundForUnknownJob(t *testing.T) {
	d := newTestDispatcher(t)
	err := d.Occupy(context.Background(), "missing", func(ctx context.Context) {
		t.Fatal("callback should not run for missing job")
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Occupy(missing) = %v, want ErrNotFound", err)
	}
}

func TestOccupy_RemoveInsideOccupyBypassesWaitLive(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "test", job.Policy{})
	if err := d.Add(j, Header{Name: "test"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Calling Remove using the occupy context must succeed without deadlocking.
	err := d.Occupy(context.Background(), "j1", func(ctx context.Context) {
		if remErr := d.Remove(ctx, "j1"); remErr != nil {
			t.Errorf("Remove inside Occupy failed: %v", remErr)
		}
	})
	if err != nil {
		t.Fatalf("Occupy: %v", err)
	}

	if _, ok := d.lookup("j1"); ok {
		t.Fatal("j1 still found after Remove inside Occupy")
	}
}

func TestOccupy_RemoveBypassesSelfDeadlock(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("job-1", "test", job.Policy{})
	if err := d.Add(j, Header{Name: "test"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.markResident("job-1")

	err := d.Occupy(context.Background(), "job-1", func(occupyCtx context.Context) {
		removeCtx, removeCancel := context.WithTimeout(occupyCtx, 100*time.Millisecond)
		defer removeCancel()

		err := d.Remove(removeCtx, "job-1")
		if err != nil {
			t.Fatalf("Remove within Occupy failed: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("Occupy: %v", err)
	}

	if _, ok := d.lookup("job-1"); ok {
		t.Fatal("job-1 still found after Remove within Occupy")
	}
}

func TestOccupy_ExternalRemoveWaitsForOccupancyToDrain(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "test", job.Policy{})
	if err := d.Add(j, Header{Name: "test"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	occupyEntered := make(chan struct{})
	occupyRelease := make(chan struct{})
	var occupyDone sync.WaitGroup

	occupyDone.Go(func() {
		_ = d.Occupy(context.Background(), "j1", func(ctx context.Context) {
			close(occupyEntered)
			<-occupyRelease
		})
	})

	<-occupyEntered

	// External Remove with a non-occupied context should wait until occupyRelease is closed.
	removeDone := make(chan error, 1)
	go func() {
		removeDone <- d.Remove(context.Background(), "j1")
	}()

	// Ensure Remove doesn't return prematurely while occupied.
	select {
	case err := <-removeDone:
		t.Fatalf("Remove returned before Occupy released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(occupyRelease)
	occupyDone.Wait()

	select {
	case err := <-removeDone:
		if err != nil {
			t.Fatalf("Remove failed after occupancy drained: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Remove timed out waiting for occupancy to drain")
	}

	if _, ok := d.lookup("j1"); ok {
		t.Fatal("j1 still present after Remove")
	}
}

func TestOccupy_RemoveErrorsWhenWaitLiveExpires(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "test", job.Policy{})
	if err := d.Add(j, Header{Name: "test"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	occupyEntered := make(chan struct{})
	occupyRelease := make(chan struct{})
	defer close(occupyRelease)

	go func() {
		_ = d.Occupy(context.Background(), "j1", func(ctx context.Context) {
			close(occupyEntered)
			<-occupyRelease
		})
	}()

	<-occupyEntered

	timeoutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := d.Remove(timeoutCtx, "j1")
	if err == nil {
		t.Fatal("Remove returned nil, want error when waitLive times out")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Remove error = %v, want DeadlineExceeded", err)
	}
}

func TestOccupy_StopSkipsParkAndEvictOnOccupancyTimeout(t *testing.T) {
	r := &fakeResidency{}
	d := newTestDispatcher(t, withResidency(r))
	d.SetStopTimeout(50 * time.Millisecond)

	j := job.New("j1", "test", job.Policy{})
	if err := d.Add(j, Header{Name: "test"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Make job resident
	d.markResident("j1")
	_ = r.Hydrate(context.Background(), "j1")

	occupyEntered := make(chan struct{})
	occupyRelease := make(chan struct{})
	defer close(occupyRelease)

	go func() {
		_ = d.Occupy(context.Background(), "j1", func(ctx context.Context) {
			close(occupyEntered)
			<-occupyRelease
		})
	}()

	<-occupyEntered

	// Stop() should time out waiting for occupancy to drain, record an error,
	// and skip Park and Evict for j1.
	stopErr := d.Stop()
	if stopErr == nil {
		t.Fatal("Stop() returned nil, want timeout error due to active occupancy")
	}

	// Verify that j1 was NOT evicted or marked not resident because Stop skipped Park/Evict for j1.
	if !d.isResident("j1") {
		t.Errorf("isResident(j1) = false, want true (Stop must skip markNotResident on timeout)")
	}
	if !r.resident("j1") {
		t.Errorf("r.resident(j1) = false, want true (Stop must skip Evict on timeout)")
	}
}

func TestWaitLive_DirectAssertions(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "test", job.Policy{})
	if err := d.Add(j, Header{Name: "test"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// 1. waitLive on an unoccupied job returns nil immediately.
	if err := d.waitLive(context.Background(), "j1"); err != nil {
		t.Fatalf("waitLive on unoccupied job = %v, want nil", err)
	}

	// 2. waitLive on an occupied job times out when context expires.
	d.mu.Lock()
	d.occupiers["j1"] = 1
	d.occupyDrained["j1"] = make(chan struct{})
	d.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := d.waitLive(ctx, "j1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitLive on occupied job with timeout = %v, want DeadlineExceeded", err)
	}

	// 3. waitLive prioritizes closed channel over expired context (pre-select).
	closedCh := make(chan struct{})
	close(closedCh)
	d.mu.Lock()
	d.occupyDrained["j1"] = closedCh
	d.mu.Unlock()

	canceledCtx, cancelImmediate := context.WithCancel(context.Background())
	cancelImmediate()
	if err := d.waitLive(canceledCtx, "j1"); err != nil {
		t.Fatalf("waitLive with closed channel and cancelled context = %v, want nil", err)
	}

	// Clean up test state
	d.mu.Lock()
	delete(d.occupiers, "j1")
	delete(d.occupyDrained, "j1")
	d.mu.Unlock()
}

func TestOccupy_RejectedWhenJobBeingRemoved(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "test", job.Policy{})
	if err := d.Add(j, Header{Name: "test"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	d.mu.Lock()
	d.removing["j1"] = 1
	d.mu.Unlock()

	err := d.Occupy(context.Background(), "j1", func(ctx context.Context) {
		t.Fatal("callback should not run when job is being removed")
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Occupy while removing = %v, want ErrNotFound", err)
	}

	d.mu.Lock()
	delete(d.removing, "j1")
	d.mu.Unlock()
}

func TestOccupy_ConcurrentOccupiers_RemoveWaitsForOtherOccupier(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "test", job.Policy{})
	if err := d.Add(j, Header{Name: "test"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	enteredA := make(chan struct{})
	releaseA := make(chan struct{})
	enteredB := make(chan struct{})
	removeDone := make(chan error, 1)

	var wg sync.WaitGroup

	// Occupier A: will stay running until released
	wg.Go(func() {
		_ = d.Occupy(context.Background(), "j1", func(ctx context.Context) {
			close(enteredA)
			<-releaseA
		})
	})

	<-enteredA

	// Occupier B: calls Remove using its own occupyCtx while A is still active
	wg.Go(func() {
		_ = d.Occupy(context.Background(), "j1", func(occupyCtx context.Context) {
			close(enteredB)
			// Remove must wait for A to exit even though B is an occupier
			removeDone <- d.Remove(occupyCtx, "j1")
		})
	})

	<-enteredB

	// Verify Remove in B does NOT return immediately while A is occupying
	select {
	case err := <-removeDone:
		t.Fatalf("Remove returned before Occupier A released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Release Occupier A
	close(releaseA)

	select {
	case err := <-removeDone:
		if err != nil {
			t.Fatalf("Remove failed after Occupier A released: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Remove timed out waiting for Occupier A to drain")
	}

	wg.Wait()

	if _, ok := d.lookup("j1"); ok {
		t.Fatal("j1 still found after Remove completed")
	}
}

func TestOccupy_StaleContext_CannotBypassWaitLive(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "test", job.Policy{})
	if err := d.Add(j, Header{Name: "test"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	var staleCtx context.Context
	err := d.Occupy(context.Background(), "j1", func(ctx context.Context) {
		staleCtx = ctx
	})
	if err != nil {
		t.Fatalf("first Occupy: %v", err)
	}

	// Now start a second occupier C that stays active
	enteredC := make(chan struct{})
	releaseC := make(chan struct{})
	defer close(releaseC)

	go func() {
		_ = d.Occupy(context.Background(), "j1", func(ctx context.Context) {
			close(enteredC)
			<-releaseC
		})
	}()

	<-enteredC

	// Calling Remove with staleCtx must NOT bypass waitLive because the token in staleCtx
	// is no longer in d.occupancyTokens. It should time out rather than succeeding immediately.
	timeoutCtx, cancel := context.WithTimeout(staleCtx, 50*time.Millisecond)
	defer cancel()

	remErr := d.Remove(timeoutCtx, "j1")
	if remErr == nil {
		t.Fatal("Remove with stale context bypassed waitLive, want DeadlineExceeded")
	}
	if !errors.Is(remErr, context.DeadlineExceeded) {
		t.Fatalf("Remove error = %v, want DeadlineExceeded", remErr)
	}
}

func TestWaitLiveExcept_DirectReference(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "test", job.Policy{})
	if err := d.Add(j, Header{Name: "test"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// 1. Unregistered token: falls back to waitLive (no occupiers -> returns nil).
	if err := d.waitLiveExcept(context.Background(), "j1", "non-existent-token"); err != nil {
		t.Fatalf("waitLiveExcept non-existent token: %v", err)
	}

	// 2. Caller is the sole occupier: returns nil immediately.
	tok1 := new(byte)
	d.mu.Lock()
	if d.occupancyTokens["j1"] == nil {
		d.occupancyTokens["j1"] = make(map[any]struct{})
	}
	d.occupancyTokens["j1"][tok1] = struct{}{}
	d.mu.Unlock()

	if err := d.waitLiveExcept(context.Background(), "j1", tok1); err != nil {
		t.Fatalf("waitLiveExcept sole occupier: %v", err)
	}

	// 3. Multiple occupiers: waitLiveExcept waits until second occupier leaves.
	tok2 := new(byte)
	d.mu.Lock()
	d.occupancyTokens["j1"][tok2] = struct{}{}
	d.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		done <- d.waitLiveExcept(context.Background(), "j1", tok1)
	}()

	select {
	case err := <-done:
		t.Fatalf("waitLiveExcept returned prematurely: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	// Release tok2 and signal
	d.mu.Lock()
	delete(d.occupancyTokens["j1"], tok2)
	if ch, ok := d.occupyStep["j1"]; ok {
		close(ch)
		delete(d.occupyStep, "j1")
	}
	d.mu.Unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waitLiveExcept after release: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("waitLiveExcept timed out waiting for release")
	}

	// 4. Context cancellation when waiting for another occupier
	d.mu.Lock()
	d.occupancyTokens["j1"][tok2] = struct{}{}
	d.mu.Unlock()

	cancelCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := d.waitLiveExcept(cancelCtx, "j1", tok1)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitLiveExcept timeout error = %v, want DeadlineExceeded", err)
	}

	// Clean up tok1 and tok2
	d.mu.Lock()
	delete(d.occupancyTokens, "j1")
	d.mu.Unlock()
}

func TestWaitLiveExcept_DoneArm_RequiresCallerPresence(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "test", job.Policy{})
	if err := d.Add(j, Header{Name: "test"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	callerTok := new(byte)
	otherTok := new(byte)

	d.mu.Lock()
	d.occupancyTokens["j1"] = map[any]struct{}{
		callerTok: {},
		otherTok:  {},
	}
	d.occupiers["j1"] = 2
	d.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())

	// Concurrently remove caller's token and cancel context so waitLiveExcept
	// wakes on ctx.Done() with len(tokens) == 1 and caller NOT present.
	go func() {
		time.Sleep(10 * time.Millisecond)
		d.mu.Lock()
		delete(d.occupancyTokens["j1"], callerTok)
		d.occupiers["j1"]--
		d.mu.Unlock()
		cancel()
	}()

	err := d.waitLiveExcept(ctx, "j1", callerTok)
	if err == nil {
		t.Fatal("waitLiveExcept returned nil under canceled context while another occupier was live")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitLiveExcept returned error = %v, want context.Canceled", err)
	}
}
