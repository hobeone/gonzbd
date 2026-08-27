# Lifecycle Intents, Half B — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give every cross-axis lifecycle rule a single computed owner, then build the scheduling decisions (`gatedBy`, `waitReason`, `grantFor`, `advance`, `Cancel`) on top of them as pure functions over one atomic snapshot.

**Architecture:** Four foundation tasks harden `internal/job` where two review rounds proved it soft — an admissibility table owning `Outcome × State`, a `Lease` with real identity, and one atomic composite read. A new `internal/sched` package then holds the Queue's decision core, depending on `internal/job` and nothing else. The existing 27k-line `internal/queue` is untouched; wiring and deletion are Half B2.

**Tech Stack:** Go 1.27.0, stdlib only. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-08-26-lifecycle-intents-design.md` — §3.4 (running-ness and grantability), §3.6 (`advance`), §3.7 (cancel), §3.9 (lease releaser/reclaimer), §5 (scenarios), §6 (testing), §8 (open questions).

**Predecessor:** `docs/superpowers/plans/2026-08-26-lifecycle-intents-half-a.md`, landed as PR #443.

---

## Global Constraints

- **Go 1.27.0**, toolchain 1.27.0. Generic methods and promoted fields in composite literals are legal at this `go` directive; both are gated on it, and a single-file `go build /tmp/x.go` probe reports the pre-1.27 rule.
- **Standing Design Rule 1 — no backwards compatibility.** Nothing persists a `State`, an `Outcome` or a `Lease` yet. No migration, no dual-read, no drain period. Before writing a guard, name the state that makes it necessary.
- **Standing Design Rule 2 — state has one owner.** Every derived value has one function that computes it and one path that mutates it. **Escalate before adding a second constructor, a second writer of a derived field, or a second enforcement point for one invariant.**
- **Standing Design Rule 3 — a bad article costs only its own bytes.** Not directly engaged by this plan; do not let it justify weakening a check that guards a protocol, path, query or command.
- **Standing Design Rule 4 — enumerate before asserting.** Any comment saying *only / sole / never / always / the one place* must state the enumeration you actually ran, from source, at the moment you write it. Prefer a test over a sentence wherever the population is machine-enumerable.
- **Red-green is mechanical, not mental.** Every pin is verified by reverting the fix and *observing* the failure, with `-count=1`. A cached `ok` is not an observation. **A build failure is not an observation either** — if a mutation stops the package compiling, re-run it with the symbol declared but unwired.
- **Never `git stash`.** The stash stack is shared with other sessions in this repo.
- **`ci.yml` is manual-dispatch only.** Local gates are the gate of record. Do not wait for CI or run `/watch-ci`.
- **Quality gates before every commit:** `goimports -w`, `go build ./...`, `go vet ./...`, `go test -race -count=1 ./...`, `golangci-lint run ./...`, plus `go run ./scripts/check_dup_comments`, `check_review_banner`, `check_coverage`, `check_test_alignment`, `check_lock_io`.
- **Do not run `go fix ./...` repo-wide.**
- **Commits:** Conventional Commits 1.0.0, scope = Go package name. Footer `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.

---

## Why this plan is ordered this way

This ordering is not the spec's. It is a response to evidence, and a reviewer should judge it against that evidence first.

Twenty-two findings arrived across two review rounds on Half A. Classified by what was actually wrong:

| Class | Round 1 | Round 2 | Total |
|---|---|---|---|
| Comment / citation claims | 6 | 1 (13 sites) | 7 |
| Test gaps and vacuity | 4 | 1 | 5 |
| Error contract / message quality | 3 | 4 | 7 |
| **Cross-axis logic** | **1** | **1** | **2** |

Only two were logic defects, and **they are the same cell**. Round 1: *"`finish` permits `OutcomeOK` before crossing the boundary."* Fixed in `e5ef38d4`. Round 2, twelve hours later: *"Require `Finalizing` before settling with `OutcomeOK`."* Same guard, still wrong.

The fix commit contains its own indictment:

```go
// OutcomeOK means "the job produced its files" (outcome.go), which cannot
// be true before the boundary — production happens in Extracting and
// Finalizing. [...] Guarding here rather than in BeginAttempt
// keeps one owner for the invariant: the door that assigns the verdict is
// the one that decides which verdicts are assignable.
if o == OutcomeOK && !a.crossed {
```

The comment **enumerated both Production states** while the guard checked the *zone*. It **invoked Rule 2 by name** and declared the invariant owned. And the same commit added `{"Extracting", OutcomeOK, …}` to `TestAttempt_FinishSucceedsFromAnyOpenState` — a test asserting the residual bug was intended, which is why a mutation could never have caught it.

**The diagnosis: Rule 2 was applied to fields and never to constraints.** Where Half A gave a rule a computed owner, two rounds found nothing — `legalEdges` owns which state changes exist and who may take them; `withOpenAttempt` owns the open-attempt precondition, and dropping `!a.isOpen()` from it now fails all five doors at once. Where rules relate *one axis to another*, they are ad-hoc `if`s written from memory:

| Cross-axis rule | Lives in | Status entering Half B |
|---|---|---|
| `OutcomeOK` × position | `attempt.go` `finish` | wrong twice, now correct |
| `OutcomeUnrecoverable` × zone | `attempt.go` `finish` | correct |
| new attempt × prior position | `job.go` `BeginAttempt` | correct |
| settled × position, for render | `sabnzbd.go` `ToSABnzbd` | correct |
| **lease × zone** | — | **missing** (spec §3.4, tested by §6.6) |
| **`IntentCancel` × `OutcomeOK`** | — | **missing** (D-I11) |

Four implemented, one wrong through two reviews; two missing. **The Queue is what adds the two missing ones** — `Grant` after crossing is admission, cancel-during-`Finalizing` is scheduling. Building it before the rules have an owner repeats the failure at a layer with concurrency in it.

Hence: Tasks 1–4 give the cross-axis rules, lease identity and composite reads their owners. Tasks 5–10 build the Queue on them.

**"One owner" means one computed rule, not one location.** A single `if` in the right function satisfies "one place" perfectly and is still folklore. The test is whether the population is enumerable and whether a test fails when it changes.

---

## What the RFC review round changed

Issue #445 drew a review that found six defects, all six real. They share one
shape, and it is the shape this plan was written to prevent — which is the
useful part, because it means the diagnosis was right and its application was
incomplete.

**Pool A had an owner; pool B had none.** Task 4 gave the lease `reclaim`, an
identity audit and a conservation walk. The compute slot got two acquisition
sites and, before this round, exactly **one** production release —
`grep -n 'slots\.release'` inside `finishCancel`. Three leaks followed directly:

| Leak | Path | Cost |
|---|---|---|
| Demotion | `Assessing → Fetching` — `grantFor` acquires for the destination, nothing frees the origin's slot | pool-B slot held for an entire download |
| Park | `park` surrendered the lease only | paused job holds a slot indefinitely |
| Settlement | `Advance` returns early on a settled attempt | slot never freed after any completion |

A fourth was ordering: `finishCancel` released the slot *after* `reclaim`, so an
audit failure returned through the error path with the slot still held —
turning one recoverable error into a permanent leak.

**The fix is an owner, not four call-site patches.** `releaseFor(j, s)` is
`grantFor`'s dual — it frees what `s` does not require, and `job.StateUnset`
requires nothing — and it sits beside `reclaim` in `queue.go` so pool A and
pool B have their single return paths in one place. Rule 2 says take the owner
over the check, and this is the case the rule was written for: four checks, one
of which was forgotten in three places.

**Why every test stayed green.** `TestBothPoolsAreAccountedAtEveryExit` walked
pool A alone. It now walks both — but with a different invariant for each, which
took a second review round to get right. Pool A is conserved across §3.9's
exits; pool B is not, because a slot is held for as long as the position needs
one and "cross into Production" legitimately ends at Extracting still holding
one. The first attempt at this fix asserted conservation for both and failed
three of the six rows, calling correct behaviour a leak.

**And one the review did not name.** `TestCancel_ReleasesTheComputeSlot` — the
only test claiming to pin the one release that did exist — was **vacuous**. Its
fixture called `q.slots.release(j.ID())` before `q.Cancel(j)`, so
`q.slots.outstanding() == 0` held no matter what `finishCancel` did; deleting
the release under test would not have turned it red. The fixture did that to
make `running()` false so cancel would settle rather than abort, and in doing so
removed the only slot the assertion could observe. It now uses `SetNext`
instead: `running` is `IsOpen && holds && Next == StateUnset` (§3.4), so a job
whose work has finished is not running **and still holds everything its
position required** — which is the configuration where the leak actually bites.

This is the same failure Half A shipped and this plan's own opening section
indicts: a test written alongside a fix, asserting the state the fix produced
rather than the behaviour it changed. It survived a plan review *and* a
cross-model review before a per-finding verification pass caught it.

**A fifth finding was a missing door.** Spec §5.9 and §5.10 write `q.Retry(j)`
into their traces and two comments in this package point callers at it, but
`Queue` never declared it — so two of the thirteen mandated scenario tests had
no entry point. Task 7 now defines it.

---

## Scope: this is Half B1

The spec (§4.8) scopes Half B as "`gatedBy`, `waitReason`, `grantFor`, `advance`, `Cancel`, the composed view, `ToSABnzbd`'s inputs, the pools", landing as "an amendment to the swap plan's item 3". `internal/queue` is 27,204 lines and already declares `type Queue struct` at `queue.go:74`. Replacing it is not one plan.

This plan is **B1: the decision core, standing alone.** It produces working, testable software: every scheduling decision in the spec, as pure functions over an atomic snapshot, with the §5 scenario suite as its regression tests. Nothing imports it yet — the same posture Half A shipped in, for the same reason (Rule 1: land the end state, do not defend an intermediate).

**B2, a separate plan, owns:** the dispatcher and its `yielded` report (§3.6), `abortWorker`'s real implementation, `q.discard`, persistence of `State`/`Outcome`/`Intent`, `ToSABnzbd`'s composed view, reorder (§4.7), and the swap that deletes the old queue.

**Decision needed before Task 5 (flagged for review, not assumed):** B1 introduces a new package `internal/sched` declaring `type Queue struct`. `sched.Queue` does not clash with `queue.Queue`, and it lets the spec's code transfer verbatim including the `q` receiver. The alternative is a file inside `internal/queue` with a different type name, which entangles a clean unit with a 27k-line package and forfeits the verbatim transfer. **A reviewer who prefers the alternative should say so now**; every later task names the package.

---

## File Structure

**Modified — `internal/job`** (the foundation the findings demand):

| File | Responsibility after this plan |
|---|---|
| `admissibility.go` *(new)* | Sole owner of "which verdict may be recorded from which position". A table plus two functions. |
| `admissibility_test.go` *(new)* | Totality over `AllOutcomes() × AllStates()`, and a product-space test that `finish` agrees with the table at every cell. |
| `attempt.go` | `finish`'s two ad-hoc outcome guards collapse into one table consultation. |
| `lease.go` | `Lease` gains `LeaseID` identity, so two leases are distinguishable and a pool can audit returns. |
| `job.go` | `Snapshot()` — one atomic composite read replacing three separately-locked ones. `Grant` refuses an unidentified lease. |

**New — `internal/sched`** (the decision core):

| File | Responsibility |
|---|---|
| `requirements.go` | §3.4's per-state resource table: `needsLease`, `needsSlot`. Total over `AllStates()`. |
| `pool.go` | `leasePool` (identity-tracked, audits returns) and `slotPool` (counted by job ID). |
| `queue.go` | `Queue` struct, `holds`, `running`, `gatedBy`, `waitReason` — the pure predicates — plus the two resource-return owners, `reclaim` (pool A) and `releaseFor` (pool B). |
| `cancel.go` | `Cancel`, `finishCancel`. Lands before `advance.go`, which calls into it. |
| `advance.go` | `advance`'s three branches, `park`, `grantFor`, `Retry`. |
| `scenario_test.go` | Each of spec §5.1–5.13 as a named test, plus the two-pool exit walk over §3.9. |

---

## Task 1: The admissibility table

Gives the `Outcome × State` rules a computed owner. This is spec change 04, deferred from Half A and promoted to first by the findings above.

**Files:**
- Create: `internal/job/admissibility.go`
- Create: `internal/job/admissibility_test.go`
- Modify: `internal/job/attempt.go` — `finish`, the two outcome guards
- Modify: `internal/job/attempt_test.go` — `TestAttempt_FinishRefusesOKBeforeFinalizing` keeps working unchanged; do not weaken it

**Interfaces:**
- Consumes: `AllOutcomes()`, `AllStates()`, `ErrInvalidOutcome`, `ErrUnrecoverableAfterBoundary` (all existing)
- Produces: `admits(o Outcome, s State) bool`, `inadmissible(o Outcome, s State) error` — unexported, consumed only by `Attempt.finish` in this plan

The table, with the spec citation for every row:

| Outcome | Fetching | Assessing | Repairing | Extracting | Finalizing | Source |
|---|---|---|---|---|---|---|
| `OutcomeOK` | ✗ | ✗ | ✗ | ✗ | ✓ | §3.3 work-spine table: `Finish(OutcomeOK)` is `Finalizing`'s row alone |
| `OutcomeFailed` | ✓ | ✓ | ✓ | ✓ | ✓ | §3.3 failure table: every state either continues or settles Failed |
| `OutcomeUnrecoverable` | ✓ | ✓ | ✓ | ✗ | ✗ | D3: "the job never crossed the boundary" |
| `OutcomeCancelled` | ✓ | ✓ | ✓ | ✓ | ✓ | §5.5 and §5.12 both settle Cancelled from `Extracting` when not running |
| `OutcomePending` | ✗ | ✗ | ✗ | ✗ | ✗ | Not a verdict; `finish` rejects it before reaching the table |

The two cells Half A recorded as undecided — `OutcomeCancelled × Production` — are decided **admissible**, and §5.12 is the case that forces it: a post-boundary job restored from a restart holds nothing, so `running(j)` is false, and `finishCancel` settles it. Refusing that cell would deadlock exactly the scenario revision 3 was fixed to unblock.

- [ ] **Step 1: Write the failing totality test**

```go
package job

import (
	"slices"
	"testing"
)

// TestAdmissibleAt_IsTotal is the exhaustiveness gate. It fails when an Outcome
// or a State is added and nobody classified the new cells — which is the
// failure mode the two ad-hoc guards had, in the form "nobody wrote the cell at
// all". A table with a hole is not an owner.
func TestAdmissibleAt_IsTotal(t *testing.T) {
	for _, o := range AllOutcomes() {
		if o == OutcomePending {
			// Not a verdict. finish rejects it before consulting the table, so
			// the table deliberately has no row — asserted, not skipped.
			if _, ok := admissibleAt[o]; ok {
				t.Errorf("admissibleAt has a row for %v; it is not a verdict and finish refuses it earlier", o)
			}
			continue
		}
		row, ok := admissibleAt[o]
		if !ok {
			t.Errorf("admissibleAt has no row for %v; every settled outcome must name the positions that admit it", o)
			continue
		}
		for _, s := range row {
			if !slices.Contains(AllStates(), s) {
				t.Errorf("admissibleAt[%v] names %v, which AllStates() does not declare", o, s)
			}
		}
	}
	for o := range admissibleAt {
		if !slices.Contains(AllOutcomes(), o) {
			t.Errorf("admissibleAt has a row for %v, which AllOutcomes() does not declare", o)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -count=1 -run TestAdmissibleAt_IsTotal ./internal/job/`
Expected: FAIL to build — `undefined: admissibleAt`. That is a build failure, not an observation; Step 4 produces the real red.

- [ ] **Step 3: Write the table**

```go
package job

import (
	"fmt"
	"slices"
)

// admissibleAt is the sole owner of "which verdict may an attempt record from
// which position". It replaces two hand-written guards in finish, one of which
// was wrong through two review rounds while its own comment stated the correct
// rule beside it.
//
// The value is the set of positions that admit the outcome, not a bool per
// pair, because the rows are what carry meaning: OutcomeOK names one state
// because §3.3 gives Finalizing the only Finish(OutcomeOK) completion, and
// OutcomeUnrecoverable names the Correctness zone because D3 defines it as
// "the job never crossed the boundary". A reader can check each row against
// its clause; a matrix of booleans hides which rule produced which cell.
//
// OutcomePending has no row on purpose: it is not a verdict, and finish
// rejects it before reaching here. TestAdmissibleAt_IsTotal asserts the
// absence rather than letting it be an oversight.
var admissibleAt = map[Outcome][]State{
	// §3.3's work-spine table: Extracting's completion is SetNext(Finalizing);
	// Finish(OutcomeOK) appears on Finalizing's row alone. At Extracting the
	// archives are unpacked into the working directory and nothing has been
	// moved to the destination or run a user script.
	OutcomeOK: {Finalizing},
	// §3.3's failure table: every state either continues to another work state
	// or settles Failed. There is no position from which failing is illegal.
	OutcomeFailed: {Fetching, Assessing, Repairing, Extracting, Finalizing},
	// D3: Unrecoverable means the job never crossed, so its files stay in the
	// working directory and it stays retryable. Past the boundary that is
	// false by construction.
	OutcomeUnrecoverable: {Fetching, Assessing, Repairing},
	// Admissible everywhere, including Production. §5.12 forces it: a
	// post-boundary job restored from a restart holds nothing, running(j) is
	// false, and finishCancel settles it Cancelled from Extracting. Refusing
	// that cell would deadlock the scenario revision 3 was fixed to unblock.
	OutcomeCancelled: {Fetching, Assessing, Repairing, Extracting, Finalizing},
}

// admits reports whether o may be recorded by an attempt sitting at s.
func admits(o Outcome, s State) bool {
	return slices.Contains(admissibleAt[o], s)
}

// inadmissible names why, preserving the sentinel each refusal already had.
// ErrUnrecoverableAfterBoundary stays distinct from ErrInvalidOutcome because
// it means something different to a caller: a downstream stage produced a
// verdict that contradicts where the attempt actually got to, which is a
// caller bug to fix rather than a job to fail.
func inadmissible(o Outcome, s State) error {
	if o == OutcomeUnrecoverable {
		return fmt.Errorf("%w: this attempt is at %s", ErrUnrecoverableAfterBoundary, s)
	}
	return fmt.Errorf("%w: %s is not admissible at %s; it is admissible at %v",
		ErrInvalidOutcome, o, s, admissibleAt[o])
}
```

- [ ] **Step 4: Run the totality test, then mutate it to prove it discriminates**

Run: `go test -count=1 -run TestAdmissibleAt_IsTotal ./internal/job/`
Expected: PASS.

Then delete the `OutcomeCancelled` row and re-run. Expected: FAIL with `admissibleAt has no row for Cancelled`. Restore from your own copy (never `git checkout --`, never `git stash`). Record the message.

- [ ] **Step 5: Pin each row against the clause that produced it**

```go
// TestAdmissibleAt_MatchesTheSpec asserts each row against the clause it comes
// from. Totality (above) proves the table has no holes; this proves the holes
// were filled with the right values. Neither is sufficient alone: a table that
// is total and wrong is exactly what shipped as two hand-written guards.
func TestAdmissibleAt_MatchesTheSpec(t *testing.T) {
	for _, tc := range []struct {
		o      Outcome
		admits []State
		why    string
	}{
		{OutcomeOK, []State{Finalizing},
			"§3.3's work-spine table: Extracting completes with SetNext(Finalizing), " +
				"and Finish(OutcomeOK) is Finalizing's row alone"},
		{OutcomeFailed, []State{Fetching, Assessing, Repairing, Extracting, Finalizing},
			"§3.3's failure table: every state either continues or settles Failed"},
		{OutcomeUnrecoverable, []State{Fetching, Assessing, Repairing},
			"D3: Unrecoverable means the job never crossed the boundary"},
		{OutcomeCancelled, []State{Fetching, Assessing, Repairing, Extracting, Finalizing},
			"§5.5 and §5.12 both settle Cancelled from Extracting when running(j) is false"},
	} {
		t.Run(tc.o.String(), func(t *testing.T) {
			for _, s := range AllStates() {
				want := slices.Contains(tc.admits, s)
				if got := admits(tc.o, s); got != want {
					t.Errorf("admits(%v, %v) = %v, want %v — %s", tc.o, s, got, want, tc.why)
				}
			}
		})
	}
}
```

Run: `go test -count=1 -run TestAdmissibleAt_MatchesTheSpec ./internal/job/` → PASS.

- [ ] **Step 6: Write the product-space test that would have caught the original defect**

```go
// TestFinish_AgreesWithTheTableAtEveryCell walks Outcome × State and asserts
// finish accepts exactly the cells admissibleAt names. This is the test the
// two ad-hoc guards never had: the guard for OutcomeOK was wrong for one
// state out of five, and every test that existed either used a state where it
// happened to be right or asserted the wrong behaviour outright.
//
// It judges with the table, which is legitimate ONLY because the table is the
// thing under test in TestAdmissibleAt_IsTotal and TestAdmissibleAt_MatchesTheSpec
// above — those pin the table against AllStates()/AllOutcomes() and against
// named spec clauses, so this test is checking that the CODE follows the
// table, not that the table agrees with itself.
func TestFinish_AgreesWithTheTableAtEveryCell(t *testing.T) {
	for _, s := range AllStates() {
		for _, o := range AllOutcomes() {
			if o == OutcomePending {
				continue // refused earlier, by IsSettled; covered separately
			}
			t.Run(s.String()+"/"+o.String(), func(t *testing.T) {
				a := attemptAt(t, s)
				err := a.finish(o, testClock())
				if admits(o, s) && err != nil {
					t.Errorf("finish(%v) at %v = %v, want nil — the table admits this cell", o, s, err)
				}
				if !admits(o, s) && err == nil {
					t.Errorf("finish(%v) at %v = nil, want a refusal — the table does not admit this cell", o, s)
				}
			})
		}
	}
}

// attemptAt returns an open attempt sitting at s, driven through the real
// doors rather than constructed field-by-field, so a state this helper cannot
// reach is a state the machine cannot reach.
func attemptAt(t *testing.T, s State) Attempt {
	t.Helper()
	a := newAttempt(testClock())
	switch s {
	case Fetching:
	case Assessing:
		mustTransition(t, &a, Assessing)
	case Repairing:
		mustTransition(t, &a, Assessing)
		mustTransition(t, &a, Repairing)
	case Extracting:
		mustTransition(t, &a, Assessing)
		mustCross(t, &a, Extracting)
	case Finalizing:
		mustTransition(t, &a, Assessing)
		mustCross(t, &a, Extracting)
		mustTransition(t, &a, Finalizing)
	default:
		t.Fatalf("attemptAt has no arm for %v; add one rather than skipping the state", s)
	}
	return a
}
```

- [ ] **Step 7: Run it against the UNCHANGED finish**

Run: `go test -count=1 -run TestFinish_AgreesWithTheTableAtEveryCell ./internal/job/`
Expected: PASS. `finish`'s current guards already agree with the table at every cell — that is the point. This test is the safety net for Step 7, which is a refactor and must not change behaviour.

- [ ] **Step 8: Collapse finish's two guards into one table consultation**

In `internal/job/attempt.go`, replace:

```go
	if o == OutcomeOK && a.state != Finalizing {
		return fmt.Errorf("%w: OutcomeOK claims the job produced its files, but this attempt settled at %s, not Finalizing", ErrInvalidOutcome, a.state)
	}
	if o == OutcomeUnrecoverable && a.crossed() {
		return fmt.Errorf("%w: this attempt already crossed into Production", ErrUnrecoverableAfterBoundary)
	}
```

with:

```go
	// One consultation, not one condition per outcome. The rules live in
	// admissibleAt (admissibility.go) because they relate two axes, and a rule
	// relating two axes written as an `if` here is invisible from the other
	// axis: outcome.go documented OutcomeOK's rule correctly while this
	// function implemented a weaker one, and nothing could see the disagreement.
	//
	// finish still consults neither legalEdges nor next. This narrows which
	// VERDICT a position admits, never whether a position can settle at all —
	// TestAttempt_FinishSucceedsFromAnyOpenState is what keeps those apart.
	if !admits(o, a.state) {
		return inadmissible(o, a.state)
	}
```

- [ ] **Step 9: Run the full package**

Run: `go test -race -count=1 ./internal/job/`
Expected: PASS, including `TestAttempt_FinishRefusesOKBeforeFinalizing` and `TestAttempt_FinishReportsWriteOnceBeforeBoundary` unchanged. If either needed editing, the refactor changed behaviour — stop and diagnose.

- [ ] **Step 10: Mutate the table to prove `finish` consults it**

Change `OutcomeOK: {Finalizing}` to `OutcomeOK: {Extracting, Finalizing}` and run `go test -count=1 -run 'TestAttempt_FinishRefusesOKBeforeFinalizing' ./internal/job/`.
Expected: FAIL at the `Extracting` subtest. Restore from your own copy and record the message. This proves the table is load-bearing rather than decorative.

- [ ] **Step 11: Sweep and commit**

Grep the claims this task falsified, from the repository root:

```bash
git grep -n 'two guards\|both guards\|OutcomeOK &&\|OutcomeUnrecoverable &&'
git grep -n 'admissib'
```

Then run every gate in Global Constraints and commit:

```bash
git add internal/job/admissibility.go internal/job/admissibility_test.go internal/job/attempt.go
git commit  # refactor(job): give the Outcome x State rules one table and one owner
```

---

## Task 2: `Lease` identity

**Files:**
- Modify: `internal/job/lease.go`
- Modify: `internal/job/job.go` — `Grant`
- Modify: `internal/job/lease_test.go`

**Interfaces:**
- Produces: `type LeaseID uint64`, `const LeaseUnset LeaseID = 0`, `func NewLease(id LeaseID) *Lease`, `func (l *Lease) ID() LeaseID`, `var ErrUnidentifiedLease` — all consumed by `internal/sched`'s `leasePool` in Task 4

`Lease` currently has no fields — both are commented out awaiting Half B. A zero-size struct's pointers are **not distinct**, which was verified rather than assumed:

```
two distinct &Lease{} are ==: true
map keyed by *Lease holds 1 entries for two jobs
```

Nothing compares leases today. The Queue is precisely the component that would write `map[*Lease]*Job` to reclaim capacity, and it would silently conflate every job. The hazard also *disappears* the moment a real field lands in B2, which is worse than a permanent bug: interim code would work by accident and break by accident.

- [ ] **Step 1: Write the failing test**

```go
package job

import (
	"errors"
	"testing"
	"unsafe"
)

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
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -count=1 -run 'TestLease_HasDistinctIdentity|TestJob_GrantRefusesUnidentifiedLease' ./internal/job/`
Expected: build failure — `undefined: NewLease`. Not an observation; Step 4 produces the real red.

- [ ] **Step 3: Give `Lease` an id**

In `internal/job/lease.go`, replace the struct and add above it:

```go
// LeaseID identifies one issuance. It exists so a pool can tell two
// outstanding leases apart, verify that a lease handed back is one it issued,
// and name a lease in a log line.
//
// Identity cannot come from the pointer. Lease had no fields while its
// manifest and barrier waited for Half B, and Go gives distinct zero-size
// allocations the same address — so `&Lease{} == &Lease{}` was true and a
// map[*Lease]*Job held one entry for two jobs. That is a defect which would
// have vanished silently when B2 adds the first real field, working by
// accident until then. TestLease_HasDistinctIdentity pins the size directly.
type LeaseID uint64

// LeaseUnset is the invalid zero, in the same spirit as StateUnset: a lease
// nobody issued must not be indistinguishable from one that was. Job.Grant
// refuses it.
const LeaseUnset LeaseID = 0

type Lease struct {
	id LeaseID
	// manifest *Manifest        // Half B2
	// barrier  *StorageBarrier  // Half B2
}

// NewLease mints a lease with the given id. The caller — the Queue's pool — is
// the only thing that knows what ids it has issued, so issuance lives there
// and this is a plain constructor. Passing LeaseUnset produces a lease Grant
// will refuse; that is deliberate, so there is one enforcement point rather
// than two.
func NewLease(id LeaseID) *Lease { return &Lease{id: id} }

// ID reports which issuance this lease is.
func (l *Lease) ID() LeaseID { return l.id }
```

In `internal/job/job.go`, add the sentinel beside `ErrNilLease` and extend `Grant`:

```go
// ErrUnidentifiedLease is returned by Grant for a lease carrying LeaseUnset —
// one nobody issued. Distinct from ErrNilLease: that one says the caller
// passed nothing, this says the caller passed something it built itself.
var ErrUnidentifiedLease = errors.New("job: Grant: lease has no id; only an issuing pool may mint one")
```

```go
	if l == nil {
		return ErrNilLease
	}
	if l.id == LeaseUnset {
		return ErrUnidentifiedLease
	}
```

- [ ] **Step 4: Run the tests, then mutate**

Run: `go test -count=1 -run 'TestLease_HasDistinctIdentity|TestJob_GrantRefusesUnidentifiedLease' ./internal/job/`
Expected: PASS.

Then neuter the id check to `if false {` and re-run. Expected: FAIL on `TestJob_GrantRefusesUnidentifiedLease` with `Grant(&Lease{}) = <nil>, want ErrUnidentifiedLease`. Restore from your own copy and record the message.

- [ ] **Step 5: Fix every `&Lease{}` in existing tests**

```bash
git grep -n '&Lease{}' -- 'internal/job/*_test.go'
```

Each is a lease a test grants. Replace with `NewLease(1)`, using distinct ids where a test grants more than one. Where a test asserts `Grant` refuses something, keep `&Lease{}` — it is now testing the new guard.

Run: `go test -race -count=1 ./internal/job/`
Expected: PASS.

- [ ] **Step 6: Gates and commit**

```bash
git add internal/job/lease.go internal/job/job.go internal/job/lease_test.go
git commit  # fix(job): give Lease an identity, so distinct leases are distinguishable
```

---

## Task 3: One atomic composite read

**Files:**
- Modify: `internal/job/job.go`
- Modify: `internal/job/job_test.go`

**Interfaces:**
- Produces: `type Snapshot struct`, `func (j *Job) Snapshot() Snapshot` — every predicate in `internal/sched` takes a `Snapshot`, never a `*Job`

`State()`, `HasRun()`, `HoldsLease()` and `Intent()` each take `j.mu.RLock()` **separately**. Every Queue predicate composes several of them:

```go
running(j) ≡ attempt open && holds(j) && next unset
```

That is at least three separate acquisitions, so a concurrent door can land between them and the predicate answers about a job that never existed in that configuration. Half A's doors are each atomic; the *composite questions the Queue asks* have no atomic reader. This is the same defect shape as Task 1 — owned per field, unowned in composition.

It also makes spec §6 test 1 achievable rather than aspirational: predicates over a value cannot acquire resources, so purity stops being a property you assert and becomes one the signature enforces.

- [ ] **Step 1: Write the failing test**

```go
// TestJob_SnapshotIsAtomic pins that a snapshot is taken under one lock
// acquisition, by racing it against the doors and requiring the result be
// internally consistent at every observation.
//
// The consistency rule chosen is the one the Queue actually depends on: a
// settled attempt has HasRun true, and a job that has never run has a
// StateUnset position with a Pending outcome. Composing State() and HasRun()
// separately can observe HasRun false with a real position, which is a job
// configuration that has never existed.
func TestJob_SnapshotIsAtomic(t *testing.T) {
	j := newTestJob(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			_ = j.BeginAttempt(testClock())
			_ = j.Transition(Assessing)
			_, _ = j.Finish(OutcomeFailed, testClock())
		}
	}()
	for i := 0; i < 20000; i++ {
		s := j.Snapshot()
		// The invariant, as a BICONDITIONAL: a job has run if and only if it is
		// at a position. newAttempt starts an attempt at Fetching, and settling
		// no longer moves it, so every job with an attempt has a position and
		// every job without one has StateUnset.
		//
		// Stated both ways deliberately. An earlier draft asserted only
		// `position set && !HasRun`, which is VACUOUS: Go evaluates a composite
		// literal's fields left to right, so the torn version reads HasRun
		// strictly after State, and len(attempts) never shrinks — so HasRun can
		// never be observed false once a position has been seen. The tear that
		// IS observable is the other direction, State read before the first
		// append and HasRun after it. A biconditional catches both and does not
		// depend on the field order, which a reader should not have to reason
		// about to trust this test.
		if s.HasRun != (s.State.State != StateUnset) {
			t.Fatalf("torn snapshot: HasRun=%v but position=%v; a job has run if and only if "+
				"it is at a position", s.HasRun, s.State.State)
		}
	}
	<-done
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -count=1 -run TestJob_SnapshotIsAtomic ./internal/job/`
Expected: build failure — `undefined: j.Snapshot`. Step 4 gives the real red.

- [ ] **Step 3: Add `Snapshot`**

```go
// Snapshot is every fact about a job that a scheduling decision needs, read
// under ONE lock acquisition.
//
// It exists because the Queue's questions are composite. running(j) is "the
// attempt is open AND it holds what its state requires AND next is unset" —
// three facts that State(), HasRun() and HoldsLease() each lock for
// separately, so a door landing between two of them yields an answer about a
// configuration the job was never in. Half A gave every FIELD an owner and
// left the composite questions unowned; this is that gap closed.
//
// It is also what makes the render path's purity structural rather than
// asserted (spec §6, test 1): a predicate over a Snapshot has no *Job to
// acquire anything from.
type Snapshot struct {
	State      StateView
	Intent     Intent
	HoldsLease bool
	HasRun     bool
}

// IsOpen reports whether an attempt is live — begun and not yet settled. It is
// on Snapshot rather than Job because every caller asking it is asking about a
// consistent moment, and Job has no exported isOpen for exactly that reason.
func (s Snapshot) IsOpen() bool { return s.HasRun && !s.State.Outcome.IsSettled() }

// Snapshot reads every scheduling-relevant fact under one RLock.
func (j *Job) Snapshot() Snapshot {
	j.mu.RLock()
	defer j.mu.RUnlock()
	s := Snapshot{
		Intent:     j.intent,
		HoldsLease: j.lease != nil,
		HasRun:     len(j.attempts) > 0,
	}
	if a := j.currentLocked(); a != nil {
		s.State = a.view()
	}
	return s
}
```

- [ ] **Step 4: Run under the race detector, then mutate**

Run: `go test -race -count=1 -run TestJob_SnapshotIsAtomic ./internal/job/`
Expected: PASS.

Then replace `Snapshot`'s body with the torn composition it exists to replace:

```go
func (j *Job) Snapshot() Snapshot {
	return Snapshot{State: j.State(), Intent: j.Intent(), HoldsLease: j.HoldsLease(), HasRun: j.HasRun()}
}
```

Re-run. Expected: FAIL with `torn snapshot: HasRun=true but position=StateUnset`. If it passes, raise the iteration counts until it fails; a test that cannot observe the tear is not pinning anything.

**Do not accept a pass here.** This exact test was written vacuous once already: the first draft asserted `position set && !HasRun`, which no torn read can produce, because the torn composite literal evaluates `HasRun` last and `len(attempts)` never shrinks. It looked like a race test and could not fail. If the mutation does not go red after raising the counts, the assertion is wrong, not the mutation. Restore from your own copy and record the message.

- [ ] **Step 5: Gates and commit**

```bash
git add internal/job/job.go internal/job/job_test.go
git commit  # feat(job): add Snapshot, one atomic read of every scheduling fact
```

---

## Task 4: `internal/sched` foundations — requirements table and pools

**Files:**
- Create: `internal/sched/requirements.go`, `internal/sched/requirements_test.go`
- Create: `internal/sched/pool.go`, `internal/sched/pool_test.go`
- Create: `internal/sched/doc.go`

**Interfaces:**
- Consumes: `job.AllStates()`, `job.State`, `job.Lease`, `job.LeaseID`, `job.NewLease`
- Produces: `needsLease(s job.State) bool`, `needsSlot(s job.State) bool`, `newLeasePool(capacity int) *leasePool`, `(*leasePool).issue() *job.Lease`, `(*leasePool).reclaim(l *job.Lease) error`, `(*leasePool).outstanding() int`, `newSlotPool(capacity int) *slotPool`, `(*slotPool).acquire(id string) bool`, `(*slotPool).release(id string)`, `(*slotPool).holds(id string) bool`

This resolves spec §8 open question 2 — where the per-state resource requirements live — in favour of `internal/sched`, because only the Queue consumes them and `internal/job` would then carry a table no in-package caller reads.

`leasePool` is the answer to the finding that **lease return has no owner**. `Cross` and `Finish` return `(*Lease, error)` and `_, err := j.Cross(…)` compiles, dropping a capacity slot. Go cannot enforce use of a return value and `golangci-lint` has no check for it, so the enforcement has to be an audit on the pool side: the pool knows what it issued, and a test can assert nothing is outstanding at the end of a scenario. That is spec §6 test 4b, made possible by Task 2's identity.

- [ ] **Step 1: Write the failing requirements test**

```go
package sched

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestRequirements_AreTotal fails when a State is added and nobody classified
// what it requires. The table is §3.4's, and a state missing from it silently
// requires nothing — which reads as "runnable with no resources" and is the
// most dangerous possible default.
func TestRequirements_AreTotal(t *testing.T) {
	want := map[job.State]struct{ lease, slot bool }{
		job.Fetching:   {lease: true, slot: false},
		job.Assessing:  {lease: true, slot: true},
		job.Repairing:  {lease: true, slot: true},
		job.Extracting: {lease: false, slot: true},
		job.Finalizing: {lease: false, slot: true},
	}
	for _, s := range job.AllStates() {
		w, ok := want[s]
		if !ok {
			t.Errorf("%v is declared by AllStates() but this test does not classify it; "+
				"add the row deliberately rather than letting the state require nothing", s)
			continue
		}
		if got := needsLease(s); got != w.lease {
			t.Errorf("needsLease(%v) = %v, want %v (§3.4)", s, got, w.lease)
		}
		if got := needsSlot(s); got != w.slot {
			t.Errorf("needsSlot(%v) = %v, want %v (§3.4)", s, got, w.slot)
		}
	}
	if len(want) != len(job.AllStates()) {
		t.Errorf("this test classifies %d states, AllStates() declares %d", len(want), len(job.AllStates()))
	}
}

// TestRequirements_StateUnsetRequiresNothing pins the sentinel separately. It
// is not in AllStates(), and §3.4 says a job with no attempt requires nothing —
// a gate that asks for a lease on behalf of a never-run job would hold pool-A
// capacity for a job that has not started.
func TestRequirements_StateUnsetRequiresNothing(t *testing.T) {
	if needsLease(job.StateUnset) || needsSlot(job.StateUnset) {
		t.Error("StateUnset requires a resource; a job with no attempt is not at a state (§3.4)")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -count=1 ./internal/sched/`
Expected: build failure, package does not exist.

- [ ] **Step 3: Write the requirements table and package doc**

`internal/sched/doc.go`:

```go
// Package sched holds the scheduling decisions for the job lifecycle: which
// jobs may run, what they are waiting for, and what moves them.
//
// It depends on internal/job and nothing else. Every decision is a function of
// a job.Snapshot — a value, taken under one lock — so no decision can acquire a
// resource as a side effect of being asked. Acquisition happens in exactly one
// place, grantFor.
//
// It does NOT contain the dispatcher, the workers, persistence, or rendering.
// Those are Half B2, which also retires internal/queue. Nothing imports this
// package yet, by design.
package sched
```

`internal/sched/requirements.go`:

```go
package sched

import "github.com/hobeone/gonzbd/internal/job"

// needsLease and needsSlot are design §3.4's resource table. They answer about
// a POSITION, never about an attempt: settledness must be checked by the caller
// first, because a settled attempt keeps the position it settled at and would
// otherwise be asked to hold a lease it no longer needs.
//
// The table lives here rather than in internal/job because the Queue is its
// only consumer (spec §8, open question 2). A table in internal/job with no
// in-package caller would be a second place to maintain one fact.
func needsLease(s job.State) bool {
	return s == job.Fetching || s == job.Assessing || s == job.Repairing
}

// needsSlot reports whether s needs a compute slot from pool B. Fetching does
// not: it is network-bound and its concurrency is pool A's business.
func needsSlot(s job.State) bool {
	return s == job.Assessing || s == job.Repairing || s == job.Extracting || s == job.Finalizing
}
```

- [ ] **Step 4: Run to verify it passes, then mutate**

Run: `go test -count=1 ./internal/sched/`
Expected: PASS.

Then drop `s == job.Repairing` from `needsSlot` and re-run. Expected: FAIL with `needsSlot(Repairing) = false, want true`. Restore and record.

- [ ] **Step 5: Write the failing pool test**

```go
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
		t.Errorf("second reclaim(a) = %v, want errNotOutstanding — a double return would " +
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
```

- [ ] **Step 6: Run to verify it fails, then write the pools**

Run: `go test -count=1 -run TestLeasePool ./internal/sched/`
Expected: build failure — `undefined: newLeasePool`. Then write `internal/sched/pool.go`:

```go
package sched

import (
	"errors"
	"fmt"

	"github.com/hobeone/gonzbd/internal/job"
)

// errNotOutstanding is returned by leasePool.reclaim for a lease this pool did
// not issue, or already got back. Both are caller bugs that inflate capacity:
// a double return frees a slot twice, so two jobs end up holding one.
var errNotOutstanding = errors.New("sched: lease is not outstanding")

// leasePool issues pool-A admission tokens and audits their return.
//
// It tracks ids rather than pointers because it must be able to say "I did not
// issue this", which pointer identity alone cannot express — and because a
// pointer-keyed map was outright broken while job.Lease was zero-sized.
//
// The audit exists because nothing else can enforce it. job.Cross and
// job.Finish return a *Lease and Go has no must-use; a caller writing
// `_, err := j.Cross(to)` silently drops a slot, and neither go vet nor
// golangci-lint sees it. The pool knowing what is outstanding is what lets a
// scenario test assert none were lost (spec §6, test 4b).
//
// Not goroutine-safe: every caller holds Queue.mu. Stated rather than locked,
// because a second lock here would be a second thing to order against
// Queue.mu and Job.mu (prior spec §7.1).
type leasePool struct {
	capacity int
	next     job.LeaseID
	issued   map[job.LeaseID]bool
}

func newLeasePool(capacity int) *leasePool {
	return &leasePool{capacity: capacity, issued: make(map[job.LeaseID]bool, capacity)}
}

// issue mints a lease, or returns nil when the pool is at capacity. nil means
// "no capacity", which callers already handle — grantFor returns false and the
// job waits with reason NoLease.
func (p *leasePool) issue() *job.Lease {
	if len(p.issued) >= p.capacity {
		return nil
	}
	p.next++
	p.issued[p.next] = true
	return job.NewLease(p.next)
}

// reclaim returns a lease to the pool. It no-ops on nil, which §3.9 requires:
// a job may legitimately reach the crossing holding nothing, and one function
// that accepts nil is fewer chances to forget than a nil check at every exit.
func (p *leasePool) reclaim(l *job.Lease) error {
	if l == nil {
		return nil
	}
	if !p.issued[l.ID()] {
		return fmt.Errorf("%w: id %d", errNotOutstanding, l.ID())
	}
	delete(p.issued, l.ID())
	return nil
}

func (p *leasePool) outstanding() int { return len(p.issued) }

// slotPool is pool-B compute capacity, held by job ID. Slots have no object
// because nothing travels with them — unlike a lease, which carries the
// Manifest and StorageBarrier (spec §6).
type slotPool struct {
	capacity int
	held     map[string]bool
}

func newSlotPool(capacity int) *slotPool {
	return &slotPool{capacity: capacity, held: make(map[string]bool, capacity)}
}

// acquire is idempotent: a job that already holds a slot keeps it and the call
// reports success, so a caller re-granting for a state it is already running
// does not consume a second slot.
func (p *slotPool) acquire(id string) bool {
	if p.held[id] {
		return true
	}
	if len(p.held) >= p.capacity {
		return false
	}
	p.held[id] = true
	return true
}

func (p *slotPool) release(id string)     { delete(p.held, id) }
func (p *slotPool) holds(id string) bool  { return p.held[id] }
func (p *slotPool) outstanding() int      { return len(p.held) }
```

- [ ] **Step 7: Run, mutate, commit**

Run: `go test -race -count=1 ./internal/sched/` → PASS.

Mutate: make `reclaim` return `nil` unconditionally instead of checking `issued`. Expected: FAIL on the double-reclaim assertion. Restore and record.

```bash
git add internal/sched/
git commit  # feat(sched): add the resource requirements table and the audited pools
```

---

## Task 5: `Queue`, `holds`, `running`, `gatedBy`, `waitReason`

**Files:**
- Create: `internal/sched/queue.go`, `internal/sched/queue_test.go`

**Interfaces:**
- Consumes: `needsLease`, `needsSlot`, `newLeasePool`, `newSlotPool` (Task 4); `job.Snapshot` (Task 3)
- Produces: `type Queue struct`, `func New(leaseCap, slotCap int, clock func() time.Time, w Workers) *Queue`, `type Workers interface`, `(*Queue).holds(id string, s job.Snapshot) bool`, `(*Queue).running(id string, s job.Snapshot) bool`, `(*Queue).gatedBy(s job.Snapshot) (job.WaitReason, bool)`, `(*Queue).waitReason(id string, s job.Snapshot) (job.WaitReason, bool)`, `(*Queue).reclaim(l *job.Lease) error`, `(*Queue).releaseFor(j *job.Job, s job.State)`

Every predicate takes a `job.Snapshot` and a job ID, never a `*job.Job`. That is what makes purity structural: a function with no `*Job` cannot call a door.

- [ ] **Step 1: Write the failing purity and precedence tests**

```go
// TestPredicatesArePure is spec §6 test 1. It calls both predicates across the
// product space with BOTH pools exhausted and asserts occupancy is unchanged.
// Acquisition leaking into the render path is the failure mode that does not
// show up as a wrong answer — it shows up as capacity disappearing while
// someone looks at a page.
func TestPredicatesArePure(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	if q.leases.issue() == nil || !q.slots.acquire("other") {
		t.Fatal("could not exhaust the pools for the test")
	}
	beforeL, beforeS := q.leases.outstanding(), q.slots.outstanding()
	for _, st := range append(job.AllStates(), job.StateUnset) {
		for _, nx := range append(job.AllStates(), job.StateUnset) {
			for _, o := range job.AllOutcomes() {
				for _, in := range job.AllIntents() {
					for _, holdsLease := range []bool{false, true} {
						for _, paused := range []bool{false, true} {
							q.paused = paused
							s := job.Snapshot{
								State:      job.StateView{State: st, Next: nx, Outcome: o},
								Intent:     in,
								HoldsLease: holdsLease,
								HasRun:     st != job.StateUnset,
							}
							q.gatedBy(s)
							q.waitReason("j1", s)
						}
					}
				}
			}
		}
	}
	if q.leases.outstanding() != beforeL || q.slots.outstanding() != beforeS {
		t.Errorf("pools moved during a render-path walk: leases %d→%d, slots %d→%d",
			beforeL, q.leases.outstanding(), beforeS, q.slots.outstanding())
	}
}

// TestGatePrecedenceIsTotal is spec §6 test 2. It walks
// Intent × globalPause × holdsLease × holdsSlot × State and asserts waitReason
// agrees with §3.4 at every point, rather than at the handful of points a
// hand-written case list would reach.
//
// The expected value is recomputed by expectedWait, which shares no code with
// waitReason, gatedBy, running or holds — the four functions under test. It
// DOES call needsLease and needsSlot, and that is a deliberate, bounded
// exception: those two are pinned against a literal table in
// TestRequirements_AreTotal, so they are not deciding this test's answer by
// the same reasoning the code uses. Restating them a third time here would put
// §3.4's resource rules in three places, which is the failure this plan exists
// to stop. Stated rather than left implicit, because "the oracle is
// independent" is exactly the kind of claim that has been wrong here before.
func TestGatePrecedenceIsTotal(t *testing.T) {
	states := append(job.AllStates(), job.StateUnset)
	for _, paused := range []bool{false, true} {
		for _, in := range job.AllIntents() {
			if in == job.IntentCancel {
				// Absent from gatedBy on purpose: advance handles cancel first,
				// so no cancel value reaches the render path (§3.4).
				continue
			}
			for _, st := range states {
				for _, holdsLease := range []bool{false, true} {
					for _, holdsSlot := range []bool{false, true} {
						for _, settled := range []bool{false, true} {
							q := New(1, 1, testClock, &stubWorkers{})
							q.paused = paused
							if holdsSlot {
								q.slots.acquire("j1")
							}
							o := job.OutcomePending
							if settled {
								o = job.OutcomeFailed
							}
							s := job.Snapshot{
								State:      job.StateView{State: st, Outcome: o},
								Intent:     in,
								HoldsLease: holdsLease,
								HasRun:     st != job.StateUnset,
							}
							gotR, gotW := q.waitReason("j1", s)
							wantR, wantW := expectedWait(st, o, in, paused, holdsLease, holdsSlot)
							if gotR != wantR || gotW != wantW {
								t.Errorf("waitReason(state=%v settled=%v intent=%v paused=%v lease=%v slot=%v) = %v,%v; want %v,%v",
									st, settled, in, paused, holdsLease, holdsSlot, gotR, gotW, wantR, wantW)
							}
						}
					}
				}
			}
		}
	}
}

// expectedWait is §3.4 written out longhand, independently of waitReason.
func expectedWait(st job.State, o job.Outcome, in job.Intent, paused, lease, slot bool) (job.WaitReason, bool) {
	if o.IsSettled() {
		return 0, false
	}
	open := st != job.StateUnset
	holdsAll := (!needsLease(st) || lease) && (!needsSlot(st) || slot)
	if open && holdsAll { // next is always unset in this walk
		return 0, false
	}
	if in == job.IntentPause {
		return job.UserPaused, true
	}
	if paused {
		return job.GlobalPause, true
	}
	if st == job.StateUnset {
		return job.NoLease, true
	}
	if needsLease(st) && !lease {
		return job.NoLease, true
	}
	return job.NoComputeSlot, true
}

// TestWaitReason_SettledAndNeverRunReturnEarly pins the two early returns §3.4
// calls "not decoration". Without them a settled job reports NoComputeSlot —
// waiting for a slot it will never use — and a never-run job reports
// NoComputeSlot when it is waiting for a lease.
func TestWaitReason_SettledAndNeverRunReturnEarly(t *testing.T) {
	q := New(0, 0, testClock, &stubWorkers{})
	settled := job.Snapshot{
		State:  job.StateView{State: job.Fetching, Outcome: job.OutcomeFailed},
		Intent: job.IntentRun, HasRun: true,
	}
	if r, waiting := q.waitReason("j1", settled); waiting {
		t.Errorf("waitReason(settled at Fetching) = %v, true; a settled attempt waits for nothing "+
			"even though its position still requires a lease (§3.4, corrected by change 03)", r)
	}
	never := job.Snapshot{Intent: job.IntentRun}
	r, waiting := q.waitReason("j2", never)
	if !waiting || r != job.NoLease {
		t.Errorf("waitReason(never run) = %v, %v; want NoLease, true", r, waiting)
	}
}

// TestWaitReason_UsesTheNextStateWhenWorkHasEnded pins §3.4's "which state's
// requirements to test" rule. Assessing{next: Fetching} after a NeedsMore
// verdict holds its lease and needs only that lease — testing Assessing's
// requirements reports NoComputeSlot for a job that should be granted at once.
func TestWaitReason_UsesTheNextStateWhenWorkHasEnded(t *testing.T) {
	q := New(1, 0, testClock, &stubWorkers{})
	s := job.Snapshot{
		State:      job.StateView{State: job.Assessing, Next: job.Fetching, Outcome: job.OutcomePending},
		Intent:     job.IntentRun,
		HoldsLease: true,
		HasRun:     true,
	}
	if r, waiting := q.waitReason("j1", s); waiting {
		t.Errorf("waitReason(Assessing{next: Fetching}, holding a lease) = %v, true; "+
			"it waits on what FETCHING needs, and it already holds that", r)
	}
}

// TestWaitReason_PostBoundaryWaitsForASlotNotALease pins the bug revision 3
// shipped: an unconditional !HoldsLease() reports NoLease for a job that gave
// its lease up at the crossing and is in fact waiting for a compute slot.
func TestWaitReason_PostBoundaryWaitsForASlotNotALease(t *testing.T) {
	q := New(1, 0, testClock, &stubWorkers{})
	s := job.Snapshot{
		State:  job.StateView{State: job.Extracting, Outcome: job.OutcomePending},
		Intent: job.IntentRun, HasRun: true,
	}
	r, waiting := q.waitReason("j1", s)
	if !waiting || r != job.NoComputeSlot {
		t.Errorf("waitReason(Extracting, no lease, no slot) = %v, %v; want NoComputeSlot, true — "+
			"Extracting does not need a lease, it surrendered one at the crossing", r, waiting)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -count=1 ./internal/sched/`
Expected: build failure — `undefined: New`.

- [ ] **Step 3: Write `queue.go`**

```go
package sched

import (
	"sync"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// Workers is what the Queue needs from the execution side. Half B2 implements
// it; B1 tests it with a stub. It is an interface rather than a concrete type
// so that this package's tests never need a real downloader, and so the
// dependency points from B2 to here rather than the other way.
type Workers interface {
	// Abort tells the worker running this job to stop. It returns
	// immediately; the job settles on a later tick, once running() has gone
	// false. §3.7: "immediately" describes when the worker is TOLD to stop,
	// not when its resources are taken.
	Abort(jobID string)
}

// Queue owns admission. Its lock is taken before any Job lock (prior spec
// §7.1's order, Queue.mu before Job.mu) and no method here calls back into a
// Job while another Job's lock is held.
type Queue struct {
	mu     sync.Mutex
	paused bool
	leases *leasePool
	slots  *slotPool
	clock  func() time.Time
	work   Workers
}

func New(leaseCap, slotCap int, clock func() time.Time, w Workers) *Queue {
	return &Queue{
		leases: newLeasePool(leaseCap),
		slots:  newSlotPool(slotCap),
		clock:  clock,
		work:   w,
	}
}

func (q *Queue) now() time.Time { return q.clock() }

// reclaim is the SOLE reclaimer (§3.9). It no-ops on nil so that no call site
// has to test for it — a job may legitimately reach the crossing holding
// nothing, having been paused at Assessing{next: Extracting} and resumed.
//
// It lives here rather than beside advance because Cancel needs it too, and
// Cancel lands first: a task whose code does not compile on its own is not a
// task, and every commit must leave the repository building.
func (q *Queue) reclaim(l *job.Lease) error { return q.leases.reclaim(l) }

// releaseFor is the SOLE owner of slot release, and grantFor's dual: it frees
// what s does not require. job.StateUnset requires nothing, which is how park,
// settlement and cancel free everything without naming pool B at the call.
//
// It lives here beside reclaim, rather than beside grantFor in advance.go, for
// exactly reclaim's reason: finishCancel needs it and Cancel lands first.
// Together the two are pool A's and pool B's single return paths.
//
// It exists because a review of this plan found THREE separate slot leaks —
// the Assessing → Fetching demotion, park, and settlement — where a per-site
// release would have had to be remembered at each, and was not. The asymmetry
// was total and is the whole argument: the lease got an owner in Task 4 and
// leaked nowhere; the slot had none and leaked everywhere.
//
// The asymmetry was not acquire-count versus release-count. Acquisition
// already HAD an owner — grantFor is the sole production caller of
// slots.acquire (`grep -n 'slots\.acquire'` returns four hits, three of them
// in tests). Release had no owner at all: one ad-hoc call inside finishCancel,
// which is why the three paths that never reach finishCancel each leaked.
//
// Slots differ from leases in that nothing travels with them: the job does not
// carry one, so there is no Surrender to mirror, and release is idempotent
// (slotPool.release is a map delete). That is why this takes a *job.Job and a
// target state rather than a token.
func (q *Queue) releaseFor(j *job.Job, s job.State) {
	if !needsSlot(s) {
		q.slots.release(j.ID())
	}
}

// holds reports whether the job holds everything its CURRENT position
// requires. It says nothing about whether the attempt is open — running()
// adds that, and §3.4 explains why the two must stay separate.
func (q *Queue) holds(id string, s job.Snapshot) bool {
	pos := s.State.State
	if needsLease(pos) && !s.HoldsLease {
		return false
	}
	if needsSlot(pos) && !q.slots.holds(id) {
		return false
	}
	return true
}

// running is §3.4's three-conjunct definition. Every conjunct is load-bearing:
// a job whose work has finished is waiting to move, not running (the next
// clause); and a settled attempt keeps its position, so holds() may be
// genuinely true for it — only the open clause excludes it.
func (q *Queue) running(id string, s job.Snapshot) bool {
	return s.IsOpen() && q.holds(id, s) && s.State.Next == job.StateUnset
}

// gatedBy reports an intent or queue-wide gate. Resources are NOT consulted:
// they are a grant question, not a gate question.
//
// IntentCancel is absent deliberately — advance handles it first, so no cancel
// value reaches the render path.
func (q *Queue) gatedBy(s job.Snapshot) (job.WaitReason, bool) {
	if s.Intent == job.IntentPause {
		return job.UserPaused, true
	}
	if q.paused {
		return job.GlobalPause, true
	}
	return 0, false
}

// waitReason explains why this job is not running, about its CURRENT state.
func (q *Queue) waitReason(id string, s job.Snapshot) (job.WaitReason, bool) {
	if s.State.Outcome.IsSettled() {
		return 0, false // terminal: waiting for nothing, whatever its position requires
	}
	if q.running(id, s) {
		return 0, false
	}
	if r, gated := q.gatedBy(s); gated {
		return r, true
	}
	if s.State.State == job.StateUnset {
		return job.NoLease, true // waiting to start
	}
	want := s.State.State
	if s.State.Next != job.StateUnset {
		want = s.State.Next // work ended; it waits on what the NEXT state needs
	}
	if needsLease(want) && !s.HoldsLease {
		return job.NoLease, true
	}
	return job.NoComputeSlot, true
}
```

Add to `queue_test.go`:

```go
type stubWorkers struct{ aborted []string }

func (s *stubWorkers) Abort(jobID string) { s.aborted = append(s.aborted, jobID) }

func testClock() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
```

- [ ] **Step 4: Run, then mutate each early return**

Run: `go test -race -count=1 ./internal/sched/` → PASS.

Three separate mutations, each restored from your own copy before the next:
1. Delete `if s.State.Outcome.IsSettled()` from `waitReason` → expect `TestWaitReason_SettledAndNeverRunReturnEarly` red.
2. Replace `want = s.State.Next` with `want = s.State.State` → expect `TestWaitReason_UsesTheNextStateWhenWorkHasEnded` red.
3. Replace `needsLease(want) && !s.HoldsLease` with `!s.HoldsLease` → expect `TestWaitReason_PostBoundaryWaitsForASlotNotALease` red.

Record all three messages. Each pins a defect a previous revision actually shipped.

- [ ] **Step 5: Gates and commit**

```bash
git add internal/sched/queue.go internal/sched/queue_test.go
git commit  # feat(sched): add the pure scheduling predicates over a job snapshot
```

---

## Task 6: `Cancel` and `finishCancel`

**Files:**
- Create: `internal/sched/cancel.go`, `internal/sched/cancel_test.go`

**Interfaces:**
- Consumes: Tasks 4 and 5
- Produces: `(*Queue).Cancel(j *job.Job) error`, `(*Queue).finishCancel(j *job.Job, s job.Snapshot) error`

`finishCancel` takes the snapshot `Advance` already read, rather than re-reading. Two reads would reintroduce the tear Task 3 closed, at the one call site where the intent has just changed.

The **gate is `IsProduction && running`, not `IsProduction && !workDone`** — §3.7 lists three ways the latter failed, all because `Finalizing` never sets `next`.

- [ ] **Step 1: Write the failing tests**

```go
// TestCancel_PreBoundaryAbortsRatherThanSeizing pins §3.7's interrupt arm.
// "Immediately" describes when the worker is TOLD to stop, not when its
// resources are taken: the Manifest and StorageBarrier come with the lease, so
// reclaiming one from under a downloader mid-article is a use-after-free in
// all but name.
func TestCancel_PreBoundaryAbortsRatherThanSeizing(t *testing.T) {
	w := &stubWorkers{}
	q := New(1, 1, testClock, w)
	j := job.New("j1", "n", job.Policy{})
	mustHoldAt(t, q, j, job.Fetching)
	if err := q.Cancel(j); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if len(w.aborted) != 1 || w.aborted[0] != "j1" {
		t.Errorf("aborted = %v, want [j1]", w.aborted)
	}
	if !j.HoldsLease() {
		t.Error("Cancel took the lease from a live worker; it must wait for the yield")
	}
	if j.Snapshot().State.Outcome.IsSettled() {
		t.Error("Cancel settled a job whose worker is still live")
	}
}

// TestCancel_PostBoundaryNotRunningSettlesAtOnce is scenario §5.12: a job
// restored from a restart holds nothing, so running() is false and there is no
// worker to protect. Revision 3 gated on !workDone and deadlocked here.
func TestCancel_PostBoundaryNotRunningSettlesAtOnce(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustHoldAt(t, q, j, job.Assessing)
	if err := j.SetNext(job.Extracting); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	l, err := j.Cross(job.Extracting) // the crossing, driven directly
	if err != nil {
		t.Fatalf("Cross: %v", err)
	}
	if err := q.reclaim(l); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	// Holds nothing now, which is exactly the restored-from-restart shape.
	if err := q.Cancel(j); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got := j.Snapshot().State.Outcome; got != job.OutcomeCancelled {
		t.Errorf("Outcome = %v, want Cancelled — nothing was in flight to protect", got)
	}
}

// TestCancel_ReleasesTheComputeSlot pins the leak §3.7 names: Assessing and
// Repairing hold a slot alongside the lease, and an earlier revision reclaimed
// only the lease, leaking pool-B capacity on every cancel from those states.
//
// The job must hold the slot AT the moment Cancel runs, or this test asserts
// nothing. An earlier draft released it in the fixture — to make running()
// false so cancel would settle rather than abort — and thereby removed the
// only slot the assertion could have caught. It passed against a finishCancel
// that released nothing.
//
// SetNext is what makes the job settle-able while still holding: running() is
// `IsOpen && holds && Next == StateUnset` (§3.4), so a job whose work has
// FINISHED is not running yet still holds everything Assessing required. That
// is a real configuration — the assessor is done and the job is waiting to
// move — and it is the one where the leak bites.
func TestCancel_ReleasesTheComputeSlot(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustHoldAt(t, q, j, job.Assessing)
	if err := j.SetNext(job.Repairing); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if !q.slots.holds(j.ID()) {
		t.Fatal("fixture holds no slot; this test cannot observe a slot leak")
	}
	if err := q.Cancel(j); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if q.leases.outstanding() != 0 {
		t.Errorf("leases outstanding = %d after a cancel, want 0", q.leases.outstanding())
	}
	if q.slots.outstanding() != 0 {
		t.Errorf("slots outstanding = %d after a cancel, want 0", q.slots.outstanding())
	}
}
```

- [ ] **Step 2: Run to verify it fails, then write `cancel.go`**

Run: `go test -count=1 -run TestCancel ./internal/sched/` → build failure, `undefined: q.Cancel`.

```go
package sched

import "github.com/hobeone/gonzbd/internal/job"

// Cancel latches the job's intent and then settles it if nothing is in flight.
// Prior spec §8.4 makes cancel an interrupt before the boundary and a gate after it.
func (q *Queue) Cancel(j *job.Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := j.SetIntent(job.IntentCancel); err != nil {
		return err
	}
	return q.finishCancel(j, j.Snapshot())
}

// finishCancel takes the snapshot its caller already read rather than reading
// again: two reads would reintroduce the tear job.Snapshot closes, at the one
// site where the intent has just changed underneath.
//
// The gate is IsProduction && running, NOT IsProduction && !workDone. §3.7
// lists three ways the latter fails, all because Finalizing never sets next so
// !workDone is permanently true there: a Finalizing job could never be
// cancelled; a post-boundary job restored from a restart gated forever; and a
// Finalizing job waiting for a slot gated forever with no work in flight.
func (q *Queue) finishCancel(j *job.Job, s job.Snapshot) error {
	if s.State.State == job.StateUnset {
		// A never-run job cannot be settled — Outcome lives on the Attempt and
		// there is none, so Finish returns ErrNoOpenAttempt. Cancelling a
		// queued job removes it from the queue, which is what a user means.
		// discard is Half B2's, with the store; naming it here keeps the case
		// from being silently unhandled.
		return nil
	}
	if s.State.Outcome.IsSettled() {
		return nil // already closed, by cancel or otherwise
	}
	if q.running(j.ID(), s) {
		// A worker owns this job's resources and is using them. Neither arm
		// may seize a lease or slot out from under it.
		if job.IsProduction(s.State.State) {
			return nil // gate: let it reach the end; D-I11 lets it complete OK
		}
		q.work.Abort(j.ID()) // interrupt: settled on the tick after it yields
		return nil
	}
	l, err := j.Finish(job.OutcomeCancelled, q.now())
	// Freed BEFORE the reclaim, and unconditionally. reclaim can fail its
	// identity audit, and the earlier order returned through that failure with
	// the slot still held — turning one audit error into a permanent pool-B
	// leak. Nothing here depends on the reclaim succeeding.
	q.releaseFor(j, job.StateUnset)
	if rerr := q.reclaim(l); rerr != nil {
		return rerr
	}
	return err
}
```

- [ ] **Step 3: Write the setup helper this task uses**

```go
// mustHoldAt puts j at `at`, holding everything that position requires, using
// the job doors and the pools DIRECTLY rather than the scheduler.
//
// Deliberately not driven through Advance. Cancel is a decision about a job's
// current holdings, and a helper that reached those holdings by running the
// scheduler would make every test here also a test of Advance — so a bug in
// Advance would show up as a cancel failure. It also lets this task land
// before Advance exists, which is what keeps each task independently
// compilable.
func mustHoldAt(t *testing.T, q *Queue, j *job.Job, at job.State) {
	t.Helper()
	if err := j.BeginAttempt(testClock()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	route := map[job.State][]job.State{
		job.Fetching:  {},
		job.Assessing: {job.Assessing},
		job.Repairing: {job.Assessing, job.Repairing},
	}
	steps, ok := route[at]
	if !ok {
		t.Fatalf("mustHoldAt has no route to %v; add one rather than reaching past the doors", at)
	}
	for _, to := range steps {
		if err := j.SetNext(to); err != nil {
			t.Fatalf("SetNext(%v): %v", to, err)
		}
		if err := j.Transition(to); err != nil {
			t.Fatalf("Transition(%v): %v", to, err)
		}
	}
	if needsLease(at) {
		l := q.leases.issue()
		if l == nil {
			t.Fatal("pool A had no capacity for the fixture")
		}
		if err := j.Grant(l); err != nil {
			t.Fatalf("Grant: %v", err)
		}
	}
	if needsSlot(at) && !q.slots.acquire(j.ID()) {
		t.Fatal("pool B had no capacity for the fixture")
	}
}
```

- [ ] **Step 4: Run, then mutate the gate**

Run: `go test -race -count=1 ./internal/sched/` → PASS.

Mutate `if q.running(j.ID(), s)` to `if s.State.Next == job.StateUnset` — revision 3's `!workDone` proxy. Expected: `TestCancel_PostBoundaryNotRunningSettlesAtOnce` red, because `Extracting{next: unset}` gates forever. Restore and record.

- [ ] **Step 5: Gates and commit**

```bash
git add internal/sched/cancel.go internal/sched/cancel_test.go
git commit  # feat(sched): add Cancel with the running-based post-boundary gate
```

---

## Task 7: `grantFor`, `park`, and `advance`

**Files:**
- Create: `internal/sched/advance.go`, `internal/sched/advance_test.go`

**Interfaces:**
- Consumes: Tasks 4 and 5; `finishCancel` from Task 6
- Produces: `(*Queue).grantFor(j *job.Job, s job.State) bool`, `(*Queue).park(j *job.Job) error`, `(*Queue).Advance(j *job.Job) error`, `(*Queue).Retry(j *job.Job) error`
- Consumes additionally: `(*Queue).releaseFor` and `(*Queue).reclaim` (Task 5)

`Advance` is exported because it is the scheduling loop's entry point; the predicates stay unexported.

- [ ] **Step 1: Write the failing branch tests**

```go
// TestAdvance_BranchOne_StartsANeverRunJob covers §3.6 branch 1. No lease is
// taken here — D-I12 decoupled opening an attempt from holding one, so a retry
// can never fail for want of capacity.
func TestAdvance_BranchOne_StartsANeverRunJob(t *testing.T) {
	q := New(0, 0, testClock, &stubWorkers{}) // no capacity at all
	j := job.New("j1", "n", job.Policy{})
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if !j.HasRun() {
		t.Error("a never-run job was not started; BeginAttempt needs no lease (D-I12)")
	}
}

// TestAdvance_NeverReopensASettledAttempt covers §3.6's settled early return.
// Retry is an explicit user action, not something a scheduling tick decides.
func TestAdvance_NeverReopensASettledAttempt(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceToSettled(t, q, j, job.OutcomeFailed)
	before := j.Attempts()
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if j.Attempts() != before {
		t.Errorf("Advance opened attempt %d on a settled job; retry is q.Retry's job", j.Attempts())
	}
}

// TestAdvance_BranchThree_CrossingReclaimsTheLease covers §3.6 branch 3 and is
// the case §3.9's table calls out: the lease must come back at the crossing,
// and grantFor's result is deliberately ignored because the decision was
// already recorded in next.
func TestAdvance_BranchThree_CrossingReclaimsTheLease(t *testing.T) {
	q := New(1, 0, testClock, &stubWorkers{}) // one lease, NO slots
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)
	if err := j.SetNext(job.Extracting); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if got := j.Snapshot().State.State; got != job.Extracting {
		t.Errorf("State = %v, want Extracting — crossing does not wait for a slot", got)
	}
	if q.leases.outstanding() != 0 {
		t.Errorf("leases outstanding = %d after a crossing, want 0", q.leases.outstanding())
	}
}

// TestAdvance_ParkingAGatedJobReturnsItsLease is §3.8's deadlock, pinned. A
// `return nil` that merely declines to move leaves a paused job holding a
// pool-A lease forever.
func TestAdvance_ParkingAGatedJobReturnsItsLease(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
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
	if j.HoldsLease() {
		t.Error("a gated job still holds its lease; §3.8 calls that a deadlock")
	}
	if q.leases.outstanding() != 0 {
		t.Errorf("leases outstanding = %d after parking, want 0", q.leases.outstanding())
	}
	// Pool B is the half §3.8's argument never mentions, and the half an
	// earlier draft leaked: Assessing holds a slot, and parking returned only
	// the lease. A paused job occupying a compute slot is the same deadlock
	// wearing the other pool's name.
	if q.slots.outstanding() != 0 {
		t.Errorf("slots outstanding = %d after parking, want 0", q.slots.outstanding())
	}
}

// TestAdvance_DemotionReleasesTheSlot pins the one demotion in the work spine.
// Assessing → Fetching is legal (par2 decided more blocks are needed) and it
// moves the job from a state that needs a slot to one that does not: §3.4 gives
// Fetching no slot because it is network-bound.
//
// grantFor cannot catch this. It acquires what the DESTINATION requires and is
// silent about what the ORIGIN held, so the job kept a pool-B slot for the
// whole download — minutes to hours — with every test green.
func TestAdvance_DemotionReleasesTheSlot(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)
	if !q.slots.holds(j.ID()) {
		t.Fatal("fixture holds no slot at Assessing; this test cannot observe the leak")
	}
	if err := j.SetNext(job.Fetching); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if got := j.Snapshot().State.State; got != job.Fetching {
		t.Fatalf("State = %v, want Fetching; the demotion did not happen and the "+
			"slot assertion below would pass for the wrong reason", got)
	}
	if q.slots.holds(j.ID()) {
		t.Error("still holds a compute slot in Fetching; §3.4 gives Fetching none")
	}
	if !j.HoldsLease() {
		t.Error("lost its lease on a demotion; Fetching needs one and the job had it")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -count=1 -run TestAdvance ./internal/sched/`
Expected: build failure — `undefined: q.Advance`.

- [ ] **Step 3: Write `advance.go`**

```go
package sched

import "github.com/hobeone/gonzbd/internal/job"

// park releases what a gated job must not keep holding. Both gated paths go
// through it.
//
// It returns an error where spec §3.6 shows it returning nothing. That is a
// deliberate divergence and it SUPERSEDES a specific spec decision: §10's
// revision history records "park returned an error that is always nil →
// returns nothing", so the signature was narrowed once, on the grounds that
// the error could not carry information. That reasoning no longer holds. Task 4
// gave reclaim an identity audit, so it now fails on a lease this pool did not
// issue or already took back — a real condition, and one whose only other
// outlet would be silence.
func (q *Queue) park(j *job.Job) error {
	q.releaseFor(j, job.StateUnset)
	return q.reclaim(j.Surrender())
}

// grantFor acquires what s requires and j does not already hold. Acquisition
// happens ONLY here.
//
// It grants the lease before the slot and does not roll back a lease it took
// when the slot is unavailable: the job keeps the lease and waits, which is
// what §3.4 means by "a job re-entering Fetching still holds its lease, so it
// is running by construction". Rolling back would make a job that is one slot
// short give up capacity it will need again on the next tick.
func (q *Queue) grantFor(j *job.Job, s job.State) bool {
	if needsLease(s) && !j.HoldsLease() {
		l := q.leases.issue()
		if l == nil {
			return false
		}
		if err := j.Grant(l); err != nil {
			// Grant refuses only nil, an unidentified lease, or a second one.
			// The pool never issues the first two, so this is the third: the
			// job acquired a lease between our HoldsLease check and here.
			// Return the one we minted rather than leaking it.
			_ = q.reclaim(l)
			return false
		}
	}
	if needsSlot(s) && !q.slots.acquire(j.ID()) {
		return false
	}
	return true
}

// Retry reopens a settled attempt. It is the door spec §5.9 and §5.10 name in
// their traces (`q.Retry(j) → BeginAttempt(now)`), and two comments in this
// package already point callers at it; without it those traces have no entry
// point and scenarios 5.9 and 5.10 cannot be written.
//
// It is deliberately NOT something Advance decides. A scheduling tick must
// never resurrect a job the user has not asked to resurrect, which is why
// Advance returns early on a settled attempt instead.
//
// It takes NO lease. BeginAttempt needs none (D-I12), and demanding one is the
// exact defect revision 3 shipped: §5.9 records a retry dropped permanently
// because the lease could not be taken and nothing recorded that a retry was
// wanted. Branch 2 grants on a later tick.
func (q *Queue) Retry(j *job.Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return j.BeginAttempt(q.now())
}

// Advance is the scheduling loop's entry point for one job. It writes no job
// state on any blocked path, so a lost acquisition race costs a tick, never a
// verdict. It takes no target — the target is next, written by the worker that
// finished the state.
func (q *Queue) Advance(j *job.Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	s := j.Snapshot()
	if s.Intent == job.IntentCancel {
		return q.finishCancel(j, s)
	}

	// 1. Never run: start it, if permitted. No lease is needed or taken here;
	//    branch 2 grants it, exactly as for a paused or restarted job.
	if s.State.State == job.StateUnset {
		if _, gated := q.gatedBy(s); gated {
			return nil
		}
		return j.BeginAttempt(q.now())
	}
	// A SETTLED attempt is never reopened here. Retry is an explicit user
	// action, not something a scheduling tick decides — q.Retry is the door.
	if s.State.Outcome.IsSettled() {
		// A settled attempt KEEPS the position it settled at (§3.3) but needs
		// none of that position's resources. Without this, a job that settles
		// in Assessing, Repairing, Extracting or Finalizing holds pool-B
		// capacity forever: no other path frees it, because every other
		// release is on a path a settled job never takes again.
		q.releaseFor(j, job.StateUnset)
		return nil
	}
	// 2. Current state's work is unfinished: make it runnable. This is the
	//    resume path AND the restart path — they are the same path.
	if s.State.Next == job.StateUnset {
		if q.holds(j.ID(), s) {
			return nil // already working; never take resources from a live worker
		}
		if _, gated := q.gatedBy(s); gated {
			return q.park(j)
		}
		q.grantFor(j, s.State.State)
		return nil
	}
	// 3. Work is finished: move.
	if _, gated := q.gatedBy(s); gated {
		return q.park(j)
	}
	if job.IsCorrectness(s.State.State) && job.IsProduction(s.State.Next) {
		l, err := j.Cross(s.State.Next)
		if err != nil {
			return err
		}
		if err := q.reclaim(l); err != nil {
			return err
		}
		// Deliberately ignoring the result: the decision was already recorded
		// in next, crossing only ADDS pool-A capacity, and a job that crosses
		// and then fails to get a slot is simply not running until it does.
		// Branch 2 grants it on a later tick. It cannot go back.
		q.grantFor(j, s.State.Next)
		return nil
	}
	if !q.grantFor(j, s.State.Next) {
		return nil
	}
	if err := j.Transition(s.State.Next); err != nil {
		return err
	}
	// A DEMOTION frees what the new position does not need. Assessing →
	// Fetching is the live case and the only one in the work spine: Assessing
	// holds a slot, Fetching is network-bound and holds none (§3.4), so
	// without this the job downloads for minutes or hours while occupying
	// pool B. Released AFTER the move, so a refused Transition cannot leave
	// the job resourceless at the position it is still occupying.
	q.releaseFor(j, s.State.Next)
	return nil
}
```

- [ ] **Step 4: Write the helpers the tests use**

In `advance_test.go`:

```go
// mustAdvanceTo drives j through the real doors until it sits at want, using
// the Queue rather than constructing state, so a configuration this helper
// cannot reach is one the machine cannot reach.
//
// The scheduler-driven counterpart to Task 6's mustHoldAt, which reaches the
// same positions through the job doors and the pools directly. Both exist on
// purpose: cancel is a decision about holdings and must not depend on Advance,
// while the scenarios in Task 8 are about the scheduler and should go through it.
func mustAdvanceTo(t *testing.T, q *Queue, j *job.Job, want job.State) {
	t.Helper()
	if err := q.Advance(j); err != nil { // branch 1: opens the attempt
		t.Fatalf("Advance (begin): %v", err)
	}
	if err := q.Advance(j); err != nil { // branch 2: grants for Fetching
		t.Fatalf("Advance (grant): %v", err)
	}
	for j.Snapshot().State.State != want {
		from := j.Snapshot().State.State
		next := map[job.State]job.State{job.Fetching: job.Assessing, job.Assessing: job.Repairing}[from]
		if next == job.StateUnset {
			t.Fatalf("mustAdvanceTo has no route from %v to %v", from, want)
		}
		if err := j.SetNext(next); err != nil {
			t.Fatalf("SetNext(%v): %v", next, err)
		}
		if err := q.Advance(j); err != nil {
			t.Fatalf("Advance (%v → %v): %v", from, next, err)
		}
	}
}

func mustAdvanceToSettled(t *testing.T, q *Queue, j *job.Job, o job.Outcome) {
	t.Helper()
	mustAdvanceTo(t, q, j, job.Fetching)
	l, err := j.Finish(o, testClock())
	if err != nil {
		t.Fatalf("Finish(%v): %v", o, err)
	}
	if err := q.reclaim(l); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
}
```

- [ ] **Step 5: Run and mutate**

Run: `go test -race -count=1 ./internal/sched/` → PASS.

Mutations, one at a time:
1. Replace `return q.park(j)` in branch 2 with `return nil` → expect `TestAdvance_ParkingAGatedJobReturnsItsLease` red.
2. Delete the `q.holds(...)` early return in branch 2 → expect a gated-but-working job to lose its lease; if no test goes red, **add one** rather than accepting the pass, because §3.6 calls that order deliberate.
3. Move `q.grantFor(j, s.State.Next)` before `j.Cross(...)` in branch 3 → expect a slot-starved job to stall at `Assessing`, failing `TestAdvance_BranchThree_CrossingReclaimsTheLease`.

Record every message.

- [ ] **Step 6: Gates and commit**

```bash
git add internal/sched/advance.go internal/sched/advance_test.go
git commit  # feat(sched): add advance, grantFor and park
```

---

## Task 8: The scenario suite and the two-pool exit walk

**Files:**
- Create: `internal/sched/scenario_test.go`

This is spec §6 test 4 ("each scenario in §5 is a test") and test 4b ("no path settles or crosses without reclaiming the lease"). §5.1, 5.4, 5.5 and 5.7 pin revision 2's defects; 5.8 through 5.13 pin revision 3's.

- [ ] **Step 1: Write the exit walk**

```go
// TestBothPoolsAreAccountedAtEveryExit is spec §6 test 4b — the only test that
// would have caught all five of revision 3's leaks at once.
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
//     correct behaviour as a leak in three of these six rows.
//
// The lesson generalises past this test: a resource with a handback has a
// conservation law, and a resource that is merely occupied has a state-matching
// rule. They are not the same claim and one walk must not assert both. This is checkable only because leasePool audits identity
// (Task 4): job.Cross and job.Finish return a *Lease a caller can silently
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
	}
	// wantsSlot is what pool-B occupancy SHOULD be for j right now: a job
	// holds a compute slot exactly while its CURRENT position requires one and
	// it is neither settled nor gated. Those are the three conditions
	// releaseFor's four call sites exist to maintain, stated once as a
	// predicate so the walk asserts the rule rather than six memorised numbers.
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

	// The count is asserted so that adding a row to §3.9's table without
	// adding a case here fails, rather than quietly narrowing the walk.
	if len(exits) != 6 {
		t.Fatalf("§3.9's exit table has 6 rows; this walk has %d", len(exits))
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
			// occupancy returns to zero fails three of these six rows and
			// calls correct behaviour a leak.
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
```

- [ ] **Step 2: Run it**

Run: `go test -race -count=1 -run TestBothPoolsAreAccounted ./internal/sched/`
Expected: PASS.

- [ ] **Step 3: Mutate `Advance` to drop a lease, and confirm the walk catches it**

In `advance.go` branch 3, change `l, err := j.Cross(...)` / `q.reclaim(l)` to `_, err := j.Cross(...)` — the exact caller mistake no linter sees.
Expected: FAIL on `cross into Production` with `pool-A occupancy 1 → 1` versus a start of 0… **run it and record the actual message**; if it does not fail, the walk's starting baseline is wrong and must be taken before the job acquires anything. Restore from your own copy.

- [ ] **Step 3b: Mutate each of the three slot releases, one at a time**

Pool B needs its own mutations, and they are the reason this step exists: every
one of these leaks was present in a draft of this plan while the whole suite
was green, because the walk covered pool A alone.

Neuter each `q.releaseFor(...)` call by making the guard unreachable — change
`if !needsSlot(s)` in `releaseFor` to `if false` for the whole-owner mutation,
and comment out individual call sites for the per-site ones. **Do not delete
the function**: an unused-parameter or unused-import failure is a build error,
and a build failure is not an observation.

| Mutation | Expect red in |
|---|---|
| `releaseFor` body → `if false` | the walk's `settle …`, `cancel` and `pause` rows — the ones that should FREE a slot. The two `cross` rows still pass: they end holding one legitimately, which is the asymmetry the oracle encodes. Plus `TestCancel_ReleasesTheComputeSlot`. |
| drop the call in `park` | the walk's `pause` row |
| drop the call in `Advance`'s settled branch | the walk's `settle …` rows |
| drop the call after the demotion `Transition` | `TestAdvance_DemotionReleasesTheSlot` |
| drop the call in `finishCancel` | `TestCancel_ReleasesTheComputeSlot` and the walk's `cancel` row |

Record each observed message. If the demotion mutation does not go red, the
demotion test is not reaching `Assessing → Fetching` — assert the position
before and after, rather than weakening the test.

- [ ] **Step 4: Write the remaining §5 scenarios**

One test per scenario, named `TestScenario_5_1_PauseMidDownloadThenResume` and so on through `5_13`. Each is a transcript of the spec's trace: drive the doors, assert the position, the holdings and the rendered status at each line. Assert the scenario count matches:

```go
// TestEveryScenarioHasATest fails when §5 grows a scenario nobody pinned.
// §5 is the regression suite for four revisions of defects; a scenario without
// a test is a defect class with nothing watching it.
func TestEveryScenarioHasATest(t *testing.T) {
	const scenariosInSpec = 13
	names := scenarioTestNames(t) // parses this file for TestScenario_ funcs
	if len(names) != scenariosInSpec {
		t.Errorf("§5 has %d scenarios, this file has %d tests: %v", scenariosInSpec, len(names), names)
	}
}
```

```go
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
```

The same technique as `constantsOfType`, for the same reason.

- [ ] **Step 5: Gates and commit**

```bash
git add internal/sched/scenario_test.go
git commit  # test(sched): pin every §5 scenario and the two-pool exit walk
```

---

## Task 9: Documentation sweep

**Files:**
- Modify: `docs/superpowers/specs/2026-08-26-lifecycle-intents-design.md` — §8 open questions 1 and 2
- Modify: `internal/job/doc.go`
- Create: `docs/queue-lifecycle.md` section, or amend the existing one if it covers `internal/sched`

- [ ] **Step 1: Answer the open questions this plan settled**

§8 Q2 ("where do the per-state resource requirements live") — answered: `internal/sched/requirements.go`, because only the Queue consumes them. Record the answer and the reason in the spec.

§8 Q1 ("can a `Finished` job be told retryable from outside") — the premise is gone; `finish` no longer erases `state`, and there is no `Finished`. A settled attempt keeps its position, so `StateView.State` already answers it. Record that the question dissolved rather than deleting it.

- [ ] **Step 2: Sweep the claims this plan falsified**

From the repository root, not from the files you edited:

```bash
git grep -n 'zero-size\|&Lease{}'
git grep -n 'two ad-hoc\|two guards\|hand-written guard'
git grep -n 'State()\|HasRun()\|HoldsLease()'   # comments claiming these compose safely
git grep -n 'Half B'                             # scope claims this plan narrows to B1
```

Read `docs/ARCHITECTURE.md` and `docs/queue-lifecycle.md` **in full** at the end, rather than grepping them — a doc restates a claim in prose and shares no token with the code.

- [ ] **Step 3: Run `pr-review-toolkit:comment-analyzer` over the cumulative diff, then commit**

```bash
git add docs/ internal/job/doc.go
git commit  # docs: record where the resource table lives and retire two open questions
```

---

## Adjacent work this plan does not do

Named so a reviewer can argue for pulling one in, rather than finding it missing.

- **A citation checker.** Sixteen comments in `internal/job` embed a literal `git grep` and a stated count, and that class produced 7 of 22 findings. Every enumeration that became a test has stayed true; every one that stayed prose has gone stale at least once. A `scripts/check_citations` that extracts and runs them would retire the class outright. It is tooling, not Half B, and belongs in its own change.
- **`q.discard`** for cancelling a never-run job — needs the store, which is B2.
- **The dispatcher's `yielded` report** (§3.6). `Fetching` gates per-article, so its worker stops without its work having ended; that yield needs a caller for `q.park(j)`. B1 defines `Workers.Abort`; B2 adds the yield path.
- **Persistence** of `State`, `Outcome` and `Intent` (spec §3.8), and **reorder** (§4.7).
- **`ToSABnzbd`'s composed view** — it cannot compute its new inputs until the Queue exists to supply running-ness and `WaitReason`.
- **A `Queue.Finish` door**, settling a non-cancel outcome and freeing both pools in one call. B1 releases the slot on settlement from a SWEEPER — `Advance`'s settled branch — rather than at the moment of settlement, because the worker calls `j.Finish` directly and that caller is B2's. The sweeper is correct (release is idempotent, and every settled job is advanced again) but it is a poll where an event would do, and it is visible in the exit walk: the `settle Unrecoverable` row needs an explicit `q.Advance` tick before the slot comes back. `finishCancel` already shows the shape a `Queue.Finish` would take. Deferred rather than built because it changes the interface B2's dispatcher writes against, which is a decision to make with B2's caller in view, not ahead of it.

---

## Review guidance

Reviewers of this plan should test it against the evidence in "Why this plan is ordered this way", and specifically:

**D1, D2 and D3 were the three open decisions. The #445 review answered all
three**, and the answers are recorded here rather than left open:

1. **D1 — `internal/sched` is the right home.** Affirmed. `internal/queue` is
   27k lines and already declares `Queue`, `Job` and `ActiveSet`; wedging the
   decision core in collides on symbols and entangles pure scheduling with
   SQLite persistence and byte tracking. No dissent was offered.
2. **D2 — `OutcomeCancelled` IS admissible in Production.** Affirmed, on §5.12's
   forcing argument: a post-boundary job restored from a restart holds nothing,
   so `running(j)` is false and there is no worker to protect. Refusing the cell
   would leave exactly those jobs unsettleable.
3. **D3 — the lease audit was strong enough; the SLOT had no audit at all.**
   This is the one that changed the plan. The answer to "is the lease audit
   enough" was yes, and the question was aimed at the wrong pool — see "What the
   RFC review round changed". Pool B now has `releaseFor` as its owner and its
   own half of the exit walk — a state-matching rule, not a conservation law,
   which took a second round to state correctly.

What is still worth attacking:

4. **Does Task 3's `Snapshot` actually close the tear?** The test races the doors and asserts internal consistency. If there is a composite the Queue asks that a `Snapshot` cannot answer, it is a gap.
5. **Is anything in "Adjacent work" actually load-bearing for B1?** `scripts/check_citations` has since found four bad section references in this plan and its spec, which is an argument for promoting it.
6. **Is `releaseFor` called everywhere a position stops requiring a slot?** It has four call sites. The population is enumerable — `grep -n 'releaseFor'` — and the demotion case proves the class is easy to miss. A fifth path that changes or abandons a position without going through `park`, `finishCancel`, the settled branch or `Transition` would leak exactly as the first three did.

Every task's mutation steps are the plan's own red-green evidence. A reviewer who thinks a mutation would not go red is pointing at a test that is not pinning what it claims.
