# Overlap Detection in the Durability Layer — Design and Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop range overlaps being *silent*. Two durable articles claiming overlapping bytes are reported to the user as a post anomaly, so par2 — which #410 already unblocked — repairs a file the user knows about.

**Spec and plan are one document.** The architectural route normally commits a separate spec, but the design below is four sections and the plan is four tasks; splitting them would make the artifact larger than the change. Sections 1–4 are the spec, Tasks 1–4 implement it.

**Tech Stack:** Go 1.26.6, standard library only. No new dependencies.

**Issue:** #387 (detection half; the CRC half landed in #410).

## Global Constraints

- **No backwards compatibility.** Fresh installs only. Do not add a guard whose only justification is state an earlier build wrote.
- **State has one owner.** This design exists partly because `HasPrefixCRC` had two writers. Do not add a second computation of the stop reason.
- **Red-green is mechanical.** Every behavioural pin observed failing against unfixed code, `-count=1`. A cached `ok` is not an observation. Record the message.
- **Never `git stash`** — the stack is shared across worktrees.
- After every `.go` edit: `goimports -w <file>`, then `go build ./...`.
- Gates before every commit: `gofmt`, `go vet ./...`, `go test -race ./...`, `golangci-lint run ./...`, `check_dup_comments`, `check_review_banner`, plus diff-scoped `check_coverage` and `check_test_alignment`.
- **Sweep the literal, not the concept**, from the repository root, after any task that changes a claim.
- `ci.yml` is manual-dispatch only. Local gates are the gate of record. Never claim "CI green".

---

## 1. What is detected, and why here

`verifiedPrefix` (`internal/durability/prefix.go`) walks a file's offset-ordered Class A facts and stops at the first that does not abut the run. Its own comment already names both causes: *"either a hole, or an overlap this walk cannot prove tiles the range (R23)."* The walk therefore **already detects overlaps** and discards which kind it saw.

Class A facts are the right source, and better than anything `FileWriter` holds:

| | `FileWriter.acceptedAt` | Class A facts |
|---|---|---|
| Carries length | ✗ — `{id articleID; written bool}` | ✓ `Offset`, `Length` |
| Survives restart | ✗ rebuilt empty per file open | ✓ persisted |
| Ordered | ✗ map keyed on exact start | ✓ `FactLog.ForFile` orders by offset |
| Already walked | — | ✓ once per barrier |

This also sidesteps what killed the withdrawn `claimIndex`: `req.Offset` is attacker-controlled (`assembler.go:1788`), so a structure ordered by insertion on that value is corruptible by a hostile server. Facts are ordered by the query — `ORDER BY offset` at `factlog_sqlite.go:83` — not by insertion.

### What this does NOT detect

"The walk already detects overlaps" is true but not complete coverage, and the difference must not be lost:

- **At most one overlap per file, ever.** The walk stops at the first non-abutting fact, so a file with two overlaps reports the lower one and never the other — permanently, since nothing resolves the state. This is a report that the file is damaged, not an inventory of the damage.
- **Nothing above a hole or a non-durable article.** `fact.Offset < prefix` is reachable only when every fact below it is durable and abuts. An overlap sitting above a permanently-failed article, or above one still in flight, is invisible.

Both are the safe direction — under-reporting a malformed post, never over-reporting a healthy one — which is why they are acceptable. They are recorded so nobody reads this design as "overlaps are detected" full stop.

## 2. Why it cannot double-report

The walk tests the predicate **before** the offset, and an article the assembler refused never gets a durable bit:

```go
if !verified(i) { break }          // not durable — no report
if fact.Offset != prefix { break } // >  hole; < overlap
```

| Stop reason | Report |
|---|---|
| `!verified(i)` | no — failed, in flight, or a hole below an undelivered article |
| `fact.Offset > prefix` | no — a hole |
| `fact.Offset < prefix` | **yes — two durable articles overlap** |

An exact-offset collision the assembler already handles never reaches the third row: its loser is resolved permanently failed and is never durable. **This ordering is load-bearing — do not reorder the two breaks.**

## 3. How it reaches the user

The barrier surfaces findings through its **return**; `Application` routes them to the existing `handlePostAnomaly`, which logs and calls `Queue.SetWarning`.

**A returned finding can be dropped silently** — `_, err := b.Run(...)` compiles — which is the failure `FileWriter.Accept`'s doc warns about. That risk is accepted here because there is exactly one production caller of each method, and **Task 3 pins that each caller routes it**.

"Each" is the load-bearing word. `Run` and `FinalizeFile` are two independent routes, so one test covering one of them would leave the other unprotected by the very argument that justified this shape. If a second production caller of either ever appears, this decision should be revisited rather than extended.

Rejected alternatives, recorded so they are not re-proposed:

- **A reporting collaborator on `Barrier`.** Rejected in favour of explicit data flow.
- **Detection in `Application` from the fact log.** A second walk of the same rows — 163 ms at 20,000 articles, measured — computing what the barrier already knows. The two-writers defect #410 deleted.
- **Widening `Stallable` or `Acker`.** Both are scoped to one concern (storage faults; article acking); this is a third.

## 4. Two constraints that are easy to miss

**Report after the unlock.** Both call sites hold a per-job barrier mutex across the barrier's I/O, and both already push reporting below `mu.Unlock()` on purpose:

> *"The lock is held across the barrier's I/O by design — that is the serialisation — but a log write is I/O of its own, and a slow handler would hold every other checkpoint for this job behind a message about one that already failed."*

**Route findings after the unlock, on the path the error report already takes.** The reason is the one the comment gives — I/O under a lock — and nothing more.

An earlier draft of this section argued the locks would newly nest. **That was false, and it is recorded here so it is not re-derived.** `Barrier.Run` calls `b.ack.AckDurable` (`barrier.go:183`; `FinalizeFile` at `:653`), `Acker` is the `*queue.Queue`, and `AckDurable` takes `q.mu` (`workset.go:45`) — so barrier-mu → q.mu is the existing, exercised order on every successful checkpoint. There is no new nesting and no deadlock; no path takes `q.mu` before `jobBarrierLock`. Writing the deadlock story into the design record would have made it a false invariant the next reader trusts.

**No latch is needed for correctness.** `Queue.SetWarning` is a field assignment, so re-setting the same string is idempotent. What repeats across checkpoints is the `Warn` log line and a dirty-flag persist. De-duplication is therefore a quality measure in the reporting path, not a guard — and it must not be implemented as persisted state on `FileExtent`, which would be a derived value that is also persisted.

---

### Task 1: `prefixWalk` records why the walk stopped

**Files:**
- Modify: `internal/durability/prefix.go`
- Test: `internal/durability/prefix_test.go`

**Interfaces:**
- Produces: `prefixWalk.overlap() (int, bool)` — the index of the first fact that starts *below* the run, and whether one exists.

**The overlapped sibling is `facts[i-1]`, and that is derivable rather than guessed.** Facts are offset-ordered, so `facts[i-1].Offset <= facts[i].Offset < prefix`, and `prefix` is exactly `facts[i-1].Offset + facts[i-1].Length` — the run's end is the previous fact's end. So `i-1` is provably the article whose bytes `facts[i]` landed inside. State this where the caller uses it.

**Guard `i > 0`, and `overlap()` must return `false` for the zero `prefixWalk`.** Reaching the classification at `i == 0` requires `Offset < 0`. The check that actually prevents that is in the decoder — `decoder.go:472` bounds the yEnc `begin` value — **not** `offsetOutOfRange`, which gates a `WriteRequest` and cannot reach a fact at all, since facts are appended before and independently of the write. Getting that attribution right matters: it is a claim about where a security-relevant check lives.

The zero-value point is the repo's own "a type with a valid zero" smell. `barrier.go` returns `prefixWalk{}` on four error paths, and a zero walk must not report an overlap at index 0. Store an explicit bool rather than using a sentinel index.

- [ ] **Step 1: Write the failing test**

```go
func TestVerifiedPrefix_DistinguishesAnOverlapFromAHole(t *testing.T) {
	crcA := uint32(0x11111111)
	all := func(int) bool { return true }

	// A0 [0,100), A1 [100,200), X [150,200) — X starts BELOW the run.
	overlapping := []ArticleFact{
		{ArtIdx: 0, Offset: 0, Length: 100, CRC32: crcA},
		{ArtIdx: 1, Offset: 100, Length: 100, CRC32: crcA},
		{ArtIdx: 2, Offset: 150, Length: 50, CRC32: crcA},
	}
	w := verifiedPrefix(overlapping, all)
	idx, ok := w.overlap()
	if !ok || idx != 2 {
		t.Errorf("overlap() = (%d, %v), want (2, true) — X starts at 150 inside a run "+
			"that already reached 200, so two durable articles claim those bytes", idx, ok)
	}

	// A0 [0,100), then a fact at 200 — a HOLE, not an overlap. Reporting this
	// as one would tell the user their post is malformed when the file is
	// merely incomplete, which is the ordinary state of a running download.
	gapped := []ArticleFact{
		{ArtIdx: 0, Offset: 0, Length: 100, CRC32: crcA},
		{ArtIdx: 2, Offset: 200, Length: 100, CRC32: crcA},
	}
	if _, ok := verifiedPrefix(gapped, all).overlap(); ok {
		t.Error("overlap() reported true for a hole — a file waiting on an article " +
			"would be reported as a malformed post on every checkpoint")
	}

	// An UNVERIFIED fact stops the walk before the offset is ever compared, so
	// it must not be classified either way. This is the case that keeps the
	// assembler's own exact-offset collisions from being reported twice: the
	// loser is resolved permanently failed and never becomes durable.
	//
	// The unverified fact must ALSO be the overlapping one. A fixture whose
	// unverified fact abuts cannot discriminate: the only available mutation
	// is swapping the two breaks, and with an abutting fact the offset test
	// never fires there under either order, so the mutant stays green and the
	// pass reads as evidence when it is nothing of the kind.
	twoOfThree := func(i int) bool { return i < 2 }
	if _, ok := verifiedPrefix(overlapping, twoOfThree).overlap(); ok {
		t.Error("overlap() reported true when the walk stopped on an unverified " +
			"fact; the assembler's own collisions would be double-reported")
	}
}
```

Trace both orders against that last fixture, because it is the only sub-case whose discrimination is not obvious:

| | correct order (`!verified` first) | mutated order (offset first) |
|---|---|---|
| `i=2`, `Offset 150`, `prefix 200`, unverified | breaks on `!verified` → no classification | `150 != 200` → classifies → **reports** |

That is the red. With `verified = {true, false, ...}` instead, the walk stops at `i=1` where `Offset 100 == prefix 100`, the offset test cannot fire under either order, and the mutation is invisible.

- [ ] **Step 2: Run to verify it fails**

Run: `go test -count=1 ./internal/durability/ -run TestVerifiedPrefix_DistinguishesAnOverlapFromAHole`
Expected: FAIL — `w.overlap undefined`.

**A compile error is not a red check.** After Step 3, mutate and record both messages:

1. Change `fact.Offset < prefix` to `fact.Offset != prefix` → the **hole** case must fail.
2. **Swap the two breaks** so the offset test runs before `!verified(i)` → the **unverified** case must fail.

Mutation 2 is the only one available for the third sub-case, because the plan's own implementation puts the classification inside the `fact.Offset != prefix` branch — there is no separate `!verified(i)` guard on the classification to remove. If a mutation you intended is not expressible against the code you wrote, that is a signal the test is pinning nothing, not a licence to skip the check.

- [ ] **Step 3: Implement**

Record the classification where the walk already knows it — inside the loop, at the break — not by re-deriving it from the returned `VerifiedTo` afterwards. Re-deriving would be a second computation of the same fact, which is the rule this design section already cites.

Keep `overlap()` a method returning `(int, bool)` rather than exporting a raw field, so the "did it stop on an overlap" question has one phrasing.

- [ ] **Step 4: Run the tests**

Expected: PASS, plus every existing `TestVerifiedPrefix_*` case unchanged.

- [ ] **Step 5: Commit**

`feat(durability): tell an overlap from a hole in the prefix walk`

Body: the classification's three cases, why the `!verified(i)` ordering is load-bearing, and both observed clause-level reds.

---

### Task 2: The barrier surfaces overlap findings

**Files:**
- Modify: `internal/durability/barrier.go` (`Run`, `FinalizeFile`, `buildExtent`)
- Test: `internal/durability/overlap_report_test.go`

**Interfaces:**
- Produces:
  ```go
  // PostAnomaly is a structural problem with what the servers returned,
  // found while walking a file's facts.
  type PostAnomaly struct {
      FileIdx int32
      Reason  string // human-readable, formatted here
  }
  ```
  `Run` returns `[]PostAnomaly` (it iterates files internally, so the caller cannot supply `FileIdx`); `FinalizeFile` returns zero or one.
- Consumes: Task 1's `overlap()`.

**No `JobID` field.** Both callers hold `jobID` as a local at the call site — it is the variable they passed in one line earlier — so carrying it back duplicates data the caller already has. `FileIdx` is different and must be carried: `Run` iterates files internally and the caller cannot know which one.

**Format the reason HERE, not in `Application`.** This mirrors the convention the assembler already uses for the same concept: `postAnomalyReason` (`assembler.go:1621`) builds the string at the detection site and hands `OnPostAnomaly` a plain `string`. `buildExtent` has `t.Path(idx)` in scope (it already uses it at `:246`), so it has everything that function has. Returning structured indices and composing the message a package away would put the phrasing further from the data than the existing precedent, and would give `Application` a job it does not otherwise do. Task 3 is then pure routing.

**Both methods currently return `error` alone and have 43 call sites, almost all tests.** Expect mechanical churn; a call site that fails to compile is retargeting work, not a behaviour change.

**`buildExtent` already returns the walk** (`barrier.go:217`, since #410) — the work is not adding that. It is `Run` at `barrier.go:144`, which currently discards it with `ext, _, arts, err := b.buildExtent(...)`, and `FinalizeFile`, which already binds it as `walk`.

One call site sits outside `internal/durability` and `internal/app`: `internal/assembler/finalize_wiring_test.go:131`. It is covered by "expect churn", but it is the only cross-package one and so the only one a package-scoped build will not surface.

- [ ] **Step 1: Write the failing test**

Build the three-fact contained-overlap fixture through `FinalizeFile` — `finalize_overlap_test.go` already has one that asserts `HasPrefixCRC == false`; model on it — and assert one finding comes back naming file 0, with a `Reason` mentioning the file's base name.

Assert on the base name rather than the whole string: pinning exact prose makes the test a change-detector for wording, while asserting nothing about the reason lets an empty string pass.

Add a second case asserting a **hole** yields no finding, driven through the same path. Without it the test cannot distinguish "reports overlaps" from "reports whenever the walk stops early", and the latter would warn on every incomplete file.

- [ ] **Step 2: Run to verify it fails**

Expected: FAIL on the signature. As in Task 1, follow with a real mutation once it compiles: make the barrier return a finding unconditionally and confirm the hole case fails.

- [ ] **Step 3: Implement**

`buildExtent` already holds the walk after #410. Thread the finding out the way the walk itself is threaded — do not re-walk, and do not recompute the classification at the call site.

**Decide, at the signature, what a finding means alongside an error.** `Run` can classify an overlap in phase 3 and then fail at `Commit` or `AckDurable`, and `checkpointJob`'s error branch returns early at three places — so a finding produced before a later failure is silently dropped unless the contract says otherwise. Pick one and write it on the method: findings on the nil-error path only, or reportable regardless. Leaving it to whichever branch gets written first is how it becomes accidental behaviour.

- [ ] **Step 4: Retarget call sites and run**

Run: `go test -race -count=1 ./internal/durability/`
Expected: PASS. Pre-existing tests should need only the extra return value.

- [ ] **Step 5: Commit**

`feat(durability): surface overlapping durable articles from the barrier`

---

### Task 3: `Application` reports the finding

**Files:**
- Modify: `internal/app/durability.go` (both barrier call sites; `handlePostAnomaly` if de-duplication lands there)
- Test: `internal/app/barrier_wiring_test.go` — it already exercises the barrier through `Application`, so the finding's route can be asserted there rather than building a new harness. `checkpoint_reporting_internal_test.go` is the fallback if the fixture there fits better.

**Interfaces:**
- Consumes: Task 2's finding type.

- [ ] **Step 1: Write the failing test — TWO cases, one per call site**

The pin that matters is **that the caller routes it at all** — Section 3 accepts a droppable return specifically because this test exists. That argument covers `Run` and `FinalizeFile` **separately**: they are two independent production call sites (`durability.go:535` and `:883`) with two independent wiring points, and a single test would leave whichever one was forgotten unprotected by the very argument that justified the design.

So: one case driving a durable overlap through **`Run`**, one through **`FinalizeFile`**.

**Assert the message content, not that `Warning` is non-empty.** `job.Warning` has at least four other writers — `handlePostAnomaly`'s own doc names the stall reason, two durability warnings and the claim-failure note, and both `Application.Stall` and `Application.Fail` set it and are reachable from a barrier that failed inside the same call. A non-emptiness assertion passes on a fixture where the barrier faulted for an unrelated reason, which is the same test-passes-via-the-wrong-path shape the third sub-case of Task 1 exhibits.

**Assert the file's base name and both article indices — NOT the file index.** `handlePostAnomaly` sets `Warning` to the reason string alone; `fileIdx` goes only to the log line and never reaches the warning. Task 2 formats the reason after `postAnomalyReason`, which uses `filepath.Base(path)` and article labels and carries no file index. Asserting on a file index here would demand something the format spec does not produce, and the two tasks would be unimplementable together.

**Budget for fixture work.** `barrier_wiring_test.go` drives writes through `application.assembler.WriteArticle` directly, bypassing `pipeline.appendArticleFacts`, so its fact log is empty. Facts must be inserted by hand and both overlapping articles driven to durable. This is more than "assert on an existing fixture".

- [ ] **Step 2: Run to verify both fail**

Expected: both FAIL with an empty warning. **Run them separately and confirm each fails on its own** — if one passes because the other's route already set the warning on a shared fixture, the test pair proves nothing about the route it names.

- [ ] **Step 3: Implement**

**Route after `mu.Unlock()`**, on the path the error report already takes. The reason is I/O under a lock, and only that — see Section 4, including its note that the deadlock story is false and must not be re-derived.

Pure routing: the reason string arrives formatted from Task 2. Attach the caller's own `jobID` and hand it to `handlePostAnomaly`.

- [ ] **Step 4: Run**

Run: `go test -race -count=1 ./internal/app/`
Expected: PASS. **The race detector matters here specifically** — this adds a call on a path that was inside a mutex one line earlier.

- [ ] **Step 5: Commit**

`feat(app): report overlapping durable articles as a post anomaly`

---

### Task 4: Correct the reproduction probes

**Files:**
- Modify: `internal/assembler/overlaprange_test.go`

The two probes stay **skipped**: they assert on the assembler's `OnArticleRejected` / `OnPostAnomaly` callbacks, and this change adds no assembler-side detection. Both messages are now wrong in ways that would mislead the next reader.

- [ ] **Step 1: Correct probe 1's skip reason, and probe 2's opening clause**

Check both before editing. `TestOverlap_PartialRangeOverwritesADurableArticle`'s reason says detection is absent, which stops being true with Task 2. Say where detection now lives, and what these probes still pin that the barrier cannot: that the bytes are overwritten in the first place, before any barrier runs.

The second probe's reason is mostly still accurate — it already says the CRC half is fixed and names what it pins — but it opens *"as above, the overlap is undetected"*, which back-references the sentence Step 1 is rewriting and reads as a system-wide claim Task 2 falsifies. Narrow it to the assembler.

- [ ] **Step 2: Correct the stale assertion message**

`TestOverlap_ContainedOverlapStillCompletesTheFile` still says the recorded whole-file CRC "matches par2, causing QuickCheck to pass and repair to be skipped". #410 fixed exactly that. The file still completes and the bytes are still overwritten — but the CRC is now withheld and par2 runs. Rewrite to claim only what is still true.

- [ ] **Step 3: Sweep**

```bash
git grep -n 'overlapping ranges'
git grep -n 'silently overwrit'
git grep -n 'acceptedAt'
git grep -n 'exact-offset\|exact start offset'
git grep -n 'reported by the assembler'
```

That last literal is `handlePostAnomaly`'s own log line, and its doc says the same thing in prose. After Task 3 the barrier is a second source, so both become false — and neither carries any of the other greps' tokens, which is why it is listed by name.

`docs/durability-contract.md` and `docs/article-validation-contract.md` describe collision detection as exact-offset only. That is still true of the *assembler*, and now incomplete as a description of the system. Correct both to say where each half lives.

**Also mark the withdrawn design.** `docs/superpowers/plans/2026-08-19-overlapping-range-detection-plan.md` describes the `claimIndex` approach and reads as current, because nothing in it records that it was abandoned. Add a note at its head naming what replaced it and why — a reader who finds it first will otherwise implement a design two reviewers broke five ways. This is not covered by any grep above, which is why it is called out by name.

- [ ] **Step 4: Commit**

`docs(assembler): say where overlap detection actually lives`

---

## Inconclusive / Deferred items

- **Prevention is not attempted.** The bytes land before the barrier sees them. The direction settled at Gate 1 is notice-and-repair. *Resolution point:* if a user reports a file par2 could not repair because too many articles overlapped, prevention becomes worth its cost — and #387's recorded constraints are the starting point.
- **Which article is "at fault" is not decided.** The finding names both. Reporting one as the loser would need a rule this design does not have, and the assembler's displacement rule does not apply because both articles are already durable.
- **De-duplication across checkpoints — BUILT, and the reason for deferring it was wrong.** This item read "a quality measure, deliberately not a guard, because `SetWarning` is idempotent". `SetWarning` is *not* idempotent in the sense that argument needed: it assigns a single string, so re-raising the same finding every checkpoint also overwrites any other warning written in between — a stall reason set at one cycle is gone by the next. `Barrier.admit` now latches per `(jobID, fileIdx)`. The prediction that the fix belonged in the reporting path rather than in persisted state on `FileExtent` held: the latch is in memory, so a restart raises each finding once more.
- **`Resumer` cannot classify this at all**, which corrects an earlier draft of this item that said it could. `Resumer.recompute` builds its predicate from `verifyRegions`, which re-reads each fact's region and compares the CRC against the bytes actually on disk. The overlap victim's bytes were overwritten, so its CRC mismatches, `verified[victim]` is false, and the walk breaks on `!verified(i)` before any offset comparison. The resume path sees an unverified article and re-fetches it — which is a repair, not a detection. Nothing to add there.
- **A finding alongside an error — SETTLED, and recorded at `Barrier.admit`.** Findings are returned only by a path that **committed**; any failure at `Commit` or `AckDurable` returns `nil` findings with the error. "Committed" rather than "returned a nil error" is the operative word, and the distinction is not academic: `Run`'s `acked == 0` branch commits and returns nil, and an early draft that read the rule as *nil error* still dropped its finding there. Classification is free and repeatable, so it happens wherever it is convenient; only admission latches, and it is called at each committing return.
- **This design inherits one assumption it does not create.** `article_facts` rows are retained for a FAILED job because a retry reuses the job ID, and `Append` is `INSERT OR IGNORE` keyed on `(job_id, art_idx)` — so a retry whose re-parsed manifest mapped a different article to some `art_idx` would keep the old offset, and the walk could then see an overlap between two articles that do not overlap: a *false* "your post is malformed". The assumption is not new. The schema comment says those retained facts are what "bound the completion truncate to the whole file", so if `art_idx` stability across a retry were false there is already a durability defect, and a spurious warning would be the least of it. **Not a blocker for this change; worth its own issue if nobody has verified it.**
