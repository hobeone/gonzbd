# Re-key FileWriter Dedup on ArtIdx — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Key `FileWriter`'s duplicate-handling sets on the article index rather than the Message-ID, so the empty-key class stops existing and the machinery that compensated for it has nothing left to do.

**Architecture:** `seenDone` and `seenFailed` become `map[int32]struct{}` keyed on `ArtIdx`. `int32` has no value that doubles as "absent", so every `msgID == ""` branch becomes unreachable and is deleted rather than defended. `resolvedUntracked` loses its job — recording a terminal disposition is what `seenFailed` already does for every other article. The assembler stops depending on invariant A7 (Message-ID uniqueness across the NZB) for its own correctness.

**Tech Stack:** Go 1.26.6, standard `testing`, no new dependencies.

**Spec:** `docs/article-validation-contract.md` §4 (enforcement ladder) and §5.F row F1; issue #392 (rewritten body) and its alternatives comment.

## Global Constraints

- **Go 1.26.6.** After editing any `.go` file: `goimports -w <file>`, `go fix ./...`, `go build ./...`.
- **Gates before every commit:** `go vet ./...`, `go test -race ./...`, `golangci-lint run ./...` (must report 0 issues), `go run ./scripts/check_dup_comments`, `go run ./scripts/check_review_banner`. `internal/app` is touched, so `go test -tags=integration ./test/integration/...` too.
- **The red check is mechanical, not mental.** Every task's "verify it fails" step must be *observed*, with `-count=1` (a cached `ok` is not an observation). Record the observed failure message in the commit body.
- **Never `git stash`** — the stash stack is shared with other worktrees. Copy the file to `$(mktemp -d)` and restore from that copy.
- **No backwards compatibility.** Persisted manifests, queue rows and NZB backups written by an earlier build may be assumed to satisfy any invariant this change introduces. Do not add a guard whose only justification is older on-disk state.
- **Do not touch `internal/queue/manifest.go`'s `MessageIDIsFetchable` re-check.** It is command-injection defence under the Standing Design Rules' security carve-out, not part of this machinery.
- **Conventional Commits**, scope `assembler`. Footer `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.
- **Claim sweep (AGENTS.md step 4):** several multi-paragraph doc comments narrate the "no map can hold an empty key" reasoning. They must be rewritten to state the new invariant, not merely deleted. Sweep for the literal from the repository root, not just in the files you edited.

---

## File Structure

| File | Responsibility in this change |
|---|---|
| `internal/assembler/filewriter.go` | The two maps, their key type, and every function that reads or writes them. All `msgID == ""` guards live here except two. |
| `internal/assembler/assembler.go` | The three read sites: `handleSuccessArticle`, `handleLateDuplicate`, `routeAcceptFailure`. |
| `internal/assembler/writecache.go` | `articleID`'s declaration and one whole-struct comparison. |
| `internal/assembler/accounting_test.go` | Tests pinning `giveBackUntrackedPart` and empty-ID admission. |
| `internal/assembler/displacedcompletion_test.go` | The repository's only construction of an empty Message-ID. |
| `docs/article-validation-contract.md` | §5.F row F1 status; §4's tier-2 discussion. |

---

### Task 1: Delete `resolvedUntracked`

`resolvedUntracked` exists to record a terminal disposition for an article no Message-ID-keyed map can hold. Its writer is `failDisplaced`'s `else` arm and its only reader is `handleSuccessArticle`'s `else if` arm. Both are reachable only with an empty Message-ID.

Collapsing `failDisplaced`'s branch makes a displaced article take `admitPermanentFailure` like every other displaced article already does — the asymmetry exists only because of the empty key.

**Files:**
- Modify: `internal/assembler/filewriter.go` (field, `newFileWriter` init, `failDisplaced`)
- Modify: `internal/assembler/assembler.go` (`handleSuccessArticle`'s `else if` arm)
- Test: `internal/assembler/displacedcompletion_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `func (w *FileWriter) failDisplaced(id articleID, off int64, by articleID)` — unchanged signature, unconditional body.

- [ ] **Step 1: Write the failing test**

Add to `internal/assembler/displacedcompletion_test.go`. This pins that a displaced article is counted exactly once and its redelivery is refused — the behaviour `resolvedUntracked` provided for untracked articles and `seenFailed` provides for every other one.

```go
// TestFailDisplaced_CountsEveryDisplacedArticleTheSameWay pins that a displaced
// article's disposition is recorded through admitPermanentFailure regardless of
// whether it carries a Message-ID.
//
// Before this change failDisplaced branched: an article with an ID went to
// admitPermanentFailure, one without went to resolvedUntracked. The branch was
// reachable only with an empty ID, which no production path produces.
func TestFailDisplaced_CountsEveryDisplacedArticleTheSameWay(t *testing.T) {
	w := newHelperFile(t, t.TempDir(), "displaced.dat", 0).w

	loser := articleID{msgID: "loser@t", artIdx: 1}
	winner := articleID{msgID: "winner@t", artIdx: 2}
	w.failDisplaced(loser, 0, winner)

	if got := w.parts(); got != 1 {
		t.Errorf("parts() = %d after one displacement, want 1", got)
	}
	// A second displacement of the same article must not count again.
	w.failDisplaced(loser, 0, winner)
	if got := w.parts(); got != 1 {
		t.Errorf("parts() = %d after redelivering the displaced article, want 1 — "+
			"the disposition was not recorded, so it was counted twice", got)
	}
}
```

- [ ] **Step 2: Run it and confirm it passes on unmodified code**

Run: `go test -count=1 ./internal/assembler/ -run TestFailDisplaced_CountsEveryDisplacedArticleTheSameWay -v`
Expected: PASS. This test is a *regression guard*, not a red-green pin — the behaviour it asserts already holds for articles with a Message-ID, and the change must not alter it. Its value is that it fails if the collapse is done wrongly.

- [ ] **Step 3: Delete the field and its initialiser**

In `internal/assembler/filewriter.go`, delete the `resolvedUntracked map[int32]struct{}` field together with its doc comment block, and delete the `resolvedUntracked: make(map[int32]struct{}),` line from `newFileWriter`.

- [ ] **Step 4: Collapse `failDisplaced`'s branch**

Replace:

```go
	if id.msgID != "" {
		w.admitPermanentFailure(id.msgID)
	} else {
		w.resolvedUntracked[id.artIdx] = struct{}{}
	}
```

with:

```go
	w.admitPermanentFailure(id.msgID)
```

- [ ] **Step 5: Delete the reader arm in `handleSuccessArticle`**

In `internal/assembler/assembler.go`, delete the entire `} else if _, resolved := w.resolvedUntracked[req.ArtIdx]; resolved {` arm and its ~14-line comment, so the `if req.MessageID != "" {` block simply closes with `}`.

- [ ] **Step 6: Rewrite the comments this falsified**

`failDisplaced`'s doc, `rollbackPart`'s doc (which names `giveBackUntrackedPart` and the removed branch), and `admitAccepted`'s doc all narrate the untracked path. Rewrite them to describe what the code now does. Then sweep:

```bash
git grep -n 'resolvedUntracked'
git grep -n 'untracked'
```

- [ ] **Step 7: Run the gates and commit**

```bash
goimports -w internal/assembler/ && go build ./... && go vet ./... && go test -race -count=1 ./...
golangci-lint run ./... && go run ./scripts/check_dup_comments
git add internal/assembler/ && git commit
```

Message: `refactor(assembler): delete resolvedUntracked, whose only writer was the empty-ID branch`

---

### Task 2: Re-key `seenDone` and `seenFailed` on `ArtIdx`

The core change. `ArtIdx` is a flat global index into `Manifest.articleIDs`, and `fileArticleOffsets` gives each file a contiguous range of it — so within one `FileWriter` the indices are distinct by construction, which is what makes them a sound key.

**This task is behaviourally inert for every reachable input, and the implementer must not go looking for a red-green cycle that does not exist.** Within a file, `msgID` and `ArtIdx` are in bijection: A7 makes the Message-ID unique document-wide (`partitionSegments` carries a `seenIDs` set and drops repeats), and the index is unique by construction. A redelivery carries both unchanged. Keying on either therefore produces identical behaviour for every input the parser can produce, and the restore path is assumed to satisfy A7 under the no-backcompat ground rule rather than re-checking it.

What the change buys is structural, not corrective: the empty-key class stops existing rather than being guarded, and the assembler stops *resting on* A7 for its own correctness — so a future relaxation of A7's scope, or a defect in its enforcement, can no longer silently short a file here. Neither is a defect being fixed today.

The verification is therefore **inertness**, not a new failing test: the full suite must pass unchanged. A test that fails during this task is evidence of a mistake in the re-key, or a test that was pinning the Message-ID key itself — triage it, do not adjust production code to keep it green.

**Files:**
- Modify: `internal/assembler/filewriter.go` (both fields, `newFileWriter`, `admitAccepted`, `admitRetryOfFailed`, `admitPermanentFailure`, `rollbackPart`, `failPermanent`)
- Modify: `internal/assembler/assembler.go` (`handleSuccessArticle`, `handleLateDuplicate` read sites)
- Test: `internal/assembler/assembler_test.go`

**Interfaces:**
- Consumes: `failDisplaced` from Task 1, calling `admitPermanentFailure`.
- Produces:
  - `func (w *FileWriter) admitAccepted(artIdx int32)`
  - `func (w *FileWriter) admitRetryOfFailed(artIdx int32)`
  - `func (w *FileWriter) admitPermanentFailure(artIdx int32) bool`
  - fields `seenDone map[int32]struct{}`, `seenFailed map[int32]struct{}`

- [ ] **Step 1: Write the design pin**

Add to `internal/assembler/assembler_test.go`. This is **not** a red-green pin and its comment must not pretend otherwise — see the inertness note above. It pins a design decision, using an input the parser cannot produce, and its job is to fail if someone re-keys on the Message-ID again.

```go
// TestFileWriter_ArticleIdentityIsTheIndexNotTheMessageID pins that the
// assembler tells articles apart by index.
//
// The input is one the parser cannot produce: A7 makes a repeated Message-ID
// impossible in a parsed NZB, and the restore path is assumed to satisfy A7
// rather than re-checking it. So this documents a property, not a defect — for
// every reachable input the two keys are in bijection and behave identically.
//
// It is worth pinning anyway because the property is invisible otherwise. While
// dedup keyed on the Message-ID, the assembler's correctness rested on A7
// holding; re-keying removed that dependence, and nothing else in the suite
// would notice if it were reintroduced.
func TestFileWriter_ArticleIdentityIsTheIndexNotTheMessageID(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	path := registerFile(t, dir, files, "job1", 0, 2)

	completed := make(chan int, 1)
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(_ string, fileIdx int) { completed <- fileIdx }
	a := startAssembler(t, opts)

	// Same Message-ID, different article indices, different offsets.
	for _, art := range []struct {
		artIdx int32
		off    int64
		data   string
	}{
		{0, 0, "AAAA"},
		{1, 4, "BBBB"},
	} {
		if err := a.WriteArticle(t.Context(), ArticleRef{
			JobID: "job1", FileIdx: 0, ArtIdx: art.artIdx, MessageID: "shared@t",
		}, WriteRequest{Offset: art.off, Data: []byte(art.data)}); err != nil {
			t.Fatalf("WriteArticle %d: %v", art.artIdx, err)
		}
	}

	select {
	case got := <-completed:
		if got != 0 {
			t.Errorf("completed file %d, want 0", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the file never completed: the second article was taken for a " +
			"duplicate of the first because they share a Message-ID, so " +
			"identity is not the index")
	}
	if got := readFile(t, path); string(got) != "AAAABBBB" {
		t.Errorf("file contents = %q, want %q", got, "AAAABBBB")
	}
}
```

- [ ] **Step 2: Run it against the UNCHANGED code and record what it does**

Run: `go test -count=1 ./internal/assembler/ -run TestFileWriter_ArticleIdentityIsTheIndexNotTheMessageID -v`
Expected: FAIL after ~5s, because dedup still keys on the Message-ID.

Record the message, but describe it accurately in the commit body: this is a **constructed** state, not a reproduction of a reachable defect. Do not write "fixes a bug where files finalize short" — no in-scope input reaches this. The honest claim is "the assembler distinguished articles by Message-ID; it now distinguishes them by index, and this pins the difference."

If it *passes* against unchanged code, the fixture is not reaching the dedup arm — fix the fixture before proceeding, rather than concluding the task is unnecessary.

- [ ] **Step 3: Change the field types**

In `internal/assembler/filewriter.go`:

```go
	seenDone   map[int32]struct{}
	seenFailed map[int32]struct{}
```

and in `newFileWriter`:

```go
		seenDone:   make(map[int32]struct{}),
		seenFailed: make(map[int32]struct{}),
```

- [ ] **Step 4: Re-key the three admit functions**

`admitAccepted` loses its guard entirely — every `int32` is a valid key, so there is nothing to skip:

```go
func (w *FileWriter) admitAccepted(artIdx int32) {
	w.seenDone[artIdx] = struct{}{}
	w.partsWritten++
}
```

`admitRetryOfFailed`:

```go
func (w *FileWriter) admitRetryOfFailed(artIdx int32) {
	w.seenDone[artIdx] = struct{}{}
}
```

`admitPermanentFailure`:

```go
func (w *FileWriter) admitPermanentFailure(artIdx int32) bool {
	if _, dup := w.seenFailed[artIdx]; dup {
		return false
	}
	_, alreadyCounted := w.seenDone[artIdx]
	w.seenFailed[artIdx] = struct{}{}
	// An article already counted as a success must not increment the part
	// total a second time.
	if alreadyCounted {
		return false
	}
	w.partsWritten++
	return true
}
```

Delete the whole paragraph in `admitPermanentFailure`'s doc that begins "The empty Message-ID is deliberately NOT guarded" — it documents a question this change answers. Delete `admitRetryOfFailed`'s "The caller owns the precondition that msgID is non-empty" paragraph for the same reason.

- [ ] **Step 5: Re-key `rollbackPart` and `failPermanent`**

In `rollbackPart`, replace the three `id.msgID` keyings with `id.artIdx`. In `failPermanent`, the same, and delete its `if id.msgID == "" { return }` guard:

```go
func (w *FileWriter) failPermanent(id articleID) {
	delete(w.seenDone, id.artIdx)
	w.seenFailed[id.artIdx] = struct{}{}
}
```

Delete `rollbackPart`'s `# Precondition: id.msgID is non-empty` block — the precondition dissolves with the key change.

- [ ] **Step 6: Update the call sites and read sites**

`failDisplaced` now calls `w.admitPermanentFailure(id.artIdx)`. In `internal/assembler/assembler.go`, `handleSuccessArticle`'s two lookups become `w.seenDone[req.ArtIdx]` and `w.seenFailed[req.ArtIdx]`, and its `admitRetryOfFailed`/`admitAccepted` calls take `req.ArtIdx`. `handleLateDuplicate`'s two lookups become `f.w.seenDone[req.ArtIdx]` and `f.w.seenFailed[req.ArtIdx]`.

Leave `handleLateDuplicate`'s `req.MessageID == ""` guard and `fail`'s early return in place — Task 3 and Task 4 remove them, and removing `fail`'s here without `giveBackUntrackedPart` would double-charge `partsWritten`.

- [ ] **Step 7: Run the design pin and confirm it PASSES**

Run: `go test -count=1 ./internal/assembler/ -run TestFileWriter_ArticleIdentityIsTheIndexNotTheMessageID -v`
Expected: PASS.

- [ ] **Step 8: Run the full gates**

```bash
goimports -w internal/assembler/ && go build ./... && go vet ./... && go test -race -count=1 ./...
golangci-lint run ./... && go test -count=1 -tags=integration ./test/integration/...
```

Any test that fails here is a test asserting Message-ID-keyed dedup. Triage each one: rewrite it to assert index-keyed dedup, or delete it if it pins only the empty-key behaviour. Do not adjust the production code to keep such a test green.

- [ ] **Step 9: Commit**

Message: `refactor(assembler): key duplicate handling on ArtIdx, not Message-ID`. Body must carry the observed red message from Step 2 and state that the assembler no longer depends on A7.

---

### Task 3: Delete `giveBackUntrackedPart` and `fail`'s early return

These two must go in **one commit**. `routeAcceptFailure` calls `giveBackUntrackedPart` for an empty-ID article, and `fail` returns early for the same case precisely so the decrement is not charged twice. Removing either alone mis-counts `partsWritten`.

**Files:**
- Modify: `internal/assembler/filewriter.go` (`fail`, `giveBackUntrackedPart`)
- Modify: `internal/assembler/assembler.go` (`routeAcceptFailure`)
- Test: `internal/assembler/accounting_test.go`

**Interfaces:**
- Consumes: index-keyed `seenDone`/`seenFailed` and `rollbackPart` from Task 2.
- Produces: `func (w *FileWriter) fail(id articleID)` with an unconditional body. `giveBackUntrackedPart` no longer exists.

- [ ] **Step 1: Write the failing test**

Add to `internal/assembler/accounting_test.go`:

```go
// TestFileWriter_FailRollsBackEveryArticlesPart pins that fail decrements the
// part count for any admitted article, with no early return.
//
// fail used to skip articles with no Message-ID because rollbackPart could not
// find them in a Message-ID-keyed map; routeAcceptFailure gave their part back
// separately through giveBackUntrackedPart. With the maps keyed on ArtIdx there
// is one path, and this asserts it charges exactly once.
func TestFileWriter_FailRollsBackEveryArticlesPart(t *testing.T) {
	w := newHelperFile(t, t.TempDir(), "rollback.dat", 0).w

	id := articleID{msgID: "a@t", artIdx: 7}
	w.admitAccepted(id.artIdx)
	if got := w.parts(); got != 1 {
		t.Fatalf("parts() = %d after admit, want 1", got)
	}
	w.fail(id)
	if got := w.parts(); got != 0 {
		t.Errorf("parts() = %d after fail, want 0 — the part was not rolled back", got)
	}
	if _, still := w.seenDone[id.artIdx]; still {
		t.Error("the article kept its seenDone entry after fail")
	}
}
```

- [ ] **Step 2: Run it to see where it stands**

Run: `go test -count=1 ./internal/assembler/ -run TestFileWriter_FailRollsBackEveryArticlesPart -v`
Expected: PASS on the Task 2 code, because this article has a Message-ID. It is a guard against the *deletion* being done wrongly, and Step 6 below is where its red is observed.

- [ ] **Step 3: Delete the two existing empty-ID accounting tests**

Delete `TestFileWriter_GiveBackUntrackedPartReturnsTheCount` and `TestFileWriter_GiveBackUntrackedPartIsClampedAtZero` from `internal/assembler/accounting_test.go`, along with any `TestFileWriter_AdmitCountsAnArticleWithNoMessageID` and the empty-ID row of the dedup table test. They pin behaviour that no longer exists.

- [ ] **Step 4: Remove `fail`'s early return**

```go
func (w *FileWriter) fail(id articleID) {
	w.rollbackPart(id)
	w.faulted = append(w.faulted, faultedArticle{id: id})
}
```

- [ ] **Step 5: Delete `giveBackUntrackedPart` and its call site**

Delete the whole `giveBackUntrackedPart` function and its doc from `filewriter.go`. In `assembler.go`'s `routeAcceptFailure`, delete the comment block and the `if req.MessageID == "" { f.w.giveBackUntrackedPart() }` statement, leaving `a.releaseFaulted(...)` and its comment intact.

- [ ] **Step 6: Observe the red for the pairing**

This is the constraint's own check, and it must be observed rather than reasoned about. Restore `fail`'s early return alone, leaving `giveBackUntrackedPart` deleted:

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/assembler/filewriter.go "$SCRATCH/filewriter.bak.go"
# re-add `if id.msgID == "" { return }` to fail
go test -count=1 ./internal/assembler/ -run TestFileWriter_FailRollsBackEveryArticlesPart
cp "$SCRATCH/filewriter.bak.go" internal/assembler/filewriter.go
```

Expected: the test still passes, because its article has a Message-ID. That is the finding, not a failure — record in the commit body that the pairing is enforced by the contract and by `partsWritten` arithmetic rather than by a test, since no test can construct the empty-ID article the double-charge required.

- [ ] **Step 7: Gates and commit**

Message: `refactor(assembler): delete giveBackUntrackedPart and fail's empty-ID return`

---

### Task 4: Delete the residual guards and narrow `articleID` equality

**Files:**
- Modify: `internal/assembler/assembler.go` (`handleLateDuplicate`)
- Modify: `internal/assembler/filewriter.go` (comparisons at `noteWritten`, `offsetSettledBy`, `Accept`)
- Modify: `internal/assembler/writecache.go` (`articleID` doc, comparison)
- Test: `internal/assembler/assembler_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–3.
- Produces: `articleID` compared by `artIdx` alone; `msgID` retained for logging only.

- [ ] **Step 1: Write the failing test**

```go
// TestHandleLateDuplicate_ResolvesAnArticleWhateverItsIdentity pins that a late
// article for a completed file is resolved by index rather than skipped.
func TestHandleLateDuplicate_ResolvesEveryUnacceptedArticle(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 1)

	rejected := make(chan int32, 1)
	opts := makeOpts(dir, files)
	opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) { rejected <- artIdx }
	a := startAssembler(t, opts)

	if err := a.WriteArticle(t.Context(), ArticleRef{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, MessageID: "first@t",
	}, WriteRequest{Offset: 0, Data: []byte("AAAA")}); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}
	// A second article arrives for the now-complete file, never accepted.
	if err := a.WriteArticle(t.Context(), ArticleRef{
		JobID: "job1", FileIdx: 0, ArtIdx: 1, MessageID: "late@t",
	}, WriteRequest{Offset: 0, Data: []byte("BBBB")}); err != nil {
		t.Fatalf("WriteArticle late: %v", err)
	}
	select {
	case got := <-rejected:
		if got != 1 {
			t.Errorf("rejected ArtIdx %d, want 1", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the late article was never resolved, so the job waits on it forever")
	}
}
```

- [ ] **Step 2: Run it**

Run: `go test -count=1 ./internal/assembler/ -run TestHandleLateDuplicate_ResolvesEveryUnacceptedArticle -v`
Expected: PASS. Record which arm it exercises; if it fails, the fixture does not reach `handleLateDuplicate` and must be corrected before proceeding.

- [ ] **Step 3: Narrow `handleLateDuplicate`'s guard**

```go
	if f == nil {
		return
	}
```

- [ ] **Step 4: Narrow the identity comparisons**

At `noteWritten`, `offsetSettledBy` and `Accept` in `filewriter.go`, and the eviction comparison in `writecache.go`, replace whole-struct equality with `artIdx` comparison — `owner.id.artIdx == id.artIdx` and so on. Update `articleID`'s doc in `writecache.go` to say that `msgID` travels with the article for logging and telemetry and is **not** part of its identity, so a reader is not left wondering why a field is present but not compared.

- [ ] **Step 5: Gates and commit**

Message: `refactor(assembler): compare articles by index, not by Message-ID`

---

### Task 5: Update the contract and sweep the claims

**Files:**
- Modify: `docs/article-validation-contract.md`
- Possibly modify: `docs/ARCHITECTURE.md`, `docs/durability-contract.md`

- [ ] **Step 1: Mark F1 landed**

In §5.F, update row F1 from its pending form to `✅ **done**` with the PR number, matching the style rows F2, F5, F6 and F7 already use. Update §4's tier-2 discussion to say the re-key has landed and what it retired.

- [ ] **Step 2: Update the A7 dependence claims**

§5.A argues A7 is load-bearing partly because two structures downstream key on the Message-ID. One of them no longer does. Read §5.A **in full** and correct the argument rather than grepping for a symbol — the claim is stated in prose that shares no token with the code.

- [ ] **Step 3: Sweep the literals from the repository root**

```bash
git grep -n 'seenDone'
git grep -n 'seenFailed'
git grep -n 'untracked'
git grep -n 'no map can hold'
git grep -n 'empty key'
```

Rewrite every hit that states the old rule. `docs/reviews/*.md` are frozen records — leave them.

- [ ] **Step 4: Run the whole-repo gates and commit**

```bash
go run ./scripts/check_dup_comments && go run ./scripts/check_review_banner
```

Message: `docs: record that FileWriter keys on ArtIdx and no longer needs A7`

---

## Self-Review

**Spec coverage.** F1's named deletions map to tasks as follows: `resolvedUntracked` → Task 1; the `resolvedUntracked` consult in `handleSuccessArticle` → Task 1; `giveBackUntrackedPart` → Task 3; the `msgID == ""` early returns in `fail`/`failPermanent`/`failDisplaced` → Tasks 2 and 3; the key change itself → Task 2. The `req.MessageID == ""` arm in `handleLateDuplicate` → Task 4. The contract's ordering constraint is honoured by Task 3 doing both halves.

**Type consistency.** `admitAccepted`, `admitRetryOfFailed` and `admitPermanentFailure` take `artIdx int32` from Task 2 onward; Task 1 calls `admitPermanentFailure(id.msgID)` because it runs *before* the re-key, and Task 2 Step 6 changes that call. That ordering is deliberate and stated in both tasks.

**Known gap, stated rather than hidden.** Task 3's pairing constraint cannot be pinned by a test: the double-charge it prevents requires an empty-ID article, which no path can construct once Task 2 lands. The commit body must say so instead of implying a red check that did not happen.

**What this change is, stated plainly.** It is inert for every reachable input, and no task here fixes a defect a user could hit today. The four accounting defects #392 was filed about became unreachable when #395 landed; the empty-key machinery has been dead code since. This plan removes the class rather than the dead code, so that the guards cannot come back and the assembler's correctness stops resting on A7. Every commit body must be honest about that — a red check on a constructed input is evidence the code changed, not evidence a bug existed. The one claim worth making is the structural one.
