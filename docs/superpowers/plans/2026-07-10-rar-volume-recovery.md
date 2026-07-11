# RAR Volume Recovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the "fully obfuscated RAR volume set" gap identified in Task 5 of `docs/superpowers/plans/2026-07-10-sabnzbd-rar-tar-improvements.md`, using the narrowest fix that actually matches the confirmed root cause — not the originally-sketched RAR5-header approach, which turned out to be unnecessary for the common case.

**Background (read before starting any task):**

A prior investigation found that `unpack.Classify`/`groupArchives` (`internal/unpack/detect.go`) is purely filename-pattern based: a RAR volume set whose filenames carry zero numbering clue (random hex names, no `.rar`/`.r00`/`.partNN.rar` suffix) produces zero `Archive` results from `unpack.Scan()`, so `UnpackStage` never even attempts extraction.

The original plan sketch proposed porting SABnzbd's `rarvolinfo.get_rar_extension` (RAR5-header `VolumeNumber` parsing) to recover volume ordering. **Before implementing that, we verified empirically (this session) that gonzbd's existing pre-Unpack `RepairStage` already solves this for the common case** — when a PAR2 recovery set exists that describes the RAR volumes by name and content:

- `RepairStage.Run` (`internal/postproc/stage_repair.go`) collects every non-`.par2` file in the download dir and passes them to the repair tool as match candidates (`dataFiles`, ~line 152).
- Both engines already perform content-hash rename-detection and an actual on-disk rename as part of running repair:
  - External `par2cmdline` (`internal/par2/par2.go:RepairWith`): `File: "X" - is a match for "Y"` — renames on disk as part of its own repair run.
  - Native Go engine (`internal/par2/go_par2.go:GoRepair`, calling `github.com/hobeone/par2engine`'s `Decoder.Repair`): **"Phase 0: Rename misnamed files before doing any reconstruction"** (`par2engine@v1.0.6/par2/repair.go:16-24`) — `detectRenameCandidate` does an MD5 content check (`par2engine@v1.0.6/par2/scan.go:747`), then `d.root.Rename(state.RenameSource, fd.Filename)` (`scan.go:837`) performs the real rename. `UseGoPar2` defaults to `true` (`internal/config/defaults.go:113`), so this is gonzbd's default code path.
- Verified end-to-end in this session: took `internal/unpack/testdata/multi_new.part01.rar`..`part10.rar`, generated a real PAR2 set for them, renamed all 10 volumes to opaque hex names with no extension, ran `RepairStage.Run` (native engine) followed by `unpack.Scan()`. Before repair: 0 archives found. After repair: the files were renamed back to `multi_new.part01.rar`..`part10.rar` and `unpack.Scan()` correctly grouped all 10 into one `Archive`.

**Conclusion — this changes the plan's scope:**

1. **No production-code gap exists for the PAR2-protected case.** It needs a regression test, not a fix (Task 1).
2. **A real, narrower gap remains**: NZBs with **no PAR2 protection at all** covering the RAR volumes (either no PAR2 in the release, or a PAR2 set that only describes post-extraction content, not the volumes). For that case, the original RAR5-header idea is still the right tool, but scoped down to exactly this residual case (Tasks 2-3).

**Architecture:** Task 1 is independent test-only work. Tasks 2-3 have an ordering dependency (Task 3 consumes Task 2's exported function) and together add a new pipeline stage. All three can be reviewed/committed independently.

**Tech Stack:** Go 1.26.4, standard `testing` package, `github.com/hobeone/rarengine` v1.0.7 (already a dependency — `ArchiveHeader.VolumeNumber`/`MultiVolume` are already exported, confirmed via direct testing this session: for `multi_new.part01.rar` `VolumeNumber=-1` (no explicit flag on the first volume — RAR5 omits it), `part02.rar` → `1`, `part10.rar` → `9`; i.e. 0-indexed, second volume onward).

## Global Constraints (from `AGENTS.md` — apply to every task below)

- Every `.go` file touched: `goimports -w <file>`, `go fix ./...`, `go build ./...` immediately after editing.
- Quality gates before any commit: `go vet ./...`, `go test -race ./...` (scoped to the touched package during development; full suite before the task's final commit), `golangci-lint run ./...`.
- Conventional Commits 1.0.0: `<type>(<scope>): <description>`, lowercase, imperative, ≤72 chars, ending with:
  ```
  Co-Authored-By: Claude <noreply@anthropic.com>
  ```
- Red-Green discipline: every test must be observed to fail for the right reason before the fix lands.
- `gremlins unleash --timeout-coefficient 100 --diff origin/main`, scoped to the touched package, is a required pre-push gate — never run unscoped (`./...`).
- Any config field added/changed (Task 3 adds one): update `gonzbd.yaml` inline comments, `test/fixtures/gonzbd.yaml`, `docs/sabnzbd_spec.md` §9.x, and — if it has a UI counterpart — `internal/config/ui_contract_test.go`'s `uiKeywords` list and the matching Svelte `keyword=` prop. Run `go test ./internal/config/ -run 'TestUI|TestAllFlat'`.

---

## Task 1: Regression test — obfuscated multi-volume RAR set recovers via PAR2 before Unpack

**Files:**
- Create: `internal/postproc/par2_rar_recovery_test.go`
- Fixture (already committed this session): `test/fixtures/par2/rar_obfuscated_recovery/recovery.par2`, `test/fixtures/par2/rar_obfuscated_recovery/recovery.vol0+1.par2` — a real PAR2 recovery set covering `internal/unpack/testdata/multi_new.part01.rar`..`part05.rar` (5 of the existing 10-volume fixture set), generated via `par2 create -q -s1024 recovery.par2 multi_new.part0[1-5].rar`. Verified this session (both via direct `internal/par2` package test and via the raw `par2` CLI) to correctly rename obfuscated copies of these 5 files back to their canonical names.

**Background:** See the plan's Background section above. This task only adds a test — no production code changes are expected. If the test fails against current code, that's a real regression from what was verified manually this session; investigate before assuming the test itself is wrong.

- [ ] **Step 1: Write the test**

```go
// internal/postproc/par2_rar_recovery_test.go
package postproc

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// TestRepairStage_RecoversObfuscatedRARVolumesViaPar2 proves that a RAR
// volume set with zero filename clues (opaque names, no .rar/.rNN/.partNN.rar
// suffix at all) is invisible to unpack.Scan() before repair, but becomes
// extractable after RepairStage.Run performs its normal PAR2 content-hash
// rename-matching -- using the *default* native Go PAR2 engine (UseGoPar2),
// which relies on github.com/hobeone/par2engine's Decoder.Repair "Phase 0:
// rename misnamed files" step. This is a regression guard for behavior that
// was verified manually (not previously covered by any test): losing it
// would silently reintroduce the "fully obfuscated RAR set never gets
// extracted" bug even though nothing in internal/unpack changed.
func TestRepairStage_RecoversObfuscatedRARVolumesViaPar2(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Copy the 5 canonical RAR volumes the fixture PAR2 set describes,
	// under fully obfuscated names with no recognizable extension.
	obfuscatedNames := map[string]string{
		"multi_new.part01.rar": "a1b2c3d4e5f6.dat",
		"multi_new.part02.rar": "b2c3d4e5f6a1.dat",
		"multi_new.part03.rar": "c3d4e5f6a1b2.dat",
		"multi_new.part04.rar": "d4e5f6a1b2c3.dat",
		"multi_new.part05.rar": "e5f6a1b2c3d4.dat",
	}
	for canonical, obfuscated := range obfuscatedNames {
		data, err := os.ReadFile(filepath.Join("..", "unpack", "testdata", canonical))
		if err != nil {
			t.Fatalf("read fixture %s: %v", canonical, err)
		}
		if err := os.WriteFile(filepath.Join(dir, obfuscated), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", obfuscated, err)
		}
	}

	// Copy the PAR2 recovery set that describes those 5 files by their
	// canonical names and content hashes.
	for _, name := range []string{"recovery.par2", "recovery.vol0+1.par2"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "par2", "rar_obfuscated_recovery", name))
		if err != nil {
			t.Fatalf("read par2 fixture %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	before, err := unpack.Scan(dir)
	if err != nil {
		t.Fatalf("unpack.Scan (before repair): %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("unpack.Scan found %d archive(s) before repair; want 0 (files are fully obfuscated)", len(before))
	}

	stage := NewRepairStage()
	stage.SetUseGoPar2(true) // matches config default
	stage.Log = slog.New(slog.DiscardHandler)

	job := &Job{DownloadDir: dir, Queue: &queue.Job{ID: "test"}}
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("RepairStage.Run: %v", err)
	}

	after, err := unpack.Scan(dir)
	if err != nil {
		t.Fatalf("unpack.Scan (after repair): %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("unpack.Scan found %d archive(s) after repair; want exactly 1", len(after))
	}
	if got := len(after[0].Parts); got != 5 {
		t.Errorf("recovered archive has %d part(s); want 5", got)
	}
	if got := filepath.Base(after[0].MainFile); got != "multi_new.part01.rar" {
		t.Errorf("recovered archive's MainFile = %q; want multi_new.part01.rar", got)
	}
}
```

- [ ] **Step 2: Run it and confirm it passes for the right reason**

Run: `go test -race -run TestRepairStage_RecoversObfuscatedRARVolumesViaPar2 -v ./internal/postproc/...`
Expected: PASS. Confirm the "before repair" assertion (`len(before) != 0`) genuinely fails if you comment out the `stage.Run(...)` call entirely and re-check `unpack.Scan(dir)` on the still-obfuscated files — this proves the test's "before" half isn't vacuous.

- [ ] **Step 3: Full package suite**

Run: `go test -race ./internal/postproc/... ./internal/unpack/... ./internal/par2/...`
Expected: all pass, no regressions.

- [ ] **Step 4: Lint and mutation gate**

Run: `golangci-lint run ./internal/postproc/...`
Run: `gremlins unleash --timeout-coefficient 100 --diff origin/main ./internal/postproc/`
Expected: 0 issues; gremlins reports 0 killed/lived/not-covered (test-only diff, no production-code mutants to generate).

- [ ] **Step 5: Commit**

```bash
git add internal/postproc/par2_rar_recovery_test.go test/fixtures/par2/rar_obfuscated_recovery/
git commit -m "test(postproc): add regression test for PAR2-based obfuscated RAR recovery

Confirms RepairStage's existing content-hash rename-matching (native
par2engine Decoder.Repair's rename-before-reconstruct phase, or external
par2cmdline's file-match-and-rename) recovers a fully obfuscated
multi-volume RAR set before UnpackStage runs. This closes Task 5 of
2026-07-10-sabnzbd-rar-tar-improvements.md for the PAR2-protected case
without any new production code -- verified manually this session, this
test locks the behavior in.

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 2: RAR5 volume-number recovery primitive (no-PAR2 residual case)

**Files:**
- Modify: `internal/rarheader/rarheader.go` (new exported `RecoverVolumeExtension`)
- Test: `internal/rarheader/rarheader_test.go`

**Interfaces:**
- Produces: `func RecoverVolumeExtension(path string) (volumeIndex int, multiVolume bool, err error)` — Task 3 calls this per candidate file. `volumeIndex` is 0-indexed (matches `rarengine.ArchiveHeader.VolumeNumber`'s convention: the first volume has no explicit flag and is normalized to `0` here rather than left at rarengine's internal `-1` sentinel; the second volume is `1`, etc.). `multiVolume` is `false` for a single-volume archive (in which case `volumeIndex` is always `0` and callers should treat the file as a complete, self-contained archive rather than part of a set). Returns `rarheader.ErrNotRAR` if the file isn't RAR5 (RAR3 archives don't carry this header field at all — see `rarengine/header_rar3.go`, confirmed to have no equivalent in a prior investigation pass — so this function only supports RAR5, matching the plan's "RAR5 first" scoping).

**Background:** `rarengine.ParseArchiveHeader` (already an exported function in the `github.com/hobeone/rarengine` v1.0.7 dependency already in `go.mod` — no version bump needed) decodes the main archive header's `MultiVolume` and `VolumeNumber` fields directly from the RAR5 block structure, without any decompression. This was verified directly this session:

```
multi_new.part01.rar: MultiVolume=true VolumeNumber=-1
multi_new.part02.rar: MultiVolume=true VolumeNumber=1
multi_new.part10.rar: MultiVolume=true VolumeNumber=9
```

`internal/rarheader/rarheader.go`'s existing `verifyPasswordFromHeaders` (~line 200) shows the established pattern for this package: open the file, skip the 8-byte RAR5 signature, loop over `rarengine.ReadBlockHeader(r)` results, switch on `h.Type`. The archive header (`rarengine.HeaderTypeArchive`, value `1`) is always the first block after the signature in a well-formed RAR5 file, so this function does not need a full loop — read one block, check its type, parse it.

- [ ] **Step 1: Write the failing test**

```go
// internal/rarheader/rarheader_test.go -- add to existing file

func TestRecoverVolumeExtension_MultiVolumeRAR5(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		file           string
		wantVolumeIdx  int
		wantMultiVol   bool
	}{
		{"first volume, no explicit flag", "../unpack/testdata/multi_new.part01.rar", 0, true},
		{"second volume", "../unpack/testdata/multi_new.part02.rar", 1, true},
		{"tenth volume", "../unpack/testdata/multi_new.part10.rar", 9, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			idx, multiVol, err := RecoverVolumeExtension(tt.file)
			if err != nil {
				t.Fatalf("RecoverVolumeExtension(%s): %v", tt.file, err)
			}
			if idx != tt.wantVolumeIdx {
				t.Errorf("volumeIndex = %d; want %d", idx, tt.wantVolumeIdx)
			}
			if multiVol != tt.wantMultiVol {
				t.Errorf("multiVolume = %v; want %v", multiVol, tt.wantMultiVol)
			}
		})
	}
}

func TestRecoverVolumeExtension_NotRAR(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	p := dir + "/not-a-rar.txt"
	if err := os.WriteFile(p, []byte("plain text, not a RAR archive"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, err := RecoverVolumeExtension(p)
	if !errors.Is(err, ErrNotRAR) {
		t.Errorf("err = %v; want ErrNotRAR", err)
	}
}

func TestRecoverVolumeExtension_RAR3ReturnsError(t *testing.T) {
	t.Parallel()

	// encrypted_header.rar in testdata is RAR5; use a RAR3 fixture if one
	// exists in internal/unpack/testdata, otherwise construct a minimal
	// RAR3-signature-only file (RAR3 support is explicitly out of scope --
	// this test only needs to prove the function doesn't panic or silently
	// return a wrong answer for RAR3 input, not exercise real RAR3 parsing).
	dir := t.TempDir()
	p := dir + "/rar3.rar"
	if err := os.WriteFile(p, rar3Sig, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, err := RecoverVolumeExtension(p)
	if err == nil {
		t.Error("RecoverVolumeExtension on RAR3 signature: expected an error (RAR3 unsupported), got nil")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -run TestRecoverVolumeExtension -v ./internal/rarheader/...`
Expected: FAIL with `undefined: RecoverVolumeExtension` (function doesn't exist yet — valid RED per `AGENTS.md`'s Red-Green Discipline for new functions).

- [ ] **Step 3: Implement**

```go
// internal/rarheader/rarheader.go -- add after VerifyPassword

// RecoverVolumeExtension opens the RAR5 archive at path and reads its main
// archive header to recover volume sequencing information, without
// decompressing any file content. It is used to reconstruct a canonical
// filename (e.g. "name.part003.rar") for a RAR volume whose on-disk name
// carries no numbering clue at all.
//
// volumeIndex is 0-indexed: the first volume in a set has no explicit
// volume-number flag in the RAR5 format and is normalized to 0 here; the
// second volume is 1, the third is 2, and so on. multiVolume is false for a
// single, non-split archive, in which case volumeIndex is always 0.
//
// Only RAR5 is supported -- RAR3 has no equivalent field in its main
// archive header (the volume number lives in the RAR3 end-of-archive block,
// which this package does not parse). Returns ErrNotRAR if path is not a
// valid RAR archive at all, and a non-nil error (not ErrNotRAR) for a
// recognized RAR3 archive, since volume recovery cannot be performed for it.
func RecoverVolumeExtension(path string) (volumeIndex int, multiVolume bool, err error) {
	ver, err := readMagic(path)
	if err != nil {
		return 0, false, err
	}
	if ver != 5 {
		return 0, false, fmt.Errorf("rarheader: RAR%d volume recovery not supported (RAR5 only)", ver)
	}

	//nolint:gosec // path is trusted input from internal caller
	f, err := os.Open(path)
	if err != nil {
		return 0, false, fmt.Errorf("rarheader: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }() // cleanup error in defer

	if _, err := io.CopyN(io.Discard, f, int64(len(rar5Sig))); err != nil {
		return 0, false, fmt.Errorf("rarheader: skip signature: %w", err)
	}

	h, err := rarengine.ReadBlockHeader(f)
	if err != nil {
		return 0, false, fmt.Errorf("rarheader: read block header: %w", err)
	}
	if h.Type != rarengine.HeaderTypeArchive {
		return 0, false, fmt.Errorf("rarheader: expected archive header block, got type %d", h.Type)
	}

	ah, err := rarengine.ParseArchiveHeader(h)
	if err != nil {
		return 0, false, fmt.Errorf("rarheader: parse archive header: %w", err)
	}

	if !ah.MultiVolume {
		return 0, false, nil
	}
	if ah.VolumeNumber < 0 {
		return 0, true, nil // first volume: RAR5 omits the explicit flag
	}
	return ah.VolumeNumber, true, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race -run TestRecoverVolumeExtension -v ./internal/rarheader/...`
Expected: PASS, all subtests green.

- [ ] **Step 5: Full package suite, lint, gremlins**

Run: `go test -race ./internal/rarheader/...`
Run: `golangci-lint run ./internal/rarheader/...`
Run: `gremlins unleash --timeout-coefficient 100 --diff origin/main ./internal/rarheader/`
Expected: all pass; no lived/not-covered mutants on the new function.

- [ ] **Step 6: Commit**

```bash
git add internal/rarheader/rarheader.go internal/rarheader/rarheader_test.go
git commit -m "feat(rarheader): add RAR5 volume-number recovery from header data

Exposes rarengine.ParseArchiveHeader's already-available VolumeNumber/
MultiVolume fields as RecoverVolumeExtension, letting a caller reconstruct
canonical volume ordering for a RAR5 set whose on-disk filenames carry no
numbering clue at all. RAR3 is out of scope (no equivalent header field;
would need new cross-repo rarengine parsing of the RAR3 end-of-archive
block).

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 3: Pre-Unpack recovery stage for the no-PAR2 residual case

**Files:**
- Create: `internal/postproc/stage_rarvolrecovery.go`
- Modify: `internal/app/stages.go` (wire the new stage between `Repair` and `Unpack`)
- Modify: `internal/config/postproc.go` (new `EnableRarVolumeRecovery` field)
- Modify: `gonzbd.yaml`, `test/fixtures/gonzbd.yaml`, `docs/sabnzbd_spec.md` §9.x — config doc sync
- Test: `internal/postproc/stage_rarvolrecovery_test.go`

**Interfaces:**
- Consumes: `rarheader.RecoverVolumeExtension(path string) (volumeIndex int, multiVolume bool, err error)` from Task 2. `rarheader.IsRAR(path string) (bool, error)` (already exported, used by `deobfuscate.go` per the original plan's file list). `markOwned`/`markRenamed` from `internal/postproc/ownedfiles.go` (already exist). `logf` from `internal/postproc/log.go` (already exists).
- Produces: `RarVolumeRecoveryStage` implementing the `Stage` interface (`Name() string`, `Run(ctx context.Context, job *Job) error`), following the exact pattern of `SampleCleanupStage`/`RecoverPar2NamesStage` (embeds `toggle`, has a `Log *slog.Logger` field, a `logger(job) *slog.Logger` helper, a `New*Stage()` constructor).

**Background:** This stage runs immediately before `UnpackStage` in the pipeline. It is a no-op fast path in the overwhelmingly common case: it only does anything when `unpack.Scan(job.DownloadDir)` finds **zero** archives of any type AND there are unclassified files in the directory that are actually RAR data by magic bytes. Since `UnpackStage` itself calls `unpack.Scan` again immediately after this stage runs, no re-scan/re-invoke plumbing is needed here — this stage just renames files on disk if it can recover volume ordering, and the very next stage's own normal `Scan()` call picks up the renamed files naturally.

Design deliberately mirrors SABnzbd's own scoping choice (`postproc.py`'s `numberofrarsets == 1` common-case check, per the earlier investigation): this task handles exactly one obfuscated RAR set per job. If more than one is detected (ambiguous — e.g. two different volume-0 candidates), it logs a warning and does nothing, rather than guessing. Mixed-set bucketing (SABnzbd's "staircase" heuristic) is explicitly out of scope — this is the narrow residual case, not a full port.

- [ ] **Step 1: Write the failing test**

```go
// internal/postproc/stage_rarvolrecovery_test.go
package postproc

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/internal/unpack"
)

func copyRARFixture(t *testing.T, srcName, dstPath string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "unpack", "testdata", srcName))
	if err != nil {
		t.Fatalf("read fixture %s: %v", srcName, err)
	}
	if err := os.WriteFile(dstPath, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", dstPath, err)
	}
}

// TestRarVolumeRecoveryStage_RecoversFullyObfuscatedSet proves the stage
// renames a RAR5 volume set with zero filename clues (no PAR2 set present)
// into canonical part-numbered names, so the immediately-following
// UnpackStage's own unpack.Scan() can find and extract it.
func TestRarVolumeRecoveryStage_RecoversFullyObfuscatedSet(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	obfuscated := map[string]string{
		"multi_new.part01.rar": "aaaaaaaaaaaa.dat",
		"multi_new.part02.rar": "bbbbbbbbbbbb.dat",
		"multi_new.part03.rar": "cccccccccccc.dat",
	}
	for canonical, name := range obfuscated {
		copyRARFixture(t, canonical, filepath.Join(dir, name))
	}

	before, err := unpack.Scan(dir)
	if err != nil {
		t.Fatalf("unpack.Scan (before): %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("unpack.Scan found %d archive(s) before recovery; want 0", len(before))
	}

	stage := NewRarVolumeRecoveryStage()
	stage.SetEnabled(true)
	stage.Log = slog.New(slog.DiscardHandler)

	job := &Job{DownloadDir: dir, Queue: &queue.Job{ID: "test"}, OwnedFiles: map[string]struct{}{}}
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("RarVolumeRecoveryStage.Run: %v", err)
	}

	after, err := unpack.Scan(dir)
	if err != nil {
		t.Fatalf("unpack.Scan (after): %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("unpack.Scan found %d archive(s) after recovery; want exactly 1", len(after))
	}
	if got := len(after[0].Parts); got != 3 {
		t.Errorf("recovered archive has %d part(s); want 3", got)
	}
}

// TestRarVolumeRecoveryStage_NoOpWhenScanFindsArchives proves the stage does
// nothing when normal filename-based detection already works -- the
// overwhelmingly common case, and the fast path this stage must not slow down.
func TestRarVolumeRecoveryStage_NoOpWhenScanFindsArchives(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	copyRARFixture(t, "multi_new.part01.rar", filepath.Join(dir, "multi_new.part01.rar"))
	copyRARFixture(t, "multi_new.part02.rar", filepath.Join(dir, "multi_new.part02.rar"))

	stage := NewRarVolumeRecoveryStage()
	stage.SetEnabled(true)
	stage.Log = slog.New(slog.DiscardHandler)

	job := &Job{DownloadDir: dir, Queue: &queue.Job{ID: "test"}, OwnedFiles: map[string]struct{}{}}
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("RarVolumeRecoveryStage.Run: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(dir, "multi_new.part01.rar")); statErr != nil {
		t.Errorf("original filename was renamed away even though Scan already found archives: %v", statErr)
	}
}

// TestRarVolumeRecoveryStage_DisabledIsNoOp proves SetEnabled(false) skips
// recovery entirely.
func TestRarVolumeRecoveryStage_DisabledIsNoOp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	copyRARFixture(t, "multi_new.part01.rar", filepath.Join(dir, "aaaaaaaaaaaa.dat"))
	copyRARFixture(t, "multi_new.part02.rar", filepath.Join(dir, "bbbbbbbbbbbb.dat"))

	stage := NewRarVolumeRecoveryStage()
	stage.SetEnabled(false)
	stage.Log = slog.New(slog.DiscardHandler)

	job := &Job{DownloadDir: dir, Queue: &queue.Job{ID: "test"}, OwnedFiles: map[string]struct{}{}}
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("RarVolumeRecoveryStage.Run: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(dir, "aaaaaaaaaaaa.dat")); statErr != nil {
		t.Errorf("file was renamed even though stage is disabled: %v", statErr)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -run TestRarVolumeRecoveryStage -v ./internal/postproc/...`
Expected: FAIL with `undefined: NewRarVolumeRecoveryStage` (function doesn't exist yet).

- [ ] **Step 3: Implement the stage**

```go
// internal/postproc/stage_rarvolrecovery.go
package postproc

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/rarheader"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// RarVolumeRecoveryStage renames a fully obfuscated RAR5 volume set (no
// filename numbering clue at all, no PAR2 protection to recover it via
// content hash) into canonical part-numbered names, using the volume
// sequencing embedded in each file's own RAR5 header. Runs immediately
// before UnpackStage; a no-op whenever unpack.Scan already finds at least
// one archive, since that means normal filename-based detection already
// works and this recovery path is unnecessary. Handles exactly one
// obfuscated set per job -- if more than one candidate claims the same
// volume position (ambiguous), it logs a warning and renames nothing.
type RarVolumeRecoveryStage struct {
	toggle
	Log *slog.Logger
}

// NewRarVolumeRecoveryStage constructs a RarVolumeRecoveryStage.
func NewRarVolumeRecoveryStage() *RarVolumeRecoveryStage { return &RarVolumeRecoveryStage{} }

// Name implements Stage.
func (*RarVolumeRecoveryStage) Name() string { return "rar_volume_recovery" }

// Run implements Stage.
func (s *RarVolumeRecoveryStage) Run(ctx context.Context, job *Job) error {
	if !s.enabled() {
		return nil
	}
	log := s.logger(job)

	found, err := unpack.Scan(job.DownloadDir)
	if err != nil {
		logf(ctx, log, job, slog.LevelWarn, "rar_volume_recovery: scan failed: %v", err)
		return nil
	}
	if len(found) > 0 {
		return nil // normal filename-based detection already works
	}

	entries, err := os.ReadDir(job.DownloadDir)
	if err != nil {
		logf(ctx, log, job, slog.LevelWarn, "rar_volume_recovery: read dir failed: %v", err)
		return nil
	}

	type candidate struct {
		path       string
		volumeIdx  int
	}
	byVolume := make(map[int]candidate)
	var ambiguous bool
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(job.DownloadDir, e.Name())
		isRAR, rarErr := rarheader.IsRAR(p)
		if rarErr != nil || !isRAR {
			continue
		}
		volIdx, multiVol, recErr := rarheader.RecoverVolumeExtension(p)
		if recErr != nil {
			logf(ctx, log, job, slog.LevelInfo, "rar_volume_recovery: skipping %s: %v", e.Name(), recErr)
			continue
		}
		if !multiVol {
			volIdx = 0
		}
		if existing, ok := byVolume[volIdx]; ok {
			logf(ctx, log, job, slog.LevelWarn,
				"rar_volume_recovery: ambiguous volume %d claimed by both %s and %s -- skipping recovery",
				volIdx, existing.path, p)
			ambiguous = true
			continue
		}
		byVolume[volIdx] = candidate{path: p, volumeIdx: volIdx}
	}

	if ambiguous || len(byVolume) == 0 {
		return nil
	}

	base := job.Queue.ID
	if job.Queue.Name != "" {
		base = job.Queue.Name
	}
	base = fsutil.SanitizeFilename(base, job.Sanitize)

	for volIdx, c := range byVolume {
		newName := fmt.Sprintf("%s.part%03d.rar", base, volIdx+1)
		newPath := filepath.Join(job.DownloadDir, newName)
		if newPath == c.path {
			continue
		}
		if err := os.Rename(c.path, newPath); err != nil {
			logf(ctx, log, job, slog.LevelWarn, "rar_volume_recovery: rename %s -> %s failed: %v", c.path, newPath, err)
			continue
		}
		markRenamed(job, c.path, newPath)
		logf(ctx, log, job, slog.LevelInfo, "rar_volume_recovery: recovered %s as volume %d -> %s", filepath.Base(c.path), volIdx, newName)
	}

	return nil
}

func (s *RarVolumeRecoveryStage) logger(job *Job) *slog.Logger {
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	return log.With("component", "rar_volume_recovery", "job", job.Queue.ID)
}
```

`job.Queue.Name` (confirmed: `internal/queue/job.go:63`, `Name string` on `queue.Job`) and `fsutil.SanitizeFilename(filename string, opts SanitizeOptions) string` (confirmed: `internal/fsutil/sanitize.go:76`, a free function — not a method on `SanitizeOptions`) are both verified against the current source; the code above already uses the correct names.

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race -run TestRarVolumeRecoveryStage -v ./internal/postproc/...`
Expected: PASS, all three tests green.

- [ ] **Step 5: Wire into the pipeline**

In `internal/app/stages.go`, insert the new stage between `repairStage` and `unpackStage` (read the surrounding ~10 lines first to match the exact variable-naming and config-field-reading conventions already used there for `repairStage`/`unpackStage`):

```go
	// RAR volume recovery: no-op unless normal filename-based RAR detection
	// found nothing at all. Must run after Repair (so PAR2-based rename
	// recovery, if a PAR2 set exists, gets first chance) and before Unpack
	// (so a successful rename here is visible to Unpack's own Scan()).
	rarVolRecoveryStage := postproc.NewRarVolumeRecoveryStage()
	rarVolRecoveryStage.Log = ppLog
	rarVolRecoveryStage.SetEnabled(enableRarVolumeRecovery)
	stages = append(stages, rarVolRecoveryStage)
```

placed after `stages = append(stages, repairStage)` and before `stages = append(stages, unpackStage)`. Add `enableRarVolumeRecovery` to the `c.RLock()`-guarded config read block alongside the other `enable*`/`use*` locals in the same function (follow the exact pattern already used for `enableTar`/`useGoPar2` there).

- [ ] **Step 6: Add the config field**

In `internal/config/postproc.go`, add near `UseGoPar2`:

```go
	// EnableRarVolumeRecovery attempts to recover a fully obfuscated RAR5
	// volume set (no filename numbering clue at all, and no PAR2 protection
	// covering the volumes) by reading volume sequencing directly from each
	// file's RAR5 header, then renaming to canonical part-numbered names.
	// A no-op whenever normal filename-based RAR detection already finds
	// something, so safe to leave enabled.
	// Default true.
	EnableRarVolumeRecovery bool `yaml:"enable_rar_volume_recovery" json:"enable_rar_volume_recovery"`
```

Add the matching default (`true`) in `internal/config/defaults.go` next to `UseGoPar2: true,`.

- [ ] **Step 7: Config doc sync**

- Add a matching commented entry to `gonzbd.yaml` and `test/fixtures/gonzbd.yaml` (find the `use_go_par2` entry in both and add `enable_rar_volume_recovery` immediately after, following the exact comment style already used there).
- Add a row to `docs/sabnzbd_spec.md` §9.x's config table (find the table containing `use_go_par2` and add a row for `enable_rar_volume_recovery`).
- This field has no Svelte UI counterpart in this task (it's an internal safety-net toggle, not user-facing) — do not add a `ui_contract_test.go` entry unless a later task adds a UI control for it. Run `go test ./internal/config/ -run 'TestUI|TestAllFlat'` to confirm this omission doesn't break the existing contract tests.

- [ ] **Step 8: Full quality gates**

Run: `goimports -w internal/postproc/stage_rarvolrecovery.go internal/postproc/stage_rarvolrecovery_test.go internal/app/stages.go internal/config/postproc.go internal/config/defaults.go`
Run: `go fix ./...`
Run: `go build ./...`
Run: `go vet ./...`
Run: `go test -race ./internal/postproc/... ./internal/app/... ./internal/config/... ./internal/rarheader/...`
Run: `golangci-lint run ./internal/postproc/... ./internal/app/... ./internal/config/...`
Run: `gremlins unleash --timeout-coefficient 100 --diff origin/main ./internal/postproc/` (and separately `./internal/app/` if that package has enough diff surface to be worth scoping — check first)

- [ ] **Step 9: Commit**

```bash
git add internal/postproc/stage_rarvolrecovery.go internal/postproc/stage_rarvolrecovery_test.go \
        internal/app/stages.go internal/config/postproc.go internal/config/defaults.go \
        gonzbd.yaml test/fixtures/gonzbd.yaml docs/sabnzbd_spec.md
git commit -m "feat(postproc): recover fully obfuscated RAR volume sets via RAR5 headers

Adds RarVolumeRecoveryStage, running between Repair and Unpack: when
unpack.Scan finds zero archives at all (the volumes' on-disk names carry
no numbering clue), reads each candidate file's RAR5 archive header
(rarheader.RecoverVolumeExtension) to recover volume ordering and renames
to canonical part-numbered names, so the immediately-following Unpack
stage's own Scan() can find and extract the set. No-op whenever normal
filename-based detection already works, or when more than one candidate
claims the same volume position (ambiguous -- logged, not guessed).
Closes the residual case of Task 5 in
2026-07-10-sabnzbd-rar-tar-improvements.md left after Task 1 of this plan
confirmed the PAR2-protected case is already handled by existing repair
behavior.

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Self-Review Notes (from plan authoring)

- **Spec coverage**: Task 1 covers the "PAR2-protected, common case" finding. Tasks 2-3 cover the "no PAR2 protection, residual case" finding. Both branches of the investigation's conclusion are addressed.
- **RAR3 is explicitly out of scope** for Tasks 2-3, same as the original Task 5 sketch — `rarengine`'s RAR3 header parsing has no volume-number field today (confirmed in the original investigation), and adding it is a cross-repo `rarengine` change not justified by this narrower residual case.
- **Mixed-set bucketing/staircase heuristics are explicitly out of scope** (Task 3's Background) — single-set-per-job is the assumption, matching SABnzbd's own common-case scoping.
- `job.Queue.Name` and `fsutil.SanitizeFilename` (Task 3, Step 3) were verified against the current source at plan-authoring time (`internal/queue/job.go:63`, `internal/fsutil/sanitize.go:76`) — no placeholder left for the implementer to resolve.
