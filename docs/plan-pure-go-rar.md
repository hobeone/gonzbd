# Plan: Pure-Go RAR Extraction (rardecode)

## Goal

Replace the `unrar` subprocess dependency with pure-Go extraction using
`github.com/nwaples/rardecode/v2`, which is already imported for header
inspection in `internal/rarheader/`. The project already carries the
dependency; this plan converts it from a read-only tool into the primary
extraction engine.

Two phases of increasing scope:

| Phase | What changes | Risk | Benefit |
|-------|-------------|------|---------|
| 1 | Add `GoUnRAR()` to `internal/unpack/` as the primary RAR extractor | Low | Removes `unrar` binary requirement |
| 2 | Rewrite `DirectUnpacker` to use rardecode + WaitFS (no subprocess) | Medium | Removes fragile interactive-subprocess parsing |

Do Phase 1 first. Stabilize it. Phase 2 follows.

---

## Handling rardecode Bugs

Bugs in rardecode will surface (the library panics on malformed archives,
as discovered in `rarheader.safeList()`). The correct response sequence is:

### Default path: defensive wrapper + upstream PR

1. **Wrap every rardecode call with `defer recover()`** — this is the
   baseline, non-negotiable. Any extraction call can panic on corrupt input.
   The pattern from `rarheader/rarheader.go:safeList()` is the model; extend
   it to the extraction path.

2. **Write a minimal failing test** that reproduces the bug. The test uses
   a crafted or captured archive file stored in `testdata/`. The test lives
   in `internal/unpack/go_unrar_test.go` or `internal/rarheader/`.

3. **Open an upstream issue** at `github.com/nwaples/rardecode` with the
   test case attached. The author is active (v2.2.2 shipped December 2025).

4. **Submit a PR upstream** with the failing test and the fix. Write the
   fix as a clean commit against the upstream version so it cherry-picks
   cleanly.

### Fallback: fork + replace directive

If upstream doesn't respond within ~2 weeks and the bug is blocking:

1. Fork to `github.com/hobeone/rardecode`.
2. Apply the fix as a tagged release: `v2.2.2-gonzbd.1`, `v2.2.2-gonzbd.2`, etc.
3. Add to `go.mod`:
   ```
   replace github.com/nwaples/rardecode/v2 => github.com/hobeone/rardecode/v2 v2.2.2-gonzbd.1
   ```
4. Update `go.sum` and commit both.
5. When upstream merges, remove the `replace` directive and bump to the
   upstream release.

**Do not vendor.** Vendoring obscures what belongs to gonzbd vs. upstream,
makes it harder to see what to PR, and complicates upgrades.

The discipline: every commit in the fork is `[failing test] + [fix]`. PRs
upstream are exactly that pair of commits.

---

## Phase 1: `GoUnRAR()` — Pure-Go Fallback Extractor

### Files to create

- `internal/unpack/go_unrar.go`
- `internal/unpack/go_unrar_test.go`

### Files to modify

- `internal/unpack/unrar.go` — add `PreferGoRAR bool` field to `Options`
- `internal/unpack/passwords.go` — add `GoUnRARWithPasswords` + update `withPasswords`
- `internal/postproc/stage_unpack.go` — dispatch to GoUnRAR when unrar binary absent or `PreferGoRAR` set
- `internal/unpack/classify.go` — add `classifyRarDecodeError` (new function)

### `go_unrar.go` function signature

```go
// GoUnRAR extracts archive.MainFile into outDir using pure-Go rardecode.
// It is equivalent to UnRAR but requires no external binary.
func GoUnRAR(ctx context.Context, log *slog.Logger, archive Archive, outDir string, opts Options) (res Result, err error)
```

### Implementation steps for `GoUnRAR`

**Step 1: Build rardecode options**

```go
rdOpts := []rardecode.Option{
    rardecode.MaxDictionarySize(512 << 20), // cap at 512 MiB — malformed archives can claim 4 GiB
}
if opts.Password != "" {
    rdOpts = append(rdOpts, rardecode.Password(opts.Password))
}
```

The 512 MiB cap is a security decision: RAR3 PPM dictionaries can be set
up to 256 MiB by the archive creator; RAR5 has no spec limit. The default
in rardecode is 4 GiB. Cap conservatively.

**Step 2: Panic-safe open**

Wrap `rardecode.OpenReader` in a `recover()`. Malformed archives panic
inside rardecode's block header parser. Pattern:

```go
func safeOpenReader(name string, opts ...rardecode.Option) (r *rardecode.ReadCloser, err error) {
    defer func() {
        if p := recover(); p != nil {
            err = fmt.Errorf("go_unrar: rardecode panic on %s: %v", name, p)
        }
    }()
    return rardecode.OpenReader(name, opts...)
}
```

**Step 3: Iterate with `Next()` + context checks**

rardecode's `Read()` does not check `context.Context`. Check between files:

```go
for {
    if err := ctx.Err(); err != nil {
        return res, err
    }
    hdr, err := r.Next()
    if errors.Is(err, io.EOF) {
        break
    }
    if err != nil {
        res.Reason = classifyRarDecodeError(err)
        return res, fmt.Errorf("go_unrar: read header: %w", err)
    }
    if err := extractEntry(ctx, outDir, hdr, r, opts); err != nil {
        res.Reason = classifyRarDecodeError(err)
        return res, err
    }
    res.ExtractedFiles = append(res.ExtractedFiles, ...)
}
```

**Step 4: `extractEntry` — write one file**

```go
func extractEntry(ctx context.Context, outDir string, hdr *rardecode.FileHeader, r io.Reader, opts Options) error
```

Rules:
- **Directory entries** (`hdr.IsDir`): `os.MkdirAll`, skip read.
- **Symlinks** (`hdr.Mode()&fs.ModeSymlink != 0`): skip with a warning log.
  Rationale: symlink targets in archives are untrusted and can escape the
  output directory. Document this in a comment.
- **Path sanitization**: call `sanitizeArchivePath(hdr.Name, opts.OneFolder)`.
  Rules: convert `\` to `/`, strip leading `/` and `../` components,
  reject null bytes. If `opts.OneFolder`, use `filepath.Base(hdr.Name)`.
  If sanitized path is empty, return an error.
- **Create parent dirs**: `os.MkdirAll(filepath.Dir(destPath), 0750)`.
- **Write**: `os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)`.
  Then `io.Copy(out, r)`. Close with `defer`.
- **Permissions**: after close, call `os.Chmod(destPath, hdr.Mode()&0666)`.
  Never set executable bits from untrusted archives (security decision —
  document it). Skip chmod if `hdr.HostOS` is unknown.
- **Modification time**: if `!opts.IgnoreUnrarDates && !hdr.ModificationTime.IsZero()`:
  call `os.Chtimes(destPath, time.Now(), hdr.ModificationTime)`.
- **Disk full**: check `io.Copy` error for `syscall.ENOSPC`. Return with
  `FailReason = FailDiskFull` so the password-iteration loop doesn't retry.

**Step 5: `sanitizeArchivePath`**

```go
func sanitizeArchivePath(name string, oneFolder bool) (string, error) {
    // Normalize separators.
    name = strings.ReplaceAll(name, "\\", "/")
    // Reject null bytes.
    if strings.ContainsRune(name, 0) {
        return "", fmt.Errorf("archive path contains null byte: %q", name)
    }
    if oneFolder {
        name = path.Base(name)
    } else {
        // Clean and strip leading traversal components.
        name = path.Clean(name)
        name = strings.TrimPrefix(name, "/")
        // Reject any remaining ../ after clean.
        if strings.HasPrefix(name, "..") {
            return "", fmt.Errorf("archive path escapes output directory: %q", name)
        }
    }
    if name == "" || name == "." {
        return "", fmt.Errorf("archive path is empty after sanitization")
    }
    return name, nil
}
```

**Step 6: `classifyRarDecodeError`**

Maps rardecode errors to `FailReason`:

```go
func classifyRarDecodeError(err error) FailReason {
    switch {
    case errors.Is(err, rardecode.ErrBadPassword):
        return FailWrongPassword
    case errors.Is(err, rardecode.ErrBadFileChecksum),
         errors.Is(err, rardecode.ErrCorruptBlockHeader),
         errors.Is(err, rardecode.ErrCorruptFileHeader),
         errors.Is(err, rardecode.ErrHuffDecodeFailed),
         errors.Is(err, rardecode.ErrCorruptPPM),
         errors.Is(err, rardecode.ErrShortFile),
         errors.Is(err, rardecode.ErrDecoderOutOfData):
        return FailCorrupt
    case errors.Is(err, rardecode.ErrNoSig):
        return FailNotArchive
    default:
        // Disk full: check syscall.ENOSPC
        var errno syscall.Errno
        if errors.As(err, &errno) && errno == syscall.ENOSPC {
            return FailDiskFull
        }
        // Missing volume: rardecode returns fs.ErrNotExist when next volume absent
        if errors.Is(err, fs.ErrNotExist) {
            return FailMissingVolume
        }
        return FailUnknown
    }
}
```

**Step 7: Password iteration**

Update `withPasswords` in `passwords.go` to check `res.Reason` in addition
to the output-string approach:

```go
if isWrongPW(res.ExitCode, res.Output) || res.Reason == FailWrongPassword {
    // try next password
}
```

This allows `GoUnRAR` to participate in password iteration without faking
an output string. Add `GoUnRARWithPasswords`:

```go
func GoUnRARWithPasswords(ctx context.Context, log *slog.Logger, archive Archive, outDir string, opts Options) (Result, error) {
    return withPasswords(ctx, log, archive, outDir, opts, GoUnRAR, isGoUnrarWrongPassword, "go_unrar")
}

func isGoUnrarWrongPassword(_ int, _ string) bool {
    return false // withPasswords checks res.Reason directly for GoUnRAR
}
```

**Step 8: Dispatch in `stage_unpack.go`**

Current dispatch:
```
if use7z → SevenZipWithPasswords
else     → UnRARWithPasswords
```

New dispatch:
```
if use7z              → SevenZipWithPasswords
else if opts.PreferGoRAR → GoUnRARWithPasswords  (explicit config)
else if unrar found   → UnRARWithPasswords
else                  → GoUnRARWithPasswords  (unrar not in PATH)
```

The `Prefer7zip` check already exists. Add `PreferGoRAR bool` to `unpack.Options`
and to the app config layer. Wire it from `config.EnableGoRAR` (new field).

### Phase 1 tests

`internal/unpack/go_unrar_test.go` must cover:

- Single-volume RAR3, no password
- Single-volume RAR5, no password
- Multi-volume RAR (new naming: `part01.rar`, `part02.rar`)
- Multi-volume RAR (legacy naming: `.rar`, `.r00`, `.r01`)
- Password-protected RAR3 — correct password → success
- Password-protected RAR3 — wrong password → `FailWrongPassword`
- Encrypted-header archive — password required to list
- Corrupt archive → `FailCorrupt` (not a panic)
- Directory traversal in archive path (`../../evil`) → sanitized, no escape
- Symlink entry → skipped, no error
- Archive with null byte in path → error, not panic
- `opts.OneFolder` → all files extracted flat
- `opts.IgnoreUnrarDates` → modification time not restored
- Disk full simulation (if feasible via temp filesystem or mock)
- Context cancellation mid-extraction → returns `ctx.Err()`

Test archives for the above go in `internal/unpack/testdata/`. Generate
them once using the system `unrar`/`rar` tool and commit the binaries.
Document the generation commands in a `testdata/README.md`.

### Phase 1 quality gates

After each file touched:
```bash
goimports -w <file>
go fix ./...
go build ./...
go test ./internal/unpack/... -race
go test ./internal/postproc/... -race
go vet ./...
golangci-lint run ./internal/unpack/... ./internal/postproc/...
```

---

## Phase 2: WaitFS — DirectUnpacker Without Subprocess

### Context

`internal/directunpack/directunpack.go` currently manages an interactive
`unrar` subprocess, coordinating volume hand-off via `[C]ontinue, [Q]uit`
prompts on stdin/stdout. Phase 2 replaces this with a pure-Go approach:
implement `fs.FS` with blocking `Open()`, pass it to rardecode, and let
the library naturally pause at volume boundaries.

### The WaitFS design

rardecode's internal `osFS` calls `os.Open(name)` directly, accepting
absolute paths. When `FileSystem(waitFS)` is passed and the archive path is
absolute (e.g. `/downloads/job/movie.part01.rar`), rardecode constructs
paths like `/downloads/job/movie.part02.rar` and calls
`waitFS.Open("/downloads/job/movie.part02.rar")`. WaitFS strips to
basename, checks the completed-volumes map, and blocks if not yet ready.

```go
// WaitFS is an fs.FS whose Open() blocks until the named volume file
// appears in the completed-volumes map, or ctx is cancelled.
// It is intentionally not a compliant io/fs.FS — like rardecode's own
// osFS, it accepts absolute paths. It is only valid as a rardecode
// FileSystem option.
type WaitFS struct {
    mu    sync.Mutex
    avail map[string]string // basename → absolute path
    ready chan struct{}      // cap=1, non-blocking send
    ctx   context.Context
}

func newWaitFS(ctx context.Context) *WaitFS {
    return &WaitFS{
        avail: make(map[string]string),
        ready: make(chan struct{}, 1),
        ctx:   ctx,
    }
}

// AddVolume records that a volume is now ready. basename is e.g. "movie.part02.rar".
func (w *WaitFS) AddVolume(basename, fullPath string) {
    w.mu.Lock()
    w.avail[basename] = fullPath
    w.mu.Unlock()
    select {
    case w.ready <- struct{}{}:
    default:
    }
}

// Open blocks until the named file is in avail or ctx is cancelled.
// name may be an absolute path; only the base component is matched.
func (w *WaitFS) Open(name string) (fs.File, error) {
    base := filepath.Base(name)
    for {
        w.mu.Lock()
        path, ok := w.avail[base]
        w.mu.Unlock()
        if ok {
            return os.Open(path) //nolint:gosec // paths from trusted internal callers
        }
        select {
        case <-w.ready:
        case <-w.ctx.Done():
            return nil, w.ctx.Err()
        }
    }
}
```

### New `DirectUnpacker.run()` structure

The existing `run()` manages the subprocess lifecycle. Replace it with:

```go
func (d *DirectUnpacker) run(ctx context.Context) {
    defer close(d.done)

    for {
        d.mu.Lock()
        if d.killed { d.mu.Unlock(); return }
        setname := d.curSetname
        d.mu.Unlock()

        if err := d.extractSet(ctx, setname); err != nil {
            // log error; mark killed if not already from abort
        }

        d.mu.Lock()
        if d.killed || len(d.nextSets) == 0 { d.mu.Unlock(); return }
        d.nextSets, d.curSetname = d.nextSets[1:], d.nextSets[0]
        d.mu.Unlock()
    }
}
```

### `extractSet` design

```go
func (d *DirectUnpacker) extractSet(ctx context.Context, setname string) error {
    // Wait for vol 1 before starting rardecode.
    if err := d.waitForVolume(ctx, setname, 1); err != nil {
        return err
    }

    d.mu.Lock()
    vol1 := d.completedVols[setname][1]
    d.mu.Unlock()

    // Build WaitFS: pre-populate with all already-completed volumes for this set.
    wfs := newWaitFS(ctx)
    d.mu.Lock()
    for _, path := range d.completedVols[setname] {
        wfs.AddVolume(filepath.Base(path), path)
    }
    d.mu.Unlock()

    // Hook: when new volumes land (via Add()), forward to WaitFS.
    // See "Coordination" below.

    rdOpts := []rardecode.Option{
        rardecode.FileSystem(wfs),
        rardecode.MaxDictionarySize(512 << 20),
    }
    if d.opts.Password != "" {
        rdOpts = append(rdOpts, rardecode.Password(d.opts.Password))
    }

    r, err := safeOpenReader(vol1, rdOpts...)
    if err != nil {
        return fmt.Errorf("directunpack: open set %q: %w", setname, err)
    }
    defer r.Close()

    var extracted []string
    var encounteredError error

    for {
        if ctx.Err() != nil { return ctx.Err() }

        hdr, err := r.Next()
        if errors.Is(err, io.EOF) { break }
        if err != nil {
            encounteredError = err
            break
        }

        destPath, err := sanitizeArchivePath(hdr.Name, d.opts.OneFolder)
        if err != nil { continue /* log and skip */ }
        fullDest := filepath.Join(d.extractDir, destPath)

        if err := extractEntry(ctx, d.extractDir, hdr, r, d.opts); err != nil {
            encounteredError = err
            break
        }
        extracted = append(extracted, fullDest)

        if d.opts.OnLine != nil {
            d.opts.OnLine("Extracting  " + hdr.Name)
        }
    }

    if encounteredError != nil {
        d.mu.Lock()
        d.killed = true
        d.mu.Unlock()
        return encounteredError
    }

    d.mu.Lock()
    var rarParts []string
    for _, p := range d.completedVols[setname] {
        rarParts = append(rarParts, p)
    }
    d.successSets[setname] = SuccessSet{RarParts: rarParts, ExtractedFiles: extracted}
    d.mu.Unlock()
    return nil
}
```

### Coordination: forwarding new volumes to WaitFS

When `DirectUnpacker.Add()` is called with a new volume, it must now also
call `wfs.AddVolume()` so rardecode's blocked `Open()` can proceed.

The cleanest approach: store a `*WaitFS` on `DirectUnpacker` per active
set, replace it atomically when starting a new set, and have `Add()` call
`wfs.AddVolume()` if `d.activeWFS != nil`:

```go
// In DirectUnpacker struct:
activeWFS   *WaitFS        // non-nil while extractSet is running
activeSet   string         // setname for the active WaitFS

// In Add(), after recording to completedVols:
d.mu.Lock()
if d.activeWFS != nil && setname == d.activeSet {
    wfs := d.activeWFS
    d.mu.Unlock()
    wfs.AddVolume(filename, path)
} else {
    d.mu.Unlock()
}
```

Set `d.activeWFS` and `d.activeSet` at the start of `extractSet`, clear
them at the end.

### Remove from `DirectUnpacker`

Delete entirely:
- `createUnrarInstance()` — subprocess management
- `readUnrarOutput()` — character-by-character output parsing
- `waitForNextVolume()` — replaced by WaitFS blocking `Open()`
- `errorPatterns` — subprocess output error detection
- `maxDuplicatePrompts` — subprocess prompt deduplication
- Fields: `cmd`, `stdin`, `stdout`, `curVolume`

Keep:
- `waitForVolume()` — still used to wait for vol 1 before starting rardecode
- `volumeReady` channel — now signals WaitFS via `AddVolume()`
- `opts.OnLine` — now called per extracted file header
- `buildVolumeMap()`, `totalVolumes`, `completedVols` — still track volume state

### Options changes in Phase 2

Remove from `Options`:
- `UnrarCommand` — no subprocess
- `ExtraArgs` — no subprocess
- `HasProblem` — no subprocess version check

Keep:
- `Password`
- `OneFolder`
- `OverwriteFiles`
- `IgnoreUnrarDates`
- `CmdCfg` — remove (no subprocess), unless used elsewhere
- `OnLine`

### Phase 2 tests

`internal/directunpack/directunpack_test.go` must cover:

- Single set, all volumes present before Add() — extraction starts immediately
- Single set, volumes arrive one at a time with delays — extraction waits correctly
- Two sets in sequence — second set starts after first completes
- Volume 1 arrives last — first set doesn't start until vol 1 present
- Abort during extraction — `WaitFS.Open()` returns ctx error, goroutine exits
- Corrupt archive — `d.killed` set, `Results()` returns empty map
- Wrong password — same as corrupt handling (no retry in DirectUnpack)
- Context cancellation — same as Abort

### Phase 2 quality gates

Same as Phase 1, plus:
```bash
go test ./internal/directunpack/... -race -count=3  # run 3x for race detector coverage
```

---

## Known rardecode Limitations and Mitigations

| Issue | Observed | Mitigation |
|-------|----------|-----------|
| Panics on malformed archives | Yes (`safeList` in rarheader) | `safeOpenReader()` + `defer recover()` on `r.Next()` calls |
| `ErrSolidOpen` on random-access of solid archives | Expected | `Next()` loop never calls random-access `Open()` — not affected |
| No SIMD decompression | By design | Performance acceptable for I/O-bound Usenet workloads |
| `MaxDictionarySize = 4GiB` default | Dangerous | Always pass `MaxDictionarySize(512 << 20)` |
| No context propagation inside `Read()` | By design | Check `ctx.Err()` between files in the `Next()` loop |
| SFX (self-extracting) archives | Not handled | Out of scope; `rarheader.IsRAR()` returns false for SFX |
| `r.Next()` may also panic | Likely same root cause | Wrap the entire extraction loop in a top-level `recover()` in `GoUnRAR` and `extractSet` |

---

## Commit Sequence

### Phase 1

```
Step P1.1: add internal/unpack/go_unrar.go — pure-Go RAR extractor
Step P1.2: add internal/unpack/go_unrar_test.go — full test coverage
Step P1.3: unpack: add PreferGoRAR option; dispatch GoUnRAR from stage_unpack
Step P1.4: unpack: add GoUnRARWithPasswords; update withPasswords to check res.Reason
```

### Phase 2

```
Step P2.1: directunpack: add WaitFS
Step P2.2: directunpack: replace subprocess with rardecode + WaitFS
Step P2.3: directunpack: remove subprocess fields and methods
Step P2.4: directunpack: update tests for new extraction path
```

Each commit must pass all quality gates before proceeding.

---

## Decision Points (Escalate Before Implementing)

1. **`PreferGoRAR` config field name and default**: Should it default to
   `true` (pure Go first, subprocess fallback) or `false` (preserve existing
   behavior)? Recommendation: default `false` initially, flip after Phase 2
   is stable.

2. **Solid archive handling in Phase 2**: rardecode's sequential `Next()`
   loop handles solid archives correctly. Verify against a real solid RAR
   before shipping.

3. **OverwriteFiles policy**: The existing `DirectUnpacker` always passes
   `-o+` (always overwrite). The new extractEntry should replicate this —
   confirm the overwrite behavior is intentional and document it.

4. **Executable bit policy**: This plan strips exec bits from extracted
   files (chmod to mode & 0666). SABnzbd preserves them. Confirm which
   behavior gonzbd should have.
