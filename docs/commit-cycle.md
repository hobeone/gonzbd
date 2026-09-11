# Commit Cycle — the evidence behind the rules

`AGENTS.md` states the per-change commit cycle as rules, at the shortest form
that can be acted on. This document holds the **evidence**: the incidents each
rule was written after, the measurements that set its shape, and the worked
examples that show what the failure actually looks like.

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
measured, not suspected. One session produced **eight** separate hand-rolled
revert harnesses. Of the three invariants they needed:

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
discriminate" and is the exact opposite of the truth. This happened once here: a
mutation check returned a cached `ok` and would have been recorded as evidence
that a pin was inert, had the second run not been questioned.

**A compile error is a red result that is not evidence.** Deleting a block often
breaks the build instead of the test, and the compiler noticing is not the test
would-have-caught-it. `scripts/mutate` reports this as its own verdict,
`COMPILE_ERROR`, rather than as `KILLED`, which is the distinction a hand-rolled
script does not draw.

**A stale anchor fails loudly; a stale `run` filter fails silently.** A `run`
line that matches nothing is caught by the baseline. One that matches five tests
and misses the sixth is not: the baseline is green, the mutation reports
`SURVIVED`, and that reads as "the assertion is inert" when the truth is "the
assertion never ran". Both times this happened here, the spec was an alternation
that had not grown a term when a test was added beside it. `scripts/mutate`
separates this case out as `EXCLUDED`.

**The example anchor in `AGENTS.md` is kept a real line on purpose.** An example
anchored on text the tree no longer contains still reads as a working example,
but every reader who runs it gets `ANCHOR — anchor matched no site` and has to
work out whether the tool or the example is broken. That example used to name
`internal/queue/queue.go`'s `if job.progress.downloadFinished.IsZero() {`, and
#464 deleted that line when it gave the download timestamps a single owner.
`internal/job/testdata/postproc_stamp.spec` is the same mutation against the
line that replaced it, kept as a committed spec.

## The sweep — four worked examples

`AGENTS.md` § "Step 4 in practice" gives the rules. Each was written after one
of these.

### A doc restating a claim shares no token with the code

`docs/ARCHITECTURE.md` said "All message-IDs are validated before use to prevent
NNTP command injection" and survived a sweep for `validateMessageID`, because
it never names the function it is describing. `git grep` is blind to paraphrase,
and `docs/*.md` is where paraphrase lives.

This is the case that grep cannot cover, and it is why `AGENTS.md` says to
**read** `docs/ARCHITECTURE.md` and the relevant contract doc in full at the end
of a change that alters what a doc describes, rather than grepping them.

### A table row is the same claim, compressed

The opposite failure: the row usually *does* carry your search token, and gets
missed anyway because it is nowhere near the prose that explains it. You rewrite
the paragraph you came for, and the row two hundred lines up still states the
old version in four words. Nothing marks it as the same claim.

This happened three times in one document on #401.
`docs/article-validation-contract.md`'s §5.B prose was rewritten to say the
response-identity check covers `BODY` 222, `ARTICLE` 220, `HEAD` 221 and `STAT`
223 — while both B1 table rows, and a row in the decidability table 150 lines
earlier, still said `222` alone. The document contradicted itself about its own
scope. A reviewer found two of the three; the third only turned up because that
finding prompted a grep for the rest of the class.

The remedy is to sweep for the **literal** — the status code, the duration, the
threshold, the field name — rather than for the concept you were editing. `git
grep -n '222'` finds all three rows in one command. No search for "the
Message-ID check" would have found any of them.

### Narrowing a referent, then broadening a scope

F2 deleted the queue's Message-ID lookup, and the rewritten comment claimed
nothing downstream keyed on Message-ID — while `internal/assembler`'s
`seenDone`/`seenFailed` still did until F1 landed. That sentence is the stated
reason A7 drops duplicates document-wide, so acting on it would have let a
second segment be taken for a duplicate of the first: buffer released, assembled
file silently short, no error raised.

"`internal/queue` no longer keys on X" is not "nothing keys on X". When a change
deletes the thing a justification named, say what *still* holds.

### Removing one term from a stated formula

`docs/job-lifecycle.md` read `~80 B + map` per article and `~3.3 MB` per
20k-article job. Deleting the `+ map` term left `80 × 20,000` visibly failing to
equal `3.3 MB`. Re-measuring showed the total had been wrong all along
(1.64 MB), which the composite form had been hiding.

Recomputing the stated result from the surviving terms is arithmetic on what is
already written down, and it takes one line. A deliberate decision not to
re-derive a figure is not a licence to leave a visible contradiction behind.

### Sweeping against the wrong diff

The failure is subtler than "comments drift", and it happened three times on one
branch — twice shipping the drift in the *same* commit as the change that caused
it. Each time the sweep ran against the state that *prompted* the correction: a
reviewer names a stale sentence, the sentence is rewritten to describe the fix,
and a clause the same fix also invalidated is carried forward untouched. The
correction is real and the comment is still wrong.

## Enumerate before asserting — the measured failure

`AGENTS.md` § "4. Enumerate before asserting" states the rule. The numbers
behind it:

**The durable-runs change shipped eight overclaims** — comments quantifying over
a population of code ("only", "sole", "never", "always", "nothing else", "the
one place") that no enumeration supported. Every one was caught by a reviewer.
**None** was caught by a gate, and none could have been: comments are neither
type-checked nor executed, `go vet` cannot read them, and `check_dup_comments`
finds only copies. One of the eight argued *for* a defect that had been fixed
hours earlier, and would have been read as the reason to undo the fix.

**A behavioural claim fails the same way but is settled differently.**
`CheckEarlyAbort`'s comment read "Nothing is lost by declining. The verdict is
deferred, not discarded." True, and only while `q.store != nil`: `PromoteNext`
guarded its `RestoreJobProgress` call on exactly that, and `newJobProgress`
alone starts the counters at zero, so a store-less queue loses the failure rate
across pause/resume and the abort never re-fires. The condition sat one branch
away from the sentence and survived a full review round, because nothing in the
claim pointed at it. Rule 4's trigger words did not fire either — "nothing is
lost" quantifies over outcomes, not over writers or callers.

**In tests the same failure wears a second costume**: an assertion that passes
through a branch you did not mean to exercise. #465's "does not restamp" subtest
pinned `if job.PostProc { return false, nil }` rather than the `IsZero` guard it
was written for, and was indistinguishable from a working test until a mutation
SURVIVED.

**Where the population is enumerable by a machine, the test is the right form.**
`job.TestOutcomeWrites_MatchTheEnumerationStatedInProse` (in
`internal/job/outcome_writer_enumeration_test.go`) is the worked example: the
same enumeration had gone stale twice in two unlinked files, and a grep from
either one could not reach the other.

## Why the gates are shaped the way they are

### The tagged-file blind spot

A tagged `internal/app` integration test was uncompilable for six weeks (#475)
because `app.New`'s signature changed under it. `go build`, `go vet` and `go
test -race` all skipped it for being tagged, and the mandated integration
command is scoped to `./test/integration/...`, which does not reach
`internal/app`. Nothing failed; the test simply stopped existing.

`go vet -tags=integration,uitest,crash ./...` closes that. It compiles those
files without running any of them, needs none of the external tools the tagged
suites need, and was measured at 6.9 s with a cold build cache and 0.24 s warm —
which is why `AGENTS.md` lists it unconditionally rather than as a conditional
note.

**What it does not cover.** Files behind an OS constraint stay invisible:
thirteen files carry one, and the ones for other platforms are simply skipped —
`internal/fsutil/crossdevice_windows.go` does not compile under `GOOS=windows`
at all (#480), and no gate in this repository notices. It also guarantees only
that the suites can be built, not that they still assert anything.

`e2e` is deliberately absent from the tag list: `test/e2e` carries no build
constraint and is gated at runtime by `E2E_CONFIG`, so `-tags=e2e` has never
done anything. `crash` is Linux-only and its files say so (`//go:build crash &&
linux`), which is what keeps the gate passing on macOS — before that constraint
existed, `unix.Fadvise` made it fail there and took `scripts/run_tests.sh` down
with it under `set -e`.

### The linter had the same blind spot

Before #481, `.golangci.yml` carried no `build-tags` key, so `golangci-lint`
loaded packages without tags and the 27 tagged files were linted by nothing,
silently. The config now lists all three tags, so the default `golangci-lint run
./...` covers them with no flag at either call site. The vet gate above does not
close this gap — it only proves the files compile.

### The whole-repository gates each found a real defect on their first run

They exist because build, vet, lint and the test suite are structurally blind to
comments and Markdown. Their first run against this tree turned up:

- a package doc comment duplicated across two files of `scripts/nntpfaultproxy`;
- a fixture comment in the since-deleted `internal/queue`'s progress helpers
  that named `resetForReload` above a test of `clone`;
- four wrong citations — `internal/sched/advance.go` claiming `parkLocked` had
  two call sites when it had three, two commands whose prose said "outside
  tests" while the command filtered nothing, and one in `internal/job/job.go`
  that could not run at all because it carried no path argument, so `grep`
  waited on stdin and reported zero.

`check_doc_citations` exists because the class is measured: `internal/queue` was
deleted in `b6651d43` and ~400 references survived across 31 files, docs cited
five migrations that were never applied, and five tests were named as the guard
on an invariant while not existing.

**These whole-repository gates (originally four, now five with
`check_test_doubles`) were once absent from `ci.yml` while the three diff-scoped
gates were present**, which is how a defect in `check_dup_comments`' own marker
handling survived in the tree: nothing ever ran the tool that would have caught
it.

### Why `check_coverage` attribution can be wrong

Issue #280: `gitscope.Diff()` unions the committed and working-tree diffs, whose
hunk headers are numbered against different files, so committed hunks can land
on the wrong function — in both directions. If a reported function looks
untouched by your change, commit and re-run before writing a test for it.

## Why CI is disabled

`ci.yml` triggers on `workflow_dispatch` only. Every gate it contains runs
locally in a fraction of the runner's wall-clock time, so the server was
removed rather than the gates.

`codeql.yml` replaced GitHub's default code-scanning setup, which could not
analyse Go at all: the extractor builds with `GOTOOLCHAIN=local`, so it used the
runner image's pre-installed Go and failed against `go.mod`'s floor from the
1.27 bump onwards. That surfaced as a failing `Analyze (go)` check with no
findings — a startup error, which reads as "nothing to report" and actually
meant "did not run".

The general lesson outlives the instance: **raising the floor in `go.mod` can
silently disable any tool that pins its own toolchain**, and such a tool fails
by not starting rather than by reporting. The 1.27 bump took out both CodeQL and
`golangci-lint` this way, neither of which announced itself. After a version
bump, check every consumer that builds the module, not only the ones whose
config names a version.

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
This has happened: a commit subjected `refactor(nzb): fold the digest` carried
an entire feature as well, and the rule against `git add -A` read as satisfied
the whole time, because `git add -A` had been avoided. `git status` shows the
file as staged either way.

The fix is to reconstruct the intermediate: check the file out at the base
commit, re-apply only the first unit's edits, verify it builds and its tests
pass in isolation, commit, then restore the final version for the second. That
is also the two-commit-split rule's real purpose — not tidy history, but making
the intermediate state exist so a red-green check is possible at all.

**Quantitative claims in commit bodies must be measured.** An extraction reduces
the *parent's* complexity by construction, but the magnitude is not guessable: a
real case here dropped 24→12, not the claimed <5.

**Refactors can introduce new lint findings.** Converting control flow (e.g. a
fall-through `return` into boolean returns) can produce `S1008` or `ifElseChain`
findings that did not exist in the original, so the gate must run against the
code you are about to commit rather than a mental model of it.
