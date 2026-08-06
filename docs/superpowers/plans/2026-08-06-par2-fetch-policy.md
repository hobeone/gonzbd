# Par2 Fetch Policy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `FileProgress.Deferred` with a `FetchPolicy` tri-state so `DiscardDeferredPar2` marks recovery volumes instead of deleting them, ending the `file_index` renumber that caused #294, #308, #310, #315 and #317.

**Architecture:** One `FetchPolicy` field replaces one bool on `FileProgress`, backed by a `fetch_policy` column replacing `deferred` on both `job_files` and `history_job_files`. Deleting the `FileDeferred` accessor turns all 28 call sites into compile errors, which is how the per-index loops that need a skip they never had get found. With the file set immutable after `Add`, the containment layer built to survive renumbering becomes unreferenced and is deleted.

**Tech Stack:** Go 1.26.4, SQLite via `modernc.org/sqlite`, `goose` migrations in `internal/history/migrations/`, Svelte 5 + TypeScript UI.

**Spec:** `docs/superpowers/specs/2026-08-06-par2-fetch-policy-design.md` (committed at `ad762089`).

**Base:** `main` at `bc59e4fa`. Branch off `docs/318-par2-fetch-policy-design`, which carries the spec.

## Global Constraints

- Module path `github.com/hobeone/gonzbd`; Go 1.26.4.
- **Never modify an existing goose migration file.** Add `009_replace_deferred_with_fetch_policy.sql`.
- Quality gates before every commit: `go fix ./...`, `goimports -w .`, `go vet ./...`, `go test -race ./...`, `golangci-lint run ./...`, plus `go run ./scripts/check_coverage`, `go run ./scripts/check_test_alignment`, `go run ./scripts/check_lock_io`.
- Never satisfy a gate by weakening it: no dummy tests, no `var _ = helper`, no `//nocover:` on code with real branching.
- Conventional Commits; scope is the Go package name. Footer `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.
- Never commit to `main`. All work lands via pull request.
- For a bug fix, write the failing test **first** and confirm it fails on unpatched code.
- No backwards-compatibility requirement: landing this work requires a full reset and reinstall (#318).
- The exact constant names are `FetchAlways`, `FetchIfNeeded`, `FetchNever`; the field is `FileProgress.Fetch`; the accessor is `JobProgress.FileFetchPolicy(fi int) FetchPolicy`.

---

## File Structure

| File | Responsibility in this change |
|---|---|
| `internal/history/migrations/009_replace_deferred_with_fetch_policy.sql` | **Create.** Column swap on both tables. |
| `internal/queue/progress.go` | `FetchPolicy` type, `FileProgress.Fetch`, `FileFetchPolicy`, the two guards, `sizeFigures`, `recompute`, JSON shape. |
| `internal/queue/job.go` | `IsComplete`; the `HasDeferredPar2`/`DeferredRecoveryIndices` wrappers; `ResetForRetry`'s downgrade; deletion of `manifestRowsStale`/`fileSetGen`. |
| `internal/queue/queue.go` | `ForEachUnfinishedArticle`; `DiscardDeferredPar2`; `undeferRecoveryLocked`. |
| `internal/queue/sqlite_store.go` | `insertJobFilesTx`, `updateTx`, `RestoreJobProgress`, `ArticleCountsByJob`, `HistoryFileProgress`, `RestoreRetryProgress`, the history retain INSERT; deletion of `ReplaceManifest`. |
| `internal/queue/store.go` | `FileMeta.Fetch`; deletion of `Store.ReplaceManifest`. |
| `internal/queue/persistence.go` | `newJobProgressSized`; deletion of `reconcileJobFiles`. |
| `internal/postproc/filelist.go` | The two #322 sites. |
| `internal/api/queue.go` | `fileState`, `firstIncompleteFile`, `Par2Held`. |
| `ui/src/lib/types.ts`, `ui/src/lib/components/QueueRow.svelte` | The `"skipped"` state. |
| `docs/queue-lifecycle.md` | Rewrite of lines 213–260. |

---

### Task 1: FetchPolicy replaces Deferred

Behaviour-neutral: nothing sets `FetchNever` yet. This task exists to absorb the call-site churn under a green test suite, so that Task 2's behaviour change lands as a small diff.

**Files:**
- Create: `internal/history/migrations/009_replace_deferred_with_fetch_policy.sql`
- Modify: `internal/queue/progress.go`, `internal/queue/job.go`, `internal/queue/queue.go`, `internal/queue/sqlite_store.go`, `internal/queue/store.go`, `internal/queue/persistence.go`, `internal/postproc/filelist.go`, `internal/api/queue.go`
- Test: `internal/queue/fetch_policy_test.go` (create)

**Interfaces:**
- Produces: `type FetchPolicy uint8` with `FetchAlways`/`FetchIfNeeded`/`FetchNever`; `FileProgress.Fetch FetchPolicy`; `(*JobProgress).FileFetchPolicy(fi int) FetchPolicy`; `FileMeta.Fetch FetchPolicy`; `RetainedFile.Fetch FetchPolicy`.
- Removes: `FileProgress.Deferred`, `(*JobProgress).FileDeferred`, `FileMeta.Deferred`, `RetainedFile.Deferred`.

- [ ] **Step 1: Write the migration**

Create `internal/history/migrations/009_replace_deferred_with_fetch_policy.sql`:

```sql
-- +goose Up
-- +goose StatementBegin
ALTER TABLE job_files ADD COLUMN fetch_policy INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE job_files SET fetch_policy = 1 WHERE deferred = 1;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE job_files DROP COLUMN deferred;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE history_job_files ADD COLUMN fetch_policy INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE history_job_files SET fetch_policy = 1 WHERE deferred = 1;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE history_job_files DROP COLUMN deferred;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE job_files ADD COLUMN deferred INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE job_files SET deferred = 1 WHERE fetch_policy = 1;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE job_files DROP COLUMN fetch_policy;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE history_job_files ADD COLUMN deferred INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE history_job_files SET deferred = 1 WHERE fetch_policy = 1;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE history_job_files DROP COLUMN fetch_policy;
-- +goose StatementEnd
```

> **Deviation from the spec, deliberate.** The spec says "no backfill". The two `UPDATE` lines are included anyway: `FetchAlways` is *not* the correct value for a row with `deferred = 1` — it would re-download a held volume — and the spec's "correct default for every row regardless" is only true of rows that were not deferred. The reset requirement makes this unobservable in practice, but a migration that is correct without relying on an out-of-band instruction costs one line. `Down` is lossy for `FetchNever` rows, which collapse to `deferred = 0`; that is acceptable because `Down` is a development affordance and `FetchNever` is re-derived by re-verification.

- [ ] **Step 2: Define the type and swap the field**

In `internal/queue/progress.go`, above `FileProgress`:

```go
// FetchPolicy records whether the job intends to download a file. It
// replaces a Deferred bool so that "held pending a verdict" and "proven
// unnecessary" cannot both be true, and so that every read site has to say
// which of the two it means.
type FetchPolicy uint8

const (
	// FetchAlways is every content file, the par2 index, and any recovery
	// volume the job has decided to fetch after all. It is the zero value
	// because it is the ordinary case for every file in a job.
	FetchAlways FetchPolicy = iota
	// FetchIfNeeded is a par2 recovery volume held back until the CRC
	// oracle rules on whether repair is needed.
	FetchIfNeeded
	// FetchNever is a recovery volume the oracle proved unnecessary. Its
	// manifest entry and job_files row stay; only the intent changes.
	FetchNever
)
```

In `FileProgress`, replace `Deferred bool` with:

```go
	// Fetch records whether this file will be downloaded. See FetchPolicy.
	Fetch FetchPolicy
```

Replace the `FileDeferred` accessor with:

```go
// FileFetchPolicy reports whether file fi will be downloaded, and if not,
// why. Out-of-range and nil receivers report FetchAlways, matching the
// permissive convention of the accessors either side of it.
func (p *JobProgress) FileFetchPolicy(fi int) FetchPolicy {
	if p == nil || fi < 0 || fi >= len(p.files) {
		return FetchAlways
	}
	return p.files[fi].Fetch
}
```

- [ ] **Step 3: Run the build to collect the call sites**

Run: `go build ./... 2>&1 | tee /tmp/fetch-policy-sites.txt`
Expected: FAIL. Roughly 28 errors naming `Deferred` or `FileDeferred`. This list is the work item for steps 4–8; do not fix them from memory.

- [ ] **Step 4: Convert the aggregates**

Four sites exclude any file that is not being fetched. All become `!= FetchAlways`.

`internal/queue/progress.go`, in `sizeFigures`:

```go
		if f.Fetch != FetchAlways {
			continue
		}
```

`internal/queue/progress.go`, in `recompute` — rename the local, since it now covers two reasons:

```go
		// Files that are not being fetched (on-demand par2 recovery volumes,
		// held or discarded) are never dispatched, so they contribute zero
		// pending work.
		fetching := p.files[fi].Fetch == FetchAlways
		for i := lo; i < hi; i++ {
			if fetching && !p.done.Get(i) && !p.emitted.Get(i) {
				n++
			}
```

`internal/queue/job.go`, in `IsComplete`:

```go
		if p.FileFetchPolicy(i) != FetchAlways {
			continue
		}
```

`internal/queue/queue.go`, in `ForEachUnfinishedArticle`:

```go
			// Files that are not being fetched (on-demand par2 recovery
			// volumes, held or discarded) already have Pending == 0 from
			// recompute, so the next check skips them too; the explicit
			// guard documents intent and protects against counter drift.
			if job.progress.files[fi].Complete || job.progress.files[fi].Pending == 0 || job.progress.files[fi].Fetch != FetchAlways {
				continue
			}
```

- [ ] **Step 5: Convert the serialization sites**

These carry the value; they do not filter on it.

`internal/queue/sqlite_store.go`, `insertJobFilesTx` — the column list becomes `fetch_policy` and the local becomes an integer conversion:

```go
	const qFiles = `
INSERT INTO job_files
  (job_id, file_index, subject, date, bytes, is_par2_recovery, complete, fetch_policy, write_cursor, bytes_downloaded, failed_bytes, filename, assembled_crc32, articles_done, article_count)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
```

and inside the loop, replacing the `complete, deferred := 0, 0` pair:

```go
		complete := 0
		if p.FileComplete(i) {
			complete = 1
		}
		// fetch_policy comes from the job, not a literal zero. Hard-coding
		// it discarded on-demand par2's whole effect (#287) back when this
		// column was `deferred`: NewJob holds each recovery volume in
		// JobProgress, the INSERT wrote 0, and the first promotion read that
		// back over the live value.
		fetch := int(p.FileFetchPolicy(i))
```

and in the `ExecContext` argument list, `complete, deferred,` becomes `complete, fetch,`.

`internal/queue/sqlite_store.go`, `updateTx`:

```go
		const qF = `UPDATE job_files SET complete = ?, fetch_policy = ?, write_cursor = ?, bytes_downloaded = ?, failed_bytes = ?, filename = ?, assembled_crc32 = ?, articles_done = ?, article_count = ? WHERE job_id = ? AND file_index = ?`
		for i := range m.NumFiles() {
			complete := 0
			if job.Progress().FileComplete(i) {
				complete = 1
			}
			fetch := int(job.Progress().FileFetchPolicy(i))
```

with `complete, deferred,` becoming `complete, fetch,` in the `ExecContext` call.

`internal/queue/sqlite_store.go`, `RestoreJobProgress` — the SELECT and its scan:

```go
	const qFiles = `
SELECT file_index, complete, fetch_policy, write_cursor, assembled_crc32, COALESCE(articles_done, '')
FROM job_files WHERE job_id = ? ORDER BY file_index ASC`
```

```go
		var idx, complete, fetch int
		var writeCursor int64
		var crc32Val uint32
		var artDoneStr string
		if err := rows.Scan(&idx, &complete, &fetch, &writeCursor, &crc32Val, &artDoneStr); err != nil {
```

and replacing the `if deferred != 0 { fp.Deferred = true }` block:

```go
			fp.Fetch = FetchPolicy(fetch)
```

Note the change of shape: the old code only ever *set* the flag, never cleared it. Assigning outright is correct here because `RestoreJobProgress` is the authority for a promotion, and leaving a stale in-memory `FetchNever` in place would survive a state the row says is over.

`internal/queue/sqlite_store.go`, `ArticleCountsByJob`:

```go
	const q = `SELECT job_id, file_index, article_count, bytes, bytes_downloaded, failed_bytes, complete, fetch_policy FROM job_files ORDER BY job_id ASC, file_index ASC`
```

with `var complete, deferred int` becoming `var complete, fetch int`, the `Scan` taking `&fetch`, and the struct literal field becoming:

```go
			Fetch: FetchPolicy(fetch),
```

`internal/queue/store.go`, in `FileMeta`, replace `Deferred bool` with:

```go
	// Fetch is the file's download intent, restored from
	// job_files.fetch_policy. See FetchPolicy.
	Fetch FetchPolicy
```

`internal/queue/persistence.go`, in `newJobProgressSized`, replace the `Deferred` assignment with:

```go
		p.files[fi].Fetch = f.Fetch
```

`internal/queue/progress.go`, the JSON shape — in `fileProgressJSON`:

```go
	Fetch FetchPolicy `json:"fetch_policy,omitempty"`
```

replacing `Deferred bool \`json:"deferred,omitempty"\``, with the corresponding `Fetch: f.Fetch` in `MarshalJSON` and `Fetch: f.Fetch` in `UnmarshalJSON`'s `FileProgress` literal.

- [ ] **Step 6: Convert the three policy-specific guards**

All three mean `FetchIfNeeded`, and getting any of them wrong is a live defect rather than a cosmetic one.

`internal/queue/progress.go`:

```go
// HasDeferredPar2 reports whether any file is still held pending the CRC
// verdict. Deliberately FetchIfNeeded only: a discarded volume is not held,
// it is decided, and reporting it as held would re-run the full CRC
// verification on every subsequent completion event.
func (p *JobProgress) HasDeferredPar2() bool {
	if p == nil {
		return false
	}
	for i := range p.files {
		if p.files[i].Fetch == FetchIfNeeded {
			return true
		}
	}
	return false
}

// DeferredRecoveryIndices returns the file indices of recovery volumes still
// held pending the verdict.
//
// FetchIfNeeded only, and that exclusion is load-bearing rather than tidy.
// undeferRecoveryLocked walks this list on any first-time permanent article
// failure while the job is not yet par2-recovered. If a discarded volume
// appeared here, one late failure would re-activate exactly the volumes the
// CRC oracle proved unnecessary — undoing on-demand par2 entirely.
func (p *JobProgress) DeferredRecoveryIndices() []int {
	var idxs []int
	for i := range p.files {
		if p.files[i].Fetch == FetchIfNeeded {
			idxs = append(idxs, i)
		}
	}
	return idxs
}
```

`internal/queue/queue.go`, in `undeferRecoveryLocked` — the second line of defence behind the list above:

```go
		if fi < 0 || fi >= job.manifest.NumFiles() || job.progress.files[fi].Fetch != FetchIfNeeded {
			continue
		}
		job.progress.files[fi].Fetch = FetchAlways
```

- [ ] **Step 7: Convert the postproc sites**

Both are #322 sites and read `!= FetchAlways`: a volume that was never downloaded is saved bandwidth whether the verdict has landed or not.

`internal/postproc/filelist.go:42`:

```go
		if p.FileFetchPolicy(fi) != FetchAlways {
			heldVols++
			heldBytes += m.FileBytes(fi)
		}
```

`internal/postproc/filelist.go:181`:

```go
		if p.FileFetchPolicy(fi) != FetchAlways {
			fileLines = append(fileLines, fmt.Sprintf("  - %s — not downloaded", name))
			continue
		}
```

These reference `queue.FetchAlways` from `internal/postproc`; add the qualifier as the package requires.

- [ ] **Step 8: Convert the API site and add the missing skip**

`internal/api/queue.go`, `fileState` — still `"held"` for now; the `"skipped"` state arrives in Task 5:

```go
	if p.FileFetchPolicy(fileIdx) != queue.FetchAlways {
		return "held"
	}
```

`internal/api/queue.go`, `firstIncompleteFile` — **this is a new skip, not a conversion.** The loop has never checked the flag, so a file that is never `Complete` and never fetched becomes the job's reported current file for the whole post-processing phase:

```go
	for i := range m.NumFiles() {
		// A file the job is not fetching is never Complete, so without this
		// skip a held or discarded recovery volume becomes the reported
		// current file for the rest of the job's life.
		if p.FileFetchPolicy(i) != queue.FetchAlways {
			continue
		}
		if !p.FileComplete(i) {
			return m.FileSubject(i)
		}
	}
```

- [ ] **Step 9: Verify the build is clean and the suite is green**

Run: `go build ./... && go test -race ./internal/... 2>&1 | grep -v "^ok" | head -30`
Expected: build succeeds; any failures are in tests that construct `FileProgress{Deferred: true}` literals. Convert those to `FileProgress{Fetch: FetchIfNeeded}`. There are roughly 30 such test sites; they are mechanical.

- [ ] **Step 10: Write the guard tests**

Create `internal/queue/fetch_policy_test.go`:

```go
package queue

import "testing"

// TestDeferredRecoveryIndices_ExcludesDiscarded pins the resurrection guard.
// undeferRecoveryLocked walks this list on any first-time permanent article
// failure, so a discarded volume appearing here re-downloads exactly what the
// CRC oracle proved unnecessary. Widening the predicate to `!= FetchAlways`
// must fail this test.
func TestDeferredRecoveryIndices_ExcludesDiscarded(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 1000, Articles: []JobArticle{{ID: "a1", Bytes: 1000}}},
		{Subject: "a.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 800}}},
		{Subject: "a.vol001+02.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v2", Bytes: 800}}},
	})
	p := newJobProgress(m)
	p.files[1].Fetch = FetchIfNeeded
	p.files[2].Fetch = FetchNever

	got := p.DeferredRecoveryIndices()
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("DeferredRecoveryIndices() = %v, want [1] — a discarded volume must never be offered for un-deferral", got)
	}
}

// TestHasDeferredPar2_FalseOnceEverythingIsDecided pins the release gate. The
// par2 release path re-runs full CRC verification while this reports true, so
// a discarded volume counted as held costs a verification pass per completion
// event for the rest of the job.
func TestHasDeferredPar2_FalseOnceEverythingIsDecided(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 1000, Articles: []JobArticle{{ID: "a1", Bytes: 1000}}},
		{Subject: "a.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 800}}},
	})
	p := newJobProgress(m)

	p.files[1].Fetch = FetchIfNeeded
	if !p.HasDeferredPar2() {
		t.Error("HasDeferredPar2() = false with a volume awaiting the verdict, want true")
	}
	p.files[1].Fetch = FetchNever
	if p.HasDeferredPar2() {
		t.Error("HasDeferredPar2() = true with every volume decided, want false — the release path would re-verify on every completion event")
	}
}

// TestSizeFigures_ExcludesDiscarded pins that a discarded volume leaves both
// derived figures, not just remaining. Expected must drop it too, or the
// downloaded identity in internal/app/history_helper.go over-reports.
func TestSizeFigures_ExcludesDiscarded(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 1000, Articles: []JobArticle{{ID: "a1", Bytes: 1000}}},
		{Subject: "a.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 800}}},
	})
	p := newJobProgress(m)
	p.files[1].Fetch = FetchNever

	expected, remaining := p.sizeFigures()
	if expected != 1000 {
		t.Errorf("expected = %d, want 1000 (a discarded volume is not part of what the job will fetch)", expected)
	}
	if remaining != 1000 {
		t.Errorf("remaining = %d, want 1000", remaining)
	}
}
```

- [ ] **Step 10b: Test the new skip in firstIncompleteFile**

This is a behaviour change, not a conversion, so it needs its own test. Create `internal/api/first_incomplete_test.go`:

```go
package api

import "testing"

// TestFirstIncompleteFile_SkipsUnfetchedVolumes pins the skip added with the
// fetch policy. A recovery volume that is never downloaded is never Complete,
// so without the skip it becomes the job's reported current file for the
// whole post-processing phase — a loop a reviewer classifies correctly as
// index-space and still ships the bug.
func TestFirstIncompleteFile_SkipsUnfetchedVolumes(t *testing.T) {
	q, job := newOnDemandPar2Job(t)

	// File 0 is the only content file; complete it. File 1 is the recovery
	// volume, held and therefore never Complete.
	if err := q.MarkFileComplete(job.ID, 0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}

	if got := firstIncompleteFile(job); got != "" {
		t.Errorf("firstIncompleteFile = %q, want empty — a held recovery volume was reported as the current file", got)
	}
}
```

Run: `go test -run TestFirstIncompleteFile_SkipsUnfetchedVolumes -count=1 ./internal/api/`
Expected: PASS. Then revert the skip added in Step 8, re-run, and confirm it fails with `firstIncompleteFile = "content.vol000+01.par2"`. Restore the skip.

- [ ] **Step 11: Run the guard tests**

Run: `go test -run 'TestDeferredRecoveryIndices_ExcludesDiscarded|TestHasDeferredPar2_FalseOnceEverythingIsDecided|TestSizeFigures_ExcludesDiscarded' -count=1 ./internal/queue/`
Expected: PASS.

- [ ] **Step 12: Mutation-verify the resurrection guard**

Temporarily widen `DeferredRecoveryIndices`'s predicate to `p.files[i].Fetch != FetchAlways`, re-run the test, and confirm it reports `[1 2], want [1]`. Restore the predicate. A guard test that passes under its own mutation is not pinning anything.

- [ ] **Step 13: Run the full gates**

```bash
go fix ./... && goimports -w . && go vet ./... && go test -race ./... && golangci-lint run ./...
go run ./scripts/check_coverage && go run ./scripts/check_test_alignment && go run ./scripts/check_lock_io
```

- [ ] **Step 14: Commit**

```bash
git add -A
git commit -m "refactor(queue)!: replace the Deferred flag with a fetch policy

FileProgress.Deferred becomes FetchPolicy, with job_files.fetch_policy and
history_job_files.fetch_policy replacing the deferred columns (migration
009). Deleting FileDeferred rather than adding a second flag makes every
read site a compile error, which is how api.firstIncompleteFile was found:
it has never checked the flag, so a held recovery volume becomes the job's
reported current file for the whole post-processing phase.

Behaviour-neutral. Nothing sets FetchNever yet.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 2: DiscardDeferredPar2 marks instead of rebuilding

**Files:**
- Modify: `internal/queue/queue.go:1833-2004`
- Test: `internal/queue/discard_marks_test.go` (create)

**Interfaces:**
- Consumes: `FetchPolicy`, `FetchIfNeeded`, `FetchNever` from Task 1.
- Produces: `DiscardDeferredPar2` no longer calls `Store.ReplaceManifest`, `bumpFileSetGen`, `setManifestRowsStale`, `setResidency` or `setScalarsFromManifest`. Task 4 depends on this.

- [ ] **Step 1: Write the failing tests**

Create `internal/queue/discard_marks_test.go`:

```go
package queue

import (
	"testing"
)

// TestDiscardDeferredPar2_KeepsFileIndicesStable is the whole point of the
// change. Removing a non-final file renumbered every file_index after it, and
// job_files rows are keyed by that index, which is the root of #294, #308,
// #310, #315 and #317.
func TestDiscardDeferredPar2_KeepsFileIndicesStable(t *testing.T) {
	q := New()
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 1000, Articles: []JobArticle{{ID: "a1", Bytes: 1000}}},
		{Subject: "a.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 800}}},
		{Subject: "b.rar", Bytes: 2000, Articles: []JobArticle{{ID: "b1", Bytes: 2000}}},
	})
	job := &Job{ID: "discard-stable"}
	job.setResidency(m, newJobProgress(m))
	job.setScalarsFromManifest(m)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q.jobs[0].progress.files[1].Fetch = FetchIfNeeded

	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}

	gotM, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if gotM.NumFiles() != 3 {
		t.Fatalf("NumFiles = %d, want 3 — the file set must not shrink", gotM.NumFiles())
	}
	// The index of the file *after* the discarded one is what a rebuild moved.
	if got := gotM.FileSubject(2); got != "b.rar" {
		t.Errorf("file 2 = %q, want %q — indices after the discarded file moved", got, "b.rar")
	}
	if got := job.Progress().FileFetchPolicy(1); got != FetchNever {
		t.Errorf("file 1 policy = %d, want FetchNever", got)
	}
}

// TestDiscardDeferredPar2_LeavesFiguresUnchanged pins that the discard needs
// no accounting fixup. Both derived figures already exclude a non-fetched
// file, so moving FetchIfNeeded to FetchNever changes neither.
func TestDiscardDeferredPar2_LeavesFiguresUnchanged(t *testing.T) {
	q := New()
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 1000, Articles: []JobArticle{{ID: "a1", Bytes: 1000}}},
		{Subject: "a.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 800}}},
	})
	job := &Job{ID: "discard-figures"}
	job.setResidency(m, newJobProgress(m))
	job.setScalarsFromManifest(m)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q.jobs[0].progress.files[1].Fetch = FetchIfNeeded

	beforeExpected := job.Progress().ExpectedBytes()
	beforeRemaining := job.Progress().RemainingBytes()

	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}

	if got := job.Progress().ExpectedBytes(); got != beforeExpected {
		t.Errorf("ExpectedBytes moved across the discard: %d -> %d", beforeExpected, got)
	}
	if got := job.Progress().RemainingBytes(); got != beforeRemaining {
		t.Errorf("RemainingBytes moved across the discard: %d -> %d", beforeRemaining, got)
	}
	// TotalBytes is the immutable whole-manifest figure and must now stay put.
	if got, want := job.TotalBytes(), int64(1800); got != want {
		t.Errorf("TotalBytes = %d, want %d — the immutable total must not shrink", got, want)
	}
}

// TestDiscardDeferredPar2_LateFailureDoesNotResurrect is the hazard #318
// names. undeferRecoveryLocked runs on a first-time permanent failure; a
// discarded volume must not come back.
func TestDiscardDeferredPar2_LateFailureDoesNotResurrect(t *testing.T) {
	q := New()
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 2000, Articles: []JobArticle{{ID: "a1", Bytes: 1000}, {ID: "a2", Bytes: 1000}}},
		{Subject: "a.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 800}}},
	})
	job := &Job{ID: "discard-no-resurrect"}
	job.setResidency(m, newJobProgress(m))
	job.setScalarsFromManifest(m)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q.jobs[0].progress.files[1].Fetch = FetchIfNeeded
	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}

	if _, err := q.MarkArticlesFailed(job.ID, []string{"a2"}); err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
	}

	if got := job.Progress().FileFetchPolicy(1); got != FetchNever {
		t.Errorf("file 1 policy = %d, want FetchNever — a late failure resurrected a discarded volume", got)
	}
	var offered []string
	q.ForEachUnfinishedArticle(func(a UnfinishedArticle) bool {
		offered = append(offered, a.MessageID)
		return true
	})
	for _, id := range offered {
		if id == "v1" {
			t.Error("a discarded recovery volume was offered for dispatch after a late failure")
		}
	}
}
```

- [ ] **Step 2: Run to verify the first test fails**

Run: `go test -run TestDiscardDeferredPar2_KeepsFileIndicesStable -count=1 ./internal/queue/`
Expected: FAIL with `NumFiles = 2, want 3`. This confirms the rebuild is still in place and the test is measuring it.

- [ ] **Step 3: Replace the body**

In `internal/queue/queue.go`, replace everything from `m := job.manifest` to the closing `}` before `return nil` with:

```go
	changed := false
	for fi := range len(job.progress.files) {
		if job.progress.files[fi].Fetch == FetchIfNeeded {
			job.progress.files[fi].Fetch = FetchNever
			changed = true
		}
	}
	if changed {
		q.dirty.Store(true)
	}
	return nil
```

and replace the doc comment with:

```go
// DiscardDeferredPar2 records that every recovery volume still awaiting the
// CRC verdict will never be downloaded. The file set does not change: the
// manifest entries and job_files rows stay exactly where they are, and only
// the fetch policy moves.
//
// This used to rebuild the manifest without those files. Removing a non-final
// file renumbers every file_index after it, and job_files rows are keyed by
// that index, which is the root of #294, #308, #310, #315 and #317. The only
// purpose removal ever served was accounting, and both derived size figures
// already exclude a file that is not being fetched, so there is nothing left
// for it to correct.
//
// No store write of its own. Nothing about the file set changed, so the
// ordinary checkpoint writes the new policy through updateTx like any other
// per-file state, and the operation cannot partially apply.
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run TestDiscardDeferredPar2 -count=1 ./internal/queue/`
Expected: PASS, all three.

- [ ] **Step 5: Fix the existing discard tests**

Run: `go test -count=1 ./internal/queue/ 2>&1 | head -40`
Expected: failures in `discard_persistence_test.go` and `ondemand_par2_test.go`, which assert the rebuild's shrunk file counts and its `ReplaceManifest` call. Rewrite each to assert the marking. Do not delete an assertion without replacing what it covered — a test that asserted "the file set shrank" becomes one that asserts "the file set did not shrink and the policy moved".

- [ ] **Step 5b: Test the round trip and the non-resident view**

The spec requires `file_index` stability asserted at both residencies, and the
policy surviving a restart. Add to `internal/queue/discard_marks_test.go`:

```go
// TestDiscardDeferredPar2_SurvivesRestart pins that the discard needs no
// special persistence. Nothing about the file set changed, so an ordinary
// checkpoint carries it, and the non-resident view built from job_files
// agrees with the resident one.
func TestDiscardDeferredPar2_SurvivesRestart(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	job := makeMultiFileJob(t, "discard-restart", 2, 1)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q.jobs[0].progress.files[1].Fetch = FetchIfNeeded
	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	metas, err := store.ArticleCountsByJob(t.Context())
	if err != nil {
		t.Fatalf("ArticleCountsByJob: %v", err)
	}
	got := metas[job.ID]
	if len(got) != 2 {
		t.Fatalf("stored %d files, want 2 — the row set must not shrink", len(got))
	}
	if got[1].Fetch != FetchNever {
		t.Errorf("stored fetch policy = %d, want FetchNever", got[1].Fetch)
	}

	// The non-resident reconstruction must agree with the resident figures.
	nonResident := newJobProgressSized(got)
	if a, b := nonResident.ExpectedBytes(), job.Progress().ExpectedBytes(); a != b {
		t.Errorf("non-resident ExpectedBytes = %d, resident = %d", a, b)
	}
	if a, b := nonResident.RemainingBytes(), job.Progress().RemainingBytes(); a != b {
		t.Errorf("non-resident RemainingBytes = %d, resident = %d", a, b)
	}
}
```

Run: `go test -run TestDiscardDeferredPar2_SurvivesRestart -count=1 ./internal/queue/`
Expected: PASS.

- [ ] **Step 6: Verify the #322 report now fires**

Run: `go test -count=1 ./internal/postproc/`
Expected: PASS. If a `filelist` test asserted that the savings report does *not* fire, it was encoding #322 as intended behaviour and must be inverted.

- [ ] **Step 7: Run the full gates**

```bash
go fix ./... && goimports -w . && go vet ./... && go test -race ./... && golangci-lint run ./...
go run ./scripts/check_coverage && go run ./scripts/check_test_alignment && go run ./scripts/check_lock_io
```

- [ ] **Step 8: Commit**

```bash
git add -A
git commit -m "fix(queue): mark discarded par2 volumes instead of removing them

DiscardDeferredPar2 sets FetchNever on every volume awaiting the verdict
and leaves the file set alone. The manifest rebuild, the article-bitset
copy, its panic guard, the scalar re-sync and the inline ReplaceManifest
write are all gone with it.

Closes #322: the savings report reads the volumes at finalize because they
are still there.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 3: Retry downgrades FetchNever to FetchIfNeeded

**Files:**
- Modify: `internal/queue/job.go` (`ResetForRetry`), `internal/queue/sqlite_store.go` (`RetainedFile`, `HistoryFileProgress`, `RestoreRetryProgress`, the history retain INSERT)
- Test: `internal/queue/retry_fetch_policy_test.go` (create)

**Interfaces:**
- Consumes: `FetchPolicy` from Task 1.
- Produces: `RetainedFile.Fetch FetchPolicy`.

- [ ] **Step 1: Write the failing tests**

Create `internal/queue/retry_fetch_policy_test.go`:

```go
package queue

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

// TestResetForRetry_DowngradesDiscardedToHeld pins the retry rule. The clean
// verdict was computed against one download's damage profile, and a retry
// re-fetches the articles that failed, so the contents the oracle certified
// may differ — the verdict is re-derived rather than inherited.
//
// It must be a downgrade, not a reset: FetchAlways here would re-download
// every recovery volume, which is #323.
func TestResetForRetry_DowngradesDiscardedToHeld(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 1000, Articles: []JobArticle{{ID: "a1", Bytes: 1000}}},
		{Subject: "a.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 800}}},
		{Subject: "a.vol001+02.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v2", Bytes: 800}}},
	})
	job := &Job{ID: "retry-downgrade", Status: constants.StatusFailed}
	job.setResidency(m, newJobProgress(m))
	job.progress.files[1].Fetch = FetchNever
	job.progress.files[2].Fetch = FetchIfNeeded

	job.ResetForRetry()

	if got := job.Progress().FileFetchPolicy(1); got != FetchIfNeeded {
		t.Errorf("discarded volume after retry = %d, want FetchIfNeeded (FetchAlways would re-download it, which is #323)", got)
	}
	if got := job.Progress().FileFetchPolicy(2); got != FetchIfNeeded {
		t.Errorf("held volume after retry = %d, want FetchIfNeeded (unchanged)", got)
	}
	if got := job.Progress().FileFetchPolicy(0); got != FetchAlways {
		t.Errorf("content file after retry = %d, want FetchAlways", got)
	}
}
```

Create the store-side test in `internal/queue/history_fetch_policy_test.go`. Note the package: the history helpers live in the **external** `queue_test` package, so the fixture reaches the policy through `AddOptions.OnDemandPar2` rather than by assigning the field.

```go
package queue_test

import (
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// TestMoveToHistory_CarriesFetchPolicy pins the carry, separately from the
// downgrade in TestResetForRetry_DowngradesDiscardedToHeld. RetainedFile
// copies a fixed field list, and a field that is not carried arrives as the
// zero value — FetchAlways — which re-downloads every recovery volume on a
// history retry. That is #323, and it fails silently, so it needs its own
// assertion rather than riding on the downgrade test.
//
// The value asserted is FetchIfNeeded rather than FetchNever because either
// proves the carry: an uncarried field is FetchAlways regardless of which
// non-zero value it held, and FetchIfNeeded is reachable from the public API.
func TestMoveToHistory_CarriesFetchPolicy(t *testing.T) {
	store, _, _ := setupTestStore(t)

	parsed := &nzb.NZB{
		Files: []nzb.File{
			{Subject: "content.rar", Bytes: 10_000, Articles: []nzb.Article{{ID: "c1@t", Bytes: 10_000, Number: 1}}},
			{Subject: "content.vol000+01.par2", Bytes: 1_000, Articles: []nzb.Article{{ID: "v1@t", Bytes: 1_000, Number: 1}}},
		},
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: "par2.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if !job.HasDeferredPar2() {
		t.Fatal("fixture guard: recovery volume not held — nothing is being tested")
	}
	if err := store.Add(t.Context(), job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	entry := history.Entry{
		NzoID:     job.ID,
		Name:      job.Name,
		Status:    string(constants.StatusFailed),
		Completed: time.Now(),
		TimeAdded: job.Added,
	}
	if err := store.MoveToHistory(t.Context(), job, entry); err != nil {
		t.Fatalf("MoveToHistory: %v", err)
	}

	retained, err := store.HistoryFileProgress(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("HistoryFileProgress: %v", err)
	}
	if len(retained) != 2 {
		t.Fatalf("retained %d files, want 2", len(retained))
	}
	if got := retained[1].Fetch; got != queue.FetchIfNeeded {
		t.Errorf("retained fetch policy = %d, want FetchIfNeeded — an uncarried field arrives as FetchAlways and re-downloads the volume (#323)", got)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -run 'TestResetForRetry_DowngradesDiscardedToHeld|TestRestoreRetryProgress_CarriesFetchPolicy' -count=1 ./internal/queue/`
Expected: the first FAILs with `want FetchIfNeeded` (the policy is untouched by `ResetForRetry`); the second fails to compile until `RetainedFile.Fetch` exists.

- [ ] **Step 3: Add the downgrade to ResetForRetry**

In `internal/queue/job.go`, inside `ResetForRetry`'s per-file loop, after the existing `if anyReset { j.progress.files[fi].Complete = false }`:

```go
		// A retry re-derives the par2 verdict rather than inheriting it. The
		// clean verdict was computed against the contents this retry is about
		// to change, so the volumes go back to awaiting a decision.
		//
		// Downgrade, not reset: FetchAlways would re-download every recovery
		// volume the oracle already ruled unnecessary, which is #323.
		if j.progress.files[fi].Fetch == FetchNever {
			j.progress.files[fi].Fetch = FetchIfNeeded
		}
```

- [ ] **Step 4: Carry the policy through the history round trip**

In `internal/queue/sqlite_store.go`, `RetainedFile`: replace `Deferred bool` with `Fetch FetchPolicy`.

The retain INSERT:

```go
		const qRetain = `
INSERT INTO history_job_files
  (job_id, file_index, complete, fetch_policy, write_cursor, bytes_downloaded, filename, assembled_crc32, articles_done, article_count)
SELECT job_id, file_index, fetch_policy, write_cursor, bytes_downloaded, filename, assembled_crc32, articles_done, article_count
FROM job_files WHERE job_id = ?`
```

> Careful: the column list and the SELECT list must stay the same length and order. Written out in full:
> `(job_id, file_index, complete, fetch_policy, write_cursor, bytes_downloaded, filename, assembled_crc32, articles_done, article_count)` paired with
> `SELECT job_id, file_index, complete, fetch_policy, write_cursor, bytes_downloaded, filename, assembled_crc32, articles_done, article_count`.

`HistoryFileProgress`:

```go
	const q = `
SELECT file_index, complete, fetch_policy, write_cursor, bytes_downloaded,
       COALESCE(filename, ''), COALESCE(assembled_crc32, 0), COALESCE(articles_done, ''), article_count
FROM history_job_files WHERE job_id = ? ORDER BY file_index ASC`
```

```go
		var complete, fetch int
		if err := rows.Scan(&f.FileIndex, &complete, &fetch, &f.WriteCursor,
			&f.BytesDownloaded, &f.Filename, &f.AssembledCRC32, &f.ArticlesDone, &f.ArticleCount); err != nil {
			return nil, fmt.Errorf("sqlite store scan history_job_file %s: %w", jobID, err)
		}
		f.Complete = complete != 0
		f.Fetch = FetchPolicy(fetch)
```

`RestoreRetryProgress`, replacing the `if f.Deferred { fp.Deferred = true }` block:

```go
		// Downgrade on the way in, for the reason ResetForRetry gives: the
		// verdict is re-derived, not inherited. Carrying the value at all is
		// what matters most here — an uncarried field arrives as FetchAlways
		// and re-downloads every recovery volume (#323).
		if f.Fetch == FetchNever {
			fp.Fetch = FetchIfNeeded
		} else {
			fp.Fetch = f.Fetch
		}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -run 'TestResetForRetry_DowngradesDiscardedToHeld|TestRestoreRetryProgress_CarriesFetchPolicy' -count=1 ./internal/queue/`
Expected: PASS.

- [ ] **Step 6: Mutation-verify the carry**

Temporarily change `RestoreRetryProgress`'s assignment to drop the `else` branch (so only `FetchNever` is handled and `FetchIfNeeded` falls through to the zero value). Confirm a test fails. If none does, the fixture does not cover a held-but-not-discarded volume on the history path and needs one.

- [ ] **Step 7: Run the full gates and commit**

```bash
go fix ./... && goimports -w . && go vet ./... && go test -race ./... && golangci-lint run ./...
go run ./scripts/check_coverage && go run ./scripts/check_test_alignment && go run ./scripts/check_lock_io
git add -A
git commit -m "fix(queue): re-derive the par2 verdict on retry instead of inheriting it

A retry moves FetchNever back to FetchIfNeeded on both routes: ResetForRetry
for a live job, and RestoreRetryProgress for one rebuilt from its NZB. The
verdict was computed against contents the retry is about to change.

Downgrade rather than reset. RetainedFile now carries the policy, because an
uncarried field arrives as FetchAlways and re-downloads every recovery volume.

Closes #323.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 4: Tear down the containment layer

**Files:**
- Modify: `internal/queue/store.go`, `internal/queue/sqlite_store.go`, `internal/queue/job.go`, `internal/queue/persistence.go`, `internal/queue/queue.go`, `internal/queue/snapshot.go`, `docs/queue-lifecycle.md`

**Interfaces:**
- Consumes: Task 2's `DiscardDeferredPar2`, which no longer calls any of this.
- Removes: `Store.ReplaceManifest`, `SQLiteStore.ReplaceManifest`, `Job.manifestRowsStale`, `Job.ManifestRowsStale`, `Job.setManifestRowsStale`, `Job.fileSetGen`, `Job.FileSetGen`, `Job.bumpFileSetGen`, `Job.clearManifestRowsStaleIfGen`, `Queue.reconcileJobFiles`.

> **Deviation from the spec, deliberate — please read.** The spec's teardown table lists `ErrManifestStale` and `describesSameJobAs` for removal. **This plan keeps both.** They are not part of the renumber containment: `describesSameJobAs` converts a `Manifest`/`JobProgress` size mismatch from a panic on a background goroutine with no `recover` into a reported error, and that value survives the discard's removal. What changes is only its *cause* — after this work no write path this system performs can produce the mismatch, so it guards on-disk corruption rather than a torn `ReplaceManifest`. Deleting it would turn a corrupt or truncated manifest blob into a background panic. The doc comments must be rewritten to say so, since both currently explain themselves entirely in terms of the discard.

- [ ] **Step 1: Delete the interface method first**

In `internal/queue/store.go`, delete the `ReplaceManifest(ctx context.Context, job *Job) error` method and its doc comment from the `Store` interface.

Run: `go build ./... 2>&1 | head -20`
Expected: FAIL, naming every remaining caller. Deleting the interface method first makes the "no callers left" claim a compiler result rather than a grep result.

- [ ] **Step 2: Delete reconcileJobFiles and its call**

In `internal/queue/persistence.go`, delete `reconcileJobFiles` (lines 40–108) entirely, and its invocation at line 125 (`q.reconcileJobFiles(ctx, snapshots)`).

- [ ] **Step 3: Delete the SQLiteStore implementation**

In `internal/queue/sqlite_store.go`, delete `func (s *SQLiteStore) ReplaceManifest(...)` and its doc comment. Update `insertJobFilesTx`'s doc comment, which says it is "Shared by addTx and ReplaceManifest":

```go
// Shared by addTx and nothing else now — it was written to be shared with
// ReplaceManifest, which no longer exists because the file set is immutable
// after Add. Kept as a function because the row shape is worth naming in one
// place: a column a writer forgets is a column that silently reverts, which
// is what #287 was.
```

- [ ] **Step 4: Delete the staleness flag and generation counter**

In `internal/queue/job.go`, delete the `manifestRowsStale bool` and `fileSetGen uint64` fields with their comments, and the four methods `ManifestRowsStale`, `setManifestRowsStale`, `FileSetGen`, `bumpFileSetGen`, `clearManifestRowsStaleIfGen`.

In `internal/queue/snapshot.go`, delete the two copy lines:

```go
	cp.manifestRowsStale = j.manifestRowsStale
	cp.fileSetGen = j.fileSetGen
```

In `internal/queue/sqlite_store.go`, `updateTx`, delete the `if job.ManifestRowsStale() { ... } else if` branch so the condition becomes:

```go
	if m, mErr := job.Manifest(); job.Progress() != nil && mErr == nil {
```

- [ ] **Step 5: Rewrite the two surviving guards' rationale**

In `internal/queue/queue.go`, replace `ErrManifestStale`'s doc comment:

```go
// ErrManifestStale is returned when the manifest stored on disk contradicts
// the job's own JobProgress — different file or article counts — so the two
// cannot be paired.
//
// Reporting it rather than tolerating it is not optional: handing a
// mismatched pair to JobProgress.recompute panics by design, and hydration
// runs on background goroutines that carry no recover.
//
// No write path this process performs can produce the mismatch any more. It
// had one ordinary cause until #294 — DiscardDeferredPar2 rebuilding a
// smaller manifest without rewriting the blob — and one residual cause after
// that, a torn Store.ReplaceManifest whose blob write and transaction could
// not be made atomic. Both are gone: the file set is now immutable after Add,
// which writes the blob before opening the transaction, so a crash between
// them leaves an orphan manifest and no job row rather than a disagreeing
// pair.
//
// What this guard actually checks is manifest-versus-progress size
// agreement — NumFiles/NumArticles against len(progress.files)/done.Len() —
// so what it can detect is a truncated or damaged manifest blob. It cannot
// detect job_files rows altered out of band: RestoreJobProgress fills
// progress.files by file_index under a bounds check and never resizes it,
// so rows deleted or renumbered outside this process pass this guard
// silently and land per-article state on the wrong file's slot instead of
// raising this error. Reporting the mismatches this can see is still worth
// doing; the alternative is not "no error" but a panic on a goroutine with
// no recover, which is strictly worse for the same underlying state.
//
// The boot path (SQLiteStore.Get) carries no guard at all — it sizes
// progress from the manifest it reads and fills it by file_index with no
// describesSameJobAs check, so a manifest/job_files disagreement at startup
// is undetected either way. What to do about that is #278, open.
var ErrManifestStale = errors.New("queue: stored manifest does not match the job's progress")
```

In `internal/queue/progress.go`, replace `describesSameJobAs`'s doc comment paragraph beginning "The two can genuinely disagree":

```go
// This compares sizes only — NumFiles/NumArticles against
// len(p.files)/p.done.Len() — so it detects a manifest blob whose shape
// disagrees with progress, which used to happen through a torn
// Store.ReplaceManifest write and now happens only through on-disk
// corruption (the file set is immutable after Add, and ReplaceManifest is
// gone). It does NOT detect job_files rows altered out of band:
// SQLiteStore.RestoreJobProgress fills progress.files by file_index without
// resizing it, so a row deleted or renumbered outside this process still
// satisfies this size check and silently attaches its state to the wrong
// file. See ErrManifestStale for the boot-path gap (#278), which this
// guard does not cover either.
```

- [ ] **Step 6: Verify nothing is left**

Run:

```bash
grep -rn "ReplaceManifest\|manifestRowsStale\|ManifestRowsStale\|fileSetGen\|FileSetGen" --include="*.go" . | grep -v "_test.go"
```

Expected: no output. Then run the same grep including tests and delete or rewrite each test that exercised the removed machinery — `reconcileJobFiles`'s two cases and the `ReplaceManifest` failure paths have tests that will no longer compile.

- [ ] **Step 7: Rewrite the documentation**

In `docs/queue-lifecycle.md`, replace lines 213–260 — the section describing `ReplaceManifest`, the torn-write window, `manifestRowsStale`, `fileSetGen` and the reconcile retry. The replacement must say: the file set is fixed at `Add`; `file_index` is stable for the life of a job; a recovery volume that is never downloaded keeps its row and carries `fetch_policy`; and the only remaining manifest/progress disagreement is corruption, reported as `ErrManifestStale`.

This also discharges the salvage obligation recorded when #317 was closed — its "Across a restart" material was left pending on this step because the restart story changes once the renumber is gone.

- [ ] **Step 8: Run the full gates and commit**

```bash
go fix ./... && goimports -w . && go vet ./... && go test -race ./... && golangci-lint run ./...
go run ./scripts/check_coverage && go run ./scripts/check_test_alignment && go run ./scripts/check_lock_io
git add -A
git commit -m "refactor(queue): remove the manifest-rewrite containment layer

ReplaceManifest, manifestRowsStale, fileSetGen and reconcileJobFiles all
existed to survive DiscardDeferredPar2 renumbering file_index. Nothing
renumbers any more, and DiscardDeferredPar2 was their only entry point.

ErrManifestStale and describesSameJobAs stay. They are not renumber
containment: they turn a Manifest/JobProgress size mismatch into a reported
error instead of a panic on a goroutine with no recover. Their cause is now
on-disk corruption rather than a torn write, and their comments say so.

Closes #320 and #321, both defects inside the removed layer.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 5: Surface the skipped state

**Files:**
- Modify: `internal/api/queue.go`, `ui/src/lib/types.ts:49`, `ui/src/lib/components/QueueRow.svelte:286`
- Test: `internal/api/queue_fetch_policy_test.go` (create)

**Interfaces:**
- Consumes: `queue.FetchPolicy` from Task 1.

- [ ] **Step 1: Write the failing test**

Create `internal/api/queue_fetch_policy_test.go`:

```go
package api

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/queue"
)

// TestFileState_DistinguishesHeldFromSkipped pins the two reasons a file is
// not being fetched. "held" is awaiting the CRC verdict; "skipped" is the
// verdict having come back clean, which is the on-demand par2 saving made
// visible per file rather than only in the history summary.
func TestFileState_DistinguishesHeldFromSkipped(t *testing.T) {
	q, job := newOnDemandPar2Job(t)
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	p := job.Progress()

	// File 1 is the recovery volume, held pending the verdict.
	if got := fileState(m, p, 1); got != "held" {
		t.Errorf("held volume state = %q, want %q", got, "held")
	}
	if got := fileState(m, p, 0); got == "held" || got == "skipped" {
		t.Errorf("content file state = %q, want a fetched state", got)
	}

	// The verdict comes back clean.
	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}
	if got := fileState(m, p, 1); got != "skipped" {
		t.Errorf("discarded volume state = %q, want %q", got, "skipped")
	}
}
```

`newOnDemandPar2Job` already exists in `internal/api/queue_nonresident_test.go:109` and returns `(*queue.Queue, *queue.Job)` with file 0 as content and file 1 as a recovery volume. Reuse it rather than adding an exported test-only setter to `internal/queue` — a mutator on production code that exists only for a test is not acceptable here.

- [ ] **Step 2: Run to verify it fails**

Run: `go test -run TestFileState_DistinguishesHeldFromSkipped -count=1 ./internal/api/`
Expected: FAIL with `discarded volume state = "held", want "skipped"`.

- [ ] **Step 3: Split the state in fileState**

```go
	switch p.FileFetchPolicy(fileIdx) {
	case queue.FetchIfNeeded:
		return "held"
	case queue.FetchNever:
		return "skipped"
	}
```

- [ ] **Step 4: Widen Par2Held**

In `internal/api/queue.go`, at the `Par2Held:` assignment:

```go
		Par2Held: j.UsesOnDemandPar2(),
```

and add to `internal/queue/job.go`:

```go
// UsesOnDemandPar2 reports whether any recovery volume is being withheld from
// download — either awaiting the CRC verdict or already ruled unnecessary.
//
// Distinct from HasDeferredPar2, which is FetchIfNeeded only because it gates
// re-verification. This drives the "par2 on-demand" badge, which describes
// what the job is doing rather than what it is waiting on: reported as
// HasDeferredPar2, the badge would disappear at the moment the feature
// succeeds.
func (j *Job) UsesOnDemandPar2() bool {
	if j.progress == nil {
		return false
	}
	for i := range j.progress.files {
		if j.progress.files[i].Fetch != FetchAlways {
			return true
		}
	}
	return false
}
```

- [ ] **Step 5: Update the UI types**

`ui/src/lib/types.ts:49`:

```ts
	state: 'queued' | 'downloading' | 'done' | 'failed' | 'held' | 'skipped';
```

`ui/src/lib/components/QueueRow.svelte`, in `fileStateColor`:

```ts
			case 'held': return 'text-slate-500 dark:text-slate-400';
			case 'skipped': return 'text-slate-400 dark:text-slate-500 italic';
```

- [ ] **Step 6: Run the tests**

Run: `go test -count=1 ./internal/api/ && cd ui && npm run check && npm test`
Expected: PASS.

- [ ] **Step 7: Run the full gates and commit**

```bash
go fix ./... && goimports -w . && go vet ./... && go test -race ./... && golangci-lint run ./...
./scripts/run_tests.sh
go run ./scripts/check_coverage && go run ./scripts/check_test_alignment && go run ./scripts/check_lock_io
git add -A
git commit -m "feat(api): distinguish skipped par2 volumes from held ones

fileState reports \"skipped\" for a volume the CRC verdict ruled
unnecessary, separately from \"held\" for one still awaiting it, so the
on-demand par2 saving is visible per file rather than only in the history
summary.

par2_held now reports any withheld volume rather than only a waiting one.
Reported as HasDeferredPar2 the badge disappeared at the moment the feature
succeeded. Semantic change under an unchanged JSON key.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Post-implementation

- [ ] Verify the integration suite, which this plan's unit runs exclude: `go test -v -tags=integration ./test/integration/...`
- [ ] Close #322 and #323 with a reference to the merge commit.
- [ ] Close #320 and #321 as subsumed, naming Task 4's commit and stating that the code each describes no longer exists.
- [ ] Update #318 with the steps completed and what remains (the size-figures spec's step 2: `content_bytes` / `recovery_bytes`, the API change and the UI change).
- [ ] Consider `./scripts/run_gremlins.sh ./internal/queue` before opening the PR — this change adds substantial branching to a package whose guards are load-bearing.
