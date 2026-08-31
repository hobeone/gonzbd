# AGENTS.md — Project Context & Instructions

This is the canonical guidance file for any AI agent (Claude Code, Gemini, etc.)
working in this repository. `CLAUDE.md` and `GEMINI.md` are symlinks to this
file. It must be read and followed for every session.

## Project Context

GoNZBD is a high-performance Go reimplementation of [SABnzbd](https://sabnzbd.org),
the automated Usenet binary newsreader. It targets fresh installations and is
**not** a drop-in replacement for the Python version. The reference Python
implementation lives at `../sabnzbd/`.

- **Module path:** `github.com/hobeone/gonzbd`
- **Go version:** 1.27.0 (toolchain 1.27.0)
- **Status:** Core backend download pipeline and legacy mode-dispatch API
  (`/api?mode=...`) are functional. The Glitter web UI port (Phase 12) is the
  current active focus.
- **Main technologies:**
    - **Language:** Go 1.27.0+
    - **Configuration:** YAML (`gopkg.in/yaml.v3`)
    - **Persistence:** SQLite (`modernc.org/sqlite`, pure Go) for both history
      and queue state; gzip-JSON only for per-job manifests
      (`manifests/<id>.json.gz`) and the NZB backups a retry re-parses
      (`nzb/<filename>.gz`).
    - **Logging:** Structured logging via `log/slog`.
    - **Concurrency:** Idiomatic goroutines + channels; `sync.RWMutex` for
      shared state.

## Standing Design Rules

Four constraints that precede any specific design decision. They are stated
here rather than in a topic doc because each has already been missed by work
that never had cause to open the doc arguing for it, and each changes what the
right answer is rather than merely how to write it down.

`docs/article-validation-contract.md` carries the full argument for rules 1-3,
the worked examples, and the article-validation consequences. Rule 4 is not
from that document and has no topic doc: it is about what a comment may claim,
so it applies to every file in the repository and its evidence is in this
file's own commit cycle. This section is the rule.

### 1. No backwards compatibility

GoNZBD targets fresh installations, is not a drop-in replacement, and runs as a
single self-administered instance.

> **No change owes anything to state an earlier build wrote, or to parity with
> any other implementation.**

Persisted manifests, queue rows, history entries and NZB backups written before
a change may be assumed to satisfy the invariants that change introduces. There
is no drain period, no dual-read path, no migration, and no "old jobs behave
differently" caveat. Where an invariant is newly enforced at ingestion, it is
simply true everywhere.

The rule's force is in what it forbids:

> **Before writing a guard, name the state that makes it necessary — then check
> whether that state is in scope at all.** If the only answer is "data an
> earlier build wrote", the guard is not needed. Delete the class rather than
> defend against it.

This does **not** weaken validation of what the *world* produces. An NZB and an
NNTP response stay untrusted regardless; on-disk corruption is a separate
failure class that `docs/durability-contract.md` owns.

**One carve-out, and it is narrow: the rule waives persistence FORMAT, not a
security invariant.** Where trusting older persisted state could hand an
attacker something — rather than produce a stale figure or a missing field —
the guard stays. The test is what the value could *do*:

- A stale total, a missing counter, a figure from a superseded rule → the rule
  applies; delete the guard.
- A value interpolated into a protocol, a path, a query, or a command → the
  rule does not apply; keep the guard and say why at the check.

This was got wrong twice on one PR before it was written down: the rule was
stretched to cover a Message-ID reaching an NNTP command line, which is a
command injection rather than a formatting difference.

### 2. State has one owner

The rule that has retired the most defects here — #372 gave every `FileWriter`
product a consumer, #378 gave the article accounting an owner, #385 made offset
collisions exact.

> **Every piece of derived state has exactly one function that computes it and
> exactly one path that mutates it. Everything else reads.**

A field whose value is *documented* as a function of other fields, but which
any caller may assign, is not an invariant — it is a comment. `File.Bytes ==
Σ Articles[].Bytes` holds because `normalizeFileStruct` is its sole writer, not
because a comment says so; the one change that wrote it from elsewhere broke it
and stranded bytes in `JobProgress`'s remaining count.

> **When a check and an owner would both work, take the owner.** A check must be
> called at every site that could violate the invariant, and the failure mode of
> forgetting one is silence. An owner cannot be forgotten, because there is
> nowhere else to write.

Three smells this names, all of which have been real here:

- **Two constructors for one type.** `newManifest` and `Manifest.UnmarshalJSON`
  populated the same eight fields by two independently-maintained paths and had
  already diverged over `totalBytes`. A doc comment saying "both paths call
  this, so they cannot disagree" is a comment doing an owner's job, and it
  covers only the fields someone remembered.
- **A derived value that is also persisted.** Anything recomputable from its
  parts should be derived on load, not stored and trusted. Storing it creates a
  second source of truth, and the stored copy is the one that drifts.
- **A type with a valid zero used as a key.** Re-keying a map from a string to
  an index trades a loud empty-key error for a silent alias to element 0. Where
  the substitute type has no invalid value, pair the change with an owner that
  makes the zero unreachable, rather than abandoning it.

When a type cannot be made incapable of the bad value, make it unreachable
except through a gatekeeper. Prefer that to adding a check at each call site.

**Escalate before adding a second constructor, a second writer of a derived
field, or a second enforcement point for one invariant** — see the Decision
Protocol below.

### 3. A bad article costs only its own bytes

> **No single bad article may degrade the handling of any other byte in the
> file.** Reject it, charge its bytes to par2, and carry on.

This is a bound on blast radius, not a licence to validate less. Reject fast and
cheaply — the point is that being *wrong about which kind of bad an article is*
must cost one article, so that precision stops being load-bearing.

**It takes an injection carve-out, like rule 1 does.** The bound never justifies
weakening a check whose absence would let a value reach a protocol, a path, a
query or a command. Rule 1's carve-out and this one arrive there by different
routes — that one asks whether trusting old persisted state hands an attacker
something, this one asks whether the blast radius is really one article — but a
Message-ID carrying CRLF fails both tests, and neither rule excuses it.

It exists as a counterweight to §2 of the article-validation contract, which
classifies every claim an NZB or article makes. That taxonomy is right and earns
its place, but a table of claim classes makes every unfilled row look like work,
and the pull toward completeness is invisible from inside it:

> **Classification decides WHERE a check belongs, not WHETHER one is owed.**

So before taking on article-validation work, ask what one instance costs when we
get it wrong. If the answer is "that article's bytes", it is par2's job and the
correct action is usually none. If the answer names *other* bytes, other
articles, or the whole file or job, it is a violation of this rule and it is
real work.

**A post with no par2 does not weaken this — it is the case that needs it
most.** Those bytes are then unrecoverable, which is a risk the poster took, and
the rule holds unchanged: one bad article still costs only its own bytes. What
changes is the consequence to the user, and that is exactly why the blast radius
must not be the whole file. Without this bound, a no-recovery post loses a
download; with it, it loses a hole.

**This rule removes work.** Applied to the 19 open issues in this cluster, three
survived as violations and ten were not article-validation work at all — see the
contract's Ground rules for the triage.

### 4. Enumerate before asserting

> **A comment that quantifies over a population of code — every writer, every
> caller, every deleter, every enforcement point — is only allowed to say what
> an enumeration you actually performed found. Perform it from source, at the
> moment you write the sentence.**

The words that trigger this are *only*, *sole*, *solely*, *never*, *always*,
*nothing else*, *the one place*, and every paraphrase of them. They are not
stylistic. Each is a claim about a set the reader cannot see, offered so that
they do not have to go and look — which is exactly why a wrong one is worse
than no comment at all: it does not merely fail to help, it actively stops the
check it replaced.

This rule exists because the failure is measured, not suspected. The
durable-runs change shipped **eight** such overclaims. Every one was caught by
a reviewer. **None** was caught by a gate, and none could have been: comments
are neither type-checked nor executed, `go vet` cannot read them, and
`check_dup_comments` finds only copies. One of the eight argued *for* a defect
that had been fixed hours earlier, and would have been read as the reason to
undo the fix.

Three things make this specific rather than an exhortation to be careful:

- **The enumeration is a command, not a recollection.** "I believe X is the
  only writer" and "`git grep -n 'X ='` returns three hits, two of which are
  tests" are different epistemic acts, and only the second is evidence. Run the
  grep even when — especially when — you are confident, and prefer
  grep-**then-read** over grep alone: the population you care about is usually
  the set of *arguments*, and a paraphrase carries none of your tokens. This is
  the same blindness `docs/*.md` has to a code identifier, in the other
  direction.
- **State the basis in the comment.** "Barrier is the only writer" becomes
  "Barrier is the only writer — `INSERT` appears once, at
  `runstore_sqlite.go:233`". The citation is what lets the next reader
  re-run your check in one command instead of re-deriving your confidence.
- **Where the population is enumerable by a machine, write the test instead.**
  A count of call sites, a set of writers of one field, the members of a
  package-private door — these fail loudly when they move, where a comment
  fails silently. `queue.TestDoneBitWriters_MatchTheEnumerationStatedInProse`
  is the worked example: the same enumeration had gone stale twice in two
  unlinked files, and a grep from either one could not reach the other.

**A claim about BEHAVIOUR is scoped by the branch that makes it true.** The
rule above governs a population of code and is settled by a command. A claim
about what *happens* has neither: you cannot grep "is anything lost?", and what
would settle it is a path through the code rather than a set of lines. It fails
the same way — a sentence asserting more than was checked — so it belongs here,
with a different check.

> **Before writing that something holds, find the branch it depends on and name
> it.** If the sentence would be false under some reachable configuration, say
> which one it assumes.

`CheckEarlyAbort`'s comment read "Nothing is lost by declining. The verdict is
deferred, not discarded." True, and only while `q.store != nil`: `PromoteNext`
guards its `RestoreJobProgress` call on exactly that, and `newJobProgress`
alone starts the counters at zero, so a store-less queue loses the failure rate
across pause/resume and the abort never re-fires. The condition sat one branch
away from the sentence and survived a full review round, because nothing in the
claim pointed at it. Rule 4's trigger words did not fire either — "nothing is
lost" quantifies over outcomes, not over writers or callers.

**The same failure wears a second costume, in tests: an assertion that passes
through a branch you did not mean to exercise.** #465's "does not restamp"
subtest pinned `if job.PostProc { return false, nil }` rather than the `IsZero`
guard it was written for, and was indistinguishable from a working test until a
mutation SURVIVED. That half *is* machine-checkable, and `scripts/mutate` is
the check — see "the red check is mechanical, not mental" below. Where a
behavioural claim is pinned by a test, mutate the branch and require the test to
die; where it lives only in a comment, name the branch in the sentence.

**The narrowing half of this is already stated** under "Two checks on what you
WROTE" in the commit cycle below — *narrowing a referent must not broaden a
scope*. That clause governs a sentence a change **falsified**; this rule governs
a sentence you are **writing for the first time**, which is where the other
seven came from. When a claim you were about to write turns out to be false,
the fix is to say what still holds and name what you checked, never to reach
for a weaker universal.

## Repository Layout

- `cmd/gonzbd/`: Entry point, flag parsing, and application orchestration.
- `internal/`: Core packages (API, app, downloader, queue, nzb, assembler, decoder, etc.).
- `docs/`: Critical design documents (`ARCHITECTURE.md`, `sabnzbd_spec.md`, `TESTING.md`).
- `test/`: Integration tests, fixtures, and a mock NNTP server.
- `ui/`: Svelte 5 + TypeScript + Vite SPA, embedded via `//go:embed`.

## Reference Materials — Authoritative Documentation (Order of Precedence)

Before writing any code, read these in order:

1. **`AGENTS.md`** (this file) — Strict development protocols, quality gates,
   and the mandatory "Decision Needed" escalation format. Read this first for
   every session; it is always loaded.
2. **`docs/ARCHITECTURE.md`** — Technical overview, architecture patterns, and
   subsystem deep dives. **Read this for architectural context.**
3. **`docs/TESTING.md`** — Comprehensive testing guide. Covers all test suites
   (unit, integration, E2E, contract), build tags, required tools, and when to
   run each. **Read this before running or modifying tests.**
4. **`docs/sabnzbd_spec.md`** — The functional specification and source of truth
   for behavior: protocols (NNTP), data formats (NZB, persistence), API endpoint
   schemas, constants.
5. **`../sabnzbd/sabnzbd/`** — The original Python source, external to this repo.
   Consult for clarification of intent when the spec is ambiguous, **but do not
   transliterate**. Translate intent into idiomatic Go. The spec has been wrong
   before — when in doubt, ask.

### Topic docs — read only when the trigger applies

These are not loaded by default; read the relevant one before touching the
area it covers, the same way you'd read `docs/ARCHITECTURE.md` before a
design-level change.

| Doc | Read before | Covers |
|-----|-------------|--------|
| [`docs/go-standards.md`](docs/go-standards.md) | Creating, editing, or refactoring any `.go` file | Idioms, anti-patterns, concurrency/persistence architecture, library selection, testing standards, the Go backend lessons-learned catalog |
| [`docs/svelte-gotchas.md`](docs/svelte-gotchas.md) | Creating, editing, or refactoring any `.svelte`/`.svelte.ts` file | Svelte 5 reactivity gotchas (module-level `$state`, native `<dialog>`/`Modal.svelte` patterns, child component update patterns) |
| [`docs/config-contract.md`](docs/config-contract.md) | Adding/renaming/removing a config field or a Svelte config `keyword=` prop | Keeping `gonzbd.yaml` comments, `docs/sabnzbd_spec.md` §9.x, and the config↔UI contract test in sync |
| [`docs/article-validation-contract.md`](docs/article-validation-contract.md) | Touching `internal/nzb`, `internal/nntp`, `internal/decoder`, the decode/reconcile path in `internal/downloader`, or the accept path in `internal/assembler` | What GoNZBD asserts about a Usenet article and where each assertion belongs; the claim-class taxonomy and the layer ladder; the full argument behind Standing Design Rules 1-3 above (rule 4 is not from this document) |
| [`docs/queue-lifecycle.md`](docs/queue-lifecycle.md) | Touching job residency, the `ActiveSet`, the promotion loop, or `Manifest`/`JobProgress` access | Which state a job always has, the header/progress/manifest tiers, which operations may fail and which must not, the memory budget, and why the invariant is compiler-enforced rather than tested |
| [`docs/nntp-downloader-contract.md`](docs/nntp-downloader-contract.md) | Touching `internal/downloader` or `internal/nntp` | Connection pool lifecycle, dispatcher/worker/tracker tiers, sequential article try-lists, failure classification matrix, and disconnect-on-idle invariants |
| [`docs/durability-contract.md`](docs/durability-contract.md) | Touching `internal/durability`, `internal/storagefault`, `internal/assembler`, or `internal/directunpack` | The `durable_runs` record and what may put content into it, the barrier and its proof, the checkpoint cadence, the startup resume sweep, storage-fault stall/fail, disk write caching, OS pre-allocation, sparse file writing, DirectUnpack streaming handoff, and NFS/SMB timeout bounds |
| [`docs/post-processing-contract.md`](docs/post-processing-contract.md) | Touching `internal/postproc`, `internal/par2`, or `internal/unpack` | Stage execution loop, self-gating matrix, Fast/Slow queue priority, QuickCheck bypass guarantees, NeedRequeue rules, and script isolation |
| [`docs/mutation-testing-playbook.md`](docs/mutation-testing-playbook.md) | Running `gremlins` | The `run_gremlins.sh` wrapper, tuning, triaging `LIVED`/`NOT COVERED` mutants, the `--diff` known-bug workaround |

## Building and Running

```bash
go build ./cmd/gonzbd                                  # Build the binary
./gonzbd --config ~/.config/gonzbd/gonzbd.yaml --serve # Run as daemon
./gonzbd --config <path> --nzb <path>                  # One-shot download
```

## Build and Test Commands

No Makefile. Standard Go tooling only:

```bash
go build ./cmd/gonzbd                                       # Build the binary
go test ./...                                               # Unit tests
go test -race ./...                                         # With race detector (required for CI/commits)
go test -run TestFoo ./internal/nzb/                        # Run a single test
go test -bench=. ./internal/decoder/                        # Run benchmarks
go test -v -tags=integration ./test/integration/...         # Integration (requires par2, rar, unrar, 7z)
go test -v -tags=uitest ./test/uitest/...                   # UI/Playwright (requires pre-built UI + Playwright Chromium)
go test -tags=e2e -timeout=10m ./test/e2e/                  # E2E (requires live Usenet server)
go test -tags=crash -timeout=20m ./test/crash/              # Crash consistency (Linux; kills a real child process)
go test ./internal/config/ -run 'TestUI|TestAllFlat'        # Config ↔ UI contract
go vet ./...                                                # Static analysis
golangci-lint run ./...                                     # Linting
./scripts/run_gremlins.sh ./internal/queue                  # Mutation testing on a package (periodic, see below — not a per-commit gate)
```

> **WARNING:** Never run `gremlins` on the entire repository or call
> `gremlins unleash` directly — always `./scripts/run_gremlins.sh <pkg>`
> scoped to one package. It has caused 168–394GB of disk usage and kernel OOM
> kills when misused. **See `docs/mutation-testing-playbook.md`** for the
> wrapper script's safety mechanisms, tuning, the `--diff` known-bug
> workaround, and the full mutant-triage process.

> **See `docs/TESTING.md` for the full testing guide** — build tags, required
> tools, per-file descriptions, and a decision guide for which suites to run
> based on the area of code being changed.

## Implementation Workflow

### Interactive Plan & Design Artifacts (Override Skill Defaults)

- All such artifacts MUST include `ArtifactMetadata` with `RequestFeedback: true` and `UserFacing: true`. This guarantees they are rendered in the interactive review modal, enabling the execution checkpoint/Proceed button.
- Filename conventions:
  - Design/Specs: `YYYY-MM-DD-<feature-name>-design.md`
  - Implementation Plans: `YYYY-MM-DD-<feature-name>-plan.md`

### Per-Change Commit Cycle

Each logical change is a self-contained unit of work. The workflow is:

1. **Read** the relevant spec/architecture sections.
2. **Implement** the change. For a bug fix, write the failing test *first* and
   confirm it fails on the unpatched code before applying the fix (see
   `docs/go-standards.md` § Red-Green Discipline, and the mechanical procedure
   below — "confirm" means *observed*, not reasoned).
3. **Verify** all quality gates pass (see below).
4. **Sweep** the comments and docs the change falsified (see below).
5. **Commit** with a Conventional Commits message. Mention the plan step in the
   body if useful context.

Each commit must leave the repository in a working state
(`go build ./... && go test ./...` passes).

#### Step 2 in practice: the red check is mechanical, not mental

A test written to pin a fix is not a pin until that fix has been reverted and
the test *observed* to fail. "Mentally reverting" is not sufficient — it is
already what this file said, and pins that passed against unfixed code have
shipped anyway. Two ways they slip through: an assertion that degenerates to a
tautology, and an assertion on a value the code only produces under conditions
the test never creates.

**Use `scripts/mutate` rather than hand-rolling the revert.** Write a spec
naming the package, the test, and each mutation; the tool applies them one at a
time, requires each to produce a red result, and restores the file on every
exit path including SIGINT.

```bash
go run ./scripts/mutate path/to/the.spec     # exits non-zero unless every mutation is KILLED
```

```
pkg ./internal/queue/
run TestTheNewPin

[the guard neutered]
file internal/queue/queue.go
--- anchor
	if job.progress.downloadFinished.IsZero() {
--- replace
	if true {
--- end
```

`scripts/mutate/testdata/self.spec` is the worked example — it is the tool's own
red check, and running it is how you verify a change to the tool.

The rest of this section is what the tool enforces, and is stated here because
the reasons outlive the implementation. The tool exists because stating them was
measurably not enough: one session produced **eight** hand-rolled harnesses of
this shape, and of the three invariants below, the two that this file supplies
as copy-pasteable text held in 8 of 8, while the one it supplies only as prose —
anchor uniqueness — held in 7 of 8. A rule re-typed from memory per use has a
per-use failure rate.

**`-count=1` is not optional.** Go caches a successful test result keyed on the
test binary and its inputs, and prints `(cached)` where it would have printed a
duration. A mutation run without it can replay the *pre-mutation* pass and
report `ok` — which reads as "the test does not discriminate" and is the exact
opposite of the truth. This has already happened once in practice: a mutation
check returned a cached `ok` and would have been recorded as evidence that a
pin was inert, had the second run not been questioned. A cached `ok` is not an
observation.

**Never `git stash`** — the stash stack is shared with any other session in this
repo and a pop can take their work. Restore from your own copy rather than
`git checkout -- <path>`, which also discards unrelated uncommitted edits in
that file.

- **Revert each half separately.** A fix with two call sites needs two reverts;
  one half being pinned says nothing about the other.
- **Prefer neutering a condition to deleting a block.** Deleting often breaks
  the build instead of the test, and a compile error does not demonstrate the
  test would have caught the behaviour.
- **Confirm the mutation landed where you meant.** A scripted string-replace
  can match an identical branch elsewhere in the file and produce a red result
  that proves nothing. Anchor on text unique to the target.

Record the observed failure message in the commit body or PR. A red-green claim
without the message it produced is an assertion, not evidence.

#### Step 4 in practice: sweep the claim, not the file

A behaviour change falsifies the same sentence in several places at once, and
they are usually **not** in the diff — an interface doc, a sibling field's
comment, a `docs/*.md` section, a migration's comment block. Fixing the copy
you happened to be editing leaves the rest reading as authoritative.

Take each claim the change invalidated and grep for its distinctive phrasing
**from the repository root**, rather than re-reading the files you touched:

```bash
git grep -n 'bytes that reached disk'   # tracked files only, so no ui/dist or node_modules
```

`git grep` rather than a path list, because the copies are not where you expect
— they turn up in `cmd/`, `test/`, `ui/`, the root `AGENTS.md`, and this file.
Restricting the search to `internal/` and `docs/` is how the first draft of this
section missed that `docs/go-standards.md` still said to `git stash`, in the
same change that added the rule against it.

**`git grep` is blind to paraphrase, and the docs are where paraphrase
lives.** A code comment usually repeats the symbol, so grepping the symbol
finds it. A `docs/*.md` file restates the claim in prose and shares no token
with the code: `docs/ARCHITECTURE.md` said "All message-IDs are validated
before use to prevent NNTP command injection" and survived a sweep for
`validateMessageID`, because it never names the function it is describing.

**A table row is the same claim, compressed — and it is missed for the
opposite reason.** The paragraph above is about a claim carrying none of your
search tokens. A Markdown table row usually carries the token and gets missed
anyway, because it is nowhere near the prose that explains it: you rewrite the
paragraph you came for, and the row two hundred lines up still states the old
version in four words. Nothing marks it as the same claim.

This happened three times in one document on #401.
`docs/article-validation-contract.md`'s §5.B prose was rewritten to say the
response-identity check covers `BODY` 222, `ARTICLE` 220, `HEAD` 221 and
`STAT` 223 — while both B1 table rows, and a row in the decidability table
150 lines earlier, still said `222` alone. The document contradicted itself
about its own scope. A reviewer found two of the three; the third only turned
up because that finding prompted a grep for the rest of the class.

So when a change alters a claim that is stated anywhere as a **literal** — a
status code, a duration, a threshold, a limit, a field name — sweep for that
literal from the repository root, not for the concept you were editing:

```bash
git grep -n '222'          # finds all three rows in one command
git grep -n 'PenaltyUnknown'
```

No search for "the Message-ID check" would have found any of them. The concept
is what you are thinking about; the literal is what is written down.

**Two checks on what you WROTE, not on what you removed.** Finding the stale
sentence is only half the sweep; both of these are about the replacement, and
neither is caught by any gate — comments are neither type-checked nor executed,
and `check_dup_comments` only finds copies.

- **Narrowing a referent must not broaden a scope.** When a change deletes the
  thing a justification named, the fix is to say what *still* holds, not to
  assert a universal. "`internal/queue` no longer keys on X" is not "nothing
  keys on X". This shipped: F2 deleted the queue's Message-ID lookup and the
  rewritten comment claimed nothing downstream keyed on Message-ID, while
  `internal/assembler`'s `seenDone`/`seenFailed` still do until F1 lands. That
  sentence is the stated reason A7 drops duplicates document-wide, so acting on
  it would have let a second segment be taken for a duplicate of the first —
  buffer released, assembled file silently short, no error raised. If a
  rewritten claim contains *nothing*, *never*, *always* or *only*, name what you
  actually checked and scope the claim to it.

- **Removing one term from a stated formula leaves the rest asserting
  something false.** Recompute the stated result from the surviving terms. This
  is arithmetic on what is already written down, not a re-derivation, and it
  takes one line. `docs/queue-lifecycle.md` read `~80 B + map` per article and
  `~3.3 MB` per 20k-article job; deleting the `+ map` term left `80 x 20,000`
  visibly failing to equal `3.3 MB`. Re-measuring showed the total had been
  wrong all along (1.64 MB), which the composite form had been hiding. A
  deliberate decision not to re-derive a figure is not a licence to leave a
  visible contradiction behind.

So when a change alters what a doc *describes* — a layer's responsibility, an
enforced invariant, a security property, a data-flow direction — **read
`docs/ARCHITECTURE.md` and the relevant `docs/*-contract.md` section in full at
the end**, rather than grepping them. That is two files and a few minutes, and
it is the only pass that catches a sentence which is wrong without containing
any of your keywords. Grep still covers the code.

Run `pr-review-toolkit:comment-analyzer` over the cumulative PR diff as well.
It and the grep cover different things: the analyzer reads the comments you
changed, the grep finds the ones you didn't.

Do this **once, on the last round** of a review-fix loop, not on every round:
each round's own fix creates fresh drift, so an early sweep goes stale.

**Sweep against the diff the commit will land as, not the diff that motivated
the edit.** The failure is subtler than "comments drift", and it has now
happened three times on this branch — twice shipping the drift in the *same*
commit as the change that caused it. Each time the sweep ran against the state
that *prompted* the correction: a reviewer names a stale sentence, the sentence
is rewritten to describe the fix, and a clause the same fix also invalidated is
carried forward untouched. The correction is real and the comment is still
wrong. Re-read each comment you touched against `git diff --cached` at the end,
as a reader who has not seen the finding that prompted it.

**Migrations are the case that cannot be fixed later.** A wrong claim in an
applied `goose` migration is frozen — the file must not be edited afterwards.
Sweep any migration this change adds *before* it merges; if a stale claim is
found in one already applied, correct it in a new migration's comment block and
name the statement it supersedes, rather than editing the original.

The schema is currently a single `001_initial.sql`, so there is no second
migration to hold a correction: a wrong claim there can only be superseded by
the next migration anyone adds. Sweep it especially carefully. Its
`jobs.recovery_bytes` block is the worked example of a superseding comment —
it states the corrected definition and why the derivation argument that
preceded it was wrong.

### Code Review Reception Protocol

When receiving code review feedback (from the user, PR comments, or external reviewers):

1. **Pause before editing**: Do NOT jump directly into writing code or applying edits.
2. **Evaluate & Acknowledge**: Restate each technical requirement or push back with technical reasoning if questionable. Do NOT use performative agreement ("You're absolutely right!", "Great point!").
3. **GitHub Thread Replies**: For inline review comments on GitHub, reply directly within the inline comment thread (`gh api repos/{owner}/{repo}/pulls/{pr}/comments/{id}/replies`), rather than posting top-level PR comments.
4. **Incremental Implementation**: Apply and test fixes one item at a time, running quality gates before committing.

### Tooling Setup

```bash
# Install goimports if not present
go install golang.org/x/tools/cmd/goimports@latest

# Install golangci-lint if not present (see https://golangci-lint.run/welcome/install/)

# Install gremlins (mutation testing) if not present
go install github.com/go-gremlins/gremlins/cmd/gremlins@latest
# scripts/run_gremlins.sh also requires systemd-run (systemd --user session)
# to enforce resource limits — check with: systemctl --user status
```

### After Editing Any `.go` File

Whenever you create, edit, or refactor a `.go` file, immediately run:

1. `goimports -w <filename>` — formats and resolves imports.
2. `go fix ./...` — applies Go toolchain modernizations (e.g., `min`/`max`
   builtins, `slices.Contains`, `wg.Go()`). Keeps the codebase current with the
   Go version in `go.mod`.
3. `go build ./...` — verify it compiles.

### Quality Gates (must pass before commit)

```bash
go fix ./...                          # Apply modernizations
goimports -w .                        # Format + resolve imports
go vet ./...                          # Must pass
go test -race ./...                   # Unit tests with the race detector
./scripts/run_tests.sh                # Full Go + UI suite
golangci-lint run ./...               # Must pass (no new issues)
```

Two gates in the Per-Change Commit Cycle above are not in that block, and
neither runs on its own — but they are not optional: the **observed** red check
(step 2) and the **claim sweep** (step 4). Both were skipped in practice while
every scripted gate stayed green, which is why each now has a runner covering
the part of it a machine can do:

```bash
go run ./scripts/mutate <spec>          # step 2 — every mutation must be KILLED
go run ./scripts/check_citations        # step 4 — the enumerations that carry a command
```

**Neither runner makes its gate automatic.** `scripts/mutate` checks the
mutations you thought to write, so it cannot tell you about the branch you did
not think to mutate; `check_citations` reaches only claims that embed a command,
and a claim about behaviour has none (see Rule 4). Both still have to be
invoked, and choosing what to put in the spec remains the judgment the gate is
actually made of.

`./scripts/run_tests.sh` runs the full Go and UI suites but **without** the race
detector, so `go test -race ./...` is a separate, required step.
Note: Standard `go test ./...` and `go test -race ./...` exclude files with `//go:build integration`. Whenever modifying files in `test/integration/` or changing startup wiring in `cmd/gonzbd/main.go` that integration tests consume, you MUST run `go test -tags=integration ./test/integration/...` locally before committing or pushing.

If any gate fails, fix the underlying issue. **Do not skip, suppress, or bypass
these checks** to make a commit go through. **Never insert dummy tests or dummy
variable references (`var _ = helper`) simply to satisfy `check_test_alignment`
or coverage numbers.** Write real unit tests validating the logic or use `//nocover: <reason>`
for trivial exempted code (see `docs/go-standards.md`). If a lint rule genuinely needs to be
disabled for a specific case, add a `//nolint:rulename // reason` comment
explaining why.

### Gate Semantics — what a failure means, and what a pass does not

The three custom gates pick their targets from the git diff (see
`scripts/gitscope`), but each then examines a **wider unit than the lines you
changed**. A gate can therefore fail on code you did not write. That is
working as intended, not a misfire, and it is not a regression you
introduced — diagnose before assuming your change caused it.

| Gate | Scope of a reported finding | Consequence |
|------|-----------------------------|-------------|
| `check_coverage` | Any function containing at least one changed line, measured **whole-function** against the 80% bar | Touching one line of a large, thinly covered function puts all of its branches on the bar. Extracting a helper counts as touching every call site. |
| `check_test_alignment` | Every unexported helper in a **touched file**, not just changed ones | A one-line fix to a hot file (`sqlite_store.go`, `app.go`) can surface a long-standing untested helper. There is no diff-scoped mode. |
| `check_lock_io` | A locked span plus **one** level of call-graph descent into a callee | I/O under a lock at callee depth >= 2 is invisible to the tool. A clean run is not proof; check callees by hand when narrowing or widening a lock. |

Three rules follow:

- **Never satisfy a gate by weakening it.** No dummy references, no test that
  asserts nothing, no `//nocover:` on code with real branching. If the finding
  is genuinely pre-existing debt in a file you merely touched, the fix is a
  real test for it — say so in the commit body so the scope is legible to a
  reviewer.
- **A green gate bounds nothing beyond its scope above.** State what was
  actually checked rather than that the gates passed.
- **Distrust `check_coverage` attribution while you have uncommitted changes**
  that shift a file's line count (issue #280). `gitscope.Diff()` unions the
  committed and working-tree diffs, whose hunk headers are numbered against
  different files, so committed hunks can land on the wrong function — in
  both directions. If a reported function looks untouched by your change,
  commit and re-run before writing a test for it.

Three further gates are **whole-repository**, not diff-scoped, and exist because
build, vet, lint and the test suite are structurally blind to what they check —
comments and Markdown are neither type-checked nor executed:

```bash
go run ./scripts/check_dup_comments     # duplicated multi-line // blocks
go run ./scripts/check_review_banner    # docs/reviews/*.md frozen-record banners
go run ./scripts/check_citations        # embedded grep / git grep claims whose count has moved
```

| Gate | What it catches | How to satisfy it |
|------|-----------------|-------------------|
| `check_dup_comments` | A multi-line `//` block appearing twice — usually a paste that still names the ORIGINAL declaration, so the copy authoritatively documents code it does not sit on | Rewrite the copy to describe what it sits on, or add `//dupcomment:ok <reason>` inside the block. The reason is mandatory, the marker must start the comment line, and a reason that wraps onto a second line must be closed by a blank `//` unless the marker is the block's last line — an unclosed wrapped reason is a hard exit-2 error, because guessing where it ends silently suppresses the finding on the *other* copy. Per-package copies of one helper file (same basename, distinct directories) are exempt automatically. |
| `check_review_banner` | An audit snapshot under `docs/reviews/` that does not declare itself frozen, or does not name the commit it describes | Add a blockquote with the phrase `Frozen record` and a backticked commit SHA. The check is presence-only — it does not judge whether the review's claims are still true, only that the file admits they may not be. |
| `check_citations` | A comment that embeds a backticked `grep` or `git grep` and states a count, where running the command no longer produces that count. This is Rule 4's enforcement arm: the rule requires the enumeration to be stated, and this runs it. | Re-run the command and correct the number, or correct the command so it means what the prose says — a count stated as "outside tests" whose command has no `\| grep -v _test.go` is the common case, as is a pattern that also matches its own comment or the declaration it describes. Where the population is real but not greppable ("the errors one function returns"), name it and do not dress it as a citation — and describe a historical command rather than backticking it, since a backticked example is indistinguishable from a live citation. A command with no count stated is reported as unverified, not failed. Commands are parsed to argv, only `grep` and `git grep` are executed, quoted text is treated as literal rather than as shell syntax, and every file operand must resolve inside the repository — so a comment can be neither a code-execution surface nor a way to read files outside the tree. |

All three are part of `ci.yml`, but `ci.yml` has no automatic trigger (see
"Continuous Integration" below) — it runs only on `workflow_dispatch`. So in
practice all three run when you run them locally, which is exactly what the
block above is for; dispatching CI by hand is the other way to reach them. They were once
absent from `ci.yml` entirely while the three diff-scoped gates were present,
which is how a defect in `check_dup_comments`' own marker handling survived in
the tree: nothing ever ran the tool that would have caught it. That failure
mode now depends on the local block being run, not on a server.

None of the three is diff-scoped, so any of them can fail on a file you did not
touch. Each found a real defect on its first run against this repository: a
package doc comment duplicated across two files of `scripts/nntpfaultproxy`; a
fixture comment in `internal/queue/progress_helpers_test.go` that named
`resetForReload` above a test of `clone`; and four wrong citations —
`internal/sched/advance.go` claiming `parkLocked` had two call sites when it had
three, two commands whose prose said "outside tests" while the command filtered
nothing, and one in `internal/job/job.go` that could not run at all because it
carried no path argument, so `grep` waited on stdin and reported zero.

### Mutation Testing (periodic, not a per-commit gate)

**Not to be confused with `scripts/mutate`.** That runs the *targeted* red
check of step 2: mutations you name, against a test you name, to prove one pin
discriminates. `gremlins` generates mutants across a whole package to find
behaviour nothing pins at all. Different questions — "does this test work?"
versus "what is untested?" — so neither substitutes for the other.

`gremlins` is **not** part of the per-commit quality gates above — it's too
slow and, with `--diff` broken upstream when scoped to a package, has no fast
incremental mode. Run it before opening a PR for a package with substantial
new branching/error-handling logic, or when you suspect a test is a
change-detector rather than a real pin on behavior. There is no CI
automation for it — it is entirely manual. **See
`docs/mutation-testing-playbook.md`** for the full process, including how to
attribute `LIVED`/`NOT COVERED` mutants to your change vs. pre-existing gaps.

### When You Get Stuck

If you cannot resolve a problem after a focused investigation:
- **Do not** try to work around the issue with a hack.
- **Do not** disable tests or skip checks.
- **Do** read the relevant Python code for clarity on intent.
- **Do** ask the user for direction with a specific proposal (see Decision Protocol below).

## Decision Protocol

This is the project-specific escalation *template*; the global
`~/.claude/CLAUDE.md` "Requires Discussion" list is the general trigger
list. Use this format whenever either applies.

When the spec or plan is ambiguous, or when an implementation choice will
significantly affect later work:

1. **Investigate first** — read the relevant Python code, check existing Go libraries, consider 2-3 approaches.
2. **Form an opinion** — pick the approach you would default to and the reasons.
3. **Present to the user** in this format:
   ```
   Decision needed: <one-line summary>

   Context: <why this matters, what depends on it>

   Options:
   1. <approach A> — pros/cons
   2. <approach B> — pros/cons
   3. <approach C> — pros/cons

   Recommendation: <your pick> because <reason>.
   ```
4. **Wait for direction** before proceeding on the affected work.

Decisions that don't need to be escalated:
- Variable names, function names, file organization within a package
- Test organization (table-driven, subtests, helpers)
- Whether to use `errors.Is` vs `errors.As` in a specific case
- Internal data structures that don't appear in any interface

Decisions that must be escalated:
- Adding new external dependencies (libraries) not already in use
- Changing public interfaces between packages
- Departing from the architecture in `docs/ARCHITECTURE.md`
- Adding a second constructor for a type, a second writer of a derived field, or a second enforcement point for one invariant (see "Standing Design Rules" — each is an owner-model violation, and each has shipped a defect here)
- Keeping a guard whose only justification is state an earlier build wrote, unless the security carve-out applies
- Persistence format changes (file paths, schema, on-disk layout)
- API behavior changes that affect compatibility with the existing Glitter web UI
- Database schema changes (always add a new `goose` migration in `internal/history/migrations/` — never modify existing migration files)

## Go Coding Standards, Testing Standards, and Backend Lessons Learned

Moved to [`docs/go-standards.md`](docs/go-standards.md) — read it before
touching any `.go` file. Covers idioms, anti-patterns, the decided
concurrency/persistence architecture, database migrations, library
selection, testing standards (including Red-Green discipline and coverage
exemptions), and the full Go backend lessons-learned catalog.

Config-specific sync rules (`gonzbd.yaml` comments, the config↔UI contract
test) moved to [`docs/config-contract.md`](docs/config-contract.md).

## Git Conventions

- **Branch**: **work lands via pull request by default**, including single-commit fixes. This holds even though it is a solo private repo, for two concrete reasons: the PR is the review surface that CodeRabbit and human review comment on, and `.github/workflows/security.yml` triggers on `pull_request` — pushing straight to `main` skips review entirely and runs the security scan only after the fact, when it is too late to be a gate. (`ci.yml` no longer triggers on `pull_request`; see "Continuous Integration" below.) A direct push to `main` requires the user to say so for that specific change — their standing preference is still the PR route.
- **This is a convention, not an enforced gate.** There is no GitHub branch protection configured for this repository, so nothing on the server will reject a direct push to `main`. It holds because we follow it. Do not read "the push succeeded" as "the push was allowed."
- **Worktrees**: for multi-step efforts, work in an isolated **git worktree** off `main`, then open a PR from that branch. Note a fresh worktree cannot build until you supply the UI bundle — `ui/dist/*` is gitignored, so `//go:embed all:dist` in `ui/embed.go` fails and `internal/web` reports `[setup failed]`. This is a worktree artifact, not a broken change:
  ```bash
  git worktree add /tmp/<lane> -b <lane>
  cp -r <main-checkout>/ui/dist /tmp/<lane>/ui/dist   # or build the UI
  ```
- **One step per commit** (or one logical sub-piece if a step is split).

(Merge/close approval, force-push, and quality-gates-before-push are global policy — see `~/.claude/CLAUDE.md`; not restated here.)

### Commit Convention

Follows the global Conventional Commits 1.0.0 policy in `~/.claude/CLAUDE.md`
(type table, scope/description/body/footer rules, breaking-change syntax).
Project-specific addition: **scope should be the Go package name or
subsystem** — `fix(assembler)`, `refactor(queue)`, `feat(nntp)`.

### Commit Hygiene (learned from real mistakes)

These rules exist because a batch of refactor commits violated them — the cost is misleading history that `git log <file>` and `git bisect` then propagate.

- **The subject line MUST describe what is actually in the diff.** Before committing, run `git diff --cached --stat` and confirm the scope and files match the message. A commit subjected `refactor(api): …` that actually changes `internal/assembler/` is a defect, not a typo — it makes the assembler change invisible to anyone searching api history.
- **One logical change per commit — verify, don't assume.** When several edits accumulate in the working tree, do not `git add -A` and split by timing. Stage per logical unit (`git add <paths>`) and confirm each commit contains only that unit. Two unrelated extractions (e.g. an assembler refactor and an api/config helper) must be two commits.
- **Per-path staging only works when the units own disjoint paths.** `git add <path>` stages the *working-tree* version of that file, not "the part of it belonging to this unit" — so if two logical units both touched `parser.go` and all the edits were made before the first commit, commit one silently gets both. This has happened: a commit subjected `refactor(nzb): fold the digest` carried an entire feature as well, and the rule above read as satisfied the whole time, because `git add -A` had been avoided. `git status` shows the file as staged either way.

  Before committing, check whether any touched file carries hunks from more than one unit. If it does, per-path staging cannot produce an honest boundary, and the fix is to reconstruct the intermediate: check the file out at the base commit, re-apply only the first unit's edits, verify it builds and its tests pass in isolation, commit, then restore the final version for the second. (Interactive `git add -p` is not available in this harness.)

  Verify with a **negative grep on the staged diff**, not by re-reading the message — `git diff --cached -- <path> | grep -c '<a-symbol-from-the-other-unit>'` must be `0`. This is also the two-commit-split rule's real purpose: not tidy history, but making the intermediate state exist so a red-green check is possible at all.
- **After rewriting commit boundaries, prove only the boundaries moved.** `git diff <old-tip> HEAD --stat` must be empty, and every commit must build and vet independently:
  ```bash
  for c in $(git rev-list --reverse <base>..HEAD); do
    git checkout -q --detach "$c" && go build ./... && go vet ./... || echo "BROKEN $c"
  done
  git checkout -q <branch>
  ```
- **Quantitative claims in commit bodies MUST be measured, not estimated.** If you write "drops cyclomatic complexity from 24 to <5," you must have run the tool (`gocyclo`/`gocognit`) on the result. An extraction reduces the *parent's* complexity by construction, but the magnitude is not guessable — a real case dropped 24→12, not the claimed <5. State the measured number or omit the claim.
- **Re-run `golangci-lint` on the final diff, not a mental model of it.** Refactors that convert control flow (e.g. fall-through `return` into boolean returns) can introduce *new* lint findings (`S1008`, `ifElseChain`) that did not exist in the original. The gate must be run against the code you are about to commit.

## Continuous Integration

**`ci.yml` is intentionally disabled and this is not a misconfiguration.** It
triggers on `workflow_dispatch` only — nothing runs automatically on push or on
pull request. Every gate it contains is run locally before each push (see
"Quality Gates" above), where the full suite completes in a fraction of the
runner's wall-clock time.

What this means in practice, for agents and humans alike:

- **Do not wait for CI, poll for checks, or run `/watch-ci` on this repo.** A
  PR whose only checks are Security Scan and CodeQL is in the expected state.
- **A PR with no CI run is not broken, not stuck, and not missing a step.** Do
  not investigate it, do not re-push to "trigger" it, and do not ask whether it
  is safe to proceed — it is.
- **The gates did not go away, only the server did.** The local block under
  "Quality Gates" is now the *only* thing standing between a defect and `main`,
  which raises rather than lowers the cost of skipping it. `go test -race ./...`
  and the whole-repo checks (`check_dup_comments`, `check_review_banner`,
  `check_citations`) are reached automatically nowhere — only by running them
  locally, or by dispatching `ci.yml` by hand.
- **`security.yml` still triggers on `push` and `pull_request`**, plus a weekly
  cron. It is unaffected by any of the above, and a failure there is real.
- **`codeql.yml` does too**, on the same two events plus a weekly cron, and a
  failure there is real for the same reason. It replaced GitHub's default
  code-scanning setup, which could not analyse Go at all: the extractor builds
  with `GOTOOLCHAIN=local`, so it used the runner image's pre-installed Go and
  failed against `go.mod`'s floor from the 1.27 bump onwards. That surfaced as
  a failing `Analyze (go)` check with no findings — a startup error, which
  reads as "nothing to report" and actually meant "did not run".

  The general lesson outlives this instance: **raising the floor in `go.mod`
  can silently disable any tool that pins its own toolchain**, and such a tool
  fails by not starting rather than by reporting. The 1.27 bump took out both
  CodeQL and `golangci-lint` this way, neither of which announced itself. After
  a version bump, check every consumer that builds the module, not only the
  ones whose config names a version.

To run CI deliberately — worth doing before a release, or when a change touches
build tags, the workflow files, or anything whose local and runner behaviour
could plausibly differ:

```bash
gh workflow run ci.yml --ref <branch>   # dispatch a manual run
gh run list --workflow=ci.yml --limit 5 # find it
gh run watch <run-id>                   # follow it
```

To restore automatic runs, replace the `on:` block in
`.github/workflows/ci.yml` with the `push`/`pull_request` triggers recorded in
this file's git history, and delete this section.

## Reading Python for Reference

When consulting the Python source for behavior clarification:

- Read for **intent and edge cases**, not for line-by-line translation.
- Python's threading model (single-threaded selector + threading.Lock) is **not** the Go model. Translate to goroutines + channels + RWMutex.
- Python's pickle persistence is **not** the Go model. Translate to JSON or SQLite as decided in the plan.
- Python's class hierarchies often translate to Go composition + interfaces. Don't reproduce inheritance.
- Variable naming should follow Go conventions (`MixedCaps`), not Python's `snake_case`.

When in doubt about whether a Python behavior is essential or accidental, ask.

## Key File Locations

- **API Handlers:** `internal/api/`
- **Download Engine:** `internal/downloader/`
- **Queue Logic:** `internal/queue/`
- **Web UI (Svelte SPA):** `ui/` — Svelte 5 + TypeScript + Vite, embedded via `//go:embed all:dist` in `ui/embed.go`
- **SPA Handler:** `internal/web/` — serves embedded dist with SPA catch-all fallback to index.html
- **Configuration Schema:** `internal/config/`

## Svelte 5 UI — Known Gotchas

Moved to [`docs/svelte-gotchas.md`](docs/svelte-gotchas.md) — read it before
touching any `.svelte`/`.svelte.ts` file. Covers module-level `$state`
reactivity, native `<dialog>`/`Modal.svelte` patterns, and child-component
update conventions.
