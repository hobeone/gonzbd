package job

import (
	"errors"
	"sync"
	"testing"
	"unsafe"
)

func TestJob_GrantAndSurrender(t *testing.T) {
	j := newTestJob(t)
	if j.HoldsLease() {
		t.Fatal("a fresh job holds a lease")
	}
	l := NewLease(1)
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
	if err := j.Grant(NewLease(1)); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := j.Grant(NewLease(2)); !errors.Is(err, ErrAlreadyLeased) {
		t.Errorf("second Grant, error = %v, want ErrAlreadyLeased", err)
	}
}

func TestJob_GrantRefusesNil(t *testing.T) {
	j := newTestJob(t)
	// Matched on the sentinel rather than on non-nil-ness. Grant has two
	// refusals — this one and ErrAlreadyLeased — and an `err == nil` check
	// passes for either, so it would not notice the two being confused.
	if err := j.Grant(nil); !errors.Is(err, ErrNilLease) {
		t.Errorf("Grant(nil) = %v, want ErrNilLease; a nil lease is indistinguishable from holding none", err)
	}
}

// TestJob_LeaseIsRaceFree runs the accessors concurrently under -race. The
// lease field is guarded by the same mutex as the lifecycle fields and must not
// be readable without it.
//
// The lease MUST be a minted one. With &Lease{} every Grant is refused by the
// id guard, so Surrender is never reached and the interleaving this test exists
// for stops happening — while the test still passes, because a race detector
// finds no race in code that does not run. The id per goroutine is only so the
// leases are distinguishable in a failure; this test is about the mutex, not
// identity.
func TestJob_LeaseIsRaceFree(t *testing.T) {
	j := newTestJob(t)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for range 200 {
				if err := j.Grant(NewLease(LeaseID(g + 1))); err == nil {
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
	l := NewLease(1)
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

// TestLease_HasDistinctIdentity pins the reason Lease carries an id at all.
//
// Go gives two distinct zero-size allocations the same address — permitted by
// the spec, and true in practice here. A pool that tracks outstanding leases by
// pointer would therefore conflate every job holding one. The sizeof assertion
// is the direct statement of the hazard; the pointer assertion is the
// consequence a reader can act on.
func TestLease_HasDistinctIdentity(t *testing.T) {
	if unsafe.Sizeof(Lease{}) == 0 {
		t.Fatal("Lease is zero-sized; distinct leases will share an address and any " +
			"pool keyed on lease identity will conflate the jobs holding them")
	}
	a, b := NewLease(1), NewLease(2)
	if a == b {
		t.Error("two distinct leases compare equal by pointer")
	}
	if a.ID() == b.ID() {
		t.Errorf("NewLease(1).ID() == NewLease(2).ID() == %v", a.ID())
	}
}

// TestJob_GrantRefusesUnidentifiedLease closes the loophole the id opens. An
// external caller can still write job.Lease{} — every field is unexported, but
// an empty composite literal of an exported type is legal outside the package —
// and that lease has id LeaseUnset. Grant is the gatekeeper that makes the
// value unreachable rather than merely discouraged (Rule 2).
func TestJob_GrantRefusesUnidentifiedLease(t *testing.T) {
	j := newTestJob(t)
	if err := j.Grant(&Lease{}); !errors.Is(err, ErrUnidentifiedLease) {
		t.Errorf("Grant(&Lease{}) = %v, want ErrUnidentifiedLease", err)
	}
	if j.HoldsLease() {
		t.Error("HoldsLease() = true after a refused Grant")
	}
}
