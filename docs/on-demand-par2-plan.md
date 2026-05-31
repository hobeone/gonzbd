# On-Demand Par2 Download — Overview & Implementation Plan

**Status:** Proposal / initial plan (no code written yet)
**Author:** drafted with Claude
**Audience:** human reviewers + LLM agents contributing to this feature

## 1. Goal

Avoid downloading par2 **recovery volumes** (the `*.volNNN+MM.par2` files,
which can be 5–15% of a job's bytes) unless the download is actually damaged
and needs repair. This mirrors SABnzbd's long-standing behaviour ("only get
par2 when needed") and saves bandwidth on the common case where every article
arrives intact.

Requirements from the request:

1. **Configurable** — a setting to turn the behaviour on/off.
2. **Surfaced in the UI** — clearly articulated in both the **queue** (while a
   job is downloading) and the **history** (after a job finishes).

## 2. Why gonzbd can do this cleanly

gonzbd already has the decision oracle. It does **not** need SABnzbd's full
block-promotion machinery to decide whether repair is needed:

- The **index** par2 file (`movie.par2`, no `.volNNN+MM`) carries the per-file
  CRC32 and 16k-hash for every protected file (`par2.ParseFileDescriptions`).
- The assembler already computes and persists an **assembled CRC32** per file
  (`JobFile.AssembledCRC32`, set via `SetFileCRC32` on file completion).
- `verifycrc.VerifyCRCs(files, sets, log)` already compares the two and returns
  `Checked / Matched / Mismatched / NoCRC / Unverified` — today it runs as the
  QuickCheck post-processing stage.

So: with **only the index par2 downloaded**, we can already tell whether the
data is intact. Recovery volumes are needed **only** when verification fails.
That is the entire bandwidth win, and it rests on code that already exists.

### Decision rule (reused, not invented)

```
repairNeeded := result.Mismatched > 0 || result.NoCRC > 0 || result.Unverified > 0
```

`NoCRC` covers files whose articles failed during download (no assembled CRC);
`Unverified` covers par2-tracked files with no matching assembled file;
`Mismatched` covers corruption. All-`Matched` ⇒ skip recovery volumes.

## 3. Architectural reframe — the key decision

The naive design re-enters the pipeline mid-repair (post-processing asks the
downloader for more files). **We will not do that.** Instead, run the CRC
verification at **download-complete, before post-processing**, so the back-edge
is **download → download**, never **postproc → download**:

```
                 ┌─────────────────────────────────────────────┐
                 ▼                                             │ (damage:
download articles + INDEX par2  ──►  all non-deferred files     │  un-defer
(recovery volumes deferred)          complete?                  │  recovery
                 │                        │ yes                  │  volumes)
                 │                        ▼                      │
                 │              run VerifyCRCs (index par2)      │
                 │                        │                      │
                 │            ┌───────────┴───────────┐          │
                 │       clean│                       │damage    │
                 │            ▼                       └──────────┘
                 │     finalize as today          (job incomplete again →
                 │     (skip recovery volumes)     normal downloader fetches
                 │                                  deferred volumes → next
                 ▼                                  completion runs postproc)
            post-processing (repair has volumes when it needs them)
```

Benefits:

- No PP worker is blocked waiting on network I/O.
- No un-completing / re-finalizing a job already handed to post-processing
  (avoids the partially-initialized-struct and double-enqueue hazards called
  out in `CLAUDE.md`).
- Reuses the **entire** existing downloader → assembler → completion path to
  fetch the recovery volumes; repair then runs exactly as it does today.

The one refactor cost: `VerifyCRCs` currently runs inside post-processing. We
lift the *index-par2 parse + verify* into a download-complete hook. It is
already an almost-pure function (`files, sets, log`), so this is tractable.

## 4. Phasing

> **Status: Phase 1 shipped. Phase 2 evaluated and deliberately not built — see
> §12 for the rationale.** The Phase 2 material below is retained as a design
> record in case a future workload justifies revisiting it.

Phase 1 was landed and tested on its own so the state-machine changes were
de-risked independently of any block-exact selection work.

### Phase 1 (foundation)

- Config flag.
- Defer recovery volumes at add-time.
- Downloader skips deferred articles; they are excluded from completion.
- CRC verify at download-complete.
- **On any damage: un-defer and fetch ALL recovery volumes** (degrade exactly
  to today's "download everything" behaviour), then post-process as today.
- **Proactive early un-defer** (see §8.7): un-defer recovery volumes the moment
  a data article *permanently fails during download*, not only at completion —
  this minimises the window in which recovery volumes can age off the server.
- UI in queue + history.

This delivers the win on the common clean case with near-zero state-machine
risk: either we skip all recovery volumes, or we fetch all of them.

### Phase 2 (follow-up, not required for v1)

- **Block-exact selection**: greedily pick recovery volumes by their encoded
  block count (`.volNNN+MM` → `MM` blocks) until the total ≥ blocks needed,
  instead of fetching all of them.
- **Incremental top-up loop**: if the first batch of recovery blocks still
  isn't enough, par2 reports "You need N more blocks." The hooks already
  exist — `Job.NeedRequeue`, `Job.RequeueBlocksNeeded`, `Job.RequeueReason`
  in `internal/postproc/stages.go`, and `needMoreBlocksRE` in
  `internal/par2/par2.go`. Phase 2 wires these to a second un-defer pass.

## 5. Data model changes

### `internal/queue/job.go` — `JobFile`

Add **persisted** fields (queue state is persisted per-NzbObject as JSON+gzip,
so these survive restart — unlike the `json:"-"` transient counters):

```go
// IsPar2Recovery marks a par2 recovery volume (*.volNNN+MM.par2), as
// opposed to the par2 index file. Set at add-time.
IsPar2Recovery bool `json:"is_par2_recovery,omitempty"`

// Deferred marks a file whose articles are intentionally NOT dispatched
// yet (on-demand par2: recovery volumes held back until repair needs
// them). Cleared by the verifier when damage is detected. Persisted so a
// restart remembers which volumes were held.
Deferred bool `json:"deferred,omitempty"`
```

### Critical invariant — `recomputePending` must exclude deferred articles

`recomputePending` (job.go:242) and the per-file `Pending` accounting must
**not count deferred files**, or the job never reaches "download complete":

```go
for fi := range j.Files {
    if j.Files[fi].Deferred {
        j.Files[fi].Pending = 0          // not pending; held back
        // BytesDownloaded still computed for UI, but contributes 0 work
        continue
    }
    // ... existing per-article counting ...
}
```

When the verifier un-defers volumes, it must `recomputePending()` (the
self-healing reset) so `PendingArticles` and `RemainingBytes` reflect the
newly-active work. `ClearAllEmitted`/`recomputePending` is the canonical
"rebuild counters from ground truth" path per `CLAUDE.md`.

### API seam — design for Phase 2 from day one

To keep Phase 2 (block-exact selection) **purely additive**, the un-defer
mutation takes an explicit set of file indices from the start:

```go
// UndeferRecoveryVolumes clears Deferred on the given recovery-volume file
// indices, re-activating them for dispatch, and recomputes pending counters.
func (q *Queue) UndeferRecoveryVolumes(jobID string, fileIdxs []int) error
```

- **Phase 1 caller** passes *all* deferred recovery-volume indices (a tiny
  `DeferredRecoveryIndices(jobID)` helper, or "all where `IsPar2Recovery &&
  Deferred`").
- **Phase 2 caller** passes the greedily-selected subset that covers the
  needed block count.

The queue-side mutation, counter recomputation, completion logic, and the
`Par2Recovered` guard are identical for both — so Phase 2 changes only the
*caller-side selection*, never the state machine. This is the seam that makes
"build in two phases, back-to-back" cost no rework: Phase 1's "fetch all"
remains the permanent fallback (index par2 damaged, names unparseable, or the
Phase 2 top-up loop exhausted), invoked by passing the full index set.

### Add-time classification — `internal/queue/job.go` (NewJob, ~line 448)

Today: `isPar2 := strings.Contains(strings.ToLower(pf.Subject), ".par2")`.
Refine to distinguish index vs recovery using the existing volume pattern
(expose `par2.IsRecoveryVolume(name)` from the unexported `isVolume`/
`volPattern` in `internal/par2/par2.go`):

```go
isPar2 := strings.Contains(lower, ".par2")
isRecovery := isPar2 && par2.IsRecoveryVolume(pf.Subject)
jf.IsPar2Recovery = isRecovery
if isRecovery && cfg.OnDemandPar2 {
    jf.Deferred = true
}
```

The **index** par2 is always downloaded (we need its CRCs to verify).
`RemainingBytes`/`TotalBytes` accounting: keep `TotalBytes` as the true NZB
total (UI shows real size), but ensure deferred bytes are not counted as
"remaining work" for completion (see `recomputePending`). `Par2Bytes` still
sums all par2 (see §8.2 for the health-gate caveat).

## 6. Downloader changes

`internal/queue/queue.go` — `ForEachUnfinishedArticle` (queue.go:503) is the
single dispatch funnel. Add a deferred skip alongside the existing
`Complete || Pending == 0` skip:

```go
if file.Complete || file.Pending == 0 || file.Deferred {
    continue
}
```

Because `Deferred` files have `Pending == 0` (from §5), the existing check
already skips them — but make `Deferred` explicit for clarity and to guard
against future counter drift. No hot-path cost (one bool check per file, not
per article).

## 7. The verification hook (download-complete)

`internal/app/app.go` — `handleFileComplete` (app.go:913) currently does:

```go
snap := app.queue.SnapshotJob(fc.JobID)
if snap != nil && snap.IsComplete() {
    app.maybeFinalize(fc.JobID, failMsgForJob(snap))
}
```

`IsComplete()` (job.go:475) returns true when all **non-deferred** files are
`Complete` (deferred files are skipped — adjust `IsComplete` to ignore
`Deferred` files). Insert the on-demand gate between completion and finalize:

```go
if snap != nil && snap.IsComplete() {
    if app.cfg.OnDemandPar2 && snap.HasDeferredPar2() {
        if app.par2NeedsRecovery(snap) {       // runs VerifyCRCs on index par2
            // Phase 1: pass ALL deferred recovery indices. Phase 2 passes a
            // block-covering subset; same method, different selection.
            app.queue.UndeferRecoveryVolumes(fc.JobID, snap.DeferredRecoveryIndices())
            app.emitter.Broadcast(Event{Type: "queue_updated"})
            return // job is incomplete again; downloader fetches volumes
        }
        // clean: drop the deferred volumes from the work set permanently
        app.queue.DiscardDeferredPar2(fc.JobID)
    }
    app.maybeFinalize(fc.JobID, failMsgForJob(snap))
}
```

`par2NeedsRecovery` builds `[]verifycrc.AssembledFile` from the snapshot's
non-par2 files (`Subject`, `AssembledCRC32`, `Bytes`) and the par2 `Set`s found
in the download dir (index only, which is on disk), calls
`verifycrc.VerifyCRCs`, and applies the decision rule from §2.

**Idempotency / loop guard:** record on the job that recovery has already been
un-deferred (`Job.Par2Recovered bool`, persisted) so a second completion pass
(after the volumes arrive) does **not** re-verify-and-redefer. Second pass:
`HasDeferredPar2()` is false ⇒ straight to `maybeFinalize` ⇒ post-processing
repair runs with volumes present.

## 8. Edge cases & cross-effects (must be handled, not assumed)

1. **Index par2 itself failed to download** ⇒ we cannot verify. Fallback:
   treat as "damage" and un-defer all recovery volumes (today's behaviour). One
   branch in `par2NeedsRecovery` (no usable sets ⇒ return true).
2. **No par2 at all** ⇒ `OnDemandPar2` is a no-op; nothing deferred.
3. **`Par2Bytes` health-gate cross-effect** — `CLAUDE.md` notes `Par2Bytes`
   feeds "maximum repair capacity" and the hopeless-job / early-abort gate
   (`UnfinishedArticle.Par2Bytes`, `IsEarlyAbort`). With recovery volumes
   deferred, fewer par2 bytes are in flight. **Action: verify the early-abort
   and hopeless-job math still behaves when par2 is deferred** (it keys off
   first-article failure rate, which is data-article dominated, so likely fine
   — but confirm, don't assume).
4. **Recovery volumes aging out (the principal new risk).** The
   recoverable-vs-unrecoverable *verdict* is unchanged: a "clean" CRC result
   means every protected file's assembled CRC32 matches the par2-recorded
   CRC32 (the same checksum par2 verifies), so skipping volumes on a clean
   result can never hide damage; and whether a *damaged* job is repairable has
   always been determined at the full par2 repair stage, today and after this
   change alike. The genuinely new exposure is **temporal**: with upfront
   download the recovery volumes are grabbed while fresh; with on-demand they
   are fetched *later*, so a job recoverable at download time can become
   unrecoverable by the time we fetch the volumes if they have aged off / been
   removed in the interim. Mitigations: (a) Phase 1 fetches **all** volumes in
   one pass on first damage — no trickle; (b) **proactive early un-defer**
   (§8.7) closes most of the window whenever damage is already evident during
   download. Note Phase 2's incremental top-up loop *increases* this exposure
   (each extra round-trip ages the remaining volumes), so it must keep the
   batches large and the loop tight.
5. **DirectUnpack** (`maybeDirectUnpack`) operates on completed RAR volumes
   during download; unaffected (par2 volumes aren't RAR parts).
6. **Resume after restart** mid-defer: `Deferred`/`IsPar2Recovery`/
   `Par2Recovered` persist; `recomputePending` on load rebuilds counters
   honouring `Deferred`.

7. **Proactive early un-defer (timing mitigation).** Independently of the
   download-complete verify, watch for the *first permanent data-article
   failure* and un-defer the recovery volumes immediately. The hook is
   `MarkArticlesFailed` in `internal/queue/queue.go` (where `FailedBytes` is
   incremented) / the early-abort accounting in `handleFileComplete`: when a
   job with `OnDemandPar2` deferred volumes records its first failed **non-par2**
   article, call `UndeferRecoveryVolumes` and `recomputePending`. This fetches
   the volumes while the connection is live and the articles are freshest,
   rather than after a full download pass + verify. It also subsumes the
   common case: if there was any failure, we were going to need the volumes
   anyway; if there was none, the download-complete verify still gates them.
   Guard with `Par2Recovered` so it fires once.

## 9. Configuration

`internal/config/downloads.go` — add to `DownloadConfig`:

```go
// OnDemandPar2 defers par2 recovery volumes (*.volNNN+MM.par2) and only
// downloads them if CRC verification shows the download needs repair.
// The par2 index file is always downloaded. Saves bandwidth on intact
// downloads. Default: true (on).
OnDemandPar2 bool `yaml:"on_demand_par2" json:"on_demand_par2"`
```

- Surfaced in the Settings UI (Downloads section) via the existing
  reflection-based config Get/Set + a `ConfigSwitch`.
- **Default: ON.** The bandwidth saving applies to the common (intact) case and
  the early-undefer mitigation (§8.7) keeps the timing risk small.
- **How "default true" is realised (no special-casing needed):** the loader
  (`internal/config/loader.go:60-71`) pre-seeds the struct with `Default()` and
  then decodes the YAML *into* it. So setting `OnDemandPar2: true` in
  `config.Default()` (`defaults.go`) means: a config file that lacks the key
  inherits `true`, while a file with an explicit `on_demand_par2: false` keeps
  `false`. Existing user configs therefore opt into the new behaviour on
  upgrade unless they have explicitly disabled it — which is the intended
  "default on" semantics, achieved without a pointer-bool or a migration step.

## 10. UI — queue (in-progress)

Reuse the per-file drawer built this session.

- **Backend** `internal/api/queue.go`: `fileState` (queue.go:217) gains a
  `"held"` state for `Deferred && !Complete` files. `buildQueueFiles` already
  emits per-file `State`.
- **Frontend** `ui/src/lib/components/QueueRow.svelte`: `fileStateColor` maps
  `"held"` to a muted/slate colour; the per-file row shows e.g.
  *"par2 (held — fetched only if repair is needed)"*.
- **Row-level badge**: when a job has deferred par2, show a small "par2 on
  demand" chip near the health indicator (`healthLabel`), so it's visible
  without expanding.

## 11. UI — history (after completion)

Reuse the per-file completion section added this session in
`internal/postproc/filelist.go` (`buildFileCompletionLines`). Add one summary
line to the synthetic **Download** stage, e.g.:

- Clean: `✓ Par2: verified clean from index — N recovery volume(s) skipped (saved 312 MiB)`
- Repaired: `⚠ Par2: fetched N recovery volume(s) for repair (X MiB)`

These lines already get colour from the `stageLineClass` helper fixed this
session (`✓` → green, `⚠` → amber). The history entry should also record a
machine-readable summary for potential columns later (optional Phase 2).

Persisted on the history `Entry` (optional, for richer UI): a small struct or
counts (`Par2VolumesSkipped`, `Par2VolumesFetched`, `Par2BytesSaved`). For v1,
the stage-log line is sufficient and requires no migration.

## 12. Decisions (resolved)

- **Default: ON.** Recovery volumes are deferred by default; users opt out with
  `on_demand_par2: false`. Realised via `config.Default()` + the loader's
  seed-then-decode pattern (§9).
- **Index-par2-missing reliability gap is accepted**, mitigated by the
  fall-back-to-all-volumes branch (§8.1) and proactive early un-defer (§8.7).
- **Phase 1 shipped; Phase 2 (block-exact) evaluated and deliberately NOT
  built.** During implementation it became clear Phase 2's value is marginal
  and conflicts with a Phase 1 safety property:
  - Phase 1 already captures the dominant win — the common case is a *clean*
    download, which now skips **all** recovery volumes.
  - Block-exact "just enough" only helps *damaged* downloads (the minority),
    and the most common damage cause (failed/missing articles) is already
    handled by the early un-defer (§8.7) fetching **all** volumes to minimise
    the aging window.
  - So block-exact only bites on the rare "all articles arrived but a file's
    CRC is wrong" case — unless the on-failure path is made to fetch "just
    enough," which trades away the aging safety §8.7 provides.
  Decision (user, after seeing this interaction): **stop at Phase 1.** If a
  future workload proves to be dominated by silent corruption rather than
  article failures, revisit — and if so, keep it download→download (size N from
  `ParsePar2Set.SliceSize`, select a covering subset, fall back to un-defer-all
  on any uncertainty) and do **not** add a postproc→download top-up loop.

## 13. Implementation order

### Phase 1 commits

1. `feat(par2)`: export `IsRecoveryVolume`; unit tests for index vs volume names.
2. `feat(config)`: add `DownloadConfig.OnDemandPar2` (default **true** in
   `config.Default()`) + validation + settings UI switch.
3. `feat(queue)`: `JobFile.IsPar2Recovery`/`Deferred` + add-time classification +
   `recomputePending`/`IsComplete` honour `Deferred` +
   `UndeferRecoveryVolumes(jobID, fileIdxs []int)` (selection from day one, §5)
   + `DeferredRecoveryIndices`/`DiscardDeferredPar2`/`HasDeferredPar2` +
   `Job.Par2Recovered`. Tests:
   pending-counter correctness with deferred files (extend `pending_test.go`'s
   `verifyPending`).
4. `refactor(verifycrc)`: ensure `VerifyCRCs` is callable from a download-complete
   hook (it already is; add a thin `app`-side adapter `par2NeedsRecovery`).
5. `feat(app)`: wire the download-complete gate in `handleFileComplete`
   (verify → un-defer-all-or-discard → finalize), with the idempotency guard.
   Tests: clean job skips volumes & finalizes; damaged job un-defers, refetches,
   finalizes via postproc; index-par2-missing fallback; restart mid-defer.
6. `feat(app)`: **proactive early un-defer** (§8.7) on first permanent
   data-article failure. Tests: a mid-download failure un-defers volumes before
   completion; guarded by `Par2Recovered` so it fires once.
7. `feat(ui)`: queue `"held"` state + chip; history summary line.
8. `docs`: update `ARCHITECTURE.md` (par2/completion sections) and
   `sabnzbd_spec.md` if behaviour is spec-relevant.

### Phase 2 commits (NOT built — see §12; retained as a design record)

9. `feat(par2)`: parse `volNNN+MM` block counts; greedy volume selection to
   cover a target block count.
10. `feat(app/queue)`: un-defer only the selected volumes; wire the
    `NeedRequeue`/`RequeueBlocksNeeded` top-up loop for "first batch wasn't
    enough" (keep batches large, loop tight — §8.4 aging note).
11. `feat(ui)`: history shows exact volumes/blocks fetched and bytes saved.

## 14. Testing strategy

- **Unit**: `IsRecoveryVolume`; `recomputePending` with deferred files;
  the `par2NeedsRecovery` decision rule (table: all-matched→false; each of
  mismatch/no-crc/unverified→true; no-sets→true).
- **Queue**: deferred articles never yielded by `ForEachUnfinishedArticle`;
  `UndeferRecoveryVolumes` restores correct `Pending`/`RemainingBytes`.
- **Integration** (`test/integration/`, `//go:build integration`): end-to-end
  with a fake NNTP server — (a) clean download skips recovery volumes; (b)
  inject a failed data article → recovery volumes fetched → repair succeeds.
- **`-race`** on all app-level tests (the completion hook touches shared state).

## 15. Risks summary

| Risk | Mitigation |
|---|---|
| Counter drift (deferred not excluded) → job hangs "downloading" | `recomputePending` excludes deferred; `verifyPending` test asserts it |
| Re-defer loop after volumes arrive | `Job.Par2Recovered` idempotency guard |
| Index par2 missing → can't verify | Fallback: un-defer all (today's behaviour) |
| Health-gate misfire with fewer par2 bytes | Verify `IsEarlyAbort`/hopeless math; data-article dominated |
| UI hides that par2 was skipped | Explicit queue chip + history summary line (requirement) |
| Persistence/restart loses defer state | Fields are persisted JSON (not `json:"-"`) |
