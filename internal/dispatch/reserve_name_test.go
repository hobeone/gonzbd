package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

func addNamed(t *testing.T, d *Dispatcher, id, name string) error {
	t.Helper()
	return d.Add(context.Background(), job.New(id, name, job.PolicyFromPP(3)), Header{Name: name})
}

// TestReserveName_HoldsANameAgainstEveryoneButItsHolder pins that a reserved
// name is refused to another job's Add, another job's rename and another
// reservation, and that the holder's own Add is admitted.
func TestReserveName_HoldsANameAgainstEveryoneButItsHolder(t *testing.T) {
	d := newTestDispatcher(t)
	if err := addNamed(t, d, "other", "Other"); err != nil {
		t.Fatalf("Add(other): %v", err)
	}
	release, err := d.ReserveName("holder", "Held")
	if err != nil {
		t.Fatalf("ReserveName: %v", err)
	}
	defer release()

	if err := addNamed(t, d, "x", "Held"); !errors.Is(err, ErrJobNameTaken) {
		t.Errorf("Add under a reserved name = %v, want ErrJobNameTaken", err)
	}
	if err := d.SetName("other", "Held"); !errors.Is(err, ErrJobNameTaken) {
		t.Errorf("SetName to a reserved name = %v, want ErrJobNameTaken", err)
	}
	if _, err := d.ReserveName("y", "Held"); !errors.Is(err, ErrJobNameTaken) {
		t.Errorf("a second ReserveName = %v, want ErrJobNameTaken", err)
	}
	if err := addNamed(t, d, "holder", "Held"); err != nil {
		t.Errorf("the holder's own Add = %v, want nil", err)
	}
}

// TestReserveName_RefusesANameARegisteredJobHas pins that nothing is reserved
// when the name is already held.
func TestReserveName_RefusesANameARegisteredJobHas(t *testing.T) {
	d := newTestDispatcher(t)
	if err := addNamed(t, d, "a", "Same"); err != nil {
		t.Fatalf("Add(a): %v", err)
	}
	if _, err := d.ReserveName("b", "Same"); !errors.Is(err, ErrJobNameTaken) {
		t.Fatalf("ReserveName = %v, want ErrJobNameTaken", err)
	}
	if len(d.reservedNames) != 0 {
		t.Errorf("a refused ReserveName left reservations %v", d.reservedNames)
	}
}

// TestReserveName_ReleaseFreesTheNameAndOnlyItsOwn pins that release frees
// the name, is idempotent, and never frees a reservation another job took
// after it.
func TestReserveName_ReleaseFreesTheNameAndOnlyItsOwn(t *testing.T) {
	d := newTestDispatcher(t)
	release, err := d.ReserveName("a", "Name")
	if err != nil {
		t.Fatalf("ReserveName(a): %v", err)
	}
	release()
	if err := addNamed(t, d, "b", "Name"); err != nil {
		t.Fatalf("Add after release = %v, want nil", err)
	}
	// b holds the name as a registered job; a second release must not
	// disturb anything, and a new reservation by another id still refuses.
	release()
	if _, err := d.ReserveName("c", "Name"); !errors.Is(err, ErrJobNameTaken) {
		t.Errorf("ReserveName over a registered job = %v, want ErrJobNameTaken", err)
	}

	releaseA, err := d.ReserveName("a", "Other")
	if err != nil {
		t.Fatalf("ReserveName(a, Other): %v", err)
	}
	releaseA()
	releaseD, err := d.ReserveName("d", "Other")
	if err != nil {
		t.Fatalf("ReserveName(d, Other) after a released: %v", err)
	}
	defer releaseD()
	releaseA() // stale: must not free d's reservation
	if _, err := d.ReserveName("e", "Other"); !errors.Is(err, ErrJobNameTaken) {
		t.Errorf("a stale release freed another job's reservation: ReserveName = %v", err)
	}
}
