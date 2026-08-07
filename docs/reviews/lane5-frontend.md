# Lane 5 — Frontend (ui/) Architecture Review

Scope: `ui/src/lib/stores/**`, `ui/src/lib/api.ts`, `ui/src/lib/types.ts`,
`ui/src/lib/components/**`, `ui/src/routes/**`, `ui/src/lib/{shortcuts,utils,favicon}*`,
build config (`vite.config.ts`, `tsconfig.json`, `package.json`).

## State architecture map

There is **no interval-based polling anywhere in the app** despite the naming
(`base-poll.svelte.ts`, `isPolling`, `startPolling`). The actual model is:

1. **`websocket.svelte.ts`** owns a single reference-counted `WebSocket` to
   `/api/ws`. `subscribeWS(handler)` increments a handler set and lazily opens
   the socket; the last unsubscribe closes it. On close it calls
   `reportDisconnect()` on the connection store and otherwise does nothing —
   it never reconnects itself.
2. **`connection.svelte.ts`** is the sole owner of reconnection: it tracks
   consecutive HTTP failures (threshold 2) or an authoritative WS-close signal,
   then runs an exponential-backoff HTTP probe (`/api?mode=version`) and fires
   `onReconnected` callbacks when the probe succeeds.
3. **`base-poll.svelte.ts`** (`BasePollStore`, extended by queue/history) does
   exactly three things on `start()`: (a) one `poll()` (HTTP GET) to prime
   state, (b) `subscribeWS` so a matching WS event triggers a re-`poll()`, (c)
   `onReconnected` so a restored connection triggers a re-`poll()`. There is no
   timer. `warnings.svelte.ts` duplicates this pattern by hand instead of
   extending `BasePollStore` (it has its own `start/stop` doing the same three
   things — see Finding 8).
4. **`telemetry.svelte.ts`** is push-only: it never does an HTTP fetch: it
   only mutates state from `metrics` WS events.
5. Components read stores via **getter functions** (`getQueue()`,
   `getSpeedBytesPerSec()`, …) called directly in templates/`$derived`, which
   is how Svelte 5 fine-grained reactivity tracks the underlying `$state`
   fields inside the store classes (this works because the `$state` is a
   **class field**, not module-level — see Positive findings).
6. Actions (`pauseJob`, `deleteJob`, `setConfig`, …) go through
   `api.ts` → `postAction`/`setConfig`, then the mutating call `await`s and
   calls `poll()` itself; separately, the backend also broadcasts a
   `queue_updated`/`history_updated` WS event that redundantly triggers the
   same store's `poll()` a second time (see Finding 1).

So: WebSocket is the live-update backbone; "polling" is better described as
"fetch-on-mount + fetch-on-notify" with an HTTP GET fallback path if WS drops
(via `onReconnected`). It is a coherent design once you see it, but see the
verdict section for the naming/vestigial-code cost.

## Findings

### 1. Actions double-poll: explicit post-action poll + WS-triggered poll race
`ui/src/lib/stores/queue.svelte.ts:85-102` — CONFIRMED. `pauseJob`/`resumeJob`/`deleteJob`
each `await postAction(...)` then `await this.poll()`. The backend (per
`docs/ARCHITECTURE.md` "WebSockets... real-time state updates... broadcaster
pattern") also emits `queue_updated` after the same mutation, which
`handleWSEvent` (line 62-63) turns into a second `this.poll()`. `poll()` does
guard against overlapping in-flight calls via `#pollInFlight`/`#pollDirty`
(lines 11-13, 22-48), so this is not a crash risk, but it is two HTTP
round-trips per user action instead of one, and the dirty-flag re-poll after
the WS-driven one lands means occasionally three. For a single-user app this
is wasted round-trips, not correctness — low-impact, but exactly the kind of
"two subsystems feeding the same store" complexity the review brief called
out. Effort to simplify: S (drop the explicit `poll()` after action methods
and rely solely on the WS-broadcast repoll — but only safe if the backend
*always* emits the event synchronously before the HTTP response returns, which
this lane can't verify — flag for backend lane).

### 2. `fetchJSON` returns a promise that never resolves or rejects on auth-expiry — can permanently wedge a store's in-flight guard
`ui/src/lib/api.ts:54-58` — CONFIRMED (traced, not runtime-reproduced).
```ts
export async function fetchJSON<T>(url: string): Promise<T> {
	const res = await fetch(url);
	if (checkRedirect(res, url)) {
		return new Promise(() => {}); // Never resolve or reject to suppress UI error states/toasts
	}
	...
```
`QueueStore.poll()` (`queue.svelte.ts:22-49`) sets `#pollInFlight = true`
before `await fetchQueue(...)` and only clears it in a `finally`. If
`fetchJSON` returns a promise that never settles, that `finally` never runs,
so `#pollInFlight` stays `true` forever — every subsequent `poll()` call
(including ones triggered by `setPage`/`setSearch`/WS events) becomes a no-op
`return` for the rest of the page's life. In practice this is masked because
`checkRedirect` schedules `window.location.href = redirectUrl` (or
`.reload()`) 1.5s later (`api.ts:28-30`, `connection.svelte.ts:174-176,
187-189`), so the page navigates away before anyone notices the store is
wedged. But if that redirect/reload for any reason doesn't fire (e.g. browser
blocks the navigation, popup blocker, or a future code path adds an early
`return` before the timeout — as already happens once at `isAuthExpired()` in
`checkRedirect`, `api.ts:22-24` and `38-41`), the queue/history/warnings
stores would silently freeze with no error surfaced, since the whole point of
this pattern is error suppression. Suggest: at minimum, document this
finally-never-runs interaction with `#pollInFlight` where the never-resolving
promise is created, or set `#pollInFlight = false` via a `.catch()`-independent
mechanism (e.g. an explicit reset). Effort: S.

### 2b. `fetchJSON`'s auth-expiry pattern also strands `HistoryStore.poll()` and `WarningStore.poll()` without a matching in-flight guard, but strands *later `finally` blocks* generally
`ui/src/lib/stores/history.svelte.ts:19-34`, `ui/src/lib/stores/warnings.svelte.ts:17-33` — CONFIRMED. Same never-settling promise means any code with a `finally`/second-half-of-try downstream of these fetches (e.g. `SettingsDialog.saveAll`'s per-field loop at `SettingsDialog.svelte:96-109`, which does `for (const field of ...) { await setConfig(...) }`) will hang mid-loop rather than error out. `saving` stays `true` forever in that dialog if a save mid-loop hits an auth-expiry. Same effort/severity note as #2 — mitigated by the redirect, but worth a one-line comment at the definition site.

### 3. Direct-mutation "optimistic updates" on `QueueSlot` props are dead code under `$state.raw`
`ui/src/lib/components/QueueRow.svelte:90, 100, 308, 325` — CONFIRMED (traced through Svelte 5 reactivity semantics, not browser-reproduced). `changeScript`/`changeCat`/`changePP`/`commitRename` do e.g. `slot.script = newScript;` directly on the `slot` prop object after the `postAction` resolves. But `slot` originates from `QueueStore.#queue = $state.raw<QueueDetail | null>(...)` in `ui/src/lib/stores/queue.svelte.ts:9` — `$state.raw` explicitly opts out of deep reactivity/proxying. Mutating a field on one of its nested objects does **not** notify any tracking effect/derived; the only thing that makes `QueueRow`'s template re-render at all is the *parent* `#each` loop re-evaluating when `store.#queue` is *reassigned* (which happens on the next `poll()`). So these four assignments have no visible effect until the next full poll overwrites `slot` anyway — they're inert. This isn't a user-visible bug today (the WS-triggered repoll after each of these actions arrives quickly, per Finding 1), but it's dead code that will mislead the next person who edits this file into thinking there's a fast local update path. Effort: S (delete the four mutation lines, or make the fields genuinely optimistic by copying-and-reassigning slot in a local `$state`).

### 4. Hand-mirrored `Record<string, any>` swallows all type safety across the entire Settings UI
`ui/src/lib/types.ts:170,172` (`FullConfig.general: Record<string, any>`, `FullConfig.postproc: Record<string, any>`) and `ui/src/lib/components/SettingsDialog.svelte:19,52` (`configData = $state<Record<string, any> | null>(null)`) — CONFIRMED. Every config section component (`GeneralSection.svelte`, `PostProcSection.svelte`, `CategoriesSection.svelte` at `configData: Record<string, any>` line 13, `DownloadsSection.svelte`) receives `configData: Record<string, any>` and reads/writes fields by string keyword with zero compiler checking. A typo'd `keyword=` prop, a renamed Go config field, or a field moved between sections would not be caught by `tsc`/`svelte-check` at all — only by the Go-side contract test (`internal/config/ui_contract_test.go`) and only for the *existence* of the keyword, not its type. Only `DownloadsConfig`, `ServerConfig`, `CategoryConfig` are properly typed; `general` and `postproc` are not. Effort: M (type out `GeneralConfig`/`PostProcConfig` interfaces mirroring the two remaining loosely-typed Go structs).

### 5. `(res as any).result` cast duplicated in two places for the same shape
`ui/src/lib/components/SettingsDialog.svelte:183`, `ui/src/lib/components/config/ServerEditDialog.svelte:107` — CONFIRMED. Both do:
```ts
const r = (res as any).result;
if (r && typeof r.passed === 'boolean') { ... }
```
against the `test_server` action response. `api.ts` already has a typed sibling for this exact shape one status-check endpoint over (`TestConnectionResult` at `api.ts:255-260`, used by `status_overview`'s `test_connection`), but the `config`-mode `test_server` action used by these two dialogs isn't typed at all, and the same untyped/duplicated logic is copy-pasted rather than extracted into a shared `testServerConfig()` helper in `api.ts`. Effort: S — add a `TestServerResult` type + a `testServerViaConfig()` wrapper in `api.ts`, used by both dialogs.

### 6. Duplicated byte/size/speed formatting logic in three places
`ui/src/lib/utils.ts:11-22` (`formatSpeed`, `formatSize`) vs. `ui/src/lib/components/HistoryRow.svelte:43-49,61-66` (local `formatSpeed(bytes, seconds)` and a byte-for-byte identical local `formatSize`) vs. `ui/src/routes/status/+page.svelte:173-178` (`formatBytes`, a third, log-based implementation reaching TB). CONFIRMED. `HistoryRow.svelte`'s `formatSize` is a literal duplicate of `utils.formatSize` — same thresholds, same output, just redeclared instead of imported (the file already imports nothing from `utils.ts`). `status/+page.svelte`'s `formatBytes` is a different but overlapping implementation (adds a TB tier via `Math.log`). Three independent unit-formatting implementations for the same numeric domain is the kind of drift docs/ARCHITECTURE.md's "duplicated formatting logic that should be in utils.ts" flag was written to catch. Effort: S — import the shared helpers in `HistoryRow.svelte`; consider extending `formatSize` in `utils.ts` with a TB tier and reusing it in `status/+page.svelte`.

### 7. `Navbar`'s `onpausetoggle` prop is wired to a no-op
`ui/src/routes/+page.svelte:58` — `<Navbar paused={isPaused()} onpausetoggle={() => {}} />`. CONFIRMED. `Navbar.togglePause()` (`Navbar.svelte:39-47`) calls `onpausetoggle?.()` after the `postAction` resolves, presumably intended to let the parent force a refresh, but the parent passes an empty callback — the actual UI update comes entirely from the next WS-triggered `queue.poll()`. This isn't broken (the callback was never load-bearing), but it's vestigial API surface: either wire it to `refreshQueue()` for a snappier UI, or delete the prop and callback entirely. Effort: S.

### 8. `WarningStore` reimplements `BasePollStore`'s three-step `start()`/`stop()` by hand
`ui/src/lib/stores/warnings.svelte.ts:35-60` vs. `ui/src/lib/stores/base-poll.svelte.ts:17-36` — CONFIRMED. `WarningStore` does not extend `BasePollStore`; it duplicates the "initial poll + subscribeWS + onReconnected, with matching cleanup" pattern inline, including near-identical comments ("React to WS events instead of polling on a timer"). It's not wrong, just inconsistent with `QueueStore`/`HistoryStore` which do extend the base class. Likely reason: `BasePollStore` also carries pagination (`currentPageState`, `pageLimitState`, `searchTextState`) that warnings doesn't need, so extending it would drag in unused fields — but that's itself a sign `BasePollStore` is conflating two orthogonal concerns (connection-lifecycle polling vs. pagination state). Effort: M if unified (split `BasePollStore` into a `PollLifecycle` mixin and separate pagination state), S if left as-is with a comment explaining the divergence.

### 9. `ConfigSwitch`/`ConfigSelect`/`ConfigTextarea` weren't reviewed line-by-line but follow the same `onupdate` pattern as `ConfigInput` (spot-checked)
`ui/src/lib/components/config/ConfigInput.svelte:1-58` was read in full: it correctly uses a component-local `$state` (`editingValue`), a `$derived` merge of prop vs. local edit, and a debounce-then-`onupdate` callback — this is the "child updates via onupdate callback" convention from `docs/ARCHITECTURE.md`/`AGENTS.md` done right, and also correctly avoids the module-level-`$state` gotcha (all state is component-local). No finding here, called out as a pattern to point other lanes/PRs at. See Positive findings.

### 10. `general`/`postproc` config UI type gap is the same shape as Finding 4 but worth separating for synthesis: it's specifically a **backend-contract coupling** risk, not just a TS-hygiene one
Cross-reference to "Backend contract coupling" section below — flagging separately since synthesis may want to route this finding to the API↔UI boundary analysis rather than pure frontend-quality bucket.

### 11. `SettingsDialog`'s misc→general remap is a silent, undocumented-on-the-Go-side compatibility shim living entirely in the UI
`ui/src/lib/components/SettingsDialog.svelte:52-59`:
```ts
const cfg: Record<string, any> = res.config ?? res;
// The API remaps "general" → "misc" for SABnzbd compatibility
// (Sonarr reads config.misc.complete_dir). Reverse-map it so
// the UI can reference configData.general.* consistently.
if (cfg.misc && !cfg.general) {
	cfg.general = cfg.misc;
	delete cfg.misc;
}
```
CONFIRMED (code present; the described backend behavior — remapping `general`→`misc` — was not independently verified against `internal/api/*.go`, that's a different lane's territory, so the *existence and shape of this remap* is CONFIRMED but the *correctness of the backend-side claim in the comment* is INFERRED from the comment text alone). If the backend ever stops sending `misc` (or starts sending both `general` and `misc` with different content), this silent `delete cfg.misc` would quietly drop data with no error. Worth a shared regression test asserting the round-trip, or better, doing this remap once in `api.ts`'s `fetchConfig()` rather than inline in a dialog component so it can't drift per-caller. Effort: S.

## Polling vs WebSocket verdict

**Coherent, but confusingly named, and the "polling" label is now actively
misleading.** There is exactly one interval timer in application state code
(`connection.svelte.ts:67-71`, a 5-minute `setInterval` session-liveness check
that only runs when the tab is visible and already connected — not a data
poll) plus one in `ConnectionOverlay.svelte:10-12` (a 100ms UI countdown
timer, purely cosmetic) plus the debounced retry backoff in
`connection.svelte.ts`. Every other "poll" in the codebase is triggered by:
mount, an explicit user action, a WS event, or a reconnect. So:

- **Is WebSocket needed?** Yes — it's the only thing that produces the
  "live" feel (queue/history/warnings refresh, metrics/speed graph,
  postproc output streaming, server connection panel) without the user doing
  anything.
- **Is the HTTP fetch path needed?** Yes — WS carries no payload for
  queue/history slots (`WSEvent` in `websocket.svelte.ts:4-16` only carries
  scalars and a `servers` array for metrics), so every "something changed" WS
  event is necessarily just a signal to go re-fetch the real data over HTTP.
  This is a reasonable "thin event, fat re-fetch" pattern given how much data
  the queue/history responses carry, and avoids duplicating the entire
  response shape into the WS protocol.
- **Is anything vestigial?** The *naming* is: `BasePollStore`, `isPolling()`,
  `startPolling()`/`stopPolling()` describe a mechanism that no longer polls
  on a timer (it did historically, per the class name and the comments in
  `warnings.svelte.ts:40` "React to WS events **instead of polling on a
  timer**" — i.e., this was migrated away from timer-based polling and the
  vocabulary never caught up). This is exactly the kind of misleading-name
  finding worth a quick rename pass (`BasePollStore` → e.g. `LiveStore`,
  `isPolling` → `isActive`) — S effort, zero behavior change, meaningfully
  reduces the "why are there two systems" confusion a new contributor (or
  future audit) will hit immediately, as this lane did.
- **Double-fetch overlap** (Finding 1) is the one real (if minor) seam where
  the two mechanisms interact suboptimally: action → HTTP mutate → explicit
  re-poll, *and* action → HTTP mutate → backend WS broadcast → re-poll. Not a
  correctness bug thanks to the in-flight guard, just wasted round-trips.

Recommendation for synthesis: this is not "polling and WebSocket fighting
each other" — it's "one live-update mechanism (WS) plus one data-fetch
mechanism (HTTP), wearing a polling-shaped name." The simplification available
is a rename + dropping the redundant explicit re-poll after actions (Finding
1), not an architectural rework.

## Backend contract coupling

Everything below is a TypeScript interface or inline shape in `ui/src/lib/types.ts`
or `api.ts` that hand-mirrors a Go response and has no generation step or
contract test on the UI side (the only automated check is
`internal/config/ui_contract_test.go`, and it only validates that `keyword=`
strings used in Svelte config components exist as Go config tags — it does
NOT validate response shapes, field types, or non-config endpoints):

- **`QueueSlot`** (`types.ts:4-33`) — mirrors whatever `internal/api`'s queue-mode handler serializes per slot: `nzo_id, filename, name, cat, priority, status, script, password, size, sizeleft, mb, mbleft, bytes, remaining_bytes, percentage, pp, warning?, failed_bytes, recovery_bytes, recovery_files, repair_state, current_stage, articles_remaining, eta_seconds, current_file, par2_held?, files?, direct_unpack?`. A field renamed or retyped on the Go side (e.g. `percentage` changing from string to number, which SABnzbd-compat APIs are prone to do) would silently produce `undefined`/`NaN` in the UI with no compile-time signal.
- **`DirectUnpackStatus`** (`types.ts:35-43`) — `active, current_set?, completed_volumes?, total_volumes?, success_sets?, failed_sets?, failed_reasons?`.
- **`QueueFile`** (`types.ts:45-50`) — `name, bytes, bytes_downloaded, state` where `state` is a hand-written union `'queued' | 'downloading' | 'done' | 'failed' | 'held'` — if the Go side adds a new state value, TS would still compile (the field is just read and displayed via `fileStateColor()` in `QueueRow.svelte:281-289`, which falls through to a default color for unknown values) — this one degrades gracefully, worth noting as a *good* instance of forward-compatible handling of an enum-shaped field.
- **`HistorySlot`** (`types.ts:72-94`) and **`HistoryStageLog`** (`types.ts:67-70`).
- **`FullConfig`** (`types.ts:169-175`) — `general` and `postproc` are **untyped** (`Record<string, any>`), so nothing here would even show a compile error on drift (Finding 4/10).
- **`ServerConfig`** (`types.ts:122-139`) and **`CategoryConfig`** (`types.ts:141-148`) — used bidirectionally (read from `fetchConfig()`, written back via `setConfig('servers', '', JSON.stringify(servers))` whole-array replacement, `SettingsDialog.svelte:126,147` and `ServerStatusPanel.svelte:57,67`) — a field this UI doesn't know about on an existing server object would be **silently dropped** on any save, since the whole array is round-tripped through this TS type and re-serialized. This is the highest-risk drift point in the file: adding a new `ServerConfig`/`CategoryConfig` field on the Go side without adding it here doesn't just fail to display it — it causes data loss on the next save from this UI.
- **`ServerSnapshot`/`ConnSnapshot`** (`types.ts:192-213`) — the WS `metrics` event payload and the `/status?name=config`-adjacent server stats endpoint both need to match this by hand.
- **`WSEvent`** (`websocket.svelte.ts:4-16`) — a single flat interface used for every event type (`queue_updated`, `job_finalized`, `metrics`, `warnings_updated`, `history_updated`, `postproc_output`) discriminated only by the `event: string` field, with all payload fields optional across the union. There's no compile-time guarantee a given `event` string actually carries the fields code reads for it (e.g. `event.line`/`event.tool`/`event.stage` for `postproc_output` at `QueueRow.svelte:260-261`) — a typo'd event name check anywhere would just silently never fire, and a discriminated union (`{event:'metrics', speed:number, ...} | {event:'postproc_output', line:string, ...}`) would let `tsc` catch both directions of drift.
- **`api.ts`'s inline response interfaces** (`StatusOverviewGeneral/System/Response`, `CheckUpdateResult`, `TestConnectionResult`, `TestDiskSpeedResult`, `BuildInfoResponse`, `RedactedServerConfig`, `RedactedConfig` — `api.ts:208-320`) — none of these round-trip into `types.ts`; they're colocated with the fetch functions instead. Not wrong, but means there are now two places (`types.ts` and `api.ts`) a reviewer has to check for "does this shape match the Go struct," which raises the chance one gets missed.
- **`test_server` action response** used by two dialogs (Finding 5) is not typed anywhere at all — `(res as any).result`.

No shape here is derived from Go source (no `go generate`-based TS emission, no
OpenAPI/JSON-schema step). All drift protection is manual code review plus
the one keyword-existence contract test. This matches the "hand-mirrored,
could silently drift" framing the brief asked about — recommend synthesis
flag it as a candidate for a lightweight generated-types step (e.g. from Go
struct tags) if API churn is expected to continue, though this is explicitly
out of scope for this lane to prescribe further given the "no cross-cutting
migration proposals" boundary.

## Positive / load-bearing

- **`ConfigInput.svelte`** (Finding 9) is an exemplary implementation of the
  project's own documented `onupdate`-callback + component-local-`$state`
  conventions — a good reference example for future config components.
- **`QueueRow.svelte`'s `$effect` for the file-detail drawer** (lines
  218-249) has an excellent inline comment explaining exactly why `untrack()`
  is required to stop the parent's per-poll fresh-prop-object from tearing
  down the drawer's WS subscription — this is precisely the kind of
  hard-won, non-obvious Svelte 5 knowledge `docs/svelte-gotchas.md` calls out,
  and it's already correctly captured in code comments even though it isn't
  in the gotchas doc itself (candidate for backporting into
  `docs/svelte-gotchas.md`).
- **`SettingsDialog.svelte`** follows the documented "component-local `$state`
  + `.then()` chains, not `async`/`await`" pattern exactly as prescribed by
  the gotcha doc (`loadConfig()` at lines 47-68) — no drift here despite this
  being the exact component the gotcha doc says the bug was originally found
  in.
- **`Modal.svelte`/dialog convention** is followed consistently everywhere
  checked (`AddNzbDialog`, `SettingsDialog`, `ServerEditDialog`,
  `CategoryEditDialog`, `ServerStatusPanel`'s custom drawer is the one
  exception — it's not a `<dialog>`-based `Modal.svelte` consumer, it's a
  hand-rolled fixed-position drawer with its own backdrop click-to-close;
  worth a note but not a bug, since a slide-out drawer isn't really the same
  UI affordance as a centered modal).
- **`connection.svelte.ts`** is well-designed for a single-user, browser-facing
  app: threshold-based flap suppression, authoritative WS-close override,
  jittered exponential backoff, and a clean `onReconnected` pub/sub — this is
  right-sized, not over-built for a multi-tenant scenario.
- **No `{@html}` usage found anywhere in `ui/src`** — grep across the whole
  tree for `{@html` returned nothing. All user-supplied strings (job names,
  category names, warning messages, postproc output lines) go through
  Svelte's default text-interpolation escaping. Good baseline XSS posture for
  a browser-facing app with no server-side auth of its own.
- **No stray `setInterval`/timer-based polling** exists beyond the two
  legitimate, narrowly-scoped ones identified in the verdict section — the
  team clearly already did the work of migrating off interval polling to a
  WS-driven model; only the naming lags the migration.
- **`utils.ts`'s `getRedirectUrl`** is a well-isolated, testable pure
  function correctly shared between `api.ts` and `connection.svelte.ts`
  rather than duplicated — a good counter-example to Finding 6.
- **`package.json` already has `knip` wired up** (`check:unused` script) for
  dead-export/dead-dependency detection — this lane did not run it (would
  require `npm install`, out of scope for a read-only review), but its
  presence means the dead-code-detection gap this lane might otherwise flag
  is already covered by project tooling. Recommend synthesis note: confirm
  `check:unused` is actually run somewhere in CI/quality gates, since
  `docs/`'s quality-gate list (`AGENTS.md`) does not mention it alongside
  `golangci-lint`/`go vet` — if it's not wired into CI it's dead tooling
  itself.
- **Single-user right-sizing is generally good**: no pagination/virtualization
  over-engineering for lists (queue/history use simple offset pagination
  matching the SABnzbd API's own `start`/`limit`, not client-side
  virtualization the single user doesn't need); no optimistic-concurrency/
  ETag/version-vector handling for concurrent-editor scenarios; no
  client-side caching layer beyond the store's last-fetched snapshot. This is
  appropriately lean for the stated single-user deployment model.

## Open questions for synthesis

1. Finding 1 (double-poll after actions) needs the backend lane to confirm
   whether `queue_updated`/`history_updated` WS broadcasts are synchronous
   with the HTTP action response or could arrive with enough delay that
   removing the explicit `poll()` after actions would introduce a visible
   staleness window.
2. Finding 11's claim about `general`→`misc` remapping happening server-side
   needs the API lane (`internal/api/*.go`) to confirm the comment's premise
   is still accurate — if the backend behavior changed, this UI-side shim is
   now dead code or, worse, actively wrong.
3. Is there an appetite for a lightweight generated-TS-from-Go-structs step
   given the size of the "Backend contract coupling" list? This lane surfaces
   the list; the tradeoff decision (worth the build-step complexity for a
   single-consumer UI?) belongs to synthesis/maintainers, not this lane.
4. `ServerStatusPanel.svelte`'s hand-rolled drawer (not going through
   `Modal.svelte`) — worth confirming with the author whether this was a
   deliberate exception (slide-out vs. centered dialog affordance) or an
   oversight predating the `Modal.svelte` convention being written down.
5. Should `docs/svelte-gotchas.md` be extended with the `untrack()`-for-
   fresh-prop-object pattern from `QueueRow.svelte` (Positive findings)? It's
   currently tribal knowledge in one file's comments, exactly the kind of
   thing the gotchas doc says was extracted from other real bugs.

## git status proof

```
$ git status --short
(no output — clean tree)
```
