# Post-Processing Pipeline Contract

This document is the contract for `internal/postproc`, `internal/par2`, and
`internal/unpack`: the stage execution state machine, queue scheduling, repair and
extraction rules, script execution isolation, and owned-file safety bounds.

`docs/ARCHITECTURE.md` describes post-processing high-level design. This document
establishes its contract-level invariants and error escalation rules.

**This states the contract in the present tense, including parts not yet built.**
That is deliberate — it is the target the code is held to, not a report on the
code as it stands. The Status section below records exactly what has landed.
Where the two disagree, the code is wrong and the gap is a bug, not a
documentation error.

## Why this exists

Post-processing transforms downloaded assemblies into final destination files. It
handles complex, non-deterministic operations: verification, PAR2 recovery block
calculations, archive decompression (RAR, 7z, TAR, split join), deobfuscate, file
renaming, and execution of arbitrary user-defined scripts.

Multi-stage post-processing introduces subtle failure modes:

- **Dirty working directories**: A failure in repair or unpack halting cleanup or
  logging stages, leaving temporary files and incomplete extractions in place.
- **Redundant PAR2 repair subprocesses**: Executing expensive multi-minute `par2`
  processes on files that already passed CRC32 checks or were successfully
  extracted by DirectUnpack.
- **Accidental deletion of un-owned files**: Cleanup stages blindly deleting files
  in a shared or reused working directory that belong to other downloads (#3462).
- **Environment leakage to user scripts**: User post-processing scripts failing
  or hanging due to unhandled environment variables, missing positional args, or
  un-capped log output.

## Pipeline Architecture & Queue Scheduling

Post-processing is orchestrated by a single `PostProcessor` instance owning a
single worker goroutine (`run`).

- **Queue primitive (`ppQueue`)**: A mutex-protected FIFO slice of `*Job` items.
  `Process(job)` appends to the queue; the single worker dequeues items via
  `q.Pop(ctx)`.
- **At most one run of a job instance**: `Process` accepts any job,
  including one whose ID is already queued or running. The app admits at most
  one post-processing run of a job instance, ever, in
  `Application.enqueuePostProc`, which keeps the admission record
  (`postProcAdmissions`). It has two callers: `appRunner.runPostProc`, and
  `maybeFinalize`, which the downloader's and the pipeline's hopeless
  callbacks, `runAssess` and `Application.Fail` call
  (`git grep -n 'maybeFinalize(' -- 'internal/*.go' ':!*_test.go'` returns
  those 4 call sites and the definition). An admission starts before the DirectUnpack wait and
  ends once `jobFinalizer.finalize` or `jobFinalizer.cancelled` has run, so it
  covers the two windows `HasJob` does not see: the wait before `Process`, and
  the callback tail after the worker clears its busy marker. Ending it does not
  deregister the instance: the dispatcher still holds one whose removal
  failed. So an ended admission leaves the instance recorded, held weakly
  until the job is collected, and a later call for it is refused rather than
  start a second run whose finalize would file the job in history again. It
  is keyed by job instance, so a retry
  registered under the ID of a job still being finalized is admitted. An
  admission a shutdown interrupts is never ended.
- **A removed job is not handed over**: `enqueuePostProc` calls `Process`
  only after `postProcAdmissions.beginHandOver`, which refuses a job instance
  `RemoveJob` has marked removed (`jobTransitions.markRemoved`); a refused
  enqueue runs nothing and hands the job to `jobFinalizer.cancelled`, which
  releases its launch claim and ends its admission. The claim is that
  enqueue's to release: at `Extracting` and `Finalizing` the dispatcher's
  cancel does not interrupt the job, so `appWorkers.Abort` never runs for it.
  After marking, `RemoveJob` calls `postProcAdmissions.withdraw`, which
  signals a DirectUnpack wait the enqueue is in to abort its unpacker —
  `duOrch.abortJob` cannot reach one `duOrch.collect` has already taken — and
  returns only once that wait has ended, so nothing `RemoveJob` deletes is
  still being written. It likewise waits for a hand-over already past the
  check, so the `Cancel` it makes afterwards finds that job queued or
  running. A `RemoveJob` whose `dispatcher.Remove` fails withdraws its mark,
  and a hand-over that begins after that is not refused; its job reaches
  post-processing with the DirectUnpack the withdraw aborted, whose sets are
  recorded as failed.
- **An admitted job is not downloaded**: `maybeFinalize` moves no persisted
  position, so a job it hands over from `Fetching` — `Fail`, the hopeless
  callbacks — keeps a dispatchable row (`IntentRun`, at `Fetching`) until the
  finalizer's `CancelJob`. The downloader skips an admitted job anyway, and a
  Fetching worker does not report one download-complete (`appRunner.runFetch`):
  both read the admission record (`postProcAdmissions.has`, which is
  `downloader.Options.HandedOff`), and `enqueuePostProc` admits before it calls
  `CloseJobHandles`. An article already in flight at the hand-off is dropped
  by the assembler's whole-job tombstone, which `CloseJobHandles` sets and
  `ForgetJob` clears for a retry. `TestFail_AJobInPostProcessingIsNotDispatched`
  and `TestCloseJobHandles_TombstonesTheWholeJob` are the pins. A downloader
  reload leaves an admitted job's progress alone: `ReloadDownloader` runs
  `Job.ClearEmittedForReload` under `postProcAdmissions.unlessAdmitted`, so
  it neither un-fails the articles whose failed bytes the run and its history
  entry read, nor overlaps an admission that begins during the reload
  (`TestReloadDownloader_LeavesAnAdmittedJobsProgressAlone`). Since no more
  files arrive for it, a job admitted before its download finished
  (`Job.IsComplete` false) has its DirectUnpacker aborted rather than awaited;
  see `docs/durability-contract.md` § "DirectUnpack streaming contract".
  Such a job keeps its pool-A lease until the finalizer's `CancelJob`, as a
  `Repairing` job waiting in the queue does (`needsLease`,
  `internal/sched/requirements.go`). Giving it back early is not a saving:
  a job parked at `Fetching` with `IntentRun` is granted a lease and launched
  again on the next tick, and while it holds none the dispatcher evicts its
  manifest (`docs/dispatch-contract.md` § "Manifest residency is derived from
  pool membership"), which the run's download listing
  (`buildDownloadFileList`) and the finalizer's `retainedProgressFor` read.
- **Which copy's information wins**: a refused call hands nothing over. The
  admitted call keeps everything it gathered, including the DirectUnpack
  results, which `duOrch.collect` hands out only once. The history entry's
  status reflects what the stages did. A refused call's failure reason becomes
  the run's `FailMsg` only if it arrives before `enqueuePostProc` builds the
  `postproc.Job`, which seals the admission, and only if the admission has no
  reason yet; the stages then skip, and the entry is Failed with that reason.
  Any other new reason is logged at warn and added to the entry's stage log as
  a `warnings` line, without changing its status.
- **In-flight tracking & cancellation**: `PostProcessor` tracks the active job
  (`currentJob`) and an independent job context (`currentJobCancel`).
  Calling `CancelJob(j)` either removes a pending job from `ppQueue` or cancels
  the active job's context mid-stage so the running tool returns promptly without
  stopping the worker itself. It matches `j` by instance, as `HasJob` does, so
  it leaves alone another instance of the job under the same ID. A job
  `CancelJob` removed or interrupted is handed
  to `OnJobCancelled` rather than `OnJobDone` — synchronously, before `CancelJob`
  returns, for a pending job, and by the worker once the stage pipeline has
  returned, for the active one. A `CancelJob` that lands after the worker has seen
  the job finish still ends in `OnJobDone`, and a shutdown fires neither; the
  `Options.OnJobCancelled` doc has the full set. The app wires that callback to
  `jobFinalizer.cancelled`, which releases the job's dispatcher launch claim.
  Releasing it only after the stage returns is what keeps `RemoveJob` from
  tearing down files a stage is still using. That holds for `Repairing` as
  well as `Extracting`/`Finalizing`: the dispatcher's cancel aborts a running
  `Repairing` job, and `appWorkers.Abort` does not yield a job instance the
  post-processor holds (`HasJob`, which compares instances, so an earlier
  instance of the job still in post-processing does not count). Its claim is released on the post-processor's way
  out instead: by this callback, by `jobFinalizer.persistAndCommit` through
  `OnJobDone` for a job that finished, or by `Shutdown`'s yield after a stop.
  `RemoveJob` cancels in the dispatcher before it cancels here, so a job the
  runner hands over after the abort looked is still stopped by this `CancelJob`,
  though the abort has already released its claim and `RemoveJob` does not
  wait for its stage to return.
- **Crash recovery handoff**: nothing marks a job as in post-processing on
  disk. If the daemon crashes or shuts down while a job is being processed,
  `workerCtx` cancellation halts stage execution and the job stays in the
  queue, and the next startup resumes it from its persisted dispatcher state.
  A job at `Repairing`, `Extracting` or `Finalizing` is post-processed again;
  one handed over from `Fetching` is back at `Fetching`, where it downloads
  again if it is incomplete. If it is complete, its Fetching worker reports it
  download-complete (`appRunner.runFetch`) and it reaches post-processing
  through `Assessing`, as a job whose last file completes in the process does.
  `Application.Start` hands no job to post-processing itself. Before the first
  tick it drops a queued job already filed in history
  (`Application.reconcileBeforeFirstTick`), so such a duplicate is not routed
  onward.

## Full 12-Stage Execution Sequence

The complete post-processing pipeline consists of 12 registered stages configured in
`internal/app/stages.go` and executed sequentially for every job:

```
[ 1. quickcheck ] ──► [ 2. repair ] ──► [ 3. rar_volume_recovery ] ──► [ 4. unpack ]
                                                                             │
                                                                             ▼
[ 7. recover_par2_names ] ◄── [ 6. sample_cleanup ] ◄───────────── [ 5. extracted_repair ]
       │
       ▼
[ 8. par2_cleanup ] ──► [ 9. deobfuscate ] ──► [ 10. extension_cleanup ] ──► [ 11. finalize ] ──► [ 12. script ]
```

### Stage Responsibilities & Self-Gating Matrix

Stages do NOT abort the loop on error. Each stage checks job flags (`ParError`,
`UnpackError`, `QuickCheck`, PP level) to decide whether to execute, skip,
or modify its behavior:

| Stage Name | Responsibilities | Skip / Gating Condition | Key Flags Updated |
|---|---|---|---|
| **`quickcheck`** | Relocates flat files into expected subdirs; verifies file CRC32s against par2 headers without executing `par2`; defers each par2 set that protects what an archive extracts to until after unpack. | Skipped if disabled or `PP < 1`. | Sets `QuickCheck` to one of `NotRun` / `Clean` / `Damaged` / `Inconclusive` / `Unidentified`, and `DeferredPar2Sets`. |
| **`repair`** | Executes native Go `go_par2` engine or external `par2` verify/repair if files are missing or corrupted, for every par2 set not in `DeferredPar2Sets`. | Skipped if `PP < 1`, `QuickCheck == Clean`, `QuickCheck == Unidentified`, OR DirectUnpack extracted all archives without errors **and** `QuickCheck == NotRun`. | Sets `ParError` and `Par2Renames`. |
| **`rar_volume_recovery`** | Renames obfuscated volume files (e.g. `abc.001` → `abc.part001.rar`) using RAR5 header volume sequencing if standard filename parsing found no RAR sets. | Skipped if disabled, standard RAR sets already detected, or volume indexing is ambiguous. | Renames volume files in `DownloadDir` & `OwnedFiles`. |
| **`unpack`** | Decompresses archives (`RAR`, `7z`, `TAR`, `split join`) up to `maxUnpackDepth = 3` recursive passes using native pure-Go engines (`go_rar`, `go_7z`, `go_tar`, `filejoin`) with optional external CLI fallbacks (`unrar`, `7z`). Respects `DirectUnpackSets` to skip already-extracted archives. | Skipped if `PP < 2` OR `ParError == true` (skips extraction unconditionally on repair failure). | Sets `UnpackError`. |
| **`extracted_repair`** | Runs `repair`'s per-set verify/repair, with `repair`'s configuration, on the sets in `DeferredPar2Sets`, against the files unpack extracted. | Does nothing when `DeferredPar2Sets` is empty; skipped if `ParError` or `UnpackError` is set. | Sets `ParError` when a deferred set cannot be verified or repaired, or is gone; otherwise sets `DeferredPar2Verified`. |
| **`sample_cleanup`** | Deletes sample video and proof files matching `(?i)(^|[\W_])(sample|proof)`. Includes a false-positive guard where all files match the pattern. | Skipped if disabled in config or if every file in the directory matches the sample pattern. | Unlinks sample files from `OwnedFiles`. |
| **`recover_par2_names`** | Restores original filenames by scanning `.par2` files on disk for 16KB MD5 hashes via `deobfuscate.Par2Rename`. | Runs unconditionally after unpack. | Renames files in `DownloadDir` & `OwnedFiles`. |
| **`par2_cleanup`** | Deletes `.par2` files and orphaned `.1`, `.2`, etc. backup files created during `par2 repair` after repair, unpack, and rename stages have finished. | Skipped if `ParError` or `UnpackError` set (preserves par2 files for manual repair), or if `DeferredPar2Sets` is non-empty and `DeferredPar2Verified` is false (a deferred set protects extracted files par2 has not checked). | Unlinks `.par2` and `.1`/`.2` backup files. |
| **`deobfuscate`** | Detects obfuscated file names and restores clean titles from job metadata. Also performs subtitle alignment (`.srt` renamed to match dominant video). | Skipped if disabled in config. | Renames files and subtitles in `DownloadDir` & `OwnedFiles`. |
| **`extension_cleanup`** | Deletes unwanted file extensions (`.sfv`, `.nfo`, etc.) based on user config. Explicitly protects `.nzb` files (`SkipNZB = true`) and files in `ConsumedFiles`. Removes newly empty subdirectories. | Skipped if cleanup list empty. | Unlinks matching extensions from `OwnedFiles`. |
| **`finalize`** | Moves processed files from `DownloadDir` to `FinalDir` (`CompleteDir/job_name`). When `job.ParError || job.UnpackError || job.FailMsg != ""`, skips moving to `FinalDir` and instead prepends `_FAILED_` to `DownloadDir` in place (when `folder_rename: true`), leaving files in incomplete download area for retry; `RetryHistoryJob` moves it back first (see Failure & Degradation Rules). | Always runs unless pre-check aborted job. | Populates `FinalDir` or renames `DownloadDir` with `_FAILED_` prefix; sets status to `StatusMoving`. |
| **`script`** | Executes user-defined post-processing script with full environment (`SAB_*` vars, including Go-specific `SAB_FINAL_PROCESSING_DIR`) and 8 positional args ($1–$8). Supports `RedactSecrets` (`SAB_API_KEY`/`SAB_PASSWORD` masked as `**REDACTED**`) and `ScriptCanFail` (non-zero exit logged as warning instead of error). | Skipped if no script configured for job/category. | Captures script exit code and stdout/stderr log (capped at 512 KiB). |

> **`quickcheck` is a permanent stage (decided 2026-09-03).**
> The job-lifecycle rework's plan 2 listed it for deletion, on the premise that
> it duplicated the download path's par2 verification. (The plan document that
> said so was deleted with the rest of `docs/superpowers/plans/` in `6c1ca60a`;
> the decision is recorded here because this is now its only home.) That
> premise was retired when #494/#495 gave both
> consumers one shared computation (`par2.Assess`) and #491 closed without a
> unified `Verdict`: the two now read one assessment and answer different
> questions — *fetch the deferred volumes?* versus *relocate, and may repair
> skip the binary?*
>
> The two responsibilities in the row above are the ones that make it
> permanent, and neither is a verification decision:
> `par2.ApplyRenames` has exactly one caller in the tree
> (`stage_quickcheck.go:101`), paired with `markRenamed` so relocation does not
> strand a file's old path in `OwnedFiles`; and `QuickCheckClean` is the only
> verdict that lets `repair` skip spawning par2 on a set it has checked
> (`stage_repair.go:110`). The
> download path cannot host either — it *"decides; it never renames"*
> (`docs/ARCHITECTURE.md`) and performs no I/O by design.
>
> The lifecycle rework's obligation to this stage was a repoint of its
> `job.Queue` reads rather than a deletion, and that is discharged: the `Queue`
> type is gone with `internal/queue` (`b6651d43`), and `git grep -n 'job\.Queue'
> -- '*.go'` now matches only two historical comments in
> `internal/postproc`'s tests.

### Verification state is derived, never persisted (#533)

**There is no per-job admin directory.** A job's download directory holds no
state of ours that outlives a run, and `repair` recomputes which par2 sets
verify on every run rather than reading a record of the last one. A retried or
crash-restarted job therefore re-verifies.

The qualifier is load-bearing. The extraction path does write into that
directory transiently: `fsutil.RootedCreateTemp` puts a `.gonzbd-tmp-<16 hex>`
file beside each entry it is about to rename into place, and the four `unpack`
engines open their root at the same directory. Those are removed on the
deferred path, so a crash or a kill mid-extraction can leave one behind. What
does not exist any more is a file we later READ BACK and act on — which is the
property that mattered, since it is the read that turns a forged write into a
decision.

This replaced a `__ADMIN__/__verified__` file, and the reasoning is worth keeping
because the file looked cheap:

- It was **inert on first runs.** Loading found nothing, so nothing was skipped.
  The cleanup stage deleted the directory on success, so the record survived only
  for jobs that had FAILED — its entire effect was letting a retry skip
  re-verification.
- It could be **stale in exactly that case.** `RetryHistoryJob` rebuilds from the
  NZB and may re-download into the same directory, so the record could assert a
  set verified whose files had since changed.
- It could be **forged by the content it was vouching for.** It lived inside the
  directory par2 relocation and archive extraction write into, so a
  poster-controlled par2 filename or archive entry naming it made `repair` return
  early on `AllVerified()` and skip verification for every set in the job, with
  no `ParError` and no "Damaged" state.

Sandboxing could not have fixed the third: `cmdutil`'s bwrap wrapper `--ro-bind`s
`/` and `--bind`s the job download directory read-write, so the admin directory
sat inside the sandbox's one writable root by construction. Nor could a
reserved-name check on our own writers, because the external `unrar` and `7z`
fallbacks are handed that directory and preserve archive paths themselves —
their entry names never pass through `unpack.SanitizeArchivePath`.

Standing Design Rule 2 states the general form: a derived value that is also
persisted acquires a second source of truth, and the stored copy is the one that
drifts. **Anything tempted back into the job directory as a sidecar inherits all
three problems**, so the answer is SQLite or the per-instance
`constants.AdminDirName`, not a new file next to the content.

Relocation itself is confined by `os.Root` rather than by a lexical path check —
see `par2.relocateFile`. That is a separate guarantee, and it survives
independently of the above: it bounds where a poster-controlled par2 name can
write at all, rather than protecting any particular file.

## Post-Processing (PP) Level Enforcement

SABnzbd post-processing levels are cumulative integer masks on `postproc.Job.PP`
(`internal/postproc/stages.go:154`) — post-processing's own job struct, not
`internal/job.Job`. `PP` does not survive past App, which resolves it into a
`job.Policy` before persistence (see `docs/dispatch-contract.md`):

- **PP = 0 (Download Only)**: Skips `quickcheck`, `repair`, and `unpack`. Runs the cleanup stages (`sample_cleanup`, `par2_cleanup`, `extension_cleanup`), finalize, and script.
- **PP = 1 (Repair Only)**: Runs `quickcheck` and `repair`. Skips `unpack`.
- **PP = 2 (Repair + Unpack)**: Runs `quickcheck`, `repair`, and `unpack`.
- **PP = 3 (Repair + Unpack + Delete)**: Full processing including archive deletion.

`shouldSkipForPP(stageName, pp)` enforces these bounds centrally. Stages like
`deobfuscate`, `sample_cleanup`, `finalize`, and `script` always run regardless of PP level.

## Native Engine Dispatch & External Fallback

`gonzbd` executes verification, repair, and archive decompression using native Go libraries by default:
- **`go_par2`**: Native Reed-Solomon verification and repair engine (`UseGoPar2`).
- **`go_rar`**: Pure-Go RAR5 extraction engine (`UseGoRAR`; falls back to external `unrar` for RAR3 and unsupported formats).
- **`go_7z`**: Pure-Go 7-Zip extraction engine (`UseGo7z`).
- **`go_tar` / `filejoin`**: Native TAR extraction and split file joining.

External command-line binaries (`par2`, `unrar`, `7z`, `7zz`) are invoked as automatic fallbacks only when native execution reports inconclusive errors or fails (`GoPar2Fallback`, `GoRarFallback`, `Go7zFallback`) or when native engines are explicitly disabled in configuration.

## Core Pipeline Invariants

1. **Non-aborting stage loop**: A stage returning a non-nil error records the error
   into `StageLogEntry.Err` but MUST NOT abort the pipeline loop. Subsequent
   stages (the cleanup stages, `finalize`, `script`) MUST still execute so
   directory hygiene is maintained and user scripts receive the failure status
   code.
2. **Pre-check abort**: If `job.FailMsg` is pre-populated (e.g. download health
   check failed) or `job.DownloadDir` is empty/missing, all processing stages are
   skipped. A synthetic `pre-check` entry is appended to `StageLog` and the job
   completes directly to history.
3. **Verification bypass guarantees**: `repair` bypasses `par2` execution when
   `QuickCheck == Clean` (verification already confirmed every CRC), or when
   DirectUnpack extracted all archives without errors **and** `QuickCheck ==
   NotRun`, or when `QuickCheck == Unidentified`. The first two eliminate
   multi-minute disk reads for healthy downloads.

   The third is not a verdict that the data is intact. It is reached when
   every par2 set that describes any file is **deferred**: judged a Layout B
   set — par2 protecting what an archive extracts to, which does not exist
   until `unpack` runs — so that a repair here has nothing to verify, and the
   `ParError` of a failed one would skip the `unpack` that produces the
   files. Deferral is judged per par2 set (`deferredSets` in
   `stage_quickcheck.go`, recorded in `Job.DeferredPar2Sets`), on the entries
   identification did not account for, and a set that fails any condition is
   repaired before `unpack` as usual:

   - `unpack` will run for the job: the stage is enabled and the job's PP is at
     least `PPUnpack` (`shouldSkipForPP`). At PP=1, or with unpack disabled,
     `repair` is the only check the job gets;
   - the set has at least one entry nothing delivered was identified as, by
     name or by content. Entries it did identify — a sidecar `.nfo` it also
     protects — do not stop the deferral; they are verified with the rest;
   - none of those unaccounted entries is itself named as an archive
     (`unpack.Classify`). par2 names are the poster's real names, so such a
     set protects archives (Layout A), and an obfuscated archive damaged
     inside its first 16 KB is what matches nothing there;
   - each of those entries' base name is a member of a RAR or 7z archive in
     the directory, read from headers (`unpack.MemberBaseNames`; for RAR only
     the first volume). An archive that names none of them — a `Subs.rar`
     beside a damaged obfuscated `Movie.mkv` — shows nothing about them, and
     a scan or listing error defers no set.

   The CRC verdict (`Clean`, `Damaged`, `Inconclusive`) is read over the sets
   that were not deferred (`par2.Assessment.CRCExcluding`), and `repair`
   skips the deferred ones, so a Layout B set beside an ordinary set leaves
   the ordinary one repaired before `unpack` and itself verified after.

   A deferred set is verified by `extracted_repair`, which runs straight
   after `unpack` and before `sample_cleanup` can delete anything it
   protects. It runs `repair`'s own per-set verify+repair
   (`RepairStage.repairSets`) with `repair`'s configuration, against the
   extracted files and whatever recovery volumes are on disk. The archive's
   own per-member checksum is not enough on its own: `go_rar` cannot check a
   BLAKE2sp-only or MAC digest (`unpack.CloseMember` filters
   `ErrChecksumUnsupported`), `go_7z` skips a member that records no CRC,
   and damage in a stored archive's member passes through. A set
   `extracted_repair` cannot verify or repair sets `ParError`, which fails
   the job; so does a deferred set it cannot find. It does not run after a
   failed `repair` or `unpack` (the files are not there, or not whole), and
   `par2_cleanup` keeps the par2 files of a job whose deferred sets were
   not verified.

   Known limits, by construction: archives are found by name
   (`unpack.Scan`), so obfuscated volumes that `rar_volume_recovery` renames
   later are not seen, and a member whose header starts in a later RAR
   volume is not listed; either leaves the set undeferred, so it is repaired
   before `unpack`. Members are matched by base name, while par2 looks for a
   file at the path it records, so a deferred set whose archive extracts a
   protected file into a different directory fails `extracted_repair`, and
   the job with it, unless the recovery volumes on disk can rebuild the file
   where par2 expects it.

   The second clause requires exactly `NotRun` — the stage was disabled or
   found no par2 sets, so there is no verification to be had and DirectUnpack's
   signal is the only one available. `Inconclusive` (quickcheck was attempted
   and could not complete, e.g. the job's manifest was unreadable) must not
   bypass: par2 sets exist and nothing has checked them. Both states were
   encoded as `!QuickCheckRan` until #294, so a job whose manifest could not be
   read had its CRCs verified by nothing at all — quickcheck bailed, and repair
   skipped on DirectUnpack's say-so. `QuickCheckOutcome` makes the two
   nameable and the switch in `stage_repair.go` exhaustive.

   **Where QuickCheck's CRCs come from.**
   The stage compares the par2 index's per-file checksums against
   `FileProgress.AssembledCRC32`, which is set only by `Queue.SetFileCRC32FromRuns`
   (see below for its one caller). The assembler used to compute a
   whole-file value by folding the per-article CRCs it happened to see, which
   was #349 — a resumed run is never sent the articles an earlier run
   completed, so its parts do not tile the file — and that writer is gone with
   the rest of the assembler's authority (see
   [`docs/durability-contract.md`](durability-contract.md)).

   The replacement is the `crc32` of the file's single **durable run**. A run
   combines the CRCs of the articles that abut as they join it, and the runs
   persist across restarts, so they account for every article of the file
   whichever run fetched it — a *resumed* file supplies a CRC as readily as a
   fresh one, which the old design could not. No read of the file is involved
   (R24), and `Application.recordAssembledCRC` threads the value to
   `Queue.SetFileCRC32FromRuns` when the file finalizes — which publishes it
   **only when the file holds exactly one run, that run starts at offset 0, and
   it covers every article of the file**. A file with an article overlapping a
   sibling keeps that article in a row of its own, so it has more than one row;
   a file where two articles claimed the SAME offset keeps one row but cannot
   cover the dropped article's index. Either way it supplies no CRC rather than
   one describing bytes it may not hold (#387). The setter takes the runs, not
   a `uint32`, so there is no way to record a CRC without the record that
   earned it.

   **The consumer is `par2.Assess`.** This was once a distinction —
   "`par2.VerifyCRCs`, not `par2.QuickCheck`" — because relocation and CRC
   comparison were separate functions, and the *stage* being named quickcheck
   made them easy to conflate. They are one call now (#494): `Assess`
   identifies each delivered file against the par2 index, verifies it against
   the CRC recorded here, and reports the relocations that follow, all from a
   single pre-rename read of the directory. `stage_quickcheck.go` and
   `app.par2Verdict` both consume that one assessment.

   What our recorded CRC buys is unchanged and is the part worth keeping from
   the old wording: verification reads no payload. Identification pays 16 KB
   per file; the comparison itself pays nothing.

   A file that holds more than one run — a permanently failed article leaves a
   hole, and a hole means a gap between rows — reads as `NoCRC`, which is zero's documented
   "unavailable" meaning rather than a mismatch, so `unverifiable > 0` and the
   stage lands on `Damaged`. The consequences stay conservative in both places
   that consume the verdict: `repair` is not bypassed by clause one, and —
   provided the file was identified against the par2 index, which this
   scenario's earlier hole in the run record does not prevent —
   `app.par2Verdict` returns `outcomeRepair`, so on-demand par2 fetches the
   recovery volumes. That costs bandwidth and a par2 pass on a file with a
   hole; it never ships an unrepaired one. A file `Identify` cannot match
   against anything reads `outcomeUnknown` instead, not `outcomeRepair`: the
   volumes are held rather than fetched, which is the conservative branch for
   that case — still nothing ships unrepaired, but the mechanism differs from
   an identified file's `NoCRC` finding.

   `Inconclusive` is also the **default** the quickcheck stage adopts as soon
   as it knows par2 sets exist, narrowing to `Clean`, `Damaged` or
   `Unidentified` only on paths that actually identified or verified
   something (#314). This inverts which state is
   free: the zero value used to be the permissive one, so any early `return`
   that forgot to assign handed repair consent to skip par2 — and one did, the
   guard in `recordVerdict` for a job whose manifest describes no files. With
   the default inverted, a future early return fails safe by construction
   rather than by review catching it.
4. **`OwnedFiles` isolation (#3462)**: `processJob` snapshots `OwnedFiles` from
   `DownloadDir` before any stage runs. Unpack and rename stages register newly
   created files into `OwnedFiles`. Cleanup stages (`extension_cleanup`,
   `sample_cleanup`) MUST ONLY delete files present in `OwnedFiles`, guaranteeing
   unrelated files in shared directories are never deleted.
5. **Script environment contract**: User scripts receive 8 positional arguments
   ($1–$8) matching Python SABnzbd:
   `script <complete_dir> <nzb_name> <job_name> <report_name> <category> <group> <status> <failure_url>`
   and environment variables: `SAB_COMPLETE_DIR`, `SAB_FILENAME`, `SAB_FINAL_NAME`,
   `SAB_CAT`, `SAB_GROUP`, `SAB_PP_STATUS`, `SAB_PP`, `SAB_SCRIPT`, `SAB_VERSION`,
   `SAB_API_KEY`, `SAB_FINAL_PROCESSING_DIR`, etc. Scripts support `RedactSecrets` (masking `SAB_API_KEY`/`SAB_PASSWORD` as `**REDACTED**`) and `ScriptCanFail` (treating non-zero exit codes as warnings).
6. **Log output cap**: Tool output lines (par2, unrar, script stdout) captured in
   `OutputLines` and `StageLogEntry.Lines` are capped at `MaxLogBytes = 512 KiB`
   per script execution to prevent memory exhaustion from verbose tools.

## On-Demand Par2: Fetch Policy and Verdict

A file's intent to download is a tri-state `FetchPolicy` (`internal/job/progress.go`),
not a bool: `FetchAlways` (every content file, the par2 index, and any
recovery volume the job has decided to fetch), `FetchIfNeeded` (a recovery
volume held back pending the CRC verdict), and `FetchNever` (a recovery volume
the verdict proved unnecessary). Only a recovery volume is ever set to
anything but `FetchAlways`.

Two predicates read the field for different questions, and the distinction is
load-bearing rather than stylistic (`internal/job/progress.go`'s own comments
on `HasDeferredPar2`/`UsesOnDemandPar2`/`DeferredRecoveryIndices` are the
source for this paragraph):

- **`!= FetchAlways`** — "is this file being withheld from download at all" —
  drives dispatch skipping, completion (`IsComplete`), byte accounting, and the
  `UsesOnDemandPar2` badge. It is true for both `FetchIfNeeded` and
  `FetchNever`, because both describe bandwidth already saved.
- **`== FetchIfNeeded`** — "is this file still awaiting the verdict" — is the
  one `HasDeferredPar2` and `DeferredRecoveryIndices` use, and the exclusion of
  `FetchNever` is deliberate: `DeferredRecoveryIndices` feeds
  `undeferRecovery`, which any first-time permanent article failure calls
  (`internal/job/content.go`). Including a discarded volume there would let one
  late failure re-fetch exactly the volumes the CRC oracle already proved
  unnecessary, undoing the feature. Likewise `HasDeferredPar2` reporting a
  discarded volume as "held" would re-run full CRC verification on every
  subsequent completion event.

`DiscardDeferredPar2` (`internal/job/content.go`) is the sole path from
`FetchIfNeeded` to `FetchNever`: a walk over the file table setting the policy,
with no file-set mutation, deletion, or renumbering involved. A retry does not
carry the previous attempt's policy forward at all.

Within one live job there is one path back, and it is hydration rather than a
verdict: `appResidency.Hydrate` restores every file's policy from `job_files`
via `Job.RestoreFetchPolicy`, so an eviction and re-hydration moves the
in-memory policy to whatever the row holds. That is only safe because every
mutation of the policy marks the job for checkpointing: both verdicts reach
`Application.markFetchPolicyDirty`, and ingest derives the policy before the row
exists. A writer that changed the policy without marking would be undone by the
next eviction, with no error anywhere — which is what made this a real defect
before #329 rather than a theoretical one.

A retry is not that path back. It rebuilds the job from scratch through
`BuildIngestJob` (`internal/app/app.go`'s `rebuildJobFromNZB`), whose own
classification re-derives the policy from the config in force *now* — a
recovery volume becomes `FetchIfNeeded` while `downloads.on_demand_par2` is
enabled and `FetchAlways` once it is disabled. So a retry neither trusts a
downgrade computed against the previous download's damage profile, nor keeps
honouring on-demand par2 after the user has turned it off
(`TestRetryHistoryJob_ConfigurationIsHonoured`, #329).

### The verdict: identify, then verify

`par2.Assess` (`internal/par2/assess.go`) is the one function both consumers
call, and it composes two operations in one fixed order — identify each
on-disk file against the par2 index by content (`Hash16k`, falling back to an
unambiguous basename or a `{CRC32, size}` match), then verify the identified
files against the CRC recorded during download — before reporting the renames
that would relocate a file to its par2-recorded path. Identification and
verification are different questions (which entry is this file, versus is that
file intact), and computing both from one pre-rename read of the directory is
what removed the ordering bug this design exists to fix: an earlier version
relocated first and verified second, which invalidated the very names
verification matched against (#492, #494). `stage_quickcheck.go` and
`app.maybeReleaseRecoveryVolumes` (via `app.par2Verdict`) are the two callers
that consume one `Assessment`; renames are computed but not applied by
`Assess` itself — `par2.ApplyRenames` is the separate, second act.

`app.par2Verdict` (`internal/app/app.go`) turns an `Assessment` into one of
three outcomes:

| Outcome | Meaning | Action |
|---|---|---|
| `outcomeClean` | Every par2-tracked file was identified and its assembled CRC matched. | Recovery volumes discarded (`DiscardDeferredPar2`); job finalizes without them. |
| `outcomeRepair` | At least one par2-tracked file is corrupt, has no CRC to check, could not be verified, or a par2 entry matched no delivered file while others in the same set did. | All deferred volumes un-deferred (`UndeferRecoveryVolumes`) and fetched; job re-enters download. |
| `outcomeUnknown` | Nothing delivered matched any par2 entry, by name or by content. | Volumes are held, neither fetched nor discarded; job finalizes as-is. |

`outcomeUnknown` covers two indistinguishable cases: a Layout B post (par2
protects the files an archive will extract to, which do not exist yet, so
nothing here can say whether the volumes will be needed) and an obfuscated
single-file post damaged inside its first 16 KB (defeating every
identification method at once). Both read the same because identification
found literally nothing to work with; holding rather than discarding avoids
asserting a verdict ("skipped") that was never earned, leaving it as "held"
instead.

Post-processing reads the same signature and narrows the ambiguity with facts
the download path does not look at, per par2 set: whether unpack will run,
whether the set's unmatched entries are named as archives, and whether each
of them is a member of a delivered RAR or 7z archive. A set where all hold is
deferred: `repair` skips it, `unpack` extracts, and `extracted_repair` runs
par2 against the extracted files with whatever volumes are on disk; any
other set is repaired before `unpack` as before (see Core Pipeline Invariant
3). Neither path fetches the held volumes. A healthy extraction verifies
without them; a damaged one they would have repaired fails the job instead,
unless an article failure had already un-deferred them during download.

### `par2_release_reason`

Persisted in the `dispatch_jobs` table and exposed via `Job.Par2ReleaseReason()` / `SetPar2ReleaseReason`. It is not a
repair result and nothing branches on its text. **Only its emptiness is
load-bearing**: `JobProgress.HasPar2Verdict()` is defined as
`par2ReleaseReason != ""`, and that single predicate is what tells a job
resuming post-processing after a restart whether a verdict was ever already
reached — distinguishing "volumes held because nothing could be identified"
from "volumes still awaiting a verdict" so `buildDownloadFileList`
(`internal/postproc/filelist.go`) does not report an unverified job as
"verified clean". `ResetForRetry` is the only clearer, so a retry re-derives
the verdict rather than inheriting stale text from the previous attempt. The
`outcomeClean` path never calls `SetPar2ReleaseReason` — a clean verdict is
recorded entirely through the fetch-policy discard, not through this field.

## Failure & Degradation Rules

- **PAR2 Repair Failure (`ParError = true`)**: `unpack` is skipped unconditionally when `ParError = true`. `finalize` skips moving files to `FinalDir` and instead prepends `_FAILED_` to `DownloadDir` (when `folder_rename: true`), leaving files in the incomplete download directory so retries can find them.
  The history entry records the `_FAILED_` path. A retry writes to, and
  post-processing reads, `DownloadDir/<name>`, so `RetryHistoryJob` renames
  the `_FAILED_` directory back to it before queuing the job
  (`restoreFailedDir`, `internal/app/retry_failed_dir.go`), and back again if
  the retry aborts before the job is queued. It refuses the retry, moving
  nothing, when `DownloadDir/<name>` already exists, when another queued job
  has that name, or when the `_FAILED_` directory is gone but
  `DownloadDir/<name>` exists — each is a directory that may not be this
  job's. `TestRetryHistoryJob_ResumesInTheFailedDirectory` runs the real
  `finalize` stage through a failure and a retry, in process and after a
  restart.
- **Unpack Failure (`UnpackError = true`)**: Extraction errors (bad password,
  corrupt archive) set `UnpackError = true`. Original archive files and `.par2` recovery files are
  preserved in `DownloadDir` for manual recovery.
- **Insufficient Recovery Blocks**: When `par2` reports it needs more blocks
  than are on disk, `repair` sets `ParError = true` and leaves the block count
  in its own log line rather than on `Job` — `unpack` reads `ParError` to skip
  extraction (`internal/postproc/stage_unpack.go:128`). Downstream stages
  continue running so the job still reaches a deterministic finished state.

## Status

### Landed
- Single worker goroutine with `ppQueue` FIFO scheduling and safe cancellation (`Cancel`).
- At most one post-processing run of a job instance, including after that run has ended (`postProcAdmissions`); a failure reason reported before the run is handed over fails it, and a later one is recorded as a stage-log warning.
- Complete 12-stage pipeline with strict stage self-gating and cumulative PP-level enforcement (`shouldSkipForPP`).
- `QuickCheckOutcome` (`NotRun`/`Clean`/`Damaged`/`Inconclusive`/`Unidentified`) bypass logic & DirectUnpack zero-failure verification bypass.
- Per-set deferral of Layout B par2 sets (`DeferredPar2Sets`) and their par2 verify+repair after unpack (`extracted_repair`).
- `OwnedFiles` snapshotting and cleanup isolation (#3462) with in-place rename tracking (`markRenamed`).
- Python-compatible 8-arg positional and `SAB_*` environment contract for user scripts with 512 KiB log caps, `RedactSecrets`, and `ScriptCanFail` runtime toggleability.
- Native Go engine dispatch (`go_par2`, `go_rar`, `go_7z`, `go_tar`, `filejoin`) with external CLI fallbacks.
- Synthetic `download`, `direct unpack`, and `summary` StageLog cards for history UI rendering.

### Open Gaps (Target Invariants Not Yet Built)
- **Block-Exact Recovery-Volume Promotion (`internal/app`)**: when `repair`
  reports insufficient blocks, nothing currently promotes the additional
  `.par2` volumes the job needs and re-enters `StatusDownloading` — the job
  simply finishes with `ParError = true`. The seam for this already exists
  and is live: `Job.UndeferRecoveryVolumes`'s `fileIdxs` argument
  (`internal/job/content.go`) takes arbitrary file indices, so it already
  accepts a block-covering subset as readily as the full deferred set its
  one production caller passes (`git grep -n 'UndeferRecoveryVolumes' -- '*.go'
  | grep -v _test.go` finds 7 lines: one call site in `internal/app/app.go`,
  the declaration and its godoc in `internal/job/content.go`, and four comment
  mentions in `internal/postproc/filelist.go`). Nothing in the signature or its godoc needs to
  change for Phase 2 — the missing piece is the caller that computes the
  subset. Target: compute the block-covering subset from the
  repair stage's reported shortfall and call `UndeferRecoveryVolumes` with
  it, falling back to `Status = "Failed"` when no further recovery volumes
  remain to undefer.
- **`ScriptCanFail == false` Authoritative Failure (`internal/postproc`)**: When a
  user script exits non-zero and `ScriptCanFail` is false, `ScriptStage.Run()` sets
  `StageLogEntry.Err` but does not set `job.FailMsg`, so `buildSummaryEntry` records
  `Status = "Completed"`. Fix: PR #275.
