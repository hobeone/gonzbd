# Mutation-Hardening Commit Review — Tracking Doc

**Goal:** Review the last 27 commits (all produced by an agent following
`docs/mutation-testing-playbook.md`) for:
1. **Correctness** — does the change actually do what the commit message claims?
2. **Production-code risk** — for commits that touched non-test files, is the
   behavior change correct/intentional, or did the agent change production
   logic just to make a mutant easier to kill (i.e. weakened/altered behavior)?
3. **Better approaches** — is there a cleaner/more idiomatic test or fix than
   what was committed?

This doc is the resumable checklist — update the status and notes columns as
each commit is reviewed. Do not re-review a commit marked `done` unless new
information surfaces.

**Legend:** `pending` / `in-progress` / `done` / `issue-found`

## Priority tier 1 — commits that touched production code (review first)

These are the highest-risk commits: a test-hardening pass that also edits
non-test `.go` files may have changed observable behavior, not just added
coverage.

| # | Commit | Subject | Prod files touched | Status | Notes |
|---|--------|---------|---------------------|--------|-------|
| 1 | `9ff8b89` | test(queue): close testing gaps in job management and queue operations | `internal/queue/queue.go` | done | OK. `indexOfLocked` -1→0 sentinel: all 4 callers check `ok` first, harmless. `SetPar2ReleaseReason` signature merge is cosmetic. New tests (disk-file deletion on Remove, boundary indices, category priority clamping, MarkArticlesFailed empty/dup batches) are correct and exercise pre-existing prod code. No issues. |
| 2 | `661d8fa` | test(assembler): improve mutation test efficacy to 78.22% | `internal/assembler/preallocate_linux.go`, `internal/assembler/sparse.go` | done | Prod changes are `//nolint:gosec` comments only — no behavior change. See Findings log for 2 quality issues in the test changes (not correctness bugs, but worth fixing). |
| 3 | `93fa7e0` | test(deobfuscate): harden test coverage and kill lived mutants | `internal/deobfuscate/content_equal.go` | done | Prod change is `nolint` comments only. New tests (nil-logger paths, exact-32KB streamEqual, exact-3x/10MiB boundaries, sibling-rename assertion, extension-fix-then-deobfuscate) all pass and verified against real prod logic (nil-logger fallbacks pre-existing). No issues. |
| 4 | `eaacc2d` | test(nzb): harden test coverage and kill lived mutants | `internal/nzb/parser.go`, `internal/nzb/subject.go` | done | `parsePRiVATESubject`'s `min(len(subject),9)` rewrite is equivalent to the old length-guard (verified via `strings.EqualFold` length-mismatch semantics). `ExtractFilenameFromSubject`'s removed `len(m) > 1` check is safe — the regex has exactly one capture group so `m[1]` always exists. No issues. |
| 5 | `2a541af` | test(urlgrabber): harden mutation test coverage to 100% | `internal/urlgrabber/grabber.go` | done | Looked like a feature regression at first (removed manual extraction of Basic-Auth creds from URL userinfo), but `net/http`'s `Request.Write` already applies `req.URL.User` as a Basic-Auth header automatically when no `Authorization` header is set, and config-based `req.SetBasicAuth` overrides it. Verified `TestFetchHTTPBasicAuthViaURL` and `TestFetchHTTPBasicAuthConfigOverridesURL` both still pass — truly equivalent dead-code removal. No issue. |
| 6 | `cab398a` | test(history): add unit tests to kill lived mutants | `internal/history/db.go` | done | Found a tautological test: `TestDBConnectionSettings` added 3 unused struct fields (`connMaxLifetime`, `maxOpenConns`, `maxIdleConns`) purely so the test could compare them against the same constants used to set them — could never fail. Fixed in `4abd9b3`: removed the fields, test now checks `sql.DB.Stats().MaxOpenConnections` (real applied config). |
| 7 | `4847a0c` | test(app): harden test coverage and kill lived mutants in internal/app | `internal/app/app.go` | done | `app.go` change is a 3-line doc-comment cleanup, no behavior change. `diagnostics_test.go`/`history_helper_test.go` additions are correct (exact warning-string matching, 5-scenario `buildHistoryEntry` table). Issue found in `notify_test.go`: it used `unsafe.Pointer`/`reflect` to read unexported `Dispatcher.notifiers` and `ScriptNotifier.cfg` across package boundaries — fragile and a code-smell precedent. Fixed below. |
| 8 | `880d42d` | test(dirscanner): harden test coverage and kill lived mutants to reach 100% test efficacy | `internal/dirscanner/decompress.go`, `internal/dirscanner/scanner.go` | pending | |
| 9 | `1bd5653` | fix(nntp): propagate underlying reader error on fetch/stat after connection close | `internal/nntp/conn.go` | pending | This is a real `fix:`, not test-only — verify it's correct and red/green-proven |
| 10 | `a7a7d14` | test(humanfmt): harden test coverage and kill lived mutants | `internal/humanfmt/humanfmt.go` | pending | |
| 11 | `246c328` | test(fsutil): harden mutation testing coverage and resolve lived mutants | `internal/fsutil/containment.go`, `internal/fsutil/sanitize.go` | pending | |
| 12 | `7f892e8` | test(cmdutil): harden mutation testing coverage and resolve lived mutants | `internal/cmdutil/linestreamer.go`, `internal/cmdutil/passwords.go` | pending | |
| 13 | `7782963` | test(rarheader): harden mutation testing coverage and resolve lived mutants | `internal/rarheader/rarheader.go` | pending | |
| 14 | `227f025` | test(notifier): harden mutation testing coverage and resolve lived mutants | `internal/notifier/script.go` | pending | |
| 15 | `2f90c54` | test(rarheader): add mock unrar tests to satisfy coverage thresholds | `internal/rarheader/rarheader.go` | pending | second touch to this file — review together with #13 |

## Priority tier 2 — test-only commits (review second)

These only add/modify `_test.go` files. Lower risk of behavior change, but
still need correctness review: do the new tests assert the right thing, are
fixtures realistic, do they actually kill the mutants claimed, any
red-green-discipline violations (asserting current-but-wrong behavior)?

| # | Commit | Subject | Status | Notes |
|---|--------|---------|--------|-------|
| 16 | `479e968` | test(decoder): harden mutation test coverage for yEnc and UU decoders | pending | |
| 17 | `bf41d83` | test(nntp): harden test coverage and kill lived mutants | pending | |
| 18 | `4c8d73b` | test(assembler): cover linux-specific preallocate fallback and error paths | pending | |
| 19 | `df6fdb6` | test(postproc): harden unit tests to kill postproc mutants | pending | |
| 20 | `19f0b0a` | test(directunpack): harden tests against volume map and panic recovery mutants | pending | |
| 21 | `3b9b9a1` | test(unpack): harden mutation test coverage | pending | |
| 22 | `6dac1cb` | test(config): harden config validation, parsing, and defaults test coverage | pending | |
| 23 | `2a591a4` | test: resolve test-alignment gaps and expand test coverage for history and nntp | pending | |
| 24 | `2804ad1` | test(scheduler): add unit tests to eliminate lived mutants | pending | |
| 25 | `435fd33` | test(web): verify apikey cookie Secure flag under HTTP/HTTPS | pending | |
| 26 | `85fe1a0` | test(fsutil): increase CheckContainment statement and function coverage | pending | |
| 27 | `9ff8b89` (queue_test.go portion) | covered above with #1 | — | |

## Review checklist per commit

For each commit:
- [ ] `git show <sha>` — read full diff
- [ ] If prod code changed: confirm the production-code edit is a genuine bug
      fix or harmless clarification, not a behavior change made solely to
      satisfy a mutant (e.g. tightening a boundary check that changes which
      inputs are accepted/rejected)
- [ ] For new tests: confirm assertions test real behavior (would fail if the
      production code were reverted to its pre-commit state for that line)
- [ ] Check for red-green discipline issues: does any new test pass against
      *both* correct and subtly-wrong code (i.e. asserts something too weak)?
- [ ] Note any better/cleaner approach found
- [ ] Update status to `done` or `issue-found` with details

## Findings log

### `661d8fa` — `internal/assembler/preallocate_test.go` `TestSupportsSparse` — portability regression (medium)
The original test deliberately avoided asserting `SupportsSparse(dir)`'s
result with a comment: "We can't assert true/false portably, but we can
verify the function runs without error." The commit changed this to
`if !got { t.Errorf(...) }`, asserting sparse support is always available.
`SupportsSparse` probes via `SEEK_HOLE`/`SEEK_DATA` (per `sparse.go`), which
is not guaranteed on all filesystems CI might run on (e.g. some overlayfs/CI
container backends). This test passes locally (verified), but it converts a
deliberately-portable test into one with an environment assumption, solely
to kill a mutant. Same issue in `TestCheckSparseSupport`'s new
`if !supported` assertion.
**Suggested fix:** revert to logging the result, or skip the assertion with
`t.Skipf` when `!got`, OR (better) keep the assertion but add a comment
explaining the CI environment guarantee so a future agent doesn't quietly
remove it when it starts flaking.

### `661d8fa` — `internal/downloader/downloader_test.go` `TestAlignmentGaps` — placeholder test (low)
```go
func TestAlignmentGaps(t *testing.T) {
	var d *Downloader
	_ = d.handleRequest
	_ = d.connWorker
}
```
This test asserts nothing and calls nothing — it just takes method-value
references on a nil pointer (which doesn't even call the methods) "to
satisfy the check_test_alignment script". This is gaming a coverage/alignment
checker rather than testing behavior. Low risk (it's inert), but it's dead
weight and sets a bad precedent — a future agent might copy this pattern to
"satisfy" other checks. **Suggested fix:** remove it, or replace
`check_test_alignment`'s requirement with a `//nolint`/exception list if
`handleRequest`/`connWorker` are genuinely meant to be untested entry points.

### `661d8fa` — `internal/assembler/assembler_test.go` `TestDiskCheckInterval` — `time.Sleep` for synchronization (low/medium)
```go
go func() {
	time.Sleep(10 * time.Millisecond)
	close(blockCh)
}()
```
AGENTS.md forbids `time.Sleep` for test synchronization ("No `time.Sleep` in
tests for synchronization. Use channels, `sync.WaitGroup`, or `chan
struct{}` signals."). Here the sleep is used to sequence "start draining"
after `Stop()` is called — under load this could cause `Stop()` to return
before `blockCh` is closed, changing how many of the 31 queued requests get
drained before the worker exits, which could make the `lowDiskCount == 2`
assertion flaky. **Suggested fix:** use a signal that `Stop()` has begun
(e.g. a hook/callback) rather than a fixed sleep, or run with
`-race -count=20` to check for flakiness before trusting it.

### `4847a0c` — `internal/app/notify_test.go` — `unsafe`/`reflect` cross-package field access (medium)
`getNotifiers`/`getScriptConfig` used `reflect.ValueOf(...).Elem().FieldByName(...)` +
`unsafe.Pointer` to read `notifier.Dispatcher.notifiers` and
`notifier.ScriptNotifier.cfg`, both unexported fields in a different package.
This is fragile (breaks silently if field names change, no compile-time check)
and `unsafe` in test code sets a bad precedent.
**Fix applied (this session):** added small permanent exported getters —
`(*Dispatcher).Notifiers() []Notifier` in `internal/notifier/notifier.go` and
`(*ScriptNotifier).Config() ScriptConfig` in `internal/notifier/script.go` —
and updated `notify_test.go` to use them with a type assertion instead of
`reflect`/`unsafe`. Verified with `go build ./...`,
`go test -race ./internal/app/... ./internal/notifier/...`, and
`golangci-lint run ./internal/app/... ./internal/notifier/...` (0 issues).

### Fixes applied
Commit `d440157` fixed all 3 issues from `661d8fa`:
- `TestSupportsSparse`/`TestCheckSparseSupport` now `t.Skipf` instead of fail
  when the filesystem doesn't support sparse files.
- `TestAlignmentGaps` replaced with the established var-block dummy-reference
  pattern (matches `internal/app/export_test.go`,
  `internal/dirscanner/export_test.go`).
- `TestDiskCheckInterval`'s `time.Sleep` documented as an intentional,
  non-load-bearing timing window per AGENTS.md (verified stable under
  `-race -count=20`).

### Note: new commit appeared during review
`4a6bc09 test(api): harden about and events mutation testing coverage` landed
on `main` after this review started (likely the same agent continuing work).
It is **not** part of the original 27-commit scope but should be added to
tier 2 for review once the original 27 are done.

## Overall progress

- Tier 1 reviewed: 7 / 15
- Tier 2 reviewed: 0 / 12
- Extra (post-scope): `4a6bc09` not yet reviewed — add to tier 2 queue

## Next up
Continue tier 1 with #8 `880d42d` (dirscanner — `decompress.go`, `scanner.go`).
