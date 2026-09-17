# Commit Cycle — the evidence behind the rules

`AGENTS.md` states the per-change commit cycle as rules, at the shortest form
that can be acted on. This document holds the **evidence**: the kinds of failure
each rule was written after, the measurements that set its shape, and worked
examples of what the failure actually looks like.

Read it when a rule in `AGENTS.md` § "Per-Change Commit Cycle" or § "Quality
Gates" looks arbitrary, when you are about to argue that one does not apply to
your change, or when you are considering changing a rule. If `AGENTS.md` and
this document ever disagree, **`AGENTS.md` is the rule and this document is out
of date.**

Nothing here is a rule. Every rule is in `AGENTS.md`.

## Why the evidence is kept at all

The rules in the commit cycle share a property that makes them easy to talk
yourself out of: each one forbids a shortcut that *looks* equivalent to the
thing it mandates. Mentally reverting a fix looks equivalent to reverting it.
Re-reading the file you edited looks equivalent to grepping the claim. Being
confident that a function is the only writer looks equivalent to running the
grep.

In every case below, the shortcut was taken by someone who knew the rule, and
the difference showed up in shipped code. That is the only argument for the
rules that has ever worked, so it is written down rather than recalled.

## The red check — why it must be observed

`AGENTS.md` mandates `scripts/mutate` and a spec. The reasons are these.

**"Mentally reverting" was already the rule, and pins that passed against
unfixed code shipped anyway.** Two ways they slip through: an assertion that
degenerates to a tautology, and an assertion on a value the code only produces
under conditions the test never creates.

**A rule re-typed from memory per use has a per-use failure rate.** This is
measured, not suspected. Eight hand-rolled revert harnesses, written one at a
time, each needed the same three invariants:

| Invariant | How `AGENTS.md` supplied it then | Held in |
|---|---|---|
| `-count=1` | copy-pasteable text | 8 of 8 |
| restore from your own copy | copy-pasteable text | 8 of 8 |
| the anchor matches exactly once | prose only | **7 of 8** |

The one supplied only as prose is the one that failed. That gap is the whole
argument for a runner: `scripts/mutate` now enforces all three, and its package
doc in `scripts/mutate/main.go` owns the details of how.

**A cached `ok` is not an observation.** Go caches a successful test result
keyed on the test binary and its inputs, and prints `(cached)` where it would
have printed a duration. A mutation run without `-count=1` can replay the
*pre-mutation* pass and report `ok` — which reads as "the test does not
discriminate" and is the exact opposite of the truth. A mutation check that
returned a cached `ok` would have been recorded as evidence that a pin was inert
had the second run not been questioned.

**A compile error is a red result that is not evidence.** Deleting a block often
breaks the build instead of the test, and the compiler noticing is not the test
would-have-caught-it. `scripts/mutate` reports this as its own verdict,
`COMPILE_ERROR`, rather than as `KILLED`, which is the distinction a hand-rolled
script does not draw.

**A stale anchor fails loudly; a stale `run` filter fails silently.** A `run`
line that matches nothing is caught by the baseline. One that matches five tests
and misses the sixth is not: the baseline is green, the mutation reports
`SURVIVED`, and that reads as "the assertion is inert" when the truth is "the
assertion never ran". The usual cause is a `run` alternation that did not grow a
term when a test was added beside it. `scripts/mutate` separates this case out
as `EXCLUDED`.

**The example anchor in `AGENTS.md` is kept a real line on purpose.** An example
anchored on text the tree no longer contains still reads as a working example,
but every reader who runs it gets `ANCHOR — anchor matched no site` and has to
work out whether the tool or the example is broken — and a refactor that moves
the line is enough to cause it. `internal/job/testdata/postproc_stamp.spec` is
the same mutation kept as a committed spec, so a moved anchor fails a run rather
than waiting to mislead a reader.

## The sweep — worked examples

`AGENTS.md` § "Step 4 in practice" gives the rules. Each was written after a
failure of one of these shapes.

### A doc restating a claim shares no token with the code

An architecture doc saying "all message-IDs are validated before use to prevent
NNTP command injection" survives a sweep for the validating function's name,
because it never names the function it is describing. `git grep` is blind to
paraphrase, and `docs/*.md` is where paraphrase lives.

This is the case that grep cannot cover, and it is why `AGENTS.md` says to
**read** `docs/ARCHITECTURE.md` and the relevant contract doc in full at the end
of a change that alters what a doc describes, rather than grepping them.

### A table row is the same claim, compressed

The opposite failure: the row usually *does* carry your search token, and gets
missed anyway because it is nowhere near the prose that explains it. You rewrite
the paragraph you came for, and the row two hundred lines up still states the
old version in four words. Nothing marks it as the same claim.

A typical instance: a contract's prose is rewritten to say a response-identity
check covers four NNTP status codes, while two table rows describing the same
check, and a row in a second table much earlier in the document, still name only
the first. The document now contradicts itself about its own scope, and a
reviewer who spots one row has no reason to look for the others.

The remedy is to sweep for the **literal** — the status code, the duration, the
threshold, the field name — rather than for the concept you were editing. A
grep for the status code finds every row in one command; no search for "the
Message-ID check" finds any of them.

### Narrowing a referent, then broadening a scope

A change deletes one component's lookup by Message-ID, and the rewritten comment
claims that nothing downstream keys on Message-ID — while a second component
still does. If that sentence is the stated reason duplicates may be dropped
document-wide, acting on it lets a second segment be taken for a duplicate of
the first: buffer released, assembled file silently short, no error raised.

"This package no longer keys on X" is not "nothing keys on X". When a change
deletes the thing a justification named, say what *still* holds.

### Removing one term from a stated formula

A doc gives a per-article cost as `~80 B + map` and a per-job total of `~3.3 MB`
for 20,000 articles. Delete the `+ map` term and `80 × 20,000` visibly fails to
equal `3.3 MB`. Re-measuring can then show the total was wrong all along, which
the composite form had been hiding.

Recomputing the stated result from the surviving terms is arithmetic on what is
already written down, and it takes one line. A deliberate decision not to
re-derive a figure is not a licence to leave a visible contradiction behind.

### Sweeping against the wrong diff

The failure is subtler than "comments drift", and it tends to repeat on one
branch — including shipping the drift in the *same* commit as the change that
caused it. The sweep runs against the state that *prompted* the correction: a
reviewer names a stale sentence, the sentence is rewritten to describe the fix,
and a clause the same fix also invalidated is carried forward untouched. The
correction is real and the comment is still wrong.

## Enumerate before asserting — how it fails

`AGENTS.md` § "4. Enumerate before asserting" states the rule. The evidence
behind it:

**A single change shipped eight overclaims** — comments quantifying over a
population of code ("only", "sole", "never", "always", "nothing else", "the one
place") that no enumeration supported. Every one was caught by a reviewer.
**None** was caught by a gate, and none could have been: comments are neither
type-checked nor executed, `go vet` cannot read them, and `check_dup_comments`
finds only copies. One of the eight argued *for* a defect that had been fixed
hours earlier, and would have been read as the reason to undo the fix.

**A behavioural claim fails the same way but is settled differently.** A comment
reading "nothing is lost by declining — the verdict is deferred, not discarded"
can be true only while a persistence store is configured: the restore call that
carries the failure rate across a pause sits behind a nil check on that store,
so a store-less instance restarts its counters at zero and the deferred verdict
never fires. The condition sits one branch away from the sentence and survives
review, because nothing in the claim points at it. Rule 4's trigger words do not
fire either — "nothing is lost" quantifies over outcomes, not over writers or
callers.

**In tests the same failure wears a second costume**: an assertion that passes
through a branch you did not mean to exercise. A "does not restamp" subtest can
pin an early `return` for an unrelated flag rather than the zero-value guard it
was written for, and it is indistinguishable from a working test until a mutation
SURVIVES.

**Where the population is enumerable by a machine, the test is the right form.**
`job.TestOutcomeWrites_MatchTheEnumerationStatedInProse` (in
`internal/job/outcome_writer_enumeration_test.go`) is the worked example: the
same enumeration had gone stale twice in two unlinked files, and a grep from
either one could not reach the other.

## Review-fix loops — why the findings keep coming

A review-fix loop that turns up a new serious finding every round is rarely
finding unrelated defects. Three causes account for most of it, and each has a
rule in `AGENTS.md`. All three are cheap to avoid before the first push and
expensive to discover one round at a time after it.

### Fixing one site of a rule

A fix is often justified by a general rule rather than by the one line it
changes: *cleanup that runs after a job has left the queue must not be abandoned
because the caller's context ended*, or *a step that can fail runs before the
steps that cannot be undone*. The rule names a set of sites. Fixing the one that
prompted the change leaves the rest untouched — and a reviewer who reads the
fix's own justification goes looking for them, finding roughly one per round,
because each round's fix is local again.

The other sites are rarely obscure. A rule about a job leaving the queue applies
to every path by which it leaves — an API removal, the finalizer, startup
reconciliation — and to every step on each path after the one that cannot be
undone. The siblings that surface once the first site is fixed look like this:

- the database delete was detached from the caller's context, but the
  file-handle close a few lines above it, on the same path, was not;
- one path learned to stop destroying state when its dequeue failed, while a
  second path of the same shape kept deleting the manifest of a job still in the
  queue;
- a lookup that failed and a lookup that found nothing were folded into one
  return value, so doubt was reported to the caller as a definite answer.

None needs new insight to find or to fix. Each is one more member of a
population that was never written down, and writing it down is a few minutes
of grep and reading against a round of review per site.

### A review after every push is already the analyzer

Running `pr-review-toolkit:comment-analyzer` only on the last round assumes
review happens once, and that an early sweep would go stale before anyone read
the result. Where a new review arrives after every push, that review reads every
changed comment each round regardless. Skipping the analyzer does not save the
pass; it moves it one round later and turns its findings into threads to answer
instead of local edits.

What it catches on such a branch is mostly prose written on the branch itself,
often by the previous round's fix:

- a timeout said to "match the sibling paths" when the siblings use different
  values;
- two waits said to be "on the same worker" that block on different ones;
- data attributed to the wrong table, because a comment and a nearby variable
  used the same word for two different things.

### Comment volume is claim volume

Every sentence in a comment is something a reviewer can check and the tree must
keep true for as long as the comment lives. Explaining a change in comment prose
— why it was needed, what else was tried, which sibling a value matches —
multiplies those claims, and the wrong ones are disproportionately the asides no
reader needed. The same reasoning in a commit body is read as a record of the
change as of when it was made, and cannot drift out from under the code.

One form is worth singling out: a comment that embeds a search for text it
contains. A comment quoting a SQL statement matches a grep for that statement,
so it inflates its own stated count — and, silently, any other comment
enumerating the same pattern — until someone recounts. State the basis in prose
that does not reproduce the token being searched for.

## Why the gates are shaped the way they are

### The tagged-file blind spot

A tagged integration test sat uncompilable for six weeks because a constructor's
signature changed under it. `go build`, `go vet` and `go test -race` all skipped
it for being tagged, and the mandated integration command was scoped to a
directory that did not contain it. Nothing failed; the test simply stopped
existing.

`go vet -tags=integration,uitest,crash ./...` closes that. It compiles those
files without running any of them and needs none of the external tools the
tagged suites need. It takes seconds with a cold build cache and well under one
warm, which is why `AGENTS.md` lists it unconditionally rather than as a
conditional note.

**What it does not cover.** Files behind an OS constraint stay invisible: the
ones for other platforms are simply skipped, so a Windows-only file can stop
compiling under `GOOS=windows` with no gate in this repository noticing. It also
guarantees only that the suites can be built, not that they still assert
anything.

`e2e` is deliberately absent from the tag list: `test/e2e` carries no build
constraint and is gated at runtime by `E2E_CONFIG`, so `-tags=e2e` has never
done anything. `crash` is Linux-only and its files say so (`//go:build crash &&
linux`), which is what keeps the gate passing on macOS — without that
constraint, a Linux-only syscall makes the tagged build fail there, and takes
`scripts/run_tests.sh` down with it under `set -e`.

### The linter had the same blind spot

A `.golangci.yml` with no `build-tags` key loads packages without tags, so every
tagged file is linted by nothing, silently. The config now lists all three tags,
so the default `golangci-lint run ./...` covers them with no flag at either call
site. The vet gate above does not close this gap — it only proves the files
compile.

### The whole-repository gates each found a real defect on their first run

They exist because build, vet, lint and the test suite are structurally blind to
comments and Markdown. Their first run against this tree turned up each kind of
defect they target:

- a package doc comment duplicated across two files of one package;
- a fixture comment naming a different function from the test it sat above;
- wrong citations — a comment stating a call-site count one lower than the
  truth, commands whose prose said "outside tests" while the command filtered
  nothing, and a `grep` with no path argument that waited on stdin and reported
  zero.

`check_doc_citations` exists because the class is measured: deleting one package
left hundreds of references to it across dozens of files, docs cited migrations
that were never applied, and tests were named as the guard on an invariant while
not existing.

**These whole-repository gates were once absent from `ci.yml` while the
diff-scoped gates were present**, which is how a defect in `check_dup_comments`'
own marker handling survived in the tree: nothing ever ran the tool that would
have caught it.

### Why `check_coverage` attribution can be wrong

`gitscope.Diff()` unions the committed and working-tree diffs, whose hunk headers
are numbered against different files, so committed hunks can land on the wrong
function — in both directions. If a reported function looks untouched by your
change, commit and re-run before writing a test for it.

## Why CI is disabled

`ci.yml` triggers on `workflow_dispatch` only. Every gate it contains runs
locally in a fraction of the runner's wall-clock time, so the server was
removed rather than the gates.

`codeql.yml` replaced GitHub's default code-scanning setup, which could not
analyse Go at all: the extractor builds with `GOTOOLCHAIN=local`, so it used the
runner image's pre-installed Go and failed as soon as `go.mod`'s floor moved past
it. That surfaced as a failing `Analyze (go)` check with no findings — a startup
error, which reads as "nothing to report" and actually meant "did not run".

The general lesson outlives the instance: **raising the floor in `go.mod` can
silently disable any tool that pins its own toolchain**, and such a tool fails
by not starting rather than by reporting. A single floor bump has taken out both
CodeQL and `golangci-lint` this way, and neither announced itself. After a
version bump, check every consumer that builds the module, not only the ones
whose config names a version.

To restore automatic runs, replace the `on:` block in
`.github/workflows/ci.yml` with the `push`/`pull_request` triggers recorded in
that file's git history.

## Commit hygiene — the mistakes these rules came from

**A subject line that misdescribes the diff makes the change invisible.** A
commit subjected `refactor(api): …` that actually changed `internal/assembler/`
is a defect, not a typo — it hides the assembler change from anyone searching
api history.

**Per-path staging only works when the units own disjoint paths.** `git add
<path>` stages the *working-tree* version of that file, not "the part of it
belonging to this unit" — so if two logical units both touched `parser.go` and
all the edits were made before the first commit, commit one silently gets both.
A commit subjected as a small refactor can carry an entire feature this way, and
the rule against `git add -A` reads as satisfied the whole time, because
`git add -A` was avoided. `git status` shows the file as staged either way.

The fix is to reconstruct the intermediate: check the file out at the base
commit, re-apply only the first unit's edits, verify it builds and its tests
pass in isolation, commit, then restore the final version for the second. That
is also the two-commit-split rule's real purpose — not tidy history, but making
the intermediate state exist so a red-green check is possible at all.

**Quantitative claims in commit bodies must be measured.** An extraction reduces
the *parent's* complexity by construction, but the magnitude is not guessable:
a claimed drop to under 5 can measure as 24 to 12.

**Refactors can introduce new lint findings.** Converting control flow (e.g. a
fall-through `return` into boolean returns) can produce `S1008` or `ifElseChain`
findings that did not exist in the original, so the gate must run against the
code you are about to commit rather than a mental model of it.
