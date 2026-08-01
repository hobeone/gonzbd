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
- **In-flight tracking & cancellation**: `PostProcessor` tracks the active job's
  ID (`currentJobID`) and an independent job context (`currentJobCancel`).
  Calling `Cancel(jobID)` either removes a pending job from `ppQueue` or cancels
  the active job's context mid-stage so the running tool returns promptly without
  stopping the worker itself.
- **Crash recovery handoff**: Jobs whose download completed have `PostProc=true`
  persisted in SQLite history. If the daemon crashes or shuts down while a job is
  being processed, `workerCtx` cancellation halts stage execution, preserving
  the job state so crash recovery re-enqueues it on next startup.

## Full 12-Stage Execution Sequence

The complete post-processing pipeline consists of 12 registered stages configured in
`internal/app/stages.go` and executed sequentially for every job:

```
[ 1. quickcheck ] ──► [ 2. repair ] ──► [ 3. rar_volume_recovery ] ──► [ 4. unpack ]
                                                                             │
[ 8. deobfuscate ] ◄── [ 7. par2_cleanup ] ◄── [ 6. recover_par2_names ] ◄── [ 5. sample_cleanup ]
       │
       ▼
[ 9. extension_cleanup ] ──► [ 10. finalize ] ──► [ 11. cleanup ] ──► [ 12. script ]
```

### Stage Responsibilities & Self-Gating Matrix

Stages do NOT abort the loop on error. Each stage checks job flags (`ParError`,
`UnpackError`, `QuickCheckPassed`, PP level) to decide whether to execute, skip,
or modify its behavior:

| Stage Name | Responsibilities | Skip / Gating Condition | Key Flags Updated |
|---|---|---|---|
| **`quickcheck`** | Relocates flat files into expected subdirs; verifies file CRC32s against par2 headers without executing `par2`. | Skipped if disabled or `PP < 1`. | Sets `QuickCheckRan`, `QuickCheckPassed`. |
| **`repair`** | Executes native Go `go_par2` engine or external `par2` verify/repair if files are missing or corrupted. | Skipped if `PP < 1`, `QuickCheckPassed == true`, OR DirectUnpack extracted all archives without errors during download. | Sets `ParError`, `NeedRequeue`, `RequeueBlocksNeeded`, and `Par2Renames`. |
| **`rar_volume_recovery`** | Renames obfuscated volume files (e.g. `abc.001` → `abc.part001.rar`) using RAR5 header volume sequencing if standard filename parsing found no RAR sets. | Skipped if disabled, standard RAR sets already detected, or volume indexing is ambiguous. | Renames volume files in `DownloadDir` & `OwnedFiles`. |
| **`unpack`** | Decompresses archives (`RAR`, `7z`, `TAR`, `split join`) up to `maxUnpackDepth = 3` recursive passes using native pure-Go engines (`go_rar`, `go_7z`, `go_tar`, `filejoin`) with optional external CLI fallbacks (`unrar`, `7z`). Respects `DirectUnpackSets` to skip already-extracted archives. | Skipped if `PP < 2` OR `ParError == true` (skips extraction unconditionally on repair failure). | Sets `UnpackError`. |
| **`sample_cleanup`** | Deletes sample video and proof files matching `(?i)(^|[\W_])(sample|proof)`. Includes a false-positive guard where all files match the pattern. | Skipped if disabled in config or if every file in the directory matches the sample pattern. | Unlinks sample files from `OwnedFiles`. |
| **`recover_par2_names`** | Restores original filenames by scanning `.par2` files on disk for 16KB MD5 hashes via `deobfuscate.Par2Rename`. | Runs unconditionally after unpack. | Renames files in `DownloadDir` & `OwnedFiles`. |
| **`par2_cleanup`** | Deletes `.par2` files and orphaned `.1`, `.2`, etc. backup files created during `par2 repair` after repair, unpack, and rename stages have finished. | Skipped if `ParError` or `UnpackError` set (preserves par2 files for manual repair). | Unlinks `.par2` and `.1`/`.2` backup files. |
| **`deobfuscate`** | Detects obfuscated file names and restores clean titles from job metadata. Also performs subtitle alignment (`.srt` renamed to match dominant video). | Skipped if disabled in config. | Renames files and subtitles in `DownloadDir` & `OwnedFiles`. |
| **`extension_cleanup`** | Deletes unwanted file extensions (`.sfv`, `.nfo`, etc.) based on user config. Explicitly protects `.nzb` files (`SkipNZB = true`) and files in `ConsumedFiles`. Removes newly empty subdirectories. | Skipped if cleanup list empty. | Unlinks matching extensions from `OwnedFiles`. |
| **`finalize`** | Moves processed files from `DownloadDir` to `FinalDir` (`CompleteDir/job_name`). When `job.ParError || job.UnpackError || job.FailMsg != ""`, skips moving to `FinalDir` and instead prepends `_FAILED_` to `DownloadDir` in place (when `folder_rename: true`), leaving files in incomplete download area for retry. | Always runs unless pre-check aborted job. | Populates `FinalDir` or renames `DownloadDir` with `_FAILED_` prefix; sets status to `StatusMoving`. |
| **`cleanup`** | Deletes internal sidecar directories (`__ADMIN__`) from the output path. | Skipped if `ParError`, `UnpackError`, or `FailMsg != ""` is set (preserves sidecar data for debugging/retry). | Removes `__ADMIN__` directory. |
| **`script`** | Executes user-defined post-processing script with full environment (`SAB_*` vars, including Go-specific `SAB_FINAL_PROCESSING_DIR`) and 8 positional args ($1–$8). Supports `RedactSecrets` (`SAB_API_KEY`/`SAB_PASSWORD` masked as `**REDACTED**`) and `ScriptCanFail` (non-zero exit logged as warning instead of error). | Skipped if no script configured for job/category. | Captures script exit code and stdout/stderr log (capped at 512 KiB). |

## Post-Processing (PP) Level Enforcement

SABnzbd post-processing levels are cumulative integer masks on `job.Queue.PP`:

- **PP = 0 (Download Only)**: Skips `quickcheck`, `repair`, and `unpack`. Runs cleanup, finalize, and script.
- **PP = 1 (Repair Only)**: Runs `quickcheck` and `repair`. Skips `unpack`.
- **PP = 2 (Repair + Unpack)**: Runs `quickcheck`, `repair`, and `unpack`.
- **PP = 3 (Repair + Unpack + Delete)**: Full processing including archive deletion.

`shouldSkipForPP(stageName, pp)` enforces these bounds centrally. Stages like
`deobfuscate`, `sample_cleanup`, `finalize`, and `script` always run regardless of PP level.

## Native Engine Dispatch & External Fallback

`gonzbd` executes verification, repair, and archive decompression using native Go libraries by default:
- **`go_par2`**: Native Reed-Solomon verification and repair engine (`UseGoPar2`).
- **`go_rar`**: Pure-Go RAR5 extraction engine (`UseGoRAR`).
- **`go_7z`**: Pure-Go 7-Zip extraction engine (`UseGo7z`).
- **`go_tar` / `filejoin`**: Native TAR extraction and split file joining.

External command-line binaries (`par2`, `unrar`, `7z`, `7zz`) are invoked as automatic fallbacks only when native execution reports inconclusive errors or fails (`GoPar2Fallback`, `GoRarFallback`, `Go7zFallback`) or when native engines are explicitly disabled in configuration.

## Core Pipeline Invariants

1. **Non-aborting stage loop**: A stage returning a non-nil error records the error
   into `StageLogEntry.Err` but MUST NOT abort the pipeline loop. Subsequent
   stages (`cleanup`, `finalize`, `script`) MUST still execute so directory hygiene
   is maintained and user scripts receive the failure status code.
2. **Pre-check abort**: If `job.FailMsg` is pre-populated (e.g. download health
   check failed) or `job.DownloadDir` is empty/missing, all processing stages are
   skipped. A synthetic `pre-check` entry is appended to `StageLog` and the job
   completes directly to history.
3. **Verification bypass guarantees**: If `QuickCheckPassed` is `true`, or if DirectUnpack successfully extracted all archives during download without errors and QuickCheck found no checksum mismatch, `repair` bypasses `par2` execution. This eliminates multi-minute disk reads for healthy downloads. `QuickCheckRan` ensures `repair` is only bypassed when QuickCheck actually ran and verified clean.
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

## Failure & Degradation Rules

- **PAR2 Repair Failure (`ParError = true`)**: `unpack` is skipped unconditionally when `ParError = true`. `finalize` skips moving files to `FinalDir` and instead prepends `_FAILED_` to `DownloadDir` (when `folder_rename: true`), leaving files in the incomplete download directory so retries can find them.
- **Unpack Failure (`UnpackError = true`)**: Extraction errors (bad password,
  corrupt archive) set `UnpackError = true`. Original archive files and `.par2` recovery files are
  preserved in `DownloadDir` for manual recovery.
- **Re-queue Information (`NeedRequeue = true`)**: If `par2` requires additional
  blocks, `NeedRequeue` and `RequeueBlocksNeeded` are recorded on `Job` for
  history/UI reporting. Downstream stages continue running so the job reaches
  a deterministic finished state.

## Status

### Landed
- Single worker goroutine with `ppQueue` FIFO scheduling and safe cancellation (`Cancel`).
- Complete 12-stage pipeline with strict stage self-gating and cumulative PP-level enforcement (`shouldSkipForPP`).
- `QuickCheckRan` / `QuickCheckPassed` bypass logic & DirectUnpack zero-failure verification bypass.
- `OwnedFiles` snapshotting and cleanup isolation (#3462) with in-place rename tracking (`markRenamed`).
- Python-compatible 8-arg positional and `SAB_*` environment contract for user scripts with 512 KiB log caps, `RedactSecrets`, and `ScriptCanFail` runtime toggleability.
- Native Go engine dispatch (`go_par2`, `go_rar`, `go_7z`, `go_tar`, `filejoin`) with external CLI fallbacks.
- Synthetic `download`, `direct unpack`, and `summary` StageLog cards for history UI rendering.

### Open Gaps (Target Invariants Not Yet Built)
- **`NeedRequeue` Automatic Promotion (`internal/app`)**: `NeedRequeue` is set by
  the repair stage but nothing in `internal/app` reads it — it is currently dead
  state. No job finalizer inspects it or promotes un-downloaded `.par2` volumes.
  Target: implement automatic `.par2` promotion and `StatusDownloading` transition,
  falling back to `Status = "Failed"` when no recovery blocks remain.
- **`ScriptCanFail == false` Authoritative Failure (`internal/postproc`)**: When a
  user script exits non-zero and `ScriptCanFail` is false, `ScriptStage.Run()` sets
  `StageLogEntry.Err` but does not set `job.FailMsg`, so `buildSummaryEntry` records
  `Status = "Completed"`. Fix: PR #275.
