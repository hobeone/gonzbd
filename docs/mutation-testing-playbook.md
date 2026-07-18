# Mutation Testing Playbook (gremlins)

This playbook documents a repeatable process for using
[go-gremlins](https://github.com/go-gremlins/gremlins) to find real test gaps
in a package and close them with targeted, mutation-proven tests. It was
developed while hardening `internal/par2` (mutation score 70.2% → 80.8%,
commit `b8669fb`) and is meant to be followed by any agent asked to "run
mutation testing on X and fix what it finds."

> **Why mutation testing, not just coverage:** coverage tells you a line
> *executed* during tests. Mutation testing tells you whether a test would
> *notice* if that line's logic were wrong. A `LIVED` mutant means the
> mutated code ran and every test still passed — the assertion that would
> catch that bug doesn't exist. `NOT COVERED` is a stronger signal: the line
> never executed at all during any test.

## 1. Run gremlins

**Always use the wrapper script — never call `gremlins unleash` directly.**

```bash
./scripts/run_gremlins.sh ./internal/<package>
```

`scripts/run_gremlins.sh` exists because a bare `gremlins unleash` copies the
whole working directory into each worker's isolated build dir without
respecting `.gitignore`. If that scratch space lives anywhere inside the repo
tree, each worker's copy recursively sweeps up scratch data from prior
workers/runs, nesting many levels deep — this produced 168–394GB of disk
usage from a *single* run, independent of worker count, and coincided with
kernel OOM kills. The script fixes this by relocating scratch space outside
the repo (default `~/.cache/gonzbd-gremlins`, wiped before each run) and adds
real resource limits: a hard memory cap enforced via a `systemd --user`
cgroup scope (so an OOM kills only this run's own processes, not arbitrary
processes across the machine) and a background watchdog that stops the run
if the scratch dir's disk usage or the wall-clock time exceeds a cap.
Requires `systemd-run` (`systemctl --user status` to check); it refuses to
run without it rather than silently falling back to an unconfined invocation.

Tunables, all optional (see the script's header comment for the full list):
`GREMLINS_WORKERS` (auto-detected based on CPU/RAM, clamped 4–16; e.g. 12 workers
on 24-core/94GB RAM systems), `GREMLINS_MEMORY_MAX` (auto-detected to 80% of system
RAM, fallback `32G`), `GREMLINS_DISK_MAX_MB` (default `51200` = 50GiB), `GREMLINS_TIMEOUT_SECS`
(default `1800` = 30min), `GREMLINS_DIR` (scratch dir base — must not be
inside the repo, the script errors out if it is).

Mutant-type selection and `timeout-coefficient` are configured project-wide
in `.gremlins.yaml` at the repo root, so they don't need to be passed as
flags (superseding the older `--timeout-coefficient=40` guidance from this
codebase's project memory — the current `.gremlins.yaml` sets `100`).

**Gotchas:**
- Pass a single package path, not a `...` wildcard suffix appended to an
  already-specific path — `./internal/par2/...` becomes
  `./internal/par2/.../...` and matches nothing. Use `./internal/par2`.
- Extra `gremlins unleash` flags can be passed through as additional
  arguments: `./scripts/run_gremlins.sh ./internal/par2 --dry-run`.
- Run it in the background (`run_in_background: true` if using the Agent
  tools) — even a focused package run produces a long mutant log and you only
  need the tail summary plus the `LIVED`/`NOT COVERED` lines.
- **NEVER run `gremlins` on the entire repository** (e.g. `./...` or `./internal/...`). Doing so will trigger parallel builds and mutant execution across dozens of packages, which rapidly consumes disk space and will completely fill up `/tmp` (potentially causing system hangs or build failures) even with the wrapper script's protections. Always scope it to a single focused package.
- If a run is killed by the memory cap or watchdog, the script prints
  `error: gremlins run was OOM-killed or stopped by the watchdog ...` or
  `error: gremlins run was stopped by the disk/timeout watchdog ...` — retry
  with a lower `GREMLINS_WORKERS` (for OOM) or a higher
  `GREMLINS_DISK_MAX_MB`/`GREMLINS_TIMEOUT_SECS` (for a genuinely large
  package that needs more room, not a runaway).

### Large packages (e.g. `internal/app`, 49 files) need `nohup` + polling, not a bare foreground run

A run scoped to a large package (as opposed to `./...`, which is forbidden
above) can still take longer than a single tool call's timeout. This has now
happened twice on `internal/app`. Use:

```bash
nohup ./scripts/run_gremlins.sh ./internal/app \
  > /tmp/gremlins-app.log 2>&1 &
echo "PID: $!"
```

then poll (`while kill -0 <PID> 2>/dev/null; do sleep 10; done`) or check back
later, rather than waiting on it in the foreground. Even the backgrounded run
can be interrupted by an *outer* timeout (e.g. an agent harness's own
long-running-command limit) before the whole package finishes — that's fine.
`gremlins` writes results incrementally, so a log truncated mid-run (look for
`Shutting down gracefully...` at the tail instead of the final `Killed: N,
Lived: N, ...` summary line) still contains real `KILLED`/`LIVED`/`NOT
COVERED` verdicts for every mutant it reached before the cutoff. Grep the
partial log for the specific line numbers your diff touched
(`grep -n "app.go:<line>:" gremlins-app.log`) — if those lines were reached,
the partial run is sufficient proof for the mutation gate on your change; you
don't need a from-scratch complete run just to get the summary footer.

### Known limitation: `--diff` is broken when scoped to a package

gremlins v0.6.0 has a confirmed upstream bug
([go-gremlins/gremlins#278](https://github.com/go-gremlins/gremlins/issues/278)):
`--diff <ref>` combined with any package/subdirectory argument — including
`cd`-ing into the package directory and omitting the path entirely — reports
every mutant in the changed file as `SKIPPED` (0 killed / 0 lived / 0
not-covered), regardless of what the diff actually contains. This was verified
directly against this repo: a real, unambiguous single-line diff produced
`Skipped: 25` with `--diff`, but `Killed: 16, Lived: 0, Not covered: 8` on the
same package without it.

Root cause per the upstream report: gremlins compares the diff's file paths
(repo-root-relative, from `git diff --merge-base`) against the AST walk's
paths (relative to the package `CallingDir`), and they only match when
`CallingDir` is `.` — which for this repo only happens at the module root,
where whole-module runs are the exact scenario the `NEVER run gremlins on the
entire repository` rule above forbids. There is no scoped invocation that
gets a working `--diff` here.

**Until upstream fixes this, do not use `--diff` for the mutation gate.**
Instead:
1. Run gremlins scoped to the changed package with no `--diff` flag, exactly
   as in step 1 above, to get real `Killed`/`Lived`/`Not covered` numbers.
2. Get the touched line ranges for your change: `git diff origin/main -- internal/<package>/<file>.go`.
3. Cross-reference: for each `LIVED`/`NOT COVERED` mutant, check whether its
   line number falls inside a range your diff touched. Only mutants on
   *your* changed lines are the mutation-testing proof obligation for this
   change — pre-existing `LIVED`/`NOT COVERED` lines outside the diff are a
   separate, already-tracked gap (see "2. Triage" below for optionally
   closing those too, if you have time).

This is slower and more manual than `--diff` would be, but it's the only
reliable signal until the upstream bug is fixed.

The summary line looks like:
```
Killed: 139, Lived: 59, Not covered: 46
Timed out: 0, Not viable: 0, Skipped: 0
Test efficacy: 70.20%
Mutator coverage: 81.15%
```

## 2. Triage: separate signal from noise

Grep the output for `LIVED` and `NOT COVERED` and read each one in source
context (`awk 'NR==<line>{print}' <file>`). Sort them into:

### a. Equivalent mutants — ignore these
A mutation that cannot change observable behavior. The classic example:
`make([]string, 0, 6+len(extraFiles))` — `ARITHMETIC_BASE` mutating the
**capacity** hint. Capacity doesn't affect correctness (slices still grow via
`append`), so no test could ever kill it. Don't write tests for these; note
them and move on.

### b. Real gaps — group by theme, not by line
Cluster the remaining `LIVED`/`NOT COVERED` lines by what *behavior* they
represent, not by file. In the `par2` run, four themes emerged:

1. **A whole optimization path with zero coverage** — the §4.5 early-exit
   scan condition (`expectedFiles > 0 && fileSize > threshold && seen >= ...`)
   had every comparison operator flippable without a test noticing. This is
   the highest-value class of finding: a documented, spec-cited, performance-
   relevant code path that no test exercises end-to-end.
2. **Boundary conditions on length checks** — `len(body) < 8`, `< 56`,
   `< 16`, `packetLen < 64`. These are the classic off-by-one blind spots:
   tests exercise "valid" and "very invalid" inputs but never the exact
   threshold values.
3. **Entire fallback branches never executed** — `quickcheck.go` Phase 3
   (Hash16k matching) and Phase 4 (CRC32+size matching) had dense
   `NOT COVERED` clusters. Existing tests called the *helper functions*
   (`relocateFile`, `ComputeHash16k`) directly with hand-built `FileDesc`
   values, but never drove the orchestrating function (`QuickCheck`) through
   a scenario that actually reaches those phases' internal index-build-and-
   match loops.
4. **Side-effect/logging wiring** — callback (`onWarn`/`onInfo`) invocation
   and `slog` formatting helpers, where no test asserts on the callback
   content or formatted output.

**Prioritize #1 and #3** (whole-path / whole-phase gaps on hot or
spec-referenced code) over #2 and #4 — they represent bigger blind spots and
their fixes generalize better.

## 3. Write the test — match the existing fixture style

Before writing anything, read the package's existing test helpers
(`buildPacket`, `buildFileDescBody`, etc. in `parser_test.go`). Mutation-gap
tests should look like the tests around them, not introduce a new style.

**For boundary conditions:** write the exact "one below" / "exactly at"
pair. E.g. `parseFileDescBody(make([]byte, 55))` → nil, `(56)` → non-nil with
empty filename, `(57+)` → non-nil with filename. Two or three cases bracketing
the literal in the `if` condition kill the `CONDITIONALS_BOUNDARY` and
`CONDITIONALS_NEGATION` mutants on that line.

**For whole-path gaps that depend on real thresholds (e.g. file size):**
build the actual fixture at the real scale rather than refactoring the
threshold into an injectable parameter. An 11 MiB temp file write takes
~20ms and proves the *actual* code path, not a parameterized stand-in. Pair
it with a "below threshold" sibling case so the assertion brackets the
condition from both sides — that's what makes the test fail (not just pass)
when the comparison operator is flipped.

**For uncovered fallback phases:** drive the public orchestrator
end-to-end with a fixture engineered so *only* the target phase can succeed:
- Make the flat filename differ from both the par2 basename and the
  flattened (`/`→`_`) name, so phases 1–2 miss.
- For a Phase-3-only fixture: give the `FileDesc` a correct `Hash16k` but
  leave `FileCRC32` at zero (e.g. by omitting the IFSC packet) so Phase 4's
  `FileCRC32 > 0` filter excludes the entry — only Phase 3 can match it.
- For a Phase-4-only fixture: give the `FileDesc` a *wrong* `Hash16k` (so
  Phase 3 misses) but a `FileCRC32` that the parser will reconstruct to match
  the flat file's actual CRC32. The easiest way to get a known reconstructed
  CRC: set `sliceSize == fileSize` so there is exactly one full slice with no
  tail padding — `crc32util.Combine(0, crc, n) == crc` (combining with an
  "empty prefix" CRC is the identity), so the IFSC slice's stored CRC32
  *is* the file's CRC32.

Add a one-line guard against the (vanishingly unlikely but real) case where
your synthetic content's CRC32 happens to be zero — that would silently
defeat the `FileCRC32 > 0` filter and test the wrong path:
```go
if actualCRC == 0 {
    t.Fatal("test content CRC32 is zero — pick different content")
}
```

## 4. Prove the test with manual mutation (red-green discipline)

This project's `AGENTS.md` requires red-green discipline for bug-fix tests —
applying the same proof to a *coverage-gap* test (where there's no "fix" to
revert) means manually mutating the targeted condition, confirming the new
test goes red for the right reason, then reverting:

```bash
# Example: prove the early-exit test catches a >= → > flip
sed -i 's/seenFileDescs >= expectedFiles && seenIFSC >= expectedFiles/seenFileDescs > expectedFiles \&\& seenIFSC > expectedFiles/' internal/par2/parser.go
go test ./internal/par2/ -run TestParsePar2Set_EarlyExitOptimization -v
git checkout internal/par2/parser.go
```

A test that passes against both the original *and* the mutated code is
testing nothing — fix the test, not the code, if that happens. Do this for
every new test before moving on; it's the only way to know you killed the
mutant you intended to, rather than getting lucky with an unrelated assertion.

## 5. Run the full quality gate suite

```bash
go fix ./internal/<package>/...
goimports -w internal/<package>/
go vet ./internal/<package>/...
go test -race ./internal/<package>/...
golangci-lint run ./internal/<package>/...
```

**Check for *new* lint findings, not just a clean run.** This codebase has
pre-existing accepted lint debt (e.g. `prealloc` on `var buf []byte; buf =
append(...)` patterns repeated through `parser_test.go`). Compare the
before/after issue counts (`git stash` → lint → `git stash pop` → lint) so
you don't get blocked by debt you didn't introduce, but also don't add to it
gratuitously — if your new code uses the same repeated-append pattern,
preallocate it (`make([]byte, 0, len(a)+len(b)+...)`) since you already know
every piece's size up front.

## 6. Re-run gremlins to measure the delta

```bash
./scripts/run_gremlins.sh ./internal/<package> 2>&1 | tail -10
```

Compare `Killed`/`Lived`/`Not covered` and `Test efficacy` against the
baseline. Then grep for the *specific* lines you targeted to confirm they
flipped from `LIVED`/`NOT COVERED` to `KILLED`:

```bash
./scripts/run_gremlins.sh ./internal/<package> 2>&1 \
  | grep -E "parser.go:(139|159|167|196|197|221|239):"
```

Some mutants may show `TIMED OUT` on a re-run under load — that's run-to-run
timing variance, not a regression; don't chase it.

It's normal for a few `CONDITIONALS_BOUNDARY` mutants to remain `LIVED` after
a focused pass (e.g. exact-zero-vs-positive distinctions like
`expectedFiles > 0` or `fd.FileCRC32 > 0 && fd.FileSize > 0`, which require
increasingly elaborate fixtures for diminishing returns). Note them and stop
— chasing 100% mutation score on a package is not the goal; closing the
*real, themed* gaps from step 2 is.

## 7. Commit with the before/after numbers in the body

Conventional commit, `test(<package>):` scope, body states *why* (which
mutation-testing-revealed gap this closes) and the measured score delta —
per this project's commit-hygiene rules, quantitative claims must be measured,
not estimated:

```
test(par2): guard parser boundary conditions and quickcheck phase 3/4 matching

Gremlins mutation testing flagged a cluster of LIVED/NOT-COVERED mutants in
the §4.5 early-exit scan optimization, packet/body length validation, and
quickcheck.go's Hash16k and CRC32+size relocation passes — code paths whose
boundary conditions and fallback branches no test exercised end-to-end.

[... what was added and why it proves the gap is closed ...]

Mutation score for internal/par2 improves from 70.2% to 80.8%
(139→173 killed, 59→41 lived).
```

## Quick checklist

- [ ] `./scripts/run_gremlins.sh ./internal/<pkg>` (background)
- [ ] Triage `LIVED`/`NOT COVERED`: mark equivalents, group real gaps by theme
- [ ] Prioritize whole-path/whole-phase gaps over single-line boundary nits
- [ ] Write tests matching existing fixture style; bracket boundaries on both sides
- [ ] Manually mutate each targeted line, confirm red, revert, confirm green
- [ ] `go vet` / `go test -race` / `golangci-lint` — diff issue counts vs. baseline
- [ ] Re-run gremlins, confirm targeted lines flipped to `KILLED`, record delta
- [ ] Commit with measured before/after mutation score in the body
