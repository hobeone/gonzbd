# GoNZBD Architecture & System Review — Synthesis

Repo `main` @ `9be19e24`, working tree clean throughout. 93 raw findings across 5 lanes,
deduped and ranked here. Detail lives in `lane1..lane5-*.md` alongside this file.

> **Frozen record.** This is a dated audit snapshot of `main` @ `9be19e24`, not
> a living contract. Several identifiers and schema columns it describes have
> since been removed by the download-durability work — `MarkArticlesDone`,
> `MarkArticlesDoneByIdx`, `MarkArticlesFailed`, `SetFileExtents`, `maxWritten`,
> `crcParts`/`crcValid`, and the `job_files.write_cursor` / `bytes_downloaded` /
> `max_written` columns. It is kept unedited as a record of what the code was
> and what the audit found. For the current contract see
> [`../durability-contract.md`](../durability-contract.md).

## Integration findings (synthesis-only — no single lane could see these)

### INT-1 — The 30s queue checkpoint durably records Done for bytes still in RAM
`internal/app/app.go:800-815` (`runCheckpoint`) × `internal/assembler/assembler.go:940-964`

> **Resolved in #355.** The ack now waits for the bytes: `writeArticleOrBuffer`
> returns a three-state outcome instead of a bool, and the write paths settle the
> articles they moved. The analysis below describes the code as it stood at the
> time of the audit and is kept unedited as a record.

`runCheckpoint` fires on a ticker (~30s), checks `app.queue.IsDirty()`, and calls
`app.queue.Save(...)`. It does **not** flush the assembler write cache first — `queue.Save`
takes a directory and has no assembler reference. Lane 1 found that `recordPendingDone`
fires as soon as `writeArticleOrBuffer` returns true, which happens when the article is
accepted into the in-memory `writeCache`, not when it is written.

Composing the two: every 30 seconds the queue can persist `done[i]=true` to SQLite for
articles whose bytes have never reached the kernel. Lane 1 rated this crash-only. It is
not — it is a periodic, designed-in exposure. Combined with Lane 1 Finding 4
(`writeCachedArticles` has no return value and swallows every `WriteAt` error on the
pressure-flush / drain / shutdown paths) the full failure chain is: bytes never written →
error swallowed → queue durably says Done → restart never re-dispatches → file has a hole
→ quickcheck reports `Mismatched` (corrupt) rather than `NoCRC` (incomplete download).
par2 is the only thing standing between this and silent corruption, and on-demand-par2
means the recovery volumes may not even have been fetched.

### INT-2 — `_FAILED_` retry writes into a fresh sparse file; the assembler does not object
`internal/postproc/stage_finalize.go:82-92` × `internal/assembler/assembler.go:860`

Lane 2 traced `folder_rename` → `_FAILED_<name>` rename → nothing ever reads the prefix
back → `ResetForRetry` resets only *failed* articles → retry re-downloads a fraction into
a fresh empty `<downloadDir>/<Name>`. It flagged as unresolved whether the assembler would
fail loudly on the missing/zero-length target. It does not:
`os.OpenFile(info.Path, os.O_WRONLY|os.O_CREATE, 0o644)` — no `O_EXCL`, no size check. The
assembler creates a new empty file and `WriteAt`s at absolute offsets, producing a sparse
file with holes exactly where the previously-`done` articles were. Silent, as feared.

### Cross-lane resolutions

- **Lane 1 Finding 12 is a non-bug.** Lane 3 confirms `SQLiteStore.Add` does write the
  manifest (`sqlite_store.go:172-178`), so `Queue.Add`'s `store == nil` branch is a
  confusing asymmetry, not a missing write. Downgrade to Nice-to-have.
- **Lane 1 Finding 2's blast radius is larger than the downloader.** `internal/app/pipeline.go:256`
  (`CheckEarlyAbort`) and `:401` (`CountUnfinishedArticles`) both call through with **no**
  nil guard. The panic family is reachable from the app pipeline, not just `dispatch.go`.
- **Lane 5 Finding 11's premise is correct and the shim is load-bearing.** The backend
  really does remap `general`→`misc` (`internal/api/config.go:20,26`). Not dead code.
- **Lane 1 Q3 answered by Lane 4 Finding 10**: documented shutdown ordering holds exactly.
  So INT-1 is the *periodic* path, not the shutdown path.

---

## CRITICAL

**C1. Nil-pointer panic on pause — process crash, common path.**
`internal/queue/queue.go:1122-1141` ← `internal/downloader/dispatch.go:553,562`; siblings at
`queue.go:1087-1100`, `:187-211`, `progress.go:469` ← `internal/app/pipeline.go:256,401`.
`Pause` sets status and evicts (nils `manifest`/`progress`) in one critical section
(`queue.go:427,431`), so any observer seeing `StatusPaused` already sees nil. The
by-messageID method family dereferences unguarded; the `…ByIdx` family guards
(`queue.go:1151`). Every article in `workCh` at pause time takes this path. No `recover()`
in workers by deliberate project decision ⇒ daemon dies. Lens 1+2. **Effort S** for guards,
**M** for a systematic "every exported Queue method against an evicted job" test.
*Root cause: lazy eviction made these fields nil-able and the guard sweep covered only half
the API. Fix the class, not the four sites.*

**C2. Done means "in RAM", not "on disk" — and the checkpoint persists it.**
See INT-1. Lane 1 Findings 3/4/5 + `app.go:800-815`. The invariant is documented in two
places the code doesn't keep (`dispatch.go:688-695`, go-standards B.6). Three sub-parts:
ack decoupled from write; `writeCachedArticles` swallows errors; `flushRun` failure demotes
only the triggering article while the rest of a coalesced run stays Done. Lens 2.
**Effort M** (fix as one unit — they share a root cause).

**C3. Five destructive post-processing stages run on failed jobs with no error gate.**
`docs/ARCHITECTURE.md:138` claims each stage self-gates on `ParError`/`UnpackError`/`FailMsg`.
Only 4 of 12 do. `rar_volume_recovery`, `sample_cleanup`, `recover_par2_names`,
`deobfuscate`, `extension_cleanup` rename and delete unconditionally — and all run *before*
`finalize`/`cleanup` announce they are "preserving files for retry"
(`stage_finalize.go:93`, `stage_cleanup.go:37`). The state being preserved has already been
mutated by four stages. Lens 1+2. **Effort M** — make the gate a property of the stage
descriptor consulted by `runStage`, not 12 hand-written `if`s (this also kills the two
`switch stage.Name()` sites in Lane 2 Finding 9).

**C4. `Get()` silently deletes an active job when its manifest is unreadable.**
`internal/queue/sqlite_store.go:227-259`. Three paths call `s.Remove()` as a side effect of
a *read*. No history entry, no fail message — the download vanishes. Bad shutdown mid-
`writeGzJSON` or a disk error is precisely what this path exists to handle, and it answers
with data loss. Lens 2. **Effort M** — route through the existing
`prepareClaimFailureLocked`/`finishClaimFailure` pattern so it lands in history as Failed.

**C5. One subprocess escapes every containment layer.**
`internal/rarheader/rarheader.go:381-407`: bare `exec.Command("unrar","vt",...)` — no
context, no timeout, no `bwrap`, no nice/ionice, unbounded `bytes.Buffer`, hardcoded binary
ignoring `UnrarCommand`/`GONZBD_UNRAR_BIN`. Reachable from the deobfuscate stage for *every*
RAR-magic file. Job cancel and daemon shutdown cannot interrupt it, and `strict_sandbox:
true` silently does not apply — a documented guarantee with a hole. Lens 1+3 (archive data
is untrusted remote input, so this is the legitimate-defence side of the lens).
**Effort M** (signature change ripples into `deobfuscate`).

**C6. `_FAILED_` retry produces sparse files.** See INT-2. Opt-in (`folder_rename` defaults
false) is the only reason this isn't louder. **Effort S** to strip the prefix on retry-dir
computation; **M** to do it with a test proving partial bytes are recovered.

---

## IMPORTANT

**I1.** No `cmd.WaitDelay` anywhere in postproc (`unrar.go:221`, `sevenzip.go:117`,
`par2.go:367,431`, `script.go:288`). With `LineStreamer` on stdout/stderr, `Wait()` blocks
indefinitely on a surviving grandchild ⇒ `PostProcessor.Stop()` never returns ⇒ daemon
cannot shut down. `internal/notifier/script.go:80` already sets it — the inconsistency is
the tell. Mitigated by `bwrap --die-with-parent`, which is **absent by design in Docker**.
**Effort S** — set it at the `cmdutil.BuildSandboxedCommand` chokepoint.

**I2.** `queue` response hardcodes `speed`/`kbpersec` to `"0"` and `timeleft` to `"0:00:00"`
(`internal/api/queue.go:477-478,483`) while the real value is computed at `:418-421` and fed
to per-slot ETAs. Third-party dashboards read exactly these fields. **Effort S.**

**I3.** `queue&name=change_complete_action` returns `{"status":true}` unconditionally with no
state stored anywhere (`internal/api/queue.go:93-95`). The worst of the three options —
its neighbours `sort`/`delete_nzf` honestly 501. **Effort S** to make it honest.

**I4.** `ServerConfig`/`CategoryConfig` round-trip through hand-mirrored TS types and are
saved as whole-array replacement (`SettingsDialog.svelte:126,147`). A Go-side field this UI
doesn't know about is **silently dropped on save** — data loss, not a display bug.
`FullConfig.general`/`.postproc` are `Record<string, any>`, so the entire settings surface
has zero compile-time drift protection. **Effort M.**

**I5.** Four raw `root.Remove`/`os.Remove` on job cleanup paths bypass the NFS silly-rename
protocol go-standards §5 mandates (`extension_cleanup.go:130,170`, `sample_cleanup.go:112`,
`unpack/passwords.go:192`; plus `app/job_finalizer.go:66,77`). No `fsutil.RemoveRoot`
single-file helper exists — that absence is probably why. **Effort S** (add the helper).

**I6.** `rar_volume_recovery` is not PP-gated (`postproc.go:646-655`), so a PP=0
"download only" job gets its filenames rewritten to `<Name>.partNNN.rar` with no unpack
ever happening, unrecoverably. **Effort S.**

**I7.** `checkExpiredPenalties` takes a **write** lock on every server on every dispatch
wake (`downloader.go:726-737`), and `signalDispatch` fires per article completion (~330/s at
target throughput), contending the same mutex `Active()` reads per-article. Exactly the
shape go-standards §7 catalogues. **Effort S.**

**I8.** Config `Set()` mutates live state, then a failed `Save()` returns 500 with no
rollback (`internal/api/config.go:163-175`, second occurrence at `:338-341`) — a restart
silently reverts. The field-level rollback machinery already exists in `SetLocked`.
**Effort S.**

**I9.** `fetchJSON` returns a never-settling promise on auth-expiry (`ui/src/lib/api.ts:54-58`),
so the `finally` that clears `#pollInFlight` never runs — the store wedges permanently.
Masked only by a 1.5s redirect timer. Also strands `SettingsDialog.saveAll`'s await-loop
with `saving` stuck true. **Effort S.**

**I10.** `CheckContainment` treats a **dangling symlink** as a traversal attack
(`fsutil/containment.go:40-45`) — `EvalSymlinks` returns ENOENT, the job is flagged
`UnpackError` and the extraction output is deleted. A legitimate release with a relative
symlink is misclassified. The codebase already knows the lexical-vs-resolved distinction
(`cleanupContainmentViolation` gets it right). **Effort S.**

**I11. Documentation has drifted far enough to actively mislead.** Four confirmed:
`queue.db` does not exist — one `history.db` holds both (`ARCHITECTURE.md:87-88`,
`sabnzbd_spec.md:103,287,288,295,1265`); "byte-for-byte compatible" can only be true of the
`history` *table*, not the file (`ARCHITECTURE.md:178`); 12 stages not 11, and `par2names`
is really `recover_par2_names` (`ARCHITECTURE.md:121-134`); the stage-gating invariant is
false (C3). **This is load-bearing, not cosmetic** — the single-file design is what makes
`MoveToHistory`'s atomicity real, and a future "fix" that split the files per the docs would
silently destroy a correctness property nobody wrote down. **Effort S.**

**I12. Dead / duplicated plumbing that widens the blast radius of C1 and C2.**
The DNS-resolution subsystem has no caller and `nntp.Dial` never consults it
(`downloader/resolver.go`, `server.go:48-56,133-158`); the assembler's four-callback ack API
has a message-ID branch that is unreachable in production yet is the family with the nil
bugs (`assembler.go:162-183` + `app.go:321-333`); `DecodeArticleBuf`'s `scratch` parameter
has no caller and is the sole reason `decoder` imports `unsafe`; the legacy JSON-only queue
persistence engine (~250 lines + tests) is unreachable from either entry point
(`queue/persistence.go`). **Effort S–M each.** *The legacy engine is a decision, not a
cleanup — see Open Decisions.*

---

## NICE-TO-HAVE

Grouped; all have `file:line` evidence in the lane reports.

- **Hot-path polish**: `hasActiveConnections` map-walk per idle pass (`downloader.go:603`);
  `sub42Span`'s grow path allocates a zeroed throwaway instead of `slices.Grow`
  (`decoder.go:309`); `serverMask.count()` hand-rolls popcount over `bits.OnesCount64`
  (`mask.go:76`); `GetBuffer` discards undersized pooled buffers (`decoder.go:126`).
- **Resource hygiene**: `handleFatalArticle`'s early returns skip `PutBuffer`
  (`assembler.go:770`); `flushPressure`/`flushWriteCache` drop drained articles without
  `PutBuffer` on the unknown-file branch (`assembler.go:1246,1280`); `directunpack`'s volume
  feeder hands off `*os.File` without the Tracking-Slice defer (`directunpack.go:584`).
- **Complexity concentrations (no bug found)**: `PromoteNext`'s three-phase
  lock-release-reacquire with a shadow `promoting` map (`queue.go:499-602`) — the place a
  future change is most likely to introduce one; postproc's `Job` as a 20-field mutable bag
  where `finalize` rewrites `DownloadDir` out from under later stages (`stages.go:695-810`);
  the `Stage` interface earning little (fixed order, one construction site, two
  `switch stage.Name()` sites, `builtStages` handing back concrete pointers to un-do it).
- **Correctness nits**: eight `_ = q.store.Update(...)` swallow errors with no comment
  (`queue.go:364,596,733,826,854,929,949,968`); `ActiveSet`'s inner mutex is only ever taken
  under `q.mu` (`active_set.go`); `par2` passes `slog.Default()` into
  `BuildSandboxedCommand` so the sandbox-degradation warning is unscoped and dropped by
  component filtering (`par2.go:367,431`); hardcoded `"__ADMIN__"` duplicating
  `constants.JobAdminDirName` (`stage_cleanup.go:41`); `LineStreamer`'s partial-line buffer
  unbounded on `\r` progress output (`linestreamer.go:33`); missing `--` end-of-options
  separator on extraction argv; `PostProcessor.History()` shallow-copies the Job's maps
  (safe today, doc comment describes the racy intent).
- **Stale comments**: `ApplyPenalty`'s doc describes a `now` parameter it doesn't have
  (`server.go:88`); three references to `popWithPause`, renamed to `popJob`
  (`postproc.go:73,242,284`); `set_apikey`/`set_nzbkey`'s "Not in spec" comment is factually
  wrong — the spec lists both (`config.go:288`).
- **Frontend**: rename the vestigial polling vocabulary (`BasePollStore`, `isPolling`,
  `startPolling`) — nothing polls on a timer; double-poll after actions (explicit + WS);
  inert `$state.raw` "optimistic updates" in `QueueRow.svelte:90,100,308,325`; three
  independent byte/speed formatters; no-op `onpausetoggle`; `WarningStore` hand-duplicating
  `BasePollStore`; untyped `(res as any).result` duplicated in two dialogs.
- **API coverage gaps that fail honestly (400, not lying)**: `switch`, `get_files`,
  `move_nzf_bulk`, `retry`, `cancel_pp`, `showlog`, `gc_stats`, `set_config_default`,
  `regenerate_certs`. `output=xml` is unimplemented and silently returns JSON. `fullstatus`
  returns 4 fields against a spec promising "all queue, server, stats".
- **Ops**: unconditional `VACUUM` on every `history.Open` (`history/db.go:83`) now rewrites
  queue state too; `WriteAtomic` omits the parent-directory fsync; `knip` (`check:unused`)
  is wired in `package.json` but not in the documented quality gates.

---

## Considered and excluded — correct for this deployment, do not "fix"

- **`percentage` as a JSON number** where the spec documents a string. Deliberate, pinned by
  `TestQueueSlot_SonarrPercentageIsInt`, verified against real Sonarr (C# int). **The spec
  doc is stale, not the code.** This is the one place observed client reality beats the spec.
- **CSRF machinery, cookie-to-trusted-source binding, ephemeral session key vs permanent API
  key.** A logged-in browser is a real CSRF target with exactly one user. Correctly scoped to
  the cookie path only — API-key callers (Sonarr) never hit it. Keep.
- **API-key vs NZB-key tiering.** Scoped-credential pattern, not RBAC: an automation tool
  needs upload access, not admin. Not removable — third-party clients depend on it.
- **Bounds checking against hostile NNTP servers and archives** — `maxResponseLineLen`,
  `maxBodySize`, `maxDecodeSize`, `os.Root` usage, `SanitizedPath`, `writeEntrySafely`,
  `cleanupContainmentViolation`. Untrusted *remote data* is real here even though untrusted
  *local users* are not. This is the best code in the repo; do not trim as over-engineering.
- **No in-app login / user management / RBAC** — reverse proxy's job, by design.
- **No `recover()` in worker goroutines** — researched and deliberately rejected. C1 is a
  bug to fix at the nil deref, not a case for a panic boundary.
- **SQLite / gzip-JSON manifests / YAML / pure-Go driver** — no correctness problem found
  that would justify revisiting. The single-file SQLite design is *better* than the two-file
  design the docs describe.
- **UI pagination over virtualization; no optimistic-concurrency or ETag handling** —
  correctly lean for one user.
- **Sorting/renaming not implemented** — deliberate; external tools own it.
- **`Config.Validate()` doing no filesystem checks** — deliberate layering (`validate.go:13-21`).

---

## Open decisions (yours, not mine)

1. **The legacy JSON-only queue persistence engine** (~250 lines + test file, unreachable):
   deliberate escape hatch for a future `--no-db` mode, or delete?
2. **`change_complete_action`**: honest 501, or implement for real?
3. **`set_apikey`/`set_nzbkey`**: implement, or accept as documented gap given direct YAML
   access? (Single self-administered instance.)
4. **Generated TS types from Go structs** — the hand-mirrored list is long enough that I4 is
   a symptom, not a one-off. Worth a build step, or keep manual with a shape contract test?
5. **PP=0 semantics for `deobfuscate`/`sample_cleanup`/`extension_cleanup`** — product
   decision. Today `default: return false` means PP=0 still renames and deletes.
