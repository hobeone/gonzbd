# Derived remaining bytes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the maintained `JobProgress.remainingBytes` counter with a value derived on read from per-file progress, so no state has to be adjusted when a file is deferred, un-deferred or discarded.

**Architecture:** `FileProgress` gains a `Bytes` field, populated from the manifest when a job is resident and from the existing `job_files.bytes` column when it is not. `RemainingBytes()` then sums `Bytes - BytesDownloaded` over files that are neither complete nor deferred, from progress alone at any residency. The `remainingBytes` field, its per-article decrements, its seeds, and `DiscardDeferredPar2`'s downward fixup are deleted.

**Tech Stack:** Go 1.26, SQLite via `modernc.org/sqlite`, goose migrations. No new dependencies.

## Global Constraints

- Step 1 of the sequencing in `docs/superpowers/specs/2026-08-05-job-size-figures-design.md`. **No reported figure may change.** `content_bytes`/`recovery_bytes` and the API change are step 2; the `Discarded` flag is step 3.
- The derivation in this step excludes `Complete` and `Deferred` only. `Discarded` does not exist yet.
- **One schema migration, in Task 2 only.** `job_files.bytes` already existed; `job_files.failed_bytes` is added by `internal/history/migrations/008_add_job_files_failed_bytes.sql`. This overrides the original no-migration constraint — the user authorised it explicitly after Task 3's agreement test proved the derivation cannot be correct without per-file failed bytes. Never edit an existing migration file. No other task may add one.
- **No backwards compatibility is required.** Landing this work in production takes a full reset and reinstall, so migrations need not preserve existing rows' semantics beyond what the `DEFAULT` gives.
- After editing any `.go` file: `goimports -w <file>`, `go fix ./...`, `go build ./...`.
- Gates before each commit: `go vet ./...`, `go test -race ./internal/queue/...`, `golangci-lint run ./...`.
- Conventional Commits, scope `queue`. Co-author trailer: `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.
- Branch from `main` (`55ee6c7e`). Never commit to `main`.
- **New tests go in `package queue`, not `package queue_test`.** They touch unexported state (`newManifest`, `p.files`, `derivedRemainingBytes`). `internal/queue` has both kinds; the external ones are `sqlite_store_test.go`, `manifest_externaltest_test.go`, `prune_cleanup_test.go`, `history_progress_test.go`, `nzb_backup_test.go`, `movetohistory_errors_test.go`, `store_contention_test.go`, `challenger_m2_test.go`. Everything else is internal.
- **Store fixtures reachable from `package queue`:** `setupResidencyTestStore(t) (*SQLiteStore, string)` in `residency_remaining_bytes_test.go`, and `makeMultiFileJob(t, name, nFiles, nArticles) *Job` in `lifecycle_test.go`. `queue_test`'s `setupTestStore` is *not* reachable — `residency_remaining_bytes_test.go` documents this explicitly. Do not add a new store fixture.

---

## File Structure

| File | Responsibility in this change |
|---|---|
| `internal/history/migrations/008_add_job_files_failed_bytes.sql` | Adds `job_files.failed_bytes` so failed bytes are reconstructible without a manifest |
| `internal/queue/progress.go` | `FileProgress.Bytes`; `FileProgress.FailedBytes`; the derived `RemainingBytes()`; deletion of the counter and its mutations |
| `internal/queue/persistence.go` | `newJobProgressSized` takes per-file bytes instead of a pre-summed total; becomes the single JobProgress constructor |
| `internal/queue/sqlite_store.go` | `RestoreJobProgress` reads `bytes`; `ArticleCountsByJob` carries bytes; `RemainingBytesByJob` deleted |
| `internal/queue/store.go` | `Store` interface: `ArticleCountsByJob` shape change, `RemainingBytesByJob` removed |
| `internal/queue/job.go` | `ResetForRetry` stops re-adding bytes |
| `internal/queue/queue.go` | `DiscardDeferredPar2` stops adjusting the counter |
| `internal/api/queue.go` | Percentage and size pair with the expectation, not the manifest total |
| `internal/app/history_helper.go` | The downloaded identity uses the expectation |
| `docs/queue-lifecycle.md` | Records that remaining is derived, not maintained |

---

### Task 1: Carry per-file bytes on progress

Adds the field and populates it on every path that builds a `JobProgress`. Nothing reads it yet, so no behaviour changes.

**Files:**
- Modify: `internal/queue/progress.go` (`FileProgress`, `newJobProgress`)
- Modify: `internal/queue/sqlite_store.go` (`RestoreJobProgress` query and scan, `ArticleCountsByJob`)
- Modify: `internal/queue/store.go` (`ArticleCountsByJob` signature)
- Modify: `internal/queue/persistence.go` (`newJobProgressSized`)
- Test: `internal/queue/progress_bytes_test.go` (create)

**Interfaces:**
- Produces: `FileProgress.Bytes int64`; `FileMeta{ArticleCount int; Bytes int64}`; `Store.ArticleCountsByJob(ctx) (map[string][]FileMeta, error)`; `newJobProgressSized(files []FileMeta) *JobProgress`

- [ ] **Step 1: Write the failing test**

Create `internal/queue/progress_bytes_test.go`:

```go
package queue

import "testing"

func TestNewJobProgress_CarriesPerFileBytes(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "payload.rar", Bytes: 5000, Articles: []JobArticle{{ID: "a1", Bytes: 2500}, {ID: "a2", Bytes: 2500}}},
		{Subject: "payload.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "b1", Bytes: 800}}},
	})
	p := newJobProgress(m)

	if got, want := p.files[0].Bytes, int64(5000); got != want {
		t.Errorf("files[0].Bytes = %d, want %d", got, want)
	}
	if got, want := p.files[1].Bytes, int64(800); got != want {
		t.Errorf("files[1].Bytes = %d, want %d", got, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/queue/ -run TestNewJobProgress_CarriesPerFileBytes -v`
Expected: FAIL — `p.files[0].Bytes undefined (type FileProgress has no field or method Bytes)`

- [ ] **Step 3: Add the field**

In `internal/queue/progress.go`, add to `FileProgress`:

```go
type FileProgress struct {
	Complete        bool
	Deferred        bool
	Pending         int
	// Bytes is the file's NZB-claimed size. Carried on progress rather than
	// read from the manifest so RemainingBytes derives from progress alone,
	// at any residency. Written from the manifest when resident and from
	// job_files.bytes when not; the two agree because the column is written
	// from the manifest.
	Bytes           int64
	BytesDownloaded int64
	WriteCursor     int64
	Filename        string // resolved on-disk filename; empty until resolved
	AssembledCRC32  uint32
}
```

- [ ] **Step 4: Populate it in `newJobProgress`**

In `newJobProgress`, inside the loop that sets each file's `Pending`, also set `Bytes`:

```go
for fi := range m.NumFiles() {
	lo, hi := m.FileRange(fi)
	p.files[fi].Pending = hi - lo
	p.files[fi].Bytes = m.FileBytes(fi)
}
```

Match the existing loop's shape in that function rather than adding a second loop.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/queue/ -run TestNewJobProgress_CarriesPerFileBytes -v`
Expected: PASS

- [ ] **Step 6: Write the restore-path failing test**

Append to `internal/queue/progress_bytes_test.go`:

```go
func TestRestoreJobProgress_CarriesPerFileBytes(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	job := makeMultiFileJob(t, "restore-bytes", 3, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	restored := &Job{ID: job.ID, manifest: m, progress: newJobProgress(m)}
	for fi := range restored.progress.files {
		restored.progress.files[fi].Bytes = 0 // prove the restore sets it, not the constructor
	}

	if err := store.RestoreJobProgress(t.Context(), restored); err != nil {
		t.Fatalf("RestoreJobProgress: %v", err)
	}
	for fi := range m.NumFiles() {
		if got, want := restored.progress.files[fi].Bytes, m.FileBytes(fi); got != want {
			t.Errorf("restored files[%d].Bytes = %d, want %d", fi, got, want)
		}
	}
}
```

Asserting against `m.FileBytes(fi)` rather than a literal keeps the test correct whatever sizes the fixture produces.

- [ ] **Step 7: Run test to verify it fails**

Run: `go test ./internal/queue/ -run TestRestoreJobProgress_CarriesPerFileBytes -v`
Expected: FAIL — restored `Bytes` is 0, want 5000

- [ ] **Step 8: Read `bytes` in `RestoreJobProgress`**

In `internal/queue/sqlite_store.go`, `RestoreJobProgress`, add `bytes` to the SELECT and to the scan, then assign it:

```go
const qFiles = `
SELECT file_index, complete, deferred, write_cursor, bytes, bytes_downloaded, assembled_crc32, COALESCE(articles_done, '')
FROM job_files WHERE job_id = ? ORDER BY file_index ASC`
```

In the scan loop add a `var fileBytes int64` and place it between `writeCursor` and `bytesDownloaded` in the `rows.Scan` argument list to match the new column order, then inside the in-range branch:

```go
fp.Bytes = fileBytes
```

- [ ] **Step 9: Run test to verify it passes**

Run: `go test ./internal/queue/ -run TestRestoreJobProgress_CarriesPerFileBytes -v`
Expected: PASS

- [ ] **Step 10: Carry bytes through the non-resident sizing path**

`ArticleCountsByJob` currently returns `map[string][]int`. It must carry bytes too, so `Load` can build progress with real per-file sizes.

In `internal/queue/store.go`, add the type above the `Store` interface and change the method:

```go
// FileMeta is the per-file shape Load needs to rebuild a non-resident job's
// progress without a manifest. It carries everything RemainingBytes reads,
// so a job reports the same figure resident or not: the article count sizes
// the bitsets, and the rest reconstructs the per-file state the derivation
// consults.
type FileMeta struct {
	ArticleCount    int
	Bytes           int64
	BytesDownloaded int64
	Complete        bool
	Deferred        bool
}
```

Carrying only `ArticleCount` and `Bytes` is not enough. `RemainingBytesByJob`,
which this replaces, computed `SUM(bytes - bytes_downloaded)`; dropping the
subtrahend makes a job paused mid-download report as having downloaded nothing
after a restart. `Complete` and `Deferred` are needed for the same reason once
the derivation consults them.

```go
	ArticleCountsByJob(ctx context.Context) (map[string][]FileMeta, error)
```

Delete the `RemainingBytesByJob` method from the interface and its doc comment.

In `internal/queue/sqlite_store.go`, change the query and the accumulation:

```go
const q = `SELECT job_id, file_index, article_count, bytes, bytes_downloaded, complete, deferred FROM job_files ORDER BY job_id ASC, file_index ASC`
```

Scan the new columns alongside `article_count` and append a fully-populated `FileMeta` where it previously appended `count`. `complete` and `deferred` are stored as INTEGER, so scan them into `int` and convert with `!= 0`. Keep the existing non-contiguous-index handling exactly as it is — only the element type changes.

Delete `RemainingBytesByJob` entirely.

- [ ] **Step 11: Update `newJobProgressSized` and `Load`**

In `internal/queue/persistence.go`:

```go
// newJobProgressSized builds a JobProgress sized to files (one element per
// file) without requiring a resident Manifest — see Store.ArticleCountsByJob.
// Used by Load to give a non-resident job (StatusQueued/StatusPaused at
// restart) a real JobProgress instead of leaving it nil.
//
// Every article starts undone/unfailed/unemitted: this sizes progress for
// reporting, it does not restore true per-article state, which needs the
// manifest. That restoration already happens whenever the job is promoted
// back to resident.
//
// Per-file Bytes is carried so RemainingBytes derives correctly for a job
// that restarted non-resident. It replaces the pre-summed figure this used
// to take from Store.RemainingBytesByJob.
func newJobProgressSized(files []FileMeta) *JobProgress {
	total := 0
	for _, f := range files {
		total += f.ArticleCount
	}
	p := &JobProgress{
		done:            newBitset(total),
		failed:          newBitset(total),
		emitted:         newBitset(total),
		files:           make([]FileProgress, len(files)),
		pendingArticles: total,
	}
	for fi, f := range files {
		p.files[fi].Pending = f.ArticleCount
		p.files[fi].Bytes = f.Bytes
		p.files[fi].BytesDownloaded = f.BytesDownloaded
		p.files[fi].Complete = f.Complete
		p.files[fi].Deferred = f.Deferred
	}
	return p
}
```

In `Load`, drop the `remainingByJob` lookup and its `RemainingBytesByJob` call, and change the seeding line to:

```go
job.progress = newJobProgressSized(countsByJob[job.ID])
```

Note the `remainingBytes` field is still set elsewhere at this point — it is removed in Task 5.

- [ ] **Step 12: Run the package suite**

Run: `go build ./... && go test ./internal/queue/ 2>&1 | tail -20`
Expected: PASS. Any failure here is a fixture that constructed `FileMeta`'s predecessor or called `RemainingBytesByJob`; update it rather than reintroducing the method.

- [ ] **Step 13: Commit**

```bash
goimports -w internal/queue/ && go fix ./... && go vet ./... && golangci-lint run ./internal/queue/...
git add internal/queue/
git commit -m "$(cat <<'EOF'
refactor(queue): carry per-file bytes on JobProgress

RemainingBytes is about to be derived from progress rather than
maintained as a counter, which needs each file's size available at any
residency. FileProgress gains Bytes, written from the manifest when
resident and from the job_files.bytes column when not.

ArticleCountsByJob now returns per-file article counts and bytes together
so Load can size a non-resident job's progress with real sizes, which
makes Store.RemainingBytesByJob redundant. It is deleted.

Nothing reads the new field yet; no reported figure changes.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Carry per-file failed bytes

The maintained counter means *unresolved* bytes: `markFailed` decrements `remainingBytes` and adds to the job-level `failedBytes`, but never touches `files[fi].BytesDownloaded` — correctly, since a permanently failed article was never downloaded. A derivation of the form `Bytes - BytesDownloaded` therefore over-reports remaining by exactly the failed bytes, and `internal/app/history_helper.go:54` computes `downloaded := totalBytes - FailedBytes() - RemainingBytes()`, an identity that only closes if remaining already excludes them.

Failed bytes are recomputable per file from the article bitmaps when a manifest is resident, but not otherwise. This task adds the per-file figure and a column to persist it, so the derivation in Task 3 works at either residency. That also closes a live pre-existing drift: `newJobProgressSized` seeds a restarted non-resident job with `sum(Bytes - BytesDownloaded)` and leaves `failedBytes` at 0, so today the same job reports different `RemainingBytes()` and `FailedBytes()` depending on residency until it is promoted. The deleted `RemainingBytesByJob` had the identical gap (`SUM(bytes - bytes_downloaded)`), so this is not a regression from Task 1.

**Files:**
- Create: `internal/history/migrations/008_add_job_files_failed_bytes.sql`
- Modify: `internal/queue/progress.go` (`FileProgress.FailedBytes`, `markFailed`, `resetForReload`, `recompute`)
- Modify: `internal/queue/store.go` (`FileMeta.FailedBytes`)
- Modify: `internal/queue/sqlite_store.go` (`insertJobFilesTx`, `updateTx`, `RestoreJobProgress`, `ArticleCountsByJob`)
- Modify: `internal/queue/persistence.go` (`newJobProgressSized` seeds per-file and job-level failed bytes)
- Test: `internal/queue/progress_bytes_test.go`

**Interfaces:**
- Consumes: `FileProgress.Bytes` and `FileMeta` from Task 1
- Produces: `FileProgress.FailedBytes int64`, `FileMeta.FailedBytes int64`. Task 3's derivation subtracts `FailedBytes` per file.

- [ ] **Step 1: Write the failing test**

Append to `internal/queue/progress_bytes_test.go` (`package queue`):

```go
func TestMarkFailed_AccumulatesPerFileFailedBytes(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 3000, Articles: []JobArticle{{ID: "a1", Bytes: 1500}, {ID: "a2", Bytes: 1500}}},
		{Subject: "b.rar", Bytes: 2000, Articles: []JobArticle{{ID: "b1", Bytes: 2000}}},
	})
	p := newJobProgress(m)

	p.markDone(m, 0)
	p.markFailed(m, 1)
	p.markFailed(m, 2)

	if got, want := p.files[0].FailedBytes, int64(1500); got != want {
		t.Errorf("file 0 FailedBytes = %d, want %d", got, want)
	}
	if got, want := p.files[1].FailedBytes, int64(2000); got != want {
		t.Errorf("file 1 FailedBytes = %d, want %d", got, want)
	}
	if got, want := p.files[0].BytesDownloaded, int64(1500); got != want {
		t.Errorf("failed bytes leaked into BytesDownloaded: got %d, want %d", got, want)
	}
	// Per-file failed bytes must sum to the job-level counter, or the two
	// disagree the moment Task 3 derives remaining from the per-file side.
	if got, want := p.files[0].FailedBytes+p.files[1].FailedBytes, p.failedBytes; got != want {
		t.Errorf("per-file sum = %d, job-level failedBytes = %d", got, want)
	}
}

func TestResetForReload_ReturnsFailedBytesToTheFile(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 3000, Articles: []JobArticle{{ID: "a1", Bytes: 3000}}},
	})
	p := newJobProgress(m)

	p.markFailed(m, 0)
	if got, want := p.files[0].FailedBytes, int64(3000); got != want {
		t.Fatalf("FailedBytes after markFailed = %d, want %d", got, want)
	}

	p.resetForReload(m, 0)
	if got, want := p.files[0].FailedBytes, int64(0); got != want {
		t.Errorf("FailedBytes after resetForReload = %d, want %d", got, want)
	}
	if got, want := p.failedBytes, int64(0); got != want {
		t.Errorf("job-level failedBytes after resetForReload = %d, want %d", got, want)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/queue/ -run 'TestMarkFailed_AccumulatesPerFileFailedBytes|TestResetForReload_ReturnsFailedBytesToTheFile' -v`
Expected: FAIL to compile — `p.files[0].FailedBytes undefined (type FileProgress has no field or method FailedBytes)`.

- [ ] **Step 3: Add the field**

In `internal/queue/progress.go`, in `FileProgress`, directly after `BytesDownloaded`:

```go
	// FailedBytes is the sum of bytes belonging to this file's permanently
	// failed articles. Carried per file, not just job-wide, so remaining
	// derives from progress alone at any residency: a failed article was
	// never downloaded, so BytesDownloaded does not account for it, and
	// without this the derivation would report its bytes as still to fetch
	// forever. Recomputable from the article bitmaps when a manifest is
	// resident; persisted in job_files.failed_bytes for when it is not.
	FailedBytes int64
```

- [ ] **Step 4: Maintain it in `markFailed`**

In `markFailed`, alongside the existing `p.failedBytes += bytes`:

```go
	p.failedBytes += bytes
	p.files[fi].FailedBytes += bytes
```

- [ ] **Step 5: Unwind it in `resetForReload`**

In `resetForReload`, inside the `if p.failed.Get(i)` block, alongside `p.failedBytes -= bytes`:

```go
		p.failedBytes -= bytes
		p.files[fi].FailedBytes -= bytes
```

`resetForReload` does not currently compute `fi`. Add `fi := m.fileIndexForArticle(i)` inside the `if` block, next to the existing `bytes := int64(m.ArticleBytes(i))`.

- [ ] **Step 6: Rebuild it in `recompute`**

`recompute` rebuilds per-file counters from the bitmaps and is the authority after a restart that restores `articles_done`. It already accumulates `downloaded`; add `failedBytes` beside it.

In the per-file loop, declare it next to `downloaded`:

```go
		var downloaded, fileFailed int64
```

In the article loop, extend the existing failed branch:

```go
			if p.done.Get(i) {
				resolved++
				if p.failed.Get(i) {
					failed++
					fileFailed += int64(m.ArticleBytes(i))
				}
			}
```

And after the loop, next to the existing assignment:

```go
		p.files[fi].BytesDownloaded = downloaded
		p.files[fi].FailedBytes = fileFailed
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/queue/ -run 'TestMarkFailed_AccumulatesPerFileFailedBytes|TestResetForReload_ReturnsFailedBytesToTheFile' -v`
Expected: PASS

- [ ] **Step 8: Commit the in-memory half**

```bash
goimports -w internal/queue/ && go build ./... && go vet ./... && go test -race -count=1 ./internal/queue/...
git add internal/queue/progress.go internal/queue/progress_bytes_test.go
git commit -m "$(cat <<'EOF'
feat(queue): track failed bytes per file, not only per job

markFailed decremented the job-wide remainingBytes and added to the
job-wide failedBytes, but left no per-file trace. A failed article is
resolved without ever being downloaded, so BytesDownloaded does not
account for it and a per-file derivation of remaining would report its
bytes as outstanding forever.

Carries the figure on FileProgress, maintained by markFailed and
resetForReload and rebuilt by recompute from the article bitmaps.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

- [ ] **Step 9: Add the migration**

Create `internal/history/migrations/008_add_job_files_failed_bytes.sql`. Never edit an existing migration file.

```sql
-- +goose Up
-- +goose StatementBegin
ALTER TABLE job_files ADD COLUMN failed_bytes INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE job_files DROP COLUMN failed_bytes;
-- +goose StatementEnd
```

- [ ] **Step 10: Write the column on both paths**

In `internal/queue/sqlite_store.go`, `insertJobFilesTx`: add `failed_bytes` to the INSERT's column list and a placeholder to its `VALUES`, then pass `p.FileFailedBytes(i)` in the same position. Add the accessor to `progress.go` beside `FileBytesDownloaded`, guarding the index the same way it does:

```go
// FileFailedBytes returns the sum of bytes belonging to permanently failed
// articles in file fileIdx.
func (p *JobProgress) FileFailedBytes(fi int) int64 {
	if fi < 0 || fi >= len(p.files) {
		return 0
	}
	return p.files[fi].FailedBytes
}
```

Match the exact bounds-guard and doc-comment shape of the existing `FileBytesDownloaded` — read it first rather than copying the block above verbatim.

In `updateTx`, extend `qF` to `SET ..., failed_bytes = ?` and pass `job.Progress().FileFailedBytes(i)` in the matching argument position. Keep the argument order aligned with the column order; a mismatch here writes one column's value into another and no test will name the swap.

- [ ] **Step 11: Read the column on both paths**

In `RestoreJobProgress`, add `failed_bytes` to the `SELECT` list, scan it into a new `failedBytes int64`, and assign `fp.FailedBytes = failedBytes`. Place it adjacent to `bytes_downloaded` in both the column list and the scan targets so the two stay visually paired.

In `ArticleCountsByJob`, add `failed_bytes` to the `SELECT`, scan it, and set it on the constructed `FileMeta`. Add the field to `FileMeta` in `internal/queue/store.go`:

```go
	FailedBytes int64
```

Update `FileMeta`'s doc comment and the `Store.ArticleCountsByJob` interface comment to name the new field, the same way Task 1 did for `BytesDownloaded`/`Complete`/`Deferred`.

- [ ] **Step 12: Close the residency drift in `newJobProgressSized`**

`newJobProgressSized` currently leaves `p.failedBytes` at 0 and omits failed bytes from its `remainingBytes` seed, so a restarted non-resident job disagrees with the same job resident. Carry the per-file figure through and sum it into the job-level counter.

In the accumulation loop, add the sum:

```go
	total := 0
	var remainingBytes, failedBytes int64
	for _, f := range files {
		total += f.ArticleCount
		failedBytes += f.FailedBytes
		if f.Complete || f.Deferred {
			continue
		}
		if left := f.Bytes - f.BytesDownloaded - f.FailedBytes; left > 0 {
			remainingBytes += left
		}
	}
```

Set `failedBytes: failedBytes` in the `&JobProgress{...}` literal, and `p.files[fi].FailedBytes = f.FailedBytes` in the per-file loop.

Then update the function's doc comment: the paragraph explaining that the seed matches what `derivedRemainingBytes` computes must now say it subtracts failed bytes too, and the paragraph claiming articles start "undone/unfailed/unemitted" must note that while the per-article bitmaps do start clear, the per-file and job-level failed *byte* totals are restored from `job_files.failed_bytes`, because reporting depends on them before the job is ever promoted.

- [ ] **Step 13: Write the residency-equivalence test for failed bytes**

```go
func TestFailedBytes_SurvivesRestartNonResident(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	job := makeMultiFileJob(t, "failed-bytes-residency", 2, 2)
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	job.Progress().markFailed(m, 0)
	job.Progress().markDone(m, 1)
	wantFailed := job.Progress().FailedBytes()
	wantRemaining := job.Progress().RemainingBytes()
	if wantFailed == 0 {
		t.Fatal("fixture produced no failed bytes; the test would pass vacuously")
	}
	if err := store.Add(context.Background(), job); err != nil {
		t.Fatalf("add: %v", err)
	}

	reloaded, err := Load(dir, WithStore(store))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := reloaded.byID[job.ID].Progress()
	if got.FailedBytes() != wantFailed {
		t.Errorf("FailedBytes across restart: got %d, want %d", got.FailedBytes(), wantFailed)
	}
	if got.RemainingBytes() != wantRemaining {
		t.Errorf("RemainingBytes across restart: got %d, want %d", got.RemainingBytes(), wantRemaining)
	}
}
```

`setupResidencyTestStore` and `makeMultiFileJob` are the existing fixtures named in Global Constraints — do not add a new one. If this test needs the job to be non-resident on reload, follow the pattern `residency_remaining_bytes_test.go` already uses to force that; read it before writing, and adapt the status handling to match. If the fixture's status leaves the job resident, the test proves nothing — say so in your report rather than leaving it silently vacuous.

- [ ] **Step 14: Run the full package and commit**

```bash
goimports -w internal/queue/ && go build ./... && go vet ./... && go test -race -count=1 ./internal/queue/... && golangci-lint run ./internal/queue/...
git add internal/history/migrations/008_add_job_files_failed_bytes.sql internal/queue/
git commit -m "$(cat <<'EOF'
feat(queue): persist per-file failed bytes and fix the residency drift

Adds job_files.failed_bytes so a job's failed byte total is
reconstructible without a resident manifest, and carries it through
insertJobFilesTx, updateTx, RestoreJobProgress, and ArticleCountsByJob.

newJobProgressSized previously seeded a restarted non-resident job with
sum(bytes - bytes_downloaded) and left failedBytes at zero, so the same
job reported different RemainingBytes and FailedBytes depending on
whether it had been promoted yet. The deleted RemainingBytesByJob had
the same gap, so this is a pre-existing defect rather than a regression.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: Give the job-level byte aggregates a single owner

Task 2's review found a Critical defect that this task fixes, and removes the mechanism that produced it.

`newJobProgressSized` seeds job-level `failedBytes`. Every hydration path then calls `Store.RestoreJobProgress`, which replays per-article state through `decodeArticlesDone` → `markFailed` (`sqlite_store.go:180`), and `markFailed` does `p.failedBytes += bytes`. The replay runs on top of the seeded value rather than a fresh one, so the two stack: a job with 100,000 failed bytes reports 200,000 after `Load` → `SnapshotJob`. `recompute` runs at the end of `RestoreJobProgress` and repairs the *per-file* `FailedBytes`, but never the job-level total.

`queue.go:1131` already states the contract the seeding broke: "newJobProgress built an all-zero JobProgress that RestoreJobProgress [fills]". The fix is to give the aggregate an owner rather than two writers, and to stop the two constructors from being able to diverge at all.

Two changes, in this order:

1. **`recompute` owns job-level `failedBytes`.** It already owns `pendingArticles`, `articlesResolved`, and `articlesFailed`, and it already computes the per-file failed total. Assigning the job-level field from the same loop makes it authoritative wherever a manifest exists, and makes seed-plus-replay stacking impossible on every path at once.

2. **One constructor.** `newJobProgress(m)` projects the manifest into `[]FileMeta` and delegates to `newJobProgressSized`, so resident and non-resident construction is literally the same code.

**`remainingBytes` is deliberately NOT given to `recompute`.** It has the same double-subtraction, but it is pre-existing (the seed has always been re-subtracted by the replay), and recomputing it from per-file `Bytes` rather than from article bytes could shift the reported figure for a resident job whose file size differs from the sum of its article sizes — which would violate this plan's "no reported figure may change" constraint in order to fix a bug that predates it. Task 5 deletes the field, and the derived replacement reads per-file state `recompute` already owns. Do not add it here.

**Files:**
- Modify: `internal/queue/progress.go` (`recompute`, `newJobProgress`, new `fileMetaFromManifest`)
- Test: `internal/queue/progress_bytes_test.go`

**Interfaces:**
- Consumes: `FileProgress.FailedBytes` and `FileMeta.FailedBytes` from Task 2
- Produces: `fileMetaFromManifest(m *Manifest) []FileMeta`. `newJobProgress` keeps its signature `func newJobProgress(m *Manifest) *JobProgress` — callers are unaffected.

- [ ] **Step 1: Write the failing regression test**

This is the defect Task 2 shipped. It must fail before the fix.

```go
func TestFailedBytes_NotDoubledByHydration(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	job := makeMultiFileJob(t, "failed-bytes-hydrate", 2, 2)
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	job.Progress().markFailed(m, 0)
	job.Progress().markDone(m, 1)
	want := job.Progress().FailedBytes()
	if want == 0 {
		t.Fatal("fixture produced no failed bytes; the test would pass vacuously")
	}
	if err := store.Add(context.Background(), job); err != nil {
		t.Fatalf("add: %v", err)
	}

	reloaded, err := Load(dir, WithStore(store))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rj := reloaded.byID[job.ID]
	// Seeded from job_files while non-resident.
	if got := rj.Progress().FailedBytes(); got != want {
		t.Fatalf("non-resident FailedBytes = %d, want %d", got, want)
	}
	// Hydrating replays per-article state on top of that seed. The total
	// must not move: it is the same job, only more of it is in memory.
	if err := store.RestoreJobProgress(context.Background(), rj); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := rj.Progress().FailedBytes(); got != want {
		t.Errorf("FailedBytes after hydration = %d, want %d (seed and replay stacked)", got, want)
	}
	// The per-file values must still sum to the job-level total.
	var sum int64
	for fi := range rj.Progress().files {
		sum += rj.Progress().files[fi].FailedBytes
	}
	if sum != want {
		t.Errorf("per-file FailedBytes sum = %d, job-level = %d", sum, want)
	}
}
```

`RestoreJobProgress` needs `job.manifest != nil` and `job.progress != nil` or it returns early without doing anything — which would make this test vacuous. Verify the reloaded job satisfies both before the call; if it does not, hydrate it the way `residency_remaining_bytes_test.go` does and say in your report which route you used.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/queue/ -run TestFailedBytes_NotDoubledByHydration -v`
Expected: FAIL at "FailedBytes after hydration", reporting exactly double the wanted value.

If it instead fails at the *non-resident* assertion, or passes outright, stop and report — the reproduction does not match the diagnosis and the rest of this task is built on that diagnosis.

- [ ] **Step 3: Give `recompute` the job-level total**

In `internal/queue/progress.go`, `recompute` already accumulates `fileFailed` per file. Accumulate a job-wide total alongside the existing `resolved`/`failed` counters and assign it after the loop, next to the existing assignments:

```go
	p.pendingArticles = total
	p.articlesResolved = resolved
	p.articlesFailed = failed
	p.failedBytes = failedBytesTotal
```

Declare `failedBytesTotal` beside `resolved`/`failed` at the top of the function and add `failedBytesTotal += fileFailed` immediately after the existing `p.files[fi].FailedBytes = fileFailed`.

Extend `recompute`'s doc comment to record that it is authoritative for the job-level `failedBytes` wherever a manifest exists, and that incremental maintenance by `markFailed`/`resetForReload` is what carries the value between recomputes. State the reason: `RestoreJobProgress` replays articles on top of an already-seeded progress, so without a single owner the seed and the replay stack.

- [ ] **Step 4: Run the regression test to verify it passes**

Run: `go test ./internal/queue/ -run TestFailedBytes_NotDoubledByHydration -v`
Expected: PASS

- [ ] **Step 5: Run the whole package**

Run: `go test -race -count=1 ./internal/queue/...`
Expected: PASS. If a test that previously passed now fails, it was depending on the doubled value — report which, with its assertion, before changing it. Do not adjust an existing assertion to match new behaviour without saying so explicitly in your report.

- [ ] **Step 6: Commit**

```bash
goimports -w internal/queue/ && go build ./... && go vet ./... && golangci-lint run ./internal/queue/...
git add internal/queue/progress.go internal/queue/progress_bytes_test.go
git commit -m "$(cat <<'EOF'
fix(queue): give job-level failedBytes a single owner

newJobProgressSized seeds failedBytes for a non-resident job, and
RestoreJobProgress then replays per-article state through markFailed on
top of that seed rather than onto a fresh JobProgress, so the two
stacked: a job with permanently failed articles reported double its
failed bytes once hydrated.

recompute already owns pendingArticles, articlesResolved and
articlesFailed, and already derives the per-file failed total from the
article bitmaps. Making it own the job-level total too closes the
double-count on every hydration path at once.

remainingBytes has the same shape of defect but is left alone: it
predates this work, and recomputing it from per-file bytes could move a
reported figure. It is deleted outright two commits from now.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

- [ ] **Step 7: Write the constructor-equivalence test**

Pins that projecting a manifest through `FileMeta` loses nothing, so the two constructors cannot diverge.

```go
func TestNewJobProgress_MatchesSizedConstruction(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 3000, Articles: []JobArticle{{ID: "a1", Bytes: 1500}, {ID: "a2", Bytes: 1500}}},
		{Subject: "b.rar", Bytes: 2000, Articles: []JobArticle{{ID: "b1", Bytes: 2000}}},
		{Subject: "c.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 800}}},
	})
	p := newJobProgress(m)

	if got, want := p.done.Len(), m.NumArticles(); got != want {
		t.Errorf("done bitset sized %d, want %d", got, want)
	}
	if got, want := len(p.files), m.NumFiles(); got != want {
		t.Errorf("files sized %d, want %d", got, want)
	}
	if got, want := p.remainingBytes, m.TotalBytes(); got != want {
		t.Errorf("remainingBytes = %d, want m.TotalBytes() = %d", got, want)
	}
	for fi := range p.files {
		lo, hi := m.FileRange(fi)
		if got, want := p.files[fi].Pending, hi-lo; got != want {
			t.Errorf("file %d Pending = %d, want %d", fi, got, want)
		}
		if got, want := p.files[fi].Bytes, m.FileBytes(fi); got != want {
			t.Errorf("file %d Bytes = %d, want %d", fi, got, want)
		}
	}
}
```

- [ ] **Step 8: Run it — it must pass against the CURRENT `newJobProgress`**

Run: `go test ./internal/queue/ -run TestNewJobProgress_MatchesSizedConstruction -v`
Expected: PASS, before you touch `newJobProgress`. This test characterises existing behaviour so the refactor in the next step is provably behaviour-preserving. If it fails now, stop and report — the properties do not hold and the delegation is not safe.

- [ ] **Step 9: Delegate to the one constructor**

Add, next to `newJobProgress`:

```go
// fileMetaFromManifest projects m into the same per-file shape
// Store.ArticleCountsByJob returns, so newJobProgress and
// newJobProgressSized are one code path rather than two that must be kept
// in agreement by hand. A fresh job has nothing downloaded, nothing
// failed, and no file complete or deferred, so every field but the sizes
// is zero.
//
// The projection is lossless for what JobProgress needs: Manifest.TotalBytes
// is the sum of every file's bytes, and Manifest.NumArticles is the sum of
// every file's article count, so the totals newJobProgressSized derives
// match the ones newJobProgress used to take from the manifest directly.
func fileMetaFromManifest(m *Manifest) []FileMeta {
	files := make([]FileMeta, m.NumFiles())
	for fi := range files {
		lo, hi := m.FileRange(fi)
		files[fi] = FileMeta{ArticleCount: hi - lo, Bytes: m.FileBytes(fi)}
	}
	return files
}
```

Then replace `newJobProgress`'s body with:

```go
func newJobProgress(m *Manifest) *JobProgress {
	return newJobProgressSized(fileMetaFromManifest(m))
}
```

Keep its doc comment accurate: it no longer builds the struct itself, and it now sets `pendingArticles` — see the next step.

- [ ] **Step 10: Check the one behavioural delta, and report it**

The old `newJobProgress` left `pendingArticles` at 0 while setting each file's `Pending`; `newJobProgressSized` sets `pendingArticles` to the article total. After delegation, `newJobProgress` sets it too.

That looks like a latent inconsistency being corrected rather than a regression, but **verify it rather than assuming**: `PendingArticles()` is reachable from reporting code. Establish whether any path reads it between `newJobProgress` and the first `recompute` — `recompute` sets it from ground truth, so the delta only matters in that window. Report what you found. If any reported figure changes for a caller, stop and report rather than proceeding: this plan forbids that.

- [ ] **Step 11: Run everything**

```bash
go build ./... && go vet ./... && go test -race -count=1 ./... && golangci-lint run ./...
```
Expected: all PASS. Run the full repo, not just `internal/queue` — `newJobProgress` is reachable from other packages.

- [ ] **Step 12: Commit**

```bash
git add internal/queue/progress.go internal/queue/progress_bytes_test.go
git commit -m "$(cat <<'EOF'
refactor(queue): build JobProgress through one constructor

newJobProgress and newJobProgressSized built the same struct from two
sources and had to be kept in agreement by hand. Three defects in this
refactor came from that pair drifting: a dropped bytes_downloaded term,
a derivation blind to failed bytes, and a seed that double-counted
against the replay layered on top of it.

newJobProgress now projects the manifest into the same []FileMeta the
store returns and delegates. The projection is lossless: TotalBytes is
the sum of per-file bytes and NumArticles the sum of per-file article
counts, both pinned by a characterisation test written against the old
constructor before the change.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: Prove a derived value agrees with the maintained one

Adds the derivation next to the existing counter and pins that they agree. This is the safety net for Task 5's deletion: if the derivation is wrong, this fails before the counter is gone.

**Files:**
- Modify: `internal/queue/progress.go` (add `derivedRemainingBytes`)
- Test: `internal/queue/progress_bytes_test.go`

**Interfaces:**
- Consumes: `FileProgress.Bytes` from Task 1; `FileProgress.FailedBytes` from Task 2, single-owner aggregates from Task 3
- Produces: `func (p *JobProgress) derivedRemainingBytes() int64`

- [ ] **Step 1: Write the failing test**

Append to `internal/queue/progress_bytes_test.go`:

```go
func TestDerivedRemaining_AgreesWithMaintainedCounter(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 3000, Articles: []JobArticle{{ID: "a1", Bytes: 1500}, {ID: "a2", Bytes: 1500}}},
		{Subject: "b.rar", Bytes: 2000, Articles: []JobArticle{{ID: "b1", Bytes: 2000}}},
	})
	p := newJobProgress(m)

	check := func(stage string) {
		t.Helper()
		if got, want := p.derivedRemainingBytes(), p.remainingBytes; got != want {
			t.Errorf("%s: derived = %d, maintained = %d", stage, got, want)
		}
	}

	check("fresh")

	p.markDone(m, 0)
	check("one article done")

	p.markDone(m, 1)
	p.files[0].Complete = true
	check("first file complete")

	p.markFailed(m, 2)
	check("second file failed")
}
```

`markDone` and `markFailed` are `func (p *JobProgress) markDone(m *Manifest, i int) bool`; `i` is the flat global article index, not a per-file one.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/queue/ -run TestDerivedRemaining_AgreesWithMaintainedCounter -v`
Expected: FAIL — `p.derivedRemainingBytes undefined`

- [ ] **Step 3: Add the derivation**

In `internal/queue/progress.go`, next to `RemainingBytes`:

```go
// derivedRemainingBytes computes what is still to fetch from per-file state
// rather than from a maintained counter: every file that is neither complete
// nor deferred contributes the part of it neither downloaded nor permanently
// failed.
//
// Failed bytes are subtracted because the counter this replaces means
// unresolved bytes, not un-downloaded ones: markFailed decrements it without
// ever adding to BytesDownloaded. internal/app/history_helper.go computes
// downloaded as totalBytes - FailedBytes() - RemainingBytes(), an identity
// that only closes under that meaning.
//
// Deferred files contribute nothing because their articles are never
// dispatched, so a deferral or an un-deferral needs no adjustment anywhere —
// the next read reflects it. That is the whole point of deriving rather than
// maintaining.
//
// O(files), and files number in the hundreds where articles number in the
// tens of thousands. Called on reporting reads, not on the download path.
func (p *JobProgress) derivedRemainingBytes() int64 {
	var remaining int64
	for fi := range p.files {
		f := &p.files[fi]
		if f.Complete || f.Deferred {
			continue
		}
		if left := f.Bytes - f.BytesDownloaded - f.FailedBytes; left > 0 {
			remaining += left
		}
	}
	return remaining
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/queue/ -run TestDerivedRemaining_AgreesWithMaintainedCounter -v`
Expected: PASS

If it fails at the "first file complete" stage, the cause is the maintained counter tracking *article* bytes while the derivation tracks *file* bytes, which differ when an NZB's file size does not equal the sum of its article sizes. Record the discrepancy and stop — that is a real finding about which figure is correct, not something to paper over by loosening the assertion.

- [ ] **Step 5: Write the deferred-file test**

```go
func TestDerivedRemaining_ExcludesDeferredFiles(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 3000, Articles: []JobArticle{{ID: "a1", Bytes: 3000}}},
		{Subject: "a.vol000+01.par2", Bytes: 800, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 800}}},
	})
	p := newJobProgress(m)
	p.files[1].Deferred = true

	if got, want := p.derivedRemainingBytes(), int64(3000); got != want {
		t.Errorf("deferred volume counted: got %d, want %d", got, want)
	}

	p.files[1].Deferred = false
	if got, want := p.derivedRemainingBytes(), int64(3800); got != want {
		t.Errorf("un-deferred volume not counted: got %d, want %d", got, want)
	}
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/queue/ -run TestDerivedRemaining_ExcludesDeferredFiles -v`
Expected: PASS. It exercises no new production code — it pins that un-deferring needs no fixup call, which is the property Task 5 relies on.

- [ ] **Step 7: Commit**

```bash
goimports -w internal/queue/ && go vet ./... && go test -race ./internal/queue/
git add internal/queue/
git commit -m "$(cat <<'EOF'
test(queue): pin derived remaining against the maintained counter

Adds derivedRemainingBytes alongside JobProgress.remainingBytes and
asserts the two agree across a download, so the counter's removal in the
next commit is guarded by a test rather than by inspection.

Also pins that a deferred file contributes nothing and an un-deferred one
contributes again, with no adjustment call in between.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: Delete the maintained counter

**Files:**
- Modify: `internal/queue/progress.go` (field, decrements, `resetForReload`, `RemainingBytes`, JSON shape)
- Modify: `internal/queue/persistence.go` (`newJobProgressSized` stops seeding the counter)
- Modify: `internal/queue/job.go` (`ResetForRetry`)
- Modify: `internal/queue/queue.go` (`DiscardDeferredPar2`)

**Interfaces:**
- Consumes: `derivedRemainingBytes` from Task 4
- Produces: `RemainingBytes()` unchanged in signature, now derived

- [ ] **Step 1: Point the accessor at the derivation**

In `internal/queue/progress.go`, change `RemainingBytes` to return `p.derivedRemainingBytes()`, and update its doc comment to say the figure is computed from per-file state rather than maintained.

- [ ] **Step 2: Run the package suite**

Run: `go test ./internal/queue/ 2>&1 | tail -20`
Expected: PASS, with the counter still present and still maintained. A failure here means a caller depends on the maintained value differing from the derived one — investigate before continuing.

- [ ] **Step 3: Delete the field and every mutation**

Remove, in `internal/queue/progress.go`:
- the `remainingBytes int64` field from `JobProgress`
- `p.remainingBytes -= bytes` in `markDone`
- `p.remainingBytes -= bytes` in `markFailed`
- `p.remainingBytes += bytes` in `resetForReload`
- `RemainingBytes: p.remainingBytes` from `MarshalJSON` and the `RemainingBytes` field from `jobProgressJSON`
- `p.remainingBytes = pj.RemainingBytes` from `UnmarshalJSON`

`newJobProgress` no longer has a seed of its own to delete — Task 3 made it
delegate to `newJobProgressSized`. The seed now lives in one place, and
removing it is what finally collapses the duplicated formula: until this
step, `newJobProgressSized` open-codes the same expression
`derivedRemainingBytes` computes, and the two agreeing has been maintained
by hand.

Remove, in `internal/queue/persistence.go`, `newJobProgressSized`:
- `remainingBytes` from the `var remainingBytes, failedBytes int64` declaration
- the `if left := f.Bytes - f.BytesDownloaded - f.FailedBytes; left > 0 { remainingBytes += left }` block
- `remainingBytes:  remainingBytes,` from the `&JobProgress{...}` literal

Keep the `Complete || Deferred` skip only if `failedBytes` still needs it —
read the loop and decide; `failedBytes` must accumulate over **every** file,
including complete and deferred ones, so the `continue` must not skip it.
Getting this wrong silently under-reports failed bytes for any job with a
complete or deferred file, and no existing test covers that combination.

Then rewrite the function's doc comment. Several paragraphs describe seeding
`remainingBytes` and why the seed matches the derivation; those become false.
What stays true and must be said: the per-file figures are carried so
`RemainingBytes` derives correctly at any residency, and `failedBytes` is
summed here because reporting depends on it before promotion.

Also fix `newJobProgress`'s doc comment, which still says `RemainingBytes`
starts at `m.TotalBytes()`.

Remove, in `internal/queue/job.go`, `ResetForRetry`:
- `j.progress.remainingBytes += int64(m.ArticleBytes(i))`

Remove, in `internal/queue/queue.go`, `DiscardDeferredPar2`:

```go
newProgress.remainingBytes -= discardedBytes
if newProgress.remainingBytes < 0 {
	newProgress.remainingBytes = 0
}
```

Keep `discardedBytes` if it is still used for anything else in that function; delete it if this was its only consumer.

- [ ] **Step 4: Run build and the full suite**

Run: `go build ./... && go test ./... 2>&1 | tail -20`
Expected: PASS. Compile errors point at remaining references; delete them, do not reintroduce the field.

- [ ] **Step 5: Update the derivation's own test**

`TestDerivedRemaining_AgreesWithMaintainedCounter` compares against a field that no longer exists. Rewrite it to assert `RemainingBytes()` against explicit expected values at each stage, keeping the same stages. Do not delete the test — the stages are the coverage.

- [ ] **Step 6: Run the full suite with the race detector**

Run: `go test -race ./... 2>&1 | tail -20`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
goimports -w internal/queue/ && go fix ./... && go vet ./... && golangci-lint run ./...
git add internal/queue/
git commit -m "$(cat <<'EOF'
refactor(queue)!: derive remaining bytes instead of maintaining them

RemainingBytes now sums the undownloaded part of every file that is
neither complete nor deferred, computed on read from per-file progress.

Deletes JobProgress.remainingBytes, its seed from the manifest total, its
per-article decrements in markDone and markFailed, the re-add in
resetForReload and ResetForRetry, and the downward fixup in
DiscardDeferredPar2. Every bug in this family — #294, #310, #317 — comes
from maintained state drifting from what it describes; there is no longer
a figure to keep in step.

Deferring or un-deferring a file needs no adjustment: the next read
reflects it.

BREAKING CHANGE: JobProgress's JSON shape drops remaining_bytes. That
path has had no production caller since #298 and there is no
compatibility requirement.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: Pair remaining with an expectation that shares its exclusion set

Task 5 made `RemainingBytes()` exclude `Deferred` files. The sizes it is paired with did not change, so two consumers now combine figures drawn from different universes:

- `internal/api/queue.go:311` computes `pct = 100*(totalBytes-remainingBytes)/totalBytes` with `totalBytes = Job.TotalBytes()`, the whole-manifest total. A freshly added on-demand-par2 job with 10 GB of content and 1 GB of deferred recovery now reports 9% complete before a byte is fetched, and `SizeLeft` disagrees with `Size` by the deferred amount.
- `internal/app/history_helper.go:54` computes `downloaded := totalBytes - FailedBytes() - RemainingBytes()`. For a job finalized while deferred volumes are still in its manifest — a failed job, an early abort, or the logged-error branch at `internal/app/app.go:1057` — this records the deferred bytes as downloaded when they were never fetched.

The fix is the expectation figure in `docs/superpowers/specs/2026-08-05-job-size-figures-design.md` § Derived expectation: a size derived on the same walk and the same predicate as remaining, excluding `Deferred` alone.

```
expected  = sum over files where !Deferred            of Bytes
remaining = sum over files where !Deferred && !Complete of (Bytes - BytesDownloaded - FailedBytes)
```

`downloaded = expected - failed - remaining` then closes for every file kind: an incomplete file yields `Bytes - F - (Bytes-D-F) = D`; a complete file with failures yields `Bytes - F - 0`; a deferred file contributes zero to all three.

`Job.TotalBytes()` is unchanged and keeps its logging and post-processing callers. Only consumers that combine a size with remaining or failed bytes move.

**Files:**
- Modify: `internal/queue/progress.go` (`ExpectedBytes`)
- Modify: `internal/api/queue.go` (percentage and size)
- Modify: `internal/app/history_helper.go` (the downloaded identity)
- Modify: `internal/app/app.go` (the total-failure check)
- Test: `internal/queue/progress_bytes_test.go`, `internal/app/history_helper_test.go`

**Interfaces:**
- Consumes: `FileProgress.Bytes`, `FileProgress.Deferred`, `derivedRemainingBytes` from earlier tasks
- Produces: `func (p *JobProgress) ExpectedBytes() int64` — exported, since `internal/api` and `internal/app` call it

- [ ] **Step 1: Write the failing identity test**

Append to `internal/queue/progress_bytes_test.go` (`package queue`):

```go
// TestExpectedBytes_ClosesTheDownloadedIdentity pins the property every
// consumer of these figures depends on: downloaded = expected - failed -
// remaining, for each kind of file a job can hold at once.
func TestExpectedBytes_ClosesTheDownloadedIdentity(t *testing.T) {
	m := newManifest([]JobFile{
		// Fully downloaded and complete.
		{Subject: "done.rar", Bytes: 2000, Articles: []JobArticle{{ID: "d1", Bytes: 2000}}},
		// Half downloaded, still going.
		{Subject: "partial.rar", Bytes: 2000, Articles: []JobArticle{{ID: "p1", Bytes: 1000}, {ID: "p2", Bytes: 1000}}},
		// One article permanently failed.
		{Subject: "failed.rar", Bytes: 1000, Articles: []JobArticle{{ID: "f1", Bytes: 1000}}},
		// Deferred recovery volume: never dispatched.
		{Subject: "x.vol000+01.par2", Bytes: 500, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 500}}},
	})
	p := newJobProgress(m)
	p.files[3].Deferred = true

	p.markDone(m, 0)
	p.files[0].Complete = true
	p.markDone(m, 1)
	p.markFailed(m, 3)

	// expected excludes only the deferred volume: 2000+2000+1000 = 5000.
	if got, want := p.ExpectedBytes(), int64(5000); got != want {
		t.Errorf("ExpectedBytes() = %d, want %d (deferred volume must not count)", got, want)
	}
	// remaining: done contributes 0 (Complete), partial 2000-1000 = 1000,
	// failed 1000-0-1000 = 0, deferred 0.
	if got, want := p.RemainingBytes(), int64(1000); got != want {
		t.Errorf("RemainingBytes() = %d, want %d", got, want)
	}
	if got, want := p.FailedBytes(), int64(1000); got != want {
		t.Errorf("FailedBytes() = %d, want %d", got, want)
	}
	// The identity every consumer relies on. Bytes actually fetched:
	// 2000 (done) + 1000 (partial's first article) = 3000.
	downloaded := p.ExpectedBytes() - p.FailedBytes() - p.RemainingBytes()
	if want := int64(3000); downloaded != want {
		t.Errorf("downloaded identity = %d, want %d", downloaded, want)
	}
}

// TestExpectedBytes_FreshOnDemandJobReportsZeroProgress pins the
// user-visible symptom directly: the percentage a queue row shows for a job
// whose recovery volumes are deferred and whose content is untouched.
func TestExpectedBytes_FreshOnDemandJobReportsZeroProgress(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "content.rar", Bytes: 10_000, Articles: []JobArticle{{ID: "c1", Bytes: 10_000}}},
		{Subject: "content.vol000+01.par2", Bytes: 1_000, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 1_000}}},
	})
	p := newJobProgress(m)
	p.files[1].Deferred = true

	expected, remaining := p.ExpectedBytes(), p.RemainingBytes()
	if expected != remaining {
		t.Errorf("nothing downloaded but expected (%d) != remaining (%d); a queue row would show non-zero progress", expected, remaining)
	}

	// Un-deferring must move both together, with no fixup call.
	p.files[1].Deferred = false
	if got, want := p.ExpectedBytes(), int64(11_000); got != want {
		t.Errorf("ExpectedBytes() after un-defer = %d, want %d", got, want)
	}
	if p.ExpectedBytes() != p.RemainingBytes() {
		t.Errorf("after un-defer, expected (%d) != remaining (%d)", p.ExpectedBytes(), p.RemainingBytes())
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/queue/ -run TestExpectedBytes -v`
Expected: FAIL to compile — `p.ExpectedBytes undefined`.

- [ ] **Step 3: Add the accessor**

In `internal/queue/progress.go`, directly after `derivedRemainingBytes`:

```go
// ExpectedBytes returns the size of what this job is expected to fetch:
// every file that has not been deferred, whether or not it has been
// downloaded yet.
//
// This is the size that must be paired with RemainingBytes. The two share
// a walk and a predicate on purpose — a consumer computing a percentage or
// a downloaded total from figures with different exclusion sets gets a
// number that is wrong in a way no test of either figure alone would
// catch. RemainingBytes additionally skips Complete files, because they
// have nothing left to fetch; ExpectedBytes counts them, because they are
// part of what the job set out to fetch.
//
// It is therefore NOT Job.TotalBytes(), which is the immutable
// whole-manifest total and still includes deferred recovery volumes. See
// docs/superpowers/specs/2026-08-05-job-size-figures-design.md, which
// records that a job's advertised expectation moving as par2 decisions are
// made is a deliberate consequence.
func (p *JobProgress) ExpectedBytes() int64 {
	if p == nil {
		return 0
	}
	var expected int64
	for fi := range p.files {
		if p.files[fi].Deferred {
			continue
		}
		expected += p.files[fi].Bytes
	}
	return expected
}
```

Match the nil-guard shape of the neighbouring exported readers — read `RemainingBytes` first and follow it rather than copying the block above blindly.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/queue/ -run TestExpectedBytes -v`
Expected: PASS

- [ ] **Step 5: Point the queue API at the expectation**

In `internal/api/queue.go`, in the function containing line 307: replace `totalBytes := j.TotalBytes()` with the expectation from progress, keeping `p` as the source so a non-resident job still works:

```go
	totalBytes := p.ExpectedBytes()
```

Read the surrounding function first. `totalBytes` feeds the percentage, `Size`, `SizeLeft`, `MBLeft` and the slot's byte fields; all of them should now describe the same universe as `remainingBytes`. If any use of `totalBytes` in that function should keep meaning the whole manifest, leave that one on `j.TotalBytes()` and say which and why in your report.

- [ ] **Step 6: Point the history record at it**

In `internal/app/history_helper.go`, the function using `totalBytes := job.Queue.TotalBytes()` at line 36 and `downloaded := totalBytes - p.FailedBytes() - p.RemainingBytes()` at line 54.

Change the size to `p.ExpectedBytes()` so the identity closes. Read the whole function first: if `totalBytes` is also used for a field that should report the job's full advertised size rather than what it expected to fetch, keep that one on the manifest total and use a separate variable for the identity. Report which fields you put on which figure and why.

- [ ] **Step 7: Fix the total-failure check**

`internal/app/app.go:1697` is `if failedBytes >= job.TotalBytes()`. With deferred recovery in the manifest, failed content can never reach the whole-manifest total, so this check cannot fire for an on-demand-par2 job whose content all failed. Change it to compare against the job progress's `ExpectedBytes()`.

Read the surrounding function to confirm progress is reachable there and that this reading of the check's intent is right. If it is not — if the check genuinely means "failed everything including what we chose not to fetch" — leave it and say so in your report with your reasoning.

- [ ] **Step 8: Add the history regression test**

In `internal/app/history_helper_test.go`, following whatever construction the existing tests in that file use — read them first and match, do not invent a fixture:

Add a test for a job finalized with a deferred recovery volume still present, asserting the recorded downloaded byte count equals only what was actually fetched. Size it so the deferred volume's bytes would visibly inflate the figure if the identity used the whole-manifest total, and say in a comment what the number would have been under the old pairing.

If that file has no usable construction path for a job with progress, put the test in the package that does and say where and why in your report.

- [ ] **Step 9: Full gates**

```bash
goimports -w internal/ && go fix ./... && go build ./... && go vet ./...
go test -race -count=1 ./...
golangci-lint run ./...
go run ./scripts/check_test_alignment
go run ./scripts/check_coverage
go run ./scripts/check_lock_io
```

`check_coverage` and `check_lock_io` must be clean. For `check_test_alignment`, only these seven known pre-existing entries may remain: `saveStore`, `readGzJSON`, `clearEmitted`, `isEarlyAbort`, `markEmitted`, `clone`, `insertJobFilesTx`.

- [ ] **Step 10: Commit**

```bash
git add internal/
git commit -m "$(cat <<'EOF'
fix(queue): pair remaining bytes with a size that excludes deferred volumes

Deriving remaining made it skip deferred recovery volumes, but the sizes
it is paired with still covered the whole manifest, so consumers combined
figures from different universes. A freshly added on-demand-par2 job
reported non-zero progress before anything was fetched, and a job
finalized before its volumes were discarded recorded those volumes as
downloaded bytes.

ExpectedBytes derives the job's advertised expectation on the same walk
and predicate as remaining, excluding deferred files alone: a complete
file still counts toward what the job set out to fetch, so the identity
downloaded = expected - failed - remaining closes for every file kind.

Job.TotalBytes stays the immutable whole-manifest total and keeps its
logging and post-processing callers.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: Pin the residency-equivalence property, and document it

The acceptance criterion. Every previous framing of this work failed on a figure meaning different things depending on residency; this asserts it cannot.

**Files:**
- Test: `internal/queue/progress_bytes_test.go`
- Modify: `docs/queue-lifecycle.md`

- [ ] **Step 1: Write the failing test**

```go
func TestRemainingBytes_IdenticalResidentAndNonResident(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	job := makeMultiFileJob(t, "residency-parity", 3, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Partially download the first file so the figure is not simply the total.
	job.progress.files[0].BytesDownloaded = job.progress.files[0].Bytes / 2
	if err := store.Update(t.Context(), job); err != nil {
		t.Fatalf("Update: %v", err)
	}
	resident := job.Progress().RemainingBytes()

	metas, err := store.ArticleCountsByJob(t.Context())
	if err != nil {
		t.Fatalf("ArticleCountsByJob: %v", err)
	}
	nonResident := newJobProgressSized(metas[job.ID])

	if got, want := nonResident.RemainingBytes(), resident; got != want {
		t.Errorf("non-resident = %d, resident = %d", got, want)
	}
}
```

The non-resident progress is **not** adjusted by hand. It must reconstruct
everything from what `ArticleCountsByJob` returns, exactly as `Load` does — a
test that sets `BytesDownloaded` itself proves nothing about production, and an
earlier draft of this plan did precisely that.

This belongs in `progress_bytes_test.go`, not `residency_remaining_bytes_test.go`. That file's two existing tests — `TestTotalRemainingBytes_NonResidentJobsCounted` and `TestTotalRemainingBytes_RestartReconstructsNonResident` — assert that non-resident jobs are *counted at all* (#262). This asserts the stronger property that one job reports the *same* figure either way. Related, not duplicative; leave both.

- [ ] **Step 2: Run the test**

Run: `go test ./internal/queue/ -run TestRemainingBytes_IdenticalResidentAndNonResident -v`
Expected: PASS. It should pass on the first run — Tasks 1–6 established the property, and this pins it against regression. If it fails, the two paths populate `FileProgress.Bytes` differently and that is a defect in Task 1, not in this test.

- [ ] **Step 3: Update the lifecycle contract**

In `docs/queue-lifecycle.md`, in the section describing what `JobProgress` carries, record:

```markdown
Remaining bytes is derived, not stored. It sums the undownloaded part of every
file that is neither complete nor deferred, from `FileProgress` alone, so it
holds at any residency and needs no adjustment when a file's state changes.
`FileProgress.Bytes` exists to make that derivation independent of the
evictable manifest.
```

Place it beside the existing statement that `JobProgress` is always resident, since the two claims depend on each other.

- [ ] **Step 4: Run the full gates**

Run:
```bash
go vet ./... && go test -race ./... && golangci-lint run ./... && ./scripts/run_tests.sh
```
Expected: all pass. Run `go test -tags=integration ./test/integration/...` as well — the `Store` interface changed, and integration tests consume startup wiring.

- [ ] **Step 5: Commit**

```bash
git add internal/queue/ docs/queue-lifecycle.md
git commit -m "$(cat <<'EOF'
test(queue): pin remaining bytes across residency

Asserts the same job reports identical remaining bytes resident and
non-resident. Every earlier attempt at this work produced a figure whose
meaning depended on residency; this is the property that rules it out.

Records the derivation in the lifecycle contract next to the
always-resident claim it depends on.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```
