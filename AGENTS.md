# AGENTS.md — Project Context & Instructions

This is the canonical guidance file for any AI agent (Claude Code, Gemini, etc.)
working in this repository. `CLAUDE.md` and `GEMINI.md` are symlinks to this
file. It must be read and followed for every session.

> These instructions are project-specific. They override default agent behavior
> but defer to the user's explicit instructions and to the global
> `~/.claude/CLAUDE.md` (which defines, among other things, the global
> Conventional Commits policy).

> **⮕ Active multi-session work:** the v2→v1 porting effort is coordinated in
> [`docs/superpowers/plans/PORT-HANDOFF.md`](docs/superpowers/plans/PORT-HANDOFF.md).
> If you are resuming/continuing that work, **read it first** and follow its
> resume protocol. Progress is tracked by the checkboxes in the plan
> (`docs/superpowers/plans/2026-06-15-port-v2-improvements-to-v1.md`) and by git
> history — not by any in-session TODO list.

## Project Context

GoNZBD is a high-performance Go reimplementation of [SABnzbd](https://sabnzbd.org),
the automated Usenet binary newsreader. It targets fresh installations and is
**not** a drop-in replacement for the Python version. The reference Python
implementation lives at `../sabnzbd/`.

- **Module path:** `github.com/hobeone/gonzbd`
- **Go version:** 1.26.4 (toolchain 1.26.4)
- **Status:** Core backend download pipeline and legacy mode-dispatch API
  (`/api?mode=...`) are functional. The Glitter web UI port (Phase 12) is the
  current active focus.
- **Main technologies:**
    - **Language:** Go 1.26.4+
    - **Configuration:** YAML (`gopkg.in/yaml.v3`)
    - **Persistence:** SQLite (`modernc.org/sqlite`, pure Go) for history;
      JSON+gzip for queue state.
    - **Logging:** Structured logging via `log/slog`.
    - **Concurrency:** Idiomatic goroutines + channels; `sync.RWMutex` for
      shared state.

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
| [`docs/svelte-gotchas.md`](docs/svelte-gotchas.md) | Creating, editing, or refactoring any `.svelte`/`.svelte.ts` file | Svelte 5 reactivity gotchas (module-level `$state`, `bits-ui` dialogs, child component update patterns) |
| [`docs/config-contract.md`](docs/config-contract.md) | Adding/renaming/removing a config field or a Svelte config `keyword=` prop | Keeping `gonzbd.yaml` comments, `docs/sabnzbd_spec.md` §9.x, and the config↔UI contract test in sync |
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

When using superpowers/workflow skills (like `brainstorming` or `writing-plans` or `work`), **always** write all design documents, specifications, and implementation plans directly to the session artifact directory: `<appDataDir>/brain/<conversation-id>/`.
- Do NOT write them to repository folders (like `docs/superpowers/` or similar), unless specifically requested by the user.
- All such artifacts MUST include `ArtifactMetadata` with `RequestFeedback: true` and `UserFacing: true`. This guarantees they are rendered in the interactive review modal, enabling the execution checkpoint/Proceed button.
- Filename conventions:
  - Design/Specs: `YYYY-MM-DD-<feature-name>-design.md`
  - Implementation Plans: `YYYY-MM-DD-<feature-name>-plan.md`

### Per-Change Commit Cycle

Each logical change is a self-contained unit of work. The workflow is:

1. **Read** the relevant spec/architecture sections.
2. **Implement** the change. For a bug fix, write the failing test *first* and
   confirm it fails on the unpatched code before applying the fix (see
   `docs/go-standards.md` § Red-Green Discipline).
3. **Verify** all quality gates pass (see below).
4. **Commit** with a Conventional Commits message. Mention the plan step in the
   body if useful context.

Each commit must leave the repository in a working state
(`go build ./... && go test ./...` passes).

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

### Mutation Testing (periodic, not a per-commit gate)

`gremlins` is **not** part of the per-commit quality gates above — it's too
slow and, with `--diff` broken upstream when scoped to a package, has no fast
incremental mode. Run it before opening a PR for a package with substantial
new branching/error-handling logic, or when you suspect a test is a
change-detector rather than a real pin on behavior; it also runs on a
rotation via the `mutation-testing` GitHub Actions workflow. **See
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

- **Branch**: **never commit directly to `main`. All work lands via pull request**, including single-commit fixes. This holds even though it is a solo private repo, for two concrete reasons: the PR is the review surface that CodeRabbit and human review comment on, and both `.github/workflows/ci.yml` and `.github/workflows/security.yml` already trigger on `pull_request` — pushing straight to `main` skips review entirely and runs CI only after the fact, when it is too late to be a gate.
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
- **Quantitative claims in commit bodies MUST be measured, not estimated.** If you write "drops cyclomatic complexity from 24 to <5," you must have run the tool (`gocyclo`/`gocognit`) on the result. An extraction reduces the *parent's* complexity by construction, but the magnitude is not guessable — a real case dropped 24→12, not the claimed <5. State the measured number or omit the claim.
- **Re-run `golangci-lint` on the final diff, not a mental model of it.** Refactors that convert control flow (e.g. fall-through `return` into boolean returns) can introduce *new* lint findings (`S1008`, `ifElseChain`) that did not exist in the original. The gate must be run against the code you are about to commit.

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
reactivity, `bits-ui` dialog patterns, and child-component update
conventions.
