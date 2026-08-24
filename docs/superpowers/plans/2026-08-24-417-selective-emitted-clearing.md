# #417 — selective Emitted clearing on reload (rev 2)

**Goal.** `ReloadDownloader` must not clear the **Emitted** bits of a job whose
checkpoint failed to protect its written-but-unacked articles.

**Architecture.** The information already exists where it is needed —
`checkpointJob` knows whether its barrier protected the job — and is discarded.
The change stops discarding it and threads it to the one caller that must
branch. Nothing new is persisted; no new concept.

**Rev 2 corrections** (both blocking, from plan review round 1):
- The empty-`Files()` exit was mapped `true`. It is `false`-or-`true` depending
  on an error the old call could not report. Task 1 now uses the
  error-returning form.
- The skip was applied to the whole job. It applies to the **Emitted clear
  only**, because the two things `ClearAllEmitted` does act on disjoint article
  sets.

## Global constraints

- `internal/app` and `internal/queue` only. No schema change, no migration.
- No new exported symbol in `internal/durability` or `internal/assembler`.
- Go lets a call statement discard return values — that is what keeps the
  shutdown and periodic callers untouched. Do not edit them.
- Every red check observed with `-count=1`.

---

## Task 1 — `checkpointJob` reports whether the job is safe to clear

**Files:** `internal/app/durability.go`; test `internal/app/durability_internal_test.go`.

**Signature:** `func (app *Application) checkpointJob(ctx context.Context, jobID string) bool`

The bool means **"this job has nothing written-but-unacked that clearing
Emitted could strand"** — not "a barrier ran". One exit is not an ack and is
still safe; one looks safe and is not.

| Exit | ~Line | Returns | Why |
|---|---|---|---|
| `app.barrier == nil` | 465 | `true` | Unreachable in production — `app.barrier` is set unless `repo == nil \|\| repo.DB() == nil` (`app.go:487-491`) and `cmd/gonzbd/main.go:206-214` fails start if `history.Open` errors. Keeping `true` preserves today's test behaviour. |
| `tgt == nil` | 492 | **`false`** | No barrier ran. Per the comment at 480-486 the assembler still holds handles for a job the queue dropped, so written bytes may exist unacked. |
| no open files | 521 | **see below** | |
| error arms | 567, 576 | **`false`** | The barrier failed; its own comment says it "claims nothing". `ErrJobNotResident` is `false` too: the articles are on stable record but the live work set has not been updated. |
| success → `noteBarrierRun` | 579 | `true` | Acked. |

**The open-files exit is the correction.** `tgt.Files()` returns `nil` on *any*
error, including a `barrierOpTimeout` on a wedged mount, and cannot report which
(`internal/assembler/synctarget.go:274-301`; `checkpointJob`'s own comment at
507-511 states it). A wedged job with megabytes written-but-unacked lands here
— the worst case #417 describes — and would be declared safe.

Use the error-returning form instead, as `finalizeCompletedFile` already does
(`app.assembler.OpenFiles(ctx, jobID)`, `durability.go:868`;
`synctarget.go:304-311`):

- `errors.Is(err, assembler.ErrAssemblerStopped)` → `true`. It is the one error
  meaning "genuinely nothing", returned both after `Stop` and when `!a.started`
  (`synctarget.go:216-218, 231, 240`). `finalizeCompletedFile` cases it out for
  the same reason (`durability.go:870-877`) and `SyncTarget.Files` demotes it to
  Debug (`synctarget.go:293`). Folding it into `false` would strand a job's
  Emitted bits until a restart for an ordinary shutdown.
- any other `err != nil` → `false`.
- empty slice, nil error → `true`.

**Why the empty slice is safe, stated correctly.** Not "nothing was written" —
that is false, and the assembler's own docs say so: `drainAndClose` leaves
written articles unacked with their Emitted bits set, and `filewriter.go:449`
and `:868` both say such an article "is stranded for the life of the process and
only a restart's `ClearAllEmitted` recovers it".

It is safe because that class cannot be re-dispatched. Reaching this exit with
unacked written articles means the handles closed between `OpenJobIDs` and this
call, and the only production path that closes them is
`Assembler.CloseJobHandles` (sole caller `app.go:1554`), whose own doc states
the job "is already StatusVerifying" (`assembler.go:1111`). Such a job has
`PostProc` set, and `ForEachUnfinishedArticle` skips it outright —
`if job.PostProc || ... { continue }` (`queue.go:1482`). Clearing its Emitted
bits therefore cannot cause the re-fetch that #417's harm chain requires.

A job that simply never opened a file has nothing written and is safe trivially.

**Steps:** table-driven test over the five exits; observe red; implement; re-run.

## Task 2 — the sweep collects the unsafe jobs

**Files:** `internal/app/durability.go`; test `internal/app/durability_internal_test.go`.

`checkpointAllWithBudget` returns `map[string]struct{}` — the jobs whose
`checkpointJob` returned `false`. A map, not a slice: it is consulted per job
inside `ClearAllEmitted`'s loop. `checkpointAllShare` and `checkpointAll` pass
it through.

Both early returns yield an empty map, i.e. today's behaviour: `app.barrier ==
nil`, and `OpenJobIDs` failing. Neither can enumerate the at-risk jobs, so
neither can protect them. State this in the doc.

**Document the coverage gap, and why it is benign.** The sweep enumerates
`app.assembler.OpenJobIDs` (`durability.go:701`) — jobs holding an open file —
while `ClearAllEmitted` iterates every resident job (`queue.go:1628`). The skip
set is therefore always a subset.

State in the doc *why that is not an unclosed instance of #417*, since the
question will recur: a resident job with no open file either never wrote
anything, or had its handles closed by `CloseJobHandles`, which runs only on a
job already at `StatusVerifying` (`assembler.go:1111`, sole caller
`app.go:1554`). A `PostProc` job is skipped by `ForEachUnfinishedArticle`
(`queue.go:1482`), so its Emitted bits cannot produce a re-fetch whether they
are cleared or not.

**No caller edits.** `durability.go:1070` (shutdown) and `:1016` (periodic) call
these as statements and continue to compile.

**Steps:** test that a failing job appears in the map and a succeeding one does
not; observe red; implement; re-run.

## Task 3 — skip the Emitted clear only, for skipped jobs

**Files:** `internal/queue/progress.go`, `internal/queue/queue.go`,
`internal/app/reloader.go`, `internal/app/app.go`.

**Tests the signature change breaks and that must be updated** — enumerated
with `git grep -n ClearAllEmitted`, not recalled:
`internal/queue/lifecycle_test.go:331`, `internal/queue/pending_test.go:205`,
`internal/queue/failed_articles_byidx_test.go:144,190`,
`internal/queue/failed_articles_race_test.go:112,123`,
`internal/queue/reload_complete_file_test.go:40`,
`internal/app/scenario_reload_test.go`, plus
`internal/queue/progress_helpers_test.go` and
`internal/queue/progress_bytes_test.go` for `resetForReload`'s new parameter.
All are compile failures, not silent breakages.
`internal/queue/manifest_gate_test.go:31` keys on the method NAME, which is
kept, so it is unaffected.

New coverage goes in `internal/queue/clearallemitted_test.go` and
`internal/app/scenario_reload_checkpoint_test.go`.

**The correction that makes this smaller than rev 1.** `ClearAllEmitted` does
two things, and they act on **disjoint article sets**:

- `markFailed` sets `done`+`failed` and **clears** `emitted` (`progress.go:747`).
- `resetForReload`'s un-fail arm is gated on `p.failed.Get(i)` (`:800`).

So a Failed article is never Emitted, and #417's strand is caused *only* by the
unconditional `p.emitted.Clear(i)` at `progress.go:798`. Skipping the whole job
would strand teardown-failed articles instead — permanently, since the only
writers that clear `failed` are `resetForReload` (`:810`) and `Job`'s retry
reset (`job.go:864-865`), `markNotDone` refuses a permanently failed article
(`:726`), and a restart re-applies `markFailed` from the persisted rows
(`sqlite_store.go:356`). That is reachable: `ErrNoServersLeft` during teardown is
terminal (`pipeline.go:330`) and reaches `AckPermanentFailure` (`:356`).

**Change:** `resetForReload(m *Manifest, i int, clearEmitted bool) bool`. When
`clearEmitted` is false it skips `p.emitted.Clear(i)` and does everything else
unchanged. One production caller, so the parameter is data flowing from the skip
set rather than a policy choice re-made per site.

`ClearAllEmitted(skipJobIDs map[string]struct{})` passes `!skipped`. A skipped
job still runs the failed-article reset, still calls `recompute(m)` — its
`failedBytes` changed — and still takes part in the `cleared`/`retained`
reconciliation from #427.

Both call sites change explicitly: `reloader.go:221` passes Task 2's map;
`app.go:924` (Start) passes `nil`, because nothing is in flight at startup and
the L3 resume sweep re-derives from `durable_runs` immediately after.

The name `ClearAllEmitted` is kept — it still clears all Emitted bits across
every job it is not told to skip, and renaming churns ~15 prose references.

**Also in this task:** when the skip set is non-empty, log one warning naming
the count and job IDs. Folded here rather than standing alone because it shares
this task's fixture and has no independent test cycle. The text must say the
stall is only **partly** self-clearing (see deferred item 3).

**Steps:** unit test that a skipped job keeps Emitted while a non-skipped one
loses them, and that a skipped job's *failed* articles are still un-failed;
observe red; implement. Then extend
`internal/app/scenario_reload_checkpoint_test.go` — which already builds the
write-but-don't-ack fixture at `TestReload_DoesNotReFetchAWrittenButUnackedArticle:39`
— with the checkpoint-failed variant.

## Task 4 — claim sweep

**Files:** the sites below.

The change falsifies live claims. Per AGENTS.md step 4, sweep for the claim, not
the file:

- `reloader.go:204-206` — "this checkpoint is best-effort: it reports no status,
  so the clear below runs whether it acked every job or none", citing #417.
- `reloader.go:211-213` and `progress.go:788-791` — both argue the narrower
  guard "is not expressible at the queue layer". The corrected design partially
  contradicts this and the replacement must say what still holds, not a weaker
  universal (AGENTS.md rule 4).
- Unconditional "ClearAllEmitted recovers them" claims at `durability.go:39`,
  `assembler.go:215`, `assembler.go:1080`, `filewriter.go:449`,
  `filewriter.go:868`, `downloader/dispatch.go:589`.
- `docs/durability-contract.md:1471` and `docs/go-standards.md:354`.

**Steps:** `git grep -n ClearAllEmitted` from the repo root, re-read each hit
against the landed diff, correct what is falsified.

---

## Inconclusive / Deferred items

1. **`app.barrier == nil` → `true`.** RESOLVED in review: unreachable in
   production (`app.go:487-491`, `main.go:206-214`). Kept `true` so test
   behaviour is unchanged.

2. **`OpenJobIDs` failure and nil barrier yield an empty skip set**, so the
   clear proceeds as today. A deliberate choice, not an unknown: the alternative
   — skipping the clear entirely on a transient listing error — would strand
   every job's in-flight articles. Recorded so a reviewer sees it was decided.

3. **The stall is only PARTLY self-clearing.** `AckDurable` reaches `markDone`
   (`workset.go:69`) which clears `emitted` (`progress.go:695`), so a later
   successful barrier releases articles **whose bytes reached disk**. An article
   that was emitted and whose data died with the old downloader will never be
   acked, stays `emitted` forever, is invisible to `ForEachUnfinishedArticle`,
   and needs a restart. The warning text in Task 3 must not claim otherwise.
   *Probe:* none outstanding — verified in review.

4. **Whether a skipped job absent from `reset` writes anything to the store.**
   RESOLVED: it does not (`queue.go:1745-1756`, `anyCleared` at `:1650`). Moot
   under the corrected design, which keeps skipped jobs in `reset`.
