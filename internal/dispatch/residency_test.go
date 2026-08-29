package dispatch

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

func TestResidency_HydratesWhenAJobAcquiresResources(t *testing.T) {
	res := &fakeResidency{}
	d := newTestDispatcher(t, withResidency(res))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	d.tick(context.Background())
	d.tick(context.Background())

	if !res.resident("j1") {
		t.Fatal("job holds resources but was never hydrated — residency is derived from pool membership (D-B8)")
	}
}

func TestResidency_EvictsWhenAJobReleasesResources(t *testing.T) {
	res := &fakeResidency{}
	d := newTestDispatcher(t, withResidency(res))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background())
	if !res.resident("j1") {
		t.Fatal("setup: job was never hydrated")
	}

	if err := d.q.Settle(j, job.OutcomeFailed); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	d.tick(context.Background())

	if res.resident("j1") {
		t.Error("job holds nothing but is still resident — a settled job's manifest must be evicted or a long queue accumulates them")
	}
}

// TestReconcileResidency_CalledDirectlyHydrates exercises reconcileResidency
// on its own, mirroring the two Advance calls tick makes before it, so the
// helper has a direct test in addition to the tick-driven behavioural tests
// above.
func TestReconcileResidency_CalledDirectlyHydrates(t *testing.T) {
	res := &fakeResidency{}
	d := newTestDispatcher(t, withResidency(res))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := d.q.Advance(j); err != nil { // begins the attempt
		t.Fatalf("Advance (begin): %v", err)
	}
	if err := d.q.Advance(j); err != nil { // grants the lease
		t.Fatalf("Advance (grant): %v", err)
	}

	if err := d.reconcileResidency(context.Background(), j); err != nil {
		t.Fatalf("reconcileResidency: %v", err)
	}

	if !res.resident("j1") {
		t.Fatal("reconcileResidency called directly did not hydrate a job that holds its lease")
	}
}

// TestResidentBookkeeping_MarksAndClears pins d.resident's own map operations
// directly, each guarded by a single d.mu acquisition (D-B9): isResident,
// markResident and markNotResident.
func TestResidentBookkeeping_MarksAndClears(t *testing.T) {
	d := newTestDispatcher(t)

	if d.isResident("j1") {
		t.Fatal("isResident(j1) = true before anything marked it")
	}

	d.markResident("j1")
	if !d.isResident("j1") {
		t.Fatal("isResident(j1) = false after markResident")
	}

	d.markNotResident("j1")
	if d.isResident("j1") {
		t.Fatal("isResident(j1) = true after markNotResident")
	}
}

func TestResidency_HydrationFailureSettlesFailedAndReturnsBothPools(t *testing.T) {
	res := &fakeResidency{failOn: map[string]error{"j1": errors.New("manifest unreadable")}}
	d := newTestDispatcher(t, withResidency(res))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	d.tick(context.Background())
	d.tick(context.Background())

	v := d.q.Render(j)
	if v.Outcome != job.OutcomeFailed {
		t.Errorf("Outcome = %v, want Failed — a job whose manifest cannot be read cannot run and must not hold resources forever", v.Outcome)
	}
	if j.HoldsLease() {
		t.Error("lease still held after a hydration failure — settling must return both pools")
	}
}

// TestResidency_HydrationCancelledDoesNotSettleTheJob is the other half of the
// test above. Both drive the same branch of reconcileResidency; they differ
// only in what Hydrate returns, and that difference must change the outcome.
//
// Settling on a cancelled context is permanent damage: run returns on
// ctx.Done() but a tick already in flight walks on with the same cancelled
// ctx, so a healthy job's Hydrate reports context.Canceled — and Outcome is
// write-once, so the user restarts to find jobs marked Failed that were only
// interrupted. DeadlineExceeded is included because a ctx with a deadline
// reports that instead, and it means the same thing here.
func TestResidency_HydrationCancelledDoesNotSettleTheJob(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"Canceled", context.Canceled},
		{"DeadlineExceeded", context.DeadlineExceeded},
		{"wrapped", fmt.Errorf("read manifest: %w", context.Canceled)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := &fakeResidency{failOn: map[string]error{"j1": tc.err}}
			d := newTestDispatcher(t, withResidency(res))
			j := job.New("j1", "n", job.Policy{})
			if err := d.Add(j, Header{}); err != nil {
				t.Fatalf("Add: %v", err)
			}

			d.tick(context.Background())
			d.tick(context.Background())

			v := d.q.Render(j)
			if v.Outcome != job.OutcomePending {
				t.Errorf("Outcome = %v, want Pending — a cancelled context is a fact about the process, not about the job, and Outcome is write-once", v.Outcome)
			}
			if !j.HoldsLease() {
				t.Error("lease released — a job whose hydration was cancelled keeps its resources; Stop's sweep parks them")
			}
			if d.isResident("j1") {
				t.Error("isResident(j1) = true after a failed Hydrate")
			}
		})
	}
}
