package sched

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/job"
)

// renderStatus renders j's status through the same seam production does:
// q.Render composes the view under one lock, and job.ToSABnzbd turns it into
// the legacy vocabulary. Scenario tests assert on this rather than on
// State/Outcome directly wherever the spec's trace names a rendered status,
// because ToSABnzbd is what a client actually sees.
//
// This was a hand-rolled copy of Render's body until Render existed. Routing
// it here is what keeps every scenario test pinning the PRODUCTION composition
// rather than a parallel one that could drift from it silently.
func renderStatus(q *Queue, j *job.Job) constants.Status {
	return job.ToSABnzbd(q.Render(j))
}

// TestBothPoolsAreAccountedAtEveryExit is spec §6 test 4b. §10's
// revision-3→4 table (docs/superpowers/specs/2026-08-26-lifecycle-intents-design.md,
// "five of the twelve were one problem: nobody returned the lease") lists
// five leak paths: pre-boundary Finish(Failed), Assessing→Finish(Unrecoverable),
// Cancel→Finish(Cancelled), Cross after an already-surrendered pause, and
// BeginAttempt orphaning the *Lease passed to it. Four of those five are rows
// in the walk below — "settle after a pre-boundary failure", "settle
// Unrecoverable from Assessing", "cancel", and "cross a job paused before
// crossing". The fifth has no row here and cannot: D-I12 removed the *Lease
// parameter from BeginAttempt, so that leak's cause no longer exists to
// exercise. This walk is checked against four of revision 3's five leaks,
// not all five at once.
//
// Two further rows — "retry after a pre-boundary settle" and "cancel a
// settled attempt" — are not from §3.9's table or revision 3's leak list at
// all. They pin the two Critical findings from this branch's final
// whole-branch review: Retry carrying a settled attempt's slot into Fetching,
// and finishCancel's settled arm never releasing the slot a settled-then-
// cancelled job still held. Both are real leaks this walk's original six rows
// never exercised — neither row settles a job and then separately retries or
// cancels it.
//
// It walks §3.9's exit table and asserts a DIFFERENT invariant per pool. That
// asymmetry is the point, and collapsing it was a defect in this plan twice,
// in opposite directions:
//
//   - Pool A is CONSERVED: occupancy returns to its starting value. §3.9 is
//     the table of where leases return, and a lease is a token that must come
//     back, so conservation is exactly the right law. Covering only this was
//     the first error — three pool-B leaks sat here with the suite green.
//   - Pool B is CORRECT, not conserved: a job holds a slot exactly while its
//     position requires one and it is neither settled nor gated. Asserting
//     conservation here was the second error — "cross into Production" ends at
//     Extracting legitimately holding a slot, and demanding zero would report
//     correct behaviour as a leak in two of these six rows: "cross into
//     Production" and "cross a job paused before crossing" (also ending at
//     Extracting). Checked by instrumenting the walk directly rather than
//     reasoning about each row: only those two log holds=true at their
//     per-row assertion; the other four settle, cancel or park, and all
//     four release pool B on the way.
//
// The lesson generalises past this test: a resource with a handback has a
// conservation law, and a resource that is merely occupied has a state-matching
// rule. They are not the same claim and one walk must not assert both.
//
// The "double-returned" half of the per-row assertion below ("a lease was
// lost or double-returned") is checkable only because leasePool.reclaim
// audits identity — `if !p.issued[l.ID()]` at pool.go:66 — rather than
// merely deleting a map key: a reclaim of an ID the pool does not currently
// have outstanding surfaces as errNotOutstanding instead of silently
// no-oping, so a double reclaim of the same lease is distinguishable from a
// single correct one. Without that audit a double `delete` on the same key
// is itself idempotent and this walk's before/after count could not tell the
// two apart. job.Cross and job.Finish return a *Lease a caller can silently
// drop, and no compiler check or linter sees that.
func TestBothPoolsAreAccountedAtEveryExit(t *testing.T) {
	exits := []struct {
		name string
		run  func(t *testing.T, q *Queue, j *job.Job)
	}{
		{"cross into Production", func(t *testing.T, q *Queue, j *job.Job) {
			mustAdvanceTo(t, q, j, job.Assessing)
			if err := j.SetNext(job.Extracting); err != nil {
				t.Fatalf("SetNext: %v", err)
			}
			if err := q.Advance(j); err != nil {
				t.Fatalf("Advance: %v", err)
			}
		}},
		{"settle after a pre-boundary failure", func(t *testing.T, q *Queue, j *job.Job) {
			mustAdvanceToSettled(t, q, j, job.OutcomeFailed)
		}},
		{"settle Unrecoverable from Assessing", func(t *testing.T, q *Queue, j *job.Job) {
			mustAdvanceTo(t, q, j, job.Assessing)
			l, err := j.Finish(job.OutcomeUnrecoverable, testClock())
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if err := q.reclaim(l); err != nil {
				t.Fatalf("reclaim: %v", err)
			}
			// The lease returns AT the settlement, because Finish hands it
			// back. The slot does not: B1 has no settlement door of its own
			// for a non-cancel outcome — the worker calls j.Finish directly —
			// so the slot is freed by the SWEEPER in Advance's settled branch,
			// on the next scheduling tick. This Advance is that tick, and it
			// is written out rather than elided because the asymmetry is a
			// real property of the design, not test scaffolding. A Queue.Finish
			// door would remove it; see "Adjacent work".
			if err := q.Advance(j); err != nil {
				t.Fatalf("Advance: %v", err)
			}
		}},
		{"cancel", func(t *testing.T, q *Queue, j *job.Job) {
			mustAdvanceTo(t, q, j, job.Assessing)
			// SetNext rather than a fixture slot release: the job must still
			// hold its slot when Cancel runs, or the pool-B half of this walk
			// is vacuous for this row. See TestCancel_ReleasesTheComputeSlot.
			if err := j.SetNext(job.Repairing); err != nil {
				t.Fatalf("SetNext: %v", err)
			}
			if err := q.Cancel(j); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
		}},
		{"pause", func(t *testing.T, q *Queue, j *job.Job) {
			mustAdvanceTo(t, q, j, job.Assessing)
			if err := j.SetNext(job.Fetching); err != nil {
				t.Fatalf("SetNext: %v", err)
			}
			if err := j.SetIntent(job.IntentPause); err != nil {
				t.Fatalf("SetIntent: %v", err)
			}
			if err := q.Advance(j); err != nil {
				t.Fatalf("Advance: %v", err)
			}
		}},
		{"cross a job paused before crossing", func(t *testing.T, q *Queue, j *job.Job) {
			mustAdvanceTo(t, q, j, job.Assessing)
			if err := j.SetNext(job.Extracting); err != nil {
				t.Fatalf("SetNext: %v", err)
			}
			if err := q.reclaim(j.Surrender()); err != nil { // the pause
				t.Fatalf("reclaim: %v", err)
			}
			if err := q.Advance(j); err != nil {
				t.Fatalf("Advance: %v", err) // Cross yields nil; reclaim must no-op
			}
		}},
		{"retry after a pre-boundary settle", func(t *testing.T, q *Queue, j *job.Job) {
			// Critical 2: Retry must release the settled attempt's slot BEFORE
			// opening the new one at Fetching, which needsSlot == false --
			// nothing on the Fetching path calls releaseFor again, so a slot
			// carried in here is never freed.
			mustAdvanceTo(t, q, j, job.Assessing)
			l, err := j.Finish(job.OutcomeFailed, testClock())
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if err := q.reclaim(l); err != nil {
				t.Fatalf("reclaim: %v", err)
			}
			if err := q.Retry(j); err != nil {
				t.Fatalf("Retry: %v", err)
			}
		}},
		{"cancel a settled attempt", func(t *testing.T, q *Queue, j *job.Job) {
			// Critical 1: finishCancel's settled arm must release the slot
			// itself -- Advance routes IntentCancel to finishCancel before its
			// own settled branch ever runs, so that branch's release is
			// permanently unreachable for a job cancelled after settling.
			mustAdvanceTo(t, q, j, job.Assessing)
			l, err := j.Finish(job.OutcomeFailed, testClock())
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if err := q.reclaim(l); err != nil {
				t.Fatalf("reclaim: %v", err)
			}
			if err := q.Cancel(j); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
		}},
	}
	// wantsSlot is what pool-B occupancy SHOULD be for j right now: a job
	// holds a compute slot exactly while its CURRENT position requires one and
	// it is neither settled nor gated. Those are the three conditions
	// releaseFor's call sites exist to maintain, stated once as a predicate so
	// the walk asserts the rule rather than memorised numbers. `grep -n
	// 'q\.releaseFor(' internal/sched/*.go | grep -v _test.go` (the `q\.`
	// prefix excludes releaseFor's own func declaration in queue.go, which a
	// bare `releaseFor(` pattern also matches) finds seven production call
	// sites: parkLocked, Retry, Advance's settled branch, Advance's release on
	// a failed Transition (the rollback for a slot grantFor already acquired
	// for a destination the job never reached), Advance's demotion,
	// finishCancel's settled-on-entry arm (cancel.go), and settleLocked
	// (settle.go) — reached both from Settle directly and from finishCancel's
	// running-then-settled path, which now delegates to it rather than
	// releasing the slot itself.
	//
	// It deliberately asks about the current position, not next: a job whose
	// work has finished still occupies the position it finished in and still
	// needs that position's slot until it actually moves.
	wantsSlot := func(q *Queue, j *job.Job) bool {
		s := j.Snapshot()
		if s.State.Outcome.IsSettled() {
			return false
		}
		if _, gated := q.gatedBy(s); gated {
			return false
		}
		return needsSlot(s.State.State)
	}

	// The count is asserted so that adding a row to §3.9's table, or removing
	// one of the two leak-regression rows below it, fails loudly rather than
	// quietly narrowing the walk. §3.9's table itself has 6 rows; the other 2
	// ("retry after a pre-boundary settle", "cancel a settled attempt") pin
	// the final review's two Critical findings, which are not exits §3.9's
	// table names — a settled attempt cancelled or retried has already left
	// the table by the row that settled it.
	if len(exits) != 8 {
		t.Fatalf("expected 6 rows from §3.9's exit table plus 2 leak-regression rows, got %d", len(exits))
	}
	for _, e := range exits {
		t.Run(e.name, func(t *testing.T) {
			q := New(1, 1, testClock, &stubWorkers{})
			j := job.New("j1", "n", job.Policy{})
			beforeA := q.leases.outstanding()
			e.run(t, q, j)
			if got := q.leases.outstanding(); got != beforeA {
				t.Errorf("pool-A occupancy %d → %d across %q; a lease was lost or double-returned",
					beforeA, got, e.name)
			}
			// Pool B gets a DIFFERENT invariant, and getting this wrong is
			// what a review of this plan caught. §3.9 is the table of where
			// LEASES return, so pool-A conservation across it is a real law:
			// the lease is a token that must come back. A slot is not a token
			// and is not conserved across these rows — it is held for exactly
			// as long as the position needs one, so "cross into Production"
			// legitimately ENDS holding one, at Extracting. Asserting pool-B
			// occupancy returns to zero fails two of these six rows — "cross
			// into Production" and "cross a job paused before crossing",
			// both legitimately ending at Extracting — and calls correct
			// behaviour a leak.
			//
			// The oracle is computed, never a per-row constant: a hand-written
			// expected count passes for the wrong reason the moment a row's
			// trace changes, which is the failure mode this whole walk exists
			// to prevent. It shares needsSlot with the code under test, and
			// that is acceptable only because TestRequirements_AreTotal pins
			// needsSlot independently against a literal table.
			if got, want := q.slots.holds(j.ID()), wantsSlot(q, j); got != want {
				t.Errorf("pool-B: holds=%v, want %v at %v across %q",
					got, want, j.Snapshot().State.State, e.name)
			}
		})
	}
}

// TestScenario_5_1_PauseMidDownloadThenResume pins §5.1: a job paused
// mid-download must surrender its lease at the dispatcher's yield handling
// (q.Park, called directly here in place of Half B2's dispatcher — see
// advance.go's Park doc comment) and must never touch Assessing on the way.
// Revision 2 jumped a partially-downloaded job straight into verification;
// asserting State stays Fetching throughout is what would catch that again.
func TestScenario_5_1_PauseMidDownloadThenResume(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Fetching)
	if !j.HoldsLease() {
		t.Fatal("fixture: job does not hold a lease at Fetching")
	}
	if got, want := renderStatus(q, j), constants.StatusDownloading; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}

	if err := j.SetIntent(job.IntentPause); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	// The downloader yields between articles; the dispatcher calls q.Park —
	// Advance's own branch 2 would decline to touch a job it still holds
	// (holds-before-gated), so this is not interchangeable with q.Advance(j).
	if err := q.Park(j); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if j.HoldsLease() {
		t.Error("job still holds its lease after the yield; §3.6 calls that a deadlock")
	}
	if got := j.Snapshot().State.State; got != job.Fetching {
		t.Errorf("State = %v, want Fetching — §5.1 never touches Assessing", got)
	}
	if got, want := renderStatus(q, j), constants.StatusPaused; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}

	if err := j.SetIntent(job.IntentRun); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if err := q.Advance(j); err != nil { // branch 2: grantFor(Fetching)
		t.Fatalf("Advance: %v", err)
	}
	if !j.HoldsLease() {
		t.Error("resume did not regrant the lease")
	}
	if got := j.Snapshot().State.State; got != job.Fetching {
		t.Errorf("State = %v, want Fetching — §5.1 never touches Assessing", got)
	}
	if got, want := renderStatus(q, j), constants.StatusDownloading; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}
}

// TestScenario_5_2_PauseAtABoundary pins §5.2: pausing once Fetching's work
// has ended (Next = Assessing, not yet transitioned) must park rather than
// let branch 3 transition, and the job must stay at Fetching until resumed.
func TestScenario_5_2_PauseAtABoundary(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Fetching)
	if err := j.SetNext(job.Assessing); err != nil { // download completes
		t.Fatalf("SetNext: %v", err)
	}

	if err := j.SetIntent(job.IntentPause); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if err := q.Advance(j); err != nil { // branch 3, gated: park
		t.Fatalf("Advance: %v", err)
	}
	if j.HoldsLease() {
		t.Error("a job gated at the boundary still holds its lease; park must release it")
	}
	if got := j.Snapshot().State.State; got != job.Fetching {
		t.Errorf("State = %v, want Fetching — a paused job must not cross into Assessing", got)
	}
	if got, want := renderStatus(q, j), constants.StatusPaused; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}

	if err := j.SetIntent(job.IntentRun); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if err := q.Advance(j); err != nil { // grantFor(Assessing); Transition clears next
		t.Fatalf("Advance: %v", err)
	}
	if got := j.Snapshot().State.State; got != job.Assessing {
		t.Errorf("State = %v, want Assessing", got)
	}
	if !q.slots.holds(j.ID()) {
		t.Error("Assessing needs a compute slot and none was granted on resume")
	}
	if got, want := renderStatus(q, j), constants.StatusVerifying; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}
}

// TestScenario_5_3_RepairLoopAndTheCrossing pins §5.3's correctness loop and
// the crossing at its end.
//
// Departs from the spec trace's prose, which reads "SetNext(Repairing); slot
// released" between Assessing and Repairing, and again "SetNext(Assessing);
// slot released" on the way back. Neither actually happens: Assessing,
// Repairing and Extracting all needsSlot (§3.4), and releaseFor only frees a
// slot for a DESTINATION that does not need one — a demotion out of the
// correctness loop entirely (Assessing → Fetching), not a move within it.
// Probed directly against this branch (mustAdvanceTo to Assessing, SetNext
// Repairing, Advance, SetNext Assessing, Advance, SetNext Extracting, Advance)
// logged `slot=true` at every one of those checkpoints, going false only when
// the crossing yields the LEASE — never the slot. The prose reads as a
// narrative simplification, not a claim this package's tests should assert;
// what is asserted below is the observed behaviour.
func TestScenario_5_3_RepairLoopAndTheCrossing(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)
	if got, want := renderStatus(q, j), constants.StatusVerifying; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}

	if err := j.SetNext(job.Repairing); err != nil { // verdict NeedsRepair
		t.Fatalf("SetNext: %v", err)
	}
	if err := q.Advance(j); err != nil { // grantFor(Repairing); Transition clears next
		t.Fatalf("Advance: %v", err)
	}
	if got := j.Snapshot().State.State; got != job.Repairing {
		t.Errorf("State = %v, want Repairing", got)
	}
	if !q.slots.holds(j.ID()) || !j.HoldsLease() {
		t.Error("Repairing needs both a lease and a slot; the loop must not have dropped either")
	}
	if got, want := renderStatus(q, j), constants.StatusRepairing; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}

	if err := j.SetNext(job.Assessing); err != nil { // repair done
		t.Fatalf("SetNext: %v", err)
	}
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if got := j.Snapshot().State.State; got != job.Assessing {
		t.Errorf("State = %v, want Assessing", got)
	}

	if err := j.SetNext(job.Extracting); err != nil { // verdict OK
		t.Fatalf("SetNext: %v", err)
	}
	if err := q.Advance(j); err != nil { // branch 3: IsCorrectness && IsProduction — the crossing
		t.Fatalf("Advance: %v", err)
	}
	if got := j.Snapshot().State.State; got != job.Extracting {
		t.Errorf("State = %v, want Extracting", got)
	}
	if j.HoldsLease() {
		t.Error("the job still holds its lease past the crossing; Cross must yield it")
	}
	if !q.slots.holds(j.ID()) {
		t.Error("Extracting needs a compute slot and none was granted at the crossing")
	}
	if got, want := renderStatus(q, j), constants.StatusExtracting; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}
}

// TestScenario_5_4_RestartBothVariants pins §5.4: a persisted job crossed into
// Production is restored into a FRESH Queue (pool state is runtime-only,
// §3.9), and which of the two variants it lands in depends only on whether
// its persisted Next names the state its worker already finished — the
// completion marker survives the crash and distinguishes the two.
func TestScenario_5_4_RestartBothVariants(t *testing.T) {
	t.Run("persisted at Extracting with no completion marker: extraction runs", func(t *testing.T) {
		build := New(1, 1, testClock, &stubWorkers{})
		j := job.New("j1", "n", job.Policy{})
		mustAdvanceTo(t, build, j, job.Assessing)
		if err := j.SetNext(job.Extracting); err != nil {
			t.Fatalf("SetNext: %v", err)
		}
		if err := build.Advance(j); err != nil { // crosses; the "process" that then dies
			t.Fatalf("Advance: %v", err)
		}
		if got := j.Snapshot().State.Next; got != job.StateUnset {
			t.Fatalf("fixture: Next = %v, want StateUnset — Cross must clear it", got)
		}

		restarted := New(1, 1, testClock, &stubWorkers{}) // fresh process, empty pools
		if err := restarted.Advance(j); err != nil {      // branch 2: grantFor(Extracting)
			t.Fatalf("Advance: %v", err)
		}
		if got := j.Snapshot().State.State; got != job.Extracting {
			t.Errorf("State = %v, want Extracting — extraction must run", got)
		}
		if !restarted.slots.holds(j.ID()) {
			t.Error("branch 2 did not grant a slot for the restored job")
		}
	})

	t.Run("persisted at Extracting with Next=Finalizing: does not re-extract", func(t *testing.T) {
		build := New(1, 1, testClock, &stubWorkers{})
		j := job.New("j1", "n", job.Policy{})
		mustAdvanceTo(t, build, j, job.Assessing)
		if err := j.SetNext(job.Extracting); err != nil {
			t.Fatalf("SetNext: %v", err)
		}
		if err := build.Advance(j); err != nil { // crosses
			t.Fatalf("Advance: %v", err)
		}
		if err := j.SetNext(job.Finalizing); err != nil { // extraction finished; "process" dies before the move
			t.Fatalf("SetNext: %v", err)
		}

		restarted := New(1, 1, testClock, &stubWorkers{})
		if err := restarted.Advance(j); err != nil { // branch 3: grantFor(Finalizing); Transition
			t.Fatalf("Advance: %v", err)
		}
		if got := j.Snapshot().State.State; got != job.Finalizing {
			t.Errorf("State = %v, want Finalizing — the completion marker must skip re-extraction", got)
		}
		if !restarted.slots.holds(j.ID()) {
			t.Error("branch 3 did not grant a slot for the restored job")
		}
	})
}

// TestScenario_5_5_CancelPostBoundary pins §5.5: cancelling a job running in
// Production gates rather than settles while work is in flight, and settles
// only once the worker's own SetNext(Finalizing) makes running() false.
//
// Departs from the spec trace's "unpacker finishes → slot released" line: the
// slot is not released at that point — Extracting and Finalizing both
// needsSlot (§3.4), and nothing in Advance's non-crossing move frees a slot
// the destination still needs. running() goes false here for the OTHER
// reason the trace itself names as independently sufficient — Next becomes
// non-unset — and probing this exact sequence (mustAdvanceTo to Assessing,
// cross to Extracting, Cancel, SetNext(Finalizing)) logged `slot=true` right
// up to the point where the settling Advance call runs; only that call, via
// the settled branch's releaseFor, actually frees it. The assertions below
// check the slot at the moment each claim is actually true rather than at
// the row where the trace states it.
func TestScenario_5_5_CancelPostBoundary(t *testing.T) {
	w := &stubWorkers{}
	q := New(1, 1, testClock, w)
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)
	if err := j.SetNext(job.Extracting); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if err := q.Advance(j); err != nil { // crosses
		t.Fatalf("Advance: %v", err)
	}
	if !q.running(j.ID(), j.Snapshot()) {
		t.Fatal("fixture: job is not running at Extracting; this test cannot observe the gate")
	}

	if err := q.Cancel(j); err != nil { // IsProduction && running → gate
		t.Fatalf("Cancel: %v", err)
	}
	if j.Snapshot().State.Outcome.IsSettled() {
		t.Error("Cancel settled a job still running in Production; D-I11 lets it complete")
	}
	if len(w.aborted) != 0 {
		t.Errorf("aborted = %v, want none — Production has no interrupt arm", w.aborted)
	}

	if err := j.SetNext(job.Finalizing); err != nil { // unpacker finishes
		t.Fatalf("SetNext: %v", err)
	}
	if q.running(j.ID(), j.Snapshot()) {
		t.Fatal("running() is still true after SetNext; the gate would keep gating instead of settling")
	}
	if err := q.Advance(j); err != nil { // finishCancel: not running → Finish(Cancelled)
		t.Fatalf("Advance: %v", err)
	}
	if got := j.Snapshot().State.Outcome; got != job.OutcomeCancelled {
		t.Errorf("Outcome = %v, want Cancelled", got)
	}
	if q.slots.holds(j.ID()) {
		t.Error("settling on cancel left the compute slot held")
	}
	if got, want := renderStatus(q, j), constants.StatusDeleted; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}
}

// TestScenario_5_6_ContentionAtABoundary pins §5.6: when branch 3's grantFor
// fails for want of a compute slot, the move must not happen and the lease
// already held must not be given up — a job denied a slot is simply not
// running yet, not parked.
func TestScenario_5_6_ContentionAtABoundary(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Fetching)
	if err := j.SetNext(job.Assessing); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	q.slots.capacity = 0 // pool B full

	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if got := j.Snapshot().State.State; got != job.Fetching {
		t.Errorf("State = %v, want Fetching — grantFor's failure must not move the job", got)
	}
	if !j.HoldsLease() {
		t.Error("the lease was given up on contention; §8.1 says it must be retained")
	}
	if got, want := renderStatus(q, j), constants.StatusQueued; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}
}

// TestScenario_5_7_PauseDuringFinalizing pins §5.7: there is no gate after
// Finalizing starts, so a pause requested mid-Finalizing changes nothing —
// the state runs to completion and settles OK, never parking.
func TestScenario_5_7_PauseDuringFinalizing(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)
	if err := j.SetNext(job.Extracting); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if err := q.Advance(j); err != nil { // crosses
		t.Fatalf("Advance: %v", err)
	}
	if err := j.SetNext(job.Finalizing); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if err := q.Advance(j); err != nil { // branch 3 move to Finalizing
		t.Fatalf("Advance: %v", err)
	}
	if got, want := renderStatus(q, j), constants.StatusMoving; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}

	if err := j.SetIntent(job.IntentPause); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if err := q.Advance(j); err != nil { // holds-before-gated: still running, untouched
		t.Fatalf("Advance: %v", err)
	}
	if got := j.Snapshot().State.State; got != job.Finalizing {
		t.Errorf("State = %v, want Finalizing — there is no gate here to park at", got)
	}
	if !q.slots.holds(j.ID()) {
		t.Error("a mid-Finalizing pause released the slot; §5.7 says the state runs to completion")
	}

	l, err := j.Finish(job.OutcomeOK, testClock())
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := q.reclaim(l); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if err := q.Advance(j); err != nil { // settled branch releases the slot
		t.Fatalf("Advance: %v", err)
	}
	if got := j.Snapshot().State.Outcome; got != job.OutcomeOK {
		t.Errorf("Outcome = %v, want OK — pausing mid-Finalizing must not turn into a failure", got)
	}
	if q.slots.holds(j.ID()) {
		t.Error("still holds a compute slot after settling; the settled branch must release it")
	}
	if got, want := renderStatus(q, j), constants.StatusCompleted; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}
}

// TestScenario_5_8_PreBoundaryFailureReturnsTheLease pins §5.8: Finish must
// yield the lease on a pre-boundary failure, and the settled branch must then
// release the slot Assessing was holding. Revision 3 leaked the lease here on
// every failed download.
func TestScenario_5_8_PreBoundaryFailureReturnsTheLease(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)

	l, err := j.Finish(job.OutcomeUnrecoverable, testClock())
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := q.reclaim(l); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if j.HoldsLease() {
		t.Error("the job still holds its lease after settling; Finish must yield it")
	}
	if q.leases.outstanding() != 0 {
		t.Errorf("leases outstanding = %d, want 0 — revision 3 leaked exactly this", q.leases.outstanding())
	}

	if err := q.Advance(j); err != nil { // settled branch: release the slot too
		t.Fatalf("Advance: %v", err)
	}
	if q.slots.holds(j.ID()) {
		t.Error("still holds a compute slot after settling")
	}
	if got, want := renderStatus(q, j), constants.StatusFailed; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}
}

// TestScenario_5_9_RetryWhenPoolAIsExhausted pins §5.9: Retry must succeed
// even when another job holds pool A's only lease, and the retried job simply
// waits — running once capacity frees on a later tick. Revision 3 dropped
// this retry permanently by demanding a lease BeginAttempt could not get.
func TestScenario_5_9_RetryWhenPoolAIsExhausted(t *testing.T) {
	q := New(1, 0, testClock, &stubWorkers{})
	other := job.New("other", "n", job.Policy{})
	if err := q.Advance(other); err != nil { // opens other's attempt
		t.Fatalf("Advance(other): %v", err)
	}
	if err := q.Advance(other); err != nil { // grants other the only lease
		t.Fatalf("Advance(other): %v", err)
	}
	if !other.HoldsLease() {
		t.Fatal("fixture: other does not hold pool A's only lease")
	}

	j := job.New("j1", "n", job.Policy{})
	mustAdvanceToSettled(t, q, j, job.OutcomeFailed)
	if got, want := renderStatus(q, j), constants.StatusFailed; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}

	if err := q.Retry(j); err != nil { // BeginAttempt — takes no lease (D-I12)
		t.Fatalf("Retry: %v", err)
	}
	if got := j.Snapshot().State.State; got != job.Fetching {
		t.Errorf("State = %v, want Fetching — Retry must open a new attempt", got)
	}
	if j.HoldsLease() {
		t.Error("Retry granted a lease directly; it must take none")
	}

	if err := q.Advance(j); err != nil { // branch 2: pool A still exhausted
		t.Fatalf("Advance: %v", err)
	}
	if j.HoldsLease() {
		t.Error("Advance granted a lease while pool A was exhausted by another job")
	}
	if got, want := renderStatus(q, j), constants.StatusQueued; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}

	l, err := other.Finish(job.OutcomeFailed, testClock()) // capacity frees
	if err != nil {
		t.Fatalf("Finish(other): %v", err)
	}
	if err := q.reclaim(l); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if err := q.Advance(j); err != nil { // branch 2: grantFor(Fetching)
		t.Fatalf("Advance: %v", err)
	}
	if !j.HoldsLease() {
		t.Error("j did not acquire the lease once pool A freed")
	}
	if got, want := renderStatus(q, j), constants.StatusDownloading; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}
}

// TestScenario_5_10_PausedThenFailedThenRetried pins §5.10: a settled attempt
// must accept SetIntent(IntentRun) so a paused-then-failed job can be
// unpaused and retried. Revision 3 refused SetIntent on a settled attempt, so
// such a job could be neither unpaused nor usefully retried.
func TestScenario_5_10_PausedThenFailedThenRetried(t *testing.T) {
	q := New(1, 0, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Fetching)

	if err := j.SetIntent(job.IntentPause); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if err := q.Park(j); err != nil { // the dispatcher's yield handling, as in §5.1
		t.Fatalf("Park: %v", err)
	}
	if got, want := renderStatus(q, j), constants.StatusPaused; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}

	// A prior attempt settles Failed while the job is still marked paused —
	// the worker settles independently of intent, which is a Queue concept.
	l, err := j.Finish(job.OutcomeFailed, testClock())
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := q.reclaim(l); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if got, want := renderStatus(q, j), constants.StatusFailed; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}

	if err := j.SetIntent(job.IntentRun); err != nil { // legal on a settled attempt (§3.1)
		t.Fatalf("SetIntent on a settled attempt: %v", err)
	}
	if err := q.Retry(j); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if err := q.Advance(j); err != nil { // branch 2 grants
		t.Fatalf("Advance: %v", err)
	}
	if !j.HoldsLease() {
		t.Error("the retried job did not acquire a lease")
	}
	if got, want := renderStatus(q, j), constants.StatusDownloading; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}
}

// TestScenario_5_11_CancellingANeverRunJob pins §5.11: a job with no attempt
// has nothing to carry OutcomeCancelled, so cancelling it must be a no-op on
// the job's own state (discard is Half B2's, alongside the store — see
// finishCancel's own comment) rather than the ErrNoOpenAttempt revision 3 got
// from calling Finish on it.
func TestScenario_5_11_CancellingANeverRunJob(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})

	if err := q.Cancel(j); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if j.HasRun() {
		t.Error("Cancel started an attempt on a never-run job")
	}
	if j.Snapshot().State.Outcome.IsSettled() {
		t.Error("Cancel settled a job with no attempt to carry the verdict")
	}
	if got, want := renderStatus(q, j), constants.StatusQueued; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}
}

// TestScenario_5_12_CancellingAPostBoundaryJobRestoredFromARestart pins
// §5.12: a job restored from a restart at a Production state holds nothing
// (pool state is runtime-only), so running() is false and Cancel must settle
// it at once rather than gate. Revision 3 gated on !workDone and deadlocked,
// because advance handles cancel before granting, so the extraction that
// would have set next never ran.
func TestScenario_5_12_CancellingAPostBoundaryJobRestoredFromARestart(t *testing.T) {
	build := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, build, j, job.Assessing)
	if err := j.SetNext(job.Extracting); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if err := build.Advance(j); err != nil { // crosses; the "process" then dies
		t.Fatalf("Advance: %v", err)
	}

	restarted := New(1, 1, testClock, &stubWorkers{}) // fresh process, empty pools
	if restarted.running(j.ID(), j.Snapshot()) {
		t.Fatal("fixture: the restored job appears to be running; it should hold nothing")
	}

	if err := restarted.Cancel(j); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got := j.Snapshot().State.Outcome; got != job.OutcomeCancelled {
		t.Errorf("Outcome = %v, want Cancelled — nothing was in flight to gate on", got)
	}
	if got, want := renderStatus(restarted, j), constants.StatusDeleted; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}
}

// TestScenario_5_13_CancellingARunningFinalizingJob pins §5.13: a cancel that
// arrives while Finalizing is running gates, and since there is no gate after
// Finalizing, the job completes as OutcomeOK rather than Cancelled — with
// IntentCancel still latched, which is what a consumer reads to say the
// request came too late (D-I11).
func TestScenario_5_13_CancellingARunningFinalizingJob(t *testing.T) {
	w := &stubWorkers{}
	q := New(1, 1, testClock, w)
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)
	if err := j.SetNext(job.Extracting); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if err := q.Advance(j); err != nil { // crosses
		t.Fatalf("Advance: %v", err)
	}
	if err := j.SetNext(job.Finalizing); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if err := q.Advance(j); err != nil { // branch 3 move to Finalizing
		t.Fatalf("Advance: %v", err)
	}
	if !q.running(j.ID(), j.Snapshot()) {
		t.Fatal("fixture: job is not running at Finalizing; this test cannot observe the gate")
	}

	if err := q.Cancel(j); err != nil { // IsProduction && running → gate
		t.Fatalf("Cancel: %v", err)
	}
	if j.Snapshot().State.Outcome.IsSettled() {
		t.Error("Cancel settled a job still running in Finalizing; §8.4 has no gate past it")
	}
	if len(w.aborted) != 0 {
		t.Errorf("aborted = %v, want none — Production has no interrupt arm", w.aborted)
	}

	// The move and the user script complete; the worker settles through the
	// door a dispatcher uses, NOT j.Finish directly. Routed here deliberately:
	// settleLocked applies the cancel latch, and this scenario is the one that
	// pins the latch NOT firing (D-I11, Finalizing is post-boundary and so
	// gated rather than interrupted). Asserting against j.Finish would leave
	// the production path unpinned.
	if err := q.Settle(j, job.OutcomeOK); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := j.Snapshot().State.Outcome; got != job.OutcomeOK {
		t.Errorf("Outcome = %v, want OK — recording Cancelled here would be false: the files moved", got)
	}
	if got := j.Intent(); got != job.IntentCancel {
		t.Errorf("Intent = %v, want IntentCancel — this is what tells a consumer the request came too late", got)
	}
	if got, want := renderStatus(q, j), constants.StatusCompleted; got != want {
		t.Errorf("status = %v, want %v", got, want)
	}

	// §5.13's trace has a trailing advance after the settle: IntentCancel is
	// still latched (asserted above), so this call routes through
	// finishCancel's settled arm rather than Advance's own — Advance sends an
	// IntentCancel job to finishCancel before it ever reaches its own settled
	// branch (advance.go). Without this step the test never exercised the
	// release Finalizing's slot needs: q.slots.holds("j1") stayed true at test
	// end.
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance (trailing, settled+cancelled): %v", err)
	}
	if q.slots.holds(j.ID()) {
		t.Error("still holds a compute slot after the trailing advance; finishCancel's settled arm must release it")
	}
}

// specScenariosPath is docs/superpowers/specs/2026-08-26-lifecycle-intents-design.md,
// relative to this package's directory — `go test` runs with the package
// directory as its working directory, so this is stable across invocation
// styles (`go test ./...` from the repo root, `go test .` from here, an IDE
// runner) without needing runtime.Caller to locate it.
const specScenariosPath = "../../docs/superpowers/specs/2026-08-26-lifecycle-intents-design.md"

// TestEveryScenarioHasATest fails when §5 grows a scenario nobody pinned.
// §5 is the regression suite for four revisions of defects; a scenario without
// a test is a defect class with nothing watching it.
//
// The comparison is against the spec's own §5 headings, parsed from the file
// (specScenarioCount below) — not a hardcoded literal. A hardcoded count
// checks this file's own test count against itself and proves nothing about
// §5 ever having grown: a fourteenth scenario added to the spec with no test
// written for it left the old version of this check green, because both
// sides of the comparison came from this file. Parsing the spec is what makes
// this a real gate on the POPULATION §5 defines, matching Standing Design
// Rule 4's enumerate-from-source requirement, and the population is
// enumerable by a machine (a heading pattern), which the same rule prefers
// over a comment stating a count by hand.
func TestEveryScenarioHasATest(t *testing.T) {
	scenariosInSpec := specScenarioCount(t)
	names := scenarioTestNames(t) // parses this file for TestScenario_ funcs
	// Both halves of this check matter separately: a parse that silently read
	// zero declarations would otherwise pass the count check below by
	// accident if scenariosInSpec were ever mistyped as 0, and would in any
	// case prove nothing about the file having been read at all.
	if len(names) == 0 {
		t.Fatal("scenarioTestNames found zero TestScenario_ functions; the enumeration did not run " +
			"against real content — check the parse succeeded and this file still declares them")
	}
	if len(names) != scenariosInSpec {
		t.Errorf("§5 has %d scenarios, this file has %d tests: %v", scenariosInSpec, len(names), names)
	}
}

// specHeadingRE matches a §5 scenario heading, e.g. "### 5.13 Cancelling a
// running `Finalizing` job". Anchored on "### 5." rather than a bare "###" so
// a level-3 heading elsewhere in the document (§6's subsections, say) cannot
// be miscounted as a scenario.
var specHeadingRE = regexp.MustCompile(`^### 5\.\d+\b`)

// specScenarioCount reads specScenariosPath and counts §5's own scenario
// headings — the population TestEveryScenarioHasATest checks this file's
// tests against. A read failure or a zero count is fatal rather than silently
// falling back to a hardcoded number: a scan that reads nothing and reports
// "0 scenarios, add none" is exactly the failure this whole suite exists to
// avoid, and a hand-maintained fallback would reintroduce the same drift risk
// the parse was written to remove.
func specScenarioCount(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(specScenariosPath)
	if err != nil {
		t.Fatalf("read %s: %v — TestEveryScenarioHasATest cannot check §5's scenario count without it", specScenariosPath, err)
	}
	n := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		if specHeadingRE.MatchString(line) {
			n++
		}
	}
	if n == 0 {
		t.Fatalf("found zero §5 scenario headings in %s; the parse did not run against real content — "+
			"check the path and the heading pattern (%q)", specScenariosPath, specHeadingRE.String())
	}
	return n
}

// scenarioTestNames returns every TestScenario_ function declared in this
// file. It parses the source for the same reason constantsOfType does in
// internal/job/constants_source_test.go: a population a machine can enumerate
// must not be a sentence, because a sentence fails silently.
func scenarioTestNames(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "scenario_test.go", nil, 0)
	if err != nil {
		t.Fatalf("parse scenario_test.go: %v", err)
	}
	var names []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "TestScenario_") {
			names = append(names, fn.Name.Name)
		}
	}
	sort.Strings(names)
	return names
}
