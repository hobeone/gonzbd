# Lane 2 — `internal/postproc` and the extraction/repair pipeline

Scope: `internal/postproc/`, `internal/unpack/`, `internal/par2/`, `internal/directunpack/`,
`internal/deobfuscate/`, `internal/rarheader/`, `internal/cmdutil/`, `internal/fsutil/` (postproc-facing parts).

Read first: `AGENTS.md`, `docs/ARCHITECTURE.md` §Post-Processing + §External subprocess containment,
`docs/post_processing_spec.md`, `docs/go-standards.md` (all, incl. Lessons Learned §1–§7).

---

## Pipeline map

The registration order is in `/home/hobe/software/gonzbd/internal/app/stages.go:126-227`.
There are **12** stages, not 11 — `ARCHITECTURE.md:121-134` omits `rar_volume_recovery` and
lists `par2names` where the stage's `Name()` is `recover_par2_names`.

State is passed **entirely by mutation of the shared `postproc.Job` struct**
(`/home/hobe/software/gonzbd/internal/postproc/stages.go:695-810`). There is no per-stage
input/output type; every stage reads and writes the same 20-field struct. The orchestrator only
owns `StageLog` and the `OutputLines` scratch buffer drain
(`/home/hobe/software/gonzbd/internal/postproc/postproc.go:474-477`).

| # | Stage (`Name()`) | File | Mutates on `Job` | Reads / assumes from prior stages |
|---|---|---|---|---|
| 1 | `quickcheck` | `stage_quickcheck.go:33` | `QuickCheckRan`, `QuickCheckPassed`, `OutputLines`; on disk: relocates flat files into par2 subdirs via `par2.QuickCheckWithOptions` | Assembler finished; `Queue.Progress().FileAssembledCRC32` populated |
| 2 | `repair` | `stage_repair.go:243` | `ParError`, `NeedRequeue`, `RequeueBlocksNeeded/Reason`, `ConsumedFiles`, `Par2Renames`, `OutputLines`; on disk: par2 repair rewrites damaged files, writes `__ADMIN__/__verified__` | `QuickCheckRan`/`QuickCheckPassed`, `DirectUnpackSets/Failures/Skipped` |
| 3 | `rar_volume_recovery` | `stage_rarvolrecovery.go:39` | `OwnedFiles` (via `markRenamed`); on disk: renames RAR-magic files to `<Name>.partNNN.rar` | Assumes `unpack.Scan` finds *nothing*; assumes repair already had first crack at PAR2-based renaming |
| 4 | `unpack` | `stage_unpack.go:775` | `UnpackError`, `OwnedFiles`, `OutputLines`; on disk: extracts, deletes source archives, chmods tree | **Gates on `ParError`** (the only stage that gates on a prior stage's error before mutating) |
| 5 | `sample_cleanup` | `sample_cleanup.go:57` | on disk: deletes sample/proof files | `OwnedFiles` populated (else unrestricted) |
| 6 | `recover_par2_names` | `stage_par2names.go:27` | `OwnedFiles`; on disk: renames by 16K-MD5 match | Assumes `.par2` files still present (must precede `par2_cleanup`) |
| 7 | `par2_cleanup` | `stage_par2cleanup.go:44` | on disk: deletes `.par2` and par2 `.N` backups | **Gates on `ParError` and `UnpackError`** |
| 8 | `deobfuscate` | `stage_deobfuscate.go:30` | `OwnedFiles`; on disk: renames obfuscated files + subtitle pairing | Assumes unpack produced the real payload |
| 9 | `extension_cleanup` | `extension_cleanup.go:64` | on disk: deletes by extension, prunes empty dirs | `ConsumedFiles`, `OwnedFiles` |
| 10 | `finalize` | `stage_finalize.go:36` | `DownloadDir` (!), `OutputLines`; on disk: moves tree to `FinalDir`, or renames to `_FAILED_<name>` | **Gates on `ParError`/`UnpackError`/`FailMsg`** |
| 11 | `cleanup` | `stage_cleanup.go:26` | on disk: `RemoveAll(<DownloadDir>/__ADMIN__)` | **Gates on `ParError`/`UnpackError`/`FailMsg`**; depends on finalize having updated `DownloadDir` |
| 12 | `script` | `stage_script.go:82` | `OutputLines` | Uses the finalize-updated `DownloadDir` as `$PWD`/`SAB_FINAL_PROCESSING_DIR`; deliberately runs on failure with a status bitmask |

Pre-stage synthetic entries (`download`, `direct unpack`) and a post-stage `summary` entry are built
in `postproc.go:308-418`. Two hard pre-gates run before any stage: `FailMsg != ""` skips everything
(`postproc.go:537-548`) and an empty/unreadable `DownloadDir` skips everything
(`postproc.go:555-574`). `OwnedFiles` is seeded from an on-disk snapshot at `postproc.go:583-587`.

---

## Findings

### 1. The "each stage self-gates on ParError/UnpackError/FailMsg" architecture claim is false for 6 of 12 stages
`/home/hobe/software/gonzbd/docs/ARCHITECTURE.md:138`; evidence at
`stage_rarvolrecovery.go:39-49`, `sample_cleanup.go:57-65`, `stage_par2names.go:27-40`,
`stage_deobfuscate.go:30-40`, `extension_cleanup.go:64-79`.
**CONFIRMED** · lenses: architecture, concurrency/persistence fit.

Only `unpack` (ParError), `par2_cleanup` (both), `finalize` (all three) and `cleanup` (all three)
actually gate. Stages 3, 5, 6, 8, 9 have **no** error gate at all, and every one of them performs
destructive on-disk mutation (rename or delete). So when repair fails (`ParError = true`):

- `unpack` correctly skips, leaving the raw `.rar`/`.r00` volumes on disk;
- `rar_volume_recovery` may then rename every RAR-magic file to `<JobName>.partNNN.rar`;
- `recover_par2_names` renames by 16K-MD5;
- `sample_cleanup` deletes anything matching `(^|[\W_])(sample|proof)`;
- `deobfuscate` renames the leftover volumes toward the job's display name;
- `extension_cleanup` deletes `.nfo`/`.sfv`/etc.;
- and only *then* do `finalize` and `cleanup` announce "preserving files for retry"
  (`stage_finalize.go:93`, `stage_cleanup.go:37`).

The state being preserved has already been renamed and partially deleted by four stages. The
documented invariant and the code disagree, and the disagreement is exactly in the partial-failure
path the invariant exists to protect.

Direction: make the gate a property of the `Stage` (e.g. a `RunsOnFailure() bool` or a
`gate` field the orchestrator consults in `runStage`) rather than 12 independent hand-written
`if job.ParError` checks that can be — and were — forgotten. That also makes the matrix testable
in one table-driven test instead of 12. Effort: **M**.

---

### 2. `folder_rename`'s `_FAILED_` prefix silently breaks retry, and the code comment claims the opposite
`/home/hobe/software/gonzbd/internal/postproc/stage_finalize.go:82-92`.
**CONFIRMED** (traced finalize → `app.enqueuePostProc` path computation → `queue.Job.ResetForRetry`).
Lens: partial-failure state / data integrity.

`handleFailure` renames the job dir in place:

```go
failedDir := prefixDirName(job.DownloadDir, "_FAILED_")
if err := os.Rename(job.DownloadDir, failedDir); err == nil { ... }
```

with the comment "Files stay in the incomplete/download area (NOT moved to complete) so that retry
can find them." Retry cannot find them:

- `grep -rn "_FAILED_" internal/` matches only this file, `stage_script.go`'s doc comment, and
  `config/postproc.go`'s doc comment. **Nothing ever reads the prefix back.**
- `Application.enqueuePostProc` recomputes the working dir unconditionally as
  `filepath.Join(downloadDirBase, job.Name)` (`/home/hobe/software/gonzbd/internal/app/app.go:1089`) —
  no prefix.
- `queue.Job.ResetForRetry` (`/home/hobe/software/gonzbd/internal/queue/job.go`, the `ResetForRetry`
  method) **only resets articles where `progress.failed[i]` is true**. Articles already `done` stay
  done and are never re-dispatched.

So `RetryHistoryJob` re-downloads only the previously-failed articles into a *fresh, empty*
`<downloadDir>/<Name>`. The assembler writes those articles at their absolute offsets into files
that no longer contain the previously-downloaded bytes — producing sparse/truncated output that
then passes through post-processing as if it were a fresh download. The `_FAILED_` directory is
orphaned on disk forever.

`folder_rename` defaults to `false` (`internal/config/postproc.go:159`), so this is opt-in, which is
the only reason it is not a headline data-loss bug today.

Direction: either strip a `_FAILED_` prefix when computing the retry working dir, or (simpler and
matching the stated intent) do the `_FAILED_` rename only for jobs that are terminal, not for jobs
still eligible for retry. Effort: **S** for the strip; **M** to do it properly with a test that
proves retry recovers the partial bytes.

---

### 3. `rarheader.inspectViaUnrar` spawns `unrar` with no context, no timeout, no sandbox, and no cancellation — on untrusted archive data
`/home/hobe/software/gonzbd/internal/rarheader/rarheader.go:381-407`.
**CONFIRMED** (call path traced end to end).
Lenses: subprocess lifecycle, security-of-remote-data.

```go
var execCommand = exec.Command                       // :45 — NOT CommandContext
...
cmd := execCommand("unrar", "vt", "-p-", p)          // :385
var stdout, stderr bytes.Buffer                       // unbounded
err := cmd.Run()                                      // :391
```

Reachable path: `DeobfuscateStage.Run` → `deobfuscate.Deobfuscate` → `extractRARUsefulName`
(`/home/hobe/software/gonzbd/internal/deobfuscate/deobfuscate.go:588-616`) → for **every** regular
file in the download dir that has RAR magic → `rarheader.Inspect(path)`
(`rarheader.go:107-135`) → falls through to `inspectViaUnrar` whenever `rarengine` cannot parse the
header (encrypted headers, RAR2/4, or any malformed archive).

Consequences, all of which the rest of the codebase gets right elsewhere:
- **No cancellation.** Job cancel (`PostProcessor.Cancel`) and daemon shutdown (`Stop` → `wg.Wait`)
  both block indefinitely if this `unrar vt` wedges. This is the one subprocess in the pipeline the
  context plumbing cannot reach.
- **No sandbox.** It bypasses `cmdutil.BuildSandboxedCommand` entirely, so `strict_sandbox: true`
  does not apply — a documented containment guarantee silently has a hole.
- **No nice/ionice.** Bypasses `cmdutil.CmdConfig`.
- **Hardcoded `"unrar"`**, ignoring `opts.UnrarCommand`, `GONZBD_UNRAR_BIN`, and the
  `unrar-nonfree` fallback that `unpack.UnrarBin` implements (`internal/unpack/unrar.go:103-116`).
- **Unbounded `bytes.Buffer`** for stdout — `unrar vt` on a 100k-entry archive is bounded only by RAM.
- Fired **once per RAR-magic file**, so an obfuscated 100-volume set is 100 sequential invocations.

Direction: thread a `context.Context` through `Inspect`/`inspectViaUnrar`, route through
`cmdutil.BuildSandboxedCommand`, resolve the binary via `unpack.UnrarBin`, and cap output with
`cmdutil.NewLineStreamerCapped`. Effort: **M** (signature change ripples to `deobfuscate`).

---

### 4. No `WaitDelay` on any extraction subprocess — a surviving grandchild wedges `Wait()` and therefore shutdown
`internal/unpack/unrar.go:221-230`, `internal/unpack/sevenzip.go:117-127`,
`internal/par2/par2.go:367-374` and `:431-438`, `internal/postproc/script.go:288-359`.
**INFERRED** (reasoning from `os/exec` semantics; not reproduced).
Lens: subprocess lifecycle.

All five sites set `cmd.Stdout`/`cmd.Stderr` to a `*cmdutil.LineStreamer`, i.e. a non-`*os.File`
writer. `os/exec` therefore creates an `os.Pipe` and a copying goroutine, and `Wait` blocks until
that copy finishes — which requires *every* holder of the write end to close it, not just the direct
child. With `WaitDelay` unset (zero), the stdlib waits **indefinitely**.

Mitigations that exist today but are incidental rather than designed:
- The `script` stage does the right thing structurally — `Setpgid` + a `cmd.Cancel` that
  `kill(-pgid, SIGKILL)` (`script.go:296-305`, `script_unix.go:14-30`). But a script grandchild that
  calls `setsid` escapes the group and still holds the pipe → `cmd.Wait()` never returns →
  `PostProcessor.Stop()`'s `wg.Wait()` never returns → the daemon cannot shut down.
- unrar/7z/par2 rely on `bwrap --die-with-parent` (`cmdutil/sandbox_linux.go:24`) to reap
  grandchildren. **In the Docker image `bwrap` is absent by design** (`ARCHITECTURE.md:163-174`), so
  the mitigation is off exactly where it is most likely to be needed.

`internal/notifier/script.go:80` already sets `cmd.WaitDelay = 2 * time.Second`. That the notifier
does and the whole postproc pipeline does not is a straightforward inconsistency.

Direction: set `cmd.WaitDelay` (a few seconds) at the single chokepoint,
`cmdutil.BuildSandboxedCommand`/`BuildCommand`, so every extraction site inherits it; add
`Setpgid` + group-kill `Cancel` there too, matching what `script.go` already does. Effort: **S**.

---

### 5. Raw `root.Remove` / `os.Remove` on job cleanup paths — the NFS silly-rename protocol is bypassed
`internal/postproc/extension_cleanup.go:130` (`root.Remove(path)`),
`internal/postproc/extension_cleanup.go:170` (`root.Remove(dirPath)` in `cleanupEmptyDirs`),
`internal/postproc/sample_cleanup.go:112` (`root.Remove(p)`),
`internal/unpack/passwords.go:192` (`os.Remove(full)` in `cleanupPartialFiles`).
**CONFIRMED** · lens: Go standards §5 (a rule distilled from a real past bug).

`docs/go-standards.md` §5: *"Never use raw `os.RemoveAll` or `os.Remove` on job output directories,
cleanup paths, or temp directories. Always use `fsutil.RemoveAll`, `fsutil.Remove`, or
`fsutil.RemoveRootAll`."* `os.Root.Remove` is the raw syscall wrapper — it has none of the
EBUSY/ENOTEMPTY backoff or `.nfs*`/`.smbdelete*`/`.fuse_hidden*` tolerance in
`internal/fsutil/remove.go:110-137`. On an NFS-backed download dir, extension/sample cleanup will
log spurious `remove …: device or resource busy` warnings and `cleanupEmptyDirs` will leave
directories behind.

Note there is currently **no** `fsutil.RemoveRoot` for a single rooted file — only
`fsutil.RemoveRootAll` for trees (`remove.go:139`). That absence is probably why these four sites
went raw.

Direction: add `fsutil.RemoveRoot(root *os.Root, rel, fullPath string) error` mirroring
`RemoveRootAll`, and switch the four sites. Effort: **S**.

---

### 6. `rar_volume_recovery` is not PP-gated, so a PP=0/PP=1 job gets its filenames rewritten with no unpack ever happening
`internal/postproc/postproc.go:646-655` (`shouldSkipForPP` switches only on
`"quickcheck"`, `"repair"`, `"unpack"`); `internal/postproc/stage_rarvolrecovery.go:100-118`.
**CONFIRMED** · lens: architecture / partial-failure state.

`shouldSkipForPP`'s `default: return false` means `rar_volume_recovery` always runs. At PP=0
("download only") and PP=1 ("+repair"), `unpack` is skipped but this stage still executes
`os.Rename(p, "<JobName>.part%03d.rar")` on every RAR-magic file in the job. A user who explicitly
asked not to unpack gets their downloaded files renamed anyway. The rename is recorded in
`OwnedFiles` via `markRenamed` but the original names are not recoverable.

Direction: add `case "rar_volume_recovery": return pp < types.PPUnpack` — this stage exists solely to
make `unpack` work, so it should share `unpack`'s gate. Effort: **S**.

---

### 7. `CheckContainment` treats a broken symlink as a containment violation and fails the job
`/home/hobe/software/gonzbd/internal/fsutil/containment.go:40-45`, consumed at
`internal/postproc/stage_unpack.go:905-913`.
**INFERRED** (logic traced; no real-world archive tested).

```go
realPath, err := filepath.EvalSymlinks(path)
if err != nil {
    // If the target doesn't exist, that's still suspicious.
    return fmt.Errorf("containment: eval symlinks %s: %w", path, err)
}
```

`EvalSymlinks` returns `ENOENT` for a dangling symlink. The walk aborts, `CheckContainment` returns
non-nil, and `extractPendingArchives` sets `job.UnpackError = true` and calls
`cleanupContainmentViolation` — deleting the extraction's output. A legitimate release containing a
relative symlink to a file outside the archive (or to a file that itself was skipped because
`OverwriteFiles=false`) is therefore misclassified as a traversal attack.

The correct security posture for a dangling symlink is to check whether its *lexical* resolution
escapes the dir (`fsutil.PathWithin` on `filepath.Join(filepath.Dir(path), target)`), not to fail
closed on the whole job. Note the codebase already knows the lexical-vs-resolved distinction —
`cleanupContainmentViolation` (`stage_unpack.go:1337-1359`) reasons about it carefully and correctly.

Direction: on `EvalSymlinks` failure, `os.Readlink` and check the lexical target; only fail if it
escapes. Effort: **S**.

---

### 8. `CheckContainment` is a full recursive tree walk with a per-entry `EvalSymlinks`, run once per archive
`internal/postproc/stage_unpack.go:905`, called inside `extractPendingArchives`'s per-archive loop.
**CONFIRMED** · lens: performance / right-sizing.

A job with 100 RAR volumes producing 5,000 extracted files does 100 walks × 5,000
`EvalSymlinks` = 500k `lstat`-chains, all after the extraction that produced them. The check itself
is legitimate (archive contents are untrusted remote data — correctly *not* an over-engineering
finding), but it should be scoped to `res.ExtractedFiles` (which the code already has, and already
passes to `cleanupContainmentViolation`) rather than the whole tree, or hoisted to run once after
the final pass.

Direction: check only the paths the extraction reported, plus one whole-tree sweep at the end of the
stage. Effort: **S**.

---

### 9. The `Stage` interface is doing very little work; the pipeline is a straight-line function wearing an interface
`internal/postproc/stages.go:677-690`, `internal/app/stages.go:126-227`.
**CONFIRMED** · lens: right-sizing.

Evidence that the abstraction is not load-bearing:
- All 12 stages are constructed in one function, in one fixed order, with no reordering, conditional
  inclusion, or plugin registration. Disabled stages are still in the slice and no-op via a `toggle`
  (`toggle.go:878-889`) — so the interface is not even buying dynamic composition; a boolean field
  would do the same.
- The interface is only two methods, one of which (`Name()`) is a constant string used for a
  `switch` in `shouldSkipForPP` (`postproc.go:646`) and another `switch` for status mapping
  (`postproc.go:451-460`). Both switches are the orchestrator special-casing stages *by name* —
  the exact coupling an interface is supposed to eliminate.
- State flows through the shared mutable `Job` struct, not through the interface, so `Run`'s
  signature communicates nothing about what a stage consumes or produces.
- `builtStages` (`app/stages.go:232-243`) exists specifically to hand back concrete typed pointers to
  10 of the 12 stages so the app can call their `Apply`/`SetEnabled`/`SetCleanup` methods — i.e. the
  caller immediately un-does the abstraction.

The genuine value the interface *does* provide is the uniform `runStage` wrapper (timing, StageLog,
status update, output drain, cancellation check) and `WithPostProcStages` for tests. That's real but
it is what a `[]func(context.Context, *Job) error` plus a name would also give you.

I am not proposing a rewrite (that would be an architecture-migration-without-a-bug finding, which
is out of scope). I am flagging it as the honest answer to "is the abstraction earning its keep":
mostly no, and the `switch stage.Name()` sites are where it visibly leaks. If anything is changed
here, the highest-value change is to move the PP gate and the error gate onto the stage descriptor
(see Finding 1) so the two name-switches disappear.

---

### 10. `Job` is a 20-field mutable bag shared across 12 stages with no ownership discipline
`internal/postproc/stages.go:695-810`.
**CONFIRMED** · lens: concurrency/persistence fit.

`Job` carries: 3 error flags, 2 quickcheck flags, 3 requeue fields, 4 maps
(`ConsumedFiles`, `OwnedFiles`, `Par2Renames`, plus 3 DirectUnpack maps), a scratch `OutputLines`
slice, a mutable `DownloadDir` that `finalize` rewrites out from under later stages, and a callback.
Nothing documents which stage may write which field, and `DownloadDir` being rewritten mid-pipeline
(`stage_finalize.go:110`, `:129`, `:169`, `:178`) is a real footgun: stage 11 (`cleanup`) is
*correct* only because it happens to run after finalize and reads the updated value.

Direction: at minimum, document the write-ownership per field in the struct doc. Better: make
`DownloadDir` an explicit "working dir" that finalize returns rather than mutates. Effort: **M**.

---

### 11. `PostProcessor.History()` shallow-copies the `Job`'s maps
`/home/hobe/software/gonzbd/internal/postproc/postproc.go:211-222`.
**INFERRED** (no live race today; latent).
Lens: concurrency.

```go
cp := *j
cp.StageLog = make([]StageLogEntry, len(j.StageLog))
copy(cp.StageLog, j.StageLog)
```

Only `StageLog` is deep-copied. `ConsumedFiles`, `OwnedFiles`, `Par2Renames`,
`DirectUnpackSets/Failures/Skipped` are shared by reference with the worker's live `*Job`. Today
this is safe because `addHistory` runs *after* `processJob` returns (`postproc.go:269`) and
`jobFinalizer.finalize` (`internal/app/job_finalizer.go:34-40`) does not mutate the `postproc.Job`.
But the doc comment on `History()` claims it covers "currently in-flight jobs" (it does not — the
in-flight job is not in the slice), so the *intent* was the racy version, and the safety is
accidental. Note `go-standards.md` §1 records exactly this class of bug
("Calling `addHistory(job)` before `processJob(job)` exposed partially-initialized `StageLog`") —
the ordering fix landed but the shallow copy that made it dangerous did not.

Direction: fix the doc comment to match reality, or deep-copy the maps. Effort: **S**.

---

### 12. Stale doc comments referencing a function that no longer exists
`internal/postproc/postproc.go:73`, `:242`, `:284` all reference `popWithPause`; the function is
named `popJob` (`postproc.go:289`). **CONFIRMED** · lens: Go quality. Effort: **S**.

---

### 13. `internal/par2` passes `slog.Default()` into `BuildSandboxedCommand`
`internal/par2/par2.go:367` and `:431`. **CONFIRMED** · lens: Go standards (Idioms: "Pass
`*slog.Logger` via constructor; do not use a package-level global logger. All loggers must be
component-scoped"). `par2.RunOptions` has no logger field, so a sandbox-degradation warning
(`cmdutil/sandbox.go:53-54`, the warning added specifically for issue #97) is emitted **unscoped**
and will be dropped by component log filtering. The unpack package does this correctly
(`unrar.go:221` passes the scoped `log`). Effort: **S**.

`internal/fsutil/remove.go:96,102,124,159,165` likewise use package-level `slog.Warn`.

---

### 14. Hardcoded `"__ADMIN__"` in `CleanupStage` duplicates `constants.JobAdminDirName`
`internal/postproc/stage_cleanup.go:41` vs. `internal/postproc/verified.go:29`.
**CONFIRMED** · lens: Go quality. Changing the constant silently orphans the cleanup. Effort: **S**.

---

### 15. `LineStreamer`'s partial-line buffer is unbounded even when the total-output cap is set
`/home/hobe/software/gonzbd/internal/cmdutil/linestreamer.go:33-36,60-63`.
**INFERRED** (bounded in practice today).

`NewLineStreamerCapped` caps `s.full` but `s.buf` (the partial-line accumulator) grows without limit
until a `\n` arrives. `ReadString('\n')` does not split on bare `\r`, so a tool emitting a
`\r`-based progress bar accumulates the entire progress stream in `buf`. In practice this is
mitigated because both callers suppress progress (`unrar -idp` at `unrar.go:165`, `7z -bd -bsp0` at
`sevenzip.go:72-73`) — but the *user script* path (`script.go:308`) has no such control, and a
script emitting `\r` progress with no newline is entirely plausible. Effort: **S** (bound `buf` and
force-emit at the cap).

---

### 16. Extraction subprocesses have no `--` end-of-options separator
`unrar.go:194` (`args = append(args, opts.ExtraArgs...)` then `archive.MainFile`),
`sevenzip.go:87-90`, `par2.go:346-349`.
**INFERRED**, low. Filenames come from NZB subjects (untrusted remote) but reach argv as absolute
paths derived from `filepath.WalkDir(job.DownloadDir, …)` (`unpack/detect.go:96-111`), so a leading
`-` is not currently reachable as long as `download_dir` is resolved absolute at startup. Adding
`--` (or `-@` for 7z-style) before the positional file arguments is a one-line hardening that
removes the dependency on that invariant. Effort: **S**.

---

### 17. The archive password is passed on argv, visible in `/proc/<pid>/cmdline`
`internal/unpack/unrar.go:158-161` (`-p<password>`), `sevenzip.go:64` (`-p<password>`).
**CONFIRMED**, low severity given the single-user model. Flagged only because the codebase goes to
real trouble to redact the password from the *displayed* command line (`formatCmdLine`,
`formatArgs`) and from the script env (`redactScriptSecrets`) — so the intent to protect it exists,
and the argv channel is the gap. unrar supports reading the password from stdin (`-p-` + stdin);
7-Zip does not have a clean equivalent. Not worth fixing unless there's a concrete threat. Effort: **M**.

---

### 18. `moveFileByFile` lacks the containment guard that `go-standards.md` §2 mandates for recursive delete
`internal/postproc/stage_finalize.go:170` (`_ = fsutil.RemoveAll(job.DownloadDir)`).
**INFERRED**, low.

`go-standards.md` §2: *"Check directory containment before recursive delete. `SortStage` deleted
`FinalDir` when it was inside `origDir`. Always verify `!strings.HasPrefix(targetDir, sourceDir)`
before removing a directory tree."* That guard is not present. If `complete_dir` were configured
inside `download_dir`, `dest` would be inside `job.DownloadDir` and the `RemoveAll` would delete the
files just moved there. Today this is unreachable because `os.Rename(src, dstInsideSrc)` returns
`EINVAL`, which `fsutil.IsRenameMergeNeeded` (`internal/fsutil/move.go:36`) does not accept, so the
fallback path is never entered. There is no config-level validation that `complete_dir` is not
nested under `download_dir` (grepped `internal/config/`). The safety rests on an incidental errno
rather than the explicit check the lessons-learned entry demands. Effort: **S**.

---

### 19. `directunpack`'s volume feeder hands `*os.File` handles off without the Tracking-Slice cleanup
`/home/hobe/software/gonzbd/internal/directunpack/directunpack.go:584-616`.
**INFERRED** (depends on `rarengine.StreamDecompressor` closing each `io.ReadCloser`; not verified —
`rarengine` is an external module).

The feeder goroutine opens one `*os.File` per volume and sends it on `volumesChan`. It closes the
handle only on the `ctx.Done()` branch (`:610`). Handles already consumed by
`rarengine.NewStreamDecompressor` are the engine's responsibility. `go-standards.md` §5's
Tracking-Slice Pattern says explicitly: *"never rely on downstream readers … to close them on early
return/error paths. Always accumulate opened handles in a slice with a deferred cleanup block."*
The code does not. The in-code comment at `:505-508` shows the author already fixed one leak here
(the feeder's own handle) — the delivered handles were not covered by that fix.

Direction: accumulate the opened handles in a slice inside `extractSet` with a `defer` that
best-effort closes them all; a double-close on an `*os.File` returns an error rather than panicking,
so this is safe to layer on top of whatever `rarengine` does. Effort: **S**.

---

### 20. `deobfuscate` runs on a failed job's raw archive volumes
`internal/postproc/stage_deobfuscate.go:30-40` (sub-case of Finding 1, called out because the
consequence is specific).
**INFERRED**.

When repair fails, `unpack` is skipped and the download dir still holds `movie.part01.rar …
movie.part40.rar`. `deobfuscate` then runs its biggest-file/extension-guess heuristics over those
volumes and may rename them toward `job.Queue.Name`. Since `par2_cleanup` correctly *kept* the
`.par2` files, a later manual or automated repair still has a path back via par2's checksum/hash16k
matching (`par2.QuickCheck`'s passes 3 and 4 and `listNonPar2Files`'s extra-file passing at
`stage_repair.go:304`) — so this is degradation, not destruction. Still, it is unnecessary work on
data the pipeline has just declared unusable, and it is the stage that triggers Finding 3's
uncancellable `unrar vt` storm.

---

### 21. `cleanupArchives` deletes source archives even when the job as a whole failed
`internal/postproc/stage_unpack.go:1018-1037`.
**CONFIRMED** but likely correct — noted for completeness.

`u.cleanup.Load() && len(allSuccessful) > 0` deletes the parts of every archive in `allSuccessful`,
which by construction only contains archives that extracted cleanly and passed containment. So the
`go-standards.md` §2 rule *"Never delete an archive on partial extraction failure"* is honoured at
per-archive granularity. Recording it here so a future reader does not have to re-derive it.

---

## Subprocess lifecycle audit

| Site | Binary | ctx / cancellation | Pipe draining | Timeout | Reaping / process group | Sandbox |
|---|---|---|---|---|---|---|
| `unpack/unrar.go:221-232` | `unrar` (or `bwrap unrar`) | ✅ `exec.CommandContext` via `cmdutil.BuildSandboxedCommand` | ✅ `LineStreamer` on both fds, `Flush()` after `Run()` (`:233`) | ❌ none | ⚠️ direct child only; no `Setpgid`, no `WaitDelay`. Grandchildren reaped only by `bwrap --die-with-parent` (Finding 4) | ✅ `BuildSandboxedCommand` |
| `unpack/sevenzip.go:117-129` | `7zz`/`7z` | ✅ | ✅ | ❌ | ⚠️ same as unrar | ✅ |
| `par2/par2.go:367-375` (verify) | `par2` | ✅ | ✅ | ❌ | ⚠️ same | ✅ but with `slog.Default()` (Finding 13) |
| `par2/par2.go:431-439` (repair) | `par2` | ✅ | ✅ | ❌ | ⚠️ same | ✅, same logger issue |
| `postproc/script.go:288-359` | user script | ✅ + custom `cmd.Cancel` | ✅ `NewLineStreamerCapped(…, 512 KiB)`; **no `Flush()` after `Wait()`** — the last unterminated line is dropped from `LogBody`/`LastLine` (minor, unlike every other site which does flush) | ❌ none (an infinite-loop script blocks postproc until job-cancel or shutdown) | ✅ **best in tree**: `Setpgid` (`script_unix.go:18`) + `kill(-pgid, SIGKILL)` (`:26`). Still no `WaitDelay`, so a `setsid` grandchild wedges `Wait` (Finding 4) | ❌ n/a by design (operator-supplied script) |
| `rarheader/rarheader.go:385-391` | `unrar vt` | ❌ **`exec.Command`, no ctx at all** | ⚠️ unbounded `bytes.Buffer` | ❌ | ❌ nothing | ❌ **bypasses `cmdutil` entirely** — see Finding 3 |
| `par2/detect.go:52,56` | `par2 -h`, `par2 -V` | ✅ `CommandContext` | `CombinedOutput` (unbounded, but `-h`/`-V` output is trivially small) | ❌ | direct child | ❌ (capability probe, not archive-driven — acceptable) |
| `unpack/version.go:65,88` | `7z`/`unrar` version probe | ✅ `CommandContext` | `CombinedOutput`, unbounded | ❌ | direct child | ❌ (probe — acceptable) |
| `notifier/script.go:76-80` | notify script | ✅ | — | ❌ | ✅ **`cmd.WaitDelay = 2s`** — the only site in the repo that sets it | ❌ n/a |

Zombie processes: none of the sites leak zombies — every one calls `Run()` or `Wait()`, so the child
is always reaped. The hazard is the inverse (a `Wait()` that never returns), covered in Finding 4.

`cmdutil` argument construction is sound: `ValidatePriorityArgs` (`runner.go:100-118`) rejects
anything outside `[A-Za-z0-9- \t]` in nice/ionice strings; `ParseExtraParams` (`:123-136`) requires
a leading `-`; `ValidateUnrarParams` (`:148-162`) enforces SABnzbd's `-mlp`/`-om`/`-ri` allowlist.
No shell is ever involved — `exec.Command` with an explicit argv throughout. **No command-injection
finding.** The only argv gaps are the missing `--` separator (Finding 16) and the argv-visible
password (Finding 17).

`bwrap` wrapping (`cmdutil/sandbox_linux.go:6-29`) is well-constructed: `--ro-bind / /`,
`--bind targetDir targetDir`, explicit `--proc`/`--dev` (with a comment explaining exactly why the
non-recursive `--ro-bind /` makes them necessary), `--unshare-all`, `--die-with-parent`, and a `--`
before the command. `ErrSandboxMisconfigured` (`sandbox.go:19-21`) correctly refuses to silently
degrade a wiring bug into "sandboxing off". `TMPDIR` is redirected into the target dir
(`sandbox.go:62`). The Docker caveat in `ARCHITECTURE.md:163-174` is accurate and matches the code
(`wrapSandbox` only `LookPath`s `bwrap`, it does not verify namespace creation works).

---

## Positive / load-bearing

1. **`writeEntrySafely` (`internal/unpack/write_entry.go:143-232`) is the best code in this lane.**
   Temp-file-then-atomic-rename with a `published` flag and a `defer` that cleans up on every error
   or panic path; `os.Root`-scoped so it cannot escape `outDir` via `..`, absolute paths, or a
   symlinked component; CRC verify *before* the publishing rename; decompression-bomb enforcement
   both projected (`projectedBombCheck`) and continuous (`boundReader`); context-aware copy with
   `ENOSPC` classification; and the `drainOnSkip` distinction is documented with the actual reason
   (7z solid-archive stream reuse). Three extractors were unified onto it. This is exactly the
   "consolidate subsystem boilerplates" rule from `go-standards.md` §6 done right.

2. **`os.Root` is used pervasively and correctly** — `snapshotOwnedFiles`, `applyPermissions`,
   `SampleCleanupStage`, `ExtensionCleanupStage`, `RecoverPar2NamesStage`,
   `directunpack.extractEntries`, `deobfuscate.extractRARUsefulName`. Combined with
   `unpack.SanitizedPath` (a constructor-enforced validated type — `sanitized_path.go:11-30`, the
   right way to make an invariant unforgeable), archive path containment is genuinely defence-in-depth
   rather than a single check.

3. **`cleanupContainmentViolation` (`stage_unpack.go:1329-1359`)** reasons correctly about the
   symlink case that most implementations get wrong: it guards *lexically* on the extracted path so
   an escaping symlink is unlinked rather than followed, and the 8-line comment explains precisely
   why following it would let a malicious archive destroy arbitrary files. This is the single most
   security-thoughtful function in the lane.

4. **`Start`/`Stop`/`Cancel` lifecycle (`postproc.go:113-181`).** `wg.Go` inside the `busyMu`
   critical section with a comment explaining the WaitGroup zero-counter ordering hazard; `Stop`
   snapshotting `started`/`cancel` under the lock then explicitly marking
   `// --- No lock held below this line ---` per `go-standards.md` §1; per-job derived contexts so
   `Cancel(jobID)` aborts one job without touching the worker; `setBusyWithJob` called *inside*
   `popJob` to close the "queue empty but not yet busy" window — that is literally the
   `go-standards.md` §1 rule ("Set state atomically with its observable effect") applied.

5. **Shutdown-vs-cancel are distinguished at `postproc.go:250-267`** with an explicit comment: a
   shutdown-interrupted job is left in the active queue for crash recovery (`PostProc=true` survives
   because `maybeFinalize` forces a queue save at `app.go:1029`); a `Cancel`-ed job is dropped
   without history. Getting that distinction right is not obvious and it is documented at the
   decision point.

6. **`maybeFinalize` calls `assembler.CloseJobHandles` before post-processing starts**
   (`app.go:1020-1022`) — the Control-Message Pattern from `go-standards.md` §5, with the correct
   stated rationale (avoiding NFS silly-rename artifacts on open files).

7. **`VerifiedSets` (`verified.go`)** persists par2 verification across crashes with
   `fsutil.WriteAtomicBytes`, and the `//lockio:` exception on `MarkVerified` is one of the best
   suppression comments I have read — it explains the lost-update hazard that decoupling the write
   would introduce and explicitly defers the redesign until a concurrent caller exists.

8. **`shouldFallbackToExternal` (`stage_repair.go:511-522`)** encodes a genuinely subtle decision
   (a `NeedMoreBlocks` verdict is deterministic Reed-Solomon arithmetic that the external binary
   must agree with, so retrying only burns a scan; a *parse* failure is ambiguous and does warrant a
   retry) with the reasoning written down.

9. **`OwnedFiles` / `ConsumedFiles`** are the right answer to the cleanup-safety problem, seeded
   automatically from a disk snapshot, extended by every producing stage, and consulted by both
   cleanup stages with a documented nil-means-untracked escape hatch for tests.

10. **`RepairStage.Apply` / `UnpackStage.Apply` (`stage_repair.go:222`, `stage_unpack.go:752`)** —
    the migration from per-field `Set*` methods to one atomic config swap, with a doc comment naming
    the exact hazard it removes ("a running job could interleave with" many independently-locked
    writes). Both `Run` methods snapshot under `RLock` and release before doing any I/O
    (`stage_unpack.go:796-803`), satisfying `go-standards.md` §1's no-I/O-under-lock rule.

11. **`ppQueue` (`queue.go`)** is a textbook cap-1-notify channel queue: single documented consumer,
    non-blocking coalescing notify, `Pop` always re-checks after a wakeup so a coalesced
    double-push cannot be lost, nils out popped slots for GC.

---

## Open questions for synthesis

1. **Finding 2 needs a queue-lane cross-check.** I traced `folder_rename` → `_FAILED_` rename →
   `RetryHistoryJob` → `ResetForRetry` (only failed articles reset) → fresh empty dir. What I did
   *not* verify is whether the assembler detects a missing/zero-length target file at
   `<downloadDir>/<Name>/<file>` and refuses to write, or whether it happily `WriteAt`s into a
   newly-created sparse file. If the assembler fails loudly, the finding downgrades from silent
   corruption to a confusing error. Someone on the assembler/queue lane should confirm.

2. **Does anything besides `RetryHistoryJob` re-enter a job's download dir?** The "files stay for
   retry" rationale appears in `stage_finalize.go:82-84` and `stage_cleanup.go:37`. If there is no
   retry path that reuses on-disk bytes at all, then a large amount of preservation logic
   (par2_cleanup's gates, cleanup's gates, finalize's `handleFailure`) is defending an outcome that
   never happens — which would be a right-sizing finding rather than a correctness one.

3. **`rarengine`'s `io.ReadCloser` ownership contract** (Finding 19) is unresolved because
   `rarengine` is an external module I did not read. If it does not close the volumes it consumes,
   that is a confirmed FD leak proportional to volume count on every DirectUnpack error path.

4. **PP-level semantics for stages 3, 5, 6, 8, 9.** Finding 6 argues `rar_volume_recovery` should
   inherit `unpack`'s PP gate. Whether `deobfuscate`/`sample_cleanup`/`extension_cleanup` should
   also be gated at PP=0 ("download only") is a product decision, not a code one — the current
   `default: return false` in `shouldSkipForPP` means PP=0 still renames and deletes.

5. **`internal/app/job_finalizer.go:66,77`** uses raw `os.Remove(jobPath)` on an admin-dir path.
   That is the app lane's territory but it is the same §5 silly-rename rule as Finding 5; worth
   folding into whatever fix that finding gets.

6. **`internal/postproc/stage_repair.go:597-640` (`cleanupPar2Backups`)** deletes par2 `.N` backup
   files anywhere in the download dir, guarded only by "the repaired original exists alongside".
   It does *not* consult `job.OwnedFiles`, unlike the two cleanup stages. I could not construct a
   case where that matters (the dir is job-exclusive), but the inconsistency with the surrounding
   ownership discipline is worth a second opinion.

---

## git status proof

```
$ git status --short
```

(empty output — no files created, modified, deleted, or staged in the repository; the only file
written this session is this report, which lives outside the repo under
`/tmp/claude-1028/.../scratchpad/review/`)
