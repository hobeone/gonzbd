# Derived remaining bytes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the maintained `JobProgress.remainingBytes` counter with a value derived on read from per-file progress, so no state has to be adjusted when a file is deferred, un-deferred or discarded.

**Architecture:** `FileProgress` gains a `Bytes` field, populated from the manifest when a job is resident and from the existing `job_files.bytes` column when it is not. `RemainingBytes()` then sums `Bytes - BytesDownloaded` over files that are neither complete nor deferred, from progress alone at any residency. The `remainingBytes` field, its per-article decrements, its seeds, and `DiscardDeferredPar2`'s downward fixup are deleted.

**Tech Stack:** Go 1.26, SQLite via `modernc.org/sqlite`, goose migrations. No new dependencies.

## Global Constraints

- Step 1 of the sequencing in `docs/superpowers/specs/2026-08-05-job-size-figures-design.md`. **No reported figure may change.** `content_bytes`/`recovery_bytes` and the API change are step 2; the `Discarded` flag is step 3.
- The derivation in this step excludes `Complete` and `Deferred` only. `Discarded` does not exist yet.
- **No schema migration.** `job_files.bytes` already exists and is already written by `insertJobFilesTx`.
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
| `internal/queue/progress.go` | `FileProgress.Bytes`; the derived `RemainingBytes()`; deletion of the counter and its mutations |
| `internal/queue/persistence.go` | `newJobProgressSized` takes per-file bytes instead of a pre-summed total |
| `internal/queue/sqlite_store.go` | `RestoreJobProgress` reads `bytes`; `ArticleCountsByJob` carries bytes; `RemainingBytesByJob` deleted |
| `internal/queue/store.go` | `Store` interface: `ArticleCountsByJob` shape change, `RemainingBytesByJob` removed |
| `internal/queue/job.go` | `ResetForRetry` stops re-adding bytes |
| `internal/queue/queue.go` | `DiscardDeferredPar2` stops adjusting the counter |
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

Note the `remainingBytes` field is still set elsewhere at this point — it is removed in Task 3.

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

### Task 2: Prove a derived value agrees with the maintained one

Adds the derivation next to the existing counter and pins that they agree. This is the safety net for Task 3's deletion: if the derivation is wrong, this fails before the counter is gone.

**Files:**
- Modify: `internal/queue/progress.go` (add `derivedRemainingBytes`)
- Test: `internal/queue/progress_bytes_test.go`

**Interfaces:**
- Consumes: `FileProgress.Bytes` from Task 1
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
	p.files[0].BytesDownloaded += int64(m.ArticleBytes(0))
	check("one article done")

	p.markDone(m, 1)
	p.files[0].BytesDownloaded += int64(m.ArticleBytes(1))
	p.files[0].Complete = true
	check("first file complete")

	p.markFailed(m, 2)
	p.files[1].BytesDownloaded += int64(m.ArticleBytes(2))
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
// nor deferred contributes the part of it not yet downloaded.
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
		if left := f.Bytes - f.BytesDownloaded; left > 0 {
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
Expected: PASS. It exercises no new production code — it pins that un-deferring needs no fixup call, which is the property Task 3 relies on.

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

### Task 3: Delete the maintained counter

**Files:**
- Modify: `internal/queue/progress.go` (field, seed, decrements, `resetForReload`, `RemainingBytes`, JSON shape)
- Modify: `internal/queue/job.go` (`ResetForRetry`)
- Modify: `internal/queue/queue.go` (`DiscardDeferredPar2`)

**Interfaces:**
- Consumes: `derivedRemainingBytes` from Task 2
- Produces: `RemainingBytes()` unchanged in signature, now derived

- [ ] **Step 1: Point the accessor at the derivation**

In `internal/queue/progress.go`, change `RemainingBytes` to return `p.derivedRemainingBytes()`, and update its doc comment to say the figure is computed from per-file state rather than maintained.

- [ ] **Step 2: Run the package suite**

Run: `go test ./internal/queue/ 2>&1 | tail -20`
Expected: PASS, with the counter still present and still maintained. A failure here means a caller depends on the maintained value differing from the derived one — investigate before continuing.

- [ ] **Step 3: Delete the field and every mutation**

Remove, in `internal/queue/progress.go`:
- the `remainingBytes int64` field from `JobProgress`
- `remainingBytes: m.TotalBytes()` from `newJobProgress`
- `p.remainingBytes -= bytes` in `markDone`
- `p.remainingBytes -= bytes` in `markFailed`
- `p.remainingBytes += bytes` in `resetForReload`
- `RemainingBytes: p.remainingBytes` from `MarshalJSON` and the `RemainingBytes` field from `jobProgressJSON`
- `p.remainingBytes = pj.RemainingBytes` from `UnmarshalJSON`

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

### Task 4: Pin the residency-equivalence property, and document it

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
Expected: PASS. It should pass on the first run — Tasks 1–3 established the property, and this pins it against regression. If it fails, the two paths populate `FileProgress.Bytes` differently and that is a defect in Task 1, not in this test.

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
