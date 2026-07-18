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
   the mandatory "Decision Needed" escalation format, and the lessons-learned
   catalog at the bottom. Read this first for every session.
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
./scripts/run_gremlins.sh ./internal/queue                  # Mutation testing on a package
```

> **WARNING:** Running `gremlins` on the entire repository (e.g. `./...` or `./internal/...`) is forbidden. It will run dozens of mutant compiles in parallel, exhausting disk space and filling up `/tmp`. Always scope `gremlins` to a single focused package.
>
> **Always invoke gremlins via `./scripts/run_gremlins.sh <pkg>`, never call
> `gremlins unleash` directly.** gremlins copies the whole working directory
> into each worker's isolated build dir and does not respect `.gitignore`
> while doing so; a bare invocation with the scratch dir anywhere inside the
> repo (as an earlier version of this script did) causes each worker's copy
> to recursively sweep up scratch data from prior workers/runs, nesting many
> levels deep — this produced 168–394GB of disk usage from a single run,
> independent of worker count, and coincided with kernel OOM kills. The
> wrapper script fixes this (scratch dir relocated outside the repo, wiped
> per run) and adds resource limits: a hard memory cap via a `systemd --user`
> cgroup scope (so OOM kills only this run's own processes, not arbitrary
> processes system-wide) and a background watchdog enforcing a disk-usage
> cap and wall-clock timeout. Tunable via `GREMLINS_WORKERS` (auto-detected
> based on CPU cores and RAM: `min(nproc/2, total_ram_gb/4)`, clamped 4–16;
> e.g. 12 workers on 24-core/94GB RAM systems), `GREMLINS_MEMORY_MAX` (auto-detected
> to 80% of total system RAM, fallback 32G), `GREMLINS_DISK_MAX_MB` (default 51200),
> `GREMLINS_TIMEOUT_SECS` (default 1800), `GREMLINS_DIR` (scratch dir base —
> must not be inside the repo). Hardware tuning guidance:
> - 8 CPU / 16GB RAM: `GREMLINS_WORKERS=4`, `GREMLINS_MEMORY_MAX=12G`
> - 16 CPU / 32GB RAM: `GREMLINS_WORKERS=8`, `GREMLINS_MEMORY_MAX=24G`
> - 24+ CPU / 64GB+ RAM: `GREMLINS_WORKERS=12-16`, `GREMLINS_MEMORY_MAX=48G-64G`
> Requires `systemd-run` (Linux only, matching
> this project's platform target); the script refuses to run without it
> rather than falling back to an unconfined invocation. Mutant-type
> selection and `timeout-coefficient` are configured project-wide in
> `.gremlins.yaml` — no need to pass those as flags.
>
> **KNOWN BUG — `--diff` is broken when scoped to a package** (the only way
> this repo permits it to be run): gremlins v0.6.0 has a confirmed upstream
> bug ([go-gremlins/gremlins#278](https://github.com/go-gremlins/gremlins/issues/278))
> where `--diff <ref>` combined with any package/subdirectory path (or even
> `cd`-ing into the package and omitting the path) reports every mutant in the
> changed file as `SKIPPED` (0 killed / 0 lived / 0 not-covered), regardless of
> what's actually in the diff — a false "clean" result. Whole-package
> (non-`--diff`) runs are unaffected and produce real kill/live numbers. Do
> not rely on `--diff` for the mutation gate until this is fixed upstream; see
> `docs/mutation-testing-playbook.md` § Known limitation for the required
> workaround.

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
   confirm it fails on the unpatched code before applying the fix (see Testing
   Standards § Red-Green Discipline).
3. **Verify** all quality gates pass (see below).
4. **Commit** with a Conventional Commits message. Mention the plan step in the
   body if useful context.

Each commit must leave the repository in a working state
(`go build ./... && go test ./...` passes).

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
./scripts/run_gremlins.sh ./internal/<pkg> # Whole-package mutation baseline
```

`./scripts/run_tests.sh` runs the full Go and UI suites but **without** the race
detector, so `go test -race ./...` is a separate, required step.

The `gremlins` gate enforces mutation-testing proof: every behavioral change in
the diff must be killed by a test (no surviving/lived mutants). If a mutant
lives, the test suite does not actually pin that behavior — add or strengthen
the test rather than weakening the gate. Run it scoped to the changed package
during development and before commit
(`./scripts/run_gremlins.sh ./internal/<pkg>`). **Do not use
`--diff`** — it is currently broken when scoped to a package (see the KNOWN BUG
note above); instead, run the whole-package baseline and manually
cross-reference `LIVED`/`NOT COVERED` line numbers against `git diff
origin/main -- internal/<pkg>` to attribute them to your change vs.
pre-existing gaps. **NEVER run gremlins on the entire repository (e.g. `./...`) as it will fill up `/tmp` and exhaust disk space.** See **`docs/mutation-testing-playbook.md`**
for the repeatable process for triaging `LIVED`/`NOT COVERED` mutants and
closing the gaps with targeted tests.

If any gate fails, fix the underlying issue. **Do not skip, suppress, or bypass
these checks** to make a commit go through. If a lint rule genuinely needs to be
disabled for a specific case, add a `//nolint:rulename // reason` comment
explaining why.

### When You Get Stuck

If you cannot resolve a problem after a focused investigation:
- **Do not** try to work around the issue with a hack.
- **Do not** disable tests or skip checks.
- **Do** read the relevant Python code for clarity on intent.
- **Do** ask the user for direction with a specific proposal (see Decision Protocol below).

## Decision Protocol

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

## Go Coding Standards

### Idioms (Required)

- **Accept interfaces, return structs**. Define interfaces at the consumer side, not the producer side.
- **Small interfaces**. Single-method interfaces are good. Compose with embedding when needed.
- **Context propagation**. Every blocking operation accepts `context.Context` as its first parameter.
- **Error wrapping**. Use `fmt.Errorf("operation failed: %w", err)` to preserve error chains. Never use `%v` on errors.
- **Structured logging**. Use `log/slog`. Pass `*slog.Logger` via constructor; do not use a package-level global logger. **All loggers must be component-scoped** using `.With("component", "name")` to support log filtering.
- **Goroutine lifecycle**. Every goroutine has a clearly defined exit condition tied to a context, channel close, or explicit signal. No "fire and forget" goroutines.
- **Standard library first**. Prefer `slices`, `maps`, `errors.Is/As`, and `min`/`max` builtins over custom helpers or reflection.

### Anti-Patterns (Forbidden)

- **No `panic` for control flow.** Panic is for unrecoverable programmer errors only.
- **No silent error swallowing.** `_ = doSomething()` requires a comment explaining why the error is intentionally ignored.
- **No `time.Sleep` in tests** for synchronization. Use channels, `sync.WaitGroup`, or `chan struct{}` signals.
- **No `init()` functions** for non-trivial setup. Use explicit `New*` constructors called from `main`.
- **No global mutable state.** Configuration, loggers, and dependencies are passed explicitly.
- **No `interface{}` / `any`** in new code unless absolutely required (e.g., generic JSON handling). Prefer concrete types or generics. When a dynamic type is necessary, prefer `any` over `interface{}`.

### Database Migrations

All schema changes MUST be implemented as a new `goose` migration file in
`internal/history/migrations/`. **Never modify existing migration files.**

### Config Documentation Sync

The root `gonzbd.yaml` contains inline comments above every directive documenting
its purpose, valid values, and important considerations. When adding, renaming,
or removing config fields in `internal/config/`, you MUST update the
corresponding comments in `gonzbd.yaml` and `test/fixtures/gonzbd.yaml` to stay
in sync. Also update `docs/sabnzbd_spec.md` §9.x tables.

### Config ↔ UI Contract Test

`internal/config/ui_contract_test.go` contains `TestUIKeywordsAreValidConfigTags`,
which is the canonical list of every `keyword=` prop used in Svelte config
components. **This file must be kept in sync with both the Go config structs and
the Svelte UI.** Specifically:

- When you **add a new `keyword=` prop** to any `ConfigInput`, `ConfigSwitch`, or `ConfigTextarea` in `ui/src/lib/components/config/`, add a matching entry to `uiKeywords` in `ui_contract_test.go`.
- When you **remove or rename a Svelte keyword**, remove or update the corresponding entry.
- When you **rename or remove a Go config field** (changing its `json:` tag), the test `TestAllFlatConfigTagsAreSettable` will catch the breakage automatically — but you must also update any matching Svelte `keyword=` props.
- Run `go test ./internal/config/ -run 'TestUI|TestAllFlat'` to verify after any config or UI change.

### Concurrency Architecture (Decided)

The architecture establishes specific concurrency patterns. Follow them:

- **Queue → Downloader signaling**: channel-based (`chan struct{}`, cap=1, non-blocking send). NOT `sync.Cond`. Rationale in `docs/ARCHITECTURE.md` § Coordination Architecture.
- **Queue internal locking**: `sync.RWMutex`. The hot path (`GetArticles`) takes RLock; mutations take full Lock.
- **Per-NzbObject locking**: `sync.Mutex` per object.
- **Article cache**: `sync.RWMutex` + `atomic.Int64` for memory tracking.
- **Downloader main loop**: `select{}` over multiple channels.

If a new component needs coordination, document the choice (mutex vs channel vs other) in a comment near its declaration.

### Persistence (Decided)

- **Queue state**: in-memory with event-triggered JSON+gzip persistence per NzbObject. NOT SQLite.
- **History**: SQLite via `modernc.org/sqlite` (pure Go, no CGO).
- **Config**: YAML via `gopkg.in/yaml.v3`.
- **Atomic writes**: all file persistence uses temp file + fsync + rename.

Rationale is documented in `docs/ARCHITECTURE.md`. Do not deviate without escalating.

## Library Selection

Prefer existing, well-maintained Go libraries over custom implementations. Before writing utility code, search for an existing solution.

When evaluating a new library:
- Check last commit date (active in last 12 months)
- Check open issues for concerning bugs
- Check that it has tests and reasonable test coverage
- Verify license compatibility (GPL-2.0+ for SABnzbd compatibility)
- Escalate the addition for user approval

## Testing Standards

- **Table-driven tests** with subtests (`t.Run`) for each case.
- **`-race` flag** required for tests involving goroutines or shared state.
- **Test files alongside source**: `foo.go` ↔ `foo_test.go`.
- **Test helpers** in `testhelper_test.go` or a `testdata/` package.
- **Integration tests** under `test/integration/` with `//go:build integration` tag.
- **Mocks/fakes** preferred over interface mocking frameworks. Hand-rolled fakes are clearer than `gomock`-generated ones for small interfaces.
- **Coverage target**: 80%+ for `internal/` packages. Don't chase coverage for trivial code paths.

### Coverage Exemptions

The `scripts/check_coverage` tool enforces an 80% per-function threshold on
changed code. Some functions are **trivially correct by inspection** and testing
them adds no confidence — e.g., no-op interface stubs, single-field getters, or
type-assertion wrappers. **Do not insert dead code** (like `_ = struct{}{}`)
to make the coverage tool instrument empty method bodies. That is coverage
gaming, not testing.

Instead, mark the function with a `//nocover:` comment on the `func` line
explaining *why* testing provides no value:

```go
func (d dummyEmitter) Broadcast(_ Event) {} //nocover: no-op interface stub
```

The coverage checker skips any function whose declaration line contains
`//nocover:`. The comment MUST include a reason after the colon. Functions
eligible for exemption:

- **No-op interface stubs** (empty method bodies satisfying an interface).
- **Trivial getters/setters** with no logic, branching, or side effects.
- **Compile-time interface checks** (`var _ Foo = (*Bar)(nil)`).

Functions NOT eligible — these must be tested:

- Anything with branching (`if`, `switch`, `for`).
- Anything with error handling or error wrapping.
- Anything that mutates shared state.

### Red-Green Discipline (write the failing test first)

**Every bug fix and every regression test MUST be proven to fail on the
unpatched code before the fix lands.** A test that already passes against the
buggy code does not test the fix — it is a change-detector that will silently
let the bug return. This has happened here repeatedly: tests named for a fix
that still passed with the fix reverted (a `"1.0 TiB"` case that never reached
the panicking branch; a `download.nzb` fallback case that never exercised the
fixed `/` path). A passing test is not evidence until you have seen it fail for
the right reason.

The required order for any fix:

1. **Write the test first**, encoding the *correct* expected behavior (not the
   current output — assert what the code *should* do, with an independent oracle
   where possible).
2. **Run it against the unfixed code and watch it FAIL.** For a pre-existing
   bug, write the test before touching the code. For a regression guard added
   alongside a fix, stash or revert the fix (e.g. the one-line change) and
   confirm the test goes red. Read the failure message — it must fail because of
   the bug, not a typo or wrong setup.
3. **Apply the fix**, confirm the test now passes, and confirm the rest of the
   suite stays green.

**The cheap pre-commit check for any `fix:` + `test:` pair:** mentally (or
actually) revert the fix and confirm the new test fails. If it still passes, the
test is exercising the wrong branch or input — fix the *test*, not just the
code. The fix and its test belong in the same change so this is verifiable.

**For de-flaking concurrency/timing tests**, the analogous proof is
`go test -race -count=N` (N ≥ 50, ideally also under `GOMAXPROCS=1`): a single
green run does not prove a flaky test is fixed, because a flaky test passes most
of the time by definition. Replace synchronization `time.Sleep` calls with a
deterministic signal (channel, `sync.WaitGroup`, or a poll-until-condition
helper); leave only genuine timing windows (mock latency, negative-observation
windows) and document each as intentional.

## Git Conventions

- **Branch**: **never commit directly to `main`. All work lands via pull request**, including single-commit fixes. This holds even though it is a solo private repo, for two concrete reasons: the PR is the review surface that CodeRabbit and human review comment on, and both `.github/workflows/ci.yml` and `.github/workflows/security.yml` already trigger on `pull_request` — pushing straight to `main` skips review entirely and runs CI only after the fact, when it is too late to be a gate.
- **This is a convention, not an enforced gate.** There is no GitHub branch protection configured for this repository, so nothing on the server will reject a direct push to `main`. It holds because we follow it. Do not read "the push succeeded" as "the push was allowed."
- **Never merge or close a PR without explicit user approval** — open it, report the CI result, and stop.
- **Worktrees**: for multi-step efforts, work in an isolated **git worktree** off `main`, then open a PR from that branch. Note a fresh worktree cannot build until you supply the UI bundle — `ui/dist/*` is gitignored, so `//go:embed all:dist` in `ui/embed.go` fails and `internal/web` reports `[setup failed]`. This is a worktree artifact, not a broken change:
  ```bash
  git worktree add /tmp/<lane> -b <lane>
  cp -r <main-checkout>/ui/dist /tmp/<lane>/ui/dist   # or build the UI
  ```
- **One step per commit** (or one logical sub-piece if a step is split).
- **Never** force-push, rewrite history, or `git reset --hard` without user approval.
- **Always** run quality gates before committing.

### Commit Convention

All commits must follow [Conventional Commits 1.0.0](https://www.conventionalcommits.org/)
(the global `~/.claude/CLAUDE.md` defines the full policy). Scope should be the
Go package name or subsystem: `fix(assembler)`, `refactor(queue)`, `feat(nntp)`.

```
<type>[optional scope]: <description>
```

| Type | When to use |
|------|-------------|
| `feat` | New user-visible capability |
| `fix` | Bug patch |
| `perf` | Performance improvement with benchmark evidence |
| `refactor` | Code restructuring, no behavior change |
| `test` | Adding or improving tests |
| `docs` | Documentation only |
| `chore` | Build, CI, dependency updates |

Append `!` or add `BREAKING CHANGE:` footer for any public API or wire-format change.

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

### Module-level `$state` in `.svelte.ts` files does not reliably trigger re-renders

**Problem**: Reactive state declared with `$state` in `.svelte.ts` module files (stores) does not reliably trigger template re-renders in consuming components when mutated inside `async` functions. Getter functions like `getConfig()` that return `$state` properties work for the initial read but miss subsequent updates. This was discovered when the SettingsDialog showed "Loading configuration..." indefinitely despite the fetch completing successfully.

**Rule**: For any component that fetches data and renders it conditionally (loading → error → data), declare `$state` variables **inside the component**, not in an external `.svelte.ts` store module. Use `.then()` chains rather than `async`/`await` for the fetch to ensure state mutations happen in a context Svelte can track.

**Pattern that works** (used in `SettingsDialog.svelte`):
```svelte
<script lang="ts">
  let data = $state(null);
  let loading = $state(false);

  $effect(() => {
    if (open && !data && !loading) {
      loading = true;
      fetch('/api/...')
        .then(res => res.json())
        .then(d => { data = d; })
        .finally(() => { loading = false; });
    }
  });
</script>
```

**Pattern that does NOT work**:
```typescript
// store.svelte.ts — mutations here don't trigger component re-renders
let data = $state(null);
export async function load() {
  data = await fetchJSON(...); // component won't see this change
}
```

> Note: this gotcha is about **module-level** `$state`. Class-field `$state`
> inside a store class (e.g. `BasePollStore`) works correctly.

### `bits-ui` `onOpenChange` vs `bind:open`

When a parent component controls a Dialog's open state via `bind:open`, the `onOpenChange` callback on `Dialog.Root` only fires when the dialog *itself* initiates a state change (clicking overlay/close). It does **not** fire when the parent sets the bound prop. Use a `$effect` watching the `open` prop instead.

### Child component updates

ConfigInput/ConfigSwitch and similar child components should receive an `onupdate` callback prop rather than importing store functions directly. This keeps the data flow explicit and avoids the module-level `$state` reactivity issue.

## Go Backend Lessons Learned

These rules are distilled from real bugs found across dozens of audit and hardening commits. **Every rule below was learned from a production-quality bug.** They must be followed for all new Go code.

### 1. Concurrency & Locking

- **Never hold a mutex during disk I/O or network calls.** Snapshot data under the lock (e.g., JSON-marshal), release the lock, then perform I/O. Holding `RLock` during `writeGzJSON` blocked the entire download pipeline for seconds. Pattern: `mu.RLock() → marshal → mu.RUnlock() → writeToDisk(marshaledBytes)`.

- **Always use `defer mu.Unlock()`.** Manual unlock-before-return in multiple branches has caused deadlocks and double-close panics. The only exception is snapshot-then-release (above), where unlock is intentional mid-function. In that case, add a `// --- No lock held below this line ---` comment.

- **Never `delete()` from a map while holding `RLock`.** `RLock` permits concurrent readers; mutation requires a full `Lock`. This caused a `concurrent map write` panic in the WebSocket broadcaster.

- **Every `select` on a channel or semaphore must also watch the relevant context/shutdown channel.** Goroutines blocked on semaphore acquisition without watching `c.ctx.Done()` blocked forever when the connection died. Pattern:
  ```go
  select {
  case sem <- struct{}{}:
  case <-ctx.Done():
      return ctx.Err()
  case <-shutdownCh:
      return ErrShutdown
  }
  ```

- **Don't expose mutable data to concurrent readers before it is fully initialized.** Calling `addHistory(job)` before `processJob(job)` exposed partially-initialized `StageLog` fields to API handlers reading the same struct.

- **Atomic flag ordering matters.** In `finishReader`, `closeErr` must be set *before* the `closed` atomic flag is flipped, otherwise concurrent readers see `closed=true` but read a nil error.

- **Use `sync.Once` or `CompareAndSwap` for idempotent stop/close.** Multiple stop paths (shutdown, error, cancel) can race. Using `closeOnce.Do(func(){...})` prevents double-close panics on channels and connections.

- **Guard `Start()`/`Stop()` state checks with a mutex, not bare reads.** `CancelJob` must check `started`/`stopped` under `mu.Lock` and track `inFlight` to prevent sending on a closed channel during `Stop()`.

- **Set state atomically with its observable effect.** `setBusyWithJob(true, ...)` must happen inside `popWithPause()`, not after return, to eliminate the window where `Empty()` returns true while a job is being processed.

### 2. File I/O & Persistence

- **All disk writes must be atomic: temp file → fsync → rename.** `os.WriteFile` truncates before writing; concurrent readers see partial/corrupt data. Use `os.CreateTemp` → write → `Sync()` → `Close()` → `os.Rename`. This pattern was missing in cache, queue, and dirscanner state — all required the same fix.

- **Use `os.CreateTemp` for unique temp files, never a hardcoded `.tmp` suffix.** Concurrent writes to `path + ".tmp"` corrupt state files. Dirscanner state had this bug.

- **Close the source file before `os.Remove` in cross-device move.** `defer in.Close()` runs after `os.Remove(src)`, which fails on some platforms because the file handle is still open.

- **On resume, count unfinished articles, not total articles.** `len(Articles)` includes already-downloaded parts that won't be re-dispatched, causing the assembler to hang waiting for parts that will never arrive.

- **Never delete an archive on partial extraction failure.** If only some files fail to extract from a ZIP/RAR, preserve the archive for retry or manual recovery.

- **Check directory containment before recursive delete.** `SortStage` deleted `FinalDir` when it was inside `origDir`. Always verify `!strings.HasPrefix(targetDir, sourceDir)` before removing a directory tree.

- **Path length limits are per-component (NAME_MAX = 255 bytes), not per-path.** This is Linux-only software; do not import Windows MAX_PATH heuristics. When sanitizing folder + filename pairs, make the folder name a function of the job alone — never derive folder truncation from the filename, or files in the same job will scatter across multiple directories.

### 3. Shutdown & Lifecycle Ordering

- **Shutdown order: stop producers → drain consumers → cancel context → wait → cleanup.** The correct order is: (1) Stop downloader (no new articles), (2) Stop assembler (drains in-flight writes, delivers completions), (3) Cancel context (watchCompletions exits), (4) Wait for goroutines, (5) Stop post-processor, flush cache, save queue. Getting this wrong drops file completion events.

- **Fallback goroutines spawned for channel delivery must watch `ctx.Done()`.** A `go func() { ch <- val }()` goroutine leaks forever if the receiver has exited. Always add a `case <-ctx.Done()` branch.

- **Don't penalize servers on `context.Canceled`.** Pause and shutdown cancel contexts, which is not a server error. Check `ctx.Err()` before calling `RecordBadConnection` or `ApplyPenalty`.

- **Clean up orphaned resources on startup.** Crash-orphaned temp files, stale lock files, and incomplete downloads accumulate across restarts. `Prune()` must clean these up.

### 4. HTTP API & Security

- **Extract `mode` and `apikey` from query params first, form body second.** For routing (`mode=`) and authentication (`apikey=`/`nzbkey=`), always check `r.URL.Query()` first. For POST requests, fall back to the form body using `formValue()` (which respects `MaxBytesReader`). This supports third-party apps (Sonarr, Radarr, NZB360) that send parameters as form fields. Never use `r.FormValue()` directly in routing/auth — it triggers implicit `ParseMultipartForm` with Go's default 32MiB limit.

- **Always apply `http.MaxBytesReader` in middleware, not in individual handlers.** Create the `statusWriter` before `MaxBytesReader` so 413 responses are logged correctly. Use `maxUploadBytes` for `multipart/form-data`, `maxFormBytes` for everything else.

- **CSRF protection requires *both* `Origin` and `Sec-Fetch-Site` checks.** Cross-origin GET requests (via `<img>` or `<form method=GET>`) don't send an `Origin` header. Modern browsers send `Sec-Fetch-Site` instead. Block requests with `Sec-Fetch-Site: cross-site` or `cross-origin`.

- **Cookie-based auth on local-network services needs Referer/Origin validation.** Even `localhost` APIs are vulnerable to CSRF if the browser sends cookies automatically.

- **Cap all query `limit` parameters.** `limit=0` or `limit=999999999` loads unbounded data into memory. Enforce `defaultLimit` and `maxLimit` constants on all list/search endpoints.

- **Never use `os.ExpandEnv` on raw config file bytes.** It leaks host environment variables into config values. Expand only explicitly marked fields.

### 5. Resource Management

- **Track and close file descriptors for cancelled jobs.** The assembler holds open file handles per job. When a job is cancelled, `CancelJob` must close all associated FDs via a control message to the worker goroutine, or FDs leak indefinitely.

- **Use tombstone sets to reject late/duplicate messages.** After a file is completed and closed, late duplicate articles can re-open it, leaking FDs. Maintain a `completedFiles` set to reject them.

- **Add idle read deadlines on long-lived network sockets.** NNTP connections without read deadlines hang silently when the remote end disappears. Use `SetReadDeadline` and reset on each successful read.

- **SQLite per-connection pragmas belong in the DSN, not in post-connect hooks.** `journal_mode=WAL` and `busy_timeout` set via `_pragma=` in the DSN ensure every connection (including pool-created ones) has them from the start.

- **Batch large deletions to avoid unbounded transactions.** Deleting thousands of history records in a single `DELETE ... WHERE id IN (...)` can lock the database. Use chunked deletes with a reasonable batch size.

### 6. Code Complexity & Hotspot Refactoring

These rules are established from real hotspots targeted by repowise. They ensure cyclomatic complexity remains low, allowing standard linter checks and manual reviews to succeed easily.

- **Simplify Multi-Strategy Fallbacks**: When a method implements multiple fallback, validation, or conditional path strategies (like CSRF `isCrossOrigin` or complex auth logic), extract each strategy into its own focused helper (e.g. `isRefererCrossOrigin`). This drops the parent method's cyclomatic complexity (CCN) and enables targeted, isolated testing.

- **Consolidate Subsystem Boilerplates**: Avoid duplicating decoder setups, channel progress monitoring goroutines, and panic recovery setups across adjacent methods (like `GoVerify` and `GoRepair`). Consolidate these into unified helper methods (e.g. `newDecoderForDir`, `monitorProgress`). This ensures setup bug-fixes propagate globally.

- **Isolate Parsing & Normalization**: Keep primary decoding handlers (like config `decode`) concise. Extract error-type partitioning loops (like parsing `yaml.TypeError`) and struct normalizations (like assigning defaults or converting nil slices) into dedicated helpers.

- **Measure the result; preserve behavior exactly**: After a complexity-reduction extraction, run `gocyclo`/`gocognit` on the function and use the *measured* number — never an estimate — in any commit claim. Confirm the extraction is behavior-preserving: when hoisting shared statements out of sibling branches (e.g. `ParError = true`), verify every branch set them; when converting fall-through into return values, re-run `golangci-lint` (it may now flag `S1008`/`ifElseChain` that the original control flow hid).

### 7. Performance & Hot-Path Discipline

These rules were learned from production pprof profiling at 2 Gbps. The download pipeline processes ~330 articles/second; any per-article overhead multiplies fast.

#### Dispatch Loop (`internal/downloader/dispatch.go`)

- **Never iterate all articles to find pending work.** `ForEachUnfinishedArticle` uses `Pending` counters on `JobFile` and `PendingArticles` on `Job` to skip completed files/jobs in O(1). Any new code that walks articles must respect these counters — do not introduce new linear scans over the article slice.

- **Maintain pending counters on every state mutation.** When changing `art.Done`, `art.Emitted`, or `art.Failed`, you **must** update `job.Files[art.FileIdx].Pending` and `job.PendingArticles`. The pattern: decrement when an article leaves the pending state (Emitted, Done, or Failed for the first time); increment when it returns (ClearArticleEmitted). If a bulk operation makes incremental tracking fragile, call `job.recomputePending()` instead. See `MarkArticleEmitted`, `MarkArticlesDone`, `ClearAllEmitted` for canonical examples.

- **Cache per-server data once per dispatch pass, not per article.** `srv.Cfg()` returns a by-value struct copy. Calling it per-article per-server cost 0.69s in production profiles. The `serverCfgs []config.ServerConfig` slice in `dispatchPass` caches these. Any new per-server state queries (e.g., `Active()`, penalty checks) should follow the same pattern: snapshot once, pass the slice to `tryDispatch`.

- **Use 2-case selects (send/default), not 3-case (send/default/ctx.Done).** `runtime.selectgo` is significantly cheaper with 2 cases. Check `ctx.Err()` once before the server loop instead.

- **Defer heap allocations past early-exit checks.** `articleRequest` is allocated only after confirming the article is not already in-flight. Moving the alloc before the `inFlight` check wasted 1.9s/10s on objects immediately discarded.

#### Decoder (`internal/decoder/decoder.go`)

- **Use the LUT for `indexSpecial`, not `bytes.IndexAny`.** The 256-byte lookup table `specialLUT` identifies CR, LF, and `=` bytes in O(1) per byte. `bytes.IndexAny` performed O(N×M) string scanning and was the #1 decoder bottleneck. Do not replace the LUT with standard library functions.

- **`sub42Span` fuses copy + subtract into one pass.** The yEnc subtract-42 operation and the output append are combined into a single unrolled scalar loop for L1 cache efficiency. Do not split this back into `copy` + loop, and do not add bounds checks inside the inner loop (the capacity pre-check at the top ensures safety).

- **The LUT must be a compile-time constant array, not built in `init()`.** `init()` functions are forbidden by project convention, and the LUT values are known at compile time.

#### NNTP I/O (`internal/nntp/io.go`)

- **Pre-size `readDotStuffedBody`'s buffer to 768 KB.** Without this, `bytes.Buffer` grows incrementally, causing `memclrNoHeapPointers` (4.1%) and `memmove` (2.6%) to dominate the profile. The 768 KB value matches a typical yEnc article (~750 KB payload).

#### Queue (`internal/queue/`)

- **Use `job.articleByID()` for O(1) lookups, never linear scans.** The `artIdx` map is built lazily on first access. All queue mutation methods (`MarkArticlesDone`, `MarkArticleFailed`, `MarkArticleEmitted`, etc.) must use this, not nested `for fi / for ai` loops.

- **`JobArticle.FileIdx` is a back-pointer set by `recomputePending` / `buildArtIndex`.** It allows mutation methods to update per-file `Pending` without scanning for the parent file. This field is `json:"-"` (not persisted) — it must be recomputed on load.

- **All transient fields (`Pending`, `PendingArticles`, `FileIdx`, `artIdx`, `Emitted`) are `json:"-"`.** They are recomputed by `recomputePending()` on load and `ClearAllEmitted`. If you add new transient state, follow this pattern and ensure it is initialized in both `Add` and `Load`.

- **`ClearAllEmitted` is the self-healing reset.** It calls `recomputePending()` to rebuild all counters from ground truth. If you suspect counter drift during development, calling `recomputePending()` on a job will correct it. The `pending_test.go` `verifyPending` helper validates counters against ground truth.

#### General Performance Rules

- **Profile before optimizing.** Use `go tool pprof` with production workloads. Synthetic benchmarks miss real bottlenecks (e.g., `selectgo` overhead only appears under multi-server dispatch contention).

- **String map keys for message-IDs are expensive.** NNTP message-IDs are long strings (40-80 bytes); `aeshashbody` for these keys costs 1.15s/10s at 2 Gbps. Avoid adding new `map[string]` lookups in the per-article hot path. If you must, consider integer keys or pre-hashed values.

- **`sync.Pool` is usually not worth it in this codebase.** The `articleRequest` allocation (0.3s at steady-state) is small enough that pool overhead (Put/Get synchronization) would offset the savings. Only pool objects that are large and allocated at >10K/sec.
