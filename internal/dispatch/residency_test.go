package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	if err := d.Add(job.New("j1", "n", job.Policy{}), Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

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

// TestResidency_HydrationFailureIsPersisted pins the store side of the
// settle above. reconcileResidency settles the job Failed and then returns an
// error, and tick's error branch continues to the next job — so the write
// that would record the settle is the one step the failure path skips.
//
// A later tick does reach persistIfChanged for this job, which is why the gap
// is invisible to every test that keeps ticking. Stop is what makes it
// permanent: it ends the loop, and the row the store kept still says Pending
// for a job that can never run. The next Start restores it as pending work.
func TestResidency_HydrationFailureIsPersisted(t *testing.T) {
	st := &fakeStore{}
	res := &fakeResidency{failOn: map[string]error{"j1": errors.New("manifest is corrupt")}}
	d := newTestDispatcher(t, withResidency(res), withStore(st))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	d.tick(context.Background()) // begins the attempt; persists Pending
	d.tick(context.Background()) // grants the lease, Hydrate fails, settles Failed

	if got := d.q.Render(j).Outcome; got != job.OutcomeFailed {
		t.Fatalf("setup: in-memory Outcome = %v, want Failed", got)
	}
	p, ok := st.row("j1")
	if !ok {
		t.Fatal("store holds no row for j1")
	}
	if p.State.Outcome != job.OutcomeFailed {
		t.Errorf("persisted Outcome = %v, want Failed — the settle that the "+
			"hydration failure performed must reach the store before tick "+
			"moves on, or a Stop before the next tick leaves the row saying "+
			"Pending for a job that already failed", p.State.Outcome)
	}
}

// TestResidency_DoesNotHydrateANeverStartedJob is the dispatch-side half of
// the same defect. A paused job never leaves StateUnset — Advance's branch 1
// returns before BeginAttempt when gatedBy reports the pause — so it holds no
// lease and no slot, and its manifest has no business being in memory.
//
// Two things went wrong when Holds was vacuously true for it. The manifest
// was hydrated for a job holding nothing, which is the memory bound
// docs/queue-lifecycle.md sets; and if that read failed, the settle attempted
// on the way out hit job.ErrNoOpenAttempt, because Outcome lives on the
// Attempt and a never-started job has none.
func TestResidency_DoesNotHydrateANeverStartedJob(t *testing.T) {
	res := &fakeResidency{}
	d := newTestDispatcher(t, withResidency(res))
	d.Pause()
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	d.tick(context.Background())

	if got := d.q.Render(j).State; got != job.StateUnset {
		t.Fatalf("setup: State = %v, want StateUnset — a paused job must not start", got)
	}
	if res.resident("j1") {
		t.Error("a paused, never-started job was hydrated — it holds no lease " +
			"and no slot, so nothing about it requires its manifest in memory")
	}
	if d.isResident("j1") {
		t.Error("isResident(j1) = true for a paused, never-started job")
	}
}

// TestReconcileResidency_NeverStartedJobDoesNotAttemptASettle pins the second
// consequence separately: settling a job with no open attempt cannot work, so
// reaching that call at all is the bug. Before the fix this returned an error
// wrapping job.ErrNoOpenAttempt.
func TestReconcileResidency_NeverStartedJobDoesNotAttemptASettle(t *testing.T) {
	res := &fakeResidency{failOn: map[string]error{"j1": errors.New("boom")}}
	d := newTestDispatcher(t, withResidency(res))
	d.Pause()
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := d.reconcileResidency(context.Background(), j)

	if errors.Is(err, job.ErrNoOpenAttempt) {
		t.Errorf("reconcileResidency = %v — it tried to settle a job with no "+
			"open attempt, which means it hydrated one that holds nothing", err)
	}
	if err != nil {
		t.Errorf("reconcileResidency = %v, want nil — a never-started job needs "+
			"no residency work at all", err)
	}
}

// TestResidency_CancelledContextDoesNotSettleOnANonContextError closes the
// gap the sentinel check alone leaves.
//
// The branch distinguishing "the job is bad" from "the process is going away"
// tested only errors.Is(err, context.Canceled/DeadlineExceeded). That
// identifies a cancellation only when the error CARRIES the sentinel, and the
// I/O a real Hydrate does mostly does not: os.Open returns *os.PathError,
// gzip and json return io.ErrUnexpectedEOF, and a reader interrupted mid-file
// reports a short read. None of those wrap a context sentinel.
//
// Settling on one of them is permanent damage, because Outcome is write-once:
// the user restarts to find a healthy job marked Failed because the process
// happened to be shutting down while its manifest was being read. When ctx is
// already cancelled, no error identity should be able to produce a settle.
func TestResidency_CancelledContextDoesNotSettleOnANonContextError(t *testing.T) {
	res := &fakeResidency{failOn: map[string]error{"j1": errors.New("read manifest: unexpected EOF")}}
	d := newTestDispatcher(t, withResidency(res))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background()) // open the attempt
	d.tick(context.Background()) // grant the lease, hydrate (and fail)
	if got := d.q.Render(j).Outcome; got != job.OutcomeFailed {
		t.Fatalf("setup: Outcome = %v, want Failed — with a LIVE ctx this error must settle", got)
	}

	// Same error, same branch, but the process is going away.
	res2 := &fakeResidency{failOn: map[string]error{"j2": errors.New("read manifest: unexpected EOF")}}
	d2 := newTestDispatcher(t, withResidency(res2))
	j2 := job.New("j2", "n", job.Policy{})
	if err := d2.Add(j2, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d2.tick(context.Background()) // opens the attempt at Fetching; no lease yet
	// Grant the lease WITHOUT going through tick, so the hydration below is
	// the first one attempted and happens under the cancelled ctx. A second
	// tick would hydrate under a live ctx and settle before we got here.
	if err := d2.q.Advance(j2); err != nil {
		t.Fatalf("setup: Advance: %v", err)
	}
	if !d2.q.Render(j2).Holds {
		t.Fatal("setup: job does not hold its lease, so reconcileResidency will not hydrate")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = d2.reconcileResidency(ctx, j2)

	if got := d2.q.Render(j2).Outcome; got != job.OutcomePending {
		t.Errorf("Outcome = %v, want Pending — ctx was already cancelled, so this "+
			"is a fact about the process, not the job, and Outcome is write-once", got)
	}
	if !j2.HoldsLease() {
		t.Error("lease released — a job interrupted by shutdown keeps its " +
			"resources; Stop's sweep parks them")
	}
}

// TestResidency_HydrationFailureSettleError pins reconcileResidency error reporting.
func TestResidency_HydrationFailureSettleError(t *testing.T) {
	res := &fakeResidency{failOn: map[string]error{"j1": errors.New("corrupt")}}
	d := newTestDispatcher(t, withResidency(res))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background()) // opens the attempt at Fetching; no lease yet
	if err := d.q.Advance(j); err != nil {
		t.Fatalf("setup: Advance: %v", err)
	}
	if !d.q.Render(j).Holds {
		t.Fatal("setup: job does not hold lease")
	}

	err := d.reconcileResidency(context.Background(), j)
	if err == nil {
		t.Fatal("expected error on corrupt manifest")
	}
	if strings.Contains(err.Error(), "and settle failed") {
		t.Errorf("error = %q; should not report settle failure when settle succeeded", err)
	}
}
