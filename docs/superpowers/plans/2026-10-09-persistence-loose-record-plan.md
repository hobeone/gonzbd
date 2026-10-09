# Loose Article Record Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace article-level durability (barrier, `durable_runs`,
checkpointer, resumer, stall finalize-recovery, the #417 guard) with a loose
per-article record that is verified by CRC readback before a restored job's
content is attached, keeping resume inside a file.

**Architecture:** Five PRs. Three independent preparations (delete the write
cache; first-writer-wins over byte intervals; delete the two barrier display
fields), one additive PR that builds and unit-tests the record, flusher and
verifier without wiring them, and one switch PR whose commits each build: cut
over, delete dead code, delete config, rewrite docs.

**Tech Stack:** Go 1.27, `modernc.org/sqlite`, `golang.org/x/sys/unix`
(`Fsync`, `Fadvise`; standing approval for `golang.org/x`), Svelte 5 for the
one UI block.

**Spec:** `docs/superpowers/specs/2026-10-09-persistence-loose-record-design.md`
— read it in full before any task. Section references below (§3.3 etc.) are to
that document.

<!-- doccite:ok internal/assembler/ranges.go — created by this plan -->
<!-- doccite:ok internal/assembler/ranges_test.go — created by this plan -->
<!-- doccite:ok TestOwnedRanges_IntersectionIsOwned — created by this plan -->
<!-- doccite:ok TestOwnedRanges — created by this plan -->
<!-- doccite:ok TestQueueSlot_HasNoBarrierFields — created by this plan -->
<!-- doccite:ok internal/durability/written.go — created by this plan -->
<!-- doccite:ok internal/durability/written_test.go — created by this plan -->
<!-- doccite:ok TestApplyRecord_RowsRequireAJobFilesRow — created by this plan -->
<!-- doccite:ok TestApplyRecord_ReplaceKeepsTheLatestWrite — created by this plan -->
<!-- doccite:ok TestApplyRecord_VerdictDeletesAndClears — created by this plan -->
<!-- doccite:ok internal/assembler/written_callback_test.go — created by this plan -->
<!-- doccite:ok TestOnArticleWritten_FiresOnlyAfterASuccessfulWrite — created by this plan -->
<!-- doccite:ok TestFileInfoOwned_RefusesARestartLoser — created by this plan -->
<!-- doccite:ok internal/app/record.go — created by this plan -->
<!-- doccite:ok internal/app/record_test.go — created by this plan -->
<!-- doccite:ok TestRecorder_DropsRowsOfAReplacedInstance — created by this plan -->
<!-- doccite:ok TestRecorder_CompleteNeverLandsWithoutItsLastRow — created by this plan -->
<!-- doccite:ok TestRecorder_RemergesOnError — created by this plan -->
<!-- doccite:ok internal/app/verify.go — created by this plan -->
<!-- doccite:ok internal/app/verify_test.go — created by this plan -->
<!-- doccite:ok internal/job/verified.go — created by this plan -->
<!-- doccite:ok internal/job/verified_test.go — created by this plan -->
<!-- doccite:ok internal/assembler/finish.go — created by this plan -->
<!-- doccite:ok internal/assembler/finish_test.go — created by this plan -->
<!-- doccite:ok internal/app/verify_bench_test.go — created by this plan -->
<!-- doccite:ok internal/app/loose_record_test.go — created by this plan -->
<!-- doccite:ok TestRecorder_ApplyIgnoresTheInstanceCheck — created by this plan -->
<!-- doccite:ok TestRecorder_UntrustPurgesPendingRows — created by this plan -->

## Global Constraints

- Standing Design Rule 1: no migration. Edit `internal/history/migrations/001_initial.sql` and `schema.golden` directly.
- Standing Design Rule 2: the flusher is the only writer that updates `written_articles` and `job_files`; `durability.Store.Admit` only inserts the admission seed. Escalate before adding any other writer.
- Standing Design Rule 4: every "only/never/always" comment is backed by a `git grep` you ran, cited in the comment.
- Every bug-fix or behaviour pin is red-checked with `go run ./scripts/mutate <spec>`, and the observed failure message goes in the commit body.
- Quality gates before each push: the full block under "Quality Gates" in `AGENTS.md`, plus `go run ./scripts/mutate --check-all`, `go run ./scripts/check_citations`, `go run ./scripts/check_doc_citations`.
- After editing any `.go` file: `goimports -w <file>`, `go fix ./...`, `go build ./...`.
- `go vet -tags=integration,uitest,crash ./...` must pass at every commit; the crash suite is behind the `crash` tag.
- Branch names `<type>/<issue#>-<slug>`; Conventional Commits; one PR per section below; never `git stash`.
- Verification reads: one reused 1 MiB buffer per pass; rows in offset order.
- Flusher interval: 5 s, plus once at clean shutdown after `assembler.Stop`.

## Review Focus

1. **A cancelled or EIO verification must change nothing.** Context cancel or an `EIO` from `pread` mid-pass leaves every row in SQLite and the job not resident; the next hydration verifies again. Pinned in Task 4.4.
2. **A file whose fsync failed must never come back trusted after eviction.** Untrusting clears rows and `complete` in SQLite *before* the job can be evicted, because the hydration-time `job_files` restore only sets `complete`. Pinned in Task 5.1, step "untrusted survives eviction".
3. **A history retry under the same job ID must not receive the previous instance's late flush.** The instance check, not the `EXISTS` guard, covers this. Pinned in Task 4.3.
4. **A file with zero accepted articles is not truncated to zero.** `maxEnd == 0` leaves the file alone. Pinned in Task 4.5.
5. **A no-par2 post whose last articles failed is not delivered.** Truncation to `maxEnd` hides the tail on disk, but failed bytes still reach `RepairNoCapacity`. Pinned in Task 4.5.
6. **An unreadable sector must not cost the whole job.** A verification fault parks the job through the stall and never settles it Failed. Pinned in Task 5.1.
7. **A completion fault must recover without a restart.** No tombstone on the fault path, so the refetch opens a fresh writer. Pinned in Task 5.1.

## Stop conditions

Stop and present a "Decision needed" (AGENTS.md § Decision Protocol) if any of
these happens. Do not work around it:

- A task needs a second writer of `written_articles` or `job_files`.
- The cold-read measurement in Task 5.0 exceeds 10 s per 4 GiB on the NFS download mount.
- WAL growth in Task 5.0 exceeds 50 MB per hour per active job.
- A mutate spec cannot be re-anchored because the invariant it pinned no longer exists, and you are unsure whether the design keeps that invariant.

---

## PR 1 — Delete the assembler write cache

Branch: `refactor/persistence-no-write-cache`. The design is specified
against one write path (§3.6): every incumbent is written before a rival
arrives.

### Task 1.1: Route every accepted article straight to `writeOne`

**Files:**
- Delete: `internal/assembler/writecache.go`, `internal/assembler/writecache_test.go` — after moving `articleID`, `articleID.sameArticle` and `bufferedArticle` out of it into `filewriter.go`; `flushRun` and `runPart` go with the cache
- Modify: `internal/assembler/filewriter.go` (remove `wc`, `failDisplaced`, `discardAt`, the cache branch of `Accept`; `Drain` no longer calls `w.wc.drainFile`; `newFileWriter` loses its `*writeCache` parameter)
- Modify: `internal/assembler/helpers_test.go` (`newHelperFile` calls `newFileWriter` without a cache)
- Modify: `internal/assembler/assembler.go` (`Options.WriteCacheBytes`, `newWriteCache` at worker start)
- Modify: `internal/app/app.go` (`writeCacheBytes`, `WriteCacheBytes:` option)
- Modify: `internal/config/downloads.go`, `internal/config/defaults.go`, `internal/constants/limits.go` (`WriteCacheSize`, `DefaultWriteCacheBytes`)
- Modify: `gonzbd.yaml.example`, `test/fixtures/gonzbd.yaml`, the UI config component that renders `write_cache_size` (find it with `git grep -n write_cache_size -- ui/src`)
- Modify: `test/crash/harness.go` (`WriteCacheBytes` option and its config line), `test/crash/crash_test.go` (the bound formula that adds the cache size)
- Test: `internal/assembler/offsetcollision_test.go`, `internal/assembler/displaced_part_test.go`, `internal/assembler/displacedcompletion_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `FileWriter.Accept` writes synchronously through `writeOne`; `articleID`, `sameArticle` and `bufferedArticle` live in `filewriter.go`. Later tasks assume no displacement path exists.

- [ ] **Step 1: List every reference to delete**

Run:
```bash
git grep -n -E 'writeCache|WriteCache|write_cache|failDisplaced|discardAt|drainFile|\.wc\b|contiguousRunSize|flushRun|runPart' -- ':!docs/superpowers'
```
Record the hit count in the PR body. Each hit is either deleted in this task or rewritten.

- [ ] **Step 2: Rewrite the settled-offset test for one write path**

In `internal/assembler/offsetcollision_test.go`, drop `cacheBytes` from `newCollisionFixture` and the `c.f.w.wc = newWriteCache(cacheBytes)` line. Delete `TestCollision_CachedIncumbentIsDisplacedNotRejected` and `TestCollision_ArrivalRejectedWithCachingDisabled` (the cache no longer exists to be disabled). Keep `TestCollision_ArrivalRejectedOnceIncumbentIsWritten` and change its preconditions to:

```go
func TestCollision_ArrivalRejectedOnceIncumbentIsWritten(t *testing.T) {
	c := newCollisionFixture(t)

	if !c.accept(1, "<first@x>", 0, []byte("AAAA")) {
		t.Fatal("precondition: the incumbent was not counted")
	}
	counted := c.accept(2, "<second@x>", 0, []byte("BBBB"))

	if !counted {
		t.Error("the rejected arrival was not counted toward the file's part total")
	}
	if len(c.rejected) != 1 || c.rejected[0] != 2 {
		t.Errorf("OnArticleRejected = %v, want [2] — the arrival loses a written offset", c.rejected)
	}
	onDisk, err := os.ReadFile(c.f.w.path)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if len(onDisk) < 4 || string(onDisk[:4]) != "AAAA" {
		t.Errorf("bytes at offset 0 = %q, want the incumbent's AAAA", onDisk[:4])
	}
}
```

Delete `displaced_part_test.go`: it pins the displacement branch, which this task removes. Keep `displacedcompletion_test.go` if it compiles without the cache; its assertion that a rolled-back article keeps its ownership stays true in this PR and is rewritten in PR 2.

- [ ] **Step 3: Delete the cache and its branch**

Move `articleID`, `sameArticle` and `bufferedArticle` into `filewriter.go`, then remove `writecache.go`. In `FileWriter.Accept`, call `writeOne` for every accepted article. **Keep** the `!owner.written` condition in `offsetSettledBy`: ownership is recorded before `writeOne`, and an article whose write faulted keeps its entry without having written, so the latch is still load-bearing. Rewrite only the doc comment's references to the cache. Delete `failDisplaced` and `discardAt`. Delete `WriteCacheBytes` from `Options` and from `app.New`, and the config key `write_cache_size` with its default and constant.

- [ ] **Step 4: Fix the crash harness**

In `test/crash/harness.go`, delete the `WriteCacheBytes` option, its default, and `c.Downloads.WriteCacheSize = ...`. In `test/crash/crash_test.go`, remove the cache term from the rework-bound arithmetic. Run:
```bash
go vet -tags=crash ./test/crash/
```
Expected: no output.

- [ ] **Step 5: Run the package tests and the config contract**

```bash
go test -race ./internal/assembler/ ./internal/app/ ./internal/config/
go test ./internal/config/ -run 'TestUI|TestAllFlat'
```
Expected: PASS.

- [ ] **Step 6: Re-anchor or delete mutate specs**

```bash
go run ./scripts/mutate --check-all
```
For each STALE anchor in `internal/assembler/testdata/`: if the spec pinned the cache, delete it; otherwise re-anchor it on the line's new form and run it with `go run ./scripts/mutate <spec>` (every mutation must be KILLED).

- [ ] **Step 7: Measure one local run**

Run the throughput benchmark that exercises 750 KB writes (`go test -run '^$' -bench . ./internal/assembler/`) before and after, and put both numbers in the PR body. A regression of more than 10 % is a stop condition.

- [ ] **Step 8: Commit**

```bash
git add -u internal/assembler internal/app internal/config internal/constants test/crash gonzbd.yaml.example test/fixtures ui/src
git commit -m "refactor(assembler)!: delete the write cache" -m "Every accepted article is written synchronously, so an incumbent is always on disk before a rival arrives. Removes the displacement branch the loose-record design would otherwise have to reason about.

BREAKING CHANGE: the downloads.write_cache_size config key is removed."
```

---

## PR 2 — First-writer-wins over byte intervals

Branch: `fix/759-interval-first-writer`. Fixes #759 in-process. A loser after
a restart is covered only once PR 4 adds the seed and PR 5 wires it; the PR
body says so.

### Task 2.1: Interval ownership in `FileWriter`

**Files:**
- Create: `internal/assembler/ranges.go`
- Create: `internal/assembler/ranges_test.go`
- Modify: `internal/assembler/filewriter.go` (`acceptedAt` → `owned ownedRanges`; `offsetSettledBy` → `rangeOwnedBy`)
- Modify: `internal/assembler/overlaprange_test.go` (remove the `t.Skip`)
- Create: `internal/assembler/testdata/interval_owner.spec`

**Interfaces:**
- Produces:
  - `type Range struct{ Off, Len int64 }` (exported; PR 4's `FileInfo.Owned` uses it)
  - `type ownedRanges struct{ ... }` with
    - `func (o *ownedRanges) ownerOf(r Range, arriving articleID) (articleID, bool)` — returns the owner of any range intersecting `r` that is not `arriving` itself
    - `func (o *ownedRanges) claim(r Range, id articleID)` — records ownership; keeps the slice sorted by `Off`
    - `func (o *ownedRanges) seed(rs []Range)` — owner is `seededOwner`, an `articleID` with `artIdx: -1`, meaning "verified before this process". Never the zero `articleID`: `sameArticle` compares `artIdx` alone, so a zero sentinel would treat every arrival of article 0 as its own owner

- [ ] **Step 1: Write the failing unit test for the range set**

```go
package assembler

import "testing"

func TestOwnedRanges_IntersectionIsOwned(t *testing.T) {
	var o ownedRanges
	a := articleID{artIdx: 1, msgID: "<a@x>"}
	b := articleID{artIdx: 2, msgID: "<b@x>"}
	o.claim(Range{Off: 0, Len: 1000}, a)

	cases := []struct {
		name string
		r    Range
		want bool
	}{
		{"same start", Range{0, 10}, true},
		{"straddles end", Range{500, 1000}, true},
		{"abuts end", Range{1000, 10}, false},
		{"zero-length inside", Range{10, 0}, false},
		{"before", Range{-10, 10}, false},
	}
	for _, tc := range cases {
		if _, got := o.ownerOf(tc.r, b); got != tc.want {
			t.Errorf("%s: ownerOf(%+v) owned = %v, want %v", tc.name, tc.r, got, tc.want)
		}
	}
	if _, got := o.ownerOf(Range{0, 1000}, a); got {
		t.Error("an article does not collide with its own range (a write-fault retry re-arrives at the same offset)")
	}
}
```

Check the `articleID` field names against `filewriter.go` before running and adjust the literal if they differ.

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test ./internal/assembler/ -run TestOwnedRanges -count=1`
Expected: FAIL to compile, `undefined: ownedRanges`.

- [ ] **Step 3: Implement `ranges.go`**

```go
package assembler

import "sort"

// Range is a half-open byte range [Off, Off+Len) in a target file.
type Range struct{ Off, Len int64 }

func (r Range) end() int64 { return r.Off + r.Len }

func (r Range) intersects(s Range) bool {
	return r.Len > 0 && s.Len > 0 && r.Off < s.end() && s.Off < r.end()
}

type ownedRange struct {
	r  Range
	id articleID
}

// ownedRanges records which article owns each written byte range of one file.
// Ranges never intersect: claim is called only after ownerOf found no owner.
type ownedRanges struct{ s []ownedRange }

// ownerOf returns the owner of a range intersecting r, unless that owner is
// arriving itself.
func (o *ownedRanges) ownerOf(r Range, arriving articleID) (articleID, bool) {
	i := sort.Search(len(o.s), func(i int) bool { return o.s[i].r.end() > r.Off })
	for ; i < len(o.s) && o.s[i].r.Off < r.end(); i++ {
		if o.s[i].r.intersects(r) && !o.s[i].id.sameArticle(arriving) {
			return o.s[i].id, true
		}
	}
	return articleID{}, false
}

func (o *ownedRanges) claim(r Range, id articleID) {
	if r.Len <= 0 {
		return
	}
	i := sort.Search(len(o.s), func(i int) bool { return o.s[i].r.Off >= r.Off })
	if i < len(o.s) && o.s[i].r == r {
		o.s[i].id = id
		return
	}
	o.s = append(o.s, ownedRange{})
	copy(o.s[i+1:], o.s[i:])
	o.s[i] = ownedRange{r: r, id: id}
}

// seededOwner owns ranges verified before this process. Its index matches no
// manifest article, so sameArticle never waves an arrival through.
var seededOwner = articleID{artIdx: -1}

func (o *ownedRanges) seed(rs []Range) {
	for _, r := range rs {
		o.claim(r, seededOwner)
	}
}
```

Add a case to the step 1 test: after `o.seed([]Range{{0, 100}})`, an arrival `articleID{artIdx: 0}` over `Range{50, 10}` must be owned — the case a zero sentinel would miss.

- [ ] **Step 4: Run the unit test**

Run: `go test ./internal/assembler/ -run TestOwnedRanges -count=1`
Expected: PASS.

- [ ] **Step 5: Switch `FileWriter` to the range set**

Replace `acceptedAt map[int64]offsetOwner` with `owned ownedRanges`. Replace `offsetSettledBy(off, arriving)` with `rangeOwnedBy(Range{off, int64(len(data))}, arriving)` at its call site in `acceptArticle`. **Move the claim to after `writeOne` returned nil** (today `acceptedAt[off] = ...` is assigned before the write): an article whose write faulted then owns nothing, and the `written` latch disappears because every owner has written. Rewrite `displacedcompletion_test.go`'s ownership assertion to the new rule — after a faulted write, `w.owned.ownerOf(...)` reports no owner — and the field's doc comment: it records written ranges, and it is seeded from verified rows once PR 4 lands.

- [ ] **Step 6: Unskip the overlap probe and run it**

Remove the `t.Skip(...)` in `TestOverlap_PartialRangeOverwritesADurableArticle` and update its comment to say the arrival is refused. Run:
```bash
go test -race ./internal/assembler/ -count=1
```
Expected: PASS.

- [ ] **Step 7: Red-check**

Create `internal/assembler/testdata/interval_owner.spec`:

```text
pkg ./internal/assembler/
run TestOverlap_PartialRangeOverwritesADurableArticle|TestOwnedRanges_IntersectionIsOwned

[intersection never reported]
file internal/assembler/ranges.go
--- anchor
		if o.s[i].r.intersects(r) && !o.s[i].id.sameArticle(arriving) {
--- replace
		if false {
--- end
```

Run: `go run ./scripts/mutate internal/assembler/testdata/interval_owner.spec`
Expected: every mutation KILLED. Copy the failure message into the commit body.

- [ ] **Step 8: Commit**

```bash
git add internal/assembler
git commit -m "fix(assembler): refuse an article whose byte range intersects an owned one" -m "Closes #759 within one process. <paste the mutate failure message>"
```

---

## PR 3 — Delete `bytes_pending` and `last_barrier_unix`

Branch: `refactor/persistence-drop-barrier-fields`. These slot fields only
display barrier state, so they can go before the barrier does. They are not
§10 fields.

### Task 3.1: Remove the two fields end to end

**Files:**
- Modify: `internal/api/queue.go` (slot fields and their assignment), `internal/api/roles.go` (comment), `internal/api/apitest/nopapp.go`
- Modify: `internal/app/statusinfo.go` (`JobCheckpointState.PendingBytes`, `.LastBarrier`; keep `StallReason`)
- Modify: `ui/src/lib/components/QueueRow.svelte` (the "written but not yet fsynced" block), `ui/src/lib/types.ts`, `ui/src/lib/components/QueueRow.test.ts`
- Modify: `test/crash/harness.go` (the parsed struct fields; nothing asserts on them)
- Test: `internal/api/stall_test.go`, `internal/app/statusinfo_test.go`, and the other test files found in step 1

**Interfaces:**
- Produces: `JobCheckpointState{StallReason string}` only. `app.jobBarrierBytes` and `app.lastBarrier` stay until PR 5 (the barrier still writes and reads them).

- [ ] **Step 1: Enumerate**

```bash
git grep -n -E 'bytes_pending|last_barrier|PendingBytes|LastBarrier' -- ':!docs/superpowers'
```
Record the count. Every hit is deleted or rewritten in this task, except the writes to `app.jobBarrierBytes`/`app.lastBarrier` inside the barrier.

- [ ] **Step 2: Write the failing API test**

In `internal/api/queue_test.go` (or the nearest queue-slot test file), add:

```go
func TestQueueSlot_HasNoBarrierFields(t *testing.T) {
	b, err := json.Marshal(queueSlot{})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"bytes_pending"`, `"last_barrier_unix"`} {
		if bytes.Contains(b, []byte(k)) {
			t.Errorf("queue slot still serialises %s", k)
		}
	}
}
```

Use the slot type's real name from `internal/api/queue.go`.

- [ ] **Step 3: Confirm it fails, then delete the fields**

Run: `go test ./internal/api/ -run TestQueueSlot_HasNoBarrierFields -count=1` → FAIL. Delete the fields and their consumers listed above, then re-run → PASS.

- [ ] **Step 4: UI**

```bash
cd ui && bun run check && bun run test && cd ..
```
Expected: PASS.

- [ ] **Step 5: Gates and commit**

```bash
go vet -tags=integration,uitest,crash ./... && go test -race ./internal/api/ ./internal/app/
git add -u internal/api internal/app ui/src test/crash
git commit -m "refactor(api,ui)!: drop the bytes_pending and last_barrier_unix slot fields" -m "Both display barrier state that the loose-record design removes; neither is a §10 field.

BREAKING CHANGE: queue slots no longer carry bytes_pending or last_barrier_unix."
```

---

## PR 4 — The record, flusher and verifier, built but not wired

Branch: `feat/persistence-loose-record-parts`. Nothing reads the new table
yet. Every unit is unit-tested on a temp directory and a temp SQLite database.

### Task 4.1: Schema and store

**Files:**
- Modify: `internal/history/migrations/001_initial.sql`, `internal/history/testdata/schema.golden` (or wherever `schema.golden` lives: `git ls-files '*schema.golden'`)
- Create: `internal/durability/written.go`, `internal/durability/written_test.go`
- Modify: `internal/durability/reclaim.go` (`perJobTables` gains `written_articles`, marked kept for a FAILED entry, as `job_files` becomes)
- Test: `internal/durability/reclaim_test.go` (`TestPerJobTables_CoversEveryJobKeyedTable`)

**Interfaces:**
- Produces:
  ```go
  // WrittenRow is one written_articles row. (durability.WrittenArticle already
  // exists in synctarget.go for the barrier and is deleted in PR 5.)
  type WrittenRow struct {
      FileIdx int
      ArtIdx  int32
      Offset  int64
      Length  int64
      CRC32   uint32
  }
  // FileState is the job_files columns the flusher updates.
  type FileState struct {
      FileIdx     int
      Complete    bool
      Filename    string
      FetchPolicy uint8
  }
  // FileVerdict is a verification or untrust result for one file.
  type FileVerdict struct {
      FileIdx       int
      DeleteAll     bool    // every row of the file is deleted
      DeleteArtIdxs []int32 // rows to delete when !DeleteAll
      ClearComplete bool
      SetComplete   bool    // set only after a successful finish-by-path
  }
  // RecordBatch is one flusher transaction.
  type RecordBatch struct {
      JobID    string
      Rows     []WrittenRow
      Files    []FileState
      Verdicts []FileVerdict
  }
  func (s *Store) ApplyRecord(ctx context.Context, batches []RecordBatch) error
  func (s *Store) WrittenRows(ctx context.Context, jobID string) ([]WrittenRow, error) // ordered by file_idx, offset
  ```

- [ ] **Step 1: Add the `written_articles` table only** (spec §2) to `001_initial.sql`, and regenerate `schema.golden` with the command its test prints on mismatch. Do **not** touch `job_files` here: `Store.Admit`, `SaveProgress` and `FileRows` still write and read `assembled_crc32` until Task 5.2.

- [ ] **Step 2: Write the failing store tests**

```go
func TestApplyRecord_RowsRequireAJobFilesRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.ApplyRecord(ctx, []RecordBatch{{
		JobID: "gone",
		Rows:  []WrittenRow{{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 10, CRC32: 1}},
	}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.WrittenRows(ctx, "gone")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("rows for a job with no job_files row = %v, want none (the EXISTS guard)", got)
	}
}

func TestApplyRecord_ReplaceKeepsTheLatestWrite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Admit(ctx, "j", []uint8{0}); err != nil {
		t.Fatal(err)
	}
	row := WrittenRow{FileIdx: 0, ArtIdx: 3, Offset: 100, Length: 10, CRC32: 1}
	for _, crc := range []uint32{1, 2} {
		row.CRC32 = crc
		if err := s.ApplyRecord(ctx, []RecordBatch{{JobID: "j", Rows: []WrittenRow{row}}}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.WrittenRows(ctx, "j")
	if len(got) != 1 || got[0].CRC32 != 2 {
		t.Errorf("rows = %+v, want one row with crc 2 (INSERT OR REPLACE)", got)
	}
}

func TestApplyRecord_VerdictDeletesAndClears(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.Admit(ctx, "j", []uint8{0})
	_ = s.ApplyRecord(ctx, []RecordBatch{{JobID: "j",
		Rows:  []WrittenRow{{0, 0, 0, 10, 1}, {0, 1, 10, 10, 2}},
		Files: []FileState{{FileIdx: 0, Complete: true, Filename: "a.bin"}},
	}})
	if err := s.ApplyRecord(ctx, []RecordBatch{{JobID: "j",
		Verdicts: []FileVerdict{{FileIdx: 0, DeleteAll: true, ClearComplete: true}},
	}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.WrittenRows(ctx, "j"); len(got) != 0 {
		t.Errorf("rows after DeleteAll = %+v", got)
	}
	rows, _ := s.FileRows(ctx, "j")
	if rows[0].Complete {
		t.Error("complete survived ClearComplete")
	}
}
```

Use the package's existing temp-store helper; find it with `git grep -n 'func newTestStore\|func openTestStore' internal/durability`.

- [ ] **Step 3: Run → FAIL** (`undefined: RecordBatch`).

- [ ] **Step 4: Implement `written.go`.** One transaction per `ApplyRecord` call (use `s.inTx`). Order inside it: verdict deletes, then `INSERT OR REPLACE ... SELECT ... WHERE EXISTS (SELECT 1 FROM job_files WHERE job_id = ?)` per row, then `UPDATE job_files SET complete, filename, fetch_policy` per file state, then verdict `complete` changes. `SetComplete` and `ClearComplete` are mutually exclusive; return an error if both are set.

- [ ] **Step 5: Run → PASS.** Then update `perJobTables` and run `go test ./internal/durability/ -run TestPerJobTables_CoversEveryJobKeyedTable -count=1` → PASS.

- [ ] **Step 6: Commit** `feat(durability): add the written_articles record and its batch writer`.

### Task 4.2: `OnArticleWritten` and the seed field in the assembler

**Files:**
- Modify: `internal/assembler/assembler.go` (`Options.OnArticleWritten`; `FileInfo.Owned []Range`)
- Modify: `internal/assembler/filewriter.go` (call the callback after `writeAt` returned nil; seed `owned` from `FileInfo.Owned` at open)
- Test: `internal/assembler/written_callback_test.go`

**Interfaces:**
- Produces:
  - `Options.OnArticleWritten func(jobID string, fileIdx int, artIdx int32, off, n int64, crc uint32)` — worker goroutine, after a successful write, never on a refused or faulted article.
  - `FileInfo.Owned []Range` — ranges verified before this process; an arrival intersecting one is refused.

- [ ] **Step 1: Failing tests**

```go
func TestOnArticleWritten_FiresOnlyAfterASuccessfulWrite(t *testing.T) {
	a := newHelperAssembler()
	var got []int32
	a.opts.OnArticleWritten = func(_ string, _ int, artIdx int32, _, _ int64, _ uint32) {
		got = append(got, artIdx)
	}
	f := newHelperFile(t, t.TempDir(), "w.dat", 1<<20)
	f.info.TotalParts = 3
	a.handleSuccessArticle(f, WriteRequest{JobID: "j", ArtIdx: 1, MessageID: "<1@x>", Offset: 0, Data: []byte("AAAA")})
	a.handleSuccessArticle(f, WriteRequest{JobID: "j", ArtIdx: 2, MessageID: "<2@x>", Offset: 2, Data: []byte("BBBB")}) // intersects, refused
	f.w.writeAt = func([]byte, int64) (int, error) { return 0, syscall.EIO }
	a.handleSuccessArticle(f, WriteRequest{JobID: "j", ArtIdx: 3, MessageID: "<3@x>", Offset: 100, Data: []byte("CCCC")}) // faulted
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("OnArticleWritten fired for %v, want [1]", got)
	}
}

func TestFileInfoOwned_RefusesARestartLoser(t *testing.T) {
	a := newHelperAssembler()
	var rejected []int32
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) { rejected = append(rejected, artIdx) }
	f := newHelperFile(t, t.TempDir(), "s.dat", 1<<20)
	f.info.TotalParts = 1
	f.w.owned.seed([]Range{{Off: 0, Len: 1000}})
	a.handleSuccessArticle(f, WriteRequest{JobID: "j", ArtIdx: 9, MessageID: "<9@x>", Offset: 500, Data: make([]byte, 1000)})
	if len(rejected) != 1 || rejected[0] != 9 {
		t.Errorf("rejected = %v, want [9]", rejected)
	}
}
```

The second test sets the seed directly; step 3 makes the open path do it from `FileInfo.Owned`, and a third test asserts that through the resolver.

- [ ] **Step 2: Run → FAIL.** **Step 3: Implement.** **Step 4: Run → PASS.**
- [ ] **Step 5: Red-check** with a spec that neuters the callback call (`if false {`) and the seed loop. Every mutation KILLED.
- [ ] **Step 6: Commit** `feat(assembler): report each written article and seed ownership from verified ranges`.

### Task 4.3: The recorder — handler plus flusher

**Files:**
- Create: `internal/app/record.go`, `internal/app/record_test.go`

**Interfaces:**
- Consumes: `durability.Store.ApplyRecord`, `durability.WrittenRow`, `durability.FileState`, `durability.FileVerdict` (Task 4.1).
- Produces:
  ```go
  type recordStore interface {
      ApplyRecord(ctx context.Context, batches []durability.RecordBatch) error
  }
  type recorder struct { /* mu guards pending and dirty */ }
  func newRecorder(st recordStore, current func(id string) *job.Job, log *slog.Logger) *recorder
  // noteWritten is the OnArticleWritten handler body: append the row, then mark Done.
  func (r *recorder) noteWritten(j *job.Job, row durability.WrittenRow, bytes int64, server string)
  func (r *recorder) markDirty(j *job.Job, fileIdx int, st durability.FileState)
  // flush snapshots under mu, writes outside it, re-merges on error.
  func (r *recorder) flush(ctx context.Context) error
  // apply commits verdicts synchronously through the same writer.
  func (r *recorder) apply(ctx context.Context, j *job.Job, v []durability.FileVerdict) error
  func (r *recorder) run(ctx context.Context, every time.Duration)
  ```
  The pending map is keyed by `*job.Job`, so the instance check is `current(j.ID()) == j` at snapshot time — in `flush` only. `apply` takes the instance from its caller and does not consult `current`: a retry commits its verdict before `dispatcher.Add`, when `current` does not yet return the rebuilt job.

- [ ] **Step 1: Failing tests** (fake `recordStore` that records batches; `job.Job` values built with the test helper the app package already uses — find it with `git grep -n 'func newTestJob\|func testJob' internal/app/*_test.go`):

```go
func TestRecorder_DropsRowsOfAReplacedInstance(t *testing.T) {
	st := &fakeRecordStore{}
	old, cur := newTestJob(t, "id"), newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return cur }, slog.Default())
	r.noteWritten(old, durability.WrittenRow{FileIdx: 0, ArtIdx: 0, Length: 10}, 10, "s")
	if err := r.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := st.rowCount(); n != 0 {
		t.Errorf("flushed %d rows from a job instance a retry replaced, want 0", n)
	}
}

func TestRecorder_ApplyIgnoresTheInstanceCheck(t *testing.T) {
	st := &fakeRecordStore{}
	rebuilt := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return nil }, slog.Default()) // not yet Added
	if err := r.apply(context.Background(), rebuilt, []durability.FileVerdict{{FileIdx: 0, ClearComplete: true}}); err != nil {
		t.Fatal(err)
	}
	if len(st.batches) != 1 || len(st.batches[0].Verdicts) != 1 {
		t.Errorf("batches = %+v, want the retry's verdict committed before Add", st.batches)
	}
}

func TestRecorder_CompleteNeverLandsWithoutItsLastRow(t *testing.T) {
	st := &fakeRecordStore{}
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, slog.Default())
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 4, Length: 10}, 10, "s")
	r.markDirty(j, 0, durability.FileState{FileIdx: 0, Complete: true})
	_ = r.flush(context.Background())
	b := st.batches[0]
	if len(b.Rows) != 1 || len(b.Files) != 1 {
		t.Errorf("batch = %+v, want the row and the complete flag in one transaction", b)
	}
}

func TestRecorder_RemergesOnError(t *testing.T) {
	st := &fakeRecordStore{failNext: true}
	j := newTestJob(t, "id")
	r := newRecorder(st, func(string) *job.Job { return j }, slog.Default())
	r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Length: 10}, 10, "s")
	if err := r.flush(context.Background()); err == nil {
		t.Fatal("want the store error")
	}
	_ = r.flush(context.Background())
	if n := st.rowCount(); n != 1 {
		t.Errorf("rows after a retried flush = %d, want 1", n)
	}
}
```

Add a fourth test, `TestRecorder_UntrustPurgesPendingRows`: `noteWritten` two rows for file 0, then `apply` a `DeleteAll` verdict for file 0, then `flush`; assert no batch carries a row for file 0.

- [ ] **Step 2: Run → FAIL.** **Step 3: Implement.** A `DeleteAll` verdict removes the file's pending rows and dirty entry under `mu` before its transaction. The `ApplyRecord` call happens with `mu` released. `noteWritten` calls `j.MarkArticleDone(int(row.ArtIdx), bytes, server)` after appending and logs `job.ErrNotResident` at Debug.
- [ ] **Step 4: Run → PASS** with `-race`.
- [ ] **Step 5: Red-check** the instance check and the single-snapshot rule (move the dirty snapshot into a second critical section) with a spec under `internal/app/testdata/recorder_*.spec`.
- [ ] **Step 6: Commit** `feat(app): add the recorder that owns the written-article record`.

### Task 4.4: `verifyJobFiles`, `fileCRCFromRows`, `InstallVerified`

**Files:**
- Create: `internal/app/verify.go`, `internal/app/verify_test.go`
- Create: `internal/job/verified.go`, `internal/job/verified_test.go`
- Modify: the eight test helpers that call `job.SeedFromRuns` (`git grep -ln 'SeedFromRuns' -- '*_test.go'`) to use `InstallVerified`, so PR 5 can delete `SeedFromRuns` without touching them

**Interfaces:**
- Consumes: `durability.WrittenRow`, `durability.FileVerdict`, `crc32util.Combine(crc1, crc2 uint32, len2 int64) uint32`, `Manifest.FileRange(fileIdx int) (lo, hi int)`.
- Produces:
  ```go
  // job package
  func (j *Job) InstallVerified(fileIdx int, rows []durability.WrittenRow) error // marks each Done, keeps rows resident for the CRC
  func (j *Job) FileRows(fileIdx int) []durability.WrittenRow
  // app package
  type verifyResult struct {
      Verdicts []durability.FileVerdict
      Verified map[int][]durability.WrittenRow // per file, rows that matched
      Failed   map[int][]int32                     // per file, articles failed by an intersection
      Finished []int                               // files finished by path in this pass
  }
  func verifyJobFiles(ctx context.Context, name string, m *job.Manifest, files []durability.FileRow, rows []durability.WrittenRow, dl string, retry bool) (verifyResult, error)
  // errVerifyFault wraps every non-definitive error verifyJobFiles returns, so
  // reconcileResidency can park the job instead of settling it Failed.
  type errVerifyFault struct{ File string; Err error }
  func fileCRCFromRows(rows []durability.WrittenRow, failed bool, lo, hi int) (uint32, bool) // false ⇒ NoCRC
  func finishFileByPath(path string, maxEnd int64) error
  ```
  `verifyJobFiles` is pure with respect to the job: it reads SQLite rows and file bytes, and returns a result. Callers commit `Verdicts` through `recorder.apply`, then attach, then install.

- [ ] **Step 1: Failing tests for `verifyJobFiles`.** Build a temp download dir and a manifest with one file of four 1 KiB articles; write the file; build rows with the true CRCs. One table-driven test with these cases, each asserting `Verdicts`, `Verified` and `Finished`:
  - all match, not every article present → `Verified` has the matching rows, no verdict;
  - one range zeroed on disk → that row in `DeleteArtIdxs`, the others verified;
  - file missing → `DeleteAll`;
  - file truncated through the last article → short read → that row deleted;
  - two rows intersecting, the first matching → second article in `Failed`, not deleted from `Verified`'s winner;
  - every article present and matching → file in `Finished`, `SetComplete` in its verdict, the file truncated to `maxEnd`;
  - `complete=1` file → no bytes read (make the file unreadable with `chmod 000` and assert no error), every row verified;
  - a `complete=0` file whose policy is on-demand → read and verified like any other.

  A second test injects a read error: replace the package-level `preadAt` seam with one returning `syscall.EIO` on the second row, and assert `verifyJobFiles` returns an `*errVerifyFault` (via `errors.As`) naming the file **and** a zero `verifyResult` (Review Focus 1). A third cancels the context after the first row and asserts the same. A fourth covers the retry path: two intersecting rows where the first matches, called with `retry=true`, must leave the file out of `Finished` and put no `SetComplete` in its verdict; the same input with `retry=false` finishes it.

- [ ] **Step 2: Failing tests for `fileCRCFromRows`.** Gapless chain → the combined CRC equals `crc32.ChecksumIEEE` of the concatenated bytes; a gap → false; a straddle (row 2 starts inside row 1) → false; first row not at 0 → false; `failed == true` → false; a missing article in `[lo, hi)` → false.

- [ ] **Step 3: Failing test for `InstallVerified`.** Attach content to a test job, install two rows for file 0, assert `CountUnfinishedArticles(0)` drops by two and `FileRows(0)` returns them in offset order.

- [ ] **Step 4: Run all three → FAIL.**

- [ ] **Step 5: Implement.** In `verifyJobFiles`, per file with `complete=0` and rows (whatever its fetch policy and the job's state): open → `unix.Fsync` → on error, `DeleteAll` → `unix.Fadvise(fd, 0, 0, unix.FADV_DONTNEED)` → read rows in offset order through one 1 MiB buffer via the `preadAt` seam → compare `crc32.ChecksumIEEE`. Any error other than `ENOENT`, a short read at EOF, or an fsync error aborts the whole pass with the zero result. Then compute `strandedComplete` over the verified and failed sets; for a finishable file, call `finishFileByPath(path, maxEnd)` (open, fsync, truncate if larger and `maxEnd > 0`, fsync, close).

- [ ] **Step 6: Run → PASS.** Migrate the `SeedFromRuns` test helpers and run `go test -race ./internal/api/ ./internal/app/ ./internal/postproc/` and `go vet -tags=uitest ./test/uitest/`.

- [ ] **Step 7: Red-check** with a spec that (a) treats a CRC mismatch as a match, (b) skips the fsync, (c) drops the `maxEnd > 0` guard, (d) lets an EIO fall through to the delete path. Every mutation KILLED.

- [ ] **Step 8: Commit** `feat(app,job): add the restart verifier and the derived whole-file CRC`.

### Task 4.5: Worker-side `finishFile` — built, not yet called

**Files:**
- Create: `internal/assembler/finish.go`, `internal/assembler/finish_test.go`

**Interfaces:**
- Produces: `func (w *FileWriter) finish() (charged []int32, err error)` — fsync; `maxEnd` over `w.owned` (accepted plus seeded); truncate if larger and `maxEnd > 0`; fsync; return the articles of any intersecting pair (none can exist after PR 2, so this is a defensive charge with its own test). Not called until PR 5.

- [ ] **Step 1: Failing tests:** truncates a preallocated file to the last owned end; leaves a file with no owned range at its preallocated size (Review Focus 4); an injected fsync error is returned and the file is not truncated.
- [ ] **Step 2: Failing pipeline-level test for Review Focus 5,** in `internal/job`: a manifest of one file, four articles, no par2; mark articles 0 and 1 Done and 2 and 3 failed; assert `RepairStateFrom(j.ContentFailedBytes(), 0, false)` returns `RepairNoCapacity`. This pins that failed bytes come from article sizes, independent of the file on disk.
- [ ] **Step 3: Implement, run → PASS, red-check, commit** `feat(assembler): add the worker-side file finish`.

---

## PR 5 — The switch

Branch: `refactor/persistence-loose-record`. Four commits, each building and
passing the full gate block. Before opening the PR, Task 5.0's measurements go
in the PR body.

### Task 5.0: Measure

- [ ] Cold read: on the NFS download mount, write a 4 GiB file, `echo 3 | sudo tee /proc/sys/vm/drop_caches` (ask the user to run it with `!`), then time `verifyJobFiles` over it with a bench harness in `internal/app/verify_bench_test.go`. Record the figure. Stop condition above.
- [ ] WAL growth: run three concurrent jobs against the mock NNTP server for 10 minutes with the recorder wired (after Task 5.1), and record `-wal` file size and the p99 `ApplyRecord` duration. Stop condition above.

### Task 5.1: Cut over (commit 1)

**Files:** `internal/app/residency.go`, `internal/dispatch/tick.go` (`reconcileResidency` does not settle on a verification fault; the residency port's doc states the new error class), `app.go` (`retryHistoryJob` — including its `checkpointer.Prune`/`FlushJob`/`Mark` calls and abort defer, the shape check, and setting `complete=0` on every file before `verifyJobFiles`; `handleFileComplete`, `completeFinalizedFile`, wiring), `internal/app/verify.go` (receives `strandedComplete` and `jobFilePath` from `resume_startup.go`), `internal/history/repository.go` (`Add` loses its per-file progress argument; its callers), the four `FileAssembledCRC32(` call sites re-pointed at `fileCRCFromRows` (`dispatcher_wiring.go`, `job_finalizer.go`, `par2names.go`, `internal/postproc/stage_quickcheck.go`), `internal/assembler` (the `charged` result of `finish()` routed to `OnArticleRejected` as failed bytes), `pipeline.go` (`registerFile` fills `FileInfo.Owned`), `job_finalizer.go` (`persistAndCommit` flush; `retainedProgressFor` removed from the call), `stall.go` (`reevaluateStall` keeps only the parking half), `reloader.go` (`Quiesce`, drop the #417 guard), `events.go` (`FileComplete.Resumed`), `internal/assembler/assembler.go` (`Quiesce` control message; call `w.finish()` before `OnFileComplete`), `test/crash/harness.go` and `crash_test.go` (new oracle; re-pin the two SIGKILL tests).

**Interfaces:**
- Consumes: everything from PR 4.
- Produces: no caller of the barrier, checkpointer, resumer, startup sweep, `restoreResolution` or `finalizeCompletedFile` remains. Their packages still compile.

- [ ] **Step 1: Failing app-level tests** (one file, `internal/app/loose_record_test.go`):
  - restart resumes inside a file: write half a file's articles, flush, build a fresh `Application` on the same dirs, hydrate, assert those articles are Done and the rest Outstanding;
  - a zeroed range is refetched: same, but zero one written range before restart; that article is Outstanding;
  - **untrusted survives eviction** (Review Focus 2): inject an fsync error at finish, then evict and re-hydrate the job; assert the file's articles are Outstanding and `job_files.complete` is 0 in SQLite;
  - retry verifies: fail a job from Assessing with a written file, retry it from history, assert written articles are Done without being re-fetched;
  - paused job reports progress: restore a Fetching job with `IntentPause`, assert `mode=queue` shows its verified percentage before resume;
  - crash after leaving Fetching: write a file fully, stop before the flush carrying `complete=1`, restore the job in Assessing, zero one range, and assert that article is Outstanding (the read happens outside Fetching);
  - retry after post-processing: complete a job, overwrite one byte of a delivered file as par2 repair would, fail and retry it; assert the changed article is refetched, not trusted;
  - shape mismatch: retry with an NZB whose file has one article fewer; assert every row of the job is deleted;
  - **a completion fault converges in-process**: inject an fsync error at finish, resume the stall, and assert the file's articles are refetched, written and the file reaches `complete=1` without a restart (no tombstone; the cached `FileInfo` was dropped);
  - **a verification fault parks, never fails**: restore a job whose file read returns `EIO` through the `preadAt` seam; assert the job is stalled with a reason naming the file, its outcome is unset, and after the seam is restored a resume verifies and attaches it;
  - **a close-time fsync fault untrusts**: inject an fsync error in `drainAndCloseAll` at shutdown; after restart assert the file has no rows and its articles are Outstanding;
  - `complete=1` at restart: assert its failed set is installed before `completeFinalizedFile` runs, by checking `hasFailedArticle` for a file with one failed article.
- [ ] **Step 2: Run → FAIL.**
- [ ] **Step 3: Implement the cutover** per spec §3.2–§3.8. In `Hydrate`: on the no-progress branch, read `FileRows` and `WrittenRows`, call `verifyJobFiles`, commit with `recorder.apply`, then `AttachContent`, `InstallVerified` per file, `MarkFileComplete` and enqueue `FileComplete{Resumed: true}` for each `Finished` file. Return the error with nothing attached if any of that fails.
- [ ] **Step 4: Rewrite the crash oracle:** after a kill, every article the restarted daemon reports Done hashes to its row's CRC (`TestSIGKILL_NoArticleIsResolvedWithoutItsBytes`); no verified article is fetched again (`TestSIGKILL_ReworkStaysWithinTheCheckpointBound`, renamed in the same commit to say what it now pins). Run `go test -tags=crash -timeout=20m ./test/crash/`.
- [ ] **Step 5: Run the full gate block → PASS.** Delete app-side unexported helpers that are now unused, and their tests, so `golangci-lint`'s `unused` passes.
- [ ] **Step 6: Red-check** each half separately: the verify-before-attach order (attach first), the untrust write (clear in memory only), the `persistAndCommit` flush (removed), the instance check (removed), `Resumed` (ignored). Every mutation KILLED.
- [ ] **Step 7: Commit** `refactor(app)!: resume from the verified article record` with the failure messages in the body.

### Task 5.2: Delete dead code (commit 2)

- [ ] Delete the app-side orchestration that 5.1 left uncalled: the barrier half of `internal/app/durability.go` (keep Stall/Fail, reclaim, sweep, `filePathFor`), `internal/app/resume_startup.go` and `startup_reconcile.go` (after 5.1 moved `strandedComplete` and `jobFilePath`), the finalize-recovery half of `stall.go`, `shutdownCheckpoint`, `noteUndeliveredCompletion`, `finalizeCompletedFile`, `restoreResolution`, `retainedProgressFor`, `forgetJobBarrierState`, and the FileWriter barrier half (`Drain`, `Sync`, `Truncate`, `written`, `reported`).
- [ ] Drop `job_files.assembled_crc32`, and with it the column from `Store.Admit`'s insert, `SaveProgress`'s update, `FileRows`' select and `FileRow.AssembledCRC32`.
- [ ] Delete `internal/checkpoint`; `internal/durability/{barrier,proof,run,resume,trim,synctarget,doc}.go` and their tests; `internal/assembler/synctarget.go` and the sync-op surface; `job.AckDurable`, `SeedFromRuns`, `ReplaceFromRuns`, `RestoreFileMeta`; `history.RetainedFiles` and `history_job_files`; `durable_runs`, `failed_articles` and `job_files.assembled_crc32` from `001_initial.sql` and `schema.golden`; `Store.FailedArticles`; the old-table reads in `test/crash/harness.go`.
- [ ] `go run ./scripts/mutate --check-all`: delete the specs whose package is gone (`internal/checkpoint/testdata/*`, `internal/durability/testdata/{commit_fault,step2_store,step4_liveness,resume_discard_fault,resume_read_fault}.spec`); re-anchor `step3_reclaim` and the reclaim-related `internal/app/testdata` specs; delete the rest that pinned the barrier. Run each re-anchored spec.
- [ ] Gates; commit `refactor!: delete the durability barrier, checkpointer and resumer`.

### Task 5.3: Config and comments (commit 3)

- [ ] Delete `checkpoint_interval` and `checkpoint_bytes` (`internal/config/{downloads,validate,defaults}.go`, `internal/constants/limits.go`, `gonzbd.yaml.example`, `test/fixtures/gonzbd.yaml`, the UI field), `WithCheckpointInterval`/`WithCheckpointBytes` and their test uses, and the crash options that set them.
- [ ] Rewrite the `bytes_durable` comments in `internal/api/queue.go`, `internal/app/statusinfo.go` and `internal/app/history_helper.go` to say "written".
- [ ] `go test ./internal/config/ -run 'TestUI|TestAllFlat'`; gates; commit `refactor(config)!: drop checkpoint_interval and checkpoint_bytes`.

### Task 5.4: Docs (commit 4)

- [ ] Rewrite `docs/durability-contract.md` around spec §3.4 and §3.5, including the quickcheck-soundness paragraph at NN1.
- [ ] Read `docs/ARCHITECTURE.md` in full and rewrite every passage on the barrier or checkpoints.
- [ ] Sweep: `git grep -n -E 'durable_runs|failed_articles|history_job_files|checkpointer|[Bb]arrier|assembled_crc32|bytes_pending' -- docs AGENTS.md` must return only lines that describe the change historically, if any. Fix `docs/sabnzbd_spec.md` §9 and its history-retention bullet.
- [ ] Fold this spec into the contracts and delete it and this plan from `docs/superpowers/`, as commit 5a222fe5 did for the earlier specs.
- [ ] `go run ./scripts/check_doc_citations`, `go run ./scripts/check_citations`; commit `docs: describe the loose article record`.
- [ ] Run `pr-review-toolkit:comment-analyzer` over the cumulative PR diff, and `quality-lenses` in `diff` mode; triage before pushing.

---

## Follow-ups (not in this plan)

Each is optional and gets its own issue if wanted: off-tick verification gate;
a failed-article column on the `job_files` flush; updating `job_files.filename`
at par2 relocation; a fresh job ID on retry.
