# RAR/Tar Improvements from SABnzbd/sabctools Upstream Review Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port five improvements identified by reviewing the last ~2 months of commits in `../sabnzbd` (the reference Python implementation) and `../sabctools` (its C-extension companion), adapted to gonzbd's Go architecture rather than transliterated. Four are concrete implementation tasks (Tasks 1–4); the fifth (Task 5) is an investigation spike whose deliverable is a written go/no-go recommendation, not necessarily code.

**How this plan was produced:** A prior conversation (not preserved in this repo) diffed `git log --since="2 months ago"` in `../sabnzbd` and `../sabctools` (paths relative to this repo's parent directory — see Reference Directories below), identified RAR/tar-handling commits, and cross-referenced each against gonzbd's existing implementation to separate "already handled" from "genuine gap." Specific upstream commit hashes are cited per task below so an agent can pull the exact diff for reference — **read intent, do not transliterate** (per `AGENTS.md` § Reading Python for Reference: Python's threading/exception model is not Go's, and CPython C-extension reference-counting fixes have no Go equivalent at all).

**Architecture:** Tasks are independent and touch different subsystems — they can be parallelized across multiple agents/sessions with no ordering dependency, except where a task explicitly says otherwise (Task 1 has an internal ordering constraint between its two repos). Each task is TDD'd individually per `AGENTS.md` § Red-Green Discipline: write the failing test first, watch it fail for the right reason, implement, confirm green, run the full package suite, commit.

**Tech Stack:** Go 1.26.4, standard `testing` package (table-driven, no mocking framework), `github.com/hobeone/rarengine` (pure-Go RAR3/RAR5 library, separate GitHub repo/module already vendored as a dependency).

## Reference Directories — Read Before Starting Any Task

All paths below are relative to this repo's own location (i.e. siblings of `gonzbd/` under the same parent directory) rather than absolute, since this plan may run on machines where that parent directory lives somewhere other than `/home/hobe/software`. If a sibling directory isn't present at the expected relative path on the machine running this plan, stop and ask the user where it lives rather than guessing.

- **This repo:** `.` (`gonzbd/`) — obviously.
- **`../rarengine`** — the pure-Go RAR3/RAR5 library gonzbd depends on (`github.com/hobeone/rarengine` in `go.mod`). It is a **separate GitHub repo** (`hobeone/rarengine`), not a vendored copy — changes here require their own commit/PR/release in that repo, then a `go.mod` version bump in gonzbd. Relevant files: `header.go` (`ArchiveHeader`, `FileHeader`, `ParseArchiveHeader`, `ParseFileHeader` — all exported), `header_crypt.go` (`CryptHeader`, `ParseCryptHeader` — exported; `headerKeyFromPassword` — unexported), `decompressor.go` (`pbkdf2HmacSha256`, `verifyEncCheck` — both unexported), `engine_rar5.go` (where per-file password verification currently happens, inline, during extraction).
- **`../sabnzbd`** — the reference Python implementation. Read for *intent*, not line-by-line translation (`AGENTS.md` § Reading Python for Reference). Specific files/commits cited per task. This is the same directory `AGENTS.md` itself refers to as `../sabnzbd/sabnzbd/`.
- **`../sabctools`** — SABnzbd's C-extension companion (yEnc decoder, RAR password key-derivation natives). Most of its recent commits are CPython reference-counting fixes with **no Go equivalent** — cited only where a *behavioral* (not memory-management) fix is relevant.

## Global Constraints (from `AGENTS.md` — apply to every task below)

- Every `.go` file touched: `goimports -w <file>`, `go fix ./...`, `go build ./...` immediately after editing.
- Quality gates before any commit: `go vet ./...`, `go test -race ./...` (scoped to the touched package during development; full suite before the task's final commit), `golangci-lint run ./...`.
- Conventional Commits 1.0.0: `<type>(<scope>): <description>`, lowercase, imperative, ≤72 chars, ending with:
  ```
  Co-Authored-By: Claude <noreply@anthropic.com>
  ```
- Red-Green discipline: every test must be observed to fail for the right reason before the fix lands. "Fails to compile because the function doesn't exist yet" is a valid RED for new functions.
- `gremlins unleash --timeout-coefficient 100 --diff origin/main`, scoped to the touched package, is a required pre-push gate per `AGENTS.md` — run it before considering a task's final commit done. **Never** run it unscoped (`./...`) — it will exhaust `/tmp`.
- Any config field added/changed: update `gonzbd.yaml` inline comments, `test/fixtures/gonzbd.yaml`, `docs/sabnzbd_spec.md` §9.x, and — if it has a UI counterpart — `internal/config/ui_contract_test.go`'s `uiKeywords` list and the matching Svelte `keyword=` prop. Run `go test ./internal/config/ -run 'TestUI|TestAllFlat'`.
- Database/persistence format changes: new `goose` migration only, never edit existing migration files. (Not expected to apply to any task below, but flagging per policy.)
- Decisions matching `AGENTS.md`'s "Decisions that must be escalated" list (new external dependencies, public interface changes between packages, departing from `docs/ARCHITECTURE.md`) must stop and present the Decision Needed format to the user rather than proceeding unilaterally. Task 1 has one flagged explicitly.

---

## Task 1: Cheap RAR password pre-verification before falling back to external `unrar`

**Files:**
- Cross-repo, Step A: `../rarengine/header_crypt.go`, `decompressor.go` (export new API)
- Cross-repo, Step B: `go.mod` (this repo — bump `github.com/hobeone/rarengine` version)
- Modify: `internal/rarheader/rarheader.go` (new `VerifyPassword` wrapper)
- Modify: `internal/unpack/passwords.go` (`withPasswords` — use pre-verification to skip subprocess spawns for known-wrong candidates)
- Test: `internal/rarheader/rarheader_test.go`, `internal/unpack/passwords_test.go`
- Reference only, do not modify: `internal/unpack/go_unrar.go`, `internal/rarengine`'s `engine_rar5.go` (already does the equivalent check lazily, see Background)

**Background:**

Upstream, `../sabnzbd` (commits `e9ff8a159`, `8a92f4ac7`) and `../sabctools` (`f598d3b`) added the ability to verify a RAR password against the archive's own embedded check-value — a PBKDF2-HMAC-SHA256 derivation folded against a stored check value per the RAR5 spec, or an SHA1-based check for RAR3 — *before* attempting real extraction. This turns "try password N, run a full extraction, inspect failure" into a cheap in-process hash check, and lets SABnzbd reject every wrong password in a list without spawning `unrar` at all.

**gonzbd already has this natively for the `GoUnRAR` path** (pure-Go extraction via `rarengine`, `UseGoRAR: true` by default):
- `rarengine/header_crypt.go:headerKeyFromPassword` verifies the password against `CryptHeader.CheckValue` at header-decrypt time for header-encrypted archives.
- `rarengine/engine_rar5.go:newDecompressionReader` (~line 274) verifies the password against each file's `EncCheck` value before handing back a decompression reader — a wrong password fails on the very first `Read()`, before any decompression or disk write.

**The actual gap is narrower than it first looks:** the external-`unrar`-subprocess path (`internal/unpack/unrar.go:UnRAR`, driven through `UnRARWithPasswords`) has none of this — it always spawns the real binary per candidate password and waits for it to fail. This path is reachable whenever `opts.UseGoRAR` is false, or via `GoRarFallback` after a Go-native failure. There's also no cross-attempt KDF-result caching anywhere in `rarengine` (sabctools added `lru_cache` around `rar3_s2k`/`rar5_s2k` upstream — useful when the same (password, salt) pair gets re-derived, e.g. checking multiple files in one archive with the same password).

**Design — three sub-steps, each independently valuable:**

1. **Export a password-verification primitive from `rarengine`.** The check logic (`verifyEncCheck`, `pbkdf2HmacSha256`) already exists but is unexported, private to that package. Add an exported function, something like:
   ```go
   // VerifyPassword reports whether password matches the archive's embedded
   // password check value, without decompressing any file content. It
   // requires that a check value is present (RAR5 archives created with
   // "store password check value" enabled, the default for modern RAR).
   // Returns (verified bool, hasCheckValue bool, err error) — hasCheckValue
   // distinguishes "definitely wrong" from "archive has no check value, we
   // can't tell, try it for real."
   func VerifyPassword(ch *CryptHeader, password string) (verified bool, err error)
   func VerifyFilePassword(fh *FileHeader, password string) (verified bool, err error)
   ```
   Study the exact field names/semantics already in `header_crypt.go` and `engine_rar5.go` lines ~274-296 before designing the signature — reuse the existing private helpers verbatim, just add exported entry points. RAR3 support (SHA1-based, see sabctools `f598d3b` and `rarfile`'s `Rar3Sha1`) is a stretch goal — the RAR5 case alone covers the common, modern format.

   **⚠️ Decision needed before this sub-step lands:** this changes `rarengine`'s public API, which is a separate GitHub repo (`hobeone/rarengine`). Per `AGENTS.md`'s escalation list ("changing public interfaces between packages"), confirm with the user whether to (a) extend `rarengine`'s public API and cut a new release, requiring a `go.mod` bump in gonzbd, or (b) duplicate the minimal PBKDF2+fold logic directly inside gonzbd's `internal/rarheader` package using Go's `crypto/pbkdf2` (confirm it exists in the stdlib at Go 1.26 — if not, this option needs `golang.org/x/crypto/pbkdf2`, itself a new-dependency decision) to avoid a cross-repo change. Option (a) is recommended — it keeps the crypto logic in one place — but is a two-repo task requiring its own PR review in `rarengine` first.

2. **Wrap it in `internal/rarheader` for gonzbd's use.** Add `rarheader.VerifyPassword(mainFilePath, password string) (verified, hasCheckValue bool, err error)`, following the existing pattern in `rarheader.go:InspectRar5` (open file, feed into `rarengine.NewStreamDecompressor`, read the archive/crypt/file headers, call the new `rarengine` exported function). Test against real fixture archives (check `internal/unpack/testdata` and `test/fixtures` for existing password-protected RAR5 test archives before generating new ones).

3. **Wire into `internal/unpack/passwords.go`.** In `withPasswords`, before calling `extract()` for each candidate (specifically for the `UnRARWithPasswords`/`UnRAR` external-subprocess path — `GoUnRARWithPasswords` doesn't need this, it already fails fast internally per Background), call `rarheader.VerifyPassword` first. If `hasCheckValue && !verified`, skip straight to the next candidate without spawning `unrar` at all — log this distinctly from a real subprocess-detected wrong password. If `!hasCheckValue` (older RAR3/no-check-value archive), fall through to the existing subprocess-based flow unchanged. This must not change behavior for any password that *does* eventually succeed — only skip subprocess spawns for candidates the pre-check can prove wrong.

**Steps:**

- [ ] Step 1: Present the Decision Needed prompt above to the user; get a ruling on cross-repo API extension vs. in-package duplication before writing any code.
- [ ] Step 2 (if cross-repo chosen): In `../rarengine`, write a failing test for the new exported `VerifyPassword`/`VerifyFilePassword` functions against a fixture archive with a known password, confirm RED (function doesn't exist), implement by extracting the existing private logic, confirm GREEN, run rarengine's own test suite, commit, tag/release, bump `github.com/hobeone/rarengine` in gonzbd's `go.mod`/`go.sum` (`go get github.com/hobeone/rarengine@<new-version>`).
- [ ] Step 3: Write a failing test in `internal/rarheader/rarheader_test.go` for `rarheader.VerifyPassword` against a real password-protected RAR5 fixture (correct password → verified=true; wrong password → verified=false; confirm it fails for the right reason before implementing). Implement, confirm GREEN.
- [ ] Step 4: Write a failing test in `internal/unpack/passwords_test.go` proving that `UnRARWithPasswords` given a password list with N wrong candidates before the correct one does **not** spawn the `unrar` subprocess for the pre-verifiably-wrong candidates (use a fake/counting `extractFunc` to assert call count, following existing test patterns in that file). Confirm it fails against current code (which spawns every candidate), then wire in the pre-check, confirm GREEN.
- [ ] Step 5: Run `go test -race ./internal/rarheader/... ./internal/unpack/...`, `golangci-lint run ./...`, `gremlins unleash --timeout-coefficient 100 --diff origin/main` scoped to changed packages.
- [ ] Step 6: Commit (`perf(unpack): skip unrar subprocess for pre-verifiably-wrong RAR5 passwords`), citing benchmark evidence if you gather any (e.g. time saved with a 10-password list against a wrong-password-heavy archive).

**Stretch (do not block the above on this):** cross-attempt KDF-result caching in `rarengine` (mirroring sabctools' `lru_cache` on `rar3_s2k`/`rar5_s2k`) — only worth doing if profiling shows repeated key derivation for the same (password, salt) pair is actually hot; the primary win above is subprocess avoidance, not KDF speed.

---

## Task 2: Restrict extension-cleanup deletion to files this job actually owns

**Files:**
- Modify: `internal/postproc/extension_cleanup.go` (`ExtensionCleanupStage.Run`)
- Modify: `internal/postproc/sample_cleanup.go` (same pattern, same fix)
- Investigate first: `internal/postproc/stages.go` (`Job` struct — `ConsumedFiles` already exists as an analogous allowlist for repair/join files), `internal/queue` and `internal/downloader` (how is `job.DownloadDir` allocated — is it provably exclusive to one job, ever?)
- Test: `internal/postproc/extension_cleanup_test.go`, `internal/postproc/sample_cleanup_test.go`

**Background:**

Upstream, `../sabnzbd` commit `5b3cf86f6` ("Track files during cleanup to prevent removing unrelated files", #3462) fixed `cleanup_list()`, which used to glob-delete *any* file in the working directory matching a cleanup extension, regardless of whether that file belonged to the current job. The fix threads an explicit `filelist` of the job's own tracked files through `cleanup_list()` and restricts deletion to it.

gonzbd's `ExtensionCleanupStage.Run` (`internal/postproc/extension_cleanup.go` line ~89) and the equivalent in `sample_cleanup.go` still do the old thing: `fs.WalkDir(root.FS(), ".", ...)` over the **entirety** of `job.DownloadDir`, deleting any matching extension found, with only one existing protection — `job.ConsumedFiles` (files consumed by repair/join operations, checked at line ~117). There is no positive allowlist of "files this job's download/extraction actually produced."

**This needs an investigation step before deciding the fix is even necessary**, because the blast radius depends entirely on whether `job.DownloadDir` can ever be non-exclusive to one job. Read how `DownloadDir` gets allocated (likely under a per-job subdirectory of the incomplete-downloads root — check `internal/queue` job creation and `internal/downloader`'s write-target logic) before assuming this is a live bug rather than defense-in-depth. Regardless of the answer, adding a tracked-files allowlist is cheap and mirrors the existing `ConsumedFiles` pattern, so it's worth doing either way — but the investigation determines whether this is `fix(postproc):` (real bug) or `refactor(postproc):`/hardening (no reachable bug today, but matches upstream's model and protects against future changes that make `DownloadDir` sharing possible).

**Steps:**

- [ ] Step 1: Investigate `job.DownloadDir` allocation (`internal/queue`, `internal/downloader`, `internal/postproc/postproc.go` around where `Job` gets constructed from a `queue.Job`). Write a short note (in the commit body, not a separate doc) on whether it's provably per-job-exclusive today.
- [ ] Step 2: Design how "files this job owns" gets tracked. Likely candidates: (a) the assembler/downloader already knows which files it wrote for this job — thread that list through into `postproc.Job` as a new field (e.g. `Job.OwnedFiles map[string]struct{}`, populated alongside `ConsumedFiles`), or (b) snapshot `DownloadDir` at job-start (before any postproc stage runs) as the ground truth, similar to `internal/unpack/passwords.go`'s existing `snapshotDir`/`diffSnapshot` helpers — reuse those if suitable rather than inventing a new mechanism.
- [ ] Step 3: Write a failing test: place a file in `job.DownloadDir` that matches a cleanup extension but is **not** one of the job's own files (simulating the upstream bug scenario), run `ExtensionCleanupStage.Run`, assert the unrelated file survives. Confirm it fails against current code (file gets deleted), then implement the allowlist check, confirm GREEN.
- [ ] Step 4: Repeat Steps 2-3's test pattern for `sample_cleanup.go`.
- [ ] Step 5: Full package test suite (`go test -race ./internal/postproc/...`), lint, gremlins scoped to `internal/postproc`.
- [ ] Step 6: Commit — `fix(postproc): ...` if Step 1 found a live exposure, `refactor(postproc): ...` if it's hardening against a currently-unreachable case. State which in the commit body.

---

## Task 3: yEnc `name=` field trailing-whitespace/null trim

**Files:**
- Modify: `internal/decoder/decoder.go` (`parseKeyValues`, line ~430)
- Test: `internal/decoder/decoder_test.go`

**Background:**

`../sabctools` commit `bc24612` ("Consistent empty file name behaviour, empty lines remain None and strip trailing whitespace", #189) changed the yEnc `name=` header field trim from stripping only trailing null bytes to stripping `" \t\r\n\0"` (space, tab, CR, LF, null) — a poster with a trailing space/tab/null in the yEnc header previously left a dirty filename.

gonzbd's `internal/decoder/decoder.go:parseKeyValues` (line ~430) currently does:
```go
val := bytes.TrimRight(rest, "\r\n")
```
for the `name` field. This should match sabctools' fixed character set.

**Steps:**

- [ ] Step 1: Write a failing test in `internal/decoder/decoder_test.go` (or extend an existing yEnc header-parsing table test) with a `name=` field followed by trailing space/tab/null before the line terminator; assert the parsed `Filename` has none of that trailing garbage. Confirm it fails against current code.
- [ ] Step 2: Change `bytes.TrimRight(rest, "\r\n")` to `bytes.TrimRight(rest, " \t\r\n\x00")` at both call sites in `parseKeyValues` (the `name` field branch, and check whether the generic "other fields" branch at line ~439 should match too — sabctools' fix only touched the name/filename cases specifically, verify against the diff before changing the generic branch).
- [ ] Step 3: Confirm GREEN, run full decoder suite including the fuzz test (`internal/decoder/fuzz_test.go`) and existing latin1 test (`decoder_latin1_test.go`) to make sure the wider trim set doesn't regress anything relying on the old narrower behavior.
- [ ] Step 4: Lint, `gremlins unleash --timeout-coefficient 100 --diff origin/main` scoped to `internal/decoder`.
- [ ] Step 5: Commit — `fix(decoder): trim trailing whitespace/null from yEnc name= field`.

---

## Task 4: Tar archive extraction support (Go-native only)

> **Rescoped 2026-07-10:** originally planned as Go-native + external-tool (mirroring the RAR/7z dual-engine pattern). The user decided to implement **native Go extraction only** — no external `tar` binary shell-out. This removes an entire sub-task (external-tool extractor, its sandboxing/plumbing, and the engine-selection option) and the associated risk of relying on system `tar`'s own path-safety behavior varying by platform/version. Go's stdlib `archive/tar` is the only extraction path; there is no fallback engine for tar (unlike RAR/7z, which fall back from Go-native to an external binary on failure).

**Files:**
- Modify: `internal/unpack/detect.go` (new `ArchiveType`, `Classify`) — **done**, see commit history for this task
- New: `internal/unpack/go_tar.go` (native extractor using stdlib `archive/tar`)
- Modify: `internal/postproc/stage_unpack.go` (dispatch tar archives to the new extractor, mirroring how `RarArchive`/`SevenZipArchive` are dispatched — but with no Go-native/external-tool choice to make, since there's only one engine)
- Modify: `internal/config/postproc.go` (new `EnableTar` config field, mirrors SABnzbd's `cfg.enable_tar()`) — **done**, see commit history for this task
- Modify: `gonzbd.yaml`, `test/fixtures/gonzbd.yaml`, `docs/sabnzbd_spec.md` §9.x — config doc sync per `AGENTS.md` — **done**
- Modify: `ui/src/lib/components/config/` (new toggle) and `internal/config/ui_contract_test.go`'s `uiKeywords` — **done**
- Test: `internal/unpack/detect_test.go` (done), `internal/unpack/go_tar_test.go`, `internal/postproc/stage_unpack_test.go`

**Background:**

Upstream, `../sabnzbd` commit `dcfe8b076` ("Support extracting tar files", #3456) added plain `.tar` extraction (not `.tar.gz`/`.tgz`/compressed variants — confirmed via `TAR_RE = re.compile(r"\.(tar$)", re.I)` in `sabnzbd/filesystem.py`). It uses Python 3.12's `tarfile.data_filter` (a stdlib security filter blocking path traversal, absolute paths, symlink/hardlink escapes, and device-file entries) plus manual stripping of setuid/setgid permission bits (`UNWANTED_FILE_PERMISSIONS`), collision-safe renaming (`get_unique_filename`), and `one_folder` flattening. See `sabnzbd/newsunpack.py:tar_extract` (~line 1095) and `tar_unpack` (~line 1030) for the full implementation, and `sabnzbd/filesystem.py:build_filelists`/`TAR_RE` for detection.

**Scope is narrow and matches upstream: plain `.tar` only**, not compressed tar variants. This is a new capability for gonzbd, not a bug fix — treat it as a `feat`.

**Design:**

1. **Detection** (`internal/unpack/detect.go`): add `TarArchive` to the `ArchiveType` enum; extend `Classify` with a case for `strings.HasSuffix(lower, ".tar")`. No multi-volume tar convention exists (unlike RAR/7z split sets) — a tar `Archive.Parts` will always be a single-element slice. **Done.**

2. **Go-native extractor** (`go_tar.go`, follow the exact structural pattern in `go_sevenzip.go`/`go_unrar.go` — study both before writing this): use stdlib `archive/tar`. Go's `archive/tar` has **no built-in equivalent to Python 3.12's `data_filter`** — you must implement the safety checks manually:
   - Reject or clean entries with path-traversal components (`..`) or absolute paths — do not trust `header.Name` as a safe relative path. Consider reusing/extending the containment-violation pattern already established in `internal/postproc/stage_unpack.go:cleanupContainmentViolation` and whatever validation `go_sevenzip.go` already does for entry paths, rather than inventing new logic.
   - Reject or skip symlink/hardlink entries whose target escapes `outDir` (simplest safe default: skip symlink/hardlink/device/FIFO entries entirely — plain tars of Usenet content essentially never need them, and this sidesteps the whole class of escape vectors).
   - Strip setuid/setgid/sticky bits from extracted file modes (`mode &^ (os.ModeSetuid | os.ModeSetgid | os.ModeSticky)` — check `go_sevenzip.go`/`go_unrar.go` for whether they already do this and follow the same helper).
   - **Sparse-file bomb protection**: `archive/tar` supports GNU/PAX sparse-file headers where a `Header.Size` can be much larger than the physical bytes actually present in the stream — this is a plain-tar-compatible decompression-bomb vector even though tar itself isn't compressed. Reuse `opts.MaxSize`/`opts.MaxRatio`/`opts.MinBombThreshold` (already defined in `internal/unpack/unrar.go`'s `Options` struct and enforced somewhere in `go_sevenzip.go`/`go_unrar.go` — find and reuse that exact enforcement helper rather than re-deriving the logic).
   - Honor `opts.OneFolder` (flatten paths) and `opts.OverwriteFiles` (collision handling — check `internal/unpack/unique_path.go` for the existing collision-safe-rename helper used by other extractors, reuse it).

3. **Config**: add `EnableTar bool` to whatever struct holds `EnableRecursive`/similar postproc toggles in `internal/config/postproc.go`, defaulting to matching SABnzbd's default. **Done.** Wire into `stage_unpack.go`'s dispatch so tar sets are skipped entirely when disabled, mirroring how other archive-type toggles gate their stages — **not yet done**, part of the remaining work below.

**Steps:**

- [x] Step 1: Read `go_sevenzip.go` and `go_unrar.go` fully, top to bottom, to identify the exact shared helpers for bomb protection, containment checking, and collision-safe renaming — this task should reuse them, not reimplement.
- [x] Step 2: `detect.go` — write failing tests for `Classify` recognizing `.tar` as `TarArchive`, confirm RED, implement, confirm GREEN.
- [x] Step 3: `go_tar.go` — TDD each safety property independently: (a) normal extraction of a well-formed tar, (b) path-traversal entry rejected, (c) symlink-escape entry rejected, (d) setuid/setgid bits stripped, (e) sparse-bomb entry rejected via existing `MaxSize`/`MaxRatio` enforcement, (f) `OneFolder` flattening, (g) collision handling respects `OverwriteFiles`. Each as its own failing-then-passing test. Also added: OnLine/panic-recovery callback coverage, cumulative multi-entry bomb tracking, ratio/ceiling/negative-size boundary coverage via direct extractTarFile/classifyTarError calls, directory-entry bad-path skipping, zero-permission-bits Chmod skip, and IgnoreUnrarDates timestamp-restore skip — added while closing gremlins-identified test gaps (see Step 6).
- [x] Step 4 (renumbered from original Step 5): Config — add `EnableTar`, sync `gonzbd.yaml`, `test/fixtures/gonzbd.yaml`, `docs/sabnzbd_spec.md` §9.x, Svelte UI toggle, `ui_contract_test.go`. Run `go test ./internal/config/ -run 'TestUI|TestAllFlat'`.
- [x] Step 5 (renumbered from original Step 6): `stage_unpack.go` — wire dispatch, gated on `EnableTar`. No engine-selection option needed (only one engine exists) — simpler than the RAR/7z dispatch it's modeled on. `EnableTar` also threaded through `internal/app/reloader.go` and `internal/app/stages.go`.
- [x] Step 6: Full suite (`go test -race ./...` for touched packages), lint, `gremlins` scoped to `internal/unpack` and `internal/postproc`. All gremlins mutants on tar-related diff lines killed; remaining lived/not-covered mutants in both packages are pre-existing (unrelated to this task) or structural analogs of long-standing accepted residuals already present in `go_sevenzip.go` (dead branch, disk-full/Chmod/Chtimes I/O-failure branches) — confirmed by re-running gremlins against `go_sevenzip.go` itself and comparing.
- [x] Step 7: Commit(s) — `refactor(unpack): parameterize boundReader for cross-extractor reuse` (7291b0a), `feat(unpack): add native Go tar extractor` (c0b7741), `feat(postproc): dispatch tar archives to unpack stage` (88e3291), as separate logical commits per `AGENTS.md`'s "one step per commit."

---

## Task 5: Investigation spike — RAR volume/extension recovery via header parsing

**Deliverable:** a written recommendation (go/no-go, and if go, a follow-up plan section), not necessarily code. Do not implement without a clear "yes, worth it" conclusion from this investigation.

**Files to read (no modification expected in this task):**
- `../sabnzbd/sabnzbd/utils/rarvolinfo.py` (`get_rar_extension`) and its caller in `../sabnzbd/sabnzbd/postproc.py` (`rar_renamer`) — see commit `a2ac2b043` for the current (refactored) version.
- `internal/deobfuscate/deobfuscate.go` (`extractRARUsefulName`, ~line 572) — gonzbd's current RAR-header-based deobfuscation strategy.
- `../rarengine/header.go` — `ArchiveHeader.VolumeNumber` and `MultiVolume` are **already parsed and exported** for RAR5 (confirmed: `ParseArchiveHeader` populates `VolumeNumber` from the archive flags/vint at line ~279). This substantially lowers the likely implementation cost if the investigation concludes it's worth doing.

**Background:**

SABnzbd's `get_rar_extension` solves a narrower problem than gonzbd's existing deobfuscation: when a RAR volume's filename gives **no clue at all** (fully randomized, no recognizable extension or numbering), SABnzbd parses the RAR header directly to recover the *volume number* and reconstruct the canonical extension (`part003.rar`, or legacy `.r00`/`.r01` numbering for RAR3). gonzbd's `extractRARUsefulName` currently recovers a *useful content name* from RAR-internal file entries (the files packed inside the archive), which is a related but different piece of information — it doesn't appear to recover *volume sequencing* for a set whose outer filenames are opaque.

**Investigation questions to answer:**

1. Does gonzbd's current deobfuscation/unpack pipeline ever actually get stuck on a RAR set whose volume order can't be determined from filenames? Trace what happens today when `internal/unpack` is handed a set of files that don't match `partPattern`/`legacyExtraPattern`/`numericSuffixPattern` in `detect.go` — does extraction fail outright, or does something else (unrar's own internal volume-sequencing, since unrar can read the "next volume" field from within the RAR5 archive header itself, independent of filename) already handle this?
2. If gonzbd's own extractors (`GoUnRAR` via `rarengine`, or external `unrar`) already correctly sequence volumes internally regardless of filename (this is plausible — RAR5's format embeds enough structure that filename-based sequencing may be redundant for actual extraction, only mattering for *cosmetic* renaming before extraction), then this feature may only matter for gonzbd's **deobfuscation heuristics** (picking a good display name / rename target), not for extraction correctness. Determine which case applies before scoping any implementation.
3. For RAR3, check whether `rarengine`'s `header_rar3.go` exposes an equivalent volume-number field (the `header.go` grep during prior research found none for RAR3 — only the RAR5 path in `header.go` had `VolumeNumber`). If RAR3 needs new parsing work in `rarengine`, that's a second cross-repo dependency alongside Task 1's — worth flagging as added cost.
4. Estimate real-world frequency: how often do Usenet posts actually obfuscate RAR volume filenames to the point of losing all numbering information, versus just obfuscating the "useful name" (which `extractRARUsefulName` already handles)? This shapes whether the feature is worth the implementation cost at all.

**Steps:**

- [ ] Step 1: Answer investigation questions 1-2 by reading `internal/unpack/detect.go`, `internal/unpack/filejoin.go`, and tracing a fully-obfuscated-RAR-set test case (construct one if none exists) through the existing pipeline to observe actual current behavior.
- [ ] Step 2: Answer question 3 by reading `rarengine/header_rar3.go` in full.
- [ ] Step 3: Answer question 4 via a judgment call informed by `docs/sabnzbd_spec.md` and/or the `../sabnzbd` issue tracker/PR discussion for `a2ac2b043` if it references a real user report motivating the refactor.
- [ ] Step 4: Write the recommendation as a short section appended to this plan document (below this line) or as a new dated investigation note in `docs/superpowers/plans/` — state clearly: implement now / implement later / not worth it, with the reasoning from Steps 1-3. If "implement now," sketch the task in the same Files/Background/Design/Steps format as Tasks 1-4 for a follow-up agent to pick up.

### Task 5 Investigation Findings (completed 2026-07-10)

**Recommendation: implement later.** This is a real, confirmed extraction-correctness gap (not merely cosmetic), but it is a narrow edge case that requires a nontrivial pipeline-ordering change plus new cross-repo `rarengine` work for RAR3, so it should not jump ahead of Tasks 1-4's already-scoped, lower-risk work. Details below.

#### Question 1 & 2: Is this fallback-only, and does gonzbd already sequence volumes from header data independent of filenames?

**SABnzbd side** (`../sabnzbd/sabnzbd/postproc.py` lines 843-852): `rar_renamer(nzo)` is called **only as a last resort**, gated by `if not rars:` — i.e. only when `build_filelists()` (filename-pattern-based RAR detection, the SABnzbd equivalent of gonzbd's `unpack.Classify`) found **zero** files it recognizes as RAR volumes at all. It is not run unconditionally, and it never runs when even one file in the set has a recognizable extension/numbering. `rar_renamer` (lines 981-1110) then physically **renames** the obfuscated files to canonical `part003.rar`/`.r00` names using `rarvolinfo.get_rar_extension` (`../sabnzbd/sabnzbd/utils/rarvolinfo.py` lines 26-65, refactored in `a2ac2b043` to lean on the `rarfile` library's own header parser: RAR5 volume number from the main archive header, `main_volume_number`; RAR3/4 from the end-of-archive block's `endarc_volnr`). This confirms SABnzbd's own extraction step (`unrar` binary) genuinely depends on filename-based next-volume-lookup — the whole point of `rar_renamer` is to give `unrar` filenames it can sequence, because the real `unrar` CLI's own next-volume discovery is itself filename-convention-based, not purely header-based.

**gonzbd side**, traced end to end:
- `internal/unpack/detect.go`'s `Classify`/`groupArchives` (lines 60-77, 118-230) is **purely filename-pattern based** (`partPattern`, `legacyExtraPattern`, `numericSuffixPattern`, `sevenSplitPattern`). A file matching none of these patterns gets `ArchiveType == UnknownArchive` and is silently **excluded from `Scan()`'s results entirely** — no `Archive` struct is even produced for it.
- `internal/postproc/stage_unpack.go` (line 252) drives extraction purely off `unpack.Scan(job.DownloadDir)`'s output. A file set with zero filename clues is invisible to this stage — extraction is never attempted, not even a failed attempt.
- Both engines gonzbd can use — `GoUnRAR`/`GoUnRAREngine` (`internal/unpack/go_unrar.go`) and the external-`unrar` path (`internal/unpack/unrar.go`) — are dispatched from the same `Scan()` output, so neither engine choice avoids this gap.
- Digging one level deeper into whether the engines themselves could recover *even if* handed a lone obfuscated file: `go_unrar.go`'s `volumesForArchive` (lines 283-292) uses `archive.Parts` (the filename-based grouping from `Scan`) when there's more than one part, and only falls back to `discoverRar5Volumes`-style filename discovery when `Parts` has a single entry — there is no header-content-based *sibling discovery* anywhere in this path. `../rarengine/unpack.go`'s own `UnpackDir`/`discoverVolumes` (lines 340-351) confirms the same limitation upstream in the library itself: it only knows how to enumerate `.partNN`/classic-numbered siblings by filename (`discoverPartVolumes`, `discoverClassicVolumes`); for anything else it returns `[]string{firstVol}` — i.e. treats the file as a lone single-volume archive. `rarengine.SortVolumes` (lines 34-81) *does* read header-embedded volume numbers via `readVolumeIndex`, but only to **order** an already-discovered set — it is never used to **discover set membership** across files with unrelated names.

**Conclusion: this is a real correctness gap, not just cosmetic.** A RAR set whose volume filenames carry zero numbering clue (e.g. all-hex names with no `.rar`/`.r00`/`.partNN.rar`/`.NNN` suffix at all) is invisible to gonzbd's entire extraction pipeline today — `Scan()` produces nothing for it, `UnpackStage` never attempts it, and neither the pure-Go `rarengine` path nor the external-`unrar` path has any content-based fallback to discover or sequence such a set. This matches SABnzbd's motivating scenario exactly (its own inline comment at `postproc.py:845`: *"If there's no RAR's, they might be super-obfuscated"*).

**However**, an important structural wrinkle not present in SABnzbd: gonzbd's stage order (`internal/app/stages.go` lines 78-90, 152-254) is `QuickCheck → Repair(par2) → Unpack → SampleCleanup → RecoverPar2Names → Par2Cleanup → Deobfuscate → ExtensionCleanup → Finalize → …` — **`UnpackStage` runs before `DeobfuscateStage`**, and `PostProcessor.run()`/`processJob` (`internal/postproc/postproc.go` line 555) executes the stage list exactly once, linearly, with no re-invocation of earlier stages. Porting SABnzbd's header-based renaming logic verbatim into gonzbd's `Deobfuscate` stage (where the analogous `extractRARUsefulName`, `internal/deobfuscate/deobfuscate.go` line 572, already lives) would rename the files too late to help extraction — `UnpackStage` has already run and skipped them by the time `Deobfuscate` executes. Making this useful for actual extraction (not just a cosmetic rename of files nothing will ever try to extract) requires **either**: (a) moving a volume-numbering-recovery step to run before `UnpackStage`, as its own new stage or as a pre-pass inside `UnpackStage` itself when `Scan()` finds nothing extractable, or (b) adding a retry/re-scan loop after `Deobfuscate` that re-invokes `UnpackStage` if renames occurred. This is a nontrivial addition to the task scope beyond "port the header-parsing function."

#### Question 3: RAR3 volume-number parsing in `rarengine`

Confirmed, correcting/sharpening the prior research pass's finding. `../rarengine/header_rar3.go` (168 lines total) only defines `ReadRAR3BlockHeader` (generic block header), `ParseRAR3FileHeader` (per-file header), and `parseDOSTime` — there is **no** RAR3 equivalent of `ParseArchiveHeader`/`ArchiveHeader.VolumeNumber`, and no end-of-archive (`ENDARC`) block parsing at all. This gap is visible in the caller too: `../rarengine/unpack.go`'s `readVolumeIndex` (lines 99-134) branches on RAR version — for `VersionRAR5` it properly calls `ParseArchiveHeader(h).VolumeNumber`; for the RAR3 branch it only validates `h.Type == 0x73` (main header) and then **unconditionally `return 0, nil`** — i.e. `rarengine`'s own header-based volume-ordering is already a no-op stub for RAR3 today (it silently reports "volume 0" for every RAR3 volume rather than reading the real number from the end-of-archive block, since RAR3's volume number is *not* in the main header the way RAR5's is — it's in the `ENDARC` block, which `rarengine` doesn't parse at all). Implementing RAR3 support for this feature therefore requires **new parsing work in `rarengine`** (a `ParseEndArcHeader`-equivalent reading `endarc_volnr`, mirroring `rarfile`'s `RAR_BLOCK_ENDARC`/`endarc_volnr`), a second cross-repo dependency alongside Task 1's, with its own release/version-bump cycle.

#### Question 4: real-world frequency

Judgment call, since neither the `a2ac2b043` commit message nor its predecessors (`3873b9a11` "Introduction of get_rar_extension", 2019; `613b8216a`; `05427b7b3` "Always run rar_renamer if no rar-files are present") reference a specific issue number or bug report — this is inferred from code comments and general knowledge of obfuscation practices, not a linked user report.

Reasoning: most Usenet obfuscation techniques hide the *content* filename (what gonzbd's existing `extractRARUsefulName` + PAR2-based renaming already handle) while still leaving RAR volumes with *some* recognizable numbering, because indexers, NZB generators, and most posting tools need predictable volume ordering to describe a multi-part post at all — losing volume numbering entirely is a strictly harder, rarer obfuscation choice than just renaming content. That said, `postproc.py`'s own comment ("super-obfuscated... can happen even if par2 is present") and the fact that this feature has been maintained continuously since 2019 (with a significant refactor as recently as this year) both indicate it is not a hypothetical/dead code path upstream — it addresses a real, recurring (if numerically small) category of releases from groups that deliberately strip *all* filename information, including volume sequencing, specifically to defeat automated/heuristic reassembly and DMCA-bot scanning. gonzbd currently has **zero** coverage for this category (extraction fails silently/completely — the job likely ends up stuck failed or in a partial state with unextracted RAR parts sitting in the download dir), whereas SABnzbd's coverage — while itself imperfect (RAR5-only precision, "staircase" heuristic guards for mixed sets, explicit bail-out on unmatched sets) — at least attempts recovery.

#### Follow-up task sketch (for when this is picked up)

**Files:**
- Cross-repo, Step A (RAR3 support, can be deferred to a stretch goal): `../rarengine/header_rar3.go` (new `ParseRAR3EndArcHeader`, exported `EndArcHeader.VolumeNumber`), `../rarengine/unpack.go`'s `readVolumeIndex` (wire it in, replacing the `return 0, nil` stub)
- Cross-repo, Step B: `go.mod` (bump `github.com/hobeone/rarengine` after any `rarengine` release)
- New: `internal/rarheader/rarheader.go` — a `RecoverVolumeExtension(path string) (volumeNumber int, extension string, err error)` wrapper mirroring `get_rar_extension`'s RAR5 logic first (reuse `rarengine.ParseArchiveHeader`'s already-exported `VolumeNumber`/`MultiVolume`), RAR3 as a follow-on once Step A lands
- Modify: `internal/unpack/detect.go` — needs a new pre-pass (likely a new exported `RecoverObfuscatedRARSet`-style function, or extending `Scan`) that, when a directory yields zero `Archive` results from the normal filename-based `groupArchives`, scans remaining regular files for RAR magic bytes (reuse `rarheader.IsRARReader`, already used by `deobfuscate.go`), calls the new volume-recovery wrapper on each, and — mirroring SABnzbd's `rar_renamer`'s per-volume-number bucketing plus "staircase" sanity check and content-matching for mixed sets (`postproc.py` lines 989-1107) — renames them to canonical `part%03d.rar`/`.rNN` names so the existing filename-based `groupArchives` picks them up on a second pass
- Modify: `internal/app/stages.go` and/or `internal/postproc/stage_unpack.go` — resolve the stage-ordering issue found in this investigation: either insert the new recovery pre-pass as its own stage running *before* `UnpackStage`, or have `UnpackStage` invoke it internally as a fallback before giving up when `Scan()` returns nothing extractable. This is a **design decision requiring escalation** per `AGENTS.md` ("departing from the architecture in `docs/ARCHITECTURE.md`" / stage-ordering is an established pipeline contract) — present as a Decision Needed before implementing.
- Test: `internal/rarheader/rarheader_test.go` (volume-number recovery against real fixture archives with names stripped of all numbering — construct fixtures by copying existing `internal/unpack/testdata` multi-volume RARs and renaming to opaque hex names), `internal/unpack/detect_test.go` (fallback pre-pass), an integration-style test proving a fully-obfuscated multi-volume set now extracts successfully end-to-end where it previously didn't

**Background:** see the investigation findings above — this is a real but narrow gap: gonzbd currently cannot extract a RAR set whose volume filenames carry zero numbering information, because both `unpack.Classify`'s filename-pattern grouping and `rarengine`'s own volume discovery depend on recognizable naming. SABnzbd's `rarvolinfo.get_rar_extension` + `rar_renamer` solve this by reading the volume number directly from RAR header data and renaming files to canonical names before extraction is attempted. RAR5 support is cheap (the needed `ArchiveHeader.VolumeNumber` field already exists in `rarengine`); RAR3 support requires new cross-repo parsing work and can be split into a later sub-step.

**Design:** Do RAR5 first (low cost, most modern releases are RAR5). Land it as its own recovery pre-pass that runs when normal `Scan()` finds nothing, renames recovered volumes to canonical extensions, then re-invokes `Scan()`/extraction — resolving the stage-ordering problem is unavoidable and should be decided (not just coded around) before implementation starts, per the Decision Needed flagged above. Mirror SABnzbd's mixed-rarset bucketing/staircase-check/content-matching logic (`postproc.py` lines 1051-1107) only if real-world testing shows single-rarset-per-job (the common case, `numberofrarsets == 1` in SABnzbd's own code, `postproc.py` line 1043) isn't sufficient — start with the simple case and treat mixed-set matching as a stretch goal, consistent with how narrow this scenario already is.

**Steps:** (fill in with TDD Red-Green detail per `AGENTS.md` once picked up — not fully speced here since this remains "implement later," not "implement now")
- [ ] Present the stage-ordering Decision Needed to the user before writing code.
- [ ] Build RAR5-only volume-number recovery in `internal/rarheader`, TDD'd against real fixture archives.
- [ ] Wire a fallback pre-pass into the unpack pipeline per the chosen stage-ordering design, TDD'd with a constructed fully-obfuscated fixture set proving extraction succeeds where it previously silently didn't.
- [ ] Defer RAR3 (`rarengine` cross-repo `ENDARC` parsing) to a follow-on sub-task; document the RAR5-only limitation in `docs/sabnzbd_spec.md` §8.6 if this lands before RAR3 support does.

#### What would flip this to "implement now"

A real user report of a stuck/failed job whose download directory contains an unextracted, fully-obfuscated multi-volume RAR set (i.e. concrete evidence this narrow scenario is actually occurring against gonzbd, not just a theoretical port of an upstream feature) would justify prioritizing this over Tasks 1-4. Absent that, the combination of (a) narrow real-world frequency, (b) a required architecture decision (stage ordering) beyond a simple function port, and (c) a second cross-repo dependency for full RAR3 coverage makes this lower priority than the plan's other four tasks, which are more contained and lower-risk.

---

## Task 6: Comprehensive tar extraction test hardening

**Status:** Task 4's `go_tar.go` extractor and its dispatch wiring are done and committed (`7291b0a`, `c0b7741`, `88e3291`). Its existing test suite (`internal/unpack/go_tar_test.go`, ~975 lines) already covers a real amount of ground: normal extraction, path traversal, one symlink case, setuid/setgid stripping, sparse-bomb rejection (declared-size, ratio, cumulative-across-entries, and boundary-exact variants via direct `extractTarFile` calls), `OneFolder`/`OverwriteFiles` collision handling, `OnLine` callback coverage, panic recovery, `classifyTarError`, and `IgnoreUnrarDates`. **The user reviewed this suite and found a real gap**: `go_tar.go`'s type-dispatch switch (`internal/unpack/go_tar.go:156`) only has explicit `case tar.TypeReg` and `case tar.TypeDir` branches; every other type (symlink, hardlink, char/block device, FIFO) falls through one shared `default:` branch that logs `"skipping non-regular tar entry"` — but only `tar.TypeSymlink` has a dedicated test (`TestGoTar_SymlinkEntrySkipped`, `go_tar_test.go:169`). Hardlinks, device files, and FIFOs hit identical code with zero direct proof. This task closes that gap and several adjacent ones identified during the same review.

**This task is testing-only.** No new production-code behavior is expected — every scenario below should already be handled correctly by the existing `go_tar.go`/`stage_unpack.go` implementation (per the "skip symlink/hardlink/device/FIFO entirely" design already in place). If a test in this task fails against the *current* implementation (not because the test itself is wrong), that's a genuine bug this task caught — fix it as part of this task rather than weakening the test, and note the discovery clearly in that step's commit.

**Files:**
- Modify: `internal/unpack/go_tar_test.go` (all new entry-type/edge-case/cancellation tests; new `FuzzGoTar` at the end)
- Modify: `internal/postproc/stage_unpack_test.go` (mixed-archive-type dispatch, containment-violation interaction)
- Reference only, do not modify unless a real bug is found: `internal/unpack/go_tar.go` (the extractor itself — see `go_tar.go:156` for the type switch, and search that file for `extractTarFile`/`extractTarEntry`/`errTarBomb`/`classifyTarError` for the other functions these tests exercise), `internal/unpack/go_unrar.go:243` (`SanitizeArchivePath` — the path-safety function `go_tar.go` reuses), `internal/postproc/stage_unpack.go:781` (`cleanupContainmentViolation` — the downstream defense-in-depth layer)
- Reference for fuzz harness style: `internal/decoder/fuzz_test.go` (`FuzzDecodeArticle`, `FuzzDecodeUU`) — this repo's only existing fuzz test, model `FuzzGoTar`'s structure after it (seed corpus via `f.Add`, no assertions beyond "doesn't panic" unless a specific invariant is cheap to check)

**Background:** See the conversation that produced this plan section for the full review; the short version is captured in Status above. Read `internal/unpack/go_tar.go` in full before starting — specifically the type-dispatch switch, `extractTarEntry`, `extractTarFile`, and how `SanitizeArchivePath` is called — so new tests target the actual code paths rather than guessed ones.

**Design — six sub-areas, each independently TDD-able and committable:**

### 6a. Entry-type coverage (the gap that prompted this task)

- `TestGoTar_HardlinkEntrySkipped` — a `tar.TypeLink` entry with `Linkname` pointing at another entry already extracted earlier in the same archive (hardlinks reference an already-seen tar member by name, unlike symlinks which can point anywhere). Assert no file is created at the hardlink entry's own path.
- `TestGoTar_DeviceFileEntriesSkipped` — table-driven over `tar.TypeChar` and `tar.TypeBlock`, each with `Devmajor`/`Devminor` set to plausible values (e.g. `1`/`5` for `/dev/zero`-like). Assert neither creates anything on disk (use `os.Lstat` and confirm `os.IsNotExist`).
- `TestGoTar_FifoEntrySkipped` — `tar.TypeFifo`. Same assertion shape.
- Each of these is a **red-green pair against the *test*, not the code**: since the implementation's `default:` branch already handles all non-Reg/non-Dir types uniformly, these tests should pass immediately once written *if* the implementation is correct. Confirm this by first checking they'd fail if you temporarily added a `case tar.TypeLink:`-style carve-out that mishandled the type (a quick manual sanity check, not a permanent test) — or, more simply, verify via `go test -run TestGoTar_Hardlink -v` that the test actually exercises the type-dispatch switch (add a `t.Logf` or step through mentally) rather than accidentally no-op'ing. The point of this sub-area is to convert an *implicit* correctness property (uniform default-branch handling) into an *explicit, individually-named* one that a future refactor can't silently break without a specific test naming the exact type that broke.

### 6b. More realistic attack vectors

- `TestGoTar_AbsolutePathEntryRejected` — an entry named exactly `/etc/passwd` (bare absolute path, no `..` component at all). This is a distinct code path through `SanitizeArchivePath` (`go_unrar.go:243`) from the existing `../../../etc/passwd-pwned`-style relative-traversal test (`TestGoTar_PathTraversalRejected`, `go_tar_test.go:132`) — read `SanitizeArchivePath`'s implementation first to confirm it actually checks `filepath.IsAbs` (or equivalent) as a separate condition from `..`-detection, then write the test to prove that specific branch.
- `TestGoTar_SymlinkNameWithTraversal` — a `tar.TypeSymlink` entry whose **name** (not `Linkname`) contains `..` (e.g. `name: "../../evil-link"`, any `linkname`). Proves the non-regular-type skip and path-sanitization checks compose correctly — i.e. that skipping symlinks happens regardless of whether the path-sanitization check would also have caught it, so a future change that reorders these checks can't accidentally let a bad-path symlink through by short-circuiting on "sanitization passed" before reaching the type check (or vice versa).
- `TestGoTar_GenuineGNUSparseEntry` — construct a real `tar.TypeGNUSparse` entry using Go's `archive/tar` writer's actual sparse-file support (`tar.Header.SparseHoles` — check the stdlib docs/source for the exact API in this Go version, `go doc archive/tar.SparseEntry` and `go doc archive/tar.Header.SparseHoles`), with a large logical size (`Header.Size`) backed by genuinely little physical data (mostly `SparseHole` regions), rather than the existing `TestGoTar_SparseBombRejected`'s approach of writing real content matching the declared size. This exercises the actual GNU sparse decode path in `archive/tar`'s reader (which synthesizes zero bytes for holes) instead of just a same-shaped-but-not-actually-sparse stand-in. If constructing a genuine sparse header proves awkward with the stdlib API, fall back to documenting why in the test's comment and keep the existing declared-size-mismatch test as the primary proof — don't force a contrived construction that doesn't actually exercise sparse decoding.

### 6c. Structural edge cases

- `TestGoTar_EmptyArchive` — a tar file with zero entries (just the two 512-byte zero blocks marking end-of-archive, which `tar.NewWriter(...).Close()` writes automatically for an entry-less writer). Assert `GoTar` returns success with `res.ExtractedFiles` empty and no error.
- `TestGoTar_DirectoryOnlyArchive` — only `tar.TypeDir` entries, no regular files. Assert the directories are created and `ExtractedFiles` reflects whatever this codebase's convention is for directory-only extraction (check whether `go_sevenzip.go`/`go_unrar.go` include directories in `ExtractedFiles` or only regular files, and match that convention rather than guessing).
- `TestGoTar_PAXLongNameEntry` — a filename long enough (>100 bytes total path, the legacy tar header's `Name` field limit) that `archive/tar`'s writer must emit a PAX extended header (`tar.Format` — check whether you need to set `hdr.Format = tar.FormatPAX` explicitly or if the writer auto-upgrades format when needed in this Go version). Confirm the resolved long name still passes through `SanitizeArchivePath` and extracts to the correct nested path, proving sanitization operates on the *resolved* long name, not a truncated legacy-field value.
- `TestGoTar_NonUTF8Filename` — an entry name containing invalid UTF-8 byte sequences. Assert extraction either sanitizes the name into something safe and extractable, or fails cleanly with a classified error (`res.Reason`) — whichever behavior `SanitizeArchivePath`/the surrounding code already implements; this test's job is to pin down and document whichever behavior is correct, not to mandate a specific one sight-unseen. Read the existing sanitization code path first to know which outcome to assert.

### 6d. Cancellation

- `TestGoTar_ContextCancellationMidExtraction` — build a multi-entry (5+) archive, wrap the entries in a custom `io.Reader`/`tar.Reader`-adjacent mechanism that lets the test cancel the `context.Context` after the first entry is processed (e.g. an `OnLine` callback — which is already invoked per-entry per the existing `TestGoTar_OnLineCallback_NormalExtraction` test — that calls `cancel()` after the first invocation, since `go_tar.go` is expected to check `ctx.Err()` between entries per `AGENTS.md`'s goroutine-lifecycle conventions). Assert extraction stops promptly (only the first entry or so extracted, not all 5+) and `res.Err`/`err` wraps or is `context.Canceled`. First read `go_tar.go`'s main extraction loop to find exactly where (if anywhere) it currently checks `ctx.Err()` — if it does NOT currently check context between entries, this is a real gap this task should fix (add the check), not just a missing test, consistent with this task's "testing-only, but fix real bugs found along the way" framing in Background above.

### 6e. Integration-level (cross-package)

- In `internal/postproc/stage_unpack_test.go`: `TestUnpackStage_MixedTarAndRarArchives` — a job download dir containing both a `.tar` and a (non-password-protected, simple) `.rar` side by side. Assert both extract successfully in one `UnpackStage.Run` pass, each via its correct dispatch path (check `res`/`ExtractedFiles` or logged output to distinguish which engine handled which). Model this on the existing `TestUnpackStage_ExtractTarArchive` (`stage_unpack_test.go:262`) and whatever the existing pure-RAR dispatch test looks like — read both before writing this one.
- `TestUnpackStage_TarTraversalDoesNotTripContainmentCleanup` — a tar containing a path-traversal entry (which `go_tar.go` already skips per `TestGoTar_PathTraversalRejected`) processed through the full `UnpackStage.Run` path (not just direct `GoTar` call), proving `cleanupContainmentViolation` (`stage_unpack.go:781`) doesn't misfire or double-delete legitimate sibling output just because a skipped entry's *would-be* path was outside `outDir` — i.e. proving the extractor-level skip and the stage-level cleanup are consistent with each other, not fighting over the same violation from two different layers.

### 6f. Optional stretch: fuzzing

- `FuzzGoTar` in `internal/unpack/go_tar_test.go`, modeled on `internal/decoder/fuzz_test.go`'s `FuzzDecodeArticle`/`FuzzDecodeUU` structure: seed the corpus (`f.Add(...)`) with byte-serialized versions of every crafted-attack tar built in sub-areas 6a/6b above (path traversal, symlink, hardlink, device, FIFO, sparse-bomb, PAX-long-name), then fuzz `GoTar` with mutated variants. Assert only "does not panic" (the panic-recovery test `TestGoTar_PanicRecovery` already proves `cmdutil.SafeEngineRun` catches panics and surfaces them as errors — a fuzz failure here would mean a panic path *outside* that recovery wrapper, which would itself be a real bug). This is genuinely optional — do it last, and only if 6a-6e are done and green with time/budget remaining.

**Steps:**

- [ ] Step 1: Read `internal/unpack/go_tar.go` in full (the type-dispatch switch at line 156, `extractTarEntry`, `extractTarFile`, `SanitizeArchivePath`'s call site) before writing any test, so each new test targets a real, specific code path.
- [ ] Step 2 (sub-area 6a): Hardlink/device/FIFO tests. TDD each — write, run, confirm it passes for the right reason (not vacuously), commit.
- [ ] Step 3 (sub-area 6b): Absolute-path, symlink+traversal-combo, genuine-GNU-sparse tests. If any of these fails against current code, fix the real bug found (don't weaken the test) and note it clearly in the commit message.
- [ ] Step 4 (sub-area 6c): Empty archive, directory-only archive, PAX long name, non-UTF8 filename tests.
- [ ] Step 5 (sub-area 6d): Context-cancellation test. If `go_tar.go` doesn't currently check `ctx.Err()` between entries, add that check as part of this step (real bug fix, TDD'd: test fails red on current code demonstrating the missing check, passes green after adding it).
- [ ] Step 6 (sub-area 6e): `stage_unpack_test.go` mixed-archive-type and containment-cleanup-interaction tests.
- [ ] Step 7 (sub-area 6f, optional/stretch): `FuzzGoTar` harness, seeded from the attack corpus built in Steps 2-3. Run it for a bounded time locally (e.g. `go test -fuzz=FuzzGoTar -fuzztime=60s ./internal/unpack/`) and commit any crash-triggering input it finds as a fixed regression test alongside the fix.
- [ ] Step 8: Full quality gates — `goimports -w`, `go fix ./...`, `go build ./...`, `go vet ./...`, `go test -race ./internal/unpack/... ./internal/postproc/...`, `golangci-lint run` on both packages, `gremlins unleash --timeout-coefficient 100 ./internal/unpack/` and separately `./internal/postproc/` (never unscoped).
- [ ] Step 9: Commit(s) — likely one commit per sub-area (`test(unpack): add hardlink/device/fifo tar entry-type coverage`, `test(unpack): add absolute-path and genuine-sparse tar attack tests`, `test(unpack): add tar structural edge case coverage`, `fix(unpack): honor context cancellation between tar entries` + its test if 6d finds a real gap, `test(postproc): add mixed tar/rar dispatch and containment-cleanup tests`, `test(unpack): add FuzzGoTar harness` if 6f is done) — per `AGENTS.md`'s "one logical change per commit," and per its Commit Hygiene rule, don't claim a bug was found/fixed unless you actually observed the red-before-green failure demonstrating it.
