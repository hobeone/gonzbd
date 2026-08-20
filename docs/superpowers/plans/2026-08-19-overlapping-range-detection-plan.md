# Overlapping Byte-Range Detection Implementation Plan

> ## ⚠ HISTORICAL — the detection design here was abandoned
>
> The durability half of this plan shipped in #410 and is current. **The
> `FileWriter`-side detection it describes was never built and should not be.**
> Two reviewers broke it five ways; the decisive one is that `req.Offset` is
> attacker-controlled, so a same-article retry returning at a different offset
> can permanently corrupt any index ordered by insertion on that value.
>
> Detection was rebuilt from the opposite direction and shipped separately: the
> durability layer classifies why `verifiedPrefix` stopped walking a file's
> Class A facts, which carry length, persist across restarts, and are ordered by
> the query rather than by insertion. See
> `2026-08-20-overlap-detection-design.md`.
>
> This file is kept because the rejected reasoning is worth reading before
> anyone proposes a `FileWriter`-side index again.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the durability layer from publishing a whole-file CRC it cannot justify, which is what makes an overlapping-range corruption *silent and unrepairable* rather than merely present.

> **Scope narrowed after two rounds of plan review.** This plan originally carried a third and fourth task adding range-overlap detection to `FileWriter`. Two independent reviewers broke that design — five distinct sequences, one of them security-relevant, since `req.Offset` is attacker-controlled (`assembler.go:1788`) and a same-article retry can return at a *different* offset, permanently corrupting a start-ordered index. Detection is deferred to its own PR with a fresh design pass; the constraints it must satisfy are recorded on #387.
>
> What ships here removes the worst property of #387 — that the corruption is invisible to par2 and the recovery volumes are never even fetched — without pretending to fix detection.

**Architecture:** `internal/durability` gets a single `verifiedPrefix` function replacing two independently-maintained prefix walks that had already diverged on the relabelling guard — one owner for `HasPrefixCRC` instead of two computations. `internal/assembler` gets only the #406 dead-code deletion, which is groundwork: it collapses `failDisplaced` from two production call sites to one, so the deferred detection change has a single disposition path to touch.

The durability half is what matters. It alone removes the *unrepairable* property: with `HasPrefixCRC` false, `app/durability.go:932` records no CRC, QuickCheck sees `NoCRC` rather than a spurious match, `stage_repair.go:111` no longer skips, and `app.go:1448` fetches the recovery volumes. The corruption becomes visible and repairable even though it is not yet prevented.

**One qualification on "one owner", stated because the claim is otherwise a false universal:** `resume.go:151` computes `HasPrefixCRC: ext.HasPrefixCRC && ext.VerifiedTo == fi.Size()` on the cache fast path, independently of either walk. It stays. It only ever *narrows* a value already produced under the guard, so it cannot manufacture a claim `verifiedPrefix` refused — but it is a third site touching the flag and should not be discovered later as an omission.

**Tech Stack:** Go 1.26.6, standard library only. No new dependencies.

**Spec:** Issue #387 (as corrected by the premise audit posted to it), plus #406 for Task 2.

## Global Constraints

- **No backwards compatibility.** Fresh installs only. No migration, no dual-read, no "old jobs behave differently". Do not add a guard whose only justification is state an earlier build wrote.
- **State has one owner.** Every derived value has exactly one function that computes it and one path that mutates it. When a check and an owner would both work, take the owner. This plan exists because that rule was violated twice.
- **Red-green is mechanical, not mental.** Every behavioural pin must be observed failing against unfixed code, with `-count=1`. A cached `ok` is not an observation. Record the observed failure message.
- **Never `git stash`** — the stash stack is shared across worktrees.
- **Sweep the literal, not the concept**, from the repository root, after each task that changes a claim.
- After every `.go` edit, immediately: `goimports -w <file>`, then `go build ./...`. These are per-edit, not per-commit, and are the ones most often skipped because the next command usually catches the failure anyway.
- Quality gates before every commit: `gofmt`, `go vet ./...`, `go test -race ./...`, `golangci-lint run ./...`, `go run ./scripts/check_dup_comments`, `go run ./scripts/check_review_banner`, plus the diff-scoped `scripts/check_coverage` and `scripts/check_test_alignment`. **Expect `check_test_alignment` to fire on Task 1** — `prefix.go` is a new file of unexported helpers.
- **The reproduction ships skipped, not failing.** `internal/assembler/overlaprange_test.go` demonstrates the undetected overlap and cannot pass until detection lands, which this plan no longer does. Committing it red would leave `main` permanently red; deleting it repeats the mistake #387 itself records ("the probe was deleted rather than committed"). So both tests carry `t.Skip` naming #387, and the full unqualified gate applies to every commit here:
  ```bash
  go test -race -count=1 ./...
  ```
  A skipped test is a known rot risk. It is accepted deliberately: the alternative is losing the only executable reproduction of the defect for a second time. Un-skipping is the first step of the detection PR.
- `ci.yml` is manual-dispatch only. Local gates are the gate of record. Never claim "CI green".

---

### Task 1: One owner for the verified-prefix walk

**Files:**
- Create: `internal/durability/prefix.go`
- Modify: `internal/durability/barrier.go` (delete `gaplessPrefix`; two `HasPrefixCRC` assignments at ~304 and ~655)
- Modify: `internal/durability/resume.go` (delete `gaplessPrefixCRC`; its caller at `:311`)
- Test: `internal/durability/prefix_test.go`

**Existing callers that must be retargeted or the package will not compile.** These are not optional and were missing from the first draft:

| Site | What it is |
|---|---|
| `internal/durability/resume_test.go:702` | 7-case table — **already covers the `consumed` clause** (`"reaching the end with a fact left over is not whole"`) and the `prefix > 0` clause (`"no facts and no bytes verifies nothing"`) |
| `internal/durability/barrier_test.go:888`, `:916` | table test + the unplaceable-fact pin, 2-value signature |
| `internal/durability/gaplessprefix_bench_test.go:56`, `:82` | whole file is named after the deleted symbol |
| `internal/durability/finalize_crc_test.go:141` | calls `gaplessPrefixCRC(nil, nil, 0)` |

**Move `resume_test.go`'s table to `prefix_test.go` and retarget it — do not leave it behind and add a near-duplicate.** It already pins two of the three properties the new tests assert; duplicating them while orphaning the originals is how coverage silently drops (the unplaceable-fact case and the CRC-value assertions have no equivalent in the new tests). Rename the bench file. The three tests in Step 1 are then *additions* covering what the tables do not.

**Interfaces:**
- Produces: `func verifiedPrefix(facts []ArticleFact, verified func(i int) bool, size int64) (verifiedTo int64, prefixCRC uint32, wholeFile bool)`
- Consumes: nothing from other tasks.

**Why a closure rather than `[]bool`:** the two existing walks differ *only* in how they decide a fact is verified, and they have genuinely different inputs. Verified against the tree during the reachability check:

```go
// barrier.go:453 — breaks on the durable bitmap, via the SyncTarget
func gaplessPrefix(facts []ArticleFact, idx int32, durable Bitmap, t SyncTarget) (verifiedTo int64, prefixCRC uint32)
//   per fact: ord, ok := t.FileLocalOrdinal(idx, f.ArtIdx); break if !ok || !durable.Get(ord)

// resume.go:410 — breaks on a precomputed slice
func gaplessPrefixCRC(facts []ArticleFact, verified []bool, size int64) (verifiedTo int64, prefixCRC uint32, wholeFile bool)
//   per fact: break if !verified[i]
```

A `func(i int) bool` is exactly that difference and nothing else. `Barrier` supplies:

```go
func(i int) bool {
    ord, ok := t.FileLocalOrdinal(idx, facts[i].ArtIdx)
    return ok && durable.Get(ord)
}
```

`Resumer` supplies `func(i int) bool { return verified[i] }`.

**Two asymmetries to carry across, both real:**

1. `barrier.go`'s walk takes no `size` and returns no `wholeFile` — the flag is computed *at the call site* (`:304`), which is precisely how it came to omit the guard. Moving the computation into `verifiedPrefix` is the fix, not an incidental tidy-up.
2. The two break in different orders (`barrier` tests offset then durability; `resume` tests verified then offset). Both still break, so `consumed` counts identically — but do not "simplify" one order into the other without checking, since `t.FileLocalOrdinal` is a call and the offset test is not.

- [ ] **Step 1: Write the failing test**

```go
func TestVerifiedPrefix_AnUnconsumedFactWithholdsTheWholeFileClaim(t *testing.T) {
	// A0 [0,100), A1 [100,200), X [150,200) — X overlaps A1 without sharing a
	// start offset. The walk tiles [0,200) from A0+A1 and breaks at X, so the
	// prefix reaches the file's end while a fact remains unconsumed. Reporting
	// wholeFile here publishes the CRC of the file that SHOULD have been
	// written, which is exactly what par2 will compare against.
	facts := []ArticleFact{
		{Offset: 0, Length: 100, CRC32: 0x11111111},
		{Offset: 100, Length: 100, CRC32: 0x22222222},
		{Offset: 150, Length: 50, CRC32: 0x33333333},
	}
	all := func(int) bool { return true }

	to, _, whole := verifiedPrefix(facts, all, 200)
	if to != 200 {
		t.Errorf("verifiedTo = %d, want 200 — A0 and A1 tile the file", to)
	}
	if whole {
		t.Error("wholeFile = true with an unconsumed overlapping fact — the CRC " +
			"describes bytes the file may not hold, and R23 wants unavailable " +
			"rather than a relabelling")
	}
}

func TestVerifiedPrefix_AGaplessFileClaimsWholeFile(t *testing.T) {
	facts := []ArticleFact{
		{Offset: 0, Length: 100, CRC32: 0x11111111},
		{Offset: 100, Length: 100, CRC32: 0x22222222},
	}
	all := func(int) bool { return true }
	if to, _, whole := verifiedPrefix(facts, all, 200); !whole || to != 200 {
		t.Errorf("verifiedTo=%d wholeFile=%v, want 200/true", to, whole)
	}
}

func TestVerifiedPrefix_AnEmptyFileClaimsNothing(t *testing.T) {
	// prefix > 0 is not redundant with the other two clauses: a zero-length
	// file with no facts satisfies both, and would report CRC32(nothing) as
	// the file's CRC while having verified nothing.
	if _, _, whole := verifiedPrefix(nil, func(int) bool { return true }, 0); whole {
		t.Error("wholeFile = true for an empty file with no facts")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -count=1 ./internal/durability/ -run TestVerifiedPrefix`
Expected: FAIL — `undefined: verifiedPrefix`.

**A compile error is not a red check.** It shows the function is absent, not that the test discriminates its behaviour. After Step 3, mutate at clause level and record each message: dropping `consumed == len(facts)` must fail test 1 and nothing else; dropping `prefix > 0` must fail test 3. If either mutation leaves all three green, that test is vacuous and must be rewritten before proceeding.

- [ ] **Step 3: Write `internal/durability/prefix.go`**

Move the doc comment currently on `resume.go`'s `gaplessPrefixCRC` wholesale — it already carries the full relabelling-guard argument and the zero-length-file argument. Add a sentence recording that this function replaced two independently-maintained walks that had diverged, and that `Barrier` was the one missing the `consumed` clause.

```go
func verifiedPrefix(facts []ArticleFact, verified func(i int) bool, size int64) (int64, uint32, bool) {
	var prefix int64
	var crc uint32
	consumed := 0
	for i, fact := range facts {
		if !verified(i) {
			break
		}
		// Not exactly abutting the run so far: either a hole, or an overlap
		// this walk cannot prove tiles the range (R23).
		if fact.Offset != prefix {
			break
		}
		crc = crc32util.Combine(crc, fact.CRC32, int64(fact.Length))
		prefix = fact.Offset + int64(fact.Length)
		consumed++
	}
	return prefix, crc, prefix > 0 && consumed == len(facts) && prefix == size
}
```

- [ ] **Step 4: Run the new tests**

Run: `go test -count=1 ./internal/durability/ -run TestVerifiedPrefix -v`
Expected: all three PASS.

- [ ] **Step 5: Rewire both callers, delete both old walks**

`resume.go`: delete `gaplessPrefixCRC`, call `verifiedPrefix(facts, func(i int) bool { return verified[i] }, size)`.

`barrier.go`: delete `gaplessPrefix`. Both `HasPrefixCRC` assignments become the third return value. **The post-truncate site at ~655 must call `verifiedPrefix` against the same `facts` slice already in scope — not re-derive from `Size` alone.** Re-deriving is the defect; an assignment that reads only `VerifiedTo == Size` reintroduces it.

- [ ] **Step 6: Prove the barrier path actually changed (red-green)**

This is the pin that matters, and Step 4's unit tests do **not** establish it — they test the new function, not that `Barrier` uses it. Revert `barrier.go`'s two assignments to `verified > 0 && verified == size`, leaving `prefix.go` intact, and confirm a barrier-level test fails. Record the message.

Add that barrier-level test first if none exists: build the three-fact contained-overlap extent through `Barrier.FinalizeFile` and assert `HasPrefixCRC == false`.

- [ ] **Step 7: Sweep and gates**

```bash
git grep -n 'gaplessPrefix'
git grep -n 'HasPrefixCRC'
```

**Do not expect `gaplessPrefix` to disappear.** The first draft of this plan asserted the sweep "must return only `docs/superpowers/plans`" — a false universal, and the exact defect class this repo's sweep discipline exists to catch. After the code deletions these prose references remain and each needs judging on its own:

| Site | Note |
|---|---|
| `internal/durability/barrier.go:680`, `:684` | `durableExtent`'s doc — the load-bearing *"Unlike gaplessPrefix…"* paragraph explaining that the two walks answer different questions. In a file this task edits |
| `internal/app/app.go:1402` | "Barrier.gaplessPrefix combines the FACTS instead" |
| `internal/crc32util/crc32combine.go:19` | cites it in a performance note |
| `internal/durability/finalize_test.go:166`, `barrier_test.go:538`, `factlog_sqlite_test.go:102` | test prose |
| `docs/superpowers/plans/*` | frozen records — **do not edit** |

`docs/durability-contract.md:1264` and `docs/post-processing-contract.md:151` name `Barrier.gaplessPrefix` as a literal. Rewrite both to name `verifiedPrefix` and to state the guard as one rule rather than two. Do not edit `docs/superpowers/plans/*` — those are frozen records.

- [ ] **Step 8: Commit**

`fix(durability): give the verified-prefix walk one owner`

Body must state: the two walks had diverged, `Barrier` was missing `consumed == len(facts)`, and the observed red from Step 6.

---

### Task 2: Delete the two unreachable displacement backstops

**Files:**
- Modify: `internal/assembler/writecache.go` (**only** the inner `if !existing.id.sameArticle(art.id)` sub-branch and `buffer`'s `displaced` return)
- Modify: `internal/assembler/filewriter.go` (`Accept`'s `for _, d := range displaced` loop, ~line 682)
- Modify: `internal/assembler/durable_ack_test.go:324` (calls `cached, displaced := wc.buffer(...)`)

Closes #406.

> **#406's own body is WRONG on this point and must be corrected, not followed.** It says "delete both", meaning the whole eviction branch. Deleting the enclosing `if existing, dup := fb.articles[art.offset]; dup` block removes live accounting:
>
> ```go
> wc.used -= int64(len(existing.data))
> fb.totalBytes -= int64(len(existing.data))
> if existing.data != nil { decoder.PutBuffer(existing.data) }
> ```
>
> That path **is reachable** — `TestFileWriter_ReacceptWhileCachedIsNotSelfDisplacement` (`offsetcollision_test.go:548`) accepts the same article twice while cache-resident, so `dup == true` with `sameArticle == true`. Deleting it makes `fb.articles[art.offset] = art` overwrite without subtracting or pooling: a permanent `wc.used` inflation and a leaked pooled buffer.
>
> Only the `!sameArticle` sub-branch is dead. **And that test asserts nothing about `wc.used`, so Step 3's "no test changed result" check would sail straight past the leak** — the vacuous-verification shape this repo keeps shipping. Step 2 below adds the missing assertion first.

- [ ] **Step 1: Confirm unreachability before deleting**

Both comments already assert it. Verify rather than trust: `Accept` calls `w.wc.discardAt(w.key, off)` before `buffer`, and `discardAt` deletes the map entry, so `buffer` finds no incumbent.

```bash
go test -count=1 ./internal/assembler/
```
Note the baseline, then delete and re-run: no test may change result. **A test that fails here means the code was reachable and this task is wrong — stop and report.**

- [ ] **Step 2: Pin the accounting FIRST, then delete only the dead sub-branch**

Before deleting anything, add to `TestFileWriter_ReacceptWhileCachedIsNotSelfDisplacement` an assertion on `wc.bytesFor(key)` after the second accept. Without it nothing observes the eviction accounting, and the deletion below cannot be verified as safe.

Then delete **only** the `if !existing.id.sameArticle(art.id) { displaced = ... }` sub-branch and `buffer`'s `displaced []articleID` return. Keep `wc.used -=`, `fb.totalBytes -=` and `decoder.PutBuffer`.

`Accept` loses the loop, and with it `alreadyFailed`/`didFail` if nothing else reads them — check before removing.

- [ ] **Step 3: Run the suite**

Run: `go test -race -count=1 ./internal/assembler/`
Expected: PASS, with no test result changed from Step 1's baseline.

- [ ] **Step 4: Sweep**

```bash
git grep -n 'displaced'
```
`writecache.go`'s and `filewriter.go`'s remaining comments describe the deleted branch at length. Delete the paragraphs that describe it; keep the ones describing `discardAt`'s live ownership.

- [ ] **Step 5: Commit**

`refactor(assembler): delete the two unreachable displacement backstops`

Body: name `discardAt` as the live owner, and state that no test changed result — which is the evidence the code was dead.

---

### Deferred: range-overlap detection in `FileWriter`

Removed from this plan after two review rounds broke the proposed `claimIndex`.
The reproduction (`internal/assembler/overlaprange_test.go`) ships **skipped**,
so the evidence stays in the tree rather than being deleted as #387's original
probe was. Un-skip it when detection lands.

The constraints any replacement design must satisfy are recorded on #387.
The load-bearing one, because it is the least obvious: `req.Offset` is
attacker-controlled (`assembler.go:1788`) and a same-article retry may return
at a different offset and length, so a structure that makes `start` load-bearing
for its own ordering can be permanently corrupted by a hostile server. The
current `map[int64]offsetOwner` is immune to that by construction.


## Inconclusive / Deferred items

- **Cross-restart collisions remain invisible.** `claimIndex`, like `acceptedAt`, is per-open-episode and rebuilt empty on every file open. Only Class A facts persist and nothing sweeps them for overlap at startup. Both sketch authors named this independently. *Resolution point:* if Task 1's barrier guard proves insufficient in practice — file separately, do not widen scope here.
- **Ingestion-time rejection** of NZBs whose segments imply overlapping ranges is not attempted, and cannot be: NZB segments carry no offsets (`git grep -i offset internal/nzb/*.go` returns nothing outside tests). The layout is downloaded data. *Not resolvable at any layer.*
- **#382's disposition enum** is deliberately not folded in. Task 4 makes it marginally harder by allowing N losers; that is accepted and recorded on #382. *Resolution point:* whoever implements #382 designs `Displaced` against a set.
- **Whether `claimIndex` should be shared with `internal/durability`** as a common "claimed range" concept, per the structural sketch. **Decided, not deferred: no.** The reason is the two access patterns, not a layer-boundary convention. `verifiedPrefix` makes one forward pass over a pre-sorted, immutable fact slice and asks only "is fact `i` verified" by index — it never inserts and never searches. `claimIndex` answers overlap queries against a mutable set that grows as articles arrive in unpredictable order. One type serving both would have to support insertion for a caller that never inserts, or forgo it for a caller that always does. Recording this as a decision rather than a deferral so a later change does not reopen it expecting new evidence to help — the shapes settle it.
