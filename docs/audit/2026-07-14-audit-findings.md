# GoNZBD Audit — Findings & Task Backlog (2026-07-14)

Cross-cutting audit across three areas: **Security**, **Optimization/Code-quality**, and
**Traceability** (UI → API client → HTTP → services → persistence). Produced by six
analysis passes (Security, Optimization, and one per traceability layer).

## How to use this file (for humans and sub-agents)

- Each task has a stable **ID** (`SEC-n`, `OPT-n`, `TRACE-n`), a **priority**, a
  **location**, the **problem**, a **recommended fix**, an **effort** estimate, and
  **acceptance criteria**.
- Pick a task by ID. Do **one task per commit** (project convention). Follow the
  Red-Green TDD discipline in `AGENTS.md`: write/adjust the failing test first, watch it
  fail, then fix.
- Respect the "do not regress documented hot paths" rule in `AGENTS.md` §Performance.
- Update the checkbox and add a short note when done.

## Status legend

`[ ]` not started · `[~]` in progress · `[x]` done

---

## Executive summary

| Area | Headline | Highest severity |
|---|---|---|
| Security | Codebase is defensively written; **one architectural auth gap dominates everything else**. | **HIGH → CRITICAL on non-loopback binds** (SEC-1) |
| Optimization | Go backend is unusually clean (1 lint finding total). Findings are consolidation/decomposition + inert config knobs. | Low risk; best ROI is removing dead/inert surface |
| Traceability | All four layers traced end-to-end and **clean/consistent**. Three real findings surfaced. | Data race (TRACE-1), unrestored early-abort counters (TRACE-3) |

> **Status as of 2026-07-15: all 23 findings are done.** Batch 1 (see `## Suggested order of attack` → Batch review) closed SEC-1, SEC-2, SEC-5, TRACE-1, TRACE-3, TRACE-4, OPT-1, OPT-2, OPT-4, OPT-5, OPT-10, OPT-11, OPT-13. Batch 2 closed the remainder — SEC-3, SEC-4, SEC-6, TRACE-2, OPT-3, OPT-6, OPT-7, OPT-8, OPT-9, OPT-12 — via `superpowers:subagent-driven-development` in an isolated worktree (branch `worktree-audit-backlog-batch2`, commits `dc00511`..`4b8c096`, 18 commits across 9 tasks), each task implemented and independently reviewed before the next began. See individual finding entries above for commit hashes and what each task's review verified.

The single most important item is **SEC-1**: the web-UI session cookie is handed to any
unauthenticated caller and grants admin, so the whole deployment's safety currently rests
on binding to `127.0.0.1`. Fix that before anything else.

---

## Branch review — `audit-traceability-refactor` (verified 2026-07-14)

Another agent committed 13 commits to `audit-traceability-refactor`. Each finding was
re-verified against the **actual branch diff** (not commit messages) by three model passes.
Result: **1 finding fully fixed (OPT-13), 2 partial (SEC-1, OPT-9), 20 unaddressed.** The
branch's target files for most findings show *zero diff* vs `main`; the commits did real
work, but mostly on **different** targets than the audit named.

| Verdict | Findings |
|---|---|
| ✅ Fixed | **OPT-13** (`tint.NewHandler`→`NewTextHandler`) |
| 🟡 Partial | **SEC-1** (cookie-CSRF closed for a fixed state-changing-mode allowlist via cookie+POST restriction, but unauthenticated `GET /` still issues an admin cookie usable for `get_config`/`browse`/`config` reads and add-job writes — core gap open) · **OPT-9** (`modeQueue` switch decomposed, CCN 18→16; the two *named* hotspots `AddJob` CCN 23 and `queueList` CCN 22 are unchanged) |
| ⬜ Not addressed | SEC-2, SEC-3, SEC-4, SEC-5, SEC-6 · OPT-1, OPT-2, OPT-3, OPT-4, OPT-5, OPT-6, OPT-7, OPT-8, OPT-10, OPT-11, OPT-12 · TRACE-1, TRACE-2, TRACE-3, TRACE-4 |

**OPT-7 note:** not the named `serveMode`/`run` target, but the branch decomposed a
*different* complexity hotspot instead (`dispatch.go` and `assembler.go` write controllers,
commit `6be4802`) — legitimate work, just not this finding.

### Net-new fixes on the branch (not in this audit — recorded so they aren't lost)

These are genuine improvements the branch added that the audit didn't call out:

- **`1eeb800`** — par2 parser caps packet-body allocation at 64 MiB (`internal/par2/parser.go`), preventing OOM from a corrupt/malicious `.par2` header. *(Security/DoS — good.)*
- **`e27fbd6`** — `cmdutil` now hard-errors on malformed `nice`/`ionice` priority strings instead of silently dropping throttling (`runner.go`, `sandbox.go`).
- **`088beea`** — enforces the unrar-switch allowlist at config-load / `set_config` time (`config/validate.go`), and `configSpeedLimit` returns 500 on save failure instead of swallowing it.
- **`2d21d87`** — cookie-auth CSRF hardening (the SEC-1 partial): 405 for cookie-authenticated non-POST state-changing modes + `isCrossOrigin` fails closed on empty Origin/Referer for cookie POSTs.
- **`849aa1d`** — decoder scratch-buffer reuse used a pointer compare that panicked on empty slices; fixed via `unsafe.SliceData` guard + regression tests (`internal/decoder/decoder.go`).
- **`5e1a431`** — `modeAbout` propagates `r.Context()` into the public-IP (ipify) lookup so client disconnects cancel it.
- **`9d73065`** — bulk-delete loop logs per-job removal errors instead of silently swallowing them.
- **`4393325`** — hot-path perf: `sync.Pool` decode scratch buffers, reused `scratchBuf` in `buildContiguousRun`, early-exit `allServersFull` in `buildDispatchPlan`.
- **`2fb4757`** — dead-code pruning of *different* symbols than OPT-1/OPT-2: unexported `SnapshotJobByName`, `goodConnections`, and moved the `nopapp` test stub out of the production package into `internal/api/apitest/`.
- **`eae5bf6`** — broad API boilerplate consolidation (`requireQueue`/`requireApp`/`requireConfig`/… helpers, `formValue` extended to read multipart/POST bodies).

> These net-new items are **not** re-verified for correctness beyond the diff read; if any
> should become tracked tasks (e.g. a security regression test for `2d21d87`), add them below.

---

## 1. SECURITY

Categorized Critical / High / Medium / Low. No Critical found at the default loopback
bind; SEC-1 becomes effectively Critical on any LAN/`0.0.0.0` bind.

### [x] SEC-1 — Web UI hands an admin session cookie to unauthenticated callers  · HIGH (CRITICAL on non-loopback bind)
> **✅ Done (PR #78, merged 2026-07-16 as `156f4a1`/`76f129f`/`83fa33c`):** Loopback-only trust by default; `general.local_ranges` (CIDRs/bare IPs) explicitly extends trust to a reverse-proxy/Docker/LAN range, `general.verify_xff_header` optionally validates the `X-Forwarded-For` chain once the peer qualifies (`internal/config/trust.go`). Enforced at **both** cookie issuance (`internal/web/spa.go`, via a `trustedFn` callback) and cookie acceptance (`internal/api/middleware.go`'s `callerLevel`) for defense-in-depth — a leaked/forged cookie replayed from an untrusted address is rejected even if issuance is ever bypassed. `cmd/gonzbd/main.go` warns at startup when bound non-loopback with no `local_ranges` configured. Red-green proven: `TestSEC1_UntrustedCookieRejected` fails when the acceptance-side check is neutralized (replaying a valid cookie from a non-loopback/untrusted-private address wrongly authenticates as admin). Went through three rounds of review (CodeRabbit + manual) that found and fixed two real bugs beyond the original design: IPv4-mapped CIDRs in `local_ranges` were silently accepted but could never match any peer (`normalizeMappedPrefix`), and `--listen :PORT` (bare port, wildcard bind) silently bypassed the non-loopback startup warning. gremlins: 0 lived mutants across `internal/config`, `internal/web`, and `cmd/gonzbd` for all diff lines.
- **Location:** `internal/web/spa.go:27-46` (cookie set unconditionally), `internal/api/middleware.go:51-78` (`callerLevel` cookie path → `LevelAdmin`), `cmd/gonzbd/main.go:319-327` (SPA mounted on API port, no auth gate).
- **Problem:** `GET /` sets `gonzbd_apikey=<sessionKey>` with **no authentication**, and a cookie whose value equals `sessionKey` is granted `LevelAdmin`. The only gate on the cookie path is `isCrossOrigin(r)`; a non-browser client sends no `Origin`/`Referer`/`Sec-Fetch-Site`, so it is treated as same-origin and accepted as admin. Anyone who can open a TCP connection can: `GET /` to receive the cookie, then call `shutdown`, `set_config`, `get_config`, `queue&name=delete&value=all`, `browse`, etc. Default `host: 127.0.0.1` mitigates out-of-box, but `0.0.0.0`/LAN binding is supported (`General.Host`, `--listen`) and yields zero-credential full admin. Weaker than upstream SABnzbd (which requires apikey or login).
- **Fix:** Do not issue an admin-capable credential to unauthenticated callers. Preferred: require the real `apikey` (or a login) before issuing a session cookie, or bind the session to server-side state established only after auth. Minimum: refuse cookie issuance/acceptance for admin when the bind address is non-loopback, and require the permanent `apikey` for remote access. Additionally require `POST` + same-origin for state-changing modes (partially present via `isStateChangingMode`).
- **Effort:** M
- **Acceptance:** An unauthenticated non-browser client bound to a non-loopback address cannot reach any `LevelProtected`/`LevelAdmin` mode; regression test covering the "GET / then replay cookie" path returns 401.

### [x] SEC-2 — `mode=get_config` returns all secrets unredacted  · MEDIUM
> **✅ Done (PR #75, `c7b3aca`, verified 2026-07-15):** `modeGetConfig` now marshals `s.config.Redacted()`. `TestModeGetConfig_RedactsSecrets` asserts no known secret value (server password, email password, Apprise URLs, apikey, nzbkey) appears in the response body; red-green proven against the unpatched handler.
- **Location:** `internal/api/config.go:51-106` (`json.Marshal(cfg)` verbatim).
- **Problem:** Unlike the status page (`statusConfig` → `config.Redacted()`), `modeGetConfig` emits `General.APIKey`, `General.NZBKey`, every `Servers[].Password`, `Notifications.Email.Password`, and Apprise URLs (embed webhook tokens) in cleartext. Chained with SEC-1 this leaks every stored credential, including the permanent API key.
- **Fix:** Run the config through `config.Redacted()` (already exists, `internal/config/redact.go`) before emitting, or emit the redaction placeholder for secret fields. Audit `Redacted()`'s allowlist whenever a new secret field is added (it does not currently cover a notification `Script` path or future server-level tokens).
- **Effort:** S
- **Acceptance:** `get_config` response contains `********` (or omits) for all credential fields; a test asserts no known secret value appears in the body.

### [x] SEC-3 — Secrets logged in cleartext via request-query logging  · MEDIUM
> **✅ Done (commit `1cc680b`, verified 2026-07-15):** `sanitizeQuery` now redacts by substring match on the param name (`pass`/`key`/`secret`/`token`, case-insensitive) in addition to the exact `apikey`/`nzbkey` match, and redacts the `value` param when a sibling `keyword` param names a known secret field (`set_config&keyword=password&value=…`). Caught and fixed a real ordering bug during implementation: the generic substring-redaction loop was clobbering the `keyword` param's own value (since "keyword" contains "key") before the keyword-indirected check could read it — fixed by capturing `keyword` before the loop runs.
- **Location:** `internal/api/middleware.go:234-238` + `sanitizeQuery` at `:245-258`.
- **Problem:** `sanitizeQuery` only redacts `apikey`/`nzbkey`; all other params are logged verbatim at Info. Secrets travel in other params: `config&name=test_server&password=…`, `set_config&keyword=password&value=…`, `addurl&…&password=…`. Sending any as a URL query writes the provider/NZB password to structured logs.
- **Fix:** Redact by value context, not just the two key names: redact any param whose name contains `pass`/`key`/`secret`/`token`, and redact `value` when `keyword` denotes a secret. Simplest robust option: log only `mode`/`name` and drop the raw query.
- **Effort:** S
- **Acceptance:** A request carrying `password=`/secret `value=` produces a log line with the secret redacted; unit test on `sanitizeQuery`.

### [x] SEC-4 — `/debug/vars` (expvar) exposed without auth  · LOW
> **✅ Done (commit `d4d850d`, verified 2026-07-15):** `/debug/` is now wrapped by a `trustGate` helper in `composeRouter` that reuses the exact same `trustedFn` predicate (`config.IsTrustedRemote`) already built for the SEC-1 admin-cookie-issuance gate — no new/parallel trust mechanism. Untrusted callers get 404 (not 403, to avoid confirming the path exists).
- **Location:** `cmd/gonzbd/main.go` `composeRouter` (`mux.Handle("/debug/", http.DefaultServeMux)`), publisher `internal/telemetry/telemetry.go`.
- **Problem:** `/debug/` routes to `http.DefaultServeMux`, outside the auth-checked API handler and `MaxBytesReader`. expvar publishes process `cmdline` (`os.Args` — leaks config/admin paths, possibly usernames) and `memstats` to any unauthenticated caller. Reachable remotely on non-loopback binds. (pprof is correctly not imported.)
- **Fix:** Don't mount `http.DefaultServeMux`; register expvar on the app mux behind the same auth as the API, or gate `/debug/` to loopback. Avoid publishing `cmdline`.
- **Effort:** S
- **Acceptance:** `/debug/vars` requires auth (or is loopback-only); no `cmdline` exposure.

### [x] SEC-5 — WebSocket upgrade disables the library Origin check  · LOW
> **✅ Done (PR #75, `bdea247`/`6bac496`, verified 2026-07-15):** Removed `InsecureSkipVerify: true`; `AcceptOptions{}` now leaves the library's default same-origin check active. `TestBroadcaster_Handle_CrossOriginRejected`/`_SameOriginAccepted` cover the rejection/acceptance paths.
- **Location:** `internal/api/events.go:90-93` (`websocket.AcceptOptions{InsecureSkipVerify: true}`).
- **Problem:** Disables `coder/websocket`'s built-in same-origin enforcement; CSRF protection for `/api/ws` then relies solely on `callerLevel`'s `isCrossOrigin` + `SameSite=Strict`. Currently adequate but removes a redundant control; would become exploitable if the cookie's cross-origin gate regressed.
- **Fix:** Set `AcceptOptions.OriginPatterns` (or leave default same-origin check on) instead of `InsecureSkipVerify: true`.
- **Effort:** S
- **Acceptance:** Cross-origin WS handshake rejected at the library layer independent of `callerLevel`.

### [x] SEC-6 — `mode=browse` / `mode=addlocalfile` allow filesystem enumeration/probing  · LOW
> **✅ Done (commit `59e86e0`, verified 2026-07-15):** Both handlers now reject any absolute path that isn't equal to or nested under `{download,complete,dirscan,script}_dir` via a shared `pathWithinConfiguredRoots` check (an all-empty config rejects every path, never allows everything). `addlocalfile` raised from `LevelProtected` to `LevelAdmin`. Adapting the allowlist broke 8 pre-existing tests across `misc_test.go`/`queue_test.go` beyond the two anticipated in planning — each was verified to be a genuine setup adaptation (configuring a root and testing within/outside it), not a weakened assertion.
- **Location:** `internal/api/misc.go:62-117` (`modeBrowse`, `LevelAdmin`), `internal/api/queue.go:795-854` (`modeAddLocalFile`, `LevelProtected`).
- **Problem:** `browse` lists any absolute directory (only requires `filepath.IsAbs`); `addlocalfile` opens any absolute path without `..` and reports open/stat/parse errors distinctly (existence/readability oracle). Intended for the directory picker; combined with SEC-1 an unauthenticated attacker can walk the filesystem. `addlocalfile` at `LevelProtected` lets even the upload-only NZB key probe arbitrary paths.
- **Fix:** Constrain both to an allowlist of configured roots (download/complete/script dirs); raise `addlocalfile` to `LevelAdmin`. Primary mitigation is SEC-1.
- **Effort:** M
- **Acceptance:** `browse`/`addlocalfile` refuse paths outside configured roots; `addlocalfile` requires admin.

**Verified-safe (defenses checked and found adequate):** constant-time key comparison; 32-byte crypto/rand session key; browser CSRF (HttpOnly + SameSite=Strict + Origin/Referer/Sec-Fetch-Site); auth param-source ordering (query/header before form, after MaxBytesReader); body-size limits; parameterized SQL with `ESCAPE` + capped limits; SSRF guards in urlgrabber (private/loopback/rebind-safe); archive path-traversal via `os.OpenRoot`/`RootedOpenFile` + non-regular entry skip + setuid strip; decompression-bomb limits; XXE not applicable (Go xml, charset allowlist); command execution via arg slices (no shell) with metachar allowlist; TLS 1.2 min with explicit opt-in insecure levels + manual `VerifyConnection`; file perms (0600 config/key, atomic writes); fixed WS broadcaster map-mutation locking; global security headers.

---

## 2. OPTIMIZATION / CODE QUALITY

Backend is clean: `golangci-lint`/`staticcheck` report **one** issue total; no `interface{}`
in prod, no `sort.Slice`, no un-preallocated slices in hot paths, no `defer` in hot loops.
Documented hot paths verified intact — **do not touch them.**

### Dead / inert code
- [x] **OPT-1** — Delete unused exported option `WithLifecycleContext` (`internal/app/app.go:1379`, zero callers incl. tests). Effort S. *Confidence High.* **✅ Done (PR #76, `89477e8`).**
- [x] **OPT-2** — Remove dead test helper `scenarioHarness.JobStatus` (`internal/app/scenario_test.go:237`, no callers). Effort S. *Confidence High.* **✅ Done (PR #76, `3e06b9e`).**
- [x] **OPT-3** — Resolve inert config knobs `no_penalties`, `pre_check`, `MaxArtOpt`: plumbed end-to-end but never consulted (`internal/config/downloads.go:37,42`; `internal/downloader/downloader.go:110,120,125`; `internal/downloader/dispatch.go:37,171`; wired `internal/app/app.go:1327-1336`). Either implement the behavior or remove the config surface + docs — shipping knobs that silently do nothing is a support trap. Effort M (implement) / S (remove). *Confidence High.* **✅ Done (commit `2f60538`, verified 2026-07-15), decided to implement rather than remove.** `NoPenalties` clamps every applied penalty to `constants.PenaltyShort`; `MaxArtOpt` caps per-article attempts on optional/backup servers separately from `MaxArtTries`; `PreCheck` issues an NNTP `STAT` before `BODY` to skip the body transfer on articles already reported missing — gonzbd's own simplified design per its config doc comments, not a transliteration of SABnzbd's larger `nzo.precheck` job-state-machine feature. Verified race-free (`d.opts` is write-once, set in `New()`, never mutated post-construction) and hot-path-safe (no new per-article allocation/scan beyond a single `optionalTried` count per `selectServerForArticle` call).

### Duplication
- [x] **OPT-4** — Consolidate gzip-atomic writers: `app.writeGzFile` (`internal/app/app.go:1524`) and `queue.writeGzJSONRaw`/`writeGzJSON` (`internal/queue/persistence.go:234,216`) are near-identical across packages. Add `fsutil.WriteGzAtomic`/`WriteGzAtomicBytes` next to `WriteAtomic`. Effort S. *Confidence High.* **✅ Done (PR #76, `e234015`/`7f71a87`).** Also hardened during review: `9aaf78a` fixed a panic-leak + chmod-TOCTOU in the shared `writeAtomic`.
- [x] **OPT-5** — `certgen.writeFileAtomic` (`internal/app/certgen.go:110`) re-implements `fsutil.WriteAtomic` (only delta is a `Chmod`). Replace with `fsutil.WriteAtomicBytes` + `os.Chmod`, or add a perm variant to `fsutil`. Effort S. *Confidence High.* **✅ Done (PR #76, `db0dbe0`).**
- [x] **OPT-6** — Three archive extractors share a large per-entry "write one entry safely" body: `extractTarFile` (`internal/unpack/go_tar.go:213`), `extractSevenZipFile` (`internal/unpack/go_sevenzip.go:197`), `ExtractEntryRarengine` (`internal/unpack/go_unrar.go:191`). Extract a shared `writeEntry(...)` + `checkBombLimits(...)` helper. Also lowers OPT-8 complexity. Effort M. *Confidence High.* **✅ Done (commit `3a25b91`, verified 2026-07-15).** New `internal/unpack/write_entry.go` (`writeEntrySafely`/`projectedBombCheck`) captures the shared skip/create/copy/chmod/chtimes/bomb-check logic; format-specific reader acquisition stays at each call site. Format-specific quirks (go_unrar's lack of an independent bomb-limit check; go_sevenzip's no-drain-on-skip and CRC32-gated-on-write behavior) verified preserved exactly, not folded away by the abstraction. All three existing test suites pass unmodified.

### Complexity (measured via `gocyclo`; re-measure and cite real numbers in commits)
- [x] **OPT-7** — Decompose `serveMode` (CCN **48**, `cmd/gonzbd/main.go:118`) and `run` (CCN 28, `:615`): extract `loadOrCreateConfig`, `ensureRuntimeDirs`, `setupTLS`, `installSignalHandlers`. Effort L. *Confidence High.* **✅ Done (10 commits `2365440`..`4b8c096`, verified 2026-07-15).** Measured `serveMode` 49→19, `run` 28→15. Extraction boundaries were derived from a full read of both functions rather than the audit's guessed names — landed as `loadOrCreateConfig`, `ensureRuntimeDirs`, `configureLogging`, `acquireLockAndPID`, `ensureSelfSignedCert`, `startListeners`, `awaitShutdownSignal`/`shutdownServeMode`, `waitForCompletion`, `printSummary`, `buildAPIServer` (`setupTLS`/`installSignalHandlers` as literally named didn't match the real control flow). One extraction per commit throughout. The two hazards this file's own `AGENTS.md` entry warns about — shutdown-sequence ordering and lost `context.Canceled` handling — were traced against the diff and confirmed preserved bit-for-bit. Deliberately stopped short of decomposing `serveMode`'s queue/router-wiring middle section (mostly straight-line initialization, not a meaningful CCN win).
- [x] **OPT-8** — Reduce the extractor trio complexity (CCN 26/24/…) via OPT-6's shared helper rather than splitting each. Effort M (shared with OPT-6). **✅ Done together with OPT-6 (commit `3a25b91`).** Measured: `extractTarFile` 24→3, `extractSevenZipFile` 26→7, `ExtractEntryRarengine` 16→3.
- [x] **OPT-9** — Decompose `(*Application).AddJob` (CCN 23, `internal/app/app.go:428`) and `(*Server).queueList` (CCN 22, `internal/api/queue.go:336`) — separate validation/filtering/assembly. Effort M. **🟡 Partial on `audit-traceability-refactor`:** `modeQueue`'s switch was decomposed (CCN 18→16, commit `eae5bf6`), but the two *named* hotspots `AddJob` (still CCN 23) and `queueList` (still CCN 22) are unchanged. Remaining work stands. **✅ Fully done (commits `3f72628`, `346d44f`, verified 2026-07-15).** Extracted `detectDuplicateNZB` from `AddJob` (CCN 23→14) and `filterQueueSlots` from `queueList` (CCN 22→13), one commit per package. The `force`/`!force` asymmetry in `AddJob` (Status set only when `!force`; Warning text differs either way) was preserved exactly. `filterQueueSlots` builds on OPT-12's `duStatuses` snapshot rather than reverting to per-job locking. A gremlins diff-scoped mutant exposed one genuine pre-existing coverage gap (`mode=queue&status=` never tested); closed with a new additive test rather than weakening the gate.

### Performance (non-hot-path; low but trivial wins)
- [x] **OPT-10** — `queueList` re-evaluates `strings.ToLower(search)` per job (`internal/api/queue.go:374`). Hoist `searchLower` above the loop; lowercase `j.Name`/`j.Filename` once. Effort S. **✅ Done (PR #77, `4fc5a7d`).**
- [x] **OPT-11** — Preallocate per-request slices with known size: `internal/api/queue.go:363`, `internal/api/history.go:139`, `internal/history/repository.go:165` (`make([]T, 0, len(src))` / cap by `limit`). Effort S. *Confidence Medium (low value).* **✅ Done (PR #77, `0c16dee`).** Hardened after review (`90aae8d`): `Repository.Search`'s preallocation is now capped at 10,000 independent of the caller-supplied `Limit` — an uncapped huge `Limit` crashed the process with an actual `fatal error: runtime: out of memory` before the fix.
- [x] **OPT-12** — `DirectUnpackStatus(j.ID)` called per job in the list loop (`internal/api/queue.go:380-383`). If it takes a mutex, fetch one `DirectUnpackStatuses()` snapshot before the loop. **Verify lock cost first.** Effort S–M. *Confidence Medium.* **✅ Done (commit `b0b0986`, verified 2026-07-15).** Lock cost verified real before fixing: `DirectUnpackStatus` took `app.mu` — a plain `sync.Mutex` shared by the entire `Application`, not scoped to direct-unpack state — once per job. Added `DirectUnpackStatuses()` snapshotting all statuses under one lock acquisition; `queueList` now calls it once before the per-job loop. `apitest.NopApp` embedding meant a single stub addition satisfied the interface for every other test fake in the package.

### Modernization
- [x] **OPT-13** — Replace deprecated `tint.NewHandler` with `tint.NewTextHandler` (`internal/app/logging.go:86`). The only lint finding in the backend. Effort S. **✅ Done on `audit-traceability-refactor` (verified 2026-07-14).**

### Tooling gaps (recommendations, not tasks)
- `deadcode` cannot see build-tagged suites (`integration`/`e2e`/`uitest`) — cross-check before deleting test-only prod helpers; don't wire as a hard CI gate without a tag strategy.
- UI has **no** unused-export detector installed (only `svelte-check`). Consider adding `knip` to `ui/devDependencies` + a `check:unused` script to cover unused TS exports/files/deps (`api.ts` exports 28 symbols, unverified).

---

## 3. TRACEABILITY

Overall the cross-layer wiring is **consistent**. UI→client and client→HTTP boundaries
(the ones with no compiler linking them) were traced end-to-end and are clean.

### [x] TRACE-1 — `queue.Get` returns a live `*Job` pointer; handlers read mutable fields without the per-job lock  · Risky (data race)
> **✅ Done (PR #77, `c056c65`, verified 2026-07-15):** `queueSetPaused`/`queueChangeCat` now read via `SnapshotJob` (deep copy under RLock) instead of the live `*Job` from `queue.Get`. `TestQueueSetPaused_ConcurrentMutationRace`/`TestQueueChangeCat_ConcurrentMutationRace` prove it under `-race`. Hardened after review: the original fixed-iteration mutator only caught the underlying regression 1/10 times under `-race -count=10`; switched to a stop-channel-driven mutator that runs for the full test duration, verified reliable. A related false-500 bug in `queueChangeCat` (returning 500 when a concurrent removal raced the post-change snapshot, despite the change already succeeding) was found and fixed in the same PR (`8420beb`).
- **Location:** `internal/queue/queue.go:87-95` (`Get` returns the live pointer), callers `internal/api/queue.go:573` (reads `job.Name`) and `:653-659` (reads `job.Category`, `job.PP`, `job.Script`, `job.Priority`) for log lines.
- **Problem:** Unlike `Snapshot`/`SnapshotJob` (which deep-copy via `cloneJob`, used by all listing/detail paths — verified safe), `Get` hands out the live struct while the download pipeline concurrently mutates it under the per-`Job` mutex. Reading these fields without that lock is a data race (a documented `AGENTS.md` hazard: "Don't expose mutable data to concurrent readers"). Impact is benign today (log values only) but it will trip `-race` and is a latent correctness trap if a caller ever acts on the read.
- **Fix:** Return the field values from inside a locked accessor (e.g. have the setter return the resulting values, or add a `SnapshotJob`-based read), or drop the live-pointer log reads. Prefer removing `queue.Get`'s live-pointer API in favor of copied accessors.
- **Effort:** S–M
- **Acceptance:** No API handler reads mutable `*Job` fields off a live pointer; `go test -race ./internal/api/... ./internal/queue/...` clean under a concurrent-mutation test.

### [x] TRACE-2 — Document/limit orphaned-from-UI handlers  · Cleanup
> **✅ Done (commit `dc00511`, verified 2026-07-15):** Added a comment block in `router.go` immediately above `registerModes`'s mode table marking `server_stats`, `fullstatus`, `watched_now`, `disconnect`, `addlocalfile`, `addurl` as third-party/SABnzbd-compat-only, not first-party-UI-driven.
- **Location:** `internal/api/router.go:77-104`.
- **Problem:** `server_stats` is registered and functional, but the UI obtains per-server stats over the **WebSocket telemetry** channel (`ui/src/lib/stores/telemetry.svelte.ts` → `getServerStats`), never via the HTTP endpoint. Similarly `fullstatus`, `watched_now`, `disconnect`, `addlocalfile`, `addurl` are not driven by the current UI. These are **valid SABnzbd/third-party compatibility endpoints**, not dead code — but that intent is undocumented, so a future reader can't tell "third-party surface" from "dead handler."
- **Fix:** Add a short comment block in `router.go` marking which modes are third-party-compat-only (no first-party UI caller). No behavior change.
- **Effort:** S
- **Acceptance:** `router.go` annotates UI-orphan-but-external modes.

### [x] TRACE-3 — Early-abort counters (`ArticlesResolved`/`ArticlesFailed`) are not restored on queue load  · Risky (narrow correctness)
> **✅ Done (PR #77, `9306a44`, verified 2026-07-15):** `recomputePending` now seeds both counters from the persisted article `Done`/`Failed` flags. New round-trip test persists a job with mixed outcomes, reloads, and asserts the restored counters and `IsEarlyAbort` outcome; red-green proven.
- **Location:** `internal/queue/job.go:171-183` (transient `json:"-"` fields), `internal/queue/job.go:253-279` (`recomputePending` — the only load-time reinit — does not touch them), incremented only live at `internal/queue/queue.go:769,833`; consumed by `IsEarlyAbort` (`internal/queue/job.go:233-246`); load path `internal/queue/persistence.go:114-152`.
- **Problem:** `recomputePending()` restores `Pending`, `PendingArticles`, `BytesDownloaded`, `FileIdx`, and `artIdx` from the persisted article `Done`/`Failed` flags, but **not** `ArticlesResolved`/`ArticlesFailed`. After a restart both reset to 0 regardless of how many articles already succeeded/failed on disk. The early-abort heuristic (fires when ≥80% of the first 10 *resolved* articles fail, to bail out of a DMCA'd/expired NZB) then re-samples only the new session's outcomes. A resumed, mostly-complete job that happens to hit a cluster of ≥10 failures right after restart (e.g. a dead file/server at the resume point) can be **false-positive early-aborted** despite having downloaded most of its content; conversely a genuinely-dead job "forgets" its prior failures. This is exactly the `AGENTS.md` hazard "ensure [transient state] is initialized in both Add and Load," and it is currently **untested** (the `IsEarlyAbort` tests set the counters in-memory only; the persistence round-trip test never asserts them).
- **Fix:** Seed both counters inside `recomputePending`'s existing article loop from ground truth — `resolved += Done`, `failed += (Done && Failed)`. (This is strictly more correct than resetting to 0: a mostly-good job then computes a low failure rate and won't abort, while a truly-dead resumed job aborts immediately.) `EarlyAborted` resetting to `false` on load is benign and can stay. Add a round-trip test that persists a job with mixed Done/Failed articles, reloads, and asserts the seeded counters + `IsEarlyAbort` outcome.
- **Effort:** S
- **Acceptance:** After `Load`, `ArticlesResolved`/`ArticlesFailed` equal the persisted Done/Failed counts; new round-trip test proves it (red-green: fails before the `recomputePending` change).

### [x] TRACE-4 — History `scanEntry` cannot read NULL into non-pointer columns  · Low (robustness)
> **✅ Done (PR #77, `36c7a53`, verified 2026-07-15):** `scanEntry` now scans nullable columns via `sql.Null*` and coalesces to zero values, instead of scanning directly into `string`/`int64`.
- **Location:** `internal/history/repository.go` `scanEntry` (scans TEXT/INTEGER columns into `string`/`int64`), schema `internal/history/migrations/001_initial.sql` (columns are nullable, no `NOT NULL`).
- **Problem:** `Add` always binds concrete (non-NULL, zero-valued) values so app-written rows never contain NULLs, but the schema permits them. A row inserted by any other means (manual `sqlite3`, a future migration, an external tool) with a NULL TEXT/INTEGER column would make `scanEntry` fail (`converting NULL to string is unsupported`), breaking `Get`/`Search` for the whole result set. Latent, not a live bug.
- **Fix:** Either add `NOT NULL DEFAULT ''`/`DEFAULT 0` in a new migration (never edit `001_initial.sql`), or scan through `sql.Null*`/`COALESCE(col, '')` in the query. Low priority.
- **Effort:** S
- **Acceptance:** A row with NULL columns round-trips through `Get`/`Search` without error.

### Traceability layers — verification status (all complete)
- [x] **Layer 1 (UI → API client):** Clean. Every `onclick`/`postAction`/store `fetch` maps to a real `api.ts` function with correct arity/param-names/types (`queue` name sub-actions delete/pause/resume/change_script/change_cat/change_opts/rename all send `value`/`value2` matching the handler; `config&name=test_server` sends host/port/username/password/ssl/ssl_verify matching `configTestServer`; `history` retry/delete/purge match). `svelte-check` passes with 0 errors.
- [x] **Layer 2 (API client → HTTP):** Clean. Every client `mode` + `name=` maps to a registered handler in `router.go`; params read by handlers match those sent; response envelopes (`{status, result:{...}}`, `{status, config}`, `{queue:{slots}}`, etc.) match the TS interfaces (`CheckUpdateResult`, `RedactedConfig`, `BuildInfoResponse`, `StatusOverviewResponse`, …). No broken/nonexistent-mode calls found.
- [x] **Layer 3 (HTTP → services):** Clean. Signatures compiler-guaranteed; nil-dependency guards present via `requireQueue`/`requireApp`/`requireParam`; contexts propagated with timeouts on blocking calls. One semantic finding → **TRACE-1**.
- [x] **Layer 4 (services → persistence):** Complete (finished inline after the agent was interrupted). **History SQLite** verified clean: INSERT (29 cols) ↔ `allColumns` (30 incl. `id`) ↔ `scanEntry` order ↔ DDL all align field-for-field; `toUnix`/`fromUnix` for timestamps; DSN pragmas (`busy_timeout`, `foreign_keys`, `synchronous`) + db-scoped WAL correct; batched deletes; parameterized queries — only robustness nit is **TRACE-4**. **Queue JSON** verified: atomic temp+fsync+rename writes; `recomputePending` restores `Pending`/`PendingArticles`/`BytesDownloaded`/`FileIdx`/`artIdx`; `Emitted` correctly left false on load (intentional re-dispatch semantics); orphaned job-file handling on load — one real gap → **TRACE-3**. **Config YAML** verified clean: `TestRoundTripDefault`/`TestRoundTripFixture` (load→save→load stable), `TestAllFlatConfigTagsAreSettable` (Set() reflection ↔ tags), `Redacted()` non-mutation confirmed. All three stores' existing tests pass.

---

## Suggested order of attack

1. ~~**SEC-1** (architectural auth gap) — everything else is secondary to this.~~ **Done** (PR #78) — see finding entry above.
2. ~~Quick security wins: **SEC-2** (reuse `Redacted()` in `get_config`), **SEC-3** (log redaction), **SEC-4/5** (debug + WS origin).~~ **SEC-2/SEC-5 done**; **SEC-3/SEC-4** still open — no longer blocked by SEC-1 landing (it's merged), pick up next.
3. ~~**TRACE-1** (data race) + **TRACE-3** (seed early-abort counters on load — small, clear correctness win).~~ **Both done.**
4. ~~Optimization quick wins: **OPT-1/2/13** (delete dead + fix deprecation), **OPT-10** (hoist ToLower), **OPT-4/5** (fsutil consolidation).~~ **All done.**
5. ~~**OPT-3** (decide fate of inert knobs), **OPT-6/8** (shared extractor helper), then the larger **OPT-7/9** decompositions. **OPT-12** (verify `DirectUnpackStatus` lock cost before batching), **SEC-6**, **TRACE-2**.~~ **All done** — see `## Batch review — audit-backlog-batch2` below.

### Batch review — audit-backlog-batch2 (2026-07-15)

Executed via `superpowers:subagent-driven-development` in an isolated worktree, one task per
commit group, each implemented by a fresh Sonnet subagent and independently reviewed (spec +
quality) by a second Sonnet subagent before the next task began — 9 tasks, 18 commits total
(`dc00511`..`4b8c096`). All nine were approved on first review pass (no fix-and-re-review
cycles needed), though several reviews required real independent verification, not a rubber
stamp:

- **SEC-3's implementer** caught and fixed a genuine bug in the plan's own reference code: a
  substring-based redaction loop was clobbering the `keyword` param's value (since "keyword"
  contains "key") before a keyword-indirected check could read it — order-of-operations bug,
  not a typo.
- **SEC-6's blast radius** was larger than planned: 6 pre-existing tests beyond the 2
  anticipated needed adapting to the new path-allowlist. The reviewer traced every one against
  its pre-refactor behavior to confirm each was a genuine setup fix, not a weakened assertion.
- **OPT-3** (the only genuine feature addition in this batch, not a bug fix or refactor) wired
  three previously-inert config knobs into the downloader's documented hot path. The reviewer
  independently re-traced the hot-path safety argument (is `Downloader.opts` truly write-once
  after `New()`?) and the `MaxArtOpt`/`clampPenalty` boundary conditions by hand rather than
  trusting the implementer's claims — both held up.
- **OPT-7** (decomposing `cmd/gonzbd/main.go`'s `serveMode`/`run`, the repo's own
  highest-churn file) was treated as the highest-risk task in the batch: reviewed against this
  file's own documented shutdown-ordering and `context.Canceled` hazards from `AGENTS.md`,
  both confirmed preserved bit-for-bit across all 10 extraction commits.

No fixes were required after any review — a first-review approval rate this repo's earlier
batches (PRs #75-78) did not achieve, though this batch's tasks were individually smaller and
better-scoped than that batch's largest items.

### Batch review — PRs #75, #76, #77 (merged 2026-07-15), #78 (merged 2026-07-16)

Delegated implementation to Sonnet agents (per-PR worktrees) for #75-77; #78 (SEC-1) was handled directly given its security-sensitive, cross-cutting nature. Opus orchestrated/verified every PR before merge — see individual finding entries above for commit hashes. All four PRs went through a full review cycle (CodeRabbit + manual/gremlins mutation-testing review) before merging; several additional bugs were found and fixed during review that weren't in the original audit findings:

- **TRACE-1's PR:** `queueChangeCat` returned a false 500 on concurrent job removal even though the change had already succeeded; the original race-test regression guard caught the underlying bug only 1/10 times under `-race -count=10` before being hardened.
- **OPT-4's PR:** `writeAtomic` leaked a file descriptor on a callback panic and had a chmod TOCTOU (path-based `os.Chmod` after close, instead of `fchmod` on the open descriptor).
- **SEC-1's PR (three review rounds):** IPv4-mapped CIDRs in `local_ranges` (e.g. `::ffff:192.168.1.0/120`) were silently accepted but could never match any peer, due to `netip.Prefix.Contains` requiring matching address families; and `--listen :PORT` (bare port, wildcard bind) silently bypassed the non-loopback startup warning, since the warning logic checked the raw config-file host, not the *effective* listener address after CLI overrides.

This reinforces the audit's own **Red-Green Discipline**: a regression test that passes against the buggy code on the first try is not evidence of anything. Every fix above required either an explicit revert-and-rerun proof, or (for the two hardest-to-force races/timing windows) an honest acknowledgment that the interleaving was too narrow to reproduce reliably via goroutine timing, and a deterministic unit test of the resulting logic instead.

**Post-merge repo cleanup (2026-07-16):** all four PR branches, their local worktree pointers, and the fully-superseded `audit-traceability-refactor` branch (both local and remote — verified every one of its "net-new" fixes not already tracked as a formal finding, e.g. the par2 packet-body OOM cap and the decoder scratch-buffer panic guard, is independently present on `main`) were deleted. `main` is the sole remaining ref; repo is clean for a fresh checkout on another machine.
