# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Context

This is a Go reimplementation of [SABnzbd](https://sabnzbd.org), the automated Usenet binary newsreader. The reference Python implementation lives at `../sabnzbd/`.

**Module path**: `github.com/hobeone/sabnzbd-go`  
**Go version**: 1.25 (toolchain 1.26.2)

## Reference Materials

Before writing any code, read these in order:

1. **`docs/implementation_notes.md`** — Cross-session knowledge. Conventions, architecture patterns, known deferrals, gotchas, and testing norms not obvious from the code. **Always read this first.**
2. **`docs/golang_implementation.md`** — The implementation plan. Lists all phases, steps, and which Claude model to use for each. Authoritative for what to build next.
3. **`docs/sabnzbd_spec.md`** — The functional specification. Defines all behaviors, data formats, API endpoints, constants, and protocols.
4. **`../sabnzbd/sabnzbd/`** — The Python source, external to this repo. Consult for clarification of intent when the spec is ambiguous, **but do not transliterate**. Translate intent into idiomatic Go. (The spec has been wrong before — see `implementation_notes.md` §5.3.)

## Implementation Workflow

### Per-Step Commit Cycle

Each step in `docs/golang_implementation.md` is a self-contained unit of work. The workflow is:

1. **Read the step** in `docs/golang_implementation.md` and any spec sections it references.
2. **Implement** the deliverables listed for that step.
3. **Verify** all quality gates pass (see below).
4. **Commit** with a message that references the step (e.g., `Step 0.2: constants package`).
5. **Move to the next step**.

Do not batch multiple steps into one commit. If a step is too large, split it — but each commit must leave the repository in a working state (`go build ./... && go test ./...` passes).

### Quality Gates (must pass before commit)

```bash
./scripts/run_tests.sh                # Must pass (full Go + UI suite)
go vet ./...                          # Must pass
golangci-lint run ./...               # Must pass (no new issues)
```

If any gate fails, fix the underlying issue. **Do not skip, suppress, or bypass these checks** to make a commit go through. If a lint rule genuinely needs to be disabled for a specific case, add a `//nolint:rulename // reason` comment explaining why.

### When You Get Stuck

If you cannot resolve a problem after a focused investigation:
- **Do not** try to work around the issue with a hack.
- **Do not** disable tests or skip checks.
- **Do** read the relevant Python code for clarity on intent.
- **Do** ask the user for direction with a specific proposal (see Decision Protocol below).

## Decision Protocol

When the spec or plan is ambiguous, or when an implementation choice will significantly affect later work:

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
- Adding new external dependencies (libraries) not already in the plan
- Changing public interfaces between packages
- Departing from the architecture in `docs/golang_implementation.md`
- Persistence format changes (file paths, schema, on-disk layout)
- API behavior changes that affect compatibility with the existing Glitter web UI

## Go Coding Standards

### Idioms (Required)

- **Accept interfaces, return structs**. Define interfaces at the consumer side, not the producer side.
- **Small interfaces**. Single-method interfaces are good. Compose with embedding when needed.
- **Context propagation**. Every blocking operation accepts `context.Context` as its first parameter.
- **Error wrapping**. Use `fmt.Errorf("operation failed: %w", err)` to preserve error chains. Never use `%v` on errors.
- **Structured logging**. Use `log/slog`. Pass `*slog.Logger` via constructor; do not use a package-level global logger.
- **Goroutine lifecycle**. Every goroutine has a clearly defined exit condition tied to a context, channel close, or explicit signal. No "fire and forget" goroutines.

### Anti-Patterns (Forbidden)

- **No `panic` for control flow.** Panic is for unrecoverable programmer errors only.
- **No silent error swallowing.** `_ = doSomething()` requires a comment explaining why the error is intentionally ignored.
- **No `time.Sleep` in tests** for synchronization. Use channels, `sync.WaitGroup`, or `chan struct{}` signals.
- **No `init()` functions** for non-trivial setup. Use explicit `New*` constructors called from `main`.
- **No global mutable state.** Configuration, loggers, and dependencies are passed explicitly.
- **No `interface{}` / `any`** in new code unless absolutely required (e.g., generic JSON handling). Prefer concrete types or generics. When a dynamic type is necessary, prefer `any` over `interface{}`.

### Concurrency Architecture (Decided)

The plan establishes specific concurrency patterns. Follow them:

- **Queue → Downloader signaling**: channel-based (`chan struct{}`, cap=1, non-blocking send). NOT `sync.Cond`. Rationale and details in `docs/golang_implementation.md` § Coordination Architecture.
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

Rationale is documented in `docs/golang_implementation.md`. Do not deviate without escalating.

## Library Selection

Prefer existing, well-maintained Go libraries over custom implementations. Before writing utility code, search for an existing solution. The plan lists vetted candidates in its "Existing Libraries to Use" table.

When evaluating a new library not in the plan:
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

## Build and Test Commands

No Makefile. Standard Go tooling only:

```bash
go build ./cmd/sabnzbd                # Build the binary
go test ./...                         # Run all unit tests
go test -race ./...                   # With race detector (use this for CI)
go test -run TestFoo ./internal/nzb/  # Run a single test
go test -bench=. ./internal/decoder/  # Run benchmarks
go test -tags=integration ./test/...  # Run integration tests
go vet ./...                          # Static analysis
golangci-lint run ./...               # Linting
```

## Git Conventions

- **Branch**: work directly on `main` for now (single developer); switch to feature branches when collaboration begins.
- **Commit messages**: `Step X.Y: <description>` for plan steps, `Fix: <description>` for bug fixes, `Refactor: <description>` for non-functional changes.
- **One step per commit** (or one logical sub-piece if a step is split).
- **Never** force-push, rewrite history, or `git reset --hard` without user approval.
- **Always** run quality gates before committing.

## Reading Python for Reference

When consulting the Python source for behavior clarification:

- Read for **intent and edge cases**, not for line-by-line translation.
- Python's threading model (single-threaded selector + threading.Lock) is **not** the Go model. Translate to goroutines + channels + RWMutex.
- Python's pickle persistence is **not** the Go model. Translate to JSON or SQLite as decided in the plan.
- Python's class hierarchies often translate to Go composition + interfaces. Don't reproduce inheritance.
- Variable naming should follow Go conventions (`MixedCaps`), not Python's `snake_case`.

When in doubt about whether a Python behavior is essential or accidental, ask.

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

### `bits-ui` `onOpenChange` vs `bind:open`

When a parent component controls a Dialog's open state via `bind:open`, the `onOpenChange` callback on `Dialog.Root` only fires when the dialog *itself* initiates a state change (clicking overlay/close). It does **not** fire when the parent sets the bound prop. Use a `$effect` watching the `open` prop instead.

### Child component updates

ConfigInput/ConfigSwitch and similar child components should receive an `onupdate` callback prop rather than importing store functions directly. This keeps the data flow explicit and avoids the module-level `$state` reactivity issue.
