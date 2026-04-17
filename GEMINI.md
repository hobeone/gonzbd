# GEMINI.md - Project Context & Instructions

This file provides foundational context and instructional mandates for the `sabnzbd-go` project. It must be read and followed by any AI agent working on this codebase.

## Project Overview
`sabnzbd-go` is a high-performance Go reimplementation of [SABnzbd](https://sabnzbd.org), the automated Usenet binary newsreader. It targets fresh installations and is not a drop-in replacement for the Python version.

- **Status:** Core backend download pipeline and legacy mode-dispatch API (`/api?mode=...`) are functional. The Glitter web UI port (Phase 12) is the current active focus.
- **Main Technologies:**
    - **Language:** Go 1.25+
    - **Configuration:** YAML (`gopkg.in/yaml.v3`)
    - **Persistence:** SQLite (`modernc.org/sqlite`, pure Go) for history; JSON+gzip for queue state.
    - **Logging:** Structured logging via `log/slog`.
    - **Concurrency:** Idiomatic goroutines + channels; `sync.RWMutex` for shared state.

## Architecture
- `cmd/sabnzbd/`: Entry point, flag parsing, and application orchestration.
- `internal/`: Core packages (API, app, downloader, queue, nzb, assembler, decoder, etc.).
- `docs/`: Critical design documents (`golang_implementation.md`, `sabnzbd_spec.md`, `implementation_notes.md`).
- `test/`: Integration tests, fixtures, and a mock NNTP server.

## Building and Running
- **Build:** `go build ./cmd/sabnzbd`
- **Run (Daemon):** `./sabnzbd --config ~/.config/sabnzbd-go/sabnzbd.yaml --serve`
- **One-shot Download:** `./sabnzbd --config <path> --nzb <path>`
- **Test (Unit):** `go test ./...`
- **Test (Race):** `go test -race ./...` (Required for CI/commits)
- **Test (Integration):** `go test -tags=integration ./test/integration/...`
- **Lint:** `go vet ./...` and `golangci-lint run ./...`

## Development Mandates

### 1. Authoritative Documentation (Order of Precedence)
1.  **`GEMINI.md`** (This file) - Foundational mandates and project overview. Read this first for every session.
2.  **`CLAUDE.md`** - Strict development protocols, quality gates, and the mandatory "Decision Needed" escalation format.
3.  **`docs/implementation_notes.md`** - Technical gotchas, architecture patterns (e.g., adapters in `cmd/sabnzbd`), and testing norms. **Read this for architectural context.**
4.  **`docs/golang_implementation.md`** - The project roadmap. Contains the detailed phase-by-step implementation plan and model recommendations. **Consult this to identify the current/next task.**
5.  **`docs/sabnzbd_spec.md`** - The source of truth for functional behavior. Defines protocols (NNTP), data formats (NZB, persistence), and API endpoint schemas. **Refer here for behavioral truth.**
6.  **`docs/nzb_processing_lifecycle.md`** - A high-level, code-level overview of the NZB download and post-processing flow. **Read this to understand how data moves through the system.**
7.  **`../sabnzbd/`** - The original Python implementation (external to this repo). Use for intent clarification, but do not transliterate.

### 2. Coding Standards
- **Idioms:** "Accept interfaces, return structs." Define interfaces at the consumer side.
- **Context:** Every blocking operation **must** accept `context.Context` as the first parameter.
- **Logging:** Pass `*slog.Logger` via constructors. **Never** use a package-level global logger.
- **Errors:** Wrap errors with `fmt.Errorf("...: %w", err)`. Never use `%v` for errors.
- **Concurrency:** Prefer channels for signaling (e.g., `chan struct{}`) over `sync.Cond`. Use `sync.RWMutex` for hot-path memory state.
- **No hacks:** No `init()` functions for setup, no `panic` for control flow, and no `time.Sleep` in tests for synchronization.

### 3. Workflow & Quality Gates
- **Per-Step Commits:** Implement one step from `docs/golang_implementation.md` at a time.
- **Verification:** Before every commit, you **must** pass:
    ```bash
    go vet ./...
    go test -race ./...
    golangci-lint run ./...
    ```
- **Ambiguity Protocol:** If the spec or plan is unclear, investigate the Python source (`../sabnzbd/`), form an opinion, and present it to the user using the "Decision Needed" format defined in `CLAUDE.md`.

## Key File Locations
- **API Handlers:** `internal/api/`
- **Download Engine:** `internal/downloader/`
- **Queue Logic:** `internal/queue/`
- **Web UI (Svelte SPA):** `ui/` — Svelte 5 + TypeScript + Vite, embedded via `//go:embed all:dist` in `ui/embed.go`
- **SPA Handler:** `internal/web/` — serves embedded dist with SPA catch-all fallback to index.html
- **Configuration Schema:** `internal/config/`

## Svelte 5 UI Gotchas

These are hard-won lessons that **must** be followed when editing the Svelte SPA in `ui/`:

1. **Do not use module-level `$state` in `.svelte.ts` stores for data that drives conditional rendering.** Mutations inside async functions in external store modules do not reliably trigger re-renders in consuming components. Instead, declare `$state` inside the component and use `.then()` chains for fetches. See `SettingsDialog.svelte` for the working pattern.

2. **`bits-ui` Dialog `onOpenChange` does not fire when `bind:open` is set by the parent.** Use a `$effect` watching the `open` prop to trigger side effects (like data loading) when a dialog opens.

3. **Child components (ConfigInput, ConfigSwitch) receive `onupdate` callbacks** instead of importing store functions directly. This keeps data flow explicit and avoids the store reactivity issue.
