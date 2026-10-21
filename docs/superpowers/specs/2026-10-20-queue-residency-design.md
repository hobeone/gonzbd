---
ArtifactMetadata:
  RequestFeedback: true
  UserFacing: true
---

# Queue residency: every job shows its state after a restart

**Status: approved 2026-10-20; implementation in progress.** Option C of
the appendix, taken to full design on 2026-10-20. The appendix keeps the
options that were weighed and why the others lost. Judgement calls made
while building it are recorded in
`2026-10-20-queue-residency-decisions.md` beside this file.

## 1. Intent

After a restart the queue shows, for every job, what it showed before: the
file list, per-file and per-job progress, and the repair verdict. It does
this without opening a job's article table or reading its partial files
back from the device. Where a figure comes from the download record rather
than from bytes checked on disk, the UI says so, and the figure is replaced
by the checked one when the job next runs.

The occasion was a job paused for an unwanted extension that came back from
a restart with no file list and "Repair data unknown". A narrow fix
(widening the startup verifier to never-run paused jobs) was written and
then withdrawn in favour of this design, which deletes that verifier. A
restart probe showed every job the tick had not yet hydrated in the same
state: a queued
half-downloaded job waiting for a lease, and a job paused anywhere past
`Fetching`. The two prior-art downloaders both keep the file table and
per-file counters resident for every job and load per-article data only to
fetch (SABnzbd `nzb/file.py:153-165`; NZBGet `DiskState.cpp:2031`,
`QueueCoordinator.cpp:588-591`). This design adopts that split.

## 2. Goals, non-goals, anti-goals

**Goals**

- Every registered job has its file table and a `JobProgress` from the
  moment restore completes, before the API listens.
- A restored job's progress is shown from the record, marked *recorded*
  until verified; the marking is per file, derived to the row.
- Resident memory per idle job is bounded by its file count, not its
  article count.
- The lifecycle gets simpler: the startup verifier, the progress-less
  window and a persisted derived column are deleted; the fallible manifest
  surface shrinks to the fetch path.

**Non-goals**

- Changing *when* the device read-back runs. It stays at the job's first
  hydration in a process, as today. A background pass that verifies idle
  jobs is a pure addition on top of this design and is not part of it.
- Showing per-article state for an unverified job. Display is file-level.
- Any change to history, post-processing, or the SABnzbd-compatible fields
  of `mode=queue`.

**Anti-goals** (what would make this a failure even if it worked)

- A dispatch, a state transition or an abort gate consuming a figure that
  came from the record rather than from a verified bit.
- A second persisted copy of the file table, or a second constructor for
  any of `FileTable`, `Manifest` or `JobProgress`.
- A migration or a dual-read path (Standing Rule 1 forbids both; a fresh
  database is assumed).

## 3. Decisions taken by the owner

- Progress may be shown from the record before verification, with an
  indicator. (2026-10-20)
- Persisted owner of the file table is SQLite (`job_files`), not the
  manifest file. (2026-10-20, option C over A.)

## 4. The model

### 4.1 Three tiers, by who reads them

| Tier | Holds | Resident | Persisted in | Readers |
|---|---|---|---|---|
| **Header** | identity, name, policy, four axes, intent, unwanted | always | `dispatch_jobs` | everything |
| **Files** | `FileTable` (per file: subject, bytes, is_par2, article_count) and `JobProgress` (per-file counters, per-article bitsets, verified flags) | always, from restore or ingest | `job_files` (+ `written_articles` for the seed) | queue listing, drawer, abort gates, post-processing, unwanted check, archive peek, **verification** |
| **Articles** | `Manifest`: Message-IDs, sizes, numbers, file offsets | while the job holds what its position requires, or is paused having held | the gzipped manifest file | fetch (`ForEachUnfinishedArticle`), byte accounting while fetching |

The rule that replaces today's "manifest is evictable, progress is not":

> **SQLite owns everything needed to show a job. The manifest file owns
> only what is needed to fetch it.**

### 4.2 Persisted shapes

`job_files` gains the NZB-derived structure of each file, authored by the
same admission writer §6 of the durability contract already names:

```sql
CREATE TABLE job_files (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id        TEXT NOT NULL,
    file_index    INTEGER NOT NULL,
    -- NZB-derived structure, written once at admission (Store.Admit).
    subject       TEXT NOT NULL,
    bytes         INTEGER NOT NULL,
    is_par2       INTEGER NOT NULL CHECK (is_par2 IN (0, 1)),
    article_count INTEGER NOT NULL CHECK (article_count >= 0),
    -- Download results, written by the recorder (unchanged).
    complete      INTEGER NOT NULL DEFAULT 0,
    filename      TEXT,
    fetch_policy  INTEGER NOT NULL DEFAULT 0 CHECK (fetch_policy BETWEEN 0 AND 2),
    UNIQUE(job_id, file_index)
);
```

`dispatch_jobs.recovery_bytes` is **deleted**: its comment gives "a
non-resident job has no manifest" as its only reason, and that state no
longer exists. `RecoveryBytes` and `RecoveryFiles` are derived from the
file table on the `Job`. `dispatch_jobs.bytes` stays (it is the header's
figure and feeds history).

The manifest file becomes a flat article array with **no file structure**:

```json
{"articles":[{"id":"<msgid>","bytes":12345,"number":1}, ...]}
```

File boundaries are the prefix sum of `article_count`, which is also what
`JobProgress.fileStarts` is today. A `Manifest` is constructed only as
`newManifest(ft *FileTable, articles []JobArticle)`, which builds the
offsets from `ft` and refuses `len(articles) != ft.TotalArticles()`. That
refusal is the one integrity check between the two stores, and it replaces
`describesSameJobAs`.

`date` is dropped from the file table: `FileDate` has no reader
(`git grep -n '\.FileDate(' -- '*.go' ':!*_test.go'` returns 0 lines on
2026-10-20).

### 4.3 In-memory shapes and their single owners

```go
// FileTable is immutable once built. One constructor.
type FileTable struct {
    files []fileEntry // subject, bytes, isPar2, articleCount
    starts []int      // prefix sum, len(files)+1
    totalBytes, recoveryBytes int64
    recoveryFiles int
}
func newFileTable(files []FileMeta) *FileTable

// Manifest is the article table. One constructor; it takes the file table
// so offsets have one owner.
func newManifest(ft *FileTable, articles []JobArticle) (*Manifest, error)

// JobProgress is sized from the file table. One constructor.
func newJobProgress(ft *FileTable, seed []FileSeed) *JobProgress
```

`FileProgress` gains two fields:

```go
// Verified is true once this process has installed this file's
// verification (InstallFileVerification), and false from a restore seed
// until then — for EVERY restored file, including one with complete=1 and
// one with no rows. A complete=1 file has no Done bits, no failed set and
// no settled CRC until the install puts them there (verification.go), and
// a row-less file still owes verification its DeleteAll verdict and the
// unrecorded-file cleanup. A freshly ingested file is Verified: nothing
// was recorded, so there is nothing to install.
Verified bool
// Recorded is the number of this file's articles the record holds a row for,
// read at restore. Meaningful only while !Verified; display derives an
// estimate from it (see 4.5). Never maintained live; replaced by the bits.
Recorded int
```

`Job` holds `files *FileTable`, `progress *JobProgress`, `manifest
*Manifest`. The doors:

| Door | Writes | Called from | Replaces |
|---|---|---|---|
| `AttachFiles(ft, seed)` | `files`, `progress` (both exactly once); seeds the download stamps and par2 release reason from the `Job`'s `restored*` fields and zeroes them, exactly as `AttachContent` does today | ingest (`BuildIngestJob`, seed nil) and the restore pass | `AttachContent`'s progress half, `SetRecoveryBytes` |
| `AttachArticles(m)` | `manifest`; then `progress.recompute(m)` | ingest (right after `AttachFiles`) and hydration | `AttachContent`'s manifest half, `RestoreContent` |
| `Evict()` | `manifest = nil` | tick, Remove, Stop | unchanged |
| `InstallFileVerification(v)` | the file's bits, counters, `Verified = true` | hydration | unchanged, plus the flag |

`TestProgressPointerWriters_MatchTheEnumerationStatedInProse` is retargeted
to `AttachFiles`; a sibling pins `AttachArticles` as the sole writer of
`manifest` and `AttachFiles` as the sole writer of `files`.

`Job.Manifest() (*Manifest, error)` stays the fallible door and now guards
only the article table. Every file-level accessor (`NumFiles`,
`FileSubject`, `FileBytes`, `FileIsPar2Recovery`, `FileRange`, `TotalBytes`,
`RecoveryBytes`, `RecoveryFiles`) moves to the `Job`, infallible. The
compiler enumerates the call sites: 18 `FileSubject`, 8 `FileBytes`,
47 `NumFiles` across non-test `.go` files including `internal/job` itself
(`git grep` on 2026-10-20; re-run before editing). `manifestGateExempt` shrinks to the methods that still read the
article table.

### 4.4 Restore

`Dispatcher.StartWith(ctx, beforeFirstTick)` already runs an app-supplied
function synchronously after every job is registered and before the first
tick (`reconcileBeforeFirstTick`). The restore pass goes there:

1. One query over `job_files LEFT JOIN` a `GROUP BY (job_id, file_idx)`
   aggregate of `written_articles` (`COUNT(*)` only — no row is
   transferred), for every job in the queue.
2. For each registered job: `AttachFiles(newFileTable(rows), seed)` where
   `seed[i] = {Recorded: count, Complete, Filename, FetchPolicy, Verified:
   false}`. The download stamps and par2 release reason reach the `Job`
   as they do today — `dispatch.reconstruct` sets the `restored*` fields —
   and `AttachFiles` seeds the fresh record from them; no new path between
   `internal/dispatch` and `internal/app` is needed.
3. A job whose structure cannot be read — no `job_files` rows, rows not
   covering `0..n-1`, or a row whose structure columns fail to scan
   (`FileRows` reports a skipped row through `ErrIncomplete`) — cannot be
   shown or run: it is settled `Failed` with an operational error naming
   the gap, as an unreadable manifest is today at its first tick. Same
   class — a job whose persisted structure is gone — moved from tick time
   to restore time. A scan failure on a *results* column (`complete`,
   `filename`, `fetch_policy`) alone is not reachable: `FileRows` scans the
   row as a unit.
4. When the application has no history database (`app.durable == nil`),
   there is no dispatch store either (`app.go:452-466` builds both under
   one condition), so nothing is restored and the pass has nothing to do.

Nothing writes the record between `restore` and the first tick (the
recorder starts with the assembler, after `StartWith` returns), so the pass
reads a consistent snapshot without needing a transaction across the two
stores.

`durability.FileRow` is widened with the four structure columns and remains
the one reader type for `job_files`; `installVerification` keeps taking it.

`Dispatcher.List`/`Row` lose the header-bytes fallback: every registered
job has progress by the time anything lists it.

### 4.5 What an unverified figure is

The record's `length` is decoded payload; `FileProgress.Bytes` and
`BytesDownloaded` are the NZB's encoded figure, a few percent higher. The
two cannot be subtracted (the field comment at `progress.go:174-194` says
so). So no byte figure is seeded. Instead, for a file with `!Verified`:

```
estimatedDownloaded(fi) = Bytes(fi) × Recorded(fi) / ArticleCount(fi)
```

Articles of one file are equal-sized except the last, so the error is
bounded by one article's bytes per file, and it is in the right unit. One
accessor, `JobProgress.fileBytesDownloaded(fi)`, returns the estimate when
`!Verified` and `BytesDownloaded` otherwise; `RemainingBytes`,
`ProgressFigures`, the slot's `percentage` and the drawer's per-file
`bytes_downloaded` all read through it. Nothing else branches on
`Verified`.

Per-article bits are **not** seeded. `pendingArticles`, `Pending`,
`failedBytes` and `articlesFailed` keep their zero-progress values until
`recompute` runs at `AttachArticles`. Consequences, each deliberate:

- Durability §3 — *a Done bit after a restart exists only because a device
  read matched its row's CRC, or its file was complete=1* — stays true
  **verbatim**. The design adds a file-level recorded count beside the bits;
  it does not loosen the bits.
- `repair_state` for an unverified job is computed from zero failures and
  is therefore `intact` (or `unknown` for the par2-naming reason). That is
  honest given the row-level *recorded* badge; the earlier `not_loaded`
  proposal is withdrawn because no registered job is ever without a file
  table now.
- `IsComplete` reads `Complete`, seeded from `job_files.complete` — a
  durable fact written after the file's fsync (§2). The *flag* is therefore
  trustworthy on the seed, but the file's bits, failed set and settled CRC
  are not yet installed (4.6), so a restored complete job must still be
  hydrated before anything acts on it. That holds **positionally**, and the
  PR must enumerate it rather than assume it: every reader of `IsComplete`
  outside `internal/job` runs on a job that has been hydrated —
  `reportDownloadComplete` on the Fetching worker (`app.go:1782`), the
  admission predicate reached through `admit`/`admitUnlessAssessing` from
  worker and fault paths (`postproc_admission.go:207`), and `app.go:2418`
  — because worker launch follows the tick's hydration. The comment at
  `app.go:1780` ("IsComplete reads true only for a job with progress") is
  falsified by this design and is on the sweep list. Outside `internal/job`,
  re-derived on `main` at the time of writing: `FileProgress.Pending` has no
  reader; `PendingArticles` has one (`internal/api/queue.go:516`, display);
  `HasProgress` has five, four for display (`internal/api/queue.go:474,515`,
  `internal/dispatch/registry.go:671,718`) and one gate, the hydration branch
  at `internal/app/residency.go:153`, which stage 2 replaces with the
  `Verified` test (4.6).

### 4.6 Hydration: verify, then correct

`appResidency.Hydrate(id)` for a job whose article table is not resident:

1. `readManifest` → `newManifest(j.Files(), articles)`; a count mismatch is
   an unreadable-manifest error (settles `Failed` at the tick, as today).
2. If any file has `!Verified` — which is every file of a restored job at
   its first hydration in the process, complete=1 and row-less ones
   included: `readRecord`, `verifyJobFiles`, `commit` verdicts,
   `cleanUnrecordedFetchingFiles` — all unchanged — then `AttachArticles(m)`
   and `InstallFileVerification` for **every** file, including one whose
   rows verification deleted entirely (an empty install), so that after a
   hydration every file is `Verified`. The order is mandatory:
   `verifyJobFiles` reads only file-level fields and takes the
   `*FileTable`, but `placeRows`, `installRows` and `settleFileCRC` need
   the article table (`verified.go`), so the install follows the attach.
3. Else (every file verified this process, evicted since):
   `AttachArticles(m)` alone. This is today's `RestoreContent` branch,
   keyed on `Verified` rather than `HasProgress`.

`fileOwedUnwantedFailures` hydrates inside `StartWith`; its job takes
step 2 like any other first hydration.

`InstallFileVerification` sets `Verified = true` and zeroes `Recorded` as
its last step; it is the only writer of the flag's `true`.

### 4.7 Residency rule

Unchanged in substance, restated for the article table:

> The article table is hydrated when the job holds what its position
> requires, and evicted when it does not and is not paused. A paused job
> keeps an article table it already has.

`verifyPausedJobs` is deleted. Its purpose was to make a paused job's
progress visible without a resume; every job's progress is now visible from
restore. The limitation-1 text in the durability contract that describes it
is deleted with it.

### 4.8 API and UI

Slot (`internal/api/queue.go`, `queueSlot`): add
`progress_verified: bool` — true iff every file is `Verified`. Per-file
(`queueFile`): add `verified: bool`.

UI (`QueueRow.svelte`): when `!progress_verified`, the row's progress bar is
outlined/hatched rather than filled, with the hint *"Progress from the
download record; it is checked against the files when the job next runs."*
The drawer shows an unfilled dot (or equivalent) on each unverified file.
Add cases to `HEALTH_LABELS`-style tables; no arithmetic in the UI
(`QueueRow.svelte`'s own rule). `files` is always populated for a
registered job; "No files in this job." remains only for a job with zero
files.

`ui/src/lib/types.ts` mirrors the two new fields; the Svelte unit test
covers both renderings.

### 4.9 Ingest

`BuildIngestJob` builds `ft := newFileTable(...)` from the parsed NZB, calls
`j.AttachFiles(ft, nil, zeroStamps)`, then `m, err := newManifest(ft,
articles)` and `j.AttachArticles(m)`. `seedJobFiles → Store.Admit` writes
the widened rows (subject, bytes, is_par2, article_count, fetch_policy),
still one transaction, still before `Dispatcher.Add`. A retry re-seeds with
`ON CONFLICT DO NOTHING`; it rebuilds from the same NZB backup, so the
structure cannot differ — say so at `Admit` rather than guard it.

`writeJobManifest` marshals the flat article array. `scripts/localnntp/
verify.go` reads only `NumArticles`/`ArticleID`; give it
`job.DecodeArticles(io.Reader) ([]JobArticle, error)` and it needs no file
table.

## 5. What is deleted

- `verifyPausedJobs`, `defaultPausedVerifyTimeout`, `pausedVerifyTimeout`,
  `pausedVerifiedHook`; `paused_verify_test.go` (10 tests);
  `testdata/paused_verify.spec`; the paused-verify mutation in
  `testdata/loose_record_waits.spec`.
- `SetRecoveryBytes`; `AttachContent` and `RestoreContent` themselves
  (replaced by the two doors in 4.3). The `restored*` fields and their
  seeding **stay**: they are how `dispatch.reconstruct` hands the stamps to
  the `Job` without `internal/dispatch` touching progress, and `AttachFiles`
  consumes them where `AttachContent` did. What disappears is the *window*
  in which they are the only copy — it now closes before the first tick.
- `JobProgress.describesSameJobAs` (replaced by `newManifest`'s refusal).
- `dispatch_jobs.recovery_bytes` and its load/persist in
  `internal/dispatch/store`; `dispatch.Persisted.RecoveryBytes`.
- The header-bytes fallback in `Dispatcher.List`/`rowFor`.
- `Manifest`'s file-level fields and accessors, `manifestFile`,
  `manifestJSONFile`, `JobFile` (the per-file NZB shape; `FileMeta` +
  `JobArticle` remain).
- `JobProgress.fileStarts`'s own construction — it becomes a reference to
  `FileTable.starts`.

## 6. Invariants and how each is pinned

| Invariant | Pin |
|---|---|
| Every registered job has `files` and `progress` after `StartWith` returns | app test: restart over a queue of {paused-never-run, paused-Fetching, queued-partial, paused-Assessing, paused-Extracting, running}, assert `NumFiles() > 0` and `HasProgress()` for each with lease cap 1 (the probe from 2026-10-20, kept as a test) |
| `AttachFiles` is the sole writer of `files` and `progress`; `AttachArticles` of `manifest` | the enumeration tests (retargeted + one new) |
| `newManifest` refuses a count mismatch | unit test + mutate spec neutering the check |
| No Done bit is set by the restore seed | unit test: after seed, `done.Count() == 0`; mutate spec that seeds bits and requires red |
| Unverified estimate is in `Bytes` units and bounded | unit test on `fileBytesDownloaded` with `Recorded` = 0, k, n |
| After a hydration every file is `Verified` | app test over a job with a file whose rows verification deletes |
| `InstallFileVerification` is the sole writer of `Verified = true` | enumeration test on the assignment |
| No gate consumes an unverified figure | enumeration in the PR body, re-derived from source at the time: readers of `fileBytesDownloaded` (display only); of `ArticleDone`/`FileComplete` outside `internal/job` (`internal/api/queue.go` display; `internal/postproc/filelist.go` post-attach; `internal/app/app.go` `retryFileStates`, after `verifyRetry` has installed verification); of `IsComplete` (all on worker or fault paths of a hydrated job, 4.5); of `Pending`/`PendingArticles`/`HasProgress` (display, and the hydration branch 4.5 names) |
| A restored job's first hydration always runs the verification pass, complete=1 and row-less files included | app test: restore a job whose files are all complete=1, hydrate, assert the failed set and settled CRC are installed; mutate spec flipping the seed's `Verified` to true and requiring red |
| Schema | `schema.golden` updated; `TestAllFlat`/config contract unaffected |

Red-green discipline per `AGENTS.md` Step 2: each new pin gets a
`scripts/mutate` spec, and the restart test must be observed failing on
`main` before the restore pass lands.

## 7. Staging

Each step lands green on its own, as a PR.

1. **File table with one owner, no behaviour change.** `FileTable`,
   `newFileTable`, `newManifest(ft, articles)`, the flat manifest format,
   widened `job_files` + `Admit`, `AttachFiles`/`AttachArticles` doors,
   file-level accessors onto `Job`, `describesSameJobAs` deleted. Hydration
   still runs the full verify-and-attach on first load. Restore still leaves
   jobs header-only (the fallback stays one more step).
2. **Progress from restore.** The restore pass in `reconcileBeforeFirstTick`;
   `Verified`/`Recorded`; `fileBytesDownloaded`; hydration keyed on
   `Verified`; API fields; UI; `recovery_bytes` and the header-bytes fallback
   deleted. **This is where the user-visible change lands.**
3. **Delete the startup verifier.** `verifyPausedJobs` and everything in §5
   not yet removed.
4. **Contract sweep.** `durability-contract.md` §3 (reaffirmed, with the
   recorded-count paragraph added), §6 (`Admit` authors structure too),
   limitation 1 (deleted); `job-lifecycle.md` §11–13 (three tiers restated,
   the "one window" section deleted, memory table updated);
   `dispatch-contract.md`; `ARCHITECTURE.md` "Compiler-Enforced Residency";
   `go-standards.md`'s persistence paragraph; the `job_files` table comment;
   the `reportDownloadComplete` comment at `app.go:1780`. Re-read
   `test/crash` against §3 — it should need no change, since §3 does not
   move.

## 8. Left to the builder

- Exact Go names and file placement within `internal/job`.
- Whether `fileStarts` is a shared slice or a pointer to the `FileTable`.
- The SQL of the restore query (one statement vs two) and its index use;
  `written_articles`' primary key `(job_id, file_idx, art_idx)` makes the
  `GROUP BY` an index scan.
- The precise UI treatment of the outlined bar and the per-file dot, within
  the no-arithmetic rule.
- Which `manifestGateExempt` entries survive.

## 9. My calls, for the owner to check while reading

- `date` dropped from the file table (no readers).
- Every restored file starts `!Verified`, complete=1 and row-less ones
  included, so a job's first hydration in a process always runs the full
  verification pass. (Builder check found the earlier rule — complete=1
  seeded as verified — would have left such a file without its bits,
  failed set and CRC.)
- The `restored*` stamp fields stay as the hand-off from `reconstruct`;
  only the window in which they are the sole copy is deleted.
- A job whose `job_files` structure cannot be read or scanned at restore
  is settled `Failed` then, rather than left header-only until a tick.
- `durability.FileRow` is widened rather than a second reader type added.
- Read-back stays at first hydration; no background verifier. (Open
  question 2 from the options round; the owner did not choose, so the
  design keeps today's behaviour. A background pass remains a pure
  addition.)
- Unverified byte figures are a per-file proportional estimate in encoded
  units, not a sum of decoded row lengths.
- `repair_state` keeps its five values; `not_loaded` withdrawn.
- The unverified marking is per file, with the row badge derived.

---

## Appendix — options weighed (2026-10-20)

### What a job needs, and who needs it

| Data | Size / 20k articles, 100 files | Readers |
|---|---|---|
| Header | — | everything |
| File table | ~10 KB | listing, drawer, unwanted check, archive peek, post-processing, **verification** (`verifyJobFiles` reads only `FileBytes/NumFiles/FileRange/FileSubject`) |
| Per-article sizes | 160 KB | byte accounting while fetching |
| Message-IDs and numbers | ~1.4 MB | **fetch only** (`ForEachUnfinishedArticle` → `internal/downloader/dispatch.go:132`) |
| `JobProgress` | ~7.5 KB | all reporting; abort gates |
| `written_articles` rows | SQLite | verification; the restore seed |

### A — file table in the manifest head

Same runtime as C; the file table persists as the head of the gzipped
manifest and restore reads each job's head with a stop-early streaming
decoder. Rejected after costing: it needs a head-first format, a
`json.Decoder.Token` walk (a struct `Decode` cannot stop early), a
`readManifestHead` with the abandon-on-deadline pattern, a startup loop
opening N files with per-job failure handling, and a new behaviour for a
job whose head will not read before the API listens — ~150–200 lines and a
new failure class that C does not have. Its one advantage, a manifest file
readable on its own, is a debugging convenience.

### B — load everything, drop residency (SABnzbd-shaped)

Decode every manifest at startup; delete `appResidency`,
`reconcileResidency`, `ErrNotResident` and all fallible sites. Rejected:
resident memory and startup decode become unbounded by queue size (1000 ×
20k-article jobs ≈ 1.7 GB resident, ~2 GB of JSON inflated before the queue
shows). `docs/job-lifecycle.md` §13 measured and declined this trade.
NZBGet does not do it.

### C — file table in `job_files` *(chosen)*

As designed above. `job_files` is already seeded one row per file at
admission by the writer §6 names; C widens that row and strips the file
structure from the manifest. Restore is one SQLite snapshot. Mirrors
NZBGet's `files` summary cache beside per-file article files.

### D — widen the startup loader and label the gap

Keep the architecture; load every restored job with `verifyPausedJobs`;
add `repair_state: "not_loaded"`. Rejected: every job is still read back
from the device before it shows anything, memory per shown job is the full
manifest, nothing gets simpler. Baseline only.

### A versus C on the tenets

- **Rule 2, one owner:** both have one persisted copy. C's `article_count`
  is a property of the NZB once the manifest carries no file structure
  (with a nested manifest it would be a second copy — hence the flat
  array). C's `Σ count == len(articles)` check is the same shape as
  `describesSameJobAs` today.
- **Durability §1/§6:** C's columns are authored by the existing admission
  writer and leave by the existing reclaim rule. A touches neither. Equal.
- **Rule 1:** both change the manifest format; C also the schema; both
  permitted.
- **Decided architecture:** A keeps "the manifest is the job's structure";
  C moves to "SQLite owns what is needed to show a job, the manifest what is
  needed to fetch it" — the sharper rule, stated by reader.
