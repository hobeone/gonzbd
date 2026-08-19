# Re-key FileWriter Dedup on ArtIdx — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Key `FileWriter`'s duplicate-handling sets on the article index rather than the Message-ID, so the empty-key class stops existing and the machinery that compensated for it has nothing left to do.

**Architecture:** `seenDone` and `seenFailed` become `map[int32]struct{}` keyed on `ArtIdx`. `int32` has no value that doubles as "absent", so every `msgID == ""` branch becomes unreachable and is deleted rather than defended. `resolvedUntracked` loses its job — recording a terminal disposition is what `seenFailed` already does for every other article. The assembler stops depending on invariant A7 (Message-ID uniqueness) for its own correctness.

**Tech Stack:** Go 1.26.6, standard `testing`, no new dependencies.

**Spec:** `docs/article-validation-contract.md` §4 (enforcement ladder) and §5.F row F1; issue #392 (rewritten body) and its alternatives comment.

## What this change is, stated plainly

It is **inert for every reachable input**, and no task here fixes a defect a user could hit today. The four accounting defects #392 was filed about became unreachable when #395 landed; the empty-key machinery has been dead code since.

The inertness rests on `ArtIdx` and `msgID` being in bijection for every article the assembler can see: `ArtIdx` is the flat global manifest index, unique per job by construction, and `fileKey` carries `jobID` — so a `FileWriter` never sees two articles with the same index. A7 makes the Message-ID unique document-wide. Redelivery, retry-after-permanent-failure, displacement, write-fault rollback and the late-duplicate path all carry the same `(msgID, artIdx)` pair unchanged.

What the change buys is structural: the empty-key class stops existing rather than being guarded, and the assembler stops *resting on* A7, so a future relaxation of A7's scope cannot silently short a file here.

**Every commit body must be honest about this.** A red check on a constructed input is evidence the code changed, not evidence a bug existed. Do not write "fixes a bug where files finalize short" — no in-scope input reaches that.

## Global Constraints

- **Go 1.26.6.** After editing any `.go` file: `goimports -w <file>`, `go fix ./...`, `go build ./...`.
- **Gates before every commit:** `go vet ./...`, `go test -race ./...`, `golangci-lint run ./...` (0 issues), `go run ./scripts/check_dup_comments`, `go run ./scripts/check_review_banner`.
- **Run the integration suite too** — `go test -tags=integration ./test/integration/...`. No task modifies `internal/app`, but the assembler's completion accounting is what those tests exercise end to end.
- **The red check is mechanical, not mental.** Every "verify it fails" step must be *observed*, with `-count=1`. Record the observed message in the commit body.
- **Never `git stash`** — the stash stack is shared across worktrees. Copy to `$(mktemp -d)` and restore from that copy.
- **No backwards compatibility.** Do not add a guard whose only justification is state an earlier build wrote.
- **Do not touch `internal/queue/manifest.go`'s `MessageIDIsFetchable` re-check.** It is command-injection defence under the security carve-out.
- **Conventional Commits**, scope `assembler`. Footer `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.
- **Use the `writeArticle` helper** (`internal/assembler/helpers_test.go`) in tests rather than calling `a.WriteArticle(ctx, ArticleRef{...}, req)` directly. It derives the ref from the request, which is what ~15 existing call sites do. The one documented exception is the ref-authority pin, which needs ref and request to *disagree*; no test in this plan does.

---

## File Structure

| File | Responsibility in this change |
|---|---|
| `internal/assembler/filewriter.go` | The two maps, their key type, every function reading or writing them, and the doc comments narrating the empty-key reasoning. |
| `internal/assembler/assembler.go` | Four sites: `handleSuccessArticle`, `handleLateDuplicate`, `routeAcceptFailure`, `handleFatalArticle`. |
| `internal/assembler/writecache.go` | `articleID`'s declaration, its identity method, and one comparison. |
| `internal/assembler/accounting_test.go` | Tests pinning `giveBackUntrackedPart` and empty-ID admission. |
| `internal/assembler/displacedcompletion_test.go` | The repository's only construction of an empty Message-ID. |
| `docs/article-validation-contract.md`, `internal/nzb/model.go` | Claims about A7 being load-bearing for the assembler. |

---

### Task 1: Re-key on `ArtIdx` and delete `resolvedUntracked`

These are **one task, not two**. `handleSuccessArticle`'s dedup arms sit inside `if req.MessageID != ""`, and the `resolvedUntracked` arm is that conditional's `else if`. The re-key removes the wrapper, so the arm must go with it.

Splitting them creates a defect that exists in neither endpoint: with `resolvedUntracked` deleted but the wrapper still keying on `msgID`, an empty-ID loser sits in `seenFailed[""]`, matches neither arm on redelivery, falls through to `admitAccepted("")` and `acceptArticle`, and ping-pongs with the article that owns its offset — the count-climb documented at `filewriter.go:107-114`.

**Files:**
- Modify: `internal/assembler/filewriter.go` — both map fields, `resolvedUntracked` field, `newFileWriter`, `admitAccepted`, `admitRetryOfFailed`, `admitPermanentFailure`, `rollbackPart`, `failPermanent`, `failDisplaced`
- Modify: `internal/assembler/assembler.go` — `handleSuccessArticle`, `handleLateDuplicate`, `handleFatalArticle`
- Test: `internal/assembler/assembler_test.go`

**Interfaces:**
- Produces:
  - `func (w *FileWriter) admitAccepted(artIdx int32)`
  - `func (w *FileWriter) admitRetryOfFailed(artIdx int32)`
  - `func (w *FileWriter) admitPermanentFailure(artIdx int32) bool`
  - fields `seenDone map[int32]struct{}`, `seenFailed map[int32]struct{}`
  - `resolvedUntracked` no longer exists.

- [ ] **Step 1: Write the design pin**

Add to `internal/assembler/assembler_test.go`. This is **not** a red-green pin on a defect — see "What this change is" above. It pins a design decision using an input the parser cannot produce, and its job is to fail if someone re-keys on the Message-ID.

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
// holding, and nothing else in the suite would notice if that were reintroduced.
func TestFileWriter_ArticleIdentityIsTheIndexNotTheMessageID(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	path := registerFile(t, dir, files, "job1", 0, 2)

	completed := make(chan int, 1)
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(_ string, fileIdx int) { completed <- fileIdx }
	a := startAssembler(t, opts)

	// Same Message-ID, different indices, different offsets.
	for _, art := range []struct {
		artIdx int32
		off    int64
		data   string
	}{
		{0, 0, "AAAA"},
		{1, 4, "BBBB"},
	} {
		if err := writeArticle(t.Context(), a, WriteRequest{
			JobID: "job1", FileIdx: 0, ArtIdx: art.artIdx, MessageID: "shared@t",
			Offset: art.off, Data: []byte(art.data),
		}); err != nil {
			t.Fatalf("writeArticle %d: %v", art.artIdx, err)
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

- [ ] **Step 2: Run it against unchanged code and record the message**

Run: `go test -count=1 ./internal/assembler/ -run TestFileWriter_ArticleIdentityIsTheIndexNotTheMessageID -v`
Expected: FAIL after ~5s, because dedup still keys on the Message-ID. Record the message. Describe it in the commit body as a constructed state, not a reproduction.

If it *passes*, the fixture is not reaching the dedup arm — fix the fixture before proceeding, rather than concluding the task is unnecessary.

- [ ] **Step 3: Change the field types and delete `resolvedUntracked`**

```go
	seenDone   map[int32]struct{}
	seenFailed map[int32]struct{}
```

and in `newFileWriter`:

```go
		seenDone:   make(map[int32]struct{}),
		seenFailed: make(map[int32]struct{}),
```

Delete the `resolvedUntracked map[int32]struct{}` field with its doc block, and its `make` line.

- [ ] **Step 4: Re-key the three admit functions**

`admitAccepted` loses its guard — every `int32` is a valid key:

```go
func (w *FileWriter) admitAccepted(artIdx int32) {
	w.seenDone[artIdx] = struct{}{}
	w.partsWritten++
}
```

```go
func (w *FileWriter) admitRetryOfFailed(artIdx int32) {
	w.seenDone[artIdx] = struct{}{}
}
```

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

Delete `admitPermanentFailure`'s "The empty Message-ID is deliberately NOT guarded…" paragraph and `admitRetryOfFailed`'s "The caller owns the precondition that msgID is non-empty…" paragraph. Both document a question this change answers.

- [ ] **Step 5: Re-key `rollbackPart`, `failPermanent`, `failDisplaced`**

In `rollbackPart` replace the three `id.msgID` keyings with `id.artIdx`, and delete its `# Precondition: id.msgID is non-empty` block — the precondition dissolves with the key change.

```go
func (w *FileWriter) failPermanent(id articleID) {
	delete(w.seenDone, id.artIdx)
	w.seenFailed[id.artIdx] = struct{}{}
}
```

In `failDisplaced`, collapse the branch:

```go
	w.admitPermanentFailure(id.artIdx)
```

- [ ] **Step 6: Update the four sites in `assembler.go`**

In `handleSuccessArticle`, delete the `if req.MessageID != "" {` wrapper **and** the `} else if _, resolved := w.resolvedUntracked[req.ArtIdx]; resolved {` arm with its comment, leaving the two dedup arms at the function's top level keyed on `req.ArtIdx`:

```go
	if _, dup := w.seenDone[req.ArtIdx]; dup {
		...
	}
	if _, was := w.seenFailed[req.ArtIdx]; was {
		w.admitRetryOfFailed(req.ArtIdx)
		...
	}
```

`admitAccepted(req.ArtIdx)` at the end. In `handleLateDuplicate`, the two lookups become `f.w.seenDone[req.ArtIdx]` and `f.w.seenFailed[req.ArtIdx]` — **leave its `req.MessageID == ""` guard in place**, Task 3 removes it. In `handleFatalArticle`, `return f.w.admitPermanentFailure(req.ArtIdx)`, and rewrite its doc comment, which narrates the Message-ID dedup.

Leave `fail`'s early return and `giveBackUntrackedPart` alone — Task 2 removes both together.

- [ ] **Step 7: Run the pin and the full suite**

Run the pin: expect PASS. Then `go test -race -count=1 ./...` and `go test -count=1 -tags=integration ./test/integration/...`.

Any failure is either a mistake in the re-key or a test pinning the Message-ID key itself. Triage each; do not adjust production code to keep such a test green.

- [ ] **Step 8: Sweep and commit**

```bash
git grep -n 'resolvedUntracked'   # must be empty
git grep -n 'no map can hold'
git grep -n 'untracked'
```

Message: `refactor(assembler): key duplicate handling on ArtIdx, not Message-ID`

---

### Task 2: Delete `giveBackUntrackedPart` and `fail`'s early return

Both in **one commit**. `routeAcceptFailure` calls `giveBackUntrackedPart` for an empty-ID article and `fail` returns early for the same case precisely so the decrement is not charged twice. Removing either alone mis-counts `partsWritten`.

**Files:**
- Modify: `internal/assembler/filewriter.go` — `fail`, `giveBackUntrackedPart`, and the doc comments listed in Step 5
- Modify: `internal/assembler/assembler.go` — `routeAcceptFailure`
- Test: `internal/assembler/accounting_test.go`

- [ ] **Step 1: Write the test, with a case that discriminates**

The second subtest is the one that matters. `articleID` is package-local, so an empty `msgID` is constructible directly even though no production path produces one — which is what makes the removal observable.

```go
// TestFileWriter_FailRollsBackEveryArticlesPart pins that fail decrements the
// part count for any admitted article, with no identity-dependent early return.
//
// fail used to skip articles with no Message-ID, because rollbackPart could not
// find them in a Message-ID-keyed map; routeAcceptFailure gave their part back
// separately through giveBackUntrackedPart. With the maps keyed on ArtIdx there
// is one path.
func TestFileWriter_FailRollsBackEveryArticlesPart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		msgID string
	}{
		{"with a Message-ID", "a@t"},
		// Constructible only in-package; no production path produces it. It is
		// here because it is the input the deleted guard responded to, and so
		// the only one that can show the guard is gone.
		{"with none", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newHelperFile(t, t.TempDir(), "rollback.dat", 0).w
			id := articleID{msgID: tc.msgID, artIdx: 7}

			w.admitAccepted(id.artIdx)
			if got := w.parts(); got != 1 {
				t.Fatalf("parts() = %d after admit, want 1", got)
			}
			w.fail(id)
			if got := w.parts(); got != 0 {
				t.Errorf("parts() = %d after fail, want 0 — the part was not "+
					"rolled back, so fail still returns early on this identity", got)
			}
			if _, still := w.seenDone[id.artIdx]; still {
				t.Error("the article kept its seenDone entry after fail")
			}
		})
	}
}
```

- [ ] **Step 2: Run it and confirm the second subtest FAILS**

Run: `go test -count=1 ./internal/assembler/ -run TestFileWriter_FailRollsBackEveryArticlesPart -v`
Expected: `with a Message-ID` PASSES, `with none` FAILS with `parts() = 1 after fail, want 0`. Record it. This is the red the earlier draft of this plan wrongly claimed was impossible to obtain.

- [ ] **Step 3: Delete the two obsolete accounting tests**

Delete `TestFileWriter_GiveBackUntrackedPartReturnsTheCount` and `TestFileWriter_GiveBackUntrackedPartIsClampedAtZero` from `accounting_test.go`, plus any `TestFileWriter_AdmitCountsAnArticleWithNoMessageID` and the empty-ID row of the dedup table test.

- [ ] **Step 4: Remove the guard and the function**

```go
func (w *FileWriter) fail(id articleID) {
	w.rollbackPart(id)
	w.faulted = append(w.faulted, faultedArticle{id: id})
}
```

Delete `giveBackUntrackedPart` and its doc. In `routeAcceptFailure`, delete the comment block and the `if req.MessageID == "" { f.w.giveBackUntrackedPart() }` statement, leaving `a.releaseFaulted(...)` intact.

- [ ] **Step 5: Rewrite the comments this falsified**

These are not optional and they are not all in the functions being edited:

- `filewriter.go` `acceptedAt` doc — "fail returns early on an empty Message-ID, so an untracked article's entry would never have been removed".
- `filewriter.go` `partsWritten` doc — "plus the articles with no Message-ID that neither map can hold", which also enumerates `giveBackUntrackedPart` as a mover.
- `filewriter.go` `Accept` — two inline comments justifying the `didFail` bool and the `!taken || owner.id != id` guard by "an untracked article". **Narrow these, do not universalize**: the re-accept path is still unreachable, for a different reason. Say which reason.
- `rollbackPart`'s doc, which names `giveBackUntrackedPart`.

- [ ] **Step 6: Run the test, gates, and commit**

Expected: both subtests PASS. Message: `refactor(assembler): delete giveBackUntrackedPart and fail's empty-ID return`

---

### Task 3: Remove the residual guard and give `articleID` one identity

**Files:**
- Modify: `internal/assembler/assembler.go` — `handleLateDuplicate`
- Modify: `internal/assembler/writecache.go` — `articleID` doc, new `sameArticle` method, the eviction comparison
- Modify: `internal/assembler/filewriter.go` — comparisons in `noteWritten`, `offsetSettledBy`, `Accept`
- Test: `internal/assembler/assembler_test.go`

**Interfaces:**
- Produces: `func (a articleID) sameArticle(b articleID) bool`

- [ ] **Step 1: Write a test that discriminates**

The empty `MessageID` is what the guard keys on, so it is what the test must supply. An earlier draft used a real Message-ID here and would have passed identically before and after.

```go
// TestHandleLateDuplicate_ResolvesAnArticleWhateverItsIdentity pins that a late
// article for a completed file is resolved by index, with no early return that
// depends on it carrying a Message-ID.
//
// An unresolved late article leaves the job waiting on it forever, so the guard
// removed here was the difference between reporting and hanging — for an input
// that, since the parse gate landed, no production path produces.
func TestHandleLateDuplicate_ResolvesAnArticleWhateverItsIdentity(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 1)

	rejected := make(chan int32, 1)
	opts := makeOpts(dir, files)
	opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) { rejected <- artIdx }
	a := startAssembler(t, opts)

	if err := writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, MessageID: "first@t",
		Offset: 0, Data: []byte("AAAA"),
	}); err != nil {
		t.Fatalf("writeArticle: %v", err)
	}
	// A late article for the now-complete file, carrying no Message-ID.
	if err := writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 1, MessageID: "",
		Offset: 0, Data: []byte("BBBB"),
	}); err != nil {
		t.Fatalf("writeArticle late: %v", err)
	}
	select {
	case got := <-rejected:
		if got != 1 {
			t.Errorf("rejected ArtIdx %d, want 1", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the late article was never resolved, so the job waits on it " +
			"forever: handleLateDuplicate still returns early when the article " +
			"carries no Message-ID")
	}
}
```

- [ ] **Step 2: Run it and confirm it FAILS**

Expected: FAIL after ~5s with the "never resolved" message. Record it.

- [ ] **Step 3: Narrow the guard**

```go
	if f == nil {
		return
	}
```

- [ ] **Step 4: Add `sameArticle` and use it everywhere**

Do **not** inline `.artIdx ==` at each site. Before this change `articleID` had one syntactic identity form that could not be applied inconsistently; four hand-written copies would replace it with four things to keep in agreement, which is the owner-model violation this plan is otherwise built on avoiding.

In `writecache.go`, beside `articleID`:

```go
// sameArticle reports whether two identities name the same article.
//
// The index alone decides. msgID travels with the article for logging and
// telemetry and is deliberately NOT compared: identity is the manifest index,
// which is what FileWriter's seen-sets key on. Comparing the pair as well would
// give the assembler a second, finer notion of sameness than the one its
// accounting uses, and the two could disagree.
func (a articleID) sameArticle(b articleID) bool { return a.artIdx == b.artIdx }
```

Replace the whole-struct comparisons in `noteWritten`, `offsetSettledBy`, `Accept` and the eviction comparison in `writecache.go` with calls to it.

- [ ] **Step 5: Write a test for the narrowed identity**

```go
// TestArticleID_IdentityIsTheIndexAlone pins that msgID is not compared.
func TestArticleID_IdentityIsTheIndexAlone(t *testing.T) {
	a := articleID{msgID: "one@t", artIdx: 4}
	b := articleID{msgID: "two@t", artIdx: 4}
	if !a.sameArticle(b) {
		t.Error("two identities with one index compared unequal; msgID is still part of identity")
	}
	if a.sameArticle(articleID{msgID: "one@t", artIdx: 5}) {
		t.Error("two identities with different indices compared equal")
	}
}
```

- [ ] **Step 6: Gates and commit**

Message: `refactor(assembler): compare articles by index, not by Message-ID`

---

### Task 4: Update the contract and sweep the claims

- [ ] **Step 1: Mark F1 landed, and sweep the literal**

`F1` appears in far more places than §5.F's row. Sweep for the literal, not the concept:

```bash
git grep -n 'F1'
```

Expected hits include the document's status line ("F1/F4 remain proposed"), §4, the decidability table, the sequence line (`→ F1a → F1b`), and §5.F. Update every one that states F1 as pending.

- [ ] **Step 2: Correct the A7 argument in the contract**

§5.A argues A7 is load-bearing partly because structures downstream key on the Message-ID. One of them no longer does. Read §5.A **in full** and correct the argument rather than grepping — the claim is stated in prose sharing no token with the code.

- [ ] **Step 3: Narrow `internal/nzb/model.go`'s A7 comment — do not delete it**

That comment gives two reasons for dropping a repeated Message-ID and explicitly says the first "stands on its own — it does not depend on anything downstream keying on the Message-ID". F1 removes only the second.

Delete the second paragraph. **Then re-derive whether its final instruction still holds** — "Do not weaken this to a counted warning, or narrow it to per-file, while that is true" was conditioned on the clause being deleted. Decide on the surviving reason's own terms and say which reason supports what survives. Carrying the instruction forward unchanged, or deleting it with the paragraph, are both wrong without that check.

- [ ] **Step 4: Sweep the remaining literals**

```bash
git grep -n 'seenDone'
git grep -n 'seenFailed'
git grep -n 'untracked'
git grep -n 'no map can hold'
git grep -n 'empty key'
```

`docs/reviews/*.md` are frozen records — leave them.

- [ ] **Step 5: Whole-repo gates and commit**

```bash
go run ./scripts/check_dup_comments && go run ./scripts/check_review_banner
```

Message: `docs: record that FileWriter keys on ArtIdx and no longer needs A7`

---

## Deferred, with reasons

- **Backing the seen-sets with a bitset rather than a map.** `internal/queue/bitset.go` exists and `JobProgress` uses it for exactly this shape, and `ArtIdx` is contiguous per file — so a bitset indexed by `artIdx - baseIdx` would cost ~1 bit per article against a map entry's ~40-50 B. It is deferred because `FileWriter` cannot see the base index: `FileInfo` carries `Path`, `TotalParts` and `ExpectedSize`, and `fileArticleOffsets` is unexported in `internal/queue`. Taking it would mean a new `FileInfo` field, plumbing through `internal/app`'s `resolveFileInfo`, and a new exported accessor on `Manifest` — three packages and a cross-package interface change, which `AGENTS.md` lists as requiring escalation, for work otherwise contained to one. The magnitude is also bounded: these maps exist only for files with an open handle, not per resident job. File as a follow-up.
- **Merging `seenDone` and `seenFailed` into one two-bit state.** They are written at different call sites with different lifetimes — `failPermanent` clears one and sets the other — so this is not a drop-in win. Not pursued.
- **#382's unification of how an article leaves the in-flight set.** Blocked on #379's `Displaced` semantics.
- **Removing `msgID` from `articleID`.** It stays for logging.

## Self-Review

**Spec coverage.** F1's named deletions map to tasks: `resolvedUntracked` and its consult → Task 1; the key change → Task 1; `giveBackUntrackedPart` and `fail`'s early return → Task 2; `failPermanent`'s and `failDisplaced`'s guards → Task 1; `handleLateDuplicate`'s guard → Task 3. The contract's ordering constraint is honoured by Task 2 doing both halves.

**Type consistency.** `admitAccepted`, `admitRetryOfFailed` and `admitPermanentFailure` take `artIdx int32` from Task 1 onward, and every caller is updated in the same task — including `handleFatalArticle`, which an earlier draft of this plan omitted.

**Inertness claim, scoped.** Task 1 is inert for reachable inputs. Tasks 2 and 3 each have an observed red on a **constructed** input — an empty `msgID`, which is package-constructible but not producible by any parse or restore path. That distinction belongs in the commit bodies: the tests discriminate, and what they discriminate is not a live defect.
