# Port v2 Improvements to v1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port seven targeted improvements identified by comparing `gonzbd2` (v2, at `/home/hobe/software/gonzbd2`) against this repo (v1), adapting each to v1's architecture (`os.Root` sandboxing, queue/assembler decoupling, existing test idioms) rather than transliterating v2's code.

**Architecture:** Each task is independent and touches a different subsystem. Tasks are TDD'd individually: write the failing test against current v1 behavior, watch it fail for the right reason, implement the minimal change, confirm green, run the package's full suite, commit. No task depends on another landing first — they can be executed in any order or in parallel by different workers. **Six of the seven are ready to implement; Task 6 is BLOCKED pending a user decision** (it requires a persistence-format change — see that task's section and the Self-Review Notes).

**Tech Stack:** Go 1.25, `gopkg.in/yaml.v3`, `github.com/pressly/goose/v3`, `modernc.org/sqlite`, standard `testing` package (table-driven, no mocking framework).

## Global Constraints

- Every `.go` file touched: run `goimports -w <file>`, `go fix ./...`, `go build ./...` immediately after editing (AGENTS.md "After Editing Any .go File").
- Quality gates before any commit: `go vet ./...`, `go test -race ./...` (scoped to the touched package is fine during development; full suite before the final commit of each task), `golangci-lint run ./...`.
- Conventional Commits 1.0.0: `<type>(<scope>): <description>`, lowercase, imperative, ≤72 chars. Use `fix`, `feat`, `refactor`, `test`, or `perf` as appropriate per task. Always append:
  ```
  Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
  ```
- Red-Green discipline (AGENTS.md): every test must be written first and observed to fail for the right reason before the implementation lands. "Fails to compile because the function doesn't exist yet" counts as a valid RED for new functions/fields.
- Do not run `gremlins` mutation testing as part of this plan's steps — it's a pre-push gate the user runs separately per AGENTS.md; each task's steps stop at green tests + lint clean.
- v2 source is reference-only, at `/home/hobe/software/gonzbd2`. Never copy v2 code verbatim where it conflicts with v1's decided architecture. (Task 6 is the cautionary case: its byte-exactness depends on v2 persisting decoded article geometry, which v1 does not — see that task's Correctness Finding.)

---

### Task 2: Sibling-rename bug fix via `originalStem`/`renameSiblings` extraction

**Files:**
- Modify: `internal/deobfuscate/deobfuscate.go` (extract lines ~389–427 into two new functions)
- Test: `internal/deobfuscate/deobfuscate_test.go`

**Background:** v1's `Deobfuscate` inlines its sibling-matching loop. When the biggest file's *pre-extension-fix* name contains an internal dot that isn't a real extension (e.g. `abc.xyz.somejunk` gets `.png` appended by content-sniffing → `abc.xyz.somejunk.png`), v1 computes the sibling-matching stem by trimming only the *appended* extension off the post-fix name, leaving `abc.xyz.somejunk`. A true sibling like `abc.xyz.eng.srt` shares only the true stem `abc.xyz` and is missed. v2 fixes this with an `originalStem` helper that looks up the pre-fix name via the extension-fix `Rename` records before trimming. Port that logic, adapted to v1's `os.Root`-relative rename mechanics (v2 uses plain `os` calls with absolute paths; v1 must keep using `root.Rename` / `root.Stat` / `fsutil.GetUniqueRelPath`).

**Interfaces:**
- Produces: `originalStem(bigPath string, extensionRenames []Rename) string` — pure string function, no I/O.
- Produces: `renameSiblings(log *slog.Logger, root *os.Root, dir, usefulName, bigPath string, paths, relPaths []string, extensionRenames []Rename, opts fsutil.SanitizeOptions) ([]Rename, error)` — replaces the inline loop in `Deobfuscate`.

- [ ] **Step 1: Write the failing test reproducing the bug**

Add to `internal/deobfuscate/deobfuscate_test.go`:

```go
func TestDeobfuscate_SiblingMatchesPreFixStem(t *testing.T) {
	dir := t.TempDir()

	// "abc.xyz" is the abc.xyz-obfuscation pattern (matched regardless of
	// what follows). The original name has no popular extension, so it
	// goes through FixExtension, which content-sniffs PNG magic bytes and
	// appends ".png" -- producing "abc.xyz.somejunk.png". A naive
	// last-extension trim of the POST-fix name leaves "abc.xyz.somejunk"
	// as the sibling-matching stem, which misses true siblings that only
	// share the pre-fix stem "abc.xyz".
	pngSig := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	big := append(pngSig, make([]byte, 11*1024*1024)...) // 11 MiB: clears the 10 MiB heuristic floor and the 3x BiggestFile ratio
	bigName := "abc.xyz.somejunk"
	if err := os.WriteFile(filepath.Join(dir, bigName), big, 0o644); err != nil {
		t.Fatal(err)
	}

	siblingName := "abc.xyz.eng.srt"
	sibling := []byte("1\n00:00:00,000 --> 00:00:01,000\nHello\n")
	if err := os.WriteFile(filepath.Join(dir, siblingName), sibling, 0o644); err != nil {
		t.Fatal(err)
	}

	renames, err := Deobfuscate(context.Background(), nil, dir, "Useful_Name", fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("Deobfuscate: %v", err)
	}

	var sawSibling bool
	for _, r := range renames {
		if filepath.Base(r.From) == siblingName {
			sawSibling = true
			if filepath.Base(r.To) != "Useful_Name.eng.srt" {
				t.Errorf("sibling renamed to %q, want %q", filepath.Base(r.To), "Useful_Name.eng.srt")
			}
		}
	}
	if !sawSibling {
		t.Errorf("sibling %q was not renamed (stem-matching bug); renames: %+v", siblingName, renames)
	}
}
```

Check the test file's existing imports include `context`, `os`, `path/filepath`, `testing`, and `github.com/hobeone/gonzbd/internal/fsutil` — add any missing ones with `goimports -w internal/deobfuscate/deobfuscate_test.go` after pasting.

- [ ] **Step 2: Run the test and confirm it fails for the right reason**

Run: `go test ./internal/deobfuscate/ -run TestDeobfuscate_SiblingMatchesPreFixStem -v`
Expected: FAIL with `sibling "abc.xyz.eng.srt" was not renamed (stem-matching bug)` — confirming the bug reproduces, not a setup error (e.g. not "no biggest file found" or "not obfuscated").

If it fails with a different message (e.g. the file wasn't classified as obfuscated, or BiggestFile didn't pick it), the fixture is wrong — fix the fixture, not the assertion, and re-run until the failure message is exactly the stem-matching one.

- [ ] **Step 3: Extract `originalStem` and `renameSiblings`**

In `internal/deobfuscate/deobfuscate.go`, add these two functions (place them after `containsIgnoredMovieFolder`, before `Subtitles`):

```go
// originalStem returns the path stem (no extension) of bigPath as it was
// BEFORE any extension-fix rename was applied. FixExtension only ever
// appends an extension to the existing name, so if bigPath is the result
// of such a rename, trimming bigPath's own last extension is not enough --
// it leaves any pre-existing pseudo-extension (e.g. ".somejunk" in
// "abc.xyz.somejunk.png") attached to the stem, which breaks matching
// against true siblings that only share the real pre-fix stem.
func originalStem(bigPath string, extensionRenames []Rename) string {
	origPath := bigPath
	for _, r := range extensionRenames {
		if r.To == bigPath {
			origPath = r.From
			break
		}
	}
	return strings.TrimSuffix(origPath, filepath.Ext(origPath))
}

// renameSiblings renames every file in paths/relPaths that shares bigPath's
// pre-fix stem (see originalStem) to usefulName plus its remaining suffix.
// bigPath itself is skipped (the caller already renamed it). Operates
// through root so all writes stay confined to dir, matching the rest of
// this package's os.Root sandboxing.
func renameSiblings(log *slog.Logger, root *os.Root, dir, usefulName, bigPath string, paths, relPaths []string, extensionRenames []Rename, opts fsutil.SanitizeOptions) ([]Rename, error) {
	var renames []Rename
	baseDirFile := originalStem(bigPath, extensionRenames)
	for i, p := range paths {
		if p == bigPath {
			continue
		}
		origP := p
		for _, r := range extensionRenames {
			if r.To == p {
				origP = r.From
				break
			}
		}
		if !strings.HasPrefix(origP, baseDirFile) {
			continue
		}
		rel := relPaths[i]
		if _, err := root.Stat(rel); err != nil {
			continue
		}
		remainingSuffix := strings.TrimPrefix(origP, baseDirFile) + strings.TrimPrefix(p, origP)
		relDstSib := fsutil.JoinSafe("", "", usefulName+remainingSuffix, opts)
		newRelSib := fsutil.GetUniqueRelPath(root, relDstSib)
		newPath := filepath.Join(dir, newRelSib)
		r, renErr := renameRecorded(log, root, rel, newRelSib, p, newPath, "", "deobfuscate: renamed sibling")
		if renErr != nil {
			return renames, fmt.Errorf("rename sibling %s → %s: %w", p, newPath, renErr)
		}
		renames = append(renames, r)
	}
	return renames, nil
}
```

- [ ] **Step 4: Replace the inline loop in `Deobfuscate` with a call to `renameSiblings`**

In `internal/deobfuscate/deobfuscate.go`, find this block (currently around lines 389–427):

```go
	renames := allRenames

	// Rename the biggest file.
	relDst := fsutil.JoinSafe("", "", usefulName+filepath.Ext(bigPath), opts)
	newBigRel := fsutil.GetUniqueRelPath(root, relDst)
	newBigPath := filepath.Join(dir, newBigRel)
	r, err := renameRecorded(log, root, bigRel, newBigRel, bigPath, newBigPath, "", "deobfuscate: renamed")
	if err != nil {
		return nil, fmt.Errorf("rename %s → %s: %w", bigPath, newBigPath, err)
	}
	renames = append(renames, r)

	// basedirfile is the path without extension — used to find siblings.
	baseDirFile := strings.TrimSuffix(bigPath, filepath.Ext(bigPath))

	// Rename siblings that share the same stem (e.g. "file-sample.iso").
	for i, p := range paths {
		rel := relPaths[i]
		if p == bigPath {
			continue
		}
		if !strings.HasPrefix(p, baseDirFile) {
			continue
		}
		if _, err := root.Stat(rel); err != nil {
			continue
		}
		remainingSuffix := strings.TrimPrefix(p, baseDirFile)
		relDstSib := fsutil.JoinSafe("", "", usefulName+remainingSuffix, opts)
		newRelSib := fsutil.GetUniqueRelPath(root, relDstSib)
		newPath := filepath.Join(dir, newRelSib)
		r, renErr := renameRecorded(log, root, rel, newRelSib, p, newPath, "", "deobfuscate: renamed sibling")
		if renErr != nil {
			return renames, fmt.Errorf("rename sibling %s → %s: %w", p, newPath, renErr)
		}
		renames = append(renames, r)
	}

	return renames, nil
}
```

Replace it with:

```go
	renames := allRenames

	// Rename the biggest file.
	relDst := fsutil.JoinSafe("", "", usefulName+filepath.Ext(bigPath), opts)
	newBigRel := fsutil.GetUniqueRelPath(root, relDst)
	newBigPath := filepath.Join(dir, newBigRel)
	r, err := renameRecorded(log, root, bigRel, newBigRel, bigPath, newBigPath, "", "deobfuscate: renamed")
	if err != nil {
		return nil, fmt.Errorf("rename %s → %s: %w", bigPath, newBigPath, err)
	}
	renames = append(renames, r)

	siblingRenames, err := renameSiblings(log, root, dir, usefulName, bigPath, paths, relPaths, allRenames, opts)
	renames = append(renames, siblingRenames...)
	if err != nil {
		return renames, err
	}

	return renames, nil
}
```

- [ ] **Step 5: Format, build, run the test**

Run: `goimports -w internal/deobfuscate/deobfuscate.go && go build ./internal/deobfuscate/`
Expected: clean build (no unused-import errors; `strings.TrimSuffix`/`HasPrefix`/`TrimPrefix` are still used elsewhere in the file via the new functions, so the `strings` import stays).

Run: `go test ./internal/deobfuscate/ -run TestDeobfuscate_SiblingMatchesPreFixStem -v`
Expected: PASS.

- [ ] **Step 6: Run the full package suite**

Run: `go test -race ./internal/deobfuscate/...`
Expected: all existing tests still PASS (the extraction is behavior-preserving for every case except the one the new test targets).

- [ ] **Step 7: Lint and commit**

Run: `go vet ./internal/deobfuscate/... && golangci-lint run ./internal/deobfuscate/...`
Expected: 0 new issues.

```bash
git add internal/deobfuscate/deobfuscate.go internal/deobfuscate/deobfuscate_test.go
git commit -m "$(cat <<'EOF'
fix(deobfuscate): match siblings against pre-extension-fix stem

Extract originalStem/renameSiblings so the sibling-matching stem is
computed from the file's name before FixExtension appended a detected
extension, not after. Trimming only the post-fix name's last extension
left any pre-existing pseudo-extension attached to the stem, causing
true siblings to be missed when the biggest file's original name
contained an internal dot that wasn't a real extension.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: YAML error diagnostics — `wrapYAMLError`

**Files:**
- Modify: `internal/config/loader.go`
- Test: `internal/config/loader_test.go`

**Background:** v1's `decode()` returns raw yaml.v3 errors with no source context. v2's `wrapYAMLError` extracts the `line N` reference yaml.v3 embeds in its error messages and appends a few lines of surrounding source so users can see exactly what's wrong without re-opening the file. Port it into v1's `decode`, wiring it into both the type-error-list path and the generic-error path.

**Interfaces:**
- Produces: `wrapYAMLError(err error, b []byte) error` — pure function, no I/O.
- Consumes: nothing new; uses the existing `partitionYAMLErrors` and `decode` already in `loader.go`.

- [ ] **Step 1: Write the failing test**

Add to `internal/config/loader_test.go`. Use a small standalone malformed YAML rather than building on the `minimalYAML(t)` helper: `decode` does not validate (only `Load` does), so the fixture needs only to be syntactically broken, and a short fixture keeps the reported line number small (2) so the asserted line is unambiguously inside the rendered context window. (Building on `minimalYAML` would append a second `general:` block and push the error to a large line number — both unnecessary noise.)

```go
func TestDecode_ErrorShowsSourceContext(t *testing.T) {
	t.Parallel()

	const malformed = "general:\n  host: [unclosed list\n"
	_, _, err := decode(strings.NewReader(malformed))
	if err == nil {
		t.Fatal("decode: expected a YAML syntax error, got nil")
	}
	if !strings.Contains(err.Error(), "Context:") {
		t.Errorf("error should contain a %q section, got:\n%v", "Context:", err)
	}
	if !strings.Contains(err.Error(), "host: [unclosed list") {
		t.Errorf("error should quote the offending line, got:\n%v", err)
	}
}
```

- [ ] **Step 2: Run it and confirm RED**

Run: `go test ./internal/config/ -run TestDecode_ErrorShowsSourceContext -v`
Expected: FAIL — the error exists (decode already returns an error for malformed YAML) but does not contain `"Context:"` or the quoted line, since `wrapYAMLError` doesn't exist yet.

- [ ] **Step 3: Implement `wrapYAMLError`**

In `internal/config/loader.go`, add these two declarations after `partitionYAMLErrors` (and add `regexp`, `strconv` to the import block):

```go
var yamlLineRegex = regexp.MustCompile(`line\s+(\d+)`)

// wrapYAMLError appends a few lines of source context around the line
// number yaml.v3 embeds in its error message (e.g. "line 5: ..."), so
// operators can see the offending YAML without re-opening the file. If
// the message has no parseable line number, err is returned unchanged.
func wrapYAMLError(err error, b []byte) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	matches := yamlLineRegex.FindStringSubmatch(msg)
	if len(matches) < 2 {
		return err
	}
	lineNum, pErr := strconv.Atoi(matches[1])
	if pErr != nil || lineNum <= 0 {
		return err
	}

	lines := bytes.Split(b, []byte("\n"))
	if lineNum > len(lines) {
		return err
	}

	var ctx strings.Builder
	ctx.WriteString(msg)
	ctx.WriteString("\nContext:\n")

	start := lineNum - 3
	if start < 0 {
		start = 0
	}
	end := lineNum + 2
	if end > len(lines) {
		end = len(lines)
	}

	for i := start; i < end; i++ {
		currLineNum := i + 1
		lineContent := strings.TrimSuffix(string(lines[i]), "\r")
		marker := " "
		if currLineNum == lineNum {
			marker = ">"
		}
		fmt.Fprintf(&ctx, "%s %3d | %s\n", marker, currLineNum, lineContent)
	}

	return errors.New(ctx.String())
}
```

- [ ] **Step 4: Wire `wrapYAMLError` into `decode`'s error paths**

In `internal/config/loader.go`, find the `decode` function's error-handling block:

```go
	if err := dec.Decode(cfg); err != nil {
		if errors.Is(err, io.EOF) {
			// Empty file — return defaults.
			return cfg, nil, nil
		}
		unknowns, fatal := partitionYAMLErrors(err)
		if fatal != nil {
			return nil, unknowns, fatal
		}
		return cfg, unknowns, nil
	}
```

Replace the `if fatal != nil` line with a wrapped version:

```go
	if err := dec.Decode(cfg); err != nil {
		if errors.Is(err, io.EOF) {
			// Empty file — return defaults.
			return cfg, nil, nil
		}
		unknowns, fatal := partitionYAMLErrors(err)
		if fatal != nil {
			return nil, unknowns, wrapYAMLError(fatal, b)
		}
		return cfg, unknowns, nil
	}
```

(`b` is already in scope — it's the `io.ReadAll(r)` result captured earlier in the same function.)

- [ ] **Step 5: Format, build, run the test**

Run: `goimports -w internal/config/loader.go && go build ./internal/config/`
Expected: clean. `goimports` will add `regexp` and `strconv` automatically; verify `bytes`, `errors`, `fmt`, `strings` are already imported (they are, per the existing file).

Run: `go test ./internal/config/ -run TestDecode_ErrorShowsSourceContext -v`
Expected: PASS.

- [ ] **Step 6: Run the full package suite, lint, commit**

Run: `go test -race ./internal/config/... && go vet ./internal/config/... && golangci-lint run ./internal/config/...`
Expected: all green, 0 new lint issues.

```bash
git add internal/config/loader.go internal/config/loader_test.go
git commit -m "$(cat <<'EOF'
feat(config): show source context around YAML parse errors

wrapYAMLError extracts the line number yaml.v3 embeds in its error
message and appends the surrounding lines of the source file, so
operators can see exactly what's wrong without re-opening
gonzbd.yaml.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: Goose migration provider modernization

**Files:**
- Modify: `internal/history/db.go`

**Background:** v1's `Open` uses the deprecated goose global-settings API (`goose.SetBaseFS` + `goose.SetDialect` behind a `sync.OnceValue`, then `goose.UpContext(ctx, sqlDB, "migrations")`). v2 uses the modern `goose.NewProvider(goose.DialectSQLite3, sqlDB, fs.FS)` + `provider.Up(ctx)`, which avoids global mutable state (a package-level goose dialect/baseFS setting shared across every `*DB` in the process) and is friendlier to concurrent or multi-database use. This is a refactor: no behavior change, so there's no new user-visible behavior to assert beyond "migrations still run and Open still succeeds" — which the existing test suite for this package already covers.

**Interfaces:** None — `Open`/`Close`'s signatures are unchanged. This is an internal implementation swap.

- [ ] **Step 1: Confirm existing coverage is sufficient as the regression guard**

Run: `go test ./internal/history/ -v 2>&1 | grep -i "open\|migrat"`
Expected: existing tests (e.g. `TestOpen...`, any migration-applying test) already exercise `Open()` end-to-end against a temp SQLite file. Read `internal/history/db_test.go` (or wherever `Open` is tested) to confirm there's a test that opens a fresh DB and checks no error — if there is, that test is the RED/GREEN guard for this refactor (it currently passes against the old implementation; it must keep passing against the new one). If no such test exists, write one first:

```go
func TestOpen_RunsMigrationsOnFreshDatabase(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
}
```

(Skip this sub-step entirely if equivalent coverage already exists — check before writing a duplicate.)

- [ ] **Step 2: Replace the global-settings goose wiring with `NewProvider`**

In `internal/history/db.go`, remove the package-level `initGooseErr` var and the `"sync"` import (it becomes unused once this is removed), and add `"io/fs"` to the import block. Replace:

```go
var initGooseErr = sync.OnceValue(func() error {
	goose.SetBaseFS(embedMigrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("history: set goose dialect: %w", err)
	}
	return nil
})
```

by deleting it entirely, and replace this block inside `Open`:

```go
	if err := initGooseErr(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("history: run migrations: %w", err)
	}
```

with:

```go
	subFS, err := fs.Sub(embedMigrations, "migrations")
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("history: sub fs: %w", err)
	}

	provider, err := goose.NewProvider(goose.DialectSQLite3, sqlDB, subFS)
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("history: new goose provider: %w", err)
	}

	if _, err := provider.Up(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("history: run migrations: %w", err)
	}
```

- [ ] **Step 3: Format, build**

Run: `goimports -w internal/history/db.go && go build ./internal/history/`
Expected: clean. `goimports` removes the now-unused `"sync"` import automatically; confirm `"io/fs"` was added.

- [ ] **Step 4: Run the full package suite**

Run: `go test -race ./internal/history/...`
Expected: all PASS, including the migration-running test from Step 1 — this is the proof the new provider applies the same migrations correctly.

- [ ] **Step 5: Lint and commit**

Run: `go vet ./internal/history/... && golangci-lint run ./internal/history/...`
Expected: 0 new issues.

```bash
git add internal/history/db.go
git commit -m "$(cat <<'EOF'
refactor(history): use goose.NewProvider instead of global settings

goose.SetBaseFS/SetDialect mutate package-level global state shared
across every *DB in the process. goose.NewProvider scopes the dialect
and migration FS to a single provider instance, removing the global
mutable state and the sync.OnceValue guard that existed only to make
that global setup idempotent.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: Default Usenet port normalization (port 0 → 119/563)

**Files:**
- Modify: `internal/config/loader.go` (the `applyNormalization` method)
- Test: `internal/config/loader_test.go`

**Background:** Today, a server entry with `port: 0` (or `port` omitted, which decodes to the Go zero value 0) fails `Validate()` with `"port: 0 is not allowed"` (`internal/config/validate.go:96-100`, `portInRange("port", s.Port, false)`). This forces every operator to know and write the right port number. Normalize port 0 to 119 (plain) or 563 (`ssl: true`) during `applyNormalization`, before `Validate()` runs, matching the documented default in the `ServerConfig.Port` field comment (`internal/config/servers.go:13`: "defaults are 119 (plain) and 563 (SSL)") which is currently aspirational, not enforced.

**Interfaces:** None new — this only changes `(cfg *Config) applyNormalization()`'s behavior, called from `decode` before `Validate()`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/loader_test.go`:

```go
func TestDecode_ServerPortDefaultsPlain(t *testing.T) {
	t.Parallel()
	yaml := minimalYAML(t) + "\nservers:\n  - name: s1\n    host: news.example.com\n    connections: 1\n"
	cfg, _, err := decode(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(cfg.Servers))
	}
	if cfg.Servers[0].Port != 119 {
		t.Errorf("Port = %d, want 119 (plain default)", cfg.Servers[0].Port)
	}
}

func TestDecode_ServerPortDefaultsSSL(t *testing.T) {
	t.Parallel()
	yaml := minimalYAML(t) + "\nservers:\n  - name: s1\n    host: news.example.com\n    connections: 1\n    ssl: true\n"
	cfg, _, err := decode(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(cfg.Servers))
	}
	if cfg.Servers[0].Port != 563 {
		t.Errorf("Port = %d, want 563 (ssl default)", cfg.Servers[0].Port)
	}
}

func TestDecode_ServerPortExplicitNotOverridden(t *testing.T) {
	t.Parallel()
	yaml := minimalYAML(t) + "\nservers:\n  - name: s1\n    host: news.example.com\n    connections: 1\n    port: 8119\n"
	cfg, _, err := decode(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Servers[0].Port != 8119 {
		t.Errorf("Port = %d, want 8119 (explicit value preserved)", cfg.Servers[0].Port)
	}
}
```

- [ ] **Step 2: Run them and confirm RED**

Run: `go test ./internal/config/ -run TestDecode_ServerPort -v`
Expected: `TestDecode_ServerPortDefaultsPlain` and `TestDecode_ServerPortDefaultsSSL` FAIL — `decode` currently returns a validation-style zero `Port` with no error from `decode` itself (validation happens later in `Load`, not in `decode`), so the assertion `cfg.Servers[0].Port != 119` / `!= 563` trips with `Port = 0`. `TestDecode_ServerPortExplicitNotOverridden` should already PASS (explicit values are untouched) — confirm it does, as a sanity check that the test fixture itself is correct.

- [ ] **Step 3: Implement the normalization**

In `internal/config/loader.go`, find `applyNormalization`:

```go
func (cfg *Config) applyNormalization() {
	if cfg.Downloads.ReplaceIllegalWith == "" {
		cfg.Downloads.ReplaceIllegalWith = "_"
	}
	if len(cfg.Downloads.CleanupList) == 0 {
		cfg.Downloads.CleanupList = DefaultCleanupList
	}
	if cfg.Servers == nil {
		cfg.Servers = []ServerConfig{}
	}
	if cfg.Categories == nil {
		cfg.Categories = []CategoryConfig{}
	}
}
```

Add the port-normalization loop at the end:

```go
func (cfg *Config) applyNormalization() {
	if cfg.Downloads.ReplaceIllegalWith == "" {
		cfg.Downloads.ReplaceIllegalWith = "_"
	}
	if len(cfg.Downloads.CleanupList) == 0 {
		cfg.Downloads.CleanupList = DefaultCleanupList
	}
	if cfg.Servers == nil {
		cfg.Servers = []ServerConfig{}
	}
	if cfg.Categories == nil {
		cfg.Categories = []CategoryConfig{}
	}
	for i := range cfg.Servers {
		if cfg.Servers[i].Port == 0 {
			if cfg.Servers[i].SSL {
				cfg.Servers[i].Port = 563
			} else {
				cfg.Servers[i].Port = 119
			}
		}
	}
}
```

- [ ] **Step 4: Build and run the new tests**

Run: `go build ./internal/config/ && go test ./internal/config/ -run TestDecode_ServerPort -v`
Expected: all three PASS.

- [ ] **Step 5: Run the full package suite**

Run: `go test -race ./internal/config/...`
Expected: all PASS. This change only normalizes **server** ports (`cfg.Servers[i].Port`); it does not touch `General.Port` (the HTTP port). The one existing test that asserts a `port: 0` rejection — `TestLoad_ValidationFailure` in `loader_test.go` — replaces `port: 4289` (the *General* HTTP port default) with `port: 0`, so it still fails validation via `portInRange("port", g.Port, false)` and is unaffected by this task. (Verified: no test asserts a *server* port-0 rejection. The `(*ServerConfig).validate()` unit tests in `validate_test.go` only construct servers with non-zero ports.) If you nonetheless hit a failing test asserting a server-port-0 rejection through `Load`, it would contradict the newly-documented default — update it to assert success with the normalized port.

- [ ] **Step 6: Lint and commit**

Run: `go vet ./internal/config/... && golangci-lint run ./internal/config/...`
Expected: 0 new issues.

```bash
git add internal/config/loader.go internal/config/loader_test.go
git commit -m "$(cat <<'EOF'
feat(config): default server port to 119/563 when unspecified

A server entry with no port (or port: 0) previously failed
validation, forcing every operator to know and write the right
Usenet port. applyNormalization now fills in the documented default
(119 plain, 563 ssl) before Validate runs, matching the ServerConfig.Port
field comment that already promised this behavior.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: Resumed write-cache coalescing stalls — BLOCKED, needs a persistence decision

> **Status: do not implement as part of this batch.** The original
> single-field `InitialWriteCursor` approach is **incorrect** (see Correctness
> Finding below), and every correct alternative changes the persisted queue
> state — which AGENTS.md's Decision Protocol says **must be escalated to the
> user** before implementation. This task is documented here so the finding
> isn't lost, but it should be lifted into its own plan after the user picks an
> approach. The other six tasks are independent and can land without it.

**The real bug (confirmed, worth fixing):** the write-coalescing cache
(`internal/assembler/writecache.go`) tracks a per-file `writeCursor` initialized
to 0 and only flushes a contiguous run starting *from that cursor*
(`buildContiguousRun`, `writecache.go:109-145`). On a **resumed** download the
already-downloaded prefix is `Done` and never re-dispatched
(`CountUnfinishedArticles` excludes `Done`, `pipeline.go:360-366`), so the first
article the assembler receives starts at some decoded offset > 0. The cache scans
`fb.articles[fb.writeCursor]` from 0, never finds it, and `flushContiguous`
returns `nil` forever — coalescing never engages for any resumed file; articles
only leave the cache via the 90%-pressure flush or the completion drain. Data is
still written correctly (no corruption); the optimization is simply dead on
resume. v2 fixes this.

**Correctness Finding (why the originally-proposed fix does not work):** the
proposed value was `InitialWriteCursor = Σ art.Bytes` over the `Done` prefix.
`art.Bytes` is the **NZB-declared encoded** segment size — the assembler itself
documents this: preallocation uses `Σ art.Bytes` and the code comments
(`assembler.go:179`, `:765`) state it is *"~2% larger than the actual decoded
content"* and must be truncated away at completion. But the write cache is keyed
by **decoded** offsets (`WriteRequest.Offset` comes from the decoder's yBegin
header). So `Σ art.Bytes` overshoots the true decoded cursor by the cumulative
yEnc overhead and can **never equal a cache key**. `buildContiguousRun` would
look at `fb.articles[overshoot]`, find nothing, and stall exactly as before. The
"safe estimate, falls back to current behavior" framing in the original draft is
technically true but hides that the fix *never* engages — it would add a queue
method, a `FileInfo` field, and pipeline plumbing for zero behavioral effect.

**Why v2 is correct and what it costs:** v2 persists the **decoded** offset and
size of each article (`queue.SetArticleDecodedInfo`, called by the assembler when
a range is durably written) and `queue.AdvanceWriteCursor` walks those persisted
decoded offsets to compute a byte-exact resume cursor
(`gonzbd2/internal/assembler/cache.go:35,213-229`, `assembler.go:637-642`). The
exactness comes entirely from storing decoded geometry in the queue — which the
v1 queue does not currently persist.

**The two correct options (both require a persisted-state change → escalate):**

1. **Per-file contiguous cursor (recommended — smaller).** Add a persisted
   `WriteCursor int64` to `queue.JobFile`. The assembler reports the advanced
   value through a *batched* callback (mirroring `MarkArticlesDone`, so the queue
   write-lock is taken per-batch, not per-article — required by the hot-path
   rules in AGENTS.md §7) whenever it advances `fb.writeCursor` past a flushed
   contiguous run. `pipeline.registerFile` reads it back as
   `FileInfo.InitialWriteCursor` on resume. Byte-exact because it stores the
   actual decoded frontier the cache reached. Note: only meaningful when
   `WriteCacheBytes > 0`; with caching off there is no coalescing to stall, so a
   cursor of 0 is correct.

2. **Per-article decoded info (faithful v2 port — larger).** Persist decoded
   `{offset,size}` per article and compute the cursor by walking the `Done`
   prefix, as v2 does. More state and more plumbing; only worth it if a later
   feature also needs per-article decoded geometry.

**Recommendation:** escalate using the AGENTS.md "Decision needed" format,
recommend Option 1, and — once approved — write a dedicated plan for it with the
batched-callback design spelled out and TDD'd (the byte-exactness and the
batching both need their own failing tests). Do **not** fold it into this batch.

**Escalation draft to present to the user:**

```
Decision needed: how to persist the resume write-cursor for coalescing

Context: the write cache never coalesces on resumed downloads because it
restarts its per-file cursor at 0 while the real first article arrives at a
decoded offset > 0. Fixing it byte-exactly requires persisting decoded write
geometry in the queue state (a persistence-format change), which the Decision
Protocol requires escalating.

Options:
1. Per-file WriteCursor on JobFile, advanced via a batched assembler callback —
   smaller, byte-exact, no per-article state. Persisted-state change: one new
   JobFile field.
2. Per-article decoded {offset,size} (faithful v2 port) — larger, reusable if
   other features need decoded geometry. Persisted-state change: per-article.
3. Do nothing — accept that resumed files don't coalesce (data is still written
   correctly; only the syscall-batching optimization is lost on resume).

Recommendation: Option 1 — byte-exact with the least new persisted state and the
least hot-path overhead.
```

---

### Task 7a: Self-contained sevenzip test fixtures

**Files:**
- Create: `internal/unpack/testdata/sevenzip/*.7z` and `*.7z.00N` (17 files, copied from `gonzbd2`)
- Modify: `internal/unpack/go_sevenzip_test.go`

**Background:** Every `TestGoSevenZip_*` test in `internal/unpack/go_sevenzip_test.go` calls `sevenZipTestdata(t)`, which looks for `../../../sevenzip/testdata` (a sibling project outside this repo) and `t.Skip`s if it's missing — which it is on this machine, so **every sevenzip test currently silently skips**. `gonzbd2` already vendored these fixtures locally at `internal/unpack/testdata/sevenzip/`. Copy them in and point the helper at the local path so the tests actually run.

**Interfaces:** None — `sevenZipTestdata`'s signature is unchanged; only its body and the files on disk change.

- [ ] **Step 1: Confirm the tests currently skip (the RED for this task)**

Run: `go test ./internal/unpack/ -run TestGoSevenZip -v 2>&1 | grep -c SKIP`
Expected: a non-zero count (every `TestGoSevenZip_*` test reports SKIP with the message `sevenzip testdata not found at .../sevenzip/testdata (run tests from gonzbd root)`).

- [ ] **Step 2: Copy the fixtures from gonzbd2**

```bash
mkdir -p internal/unpack/testdata/sevenzip
cp /home/hobe/software/gonzbd2/internal/unpack/testdata/sevenzip/*.7z internal/unpack/testdata/sevenzip/
cp /home/hobe/software/gonzbd2/internal/unpack/testdata/sevenzip/*.7z.0* internal/unpack/testdata/sevenzip/
ls internal/unpack/testdata/sevenzip/
```

Expected output: 17 files — `aes7z.7z`, `bcj.7z`, `bcj2.7z`, `brotli.7z`, `bzip2.7z`, `copy.7z`, `deflate.7z`, `empty.7z`, `lzma.7z`, `lzma2.7z`, `multi.7z.001` through `multi.7z.006`, `zstd.7z`.

- [ ] **Step 3: Point `sevenZipTestdata` at the local directory**

In `internal/unpack/go_sevenzip_test.go`, replace:

```go
// sevenZipTestdata returns the path to the bodgit/sevenzip testdata directory.
// Tests are skipped if the directory doesn't exist.
func sevenZipTestdata(t *testing.T) string {
	t.Helper()
	// Relative to the gonzbd project root: ../sevenzip/testdata
	dir := filepath.Join("..", "..", "..", "sevenzip", "testdata")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Skipf("sevenzip testdata not found at %s (run tests from gonzbd root)", dir)
	}
	return dir
}
```

with:

```go
// sevenZipTestdata returns the path to the local sevenzip testdata
// directory, vendored into this repo so these tests are self-contained
// and don't depend on a sibling project being checked out.
func sevenZipTestdata(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("testdata", "sevenzip")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Fatalf("local sevenzip testdata not found at %s", dir)
	}
	return dir
}
```

(`t.Fatalf` instead of `t.Skipf` is intentional: the fixtures are now committed to the repo, so a missing directory means something is actually broken, not an expected/acceptable environment gap.)

- [ ] **Step 4: Add the permissions assertion to `TestGoSevenZip_LZMA2`**

In the same file, find `TestGoSevenZip_LZMA2` and add this block right before its closing `}` (after the existing `t.Logf("Extracted %d files", len(res.ExtractedFiles))` line):

```go
	filePath := filepath.Join(outDir, res.ExtractedFiles[0])
	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("expected file %s to have mode 0644, got %o", res.ExtractedFiles[0], info.Mode().Perm())
	}
```

- [ ] **Step 5: Run the full sevenzip suite and confirm it actually executes (not skips)**

Run: `go test ./internal/unpack/ -run TestGoSevenZip -v`
Expected: every `TestGoSevenZip_*` test PASSes with `--- PASS`, none report `SKIP`. This is the GREEN — these tests were never exercised before; this run is the first time they've actually executed in this environment.

- [ ] **Step 6: Run the full package suite, lint, commit**

Run: `go test -race ./internal/unpack/... && go vet ./internal/unpack/... && golangci-lint run ./internal/unpack/...`
Expected: all PASS, 0 new lint issues.

```bash
git add internal/unpack/testdata/sevenzip internal/unpack/go_sevenzip_test.go
git commit -m "$(cat <<'EOF'
test(unpack): vendor sevenzip fixtures so tests stop silently skipping

Every TestGoSevenZip_* test depended on a sibling ../sevenzip/testdata
checkout that doesn't exist in this environment, so all of them
silently t.Skip'd. Copy the fixtures gonzbd2 already vendored locally
into internal/unpack/testdata/sevenzip and point the helper there,
making the suite self-contained. Also assert extracted-file
permissions are 0644 in the LZMA2 case.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

### Task 7b: Close ETA boundary-condition test gaps

**Files:**
- Modify: `internal/par2/cmdline_test.go`

**Background:** v1 already has `TestRepairOutput_ETA_BeforeStart`, `_ZeroPercent`, `_Complete`, and `_InProgress` in `cmdline_test.go`, covering the same ground as v2's single `TestRepairOutput_ETA` (just as separate top-level functions instead of one test with subtests) — porting v2's test verbatim would create near-duplicate coverage. The one real gap: `ETA()`'s guard is `ro.RepairPercent <= 0 || ro.RepairPercent >= 100` (`internal/par2/parse_output.go:89`), and v1 only tests the `== 0` / `== 100` boundary values, never the `< 0` / `> 100` ones. Add those two missing cases — cheap, and they close a `CONDITIONALS_BOUNDARY` mutation gap (`<=` → `<`, `>=` → `>`) per AGENTS.md's mutation-testing guidance.

**Interfaces:** None — pure test additions.

- [ ] **Step 1: Write the new boundary tests**

Add to `internal/par2/cmdline_test.go`, right after `TestRepairOutput_ETA_ZeroPercent`:

```go
func TestRepairOutput_ETA_NegativePercent(t *testing.T) {
	t.Parallel()
	ro := &RepairOutput{
		RepairStarted: time.Now().Add(-10 * time.Second),
		RepairPercent: -5.0,
	}
	if eta := ro.ETA(); eta != 0 {
		t.Errorf("ETA at -5%% = %v, want 0", eta)
	}
}

func TestRepairOutput_ETA_OverHundredPercent(t *testing.T) {
	t.Parallel()
	ro := &RepairOutput{
		RepairStarted: time.Now().Add(-10 * time.Second),
		RepairPercent: 105.0,
	}
	if eta := ro.ETA(); eta != 0 {
		t.Errorf("ETA at 105%% = %v, want 0", eta)
	}
}
```

- [ ] **Step 2: Run and confirm they currently pass (this is intentional — see note)**

Run: `go test ./internal/par2/ -run TestRepairOutput_ETA_NegativePercent\|TestRepairOutput_ETA_OverHundredPercent -v`
Expected: both PASS immediately, because `ETA()`'s existing `<= 0 || >= 100` guard already happens to handle `< 0` and `> 100` correctly — there is no bug here today. This is not a red-green cycle for a behavior change; it's closing a coverage gap so a future regression (e.g. someone "simplifies" the guard to `== 0 || == 100`) gets caught. Confirm the tests fail if you temporarily edit `parse_output.go`'s guard to `ro.RepairPercent == 0 || ro.RepairPercent == 100` — that is the actual proof these tests pin real behavior:

```bash
# Temporarily weaken the guard to prove the new tests catch it:
sed -i 's/RepairPercent <= 0 || ro.RepairPercent >= 100/RepairPercent == 0 || ro.RepairPercent == 100/' internal/par2/parse_output.go
go test ./internal/par2/ -run TestRepairOutput_ETA -v   # expect the two new tests to FAIL now
git checkout internal/par2/parse_output.go              # revert the temporary weakening
go test ./internal/par2/ -run TestRepairOutput_ETA -v   # expect all to PASS again
```

- [ ] **Step 3: Run the full package suite, lint, commit**

Run: `go test -race ./internal/par2/... && go vet ./internal/par2/... && golangci-lint run ./internal/par2/...`
Expected: all PASS, 0 new lint issues.

```bash
git add internal/par2/cmdline_test.go
git commit -m "$(cat <<'EOF'
test(par2): cover ETA's negative and over-100 percent boundaries

ETA()'s guard rejects RepairPercent <= 0 || >= 100, but existing
tests only exercised the == 0 / == 100 boundary values, leaving a
CONDITIONALS_BOUNDARY mutation gap on the < / > comparisons. No
behavior change -- ETA() already handles these correctly; this just
pins it so a future regression is caught.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

### Task 8: SPA catch-all 404s on missing files with an extension

**Files:**
- Modify: `internal/web/spa.go`
- Test: `internal/web/spa_test.go`

**Background:** v1's `NewSPAHandler` catch-all currently serves `index.html` for *any* unmatched path, including ones that look like static assets (e.g. a typo'd `/_app/missing.js` or a stale cached reference to a deleted hashed asset). That's wrong for paths with a file extension — those should 404, not silently return an HTML document that the browser will then fail to parse as JS/CSS. `gonzbd2`'s `web.go` (a different, simpler handler with no auth/cookie logic) already has this check via `path.Ext(name) != ""`. Port just that check into v1's existing handler — do not replace v1's handler with v2's, since v2's lacks the auth gating and API-key cookie logic v1's `NewSPAHandler` provides.

**Interfaces:** None — `NewSPAHandler`'s signature is unchanged.

- [ ] **Step 1: Write the failing test**

Add to `internal/web/spa_test.go`:

```go
func TestNewSPAHandler_MissingFileWithExtensionReturns404(t *testing.T) {
	handler := NewSPAHandler(testSPAFS(), func() string { return "test-key" }, nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", "/_app/does-not-exist.js", nil))

	if rr.Code != http.StatusNotFound {
		t.Errorf("GET /_app/does-not-exist.js status = %d, want 404", rr.Code)
	}
}
```

- [ ] **Step 2: Run and confirm RED**

Run: `go test ./internal/web/ -run TestNewSPAHandler_MissingFileWithExtensionReturns404 -v`
Expected: FAIL — `rr.Code` is 200 (the catch-all currently serves `index.html` for this path), not 404.

- [ ] **Step 3: Add the extension check before the catch-all fallback**

In `internal/web/spa.go`, find the catch-all block:

```go
		// SPA catch-all: serve index.html for client-side routing.
		if authCheck != nil && !authCheck(w, r) {
			return
		}
		// Clone the request so the original path is preserved for
		// upstream logging middleware.
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		setAPIKeyCookie(w, r)
		fileServer.ServeHTTP(w, r2)
	})
```

Replace with:

```go
		// A missing path with a file extension is a dead asset reference
		// (404), not a client-side route -- SPA routes are always
		// extension-less paths. Check before authCheck since a 404 for a
		// non-existent asset shouldn't require authentication to learn.
		if path.Ext(clean) != "" {
			http.NotFound(w, r)
			return
		}

		// SPA catch-all: serve index.html for client-side routing.
		if authCheck != nil && !authCheck(w, r) {
			return
		}
		// Clone the request so the original path is preserved for
		// upstream logging middleware.
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		setAPIKeyCookie(w, r)
		fileServer.ServeHTTP(w, r2)
	})
```

`clean` is already defined earlier in the handler (`clean := strings.TrimPrefix(path, "/")`), so it's in scope here. Add `"path"` to the import block (`net/http`, `strings`, `io/fs` are already imported; `path` is new).

- [ ] **Step 4: Build, run the new test**

Run: `goimports -w internal/web/spa.go && go build ./internal/web/`
Run: `go test ./internal/web/ -run TestNewSPAHandler_MissingFileWithExtensionReturns404 -v`
Expected: PASS.

- [ ] **Step 5: Run the full package suite — confirm no regression on the existing extension-less catch-all test**

Run: `go test -race ./internal/web/...`
Expected: all PASS, including `TestNewSPAHandler_UnknownPathFallsBackToIndex` (path `/some/deep/route`, no extension) which must still return 200 with `index.html` — this is the regression guard proving the new check is scoped to extensioned paths only.

- [ ] **Step 6: Lint and commit**

Run: `go vet ./internal/web/... && golangci-lint run ./internal/web/...`
Expected: 0 new issues.

```bash
git add internal/web/spa.go internal/web/spa_test.go
git commit -m "$(cat <<'EOF'
fix(web): 404 missing assets instead of serving index.html

The SPA catch-all served index.html for any unmatched path, including
ones with a file extension (e.g. a stale reference to a deleted
hashed asset, or a typo'd script path). SPA client-side routes are
always extension-less, so a missing path with an extension is a dead
asset reference and should 404, not silently return an HTML document
the browser will fail to parse as JS/CSS.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

## Self-Review Notes

- **Spec coverage:** all seven tasks from the source request (numbered 2–8, preserved verbatim for traceability) have a corresponding section above. Six are ready to implement. **Task 6 is BLOCKED** — see its section for the correctness finding; it needs a user decision on a persistence change before it can be written.
- **Deviations from the literal request, and why:**
  - Task 2's "originalStem/renameSiblings" port is adapted to v1's `os.Root` sandboxing throughout (not just "where needed") — v1 has no plain-`os`-call code path left in `Deobfuscate` to leave alone.
  - Task 6's originally-specified `InitialWriteCursor = Σ art.Bytes` approach is **incorrect** and has been replaced with a blocked-task writeup: `art.Bytes` is the NZB *encoded* size, but the write cache is keyed by *decoded* offsets, so the value never aligns and the "fix" would be a no-op. Every correct alternative changes persisted queue state, which AGENTS.md requires escalating. The section gives the finding, two correct options, and a ready-to-send escalation draft.
  - Task 7's "port TestRepairOutput_ETA" is downgraded to "add the two boundary cases v1 doesn't already cover" rather than porting a duplicate composite test, per the project's anti-redundant-test stance.
- **Placeholder scan:** every step in the six ready tasks has complete, copy-pasteable code or an exact shell command; no "TBD"/"handle edge cases"/"similar to Task N" language.
- **Type consistency:** within each ready task, every type/method/field name matches between its definition and its call sites (e.g. Task 2's `renameSiblings`/`originalStem` signatures, Task 5's port loop, Task 8's `path.Ext(clean)`).
