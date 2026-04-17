# Phase 13: Svelte 5 SPA — Glitter Replacement

## Decision Record

Decided 2026-04-16 with user:

| Choice | Decision | Rationale |
|---|---|---|
| Architecture | SPA + Vite, embedded via `//go:embed` | SABnzbd UI needs real-time polling, drag-drop, complex modals — past htmx comfort zone |
| Framework | **Svelte 5** | Simplest mental model for a frontend-inexperienced Go developer. `.svelte` files are enhanced HTML. Reactive `$state()` runes, no virtual DOM, no JSX. |
| Language | **TypeScript** | Familiar type discipline for a Go developer; catches API contract bugs at build time. Svelte 5 has first-class TS support. |
| CSS | Tailwind CSS 4 | Required by shadcn-svelte; utility classes are simpler than writing CSS files |
| Components | shadcn-svelte | Production-ready (7.5K+ stars), Svelte 5 compatible, provides tables, progress bars, modals, tabs, badges, dropdowns |
| Build tool | Vite | Universal default, no alternatives worth considering |
| Drag-and-drop | sveltednd | Svelte 5 native, TypeScript, runes-based |
| Data fetching | Start with `fetch()` + polling; upgrade to `@tanstack/svelte-query` if caching/dedup needed |

## Architecture Overview

```
┌──────────────────────────────────────┐
│  Go binary (single process)          │
│                                      │
│  /api?mode=...  → api.Server (JSON)  │
│  /              → SPA index.html     │
│  /assets/...    → Vite hashed assets │
│  /staticcfg/... → favicons/icons     │
└──────────────────────────────────────┘

Development:
  Vite dev server (:5173) → proxies /api to Go (:8080)
  Hot module reload, instant feedback

Production:
  `npm run build` → ui/dist/
  go:embed ui/dist → served by Go's http.FileServer
  Single binary, zero Node.js runtime dependency
```

## Directory Layout

```
ui/                          ← NEW: Svelte SPA root (Vite project)
├── package.json
├── vite.config.ts
├── tsconfig.json
├── svelte.config.js
├── tailwind.config.ts
├── src/
│   ├── app.html              ← HTML shell (Vite entry)
│   ├── app.css               ← Tailwind imports
│   ├── lib/
│   │   ├── api.ts            ← typed fetch() wrappers for /api?mode=...
│   │   ├── types.ts          ← TypeScript types mirroring Go API structs
│   │   ├── stores/
│   │   │   ├── queue.ts      ← reactive queue state + polling
│   │   │   ├── history.ts    ← reactive history state + polling
│   │   │   └── status.ts     ← speed, paused state, warnings
│   │   └── components/
│   │       ├── ui/           ← shadcn-svelte components (auto-added by CLI)
│   │       ├── QueueTable.svelte
│   │       ├── QueueRow.svelte
│   │       ├── HistoryTable.svelte
│   │       ├── HistoryRow.svelte
│   │       ├── SpeedGraph.svelte
│   │       ├── StatusBar.svelte
│   │       ├── AddNzbDialog.svelte
│   │       ├── SettingsDialog.svelte
│   │       └── Navbar.svelte
│   └── routes/               ← SvelteKit file-based routing (or single App.svelte)
│       └── +page.svelte      ← main page
├── static/                   ← static assets (favicon copied here)
│   └── favicon.ico
└── dist/                     ← Vite build output (gitignored, embedded by Go)

internal/web/                 ← MODIFIED: serves SPA instead of templates
├── server.go                 ← simplified: embed dist/, serve index.html for all routes
└── server_test.go
```

## API Surface the SPA Must Cover

These are the `/api?mode=...` endpoints the Glitter UI calls. All return JSON.
The Go implementations already exist; the SPA just needs typed fetch wrappers.

### Core polling (called every ~2 seconds)

| Mode | Sub-action | Returns | Used for |
|---|---|---|---|
| `queue` | `name=list` | `{ queue: { paused, slots: [...], noofslots, ... } }` | Queue tab, progress bars, speed |
| `history` | `name=list` | `{ history: { slots: [...], noofslots, ... } }` | History tab |
| `warnings` | — | `{ warnings: [...] }` | Warning badge + messages panel |

### User actions

| Mode | Purpose | SPA trigger |
|---|---|---|
| `pause` / `resume` | Global pause/resume | Toolbar button |
| `addfile` | Upload NZB (multipart POST) | "Add NZB" dialog |
| `addurl` | Fetch NZB from URL | "Add NZB" dialog URL tab |
| `queue` + `name=delete` | Remove queue item | Queue row action |
| `queue` + `name=pause`/`resume` | Pause/resume single job | Queue row action |
| `queue` + `name=priority` | Change job priority | Queue row dropdown |
| `queue` + `name=sort` | Sort queue | Column header click |
| `history` + `name=delete` | Remove history item | History row action |
| `status` | Dashboard stats | Settings/status dialog |
| `get_config` / `set_config` | Read/write config | Settings dialog |
| `get_cats` | List categories | Category dropdowns |
| `get_scripts` | List post-proc scripts | Script dropdowns |
| `config` + `name=speedlimit` | Set speed limit | Speed input in navbar |
| `watched_now` | Trigger dir scan | Menu action |
| `rss_now` | Trigger RSS scan | Menu action |
| `shutdown` / `restart` | Server lifecycle | Menu actions |

### Stubbed (not yet implemented in Go backend)

These return 400 "not implemented" — the SPA should show them as disabled:
`rename`, `change_complete_action`, `change_name`, `change_cat`,
`change_script`, `change_opts`, `switch` (server priority swap),
`get_files` (per-NZB file listing), `retry`, `retry_all`,
`move_nzf_bulk`, `cancel_pp`, `restart_repair`.

## Implementation Steps

### Phase 13.0: Project scaffold [sonnet]

**Deliverables:**
1. Initialize Vite + Svelte 5 + TypeScript project in `ui/`
2. Install Tailwind CSS 4 + shadcn-svelte
3. Configure `vite.config.ts` with proxy to `localhost:8080` for `/api`
4. Create minimal `App.svelte` that renders "SABnzbd-Go" heading
5. Add `ui/dist/` to `.gitignore`
6. Test: `npm run build` produces `dist/index.html`

**Validation:**
- `npm run dev` starts Vite on :5173
- `npm run build` succeeds with zero errors
- `npx svelte-check` passes (TypeScript)

### Phase 13.1: Go embed integration [haiku]

**Deliverables:**
1. Modify `internal/web/server.go`:
   - In production: `//go:embed` the `ui/dist` directory, serve `index.html` for all non-API routes (SPA catch-all)
   - In development: reverse-proxy to Vite dev server (or just tell users to use Vite directly)
2. Remove or gate old template-rendering code behind a build tag `//go:build legacy_ui`
3. Update `composeRouter` in `cmd/sabnzbd/main.go` if needed
4. Keep `/staticcfg/` route for favicons (or move into the SPA's `static/`)

**Validation:**
- `go build ./cmd/sabnzbd` succeeds
- Running the binary serves the SPA at `/`
- `/api?mode=version` still works
- `go test ./internal/web/...` passes

### Phase 13.2: API client + TypeScript types [haiku]

**Deliverables:**
1. `ui/src/lib/types.ts` — TypeScript interfaces matching Go API response shapes:
   - `QueueResponse`, `QueueSlot`, `HistoryResponse`, `HistorySlot`, `WarningsResponse`
2. `ui/src/lib/api.ts` — typed fetch wrappers:
   - `fetchQueue(apiKey, start, limit): Promise<QueueResponse>`
   - `fetchHistory(apiKey, start, limit): Promise<HistoryResponse>`
   - `fetchWarnings(apiKey): Promise<WarningsResponse>`
   - `postAction(apiKey, mode, params): Promise<StatusResponse>`
   - `uploadNzb(apiKey, file): Promise<StatusResponse>`
3. API key stored in a Svelte `$state` rune (prompted on first load if not set)

**Validation:**
- `npx svelte-check` passes
- Manual: open browser, see API key prompt, enter key, see console log of queue fetch

### Phase 13.3: Layout shell + Navbar [sonnet]

**Deliverables:**
1. `Navbar.svelte` — app bar with:
   - SABnzbd logo/title
   - Speed display (placeholder)
   - Pause/Resume button (wired to `POST /api?mode=pause`/`resume`)
   - Speed limit input
   - "Add NZB" button (opens dialog — dialog itself is a later step)
   - Tab switcher: Queue / History / Warnings
2. Main layout with tab content area
3. Use shadcn-svelte `Button`, `Input`, `Tabs`, `Badge` components

**Validation:**
- Tabs switch between placeholder panels
- Pause/Resume button calls API and toggles state
- Responsive: usable at mobile width

### Phase 13.4: Queue table [sonnet]

**Deliverables:**
1. `QueueTable.svelte` — table of queue items with columns:
   - Name, Size, Progress (%), Status, Speed, ETA, Category, Priority, Actions
2. `QueueRow.svelte` — single row with:
   - Progress bar (shadcn `Progress` component)
   - Pause/Resume/Delete action buttons
3. Polling store: `queue.ts` — fetches `/api?mode=queue` every 2 seconds, exposes reactive `$state`
4. Pagination (use API's `start`/`limit` params)

**Validation:**
- Queue items appear with real data from Go backend
- Progress bars update on each poll
- Pause/Resume/Delete actions work and UI reflects change on next poll
- Empty state shown when queue is empty

### Phase 13.5: History table [haiku]

**Deliverables:**
1. `HistoryTable.svelte` + `HistoryRow.svelte` — mirrors queue pattern:
   - Name, Size, Status (Completed/Failed), Category, Completed time, Actions
2. Delete action, "Purge" button
3. Polling store: `history.ts`
4. Pagination

**Validation:**
- History items display with correct status badges
- Delete and purge actions work
- Pagination controls navigate pages

### Phase 13.6: Status bar + speed display [haiku]

**Deliverables:**
1. `StatusBar.svelte` — bottom or top bar showing:
   - Current download speed (from queue response)
   - Total remaining size
   - ETA for full queue
   - Free disk space (from `status` mode)
2. `SpeedGraph.svelte` — simple sparkline or area chart of recent speed readings
   - Use lightweight SVG — no charting library needed for a simple line
   - Keep a rolling buffer of last 60 readings (2 minutes at 2s interval)

**Validation:**
- Speed updates in real-time as queue downloads
- Graph shows recent speed history
- Values match what `curl /api?mode=queue` returns

### Phase 13.7: Add NZB dialog [sonnet]

**Deliverables:**
1. `AddNzbDialog.svelte` with two tabs:
   - **File upload**: drag-and-drop or file picker, calls `mode=addfile` (multipart POST)
   - **URL**: text input, calls `mode=addurl`
2. Category dropdown (populated from `mode=get_cats`)
3. Priority dropdown
4. Post-processing dropdown
5. Use shadcn-svelte `Dialog`, `Tabs`, `Select`, `Input` components

**Validation:**
- Upload an NZB file → appears in queue
- Paste a URL → item appears in queue
- Category/priority selection works

### Phase 13.8: Warnings panel + toast notifications [haiku]

**Deliverables:**
1. Warnings tab content: list of warnings from `mode=warnings`
2. Badge on tab showing warning count
3. Toast notification when new warning arrives (compare poll results)
4. "Clear" button to dismiss warnings

**Validation:**
- Warnings display in a scrollable list
- Badge count matches warning count
- New warnings trigger a toast

### Phase 13.9: Drag-and-drop queue reordering [sonnet]

**Deliverables:**
1. Integrate `sveltednd` into `QueueTable.svelte`
2. On drop: call `mode=switch` or `mode=queue&name=priority` to persist new order
3. Optimistic update: move row immediately, revert on API error

**Validation:**
- Drag a queue row to a new position
- Order persists on next poll
- Works on mobile (touch events)

### Phase 13.10: Settings dialog (read-only to start) [haiku]

**Deliverables:**
1. `SettingsDialog.svelte` — modal showing current config from `mode=get_config`
2. Display sections: General, Servers, Categories, Scheduling
3. Read-only for v1; editing requires wiring `mode=set_config` (later)

**Validation:**
- Dialog opens, config values display correctly
- No crashes on missing/optional fields

### Phase 13.11: Polish + smoke test [sonnet]

**Deliverables:**
1. Loading skeletons during initial fetch
2. Error boundary — show "API unreachable" banner if polling fails
3. Dark mode via Tailwind (honors OS preference)
4. Page title updates with queue status: "▶ 3 items | SABnzbd-Go"
5. Keyboard shortcuts: Space = pause/resume, A = add NZB
6. Update `docs/ui_smoke_checklist.md` for the new SPA
7. Update `README.md` Quickstart

**Validation:**
- Full browser walkthrough per updated smoke checklist
- `npm run build` produces a < 500KB gzipped bundle
- `npx svelte-check` passes
- All Go tests pass
- `go build` produces a working single binary with embedded SPA

## Model Dispatch Guide

| Step | Model | Rationale |
|---|---|---|
| 13.0 Scaffold | sonnet | Project setup decisions, config tuning |
| 13.1 Go embed | haiku | Mechanical embed wiring, small Go changes |
| 13.2 API types | haiku | Mechanical translation of Go structs to TS |
| 13.3 Layout | sonnet | Design decisions: layout, component selection, UX |
| 13.4 Queue | sonnet | Most complex view — polling, progress, actions |
| 13.5 History | haiku | Mirrors queue pattern, less complex |
| 13.6 Status bar | haiku | Small component, straightforward data binding |
| 13.7 Add NZB | sonnet | File upload, multipart POST, multiple tabs |
| 13.8 Warnings | haiku | Simple list + badge, follows established patterns |
| 13.9 Drag-drop | sonnet | Third-party lib integration, optimistic updates |
| 13.10 Settings | haiku | Read-only display of API data |
| 13.11 Polish | sonnet | UX judgment calls, cross-cutting concerns |

## Build Integration

### Development workflow

```bash
# Terminal 1: Go backend
go run ./cmd/sabnzbd --config ~/.config/sabnzbd-go/sabnzbd.yaml --serve

# Terminal 2: Vite dev server (proxies /api to Go)
cd ui && npm run dev
# Open http://localhost:5173
```

### Production build

```bash
cd ui && npm run build        # → ui/dist/
cd .. && go build ./cmd/sabnzbd  # embeds ui/dist/ into binary
./sabnzbd --serve             # serves SPA + API on :8080
```

### CI quality gates

```bash
# Frontend
cd ui && npm run build && npx svelte-check

# Backend (unchanged)
go vet ./...
go test -race -count=1 ./...
golangci-lint run ./...
```

## What Happens to Phase 12 Template Work

The Phase 12 Cheetah-to-Go-template port (Steps 12.1–12.11) stays in the repo
behind a `//go:build legacy_ui` tag. It served its purpose:

1. Proved the template structure and API wiring work end-to-end
2. Identified the translation catalog gap (Step 12.10) and the JS assembly
   gap (Step 12.11) — both of which would have bitten the SPA too
3. Provides a functional (if rough) fallback if someone needs it

The SPA replaces it as the default UI. The old `internal/web/templates/`,
`internal/i18n/english.go`, and assembled `glitter.js` become dead code
once the SPA is wired. They can be removed in a cleanup step after the
SPA is validated.

## Dependencies (npm packages)

| Package | Purpose | Size |
|---|---|---|
| `svelte` | Framework | ~15KB runtime |
| `@sveltejs/vite-plugin-svelte` | Vite integration | build-only |
| `typescript` | Type checking | build-only |
| `tailwindcss` | Utility CSS | build-only (purged) |
| `bits-ui` | Headless components (shadcn-svelte dep) | ~20KB |
| `sveltednd` | Drag-and-drop | ~5KB |
| `clsx` + `tailwind-merge` | Class utilities (shadcn dep) | ~3KB |

Expected production bundle: **< 200KB** gzipped (vs ~300KB+ for the legacy jQuery+Knockout stack).

## Open Questions (to resolve during implementation)

1. **SvelteKit vs plain Svelte + Vite?** SvelteKit adds file-based routing and
   SSR, but we don't need SSR (Go serves the API). Plain Svelte + Vite with
   a single `App.svelte` is simpler. Recommendation: start with plain Svelte;
   upgrade to SvelteKit only if routing gets complex.

2. **API key storage**: localStorage? URL parameter? Cookie? Upstream uses
   a URL parameter (`apikey=...`). Recommendation: localStorage with a
   first-run prompt, matching the upstream pattern of "enter your API key."

3. **WebSocket vs polling**: The current API is polling-based (Glitter polls
   every 2 seconds). WebSocket would be more efficient but requires new Go
   code. Recommendation: start with polling (matches existing API), add
   WebSocket as a future optimization.
