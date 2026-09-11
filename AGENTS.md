# AGENTS.md — Project Context & Instructions

This is the canonical guidance file for any AI agent (Claude Code, Gemini, etc.)
working in this repository. `CLAUDE.md` and `GEMINI.md` are symlinks to this
file. It must be read and followed for every session.

**This file states rules at the shortest form that can be acted on. The
argument for a rule — the incident it came from, the measurement that set its
shape, the worked example — lives wherever that argument can be checked**: a
topic doc, or the package doc of the tool that enforces it. When a rule gains a
runner, the prose arguing for it moves to the runner and leaves a command
behind. This file was split down to 313 lines once before and regrew to three
times that by accumulating postmortems; keeping the split is a standing
obligation, not a one-off cleanup.

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

Four constraints that precede any specific design decision. Each changes what
the right answer is, not merely how to write it down, and each has already been
missed by work that never had cause to open the doc arguing for it.

`docs/article-validation-contract.md` § "Ground rules" carries the full argument
for rules 1-3 and their worked cases. `docs/commit-cycle.md` carries the
evidence for rule 4. **These four statements are the rule; those documents are
the argument.**

### 1. No backwards compatibility

GoNZBD targets fresh installations, is not a drop-in replacement, and runs as a
single self-administered instance.

> **No change owes anything to state an earlier build wrote, or to parity with
> any other implementation.**

Persisted manifests, queue rows, history entries and NZB backups written before
a change may be assumed to satisfy the invariants that change introduces. There
is no drain period, no dual-read path, no migration, and no "old jobs behave
differently" caveat.

The rule's force is in what it forbids:

> **Before writing a guard, name the state that makes it necessary — then check
> whether that state is in scope at all.** If the only answer is "data an
> earlier build wrote", the guard is not needed. Delete the class rather than
> defend against it.

This does **not** weaken validation of what the *world* produces. An NZB and an
NNTP response stay untrusted regardless; on-disk corruption is a separate
failure class that `docs/durability-contract.md` owns.

**One carve-out, and it is narrow: the rule waives persistence FORMAT, not a
security invariant.** The test is what the value could *do*:

- A stale total, a missing counter, a figure from a superseded rule → the rule
  applies; delete the guard.
- A value interpolated into a protocol, a path, a query, or a command → the
  rule does not apply; keep the guard and say why at the check.

The carve-out was stretched twice on one PR to cover a Message-ID reaching an
NNTP command line, which is a command injection rather than a formatting
difference.

### 2. State has one owner

> **Every piece of derived state has exactly one function that computes it and
> exactly one path that mutates it. Everything else reads.**

A field whose value is *documented* as a function of other fields, but which
any caller may assign, is not an invariant — it is a comment.

> **When a check and an owner would both work, take the owner.** A check must be
> called at every site that could violate the invariant, and the failure mode of
> forgetting one is silence. An owner cannot be forgotten, because there is
> nowhere else to write.

Three smells this names, all of which have been real here:

- **Two constructors for one type.** Two independently-maintained paths
  populating the same fields will diverge, and a doc comment saying "both paths
  call this, so they cannot disagree" is a comment doing an owner's job — it
  covers only the fields someone remembered.
- **A derived value that is also persisted.** Anything recomputable from its
  parts should be derived on load, not stored and trusted. The stored copy is
  the one that drifts.
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

> **Classification decides WHERE a check belongs, not WHETHER one is owed.**

So before taking on article-validation work, ask what one instance costs when we
get it wrong. If the answer is "that article's bytes", it is par2's job and the
correct action is usually none. If the answer names *other* bytes, other
articles, or the whole file or job, it is a violation of this rule and it is
real work.

**A post with no par2 does not weaken this — it is the case that needs it
most.** Without this bound, a no-recovery post loses a download; with it, it
loses a hole.

**It takes an injection carve-out, like rule 1 does.** The bound never justifies
weakening a check whose absence would let a value reach a protocol, a path, a
query or a command. A Message-ID carrying CRLF fails both tests.

### 4. Enumerate before asserting

> **A comment that quantifies over a population of code — every writer, every
> caller, every deleter, every enforcement point — is only allowed to say what
> an enumeration you actually performed found. Perform it from source, at the
> moment you write the sentence.**

The words that trigger this are *only*, *sole*, *solely*, *never*, *always*,
*nothing else*, *the one place*, and every paraphrase of them. They are claims
about a set the reader cannot see, offered so that they do not have to go and
look — which is why a wrong one is worse than no comment at all: it does not
merely fail to help, it actively stops the check it replaced.

No gate catches this, and none could: see `docs/commit-cycle.md` § "Enumerate
before asserting" for the eight that shipped.

- **The enumeration is a command, not a recollection.** "I believe X is the
  only writer" and "`git grep -n 'X ='` returns three hits, two of which are
  tests" are different epistemic acts, and only the second is evidence. Run the
  grep even when — especially when — you are confident, and prefer
  grep-**then-read** over grep alone: the population you care about is usually
  the set of *arguments*, and a paraphrase carries none of your tokens.
- **State the basis in the comment.** "Barrier is the only writer" becomes
  "Barrier is the only writer — `INSERT` appears once, at
  `runstore_sqlite.go:233`". The citation is what lets the next reader re-run
  your check in one command instead of re-deriving your confidence.
- **Where the population is enumerable by a machine, write the test instead.** A
  count of call sites, a set of writers of one field, the members of a
  package-private door — these fail loudly when they move, where a comment fails
  silently.

**A claim about BEHAVIOUR is scoped by the branch that makes it true.** You
cannot grep "is anything lost?", and what would settle it is a path through the
code rather than a set of lines.

> **Before writing that something holds, find the branch it depends on and name
> it.** If the sentence would be false under some reachable configuration, say
> which one it assumes.

Where a behavioural claim is pinned by a test, mutate the branch and require the
test to die (see Step 2 below); where it lives only in a comment, name the
branch in the sentence.

**The narrowing half of this is stated separately** under "Two checks on what
you WROTE" below — *narrowing a referent must not broaden a scope*. That clause
governs a sentence a change **falsified**; this rule governs a sentence you are
**writing for the first time**. When a claim you were about to write turns out
to be false, the fix is to say what still holds and name what you checked, never
to reach for a weaker universal.

## Repository Layout

- `cmd/gonzbd/`: Entry point, flag parsing, and application orchestration.
- `internal/`: Core packages (API, app, downloader, job, sched, dispatch, nzb, assembler, decoder, etc.).
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

| Doc | Read before |
|-----|-------------|
| [`docs/go-standards.md`](docs/go-standards.md) | Creating, editing, or refactoring any `.go` file |
| [`docs/svelte-gotchas.md`](docs/svelte-gotchas.md) | Creating, editing, or refactoring any `.svelte`/`.svelte.ts` file |
| [`docs/config-contract.md`](docs/config-contract.md) | Adding/renaming/removing a config field or a Svelte config `keyword=` prop |
| [`docs/commit-cycle.md`](docs/commit-cycle.md) | Arguing that a commit-cycle or quality-gate rule does not apply to your change, or changing one |
| [`docs/article-validation-contract.md`](docs/article-validation-contract.md) | Touching `internal/nzb`, `internal/nntp`, `internal/decoder`, the decode/reconcile path in `internal/downloader`, or the accept path in `internal/assembler` |
| [`docs/job-lifecycle.md`](docs/job-lifecycle.md) | Touching job residency, the state model, or `Manifest`/`JobProgress` access |
| [`docs/dispatch-contract.md`](docs/dispatch-contract.md) | Touching `internal/dispatch` or `internal/sched` |
| [`docs/nntp-downloader-contract.md`](docs/nntp-downloader-contract.md) | Touching `internal/downloader` or `internal/nntp` |
| [`docs/durability-contract.md`](docs/durability-contract.md) | Touching `internal/durability`, `internal/storagefault`, `internal/assembler`, or `internal/directunpack` |
| [`docs/post-processing-contract.md`](docs/post-processing-contract.md) | Touching `internal/postproc`, `internal/par2`, or `internal/unpack` |
| [`docs/mutation-testing-playbook.md`](docs/mutation-testing-playbook.md) | Running `gremlins` |

Each doc's own header says what it covers; summarising it here creates a second
copy that drifts. The trigger is the only column this table needs.

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
go test -v -tags=integration ./test/integration/... ./internal/par2/...   # Integration (requires par2, rar, unrar, 7z)
go test -v -tags=uitest ./test/uitest/...                   # UI/Playwright (requires pre-built UI + Playwright Chromium)
go test -timeout=10m ./test/e2e/                            # E2E (requires live Usenet server; env-gated, not tag-gated)
go test -tags=crash -timeout=20m ./test/crash/              # Crash consistency (Linux; kills a real child process)
go test ./internal/config/ -run 'TestUI|TestAllFlat'        # Config ↔ UI contract
go vet ./...                                                # Static analysis
golangci-lint run ./...                                     # Linting
./scripts/run_gremlins.sh ./internal/job                    # Mutation testing on a package (periodic, see below — not a per-commit gate)
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
   `docs/go-standards.md` § Red-Green Discipline, and Step 2 below — "confirm"
   means *observed*, not reasoned).
3. **Verify** all quality gates pass (see below).
4. **Sweep** the comments and docs the change falsified (see below).
5. **Commit** with a Conventional Commits message. Mention the plan step in the
   body if useful context.

Each commit must leave the repository in a working state
(`go build ./... && go test ./...` passes).

#### Step 2 in practice: the red check is mechanical, not mental

A test written to pin a fix is not a pin until that fix has been reverted and
the test *observed* to fail. "Mentally reverting" is not sufficient, and pins
that passed against unfixed code have shipped anyway.

**Use `scripts/mutate` rather than hand-rolling the revert.** Write a spec
naming the package, the test, and each mutation; the tool applies them one at a
time, requires each to produce a red result, and restores the file on every exit
path including SIGINT. It enforces `-count=1`, refuses an anchor that does not
match exactly once, and distinguishes a compile error from a killed mutation —
three invariants that a hand-rolled harness has to re-derive per use, and
measurably fails to. `scripts/mutate/main.go`'s package doc is the reference for
its five verdicts; `docs/commit-cycle.md` has the measurement.

```bash
go run ./scripts/mutate path/to/the.spec     # exits non-zero unless every mutation is KILLED
```

```text
pkg ./internal/job/
run TestTheNewPin

[the guard neutered]
file internal/job/progress.go
--- anchor
	if !isJobStamp(t) || !p.downloadFinished.IsZero() {
--- replace
	if false {
--- end
```

<!-- doccite:ok TestTheNewPin — a deliberate placeholder; the sentence below says it names no test -->
<!-- doccite:ok internal/pkg/target.go — the spec format's illustrative path, not a real file -->

That anchor is a real line, and keeping it one is deliberate — an example
anchored on text the tree no longer contains still reads as a working example.
Its `run` line is a placeholder: `TestTheNewPin` names no test.
`internal/job/testdata/postproc_stamp.spec` is the same mutation as a committed
spec, and runs `TestMarkDownloadFinished_FirstWins`.
`scripts/mutate/testdata/self.spec` is the tool's own red check, and running it
is how you verify a change to the tool.

Two judgments the tool cannot make for you:

- **Revert each half separately.** A fix with two call sites needs two reverts;
  one half being pinned says nothing about the other.
- **Prefer neutering a condition to deleting a block.** A compile error does not
  demonstrate the test would have caught the behaviour.

**Never `git stash`** — the stash stack is shared with any other session in this
repo and a pop can take their work. Restore from your own copy rather than
`git checkout -- <path>`, which also discards unrelated uncommitted edits in
that file. (`scripts/mutate` already does both correctly.)

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

`git grep` rather than a path list, because the copies turn up in `cmd/`,
`test/`, `ui/`, and this file.

Three things grep does not cover, each of which has shipped drift here
(`docs/commit-cycle.md` § "The sweep" has the worked cases):

- **`git grep` is blind to paraphrase, and the docs are where paraphrase
  lives.** A code comment usually repeats the symbol; a `docs/*.md` file
  restates the claim in prose and shares no token with the code.
- **A table row is the same claim, compressed** — and it carries the token but
  sits nowhere near the prose that explains it, so rewriting the paragraph
  leaves the row stating the old version in four words.
- **A literal is what is written down; the concept is only what you were
  thinking about.** When a change alters a status code, a duration, a
  threshold, a limit or a field name, sweep for that literal from the root:
  `git grep -n '222'`, not a search for "the Message-ID check".

So when a change alters what a doc *describes* — a layer's responsibility, an
enforced invariant, a security property, a data-flow direction — **read
`docs/ARCHITECTURE.md` and the relevant `docs/*-contract.md` section in full at
the end**, rather than grepping them. That is two files and a few minutes, and
it is the only pass that catches a sentence which is wrong without containing
any of your keywords. Grep still covers the code.

**Two checks on what you WROTE, not on what you removed.** Finding the stale
sentence is only half the sweep, and neither of these is caught by any gate.

- **Narrowing a referent must not broaden a scope.** When a change deletes the
  thing a justification named, the fix is to say what *still* holds, not to
  assert a universal. "`internal/queue` no longer keys on X" is not "nothing
  keys on X". If a rewritten claim contains *nothing*, *never*, *always* or
  *only*, name what you actually checked and scope the claim to it.
- **Removing one term from a stated formula leaves the rest asserting something
  false.** Recompute the stated result from the surviving terms. This is
  arithmetic on what is already written down, and it takes one line.

**Sweep against the diff the commit will land as, not the diff that motivated
the edit.** Re-read each comment you touched against `git diff --cached` at the
end, as a reader who has not seen the finding that prompted it. This one has
shipped three times on one branch; `docs/commit-cycle.md` § "Sweeping against
the wrong diff" has the shape.

Run `pr-review-toolkit:comment-analyzer` over the cumulative PR diff as well.
It and the grep cover different things: the analyzer reads the comments you
changed, the grep finds the ones you didn't. Do this **once, on the last round**
of a review-fix loop — each round's own fix creates fresh drift, so an early
sweep goes stale.

**Migrations are the case that cannot be fixed later.** A wrong claim in an
applied `goose` migration is frozen — the file must not be edited afterwards.
Sweep any migration this change adds *before* it merges; if a stale claim is
found in one already applied, correct it in a new migration's comment block and
name the statement it supersedes. The schema is currently a single
`001_initial.sql`, so there is no second migration to hold a correction. Its
`jobs.recovery_bytes` block is the worked example of a superseding comment.

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
go vet -tags=integration,uitest,crash ./...       # The 3 tagged suites still compile
go test -race ./...                   # Unit tests with the race detector
./scripts/run_tests.sh                # Full Go + UI suite
golangci-lint run ./...               # Must pass (no new issues)
```

Plus the two gates that have a runner but no automatic trigger:

```bash
go run ./scripts/mutate <spec>          # step 2 — every mutation must be KILLED
go run ./scripts/check_citations        # step 4 — the enumerations that carry a command
```

**Neither runner makes its gate automatic.** `scripts/mutate` checks the
mutations you thought to write, so it cannot tell you about the branch you did
not think to mutate; `check_citations` reaches only claims that embed a command,
and a claim about behaviour has none (see Rule 4). Choosing what to put in the
spec remains the judgment the gate is actually made of.

Notes on the gate block:

- `./scripts/run_tests.sh` runs the full Go and UI suites but **without** the
  race detector, so `go test -race ./...` is a separate, required step.
- Standard `go test ./...` and `go test -race ./...` exclude files with
  `//go:build integration`. Whenever modifying files in `test/integration/` or
  changing startup wiring in `cmd/gonzbd/main.go` that integration tests
  consume, you MUST run
  `go test -tags=integration ./test/integration/... ./internal/par2/...`
  locally before committing or pushing.
- **A tagged file is invisible to every default gate, and it rots silently.**
  `go vet -tags=integration,uitest,crash ./...` compiles the three tagged
  suites without running them. Scope it exactly: it covers the three tags this
  repository defines, built for the **host `GOOS`/`GOARCH`**, so files behind
  an OS constraint stay invisible. It proves those files build; it does not
  prove they still assert anything. (`docs/commit-cycle.md` § "Why the gates
  are shaped the way they are" has the six-week regression that motivated it.)
- `.golangci.yml`'s `run.build-tags` lists all three tags, so the default
  `golangci-lint run ./...` lints the tagged files too, with no flag needed.

If any gate fails, fix the underlying issue. **Do not skip, suppress, or bypass
these checks** to make a commit go through. **Never insert dummy tests or dummy
variable references (`var _ = helper`) simply to satisfy `check_test_alignment`
or coverage numbers.** Write real unit tests validating the logic or use
`//nocover: <reason>` for trivial exempted code (see `docs/go-standards.md`). If
a lint rule genuinely needs to be disabled for a specific case, add a
`//nolint:rulename // reason` comment explaining why.

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
| `check_lock_io` | A locked span plus **one** level of call-graph descent into a `*Locked`-named callee | Callee descent is *only* into callees with the `*Locked` suffix naming convention. Un-suffixed callees (e.g. `reallyQuery`, `flushRow`) are not inspected even at depth 1, and I/O under a lock at callee depth >= 2 is invisible to the tool. A clean run is not proof; check callees by hand when narrowing or widening a lock. |

Three rules follow:

- **Never satisfy a gate by weakening it.** No dummy references, no test that
  asserts nothing, no `//nocover:` on code with real branching. If the finding
  is genuinely pre-existing debt in a file you merely touched, the fix is a
  real test for it — say so in the commit body so the scope is legible to a
  reviewer.
- **A green gate bounds nothing beyond its scope above.** State what was
  actually checked rather than that the gates passed.
- **Distrust `check_coverage` attribution while you have uncommitted changes**
  that shift a file's line count (issue #280). If a reported function looks
  untouched by your change, commit and re-run before writing a test for it —
  `docs/commit-cycle.md` has why.

Five further gates are **whole-repository**, not diff-scoped, and exist because
build, vet, lint and the test suite are structurally blind to what they check —
comments and Markdown are neither type-checked nor executed:

```bash
go run ./scripts/check_dup_comments         # duplicated multi-line // blocks
go run ./scripts/check_review_banner        # docs/reviews/*.md frozen-record banners
go run ./scripts/check_citations            # embedded grep / git grep claims whose count has moved
go run ./scripts/check_doc_citations        # cited paths and Test names that resolve to nothing
go run ./scripts/check_test_doubles --all   # test doubles or test-named files leaking into production builds
```

| Gate | What it catches | How to satisfy it |
|------|-----------------|-------------------|
| `check_dup_comments` | A multi-line `//` block appearing twice — usually a paste that still names the ORIGINAL declaration, so the copy authoritatively documents code it does not sit on | Rewrite the copy to describe what it sits on, or add `//dupcomment:ok <reason>` inside the block. |
| `check_review_banner` | An audit snapshot under `docs/reviews/` that does not declare itself frozen, or does not name the commit it describes | Add a blockquote with the phrase `Frozen record` and a backticked commit SHA. Presence-only — it does not judge whether the review's claims are still true. |
| `check_citations` | A comment that embeds a backticked `grep` or `git grep` and states a count, where running the command no longer produces that count. Rule 4's enforcement arm. | Re-run the command and correct the number, or correct the command so it means what the prose says. Where the population is real but not greppable ("the errors one function returns"), name it and do not dress it as a citation. |
| `check_doc_citations` | A cited file path that names nothing in the tree, a `Test...` name cited as a guard that is declared nowhere, or a `doc.md § Section` citation naming a heading that document does not have. Reads Markdown too, and claims with no command behind them. A bare `§3.4` names no document and is deliberately not checked. | Correct the reference, or mark it deliberate — `//doccite:ok <token> — <why>` in Go, `<!-- doccite:ok <token> — <why> -->` in Markdown. |
| `check_test_doubles` | Test doubles (`Fake`, `Mock`, `Stub`, `Nop`, `*ForTesting`) or test-named files leaking into production builds without test build tags | Add a recognized test build tag, move to a `*_test.go` file, or add `//testdouble:allow <reason>` — on the declaration, or before `package` to exempt a whole test-named file. |

**Every marker's `<reason>` is mandatory — a bare marker is itself an error.**
Each tool's package doc under `scripts/` owns the rest of its behaviour: what
it scans, how markers wrap, and (for `check_citations`) why it parses to argv
and never runs a shell. Read the tool when a finding looks wrong, rather than
expecting this table to explain it.

All five are in `ci.yml`, but `ci.yml` has no automatic trigger (see
"Continuous Integration" below), so in practice they run when you run them
locally. None is diff-scoped, so any of them can fail on a file you did not
touch.

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

- **Branch**: **work lands via pull request by default**, including single-commit fixes. This holds even though it is a solo private repo, for two concrete reasons: the PR is the review surface that CodeRabbit and human review comment on, and `.github/workflows/security.yml` triggers on `pull_request` — pushing straight to `main` skips review entirely and runs the security scan only after the fact, when it is too late to be a gate. A direct push to `main` requires the user to say so for that specific change — their standing preference is still the PR route.
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

### Commit Hygiene

These rules exist because a batch of refactor commits violated them — the cost
is misleading history that `git log <file>` and `git bisect` then propagate.
`docs/commit-cycle.md` § "Commit hygiene" has the cases.

- **The subject line MUST describe what is actually in the diff.** Before
  committing, run `git diff --cached --stat` and confirm the scope and files
  match the message.
- **One logical change per commit — verify, don't assume.** Stage per logical
  unit (`git add <paths>`), never `git add -A` with a split by timing.
- **Per-path staging only works when the units own disjoint paths.** `git add
  <path>` stages the *working-tree* version of that file, so two units that both
  touched one file collapse into the first commit. Before committing, check
  whether any touched file carries hunks from more than one unit; if it does,
  reconstruct the intermediate rather than pretending the boundary is honest.
  Verify with a **negative grep on the staged diff**, not by re-reading the
  message — `git diff --cached -- <path> | grep -c '<a-symbol-from-the-other-unit>'`
  must be `0`.
- **After rewriting commit boundaries, prove only the boundaries moved.**
  `git diff <old-tip> HEAD --stat` must be empty, and every commit must build
  and vet independently:
  ```bash
  for c in $(git rev-list --reverse <base>..HEAD); do
    git checkout -q --detach "$c" && go build ./... && go vet ./... || echo "BROKEN $c"
  done
  git checkout -q <branch>
  ```
- **Quantitative claims in commit bodies MUST be measured, not estimated.** If
  you write "drops cyclomatic complexity from 24 to <5," you must have run
  `gocyclo`/`gocognit` on the result. State the measured number or omit the claim.
- **Re-run `golangci-lint` on the final diff, not a mental model of it.**
  Refactors that convert control flow can introduce *new* findings that did not
  exist in the original.

## Continuous Integration

**`ci.yml` is intentionally disabled and this is not a misconfiguration.** It
triggers on `workflow_dispatch` only — nothing runs automatically on push or on
pull request. Every gate it contains is run locally before each push (see
"Quality Gates" above).

- **Do not wait for CI, poll for checks, or run `/watch-ci` on this repo.** A
  PR whose only checks are Security Scan and CodeQL is in the expected state.
  A PR with no CI run is not broken, not stuck, and not missing a step.
- **The gates did not go away, only the server did.** The local block under
  "Quality Gates" is now the *only* thing standing between a defect and `main`,
  which raises rather than lowers the cost of skipping it.
- **`security.yml` and `codeql.yml` still trigger on `push` and
  `pull_request`**, plus a weekly cron. A failure in either is real.

To run CI deliberately — worth doing before a release, or when a change touches
build tags, the workflow files, or anything whose local and runner behaviour
could plausibly differ:

```bash
gh workflow run ci.yml --ref <branch>   # dispatch a manual run
gh run list --workflow=ci.yml --limit 5 # find it
gh run watch <run-id>                   # follow it
```

`docs/commit-cycle.md` § "Why CI is disabled" has the CodeQL/toolchain history
and how to restore automatic runs.

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
- **Job Record & Content Tiers:** `internal/job/` — the `Job`, its `Manifest` and `JobProgress`
- **Scheduling Decisions:** `internal/sched/` — leases, compute slots, state transitions, no I/O
- **Job Registry & Persistence:** `internal/dispatch/` — the ordered registry, the tick loop, manifest residency, queue-state rows
- **Batched Progress Writes:** `internal/checkpoint/`
- **Web UI (Svelte SPA):** `ui/` — Svelte 5 + TypeScript + Vite, embedded via `//go:embed all:dist` in `ui/embed.go`
- **SPA Handler:** `internal/web/` — serves embedded dist with SPA catch-all fallback to index.html
- **Configuration Schema:** `internal/config/`

## Svelte 5 UI — Known Gotchas

Moved to [`docs/svelte-gotchas.md`](docs/svelte-gotchas.md) — read it before
touching any `.svelte`/`.svelte.ts` file. Covers module-level `$state`
reactivity, native `<dialog>`/`Modal.svelte` patterns, and child-component
update conventions.
