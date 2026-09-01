# Download-Stamp Owner Implementation Plan

> **Executed and complete — this is a frozen record, not open work.** All seven tasks landed on
> the `fix-464-timestamp-encoding` branch. The document stays in the future tense throughout,
> including the Rule 4 sweep list below, which reads as an open TODO and is not one. It is left
> uncorrected for the same reason the b24a plan's stale writers table is: a plan records what was
> believed before the work, and rewriting it to match the outcome deletes the reasoning that
> justified the approach — including the three defects successive review rounds removed from it.
> Where the plan and the code disagree, the code is right. The writer population it argues for is
> enforced by `TestDownloadStampWriters_MatchTheEnumerationStatedInProse`, not by this file.
>
> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make it impossible for `JobProgress.downloadStarted`/`downloadFinished` to hold an
instant that the store cannot distinguish from "absent", by giving the two fields a single owner,
and collapse the duplicated encode/decode of those fields into one codec pair.

**Architecture:** Four private methods on `*JobProgress` become the only code that assigns either
field by name. Every existing writer routes through them. The validity rule is stated once, in the
owner, rather than as a clause appended to each caller's sentinel test. No schema migration: once
no writer can produce a value that encodes to `0`, the store's in-band `0`-means-absent sentinel is
unambiguous again.

**Tech Stack:** Go 1.27, `internal/queue`, `database/sql` + `modernc.org/sqlite`, `go/ast` for the
enumeration pin test.

**Spec:** [issue #464](https://github.com/hobeone/gonzbd/issues/464), plus its
[premise audit](https://github.com/hobeone/gonzbd/issues/464#issuecomment-5489162055),
[solution-space comparison](https://github.com/hobeone/gonzbd/issues/464#issuecomment-5489176206)
and [Gate 1 decision](https://github.com/hobeone/gonzbd/issues/464#issuecomment-5489212850).

## Global Constraints

- **The validity rule is `t.Unix() > 0`, not `t.After(epoch)`.** The two are not equivalent and the
  difference is the bug: `time.Unix(0, 5e8).After(time.Unix(0,0))` is true, but its `.Unix()` is
  `0`, which is the wire value for absent. A bound the encoding cannot express is not a bound.
  `t.Unix() > 0` also rejects `time.Time{}` (whose `.Unix()` is -62135596800), so it subsumes the
  existing `IsZero()` clause rather than being appended to it, and it matches the decode side's
  `unix <= 0` exactly.
- **The domain rule is a user decision, not an inference.** "No job will have a pre-1970 timestamp"
  was ratified at Gate 1. Say so where the rule is stated; do not present it as derived.
- **No goose migration.** Declined under Standing Rule 1 — see the Gate 1 comment. Do not add one,
  and do not edit `internal/history/migrations/001_initial.sql`.
- **No change to any `!IsZero()` consumer.** Four sites read these fields under that gate —
  `internal/app/history_helper.go:55-58`, `internal/app/app.go:1725-1726`,
  `internal/postproc/filelist.go:65-66` and `internal/postproc/postproc.go:316-317`. All four become
  correct once the owner exists, and changing any of them is the defence-in-depth Rule 1 declines.
  The list is stated in full because naming one site made an earlier draft read as though it were
  the only one.
- **`internal/queue` has two test packages.** `progress_test.go`, `sqlite_store_internal_test.go`
  and `lifecycle_test.go` are `package queue`; `sqlite_store_test.go` is `package queue_test` and
  cannot see unexported identifiers (it says so itself at `:604`). Any test calling `isJobStamp`,
  `encodeJobStamp`, `decodeJobStamp` or the owner methods must live in a `package queue` file.
- **Existing coverage is checked before a test is written, every time.** Round 1 of this plan's
  review removed two invented tests that duplicated `TestSetPostProcStarted`; round 2 found the
  same defect again in another task, against `TestSQLiteStore_DownloadTimestampsPersistence`
  (`sqlite_store_test.go:812`), which already round-trips both stamps and passes today. Grep for
  the behaviour before adding a test, not after.
- **Rule 4 sweep list — this change falsifies claims in six places, not two.** Task 7 owns the
  sweep, but every task that falsifies one should note it:
  1. `internal/queue/job.go` — `markStartedOnce` and `markDownloadFinishedOnce` doc comments.
  2. `internal/queue/lifecycle_test.go:696-706` — a **live `check_citations` citation** stating the
     writer grep "returns 5 writers" and enumerating them as "both IsZero-guarded… plus
     ResetForRetry clearing it and two restore paths". Every clause is falsified. **Re-run the
     command and record what it returns — do not carry a predicted number into the comment.** The
     grep is on `downloadFinished` alone, so `setDownloadStartedOnce` does not appear in it; an
     earlier draft of this plan asserted "4" without running it, which is the same unenumerated
     count the rule forbids.
  3. `internal/queue/lifecycle_test.go:568-575` — names the literal assignment in `queue.go` that
     Task 3 deletes.
  4. `internal/queue/lifecycle_test.go:600-604` and `:611-617` — reason about "the IsZero guard"
     and "the two writers" at a site that will have neither.
  5. `internal/queue/job_test.go:606-627` — "the zero value is the sentinel these methods test
     against, so it is the one argument they cannot store" — after Task 2 they refuse a half-line.
  6. `AGENTS.md:378-382` — the worked `scripts/mutate` example anchors on
     `if job.progress.downloadFinished.IsZero() {`, the exact line Task 3 deletes. Judgement call
     (it is illustrative), but it is the file's own worked example and it will stop applying.
- **Red-green is observed, not reasoned.** Every behavioural pin gets a `scripts/mutate` spec run
  with `go run ./scripts/mutate <spec>`, and each mutation must be reported KILLED. Specs live in
  `internal/queue/testdata/`; the only prior art is `scripts/mutate/testdata/self.spec`, so this
  establishes the beside-the-package convention — say so in the first commit that adds one.
- **`check_citations` scans tracked files**, so run it after `git add`, not before.
- After editing any `.go` file: `goimports -w <file>`, `go fix ./...`, `go build ./...`.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/queue/progress.go` | **New:** `isJobStamp` and the four owner methods. `UnmarshalJSON` routes through them. |
| `internal/queue/job.go` | `markStartedOnce`, `markDownloadFinishedOnce`, `ResetForRetry` delegate. Doc comments restated. |
| `internal/queue/queue.go` | `SetPostProcStarted` delegates instead of re-implementing first-wins. |
| `internal/queue/sqlite_store.go` | **New:** `encodeJobStamp`/`decodeJobStamp`. Three sites collapse; the `Get` decode routes through the owner. |
| `internal/queue/stamp_enumeration_test.go` | **New:** pin test asserting the owner is the only by-name writer. |

**Task order is load-bearing.** Task 6's enumeration test asserts an exact writer set, so it must
run after every writer has been routed — including `SQLiteStore.Get`, which Task 4 handles. An
earlier draft placed the pin before the store routing and would have failed at its own commit.

---

### Task 1: The owner and its validity rule

**Files:**
- Modify: `internal/queue/progress.go` (add after the `DownloadFinished` accessor, ~:490)
- Test: `internal/queue/progress_test.go`

**Interfaces:**
- Produces: `isJobStamp(time.Time) bool`,
  `(*JobProgress).setDownloadStartedOnce(time.Time) bool`,
  `(*JobProgress).setDownloadFinishedOnce(time.Time) bool`,
  `(*JobProgress).clearDownloadStamps()`,
  `(*JobProgress).restoreDownloadStamps(started, finished time.Time)`
- Consumes: nothing.

**Why four methods and not three.** `restoreDownloadStamps(time.Time{}, time.Time{})` is
behaviourally identical to `clearDownloadStamps()`, so the fourth is a degenerate case of the
third. It is kept because the two call sites mean different things — a retry *reopens* slots it
intends to re-win, a load *installs* slots already won in another run — and a reader of
`ResetForRetry` should not have to evaluate `isJobStamp(time.Time{})` to see that the line clears.
That is a readability argument, not a correctness one; if a later change makes the pair awkward,
collapsing to three is sanctioned. `clearDownloadStamps` is the shared implementation, so the two
do not duplicate their zeroing.

- [ ] **Step 1: Write the failing test**

```go
func TestIsJobStamp_MatchesTheWireForm(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   time.Time
		want bool
	}{
		{"zero value", time.Time{}, false},
		{"the epoch itself", time.Unix(0, 0), false},
		{"one second before the epoch", time.Unix(-1, 0), false},
		// The case that makes this predicate Unix()-based rather than
		// After(epoch)-based: it is after the epoch, and it still encodes
		// to the integer 0 that means absent.
		{"half a second after the epoch", time.Unix(0, 500000000), false},
		{"one second after the epoch", time.Unix(1, 0), true},
		{"a plausible now", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isJobStamp(tc.in); got != tc.want {
				t.Errorf("isJobStamp(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
```

The two setters are table-driven, parameterised by **both** the setter and a field-specific getter.
That is what keeps the table honest: if `setDownloadFinishedOnce` were miswired to write
`downloadStarted`, the `"finished"` case's getter still catches it, exactly as two copied test
bodies would.

```go
func TestSetDownloadStampOnce_FirstWinsAndRefusesNonStamps(t *testing.T) {
	real := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		set  func(*JobProgress, time.Time) bool
		get  func(*JobProgress) time.Time
	}{
		{"started", (*JobProgress).setDownloadStartedOnce, (*JobProgress).DownloadStarted},
		{"finished", (*JobProgress).setDownloadFinishedOnce, (*JobProgress).DownloadFinished},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &JobProgress{}
			if !tc.set(p, real) {
				t.Fatal("first real stamp was refused")
			}
			if !tc.get(p).Equal(real) {
				t.Fatalf("stamp = %v, want %v", tc.get(p), real)
			}
			if tc.set(p, real.Add(time.Hour)) {
				t.Error("second stamp took; first-wins was not enforced")
			}
			if !tc.get(p).Equal(real) {
				t.Errorf("stamp moved to %v", tc.get(p))
			}

			// A refusal must not consume the slot.
			q := &JobProgress{}
			if tc.set(q, time.Unix(0, 0)) {
				t.Error("epoch zero was accepted")
			}
			if !tc.get(q).IsZero() {
				t.Errorf("epoch zero was stored: %v", tc.get(q))
			}
			if !tc.set(q, real) {
				t.Error("the refusal consumed the first-wins slot")
			}
		})
	}
}
```

Also pin the two methods the earlier draft left unpinned:

```go
func TestClearDownloadStamps_ClearsBothFields(t *testing.T) {
	real := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	p := &JobProgress{downloadStarted: real, downloadFinished: real.Add(time.Hour)}
	p.clearDownloadStamps()
	if !p.downloadStarted.IsZero() || !p.downloadFinished.IsZero() {
		t.Errorf("after clear: started=%v finished=%v, want both zero",
			p.downloadStarted, p.downloadFinished)
	}
}

func TestRestoreDownloadStamps_FiltersEachFieldIndependently(t *testing.T) {
	real := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                     string
		started, finished        time.Time
		wantStarted, wantFinished time.Time
	}{
		{"both real", real, real, real, real},
		{"started fails the rule", time.Unix(0, 0), real, time.Time{}, real},
		{"finished fails the rule", real, time.Unix(0, 0), real, time.Time{}},
		{"both fail", time.Unix(0, 0), time.Time{}, time.Time{}, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &JobProgress{}
			p.restoreDownloadStamps(tc.started, tc.finished)
			if !p.downloadStarted.Equal(tc.wantStarted) {
				t.Errorf("started = %v, want %v", p.downloadStarted, tc.wantStarted)
			}
			if !p.downloadFinished.Equal(tc.wantFinished) {
				t.Errorf("finished = %v, want %v", p.downloadFinished, tc.wantFinished)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -count=1 -run 'TestIsJobStamp|TestSetDownloadStamp|TestClearDownloadStamps|TestRestoreDownloadStamps' ./internal/queue/`
Expected: FAIL — `undefined: isJobStamp`.

- [ ] **Step 3: Write the implementation**

```go
// isJobStamp reports whether t may be stored as a download timestamp.
//
// The bound is expressed on t.Unix() rather than on t itself, and that is the
// whole point: the store encodes these fields with t.Unix() and reads 0 as
// "absent" (see encodeJobStamp in sqlite_store.go). A bound of
// t.After(time.Unix(0,0)) would admit the interval (epoch, epoch+1s), every
// member of which encodes to 0 and comes back as time.Time{} — the exact
// round-trip loss #464 reports, merely narrowed. Comparing on the encoded
// value makes the predicate and the wire form the same set by construction.
//
// It also subsumes the IsZero() test it replaces: time.Time{} is year 1, whose
// Unix() is -62135596800. Callers therefore need one predicate, not two.
//
// That no job timestamp is at or before the Unix epoch is a decision settled on
// #464, not something derived from the code. Every production path stamps from
// time.Now(), so a value failing this test is a programming error, not data.
func isJobStamp(t time.Time) bool { return t.Unix() > 0 }

// setDownloadStartedOnce records the download start, reporting whether it took.
// A later call is a no-op: first start wins.
//
// This and its three siblings below WILL BE the only functions in this
// package's non-test sources that assign p.downloadStarted or
// p.downloadFinished by name, once #464's tasks 2-4 route the five remaining
// writers in job.go, queue.go and sqlite_store.go through them.
//
// Write the sentence in that tense until those tasks land, and tighten it to
// the present tense in the same commit that adds
// TestDownloadStampWriters_MatchTheEnumerationStatedInProse — which is what
// makes it checkable rather than asserted. A comment claiming sole ownership
// at THIS commit would be false, and no gate would catch it.
//
// Two exclusions the sentence above depends on, both deliberate:
//   - Test sources are not scanned. challenger_m3_test.go writes
//     snap.progress.downloadFinished directly, with its own comment defending
//     why, and that is a fixture reaching past the door on purpose.
//   - A whole-struct copy is invisible to the scan. clone does `cp := *p`,
//     which propagates whatever these four stored and mints no value of its own.
//
// Refusing a non-stamp does NOT consume the first-wins slot — a real timestamp
// arriving afterwards is still the first mark.
func (p *JobProgress) setDownloadStartedOnce(t time.Time) bool {
	if !isJobStamp(t) || !p.downloadStarted.IsZero() {
		return false
	}
	p.downloadStarted = t
	return true
}

// setDownloadFinishedOnce records the download completion, reporting whether it
// took. A later call is a no-op: first finish wins. See setDownloadStartedOnce
// for the ownership rule both obey.
func (p *JobProgress) setDownloadFinishedOnce(t time.Time) bool {
	if !isJobStamp(t) || !p.downloadFinished.IsZero() {
		return false
	}
	p.downloadFinished = t
	return true
}

// clearDownloadStamps reopens both first-wins slots. Retry is the only caller:
// a re-download legitimately re-stamps.
func (p *JobProgress) clearDownloadStamps() {
	p.downloadStarted = time.Time{}
	p.downloadFinished = time.Time{}
}

// restoreDownloadStamps installs stamps read back from persistence, bypassing
// first-wins because a restore is not a mark — the slots it fills were already
// won in the run that wrote them.
//
// It still applies isJobStamp, so a stamp that fails the rule is dropped rather
// than restored. Under Standing Rule 1 no persisted row is owed compatibility,
// so this is not a migration path: it is the one place a value arrives from
// outside this process and the rule must be re-checked.
func (p *JobProgress) restoreDownloadStamps(started, finished time.Time) {
	p.clearDownloadStamps()
	if isJobStamp(started) {
		p.downloadStarted = started
	}
	if isJobStamp(finished) {
		p.downloadFinished = finished
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -count=1 -run 'TestIsJobStamp|TestSetDownloadStamp|TestClearDownloadStamps|TestRestoreDownloadStamps' ./internal/queue/`
Expected: PASS.

- [ ] **Step 5: Prove the pins discriminate**

Create `internal/queue/testdata/stamp_owner.spec`:

```text
pkg ./internal/queue/
run TestIsJobStamp|TestSetDownloadStamp|TestClearDownloadStamps|TestRestoreDownloadStamps

[the bound relaxed to After(epoch), admitting the sub-second interval]
file internal/queue/progress.go
--- anchor
func isJobStamp(t time.Time) bool { return t.Unix() > 0 }
--- replace
func isJobStamp(t time.Time) bool { return t.After(time.Unix(0, 0)) }
--- end

[the bound relaxed to the IsZero test it replaces]
file internal/queue/progress.go
--- anchor
func isJobStamp(t time.Time) bool { return t.Unix() > 0 }
--- replace
func isJobStamp(t time.Time) bool { return !t.IsZero() }
--- end

[first-wins neutered on started]
file internal/queue/progress.go
--- anchor
	if !isJobStamp(t) || !p.downloadStarted.IsZero() {
--- replace
	if !isJobStamp(t) {
--- end

[first-wins neutered on finished]
file internal/queue/progress.go
--- anchor
	if !isJobStamp(t) || !p.downloadFinished.IsZero() {
--- replace
	if !isJobStamp(t) {
--- end

[clear forgets the finished field]
file internal/queue/progress.go
--- anchor
func (p *JobProgress) clearDownloadStamps() {
	p.downloadStarted = time.Time{}
	p.downloadFinished = time.Time{}
--- replace
func (p *JobProgress) clearDownloadStamps() {
	p.downloadStarted = time.Time{}
--- end

[restore stops filtering the started field]
file internal/queue/progress.go
--- anchor
	if isJobStamp(started) {
		p.downloadStarted = started
	}
--- replace
	p.downloadStarted = started
--- end

[restore stops filtering the finished field]
file internal/queue/progress.go
--- anchor
	if isJobStamp(finished) {
		p.downloadFinished = finished
	}
--- replace
	p.downloadFinished = finished
--- end
```

The first mutation is the one that matters: it is the design error this plan's own review caught,
so a SURVIVED there means the sub-second case is not pinned.

**The test's third assertion — that a refusal leaves the slot open — is deliberately not mutated,
and this is a limitation worth stating rather than papering over.** An earlier draft added a
mutation that stored the bad value and then restored the zero value before returning false. That is
an *equivalent mutant*: the slot IS the field's zero-ness, so restoring the zero restores the slot,
and no input distinguishes it from the original. It could only ever report SURVIVED, which would
make Step 5's "every mutation KILLED" unsatisfiable. The assertion still earns its place as a
regression net; it simply is not independently pinnable by mutation, and saying so is more honest
than shipping a spec that cannot pass.

Run: `go run ./scripts/mutate internal/queue/testdata/stamp_owner.spec`
Expected: every mutation KILLED.

- [ ] **Step 6: Commit**

```bash
git add internal/queue/progress.go internal/queue/progress_test.go internal/queue/testdata/stamp_owner.spec
git commit -m "feat(queue): give the download timestamps a single owner"
```

---

### Task 2: Route the two mark methods through the owner

**Files:**
- Modify: `internal/queue/job.go:553-559` (`markStartedOnce`), `:598-605` (`markDownloadFinishedOnce`)
- Test: `internal/queue/job_test.go` — **extend `TestJobMarkOnce_RefusesAZeroTimestamp` at `:628`;
  do not add new tests.**

**Interfaces:** consumes Task 1's two setters. No signature change to either method.

- [ ] **Step 1: Extend the existing test, do not write a new one**

`TestJobMarkOnce_RefusesAZeroTimestamp` (`job_test.go:628`, from #462) is already a table over both
mark methods with per-method getters, asserting exactly the three properties this task needs:
refused, not stored, and the refusal does not consume the first-wins slot (`:648-673`). The only
thing it lacks is the bad value — it covers `time.Time{}` and this task widens the rule to an
interval.

Add a `bad time.Time` dimension rather than a second table:

```go
	for _, bad := range []struct {
		name string
		in   time.Time
	}{
		{"the zero value", time.Time{}},
		{"the epoch", time.Unix(0, 0)},
		{"half a second after the epoch", time.Unix(0, 500000000)},
	} {
```

The third case is the one that matters: it is the value `After(epoch)` would have admitted, and it
is the actual shape of #464 rather than the zero value #459 already closed.

**Nesting, which the fragment above does not show.** Put the `bad` loop *inside* the existing
`t.Run(tc.name, …)`, after its `t.Parallel()`, wrapping the body in a nested `t.Run(bad.name, …)`,
and substitute `bad.in` for the three `time.Time{}` literals at `job_test.go:649, 665, 671`.
**The fixture at `:647` — `j := &Job{ID: "j1", progress: &JobProgress{}}` — must move inside the
`bad` loop.** Left where it is, the second `bad` case starts against a job already holding `real`,
first-wins refuses, and `if !tc.mark(j, real)` Fatals at `:661`. It fails loudly rather than
silently, which is why it is worth one sentence rather than a debugging session.

Also reword the three assertion strings: they say "a zero timestamp", which will now fire for the
epoch and the sub-second case too. Name `bad.name` instead.

Renaming the test is optional, but if you do: `internal/queue/lifecycle_test.go:1227` refers to
`TestJobMarkOnce_RefusesAZeroTimestamp` by name, in a file this task does not stage, and Task 7's
greps do not reach it. Task 7 Step 1 carries a grep for the name — keep them together.

**This is the third time a draft of this plan proposed a test that already existed** — Task 3 in
round 1, Task 4 in round 2, this in round 3. Grep before writing; the constraint at the top of this
plan exists because stating it once did not work.

- [ ] **Step 2: Run to verify it fails**

Run: `go test -count=1 -run 'TestJobMarkOnce' ./internal/queue/`
Expected: FAIL on the two new `bad` cases, and PASS on the pre-existing zero-value case. **Record
the message the run actually prints; do not predict it here.** An earlier draft of this step quoted
a string that exists nowhere in the tree, left over from when this task wrote its own tests — the
existing assertions have their own wording (`job_test.go:650` onwards), which Step 1 also rewords.

- [ ] **Step 3: Write the implementation**

```go
func (j *Job) markStartedOnce(t time.Time) bool {
	return j.progress.setDownloadStartedOnce(t)
}
```

```go
func (j *Job) markDownloadFinishedOnce(t time.Time) bool {
	return j.progress.setDownloadFinishedOnce(t)
}
```

- [ ] **Step 4: Restate the doc comments (sweep items 1 and 5)**

Both comments make claims this task falsifies. Re-run each enumeration before rewriting, and state
the basis:

- `markStartedOnce`'s comment says a zero time "is not reachable today, and refusing it is
  hardening against a future caller rather than a live defect". The reachability half still holds —
  re-verify with `git grep -n 'MarkJobStarted(' -- '*.go' ':!*_test.go'` — but the sentence must now
  describe what is refused: everything whose `Unix()` is not positive, not only the zero value.
- `markDownloadFinishedOnce`'s comment says "There are already two [enforcement sites] — this
  method, and `Queue.SetPostProcStarted`… That second site is not this change's to remove, and
  since #457's review it is tested: `TestSetPostProcStarted`'s '…' subtest is what holds the two
  writers to the same rule." **Task 3 removes that second site — so this rewrite belongs in Task 3,
  not here.** Leave it alone in this task; a comment describing the post-Task-3 state, committed
  before Task 3, is false for exactly one commit and no gate catches it. Task 3 stages `job.go` for
  this reason. When rewriting there, keep the citation: `TestSetPostProcStarted` survives unchanged,
  so the subtest name stays valid, and it is what makes the invariant checkable.
- `internal/queue/job_test.go:606-627` says "the zero value is the sentinel these methods test
  against, so it is the one argument they cannot store". After this task they refuse an interval.
  Restate.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -count=1 -race ./internal/queue/`
Expected: PASS, including the pre-existing mark tests — delegation must not change behaviour for
real timestamps.

- [ ] **Step 6: Commit**

```bash
git add internal/queue/job.go internal/queue/job_test.go
git commit -m "fix(queue): refuse a stamp the store cannot distinguish from absent"
```

---

### Task 3: SetPostProcStarted delegates instead of re-implementing

**Files:**
- Modify: `internal/queue/queue.go:1354-1356`
- Test: none written or modified. `internal/queue/lifecycle_test.go:561`'s existing
  `TestSetPostProcStarted` is the regression net; this task adds no assertion to it (see Step 1).

**Interfaces:** consumes Task 1's `setDownloadFinishedOnce`.

This is the task the direction turned on. `SetPostProcStarted` contains its own copy of the
first-wins rule, and two independently-maintained copies of one invariant is the Rule 2 smell that
made a check insufficient here.

- [ ] **Step 1: Do NOT write new tests — the coverage exists**

`internal/queue/lifecycle_test.go:561`'s `TestSetPostProcStarted` already has four subtests,
including `"first call returns true and stamps the finish time"` (`:578`) and `"does not overwrite
a finish time MarkDownloadFinished already set"` (`:615`). Those are exactly the two properties
this task must preserve. An earlier draft of this plan proposed writing both again as new top-level
tests in the wrong file; that would have produced duplicate coverage and orphaned the citation in
`job.go` that names the existing subtest.

Note also why the new tests would not have worked: they set no status, and
`legalStatusEdges[StatusQueued]` (`status.go:26-29`) has no edge to `StatusVerifying`, so
`SetPostProcStarted` returns `ErrIllegalStatusTransition` and the test dies at its first `Fatalf`.
The existing test does `q.SetStatus(j.ID, constants.StatusDownloading)` first (`:566`, `:621`).
This is the #465 shape — a test passing or failing through a branch nobody meant to exercise — and
it is why Step 4's mutation, not a hand-written red, is this task's evidence.

**Add no subtest either.** An earlier draft added one that built a bare `&JobProgress{}` and called
`setDownloadFinishedOnce(time.Unix(0,0))` directly — no `Queue`, no `SetPostProcStarted`. That
assertion is already the `"finished"` row of Task 1's table, and putting it inside
`TestSetPostProcStarted` misattributes the owner's coverage to one of its callers. The four existing
subtests are the regression net; Step 4's mutation is this task's evidence.

Also rewrite `markDownloadFinishedOnce`'s doc comment here (Task 2 deliberately left it): it names
`Queue.SetPostProcStarted` as a second enforcement site that "is not this change's to remove", and
this is the change that removes it.

- [ ] **Step 2: Write the implementation**

```go
	job.PostProc = true
	// The bool is discarded on purpose: a job whose finish was already marked
	// is the ordinary case here, not an error. Before #464 this site applied
	// its own IsZero test to the same field; delegating is what stops the two
	// copies of first-wins from drifting.
	job.progress.setDownloadFinishedOnce(time.Now().UTC())
```

- [ ] **Step 3: Run tests to verify they pass**

Run: `go test -count=1 -race ./internal/queue/ ./internal/app/`
Expected: PASS, `TestSetPostProcStarted` included.

- [ ] **Step 4: Prove the delegation is load-bearing**

Create `internal/queue/testdata/postproc_stamp.spec`:

```text
pkg ./internal/queue/
run TestSetPostProcStarted

[the first-wins check dropped from the delegated setter]
file internal/queue/progress.go
--- anchor
	if !isJobStamp(t) || !p.downloadFinished.IsZero() {
--- replace
	if false {
--- end
```

Run: `go run ./scripts/mutate internal/queue/testdata/postproc_stamp.spec`
Expected: KILLED by the `"does not overwrite a finish time MarkDownloadFinished already set"`
subtest. **The `run` filter matches the existing top-level test, which is intended** — that test is
the regression net for this task. Record which subtest reported the failure, so the attribution is
observed rather than assumed.

- [ ] **Step 5: Commit**

```bash
git add internal/queue/queue.go internal/queue/job.go internal/queue/testdata/postproc_stamp.spec
git commit -m "refactor(queue): SetPostProcStarted delegates its first-wins rule"
```

---

### Task 4: Route the remaining three writers through the owner

**Files:**
- Modify: `internal/queue/job.go:1054-1055` (`ResetForRetry`), `internal/queue/progress.go:921-922`
  (`JobProgress.UnmarshalJSON`), `internal/queue/sqlite_store.go:567-572` (`SQLiteStore.Get` decode)
- Test: none written. `internal/queue/sqlite_store_test.go:812`'s existing
  `TestSQLiteStore_DownloadTimestampsPersistence` is the regression net (see Step 1).
- Create: `internal/queue/testdata/store_decode.spec` (deleted again in Task 5 — see there).

**Interfaces:** consumes Task 1's `clearDownloadStamps` and `restoreDownloadStamps`.

The store decode is routed **here, not in Task 5**, because Task 6's enumeration asserts an exact
writer set and `Get` is one of the writers. Routing it later would make Task 6 fail at its own
commit.

`ResetForRetry` and `UnmarshalJSON` cannot produce a bad stamp on a production path today — the
first writes `time.Time{}`, the second is unreachable in production (see the premise audit). They
are routed anyway, because Task 6 asserts the owner is the *only* writer and an exception list is
the thing that goes stale.

- [ ] **Step 1: Do NOT write a round-trip test — one exists**

`TestSQLiteStore_DownloadTimestampsPersistence` (`internal/queue/sqlite_store_test.go:812`) already
marks started via `MarkJobStarted`, finishes via `SetPostProcStarted`, round-trips through
`store.Get`, asserts both stamps at `:857-868`, and then again through `queue.Load`. Verified
passing on the unmodified tree:

```
$ go test -count=1 -run TestSQLiteStore_DownloadTimestampsPersistence ./internal/queue/
ok  	github.com/hobeone/gonzbd/internal/queue	0.008s
```

It is this task's regression net. An earlier draft invented
`TestStore_DownloadStampsSurviveARoundTrip` for the same behaviour — **the second time this plan
made that mistake**, after round 1 removed the same duplication from Task 3. That is why the Global
Constraints now require grepping for existing coverage before writing a test.

Its existence also settles the deferred question about fixture status: it necessarily drives the job
through a resident status, or its own assertions could not pass.

- [ ] **Step 2: Write the implementation**

`ResetForRetry`, replacing the two direct assignments:

```go
	j.progress.clearDownloadStamps()
```

`JobProgress.UnmarshalJSON`, replacing the two direct assignments:

```go
	p.restoreDownloadStamps(pj.DownloadStarted, pj.DownloadFinished)
```

`SQLiteStore.Get`, replacing the two guarded assignments at `:567-572`:

```go
			job.progress.restoreDownloadStamps(
				time.Unix(dlStartedUnix, 0).UTC(), time.Unix(dlFinishedUnix, 0).UTC())
```

The `> 0` guards go away because `restoreDownloadStamps` applies `isJobStamp`, which is the same
test on the same values. Task 5 replaces the two `time.Unix(...)` conversions with `decodeJobStamp`.

- [ ] **Step 3: Prove the store decode is load-bearing**

**A separate spec file, not an addition to `stamp_owner.spec`.** `scripts/mutate` parses one `run`
line per spec and applies it to every mutation in that file (parsed at `scripts/mutate/spec.go:151-155`,
which also refuses a global directive inside a `[mutation]` block at `:147-149`; the shared spec is
then passed to every mutation by the loop at `main.go:167-170`), so a store
mutation appended to a spec whose filter names only the owner tests would execute no store test and
report SURVIVED — the task would fail its own gate for a reason that has nothing to do with the
code.

Create `internal/queue/testdata/store_decode.spec`:

```text
pkg ./internal/queue/
run TestSQLiteStore_DownloadTimestampsPersistence

[the store decode stops restoring the stamps]
file internal/queue/sqlite_store.go
--- anchor
			job.progress.restoreDownloadStamps(
				time.Unix(dlStartedUnix, 0).UTC(), time.Unix(dlFinishedUnix, 0).UTC())
--- replace
			_, _ = dlStartedUnix, dlFinishedUnix
--- end
```

This compiles — `time` stays used elsewhere in the file (`sqlite_store.go:531`). An earlier draft
used `_ = func(...time.Time) {}(`, which assigns from a call with no results and would have
produced COMPILE_ERROR, a verdict `scripts/mutate` explicitly calls a false green for the pin.

Expected: KILLED. Be honest in the commit body about what this does and does not show: it proves
the decode reaches the fields, **not** that routing through the owner rather than assigning
directly matters. Routed and guarded-direct are behaviourally identical here; what makes the
routing load-bearing is Task 6's enumeration, and that is where its evidence lives.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -count=1 -race ./internal/queue/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/queue/job.go internal/queue/progress.go internal/queue/sqlite_store.go internal/queue/testdata/store_decode.spec
git commit -m "refactor(queue): route reset, restore and store decode through the owner"
```

---

### Task 5: One codec for the store's timestamp columns

**Files:**
- Modify: `internal/queue/sqlite_store.go:405-413` (`addTx`), `:1012-1020` (`updateTx`), and the
  `Get` decode Task 4 rewrote
- Test: `internal/queue/progress_test.go` — **not `sqlite_store_test.go`**, which is
  `package queue_test` and cannot see `encodeJobStamp`, `decodeJobStamp` or `isJobStamp`. The codec
  test asserts the owner/codec agreement, so it belongs beside the owner's own tests anyway.

**Interfaces:**
- Produces: `encodeJobStamp(time.Time) int64`, `decodeJobStamp(int64) time.Time`.
- Consumes: Task 1's `isJobStamp` (for the shared bound) and Task 4's routed decode site. This half
  is **not** independent of the earlier tasks; an earlier draft claimed it was.

The two encode blocks are byte-identical today and have no shared owner — issue #464's option 3,
worth doing whether or not the epoch bug existed.

- [ ] **Step 1: Write the failing test**

```go
func TestJobStampCodec_RoundTripsAndAgreesWithTheOwnersBound(t *testing.T) {
	real := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	if got := encodeJobStamp(real); got != real.Unix() {
		t.Errorf("encodeJobStamp(%v) = %d, want %d", real, got, real.Unix())
	}
	if got := encodeJobStamp(time.Time{}); got != 0 {
		t.Errorf("encodeJobStamp(zero) = %d, want 0", got)
	}
	if got := decodeJobStamp(real.Unix()); !got.Equal(real) {
		t.Errorf("decodeJobStamp(%d) = %v, want %v", real.Unix(), got, real)
	}
	for _, absent := range []int64{0, -1} {
		if got := decodeJobStamp(absent); !got.IsZero() {
			t.Errorf("decodeJobStamp(%d) = %v, want the zero value", absent, got)
		}
	}

	// The codec and the owner must accept the same set. This is the property
	// that lets #464 close without a nullable column, so assert it rather than
	// stating it in a comment.
	for _, tc := range []time.Time{
		time.Time{}, time.Unix(0, 0), time.Unix(0, 500000000), time.Unix(1, 0), real,
	} {
		if isJobStamp(tc) != (encodeJobStamp(tc) > 0) {
			t.Errorf("isJobStamp(%v) = %v but encodeJobStamp gives %d",
				tc, isJobStamp(tc), encodeJobStamp(tc))
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -count=1 -run TestJobStampCodec ./internal/queue/`
Expected: FAIL — `undefined: encodeJobStamp`.

- [ ] **Step 3: Write the implementation**

```go
// encodeJobStamp is the wire form of a download timestamp: 0 means absent.
//
// The sentinel is unambiguous because isJobStamp — the owner's bound in
// progress.go — is defined on t.Unix() > 0, the same quantity this writes. Any
// stamp the owner accepts encodes to a positive integer, and no stamp it
// accepts encodes to 0. That equivalence is what let #464 close without making
// the columns nullable, and TestJobStampCodec asserts it rather than trusting
// this sentence.
func encodeJobStamp(t time.Time) int64 {
	if !isJobStamp(t) {
		return 0
	}
	return t.Unix()
}

// decodeJobStamp inverts encodeJobStamp.
func decodeJobStamp(unix int64) time.Time {
	if unix <= 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0).UTC()
}
```

Both encode sites collapse to:

```go
	var dlStartedUnix, dlFinishedUnix int64
	if p := job.Progress(); p != nil {
		dlStartedUnix = encodeJobStamp(p.DownloadStarted())
		dlFinishedUnix = encodeJobStamp(p.DownloadFinished())
	}
```

The decode site Task 4 rewrote becomes:

```go
			job.progress.restoreDownloadStamps(
				decodeJobStamp(dlStartedUnix), decodeJobStamp(dlFinishedUnix))
```

**Delete `internal/queue/testdata/store_decode.spec` in this task.** Its anchor is the
`time.Unix(...)` form the line above replaces, so from this commit onward it can only report
`ANCHOR — anchor matched no site`, which is a non-zero exit: a committed spec that cannot run. The
mutation is not lost — `stamp_codec.spec` below already names
`TestSQLiteStore_DownloadTimestampsPersistence` in its `run` filter, and its "decode neutered"
mutation covers the same path through `decodeJobStamp`. Say in the commit body that the spec was
deleted rather than re-anchored, and why.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -count=1 -race ./internal/queue/`
Expected: PASS, including Task 4's round-trip test and the existing store tests.

- [ ] **Step 5: Prove the decode is load-bearing**

Create `internal/queue/testdata/stamp_codec.spec`:

```text
pkg ./internal/queue/
run TestJobStampCodec|TestSQLiteStore_DownloadTimestampsPersistence

[decode neutered]
file internal/queue/sqlite_store.go
--- anchor
	if unix <= 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0).UTC()
--- replace
	_ = unix
	return time.Time{}
--- end

[encode drops the owner's bound]
file internal/queue/sqlite_store.go
--- anchor
	if !isJobStamp(t) {
		return 0
	}
	return t.Unix()
--- replace
	return t.Unix()
--- end
```

Run: `go run ./scripts/mutate internal/queue/testdata/stamp_codec.spec`
Expected: both KILLED.

- [ ] **Step 6: Commit**

```bash
git rm internal/queue/testdata/store_decode.spec
git add internal/queue/sqlite_store.go internal/queue/progress_test.go internal/queue/testdata/stamp_codec.spec
git commit -m "refactor(queue): one codec for the download-timestamp columns"
```

---

### Task 6: Pin the owner as the only by-name writer

**Files:**
- Create: `internal/queue/stamp_enumeration_test.go`

**Interfaces:** consumes nothing at compile time; reads the package's own sources with `go/ast`.

**Model this on `internal/job/writer_enumeration_test.go`'s `scanWriters(t, field)`, not on
`internal/queue/donebit_enumeration_test.go`.** The donebit scanner matches *call expressions*
(`calleeName(call.Fun) == "markDone"`), which is the wrong AST shape for this job. `scanWriters`
matches *field assignment*, which is the right one, and it already handles three shapes an
`AssignStmt`-only scan misses: compound assignment, `*ast.IncDecStmt`, and `*ast.CompositeLit`
(`&JobProgress{downloadStarted: t}` — a shape already used in this package for other fields at
`internal/queue/persistence.go:140`). It is unexported and scoped to the `job` package's own
directory, so this is "copy the more correct pattern", not "call the existing function".

Consider extracting the directory-walk half so this test and `donebit_enumeration_test.go` parse
`internal/queue`'s ~8,300 non-test lines once rather than twice per run. That is a small,
test-time-only cost; take it if the extraction is clean, skip it and say so if it entangles the two
tests' matching logic.

- [ ] **Step 1: Write the test**

```go
// downloadStampWriters is the enumeration this test defends: the functions
// permitted to assign p.downloadStarted or p.downloadFinished by name.
//
// The rule these four enforce — a job timestamp's Unix() must be positive — is
// stated in progress.go's isJobStamp comment and settled on #464. A fifth
// writer could store a value that rule forbids, and nothing else in the build
// would notice: the fields are unexported, so no compiler error follows.
//
// SCOPE, three exclusions, all deliberate:
//   - Whole-struct copy. JobProgress.clone does `cp := *p`, propagating both
//     fields and minting neither. Cited by name, not line: Task 1 inserts ~60
//     lines above it in the same file.
//   - Test sources. challenger_m3_test.go assigns downloadFinished directly,
//     by design and with its own justification.
//   - Field identity. The scan matches on selector name alone, with no
//     go/types resolution, so a second struct in this package declaring a
//     field of either name would poison the set. Today JobProgress is the sole
//     declarer (progress.go, the two fields in its struct literal).
//
// ASSERT PER FIELD, NOT AS A UNION. A single four-name set is strictly weaker:
// setDownloadFinishedOnce miswired to write downloadStarted still produces
// exactly those four names, which is the miswiring Task 1's paired-getter table
// exists to catch. Two three-name sets catch it here as well.
var downloadStartedWriters = []string{
	"clearDownloadStamps", "restoreDownloadStamps", "setDownloadStartedOnce",
}
var downloadFinishedWriters = []string{
	"clearDownloadStamps", "restoreDownloadStamps", "setDownloadFinishedOnce",
}

func TestDownloadStampWriters_MatchTheEnumerationStatedInProse(t *testing.T) { ... }
```

- [ ] **Step 2: Run to verify it passes**

Run: `go test -count=1 -run TestDownloadStampWriters ./internal/queue/`
Expected: PASS with exactly those four names. If `Get`, `ResetForRetry`, `UnmarshalJSON`,
`markStartedOnce`, `markDownloadFinishedOnce` or `SetPostProcStarted` appears, Tasks 2-5 are
incomplete — fix the routing, not the list.

- [ ] **Step 3: Prove the pin discriminates**

Create `internal/queue/testdata/stamp_writers.spec`:

```text
pkg ./internal/queue/
run TestDownloadStampWriters

[a fifth writer appears, by named assignment]
file internal/queue/job.go
--- anchor
func (j *Job) markStartedOnce(t time.Time) bool {
	return j.progress.setDownloadStartedOnce(t)
--- replace
func (j *Job) markStartedOnce(t time.Time) bool {
	j.progress.downloadStarted = t
	return j.progress.setDownloadStartedOnce(t)
--- end

[a fifth writer appears, via composite literal]
file internal/queue/job.go
--- anchor
func (j *Job) markDownloadFinishedOnce(t time.Time) bool {
	return j.progress.setDownloadFinishedOnce(t)
--- replace
func (j *Job) markDownloadFinishedOnce(t time.Time) bool {
	_ = JobProgress{downloadFinished: t}
	return j.progress.setDownloadFinishedOnce(t)
--- end
```

Run: `go run ./scripts/mutate internal/queue/testdata/stamp_writers.spec`
Expected: both KILLED. **The second is the one that justifies the `scanWriters` model** — an
`AssignStmt`-only scanner would report SURVIVED, which is how the earlier draft of this plan would
have shipped a pin blind to composite literals.

- [ ] **Step 4: Commit**

```bash
git add internal/queue/stamp_enumeration_test.go internal/queue/testdata/stamp_writers.spec
git commit -m "test(queue): pin the download-stamp owner as the only by-name writer"
```

---

### Task 7: Sweep the claims this change falsified

**Files:** the six sites in Global Constraints, plus whatever the greps below turn up.

Run this **last**, per AGENTS.md's rule that an early sweep goes stale — each earlier task's own
fix creates fresh drift.

- [ ] **Step 1: Sweep the literals, from the repository root**

```bash
git grep -n 'downloadFinished' -- '*_test.go' AGENTS.md docs/
git grep -n 'downloadStarted' -- '*_test.go' AGENTS.md docs/
git grep -n 'IsZero' -- internal/queue/
git grep -n 'TestJobMarkOnce'   # only if Task 2 renamed it — lifecycle_test.go:1227 names it
```

One site the greps will surface that is **not** a gate failure and needs a decision rather than an
edit: `docs/superpowers/plans/2026-08-30-b24a-job-methods-plan.md:175-190` carries the same writer
grep and a writers table this change falsifies. `check_citations` scans tracked `.go` files only
(`scripts/check_citations/main.go:206`), so a stale `docs/` plan cannot turn it red. It is a
historical planning record. Say in the commit body which way it went — corrected, or left as a
frozen record of what was true then — rather than leaving it silently stale.

- [ ] **Step 2: Fix the six known sites**

Global Constraints lists them. `lifecycle_test.go:696-706` is the one that fails a gate rather than
merely reading wrong: its embedded `git grep` citation states "returns 5 writers" and enumerates
them. Re-run the command and record what it returns — Constraint 2 forbids carrying a predicted
number here, and a draft of this plan violated that by asserting one anyway.

- [ ] **Step 3: Run the gates that read prose**

```bash
git add -A
go run ./scripts/check_citations
go run ./scripts/check_dup_comments
```

`check_citations` scans **tracked** files, so the `git add` is required, not incidental.
Expected: 0 wrong.

- [ ] **Step 4: Run `pr-review-toolkit:comment-analyzer` over the cumulative branch diff**

The grep finds comments that share a token with the change; the analyzer reads the comments the
change *touched*. They cover different things — run both.

- [ ] **Step 5: Full gates and commit**

```bash
go fix ./... && goimports -w . && go vet ./... && go vet -tags=integration,uitest,crash ./...
go test -race ./... && ./scripts/run_tests.sh && golangci-lint run ./...
git commit -m "docs(queue): sweep the claims the stamp owner falsified"
```

---

## Inconclusive / Deferred items

- **Whether `download_started` can scan as SQL `NULL` rather than `0`.** The column is
  `INTEGER NOT NULL DEFAULT 0` and is scanned into a bare `int64` with no `COALESCE`, unlike the
  string columns beside it.
  *Probe:* `git grep -n 'download_started' -- internal/` and read every INSERT/UPDATE naming the
  column, plus `001_initial.sql:104`.
  *Expected branches:* (a) every writer supplies a value and `NOT NULL` holds, so the bare scan is
  safe and Task 5 needs no change; (b) some path can leave it NULL, in which case the scan already
  errors today — a **separate pre-existing defect**; file it, do not fold it in.

- **Non-resident jobs never restore their stamps at all.** `sqlite_store.go` scans both columns at
  `:505` but applies them only inside `if isResidentStatus(job.Status) { if manifest exists { … } }`
  (`:555-573`). A job restored at a non-resident status silently reports both as zero. Pre-existing,
  and Task 4 keeps the decode inside that branch.
  *Probe:* read `:555-573` and check whether any consumer reads these fields for a non-resident job.
  *Expected branches:* (a) no consumer does, so the behaviour is correct and only undocumented —
  add a sentence; (b) one does, which is a **new issue**, not a task here.

- **Whether the `toUnix`/`fromUnix` codec in `internal/history/repository.go:589-602` should be
  shared.** Two questions, not one, and only the first was scoped out at Gate 1:
  (i) *domain* — are `Completed`/`TimeAdded` ever optional, such that they carry the same collision?
  (ii) *mechanism* — should there be one codec for both packages rather than two private copies?
  *Probe:* read `toUnix`/`fromUnix` and their callers; count occurrences of the pattern repo-wide.
  *Expected branches:* (i) the fields are never optional → the collision is unreachable, no work;
  (ii) sharing means exporting and relocating an unexported helper across a package boundary — a
  **Rule 2 escalation**, and with only two occurrences the rule-of-three is not met, so the
  standing answer is "no, and here is why" rather than silence.

- **Whether `JobProgress.MarshalJSON` (`progress.go:890-891`) needs the codec.** It writes
  `time.Time` into JSON, not an integer, so it has no `0` sentinel — but it mirrors `UnmarshalJSON`,
  which Task 4 routes through the owner.
  *Probe:* read `:880-925`; check whether the JSON round trip can produce a stamp `isJobStamp`
  rejects.
  *Expected branches:* (a) RFC-3339 carries the zero value as the zero value → no change;
  (b) it can produce epoch zero → `restoreDownloadStamps` already filters it on the way back in, so
  still no change, but say so.

- ~~**Whether the existing `internal/queue` store fixture sets a resident status.**~~ **Resolved
  during review.** `TestSQLiteStore_DownloadTimestampsPersistence` (`sqlite_store_test.go:812`)
  asserts both stamps after `store.Get` and passes today, so it necessarily drives the job through
  a resident status — `Get` restores nothing otherwise. Task 4 uses that test rather than a new
  fixture, so the question no longer gates anything.
