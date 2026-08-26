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
