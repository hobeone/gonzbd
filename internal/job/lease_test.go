package job

import (
	"errors"
	"sync"
	"testing"
)

func TestJob_GrantAndSurrender(t *testing.T) {
	j := newTestJob(t)
	if j.HoldsLease() {
		t.Fatal("a fresh job holds a lease")
	}
	l := &Lease{}
	if err := j.Grant(l); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if !j.HoldsLease() {
		t.Error("HoldsLease() is false after Grant")
	}
	if got := j.Surrender(); got != l {
		t.Errorf("Surrender() = %p, want the granted lease %p", got, l)
	}
	if j.HoldsLease() {
		t.Error("HoldsLease() is true after Surrender")
	}
}

// TestJob_SurrenderIsNilWhenNothingHeld pins the property §3.9 depends on: a
// job may legitimately reach the crossing holding no lease, having been paused
// at Assessing{next: Extracting} and resumed. Surrender must report that rather
// than assert, so the Queue's sole reclaimer can no-op on nil.
func TestJob_SurrenderIsNilWhenNothingHeld(t *testing.T) {
	j := newTestJob(t)
	if got := j.Surrender(); got != nil {
		t.Errorf("Surrender() with nothing held = %p, want nil", got)
	}
	if got := j.Surrender(); got != nil {
		t.Errorf("Surrender() twice = %p, want nil", got)
	}
}

func TestJob_GrantRefusesASecondLease(t *testing.T) {
	j := newTestJob(t)
	if err := j.Grant(&Lease{}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := j.Grant(&Lease{}); !errors.Is(err, ErrAlreadyLeased) {
		t.Errorf("second Grant, error = %v, want ErrAlreadyLeased", err)
	}
}

func TestJob_GrantRefusesNil(t *testing.T) {
	j := newTestJob(t)
	if err := j.Grant(nil); err == nil {
		t.Error("Grant(nil) = nil; a nil lease is indistinguishable from holding none")
	}
}

// TestJob_LeaseIsRaceFree runs the accessors concurrently under -race. The
// lease field is guarded by the same mutex as the lifecycle fields and must not
// be readable without it.
func TestJob_LeaseIsRaceFree(t *testing.T) {
	j := newTestJob(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 200 {
				if err := j.Grant(&Lease{}); err == nil {
					_ = j.Surrender()
				}
				_ = j.HoldsLease()
			}
		})
	}
	wg.Wait()
}

// TestJob_SurrenderLockedYieldsAndClears pins surrenderLocked's two
// behaviours directly, rather than only through Cross/Finish's public API —
// check_test_alignment flags an unexported helper that no test file
// references by name, and both callers currently reach it only via a state
// transition that would obscure which of the two behaviours failed.
//
// surrenderLocked requires the caller to already hold j.mu — that is its
// entire reason for existing separately from the exported Surrender, since
// j.mu is a non-reentrant sync.RWMutex and Cross/Finish take it themselves
// and hold it across their own bodies (see surrenderLocked's doc comment).
// So this test takes j.mu.Lock() itself before calling it, the same way
// Cross and Finish do, rather than calling it unlocked the way a
// single-goroutine test could get away with — encoding "call this only
// while already holding the lock" here is the point, not an incidental
// detail -race happens not to catch.
func TestJob_SurrenderLockedYieldsAndClears(t *testing.T) {
	j := newTestJob(t)
	l := &Lease{}
	if err := j.Grant(l); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	j.mu.Lock()
	got := j.surrenderLocked()
	j.mu.Unlock()

	if got != l {
		t.Errorf("surrenderLocked() = %p, want the granted lease %p (identity, not just non-nil)", got, l)
	}
	if j.HoldsLease() {
		t.Error("HoldsLease() is true after surrenderLocked; it must leave the job holding nothing")
	}
}

// TestJob_SurrenderLockedIsNilWhenNothingHeld pins the other half: it must
// report nil rather than panic when nothing is held, which is what lets
// Cross and Finish call it unconditionally on every success path without
// first testing HoldsLease() — see Job.Cross and Job.Finish.
func TestJob_SurrenderLockedIsNilWhenNothingHeld(t *testing.T) {
	j := newTestJob(t)

	j.mu.Lock()
	got := j.surrenderLocked()
	j.mu.Unlock()

	if got != nil {
		t.Errorf("surrenderLocked() with nothing held = %p, want nil", got)
	}
}
