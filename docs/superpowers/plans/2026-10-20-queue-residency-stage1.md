# Queue Residency Stage 1 — File Table With One Owner

> **Executed 2026-10-20.** Where the code differs from this plan, the decision log (`docs/superpowers/specs/2026-10-20-queue-residency-decisions.md`, D8, D12, D13, D14) records the change.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give every job an immutable `FileTable` with exactly one constructor, persisted in `job_files`, and reduce the manifest file to a flat article array — with no behaviour change visible to a running job.

**Architecture:** `internal/job` gains `FileTable` (per-file subject, bytes, article count; derived recovery figures and offsets). `Manifest` becomes the article table and is constructed only from a `FileTable` plus articles. `Job` grows two doors, `AttachFiles` and `AttachArticles`, replacing `AttachContent`/`RestoreContent`. `job_files` gains `subject`, `bytes`, `article_count`, written by the existing admission writer; hydration builds the file table from those rows before verifying. Restore still leaves jobs header-only until hydration (that moves in stage 2).

**Tech Stack:** Go (version per `go.mod`), SQLite via `modernc.org/sqlite`, gzip-JSON manifests, `scripts/mutate` for red checks.

**Spec:** `docs/superpowers/specs/2026-10-20-queue-residency-design.md` §4.2, §4.3, §4.9, §5, §7 step 1. Decision log: `docs/superpowers/specs/2026-10-20-queue-residency-decisions.md` (append D5–D9 below in Task 0).

## Global Constraints

- Standing Rule 2: one constructor per type. `newFileTable`, `newManifest`, `newJobProgress` are each the sole place their type's derived state is computed; `BuildContent` is a wrapper, not a constructor.
- Standing Rule 1: no migration, no dual-read. `001_initial.sql` and `schema.golden` are edited in place.
- Durability §3 is untouched in this stage: hydration still verifies before any Done bit is installed.
- `internal/dispatch` must not name `job.Manifest` (`TestDispatchNamesNoManifestType`); it does not name `job.FileTable` either.
- Every `.go` edit: `goimports -w`, `go fix ./...`, `go build ./...`.
- Every new pin gets a `scripts/mutate` spec under the package's `testdata/`, observed KILLED.
- Commit messages: Conventional Commits, scope = package name, body cites the observed red message.
- Never `git stash`.

## Review Focus

1. **A manifest whose article count disagrees with its `job_files` rows** — `NewManifest` must refuse it and hydration must treat the refusal as an unreadable manifest (settles `Failed` at the tick), never attach a misaligned table. Test in Task 2.
2. **A Message-ID read from disk carrying CR/LF** — the security re-check in today's `UnmarshalJSON` must survive the move to `DecodeArticles`; a CRLF ID refuses the whole manifest. Test in Task 2.
3. **A retry re-seeding `job_files`** — `ON CONFLICT DO NOTHING` keeps the original structure row; the test must show a second `Admit` with the same structure leaves `complete`/`filename` intact and does not duplicate. Test in Task 3.
4. **A residency with no record store hydrating a job that has no file table** — must return an error, not a nil-pointer panic. Test in Task 5.
5. **`AttachArticles` given a manifest built from a different `FileTable`** — must refuse (pointer inequality), so the job's offsets and the manifest's can never diverge. Test in Task 2.

---

### Task 0: Record the stage-1 decisions

**Files:**
- Modify: `docs/superpowers/specs/2026-10-20-queue-residency-decisions.md`

- [ ] **Step 1: Append D5–D10 under a new `## 2026-10-20 — stage 1` heading**

Entries, each with Call / Why / Cost:

- **D5 — `is_par2` is not a column.** `IsRecoveryVolume(subject)` is a pure function of `subject` (its regex requires `.par2`), so storing the flag would be a persisted derived value. `newFileTable` computes it; `job_files` carries `subject`, `bytes`, `article_count` only.
- **D6 — `date` dropped from `JobFile` and the manifest JSON.** `FileDate` has no reader; the field is deleted rather than carried.
- **D7 — `Manifest` keeps `NumFiles()` and `FileRange()` as forwarders to its file table; the content accessors (`FileSubject`, `FileBytes`, `FileIsPar2Recovery`, `TotalBytes`, `RecoveryBytes`, `RecoveryFiles`) move to `FileTable`, reached as `m.Files()` or `j.Files()`.** Why: `FileRange`/`NumFiles` have ~25 internal callers that hold a `*Manifest` for article work; the content accessors are what callers hold a manifest *only* to read.
- **D8 — `NewManifest(ft, articles)` is the constructor; `BuildContent([]JobFile) (*FileTable, *Manifest, error)` is the one wrapper over `newFileTable`+`newManifest` for ingest and tests; `AttachArticles` refuses a manifest whose `Files()` is not the job's own pointer.**
- **D9 — `SetRecoveryBytes` and `dispatch_jobs.recovery_bytes` stay until stage 2.** Deleting them now would make a restored job report 0 recovery bytes before hydration — a behaviour change stage 1 must not make.
- **D10 — Stage 1 is executed subagent-driven in a worktree, implementers on Sonnet.** Why: nine tasks whose interfaces depend on each other, and a mistake lands in the durability core. Cost: a fresh context per task and per review.

- [ ] **Step 2: Commit**

```bash
git add docs/superpowers/specs/2026-10-20-queue-residency-decisions.md
git commit -m "docs: record the stage-1 residency decisions"
```

---

### Task 1: `FileTable` with one constructor

**Files:**
- Create: `internal/job/filetable.go`
- Test: `internal/job/filetable_test.go`
- Spec: `internal/job/testdata/filetable.spec`

**Interfaces:**
- Produces:
  ```go
  // FileSpec is the per-file NZB-derived structure newFileTable takes.
  type FileSpec struct {
      Subject      string
      Bytes        int64
      ArticleCount int
  }
  type FileTable struct { /* unexported */ }
  func NewFileTable(files []FileSpec) *FileTable
  func (t *FileTable) NumFiles() int
  func (t *FileTable) Subject(fi int) string
  func (t *FileTable) Bytes(fi int) int64
  func (t *FileTable) IsPar2Recovery(fi int) bool   // IsRecoveryVolume(Subject(fi)), computed once in the constructor
  func (t *FileTable) ArticleCount(fi int) int
  func (t *FileTable) Range(fi int) (lo, hi int)    // prefix sum, [lo, hi)
  func (t *FileTable) TotalArticles() int
  func (t *FileTable) TotalBytes() int64
  func (t *FileTable) RecoveryBytes() int64
  func (t *FileTable) RecoveryFiles() int
  func (t *FileTable) Specs() []FileSpec             // copy, for Admit and tests
  ```
  `NewFileTable` is exported and calls the unexported `newFileTable`, the sole place the derived fields (offsets, totals, recovery figures, isPar2Recovery) are computed. Move `recoveryFigures` here from `manifest.go`, retyped over `FileTable`'s entries.

- [ ] **Step 1: Write the failing tests**

```go
func TestNewFileTable_DerivesOffsetsAndTotals(t *testing.T) {
    ft := NewFileTable([]FileSpec{
        {Subject: "a.bin", Bytes: 300, ArticleCount: 3},
        {Subject: "a.vol00+01.par2", Bytes: 50, ArticleCount: 1},
        {Subject: "a.par2", Bytes: 10, ArticleCount: 1},
    })
    // Range(1) == (3,4); TotalArticles()==5; TotalBytes()==360
    // RecoveryBytes()==50; RecoveryFiles()==1 (the index .par2 is not recovery)
    // IsPar2Recovery(1)==true, IsPar2Recovery(2)==false
}
func TestNewFileTable_EmptyIsValid(t *testing.T) { /* zero files: NumFiles 0, TotalArticles 0, Range panics are not tested */ }
func TestFileTable_SpecsIsACopy(t *testing.T) { /* mutate the returned slice; table unchanged */ }
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -count=1 -run 'TestNewFileTable|TestFileTable' ./internal/job/`
Expected: FAIL, `undefined: NewFileTable`

- [ ] **Step 3: Implement `filetable.go`**

Header comment states: immutable after construction; `newFileTable` is the only writer of every field; `IsRecoveryVolume` is the one predicate for recovery capacity (move the "conventionally" paragraph from `recoveryFigures`' comment here).

- [ ] **Step 4: Run to verify they pass**

Run: `go test -count=1 -run 'TestNewFileTable|TestFileTable' ./internal/job/`
Expected: PASS

- [ ] **Step 5: Write `testdata/filetable.spec`** — two mutations: `RecoveryFiles` counting every `.par2` (neuter the `IsRecoveryVolume` call to `isPar2File`), and the prefix sum off by one. Run `go run ./scripts/mutate internal/job/testdata/filetable.spec`; expected: all KILLED.

- [ ] **Step 6: Commit**

```bash
git add internal/job/filetable.go internal/job/filetable_test.go internal/job/testdata/filetable.spec
git commit -m "feat(job): add FileTable, the resident per-file structure with one constructor"
```

---

### Task 2: `Manifest` becomes the article table; `Job` gets `AttachFiles`/`AttachArticles`

This task must land as one compiling commit for `internal/job` (the package's tests are its only consumer until Task 4). Do it in three sub-steps that each build.

**Files:**
- Modify: `internal/job/manifest.go` (whole file)
- Modify: `internal/job/content.go:20-130` (doors), `:541-555` (`NumFiles`)
- Modify: `internal/job/job.go:213-215` (add `files *FileTable` beside `manifest`/`progress`)
- Modify: `internal/job/progress.go:329-354` (`fileMetaFromManifest` → `fileMetaFromTable`), `:855-890` (`m.FileIsPar2Recovery`/`m.FileSubject` → `m.Files().X`), `:984-998` (delete `describesSameJobAs`), `:1076` (`m.FileBytes` → `m.Files().Bytes`)
- Modify: `internal/job/content.go:47-49, 95-97, 458, 777-780` and `internal/job/verified.go`, `verification.go` content-accessor uses
- Modify: `internal/job/filesort.go` (drop `Date` from `JobFile`)
- Modify tests: `manifest_owner_test.go` (rewrite), `writer_enumeration_test.go:300-335`, `manifest_gate_test.go:23-27`, `content_test.go`, every `_test.go` in `internal/job` that calls `NewManifest`/`AttachContent`
- Create: `internal/job/content_helpers_test.go` helper `attachFiles(t, j, files []JobFile) (*FileTable, *Manifest)` (unexported; this package's tests cannot import `jobtest`)
- Spec: `internal/job/testdata/manifest_owner.spec`

**Interfaces:**
- Consumes: `FileTable`, `FileSpec` from Task 1.
- Produces:
  ```go
  type JobFile struct { Subject string; Articles []JobArticle; Bytes int64; IsPar2Recovery bool; Deferred bool } // Date deleted
  func NewManifest(ft *FileTable, articles []JobArticle) (*Manifest, error) // refuses len(articles) != ft.TotalArticles()
  func BuildContent(files []JobFile) (*FileTable, *Manifest, error)          // the wrapper: NewFileTable(specs) then NewManifest
  func DecodeArticles(r io.Reader) ([]JobArticle, error)                     // flat JSON {"articles":[...]}, re-checks nzb.MessageIDIsFetchable
  func (m *Manifest) MarshalJSON() ([]byte, error)                           // writes {"articles":[...]}; no UnmarshalJSON
  func (m *Manifest) Files() *FileTable
  func (m *Manifest) NumFiles() int           // forwards to Files()
  func (m *Manifest) FileRange(fi int) (lo, hi int) // forwards to Files().Range
  // deleted from Manifest: FileSubject, FileBytes, FileDate, FileIsPar2Recovery, TotalBytes, RecoveryBytes, RecoveryFiles, UnmarshalJSON
  func (j *Job) AttachFiles(ft *FileTable) error        // sole writer of j.files and j.progress; refuses a second call; seeds stamps from restored* and zeroes them (moved verbatim from AttachContent)
  func (j *Job) AttachArticles(m *Manifest) error       // sole writer of j.manifest; refuses j.files == nil, m.Files() != j.files, or j.manifest != nil; then progress.recompute(m)
  func (j *Job) Files() *FileTable                      // nil before AttachFiles
  // deleted: AttachContent, RestoreContent, JobProgress.describesSameJobAs
  ```
  `j.totalBytes/recoveryBytes/recoveryFiles` are now written by `AttachFiles` only (and `SetRecoveryBytes`, kept per D9). `newJobProgress(ft *FileTable)`.

- [ ] **Step 1: Write the failing tests** (in `manifest_owner_test.go`, rewritten, and `content_test.go`)

```go
func TestNewManifest_RefusesAnArticleCountTheTableDoesNotHave(t *testing.T) // 3-article table, 2 articles → error mentions both counts <!-- doccite:ok TestNewManifest_RefusesAnArticleCountTheTableDoesNotHave — landed as TestNewManifestFromArticles_RefusesAnArticleCountTheTableDoesNotHave -->
func TestManifestRoundTrip_FlatArticlesOnly(t *testing.T)                   // MarshalJSON → DecodeArticles → NewManifest(sameFT) equals original (IDs, bytes, numbers); the JSON contains no "subject" <!-- doccite:ok TestManifestRoundTrip_FlatArticlesOnly — landed as TestDecodeArticles_RoundTripsMarshalJSON -->
func TestDecodeArticles_RefusesACRLFMessageID(t *testing.T)                 // keep today's cases from TestManifestRefusesAnUnfetchableMessageIDFromDisk <!-- doccite:ok TestDecodeArticles_RefusesACRLFMessageID — landed as TestDecodeArticles_RefusesAnUnfetchableMessageID --> <!-- doccite:ok TestManifestRefusesAnUnfetchableMessageIDFromDisk — the pre-stage-1 test, replaced by the one above -->
func TestDecodeArticles_AcceptsOrdinaryMessageIDs(t *testing.T)
func TestAttachFiles_RefusesASecondAttach(t *testing.T)
func TestAttachArticles_RefusesAManifestFromAnotherTable(t *testing.T)      // two tables with identical specs; manifest built from ft2 attached to a job holding ft1 → error
func TestAttachArticles_RequiresFiles(t *testing.T)
func TestAttachArticles_RecomputesInPlace(t *testing.T)                     // MarkArticleFailed while evicted, Evict, AttachArticles → counters reflect the bit (today's RestoreContent test, retargeted)
```

Retarget enumeration tests: `TestProgressPointerWriters…` wants `[]string{"AttachFiles"}`; `TestRecoveryBytesWriters…` wants `[]string{"AttachFiles", "SetRecoveryBytes"}`; add `TestFilesPointerWriters_MatchTheEnumerationStatedInProse` (`scanWriters(t, "files")` → `[]string{"AttachFiles"}`; check `fieldOwner` in that file covers the new name) and `TestManifestPointerWriters_MatchTheEnumerationStatedInProse` (`[]string{"AttachArticles", "Evict"}`). Update `manifestGateExempt`: replace `Job.AttachContent`/`Job.RestoreContent` with `Job.AttachFiles` ("assigns files and progress; the manifest is not read") and `Job.AttachArticles` ("assigns the manifest; establishes residency"); `Job.NumFiles` entry becomes "reads files, never the manifest".

- [ ] **Step 2: Run to verify they fail**

Run: `go test -count=1 ./internal/job/`
Expected: FAIL to compile, `undefined: NewManifest` signature mismatch / `AttachFiles`

- [ ] **Step 3a: Add the new types and doors beside the old ones** — `Manifest` gains `ft *FileTable`; `newManifest(ft, articles)`; `AttachFiles`/`AttachArticles`/`Files()`; reimplement `AttachContent(m)` as `AttachFiles(m.Files())` + `AttachArticles(m)` and `RestoreContent(m)` as `AttachArticles(m)` so the package builds. `go build ./internal/job/`.

- [ ] **Step 3b: Migrate every in-package caller and test** to `BuildContent` + the two doors, using the `attachFiles` test helper. Change `newJobProgress(ft)`, `fileMetaFromTable(ft)` (`IsPar2: isPar2File(ft.Subject(fi))`), `recompute` (`m.Files().Bytes(fi)`), `RecoveryReleased`/`FetchedRecoveryBlocks` (`m.Files().IsPar2Recovery`, `.Subject`), `ForEachUnfinishedArticle` (`m.Files().Subject(fi)`), `Job.NumFiles` (`j.files` first, then progress). Delete `describesSameJobAs`. `go build ./internal/job/ && go vet ./internal/job/`.

- [ ] **Step 3c: Delete `AttachContent`, `RestoreContent`, `UnmarshalJSON`, `manifestFile`, `manifestJSONFile`, the content accessors, `JobFile.Date`.** Rewrite `manifest.go`'s header comment: Manifest is the article table; its file structure is the job's `FileTable`; `newManifest` is the sole constructor and refuses a count mismatch; `DecodeArticles` carries the Message-ID re-check with today's comment moved verbatim.

- [ ] **Step 4: Run the package tests**

Run: `go test -count=1 -race ./internal/job/...`
Expected: PASS

- [ ] **Step 5: Write `testdata/manifest_owner.spec`** — mutations: neuter the count check in `newManifest` (`if false {`); neuter the pointer check in `AttachArticles`; neuter the `MessageIDIsFetchable` check in `DecodeArticles`. Run `go run ./scripts/mutate internal/job/testdata/manifest_owner.spec`; all KILLED. Run `go run ./scripts/mutate --check-all`; fix any spec whose anchor moved (expect hits in `internal/job/testdata/*.spec` anchored on `AttachContent`/`RestoreContent` lines — re-anchor them on the new door).

- [ ] **Step 6: Commit**

```bash
git add internal/job
git commit -m "refactor(job): split Manifest into FileTable and article table behind AttachFiles/AttachArticles"
```
Body: name the deleted doors, cite the spec's KILLED lines.

---

### Task 3: `job_files` carries the file structure

**Files:**
- Modify: `internal/history/migrations/001_initial.sql:88-96` (columns) and `:60-87` (comment)
- Modify: `internal/history/testdata/schema.golden:9`
- Modify: `internal/durability/progress.go:19-24` (`FileRow`), `:43-75` (`Admit`), `:77-120` (`FileRows`)
- Test: `internal/durability/progress_test.go`
- Spec: `internal/durability/testdata/admit.spec` (create if the dir has none)

**Interfaces:**
- Produces:
  ```go
  type FileRow struct { FileIndex int; Subject string; Bytes int64; ArticleCount int; Complete bool; FetchPolicy uint8; Filename string }
  type AdmitFile struct { Subject string; Bytes int64; ArticleCount int; FetchPolicy uint8 }
  func (s *Store) Admit(ctx context.Context, jobID string, files []AdmitFile) error
  ```
  Schema: after `file_index`, add `subject TEXT NOT NULL`, `bytes INTEGER NOT NULL CHECK (bytes >= 0)`, `article_count INTEGER NOT NULL CHECK (article_count >= 0)`. `FileRows` selects and scans them.

- [ ] **Step 1: Write the failing tests** (extend `progress_test.go`)

```go
func TestStore_AdmitWritesTheFileStructure(t *testing.T)        // Admit 2 files; FileRows returns subject/bytes/article_count as given
func TestStore_AdmitAgainKeepsTheFirstRow(t *testing.T)         // Admit, UPDATE complete=1 by SQL, Admit same again → complete still 1, one row per index (Review Focus 3)
```
Keep `TestStore_AdmitSeedsAndKeepsExistingRows` and adapt its call to `[]AdmitFile`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test -count=1 -run 'TestStore_Admit' ./internal/durability/`
Expected: FAIL to compile (`[]uint8` vs `[]AdmitFile`)

- [ ] **Step 3: Edit the schema, the golden, `FileRow`, `Admit`, `FileRows`.** Rewrite the `job_files` comment block: the table now holds the file's NZB-derived structure (written once by `Admit`) and its download results (written by the recorder); delete the sentence "Anything the manifest already says does not belong here" and say instead that the manifest file carries articles only. Re-run the comment's embedded `git grep` and correct its count.

- [ ] **Step 4: Run**

Run: `go test -count=1 -race ./internal/durability/... ./internal/history/...`
Expected: PASS (the golden test compares the live schema to `schema.golden`)

- [ ] **Step 5: Spec** — one mutation: `Admit` writes `article_count` as 0. Observed KILLED by `TestStore_AdmitWritesTheFileStructure`.

- [ ] **Step 6: Commit**

```bash
git add internal/history internal/durability
git commit -m "feat(durability): persist each file's subject, bytes and article count in job_files"
```

---

### Task 4: Ingest builds the two tiers and seeds the structure

**Files:**
- Modify: `internal/app/ingest.go:118-150`
- Modify: `internal/app/app.go:783-790, 867-873, 2925-2930` (`seedJobFiles` and callers)
- Test: `internal/app/ingest_test.go` (or the existing test that covers `BuildIngestJob`; `git grep -n 'BuildIngestJob(' -- 'internal/app/*_test.go'`)

**Interfaces:**
- Consumes: `job.BuildContent`, `job.AttachFiles`, `job.AttachArticles`, `durability.AdmitFile`.
- Produces: `func seedJobFiles(ctx, st durabilityStore, j *job.Job) error` — builds `[]AdmitFile` from `j.Files().Specs()` and `j.FileFetchPolicy(i)`.

- [ ] **Step 1: Write the failing test**

```go
func TestBuildIngestJob_AttachesFilesAndArticles(t *testing.T) // j.Files() != nil, NumFiles == len(parsed.Files), Manifest() ok, Resident()
func TestSeedJobFiles_WritesTheStructure(t *testing.T)         // after AddJob over a real durability.Store, FileRows subjects == NZB subjects, article counts match
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -count=1 -run 'TestBuildIngestJob_AttachesFilesAndArticles|TestSeedJobFiles' ./internal/app/`
Expected: FAIL to compile

- [ ] **Step 3: Implement.** In `BuildIngestJob`: build `[]job.JobFile` as today minus `Date`; `ft, m, err := job.BuildContent(files)`; `j.AttachFiles(ft)`; `j.AttachArticles(m)`; header `Bytes: ft.TotalBytes()`. `seedJobFiles(ctx, st, j)` replaces the `(numFiles, fetch)` form at both call sites.

- [ ] **Step 4: Run**

Run: `go build ./... 2>&1 | head -40` — expect the remaining compile errors to be exactly the sites Tasks 5–6 own (`residency.go`, `verify.go`, `internal/api/queue.go`, `postproc`, `directunpack_orchestrator.go`, `unwanted.go`, `par2names.go`, `pipeline.go`, `archive_peek.go`, `jobtest`, `scripts/localnntp`). Do not fix them here. Run only `go test -count=1 -run 'TestBuildIngestJob|TestSeedJobFiles' ./internal/app/` once Task 5 has made `internal/app` compile — so **Task 4 and Task 5 are committed together** if the package will not otherwise build. Prefer: complete Task 5 then run both tasks' tests, commit as two commits only if each builds alone; otherwise one commit named for both.

---

### Task 5: Hydration builds the file table from the record, then verifies

**Files:**
- Modify: `internal/app/residency.go:104-200` (`Hydrate`, `verifyAndAttach`), `:361-408` (`readManifest`, `decodeManifest`)
- Modify: `internal/app/verify.go:106, 210, 499, 572` (`*job.Manifest` → `*job.FileTable`), `:591-595`
- Modify: `internal/app/app.go:2868, 2992` (`verifyRetry(ctx, j, ft)`)
- Modify: `internal/app/manifestpath.go:33-55` (`writeJobManifest` unchanged in shape; confirm `json.Marshal(m)` still produces the flat form)
- Tests: `internal/app/residency_manifest_test.go`, `residency_linux_test.go`, `loose_record_helpers_internal_test.go`, `verify_test.go`

**Interfaces:**
- Consumes: `job.DecodeArticles`, `job.NewManifest`, `job.AttachFiles`, `job.AttachArticles`, `durability.FileRow` (widened).
- Produces:
  ```go
  func (r *appResidency) readArticles(ctx, id string) ([]job.JobArticle, error) // replaces readManifest; same goroutine/ctx-abandon shape
  func fileTableFromRows(rows []durability.FileRow) (*job.FileTable, error)      // refuses rows not covering 0..n-1 exactly once each
  func verifyJobFiles(ctx, ft *job.FileTable, files []durability.FileRow, rows []durability.WrittenRow, locate ..., retry bool) (verifyResult, error)
  ```
  `Hydrate` order for a job with `Files() == nil`: `readRecord` → `fileTableFromRows` → `AttachFiles(ft)` → `readArticles` → `NewManifest(ft, arts)` (error ⇒ unreadable manifest, returned as today's decode error) → `verifyJobFiles(ft, …)` → `commit` → `cleanUnrecordedFetchingFiles` → `AttachArticles(m)` → `installVerification`. For a job with files and progress already (evicted since): `readArticles` → `NewManifest(j.Files(), arts)` → `AttachArticles(m)`. With `r.store == nil` and `Files() == nil`: return `fmt.Errorf("residency: hydrate %s: no record store to build the file table from", id)`.

- [ ] **Step 1: Write the failing tests**

```go
func TestHydrate_BuildsTheFileTableFromJobFilesBeforeVerifying(t *testing.T) // job with rows, no files; after Hydrate: Files().Subject(0) == admitted subject, Resident()
func TestHydrate_RefusesAManifestWhoseCountDisagreesWithTheRows(t *testing.T) // manifest file with one article fewer → error, !Resident(), not a residency fault (Review Focus 1)
func TestHydrate_WithoutAStoreNeedsAnAttachedFileTable(t *testing.T)          // r.store nil, Files() nil → error containing "no record store" (Review Focus 4)
func TestFileTableFromRows_RefusesAGap(t *testing.T)                           // rows {0,2} → error
```
Adapt `verify_test.go` fixtures: where a test built `m := job.NewManifest(...)` to pass to `verifyJobFiles`, build `ft, m, _ := job.BuildContent(...)` and pass `ft`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test -count=1 -run 'TestHydrate_|TestFileTableFromRows' ./internal/app/`
Expected: FAIL to compile

- [ ] **Step 3: Implement** per the Interfaces block. `decodeManifest` becomes `job.DecodeArticles(zr)`.

- [ ] **Step 4: Run**

Run: `go build ./... && go test -count=1 -race ./internal/app/...`
Expected: `internal/app` builds (other packages may still fail until Task 6); app tests PASS.

- [ ] **Step 5: Commit** (with Task 4 if they could not be separated)

```bash
git add internal/app
git commit -m "refactor(app): build the file table from job_files at hydration and verify against it"
```

---

### Task 6: Migrate the remaining readers to `Files()`

**Files:**
- Modify: `internal/api/queue.go:286-300, 354-370` (`buildQueueFiles`, `firstIncompleteFile`: take `j.Files()` for subject/bytes, keep `Progress()`; the `Manifest()` call there disappears)
- Modify: `internal/app/unwanted.go:117-126` (`j.Files()` — the `Manifest()` call and its error branch disappear)
- Modify: `internal/app/archive_peek.go:128`, `directunpack_orchestrator.go:66-69, 137-139`, `par2names.go:30, 48-49`, `pipeline.go:552-553, 616`, `app.go:2206`
- Modify: `internal/postproc/filelist.go:59, 200-218`, `stage_quickcheck.go:127-129` (`m.Files().X`)
- Modify: `internal/job/jobtest/markwritten.go:60-61` (`m.Files().Bytes`)
- Modify: `scripts/localnntp/verify.go:231-262` (`readGzipQueueManifest` returns `[]job.JobArticle` via `job.DecodeArticles`; index the slice)
- Modify: `scripts/nzbprobe/main.go:142` (comment only)
- Tests: `internal/api/queue_nonresident_test.go`, `internal/postproc/*_test.go`, `internal/job/jobtest/markwritten_test.go` — fixtures to `BuildContent`/`jobtest.Attach`
- Create: `internal/job/jobtest/attach.go` — `func Attach(t testing.TB, j *job.Job, files []job.JobFile) (*job.FileTable, *job.Manifest)` for out-of-package tests <!-- doccite:ok internal/job/jobtest/attach.go — superseded by decision D12 (AttachContent kept; no jobtest.Attach) -->

- [ ] **Step 1: Make the tree build**

Run: `go build ./... && go vet ./...` — fix each error at the site; where a caller held a `*Manifest` only for file-level reads and the job is in hand, switch to `j.Files()` and delete the `Manifest()` error branch (record in the commit body which sites became infallible: expected `unwanted.go`, `internal/api/queue.go` ×2, `directunpack_orchestrator.go:137`, `par2names.go`).

- [ ] **Step 2: Make the tests build and pass**

Run: `go vet -tags=integration,uitest,crash ./... && go test -count=1 -race ./...`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add -A -- internal scripts
git commit -m "refactor: read file structure from FileTable instead of the manifest"
```

---

### Task 7: Sweep the claims stage 1 falsified

**Files:**
- Modify: `docs/job-lifecycle.md:24-33` (§1 names `newManifest`/`UnmarshalJSON` as the worked example), `:1180-1200` (§11 tier table: the Manifest row now holds articles; the Files tier is new), `:1462-1480` (§12 "boundaries come from `Manifest.fileArticleOffsets`" → from the job's `FileTable`, persisted in `job_files`)
- Modify: `docs/durability-contract.md:726-732` (§6: `Admit` also writes the file's structure)
- Modify: `docs/ARCHITECTURE.md` (grep for `UnmarshalJSON`, `fileArticleOffsets`, `JobFile`)
- Modify: `internal/job/doc.go`, `internal/job/progress.go` comments naming `AttachContent`/`RestoreContent` (`git grep -n -E 'AttachContent|RestoreContent|describesSameJobAs|UnmarshalJSON|fileArticleOffsets|JobFile\b' -- '*.go' '*.md' '*.sql' ':!docs/superpowers'`)

- [ ] **Step 1: Grep and fix every hit** from the command above; for each prose claim, state what still holds rather than a new universal.
- [ ] **Step 2: Run the whole-repo gates**

```bash
go run ./scripts/check_citations && go run ./scripts/check_doc_citations && go run ./scripts/check_dup_comments && go run ./scripts/check_test_doubles --all && go run ./scripts/mutate --check-all
```
Expected: each reports a clean status.

- [ ] **Step 3: Commit**

```bash
git add docs internal
git commit -m "docs: describe the file table tier and the articles-only manifest"
```

---

### Task 8: Quality gates, review, PR

- [ ] **Step 1: Gates**

```bash
go fix ./... && goimports -w . && go vet ./... && go vet -tags=integration,uitest,crash ./... \
  && go test -race ./... && ./scripts/run_tests.sh && golangci-lint run ./...
go test -tags=integration ./test/integration/... ./internal/par2/...
```
Expected: all pass. `run_tests.sh` runs every `*.spec`; any ANCHOR verdict means a committed spec still points at moved code — fix the spec.

- [ ] **Step 2: Reviews** — `pr-review-toolkit:review-pr` on `git diff main...HEAD`; `pr-review-toolkit:comment-analyzer` on the same diff; `quality-lenses` in `diff` mode. Triage each finding; fix Critical/Important; record any deliberate rejection in the decision log.

- [ ] **Step 3: PR** — push `HEAD:refactor/queue-residency-stage1`, body via `file-pullreq` (math scan + cold reader, filed without the approval pause per D4). Body must carry: the enumeration of sites that became infallible, the KILLED lines from each new spec, and the sentence "no behaviour change for a running job; restored jobs are still header-only until hydration (stage 2)".
