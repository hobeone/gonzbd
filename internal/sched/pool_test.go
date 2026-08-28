package sched

import (
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestLeasePool_AuditsReturns pins the property that makes spec §6 test 4b
// possible. Cross and Finish return a *Lease that a caller can drop —
// `_, err := j.Cross(to)` compiles — and no compiler check or linter sees it.
// The enforcement therefore lives on the pool: it knows what it issued, so a
// scenario can assert nothing was lost.
func TestLeasePool_AuditsReturns(t *testing.T) {
	p := newLeasePool(2)
	a, b := p.issue(), p.issue()
	if a == nil || b == nil {
		t.Fatalf("issue() returned nil within capacity: a=%v b=%v", a, b)
	}
	if got := p.issue(); got != nil {
		t.Errorf("issue() = %v past capacity, want nil", got)
	}
	if got := p.outstanding(); got != 2 {
		t.Errorf("outstanding() = %d, want 2", got)
	}
	if err := p.reclaim(a); err != nil {
		t.Errorf("reclaim(a) = %v, want nil", err)
	}
	if err := p.reclaim(a); !errors.Is(err, errNotOutstanding) {
		t.Errorf("second reclaim(a) = %v, want errNotOutstanding — a double return would "+
			"inflate capacity and let two jobs hold one slot", err)
	}
	if err := p.reclaim(job.NewLease(9999)); !errors.Is(err, errNotOutstanding) {
		t.Errorf("reclaim of a lease this pool never issued = %v, want errNotOutstanding", err)
	}
	if err := p.reclaim(nil); err != nil {
		t.Errorf("reclaim(nil) = %v, want nil — §3.9 requires the sole reclaimer to no-op "+
			"on nil so that no call site has to test for it", err)
	}
	if got := p.outstanding(); got != 1 {
		t.Errorf("outstanding() = %d after one successful reclaim, want 1", got)
	}
}

// TestSlotPool_TracksHolds exercises slotPool end to end: acquire fills
// capacity, a second acquire for a job already holding a slot is a no-op
// success rather than a second charge, acquire past capacity fails, and
// release frees the slot for the next caller. No caller exists yet — Task 4
// only builds the pools, grantFor arrives in a later task — so this test is
// what satisfies golangci-lint's unused check honestly rather than by a
// blank reference or a nolint.
func TestSlotPool_TracksHolds(t *testing.T) {
	p := newSlotPool(1)
	if p.holds("job-a") {
		t.Fatalf("holds(job-a) = true before any acquire, want false")
	}
	if !p.acquire("job-a") {
		t.Fatalf("acquire(job-a) = false within capacity, want true")
	}
	if !p.holds("job-a") {
		t.Errorf("holds(job-a) = false after acquire, want true")
	}
	if got := p.outstanding(); got != 1 {
		t.Errorf("outstanding() = %d, want 1", got)
	}
	if !p.acquire("job-a") {
		t.Errorf("acquire(job-a) = false on an id that already holds, want true (idempotent)")
	}
	if got := p.outstanding(); got != 1 {
		t.Errorf("outstanding() = %d after re-acquire by the same id, want 1 (must not double-charge)", got)
	}
	if p.acquire("job-b") {
		t.Errorf("acquire(job-b) = true past capacity, want false")
	}
	p.release("job-a")
	if p.holds("job-a") {
		t.Errorf("holds(job-a) = true after release, want false")
	}
	if got := p.outstanding(); got != 0 {
		t.Errorf("outstanding() = %d after release, want 0", got)
	}
	if !p.acquire("job-b") {
		t.Errorf("acquire(job-b) = false after job-a released its slot, want true")
	}
}
