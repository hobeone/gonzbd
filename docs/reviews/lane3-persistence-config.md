# Lane 3 — Persistence & Configuration Review

Scope: `internal/history/`, `internal/queue/{sqlite_store.go,store.go,persistence.go,manifest.go,active_set.go,queue.go}`, `internal/config/`.

> **Frozen record.** This is a dated audit snapshot of `main` @ `9be19e24`, not
> a living contract. Several identifiers and schema columns it describes have
> since been removed by the download-durability work — `MarkArticlesDone`,
> `MarkArticlesDoneByIdx`, `MarkArticlesFailed`, `SetFileExtents`, `maxWritten`,
> `crcParts`/`crcValid`, and the `job_files.write_cursor` / `bytes_downloaded` /
> `max_written` columns. It is kept unedited as a record of what the code was
> and what the audit found. For the current contract see
> [`../durability-contract.md`](../durability-contract.md).

## What the persistence layer actually is today

The review brief's premise needs two corrections, one of them load-bearing for the whole "two persistence mechanisms" framing:

1. **`docs/ARCHITECTURE.md` and `docs/sabnzbd_spec.md` are themselves stale on this point.** Both documents describe a separate `queue.db` SQLite file (`docs/ARCHITECTURE.md:87-88`, `docs/sabnzbd_spec.md:103,287-288,295,1265`). **There is no `queue.db` in the code.** `cmd/gonzbd/main.go:206,1022` opens exactly one SQLite file, `<AdminDir>/history.db`, via `history.Open`. `internal/app/app.go:194-195` wires the queue's `SQLiteStore` to `repo.DB()` — the *history* repository's own `*sql.DB` handle:
   ```go
   store := queue.NewSQLiteStore(repo.DB(), queueStateDir, repo)
   ```
   The `jobs`, `job_files`, and `queue_meta` tables live in the **same physical database file** as the `history` table, added via `internal/history/migrations/002_add_jobs_tables.sql` and `003_add_download_timestamps.sql` — migrations that live in the `history` package's migration directory despite being queue schema. So this is **one SQLite database with two logical table groups**, not two databases. This actually resolves the brief's central worry (a single transaction *can* span "queue" and "history" data, because they're the same file) — see Finding 1.

2. **The gzip-JSON manifest split is real and as described**: immutable per-job article/file structure (`Manifest`, `internal/queue/manifest.go`) is written to `<AdminDir>/queue/manifests/<id>.json.gz` (`internal/queue/sqlite_store.go:172-178`), separate from the mutable per-file progress counters (`complete`, `write_cursor`, `bytes_downloaded`, `articles_done` bitmap) stored as SQL columns in `job_files` (`internal/history/migrations/002_add_jobs_tables.sql`). This is the real two-store boundary in this codebase: **SQL rows vs. filesystem blob**, both anchored to the same SQLite file's transactions for the row side.

3. **A second, fully parallel persistence implementation still exists and is dead in production.** `internal/queue/persistence.go` contains a complete legacy whole-queue JSON+gzip implementation (`saveInner`, `Load`'s non-store branch, `indexFile`/`queue.json.gz`, per-job `jobs/<id>.json.gz`) that predates the SQLite store. It is only exercised when `q.store == nil`, which never happens on either production code path (`app.New` is always called with a non-nil `historyRepo` whose `.DB()` is non-nil — see `cmd/gonzbd/main.go:214,1033`). See Finding 9.

4. **Config** is a single in-memory `*Config` (`internal/config/config.go`) guarded by one `sync.RWMutex`, loaded from and saved to a single YAML file via `internal/config/loader.go`. `Set`/`SetLocked` (`internal/config/set.go`) mutate via reflection and re-run the same `Validate()` used at load time.

## Findings

### 1. `MoveToHistory`'s "single transaction" claim is true, but only because queue and history secretly share one file — and that coupling is nowhere documented
`internal/queue/sqlite_store.go:475-510`, `internal/app/app.go:194-195`. CONFIRMED.
`MoveToHistory` opens one `*sql.Tx` on `s.db` and passes it into `historyRepo.AddTx(ctx, tx, entry)` (`internal/history/repository.go:105`). This only works because `s.db` **is** `historyRepo`'s db (same `*sql.DB`, same file) — `queue.NewSQLiteStore(repo.DB(), ...)`. `go-standards.md`'s claim ("queue-to-history transitions execute atomically within a single database transaction") is therefore accurate, but the *reason* it's true (queue and history are the same SQLite file) is undocumented anywhere and looks, from `ARCHITECTURE.md` alone, like it shouldn't be possible. Anyone reading only the architecture doc would conclude this code has a cross-database-transaction bug; it doesn't, because the premise (two files) is wrong.
Direction: fix `docs/ARCHITECTURE.md`/`docs/sabnzbd_spec.md` to say "single `history.db` containing both `history` and `jobs`/`job_files` tables" and drop the `queue.db` name, or actually name the shared file something more accurate (e.g. `state.db`) if a rename is ever considered. Effort: S (docs only).

### 2. `docs/ARCHITECTURE.md:178`'s "byte-for-byte compatible" claim is at minimum incomplete, possibly stale
`docs/ARCHITECTURE.md:178`, `internal/history/db.go:1-4`. CONFIRMED (doc statement) / INFERRED (whether the Python schema itself still matches — not independently diffed against `../sabnzbd/` in this pass since it's out of this lane's primary evidence set).
The doc says "history.db... is designed to be byte-for-byte compatible with the original Python implementation's history database." The package doc comment in `internal/history/db.go:1-4` repeats this. But the same physical file now also carries `jobs`/`job_files`/`queue_meta` tables that have no Python analogue (SABnzbd used `queue10.sab` pickle, not SQLite, for the queue). "Byte-for-byte compatible" can only be true of the `history` table specifically, not of `history.db` as a file. This is a documentation-accuracy finding per the task's own callout.
Direction: scope the compatibility claim explicitly to the `history` table, not the file.  Effort: S.

### 3. `Get()` silently deletes the active-job row when its manifest is missing or corrupt
`internal/queue/sqlite_store.go:227-259`. CONFIRMED.
Three separate paths in `Get` call `s.Remove(ctx, id)` as a side effect of a **read**:
- `fileCount > 0` but `os.Stat(manifestPath)` fails (line 234-238) → removes the job.
- Manifest read (`readGzJSON`) fails for a resident-status job (line 244-248) → renames to `.corrupt`, then removes the job.
- (implicitly) any code path that calls `Get`/`List` during that window will trigger deletion.
For a resident job (queued/downloading), a corrupted or momentarily-missing manifest file causes the **entire download to vanish from the queue** — not just lose progress, but be forgotten outright, with no history entry recorded (this is not routed through `MoveToHistory`). The user loses visibility into what happened; the NZB has to be manually re-added if they still have it. Given single-user right-sizing this isn't multi-writer corruption, but real corruption (bad shutdown mid-`writeGzJSON`, disk error, manual tampering) is exactly the case this code path exists to handle, and it currently answers with silent data loss rather than a `Failed` history entry.
Direction: on manifest-read failure, prefer routing through the same "claim failure" pattern used elsewhere (`prepareClaimFailureLocked`/`finishClaimFailure` in `queue.go:627-700`) — mark `Failed`, write a history entry with a fail message, *then* remove — so the job is auditable in history instead of disappearing. Effort: M.

### 4. `SQLiteStore.Add` performs manifest disk I/O (gzip write with fsync) while holding the Queue's full write mutex
`internal/queue/sqlite_store.go:172-178` invoked from `internal/queue/queue.go:355-360` under `q.mu.Lock()`. CONFIRMED, but explicitly the documented exception (`//lockio:` comment at `queue.go:356`).
This is technically compliant with the project's own escape hatch (`go-standards.md` Lesson #1: "a genuine exception... is suppressed with a same-line `//lockio:` comment"), and the accompanying comment (`queue.go:351-354`) gives a real justification (TOCTOU-free name/insert). Flagging anyway because the *cost* of the exception is nontrivial: `store.Add` does an `INSERT INTO jobs`, N `INSERT INTO job_files` (one row per file, not batched into a single multi-row statement), a `MkdirAll`, and a full atomic gzip-write-with-fsync (`WriteAtomic`'s `tmp.Sync()`), all synchronously under the lock that also gates `GetArticles`, `MarkArticlesDone`, etc. For jobs with many files this could stall the dispatch hot path for longer than a typical add. Single-user right-sizing softens this (job adds are rare relative to article throughput), so this is reported as a small/positive-adjacent finding rather than a defect.
Direction: no action required given add frequency; if profiling ever shows dispatch stalls correlated with job adds, consider narrowing to writing the manifest before acquiring `q.mu` and only holding the lock for the SQL insert. Effort: N/A (informational).

### 5. Config `Set()` can leave the in-memory config ahead of the on-disk file if `Save()` fails
`internal/api/config.go:163-175`. CONFIRMED.
```go
if err := s.config.Set(section, keyword, value); err != nil { ... }
if s.configPath != "" {
    if err := s.config.Save(s.configPath); err != nil {
        s.log.Error("persist config", ...)
        s.respondError(w, http.StatusInternalServerError, "persist config: "+err.Error())
        return
    }
}
```
`Set` mutates `s.config` in place and immediately validates/applies (other code paths read `s.config.GetGeneral()` etc. live). If `Save` then fails (disk full, permission error), the API returns a 500 to the caller, but the **running daemon's in-memory config already reflects the new value** — a restart would silently revert to the old on-disk value, producing behavior that depends on whether/when the process restarts. Not rolled back anywhere.
Direction: either roll back the in-memory `Set` on `Save` failure, or re-order to validate-then-marshal-then-atomic-write-then-only-then-mutate live state (mirrors "validate before commit" used elsewhere). Given the field-level Set/rollback machinery already exists in `SetLocked` (`internal/config/set.go:29-76`, it restores `prev` on validation failure), extending the same rollback to a `Save` failure is a natural, small change. Effort: S.

### 6. `history.Open`'s unconditional `VACUUM` on every startup now rewrites the live queue state too
`internal/history/db.go:83-86`. CONFIRMED (mechanism) / INFERRED (practical severity — no measurement taken of typical DB size or VACUUM duration).
`VACUUM` runs on every `Open()` call (both `--serve` and one-shot `--nzb` invocations, `cmd/gonzbd/main.go:206` and `:1022`), rebuilding the entire database file. This was designed/tested (`internal/history/vacuum_test.go`) when the file held only history rows. Now that `jobs`/`job_files` (including per-file `articles_done` hex blobs, potentially sizeable for large multi-part jobs — see Finding 7) live in the same file, every daemon restart pays a full-file rewrite proportional to **queue + history** size, not just history size, and does so before the queue can even be loaded/serve traffic. `VACUUM` also takes an exclusive lock and needs up to 2x disk space transiently.
Direction: consider running `VACUUM` conditionally (e.g. only when free-page ratio via `PRAGMA freelist_count`/`page_count` crosses a threshold, or on an explicit maintenance trigger) rather than unconditionally on every process start, now that the file is shared with hot queue state. Single-user right-sizing note: at realistic single-user history sizes this is probably sub-second and not worth fixing pre-emptively — flagging as a latent scaling risk, not a current bug. Effort: S–M if pursued.

### 7. `job_files.articles_done` is a hand-rolled bitset stored as a hex `TEXT` column, re-encoded on every batched update
`internal/queue/sqlite_store.go:65-102` (`encodeArticlesDone`/`decodeArticlesDone`), used in `Add` (line 163), `updateTx` (line 396). CONFIRMED.
Every `Update`/`UpdateBatch` call re-serializes the *entire* per-file done-bitmap to a hex string and rewrites the whole `TEXT` column, even if only one article's bit flipped since the last write. For files with many articles (large multi-part RAR/PAR2 volumes), this is `O(articles/8)` bytes marshaled to hex (`O(articles/4)` chars) on every progress checkpoint, not `O(1)`. This isn't a correctness bug — it's an efficiency/complexity note in the "concurrency & persistence model fit" lens: a purpose-built column for something that could also have been derived (e.g. from `write_cursor`/`bytes_downloaded` alone for sequentially-assembled files, falling back to the bitmap only for out-of-order completion). Not flagging for fix — no evidence of profiling showing this as a bottleneck — but noting it as unnecessary-generality-adjacent complexity worth watching if job-file sizes grow.
Effort: N/A (informational).

### 8. Redundant manual `job_files` deletes despite `ON DELETE CASCADE`
`internal/queue/sqlite_store.go:447-450` (`Remove`) and `:490-492` (`MoveToHistory`) both explicitly `DELETE FROM job_files WHERE job_id = ?` before deleting the `jobs` row, even though `internal/history/migrations/002_add_jobs_tables.sql` declares `job_files.job_id ... REFERENCES jobs(id) ON DELETE CASCADE`, and `internal/history/db.go:44` enables `_pragma=foreign_keys(1)` in the DSN. The manual deletes are harmless (redundant no-op given the FK) but are an extra round-trip per job removal, and their presence suggests the cascade isn't trusted or wasn't noticed. INFERRED severity (functionally correct either way).
Direction: either rely on the cascade and drop the manual `job_files` delete, or add a one-line comment explaining why it's kept explicit (e.g. clarity, or defense against `foreign_keys` pragma not applying to some connection). Effort: S.

### 9. A full legacy JSON-only queue persistence engine (`saveInner`/non-store `Load`) is dead code on both production paths
`internal/queue/persistence.go:92-141` (`saveInner`), `:206-264` (`Load`'s `idx`/`indexFile` branch), plus `Queue.Add`'s `q.store == nil && q.stateDir != ""` branch (`queue.go:325-330`) and `Remove`'s equivalent (`queue.go:400-413`). CONFIRMED dead in production / exercised only by unit tests (`persistence_test.go`).
Both `cmd/gonzbd/main.go` call sites (`:214`, `:1033`) always construct `app.New` with a non-nil `*history.Repository`, and `app.go:194` only skips `queue.WithStore` if `repo == nil || repo.DB() == nil` — neither of which occurs on any code path reachable from `main.go`. This means the entire whole-queue-as-gzip-JSON implementation (index file + per-job files + quarantine-on-corruption logic + `Prune`'s job-file variant) is maintained, tested, and reviewed, but never runs in the shipped binary. This is exactly the kind of "generalized abstraction over storage backends with one implementation" the single-user right-sizing lens calls out — except here it's not even reachable, so it's pure maintenance cost with zero live behavior to justify it.
Direction: this is a genuine design question, not an obvious deletion — confirm intent (was it meant as a testing seam / no-DB fallback mode that got abandoned, or is it still a deliberate escape hatch for e.g. a future `--no-db` flag?) before removing ~250 lines of parallel logic and its test file. If there's no concrete plan to ever construct a store-less `Queue` in production, delete it. Effort: M (removal + test cleanup), or S (just document why it's kept) if the answer is "keep it."

### 10. `SQLiteStore.Remove`/`MoveToHistory` manifest cleanup happens *after* the SQL commit — correct ordering, but relies entirely on `Prune()` for the crash window
`internal/queue/sqlite_store.go:463-472` (Remove), `:501-509` (MoveToHistory). CONFIRMED, no defect — noted for the crash-consistency section below as the mechanism that makes the split-store boundary safe.
`os.Remove` of the manifest/progress `.json.gz` files happens strictly after `tx.Commit()`. A crash between commit and the `os.Remove` calls leaves an orphaned manifest file with no corresponding `jobs` row. This is intentional and safe: `Prune()` (`sqlite_store.go:588-625`) treats any `manifests/*.json.gz` / `progress/*.json.gz` file whose ID isn't in the live `jobs` table as orphaned and deletes it. `Prune` runs at `Load()` (startup, `persistence.go:202`) and at the end of every `saveStore` checkpoint (`persistence.go:86-88`). The only gap: nothing prunes between two `Save()`/checkpoint calls, so on a long-lived process with no periodic checkpoint trigger, orphans could accumulate for the process's lifetime — cosmetic disk usage, not correctness.

### 11. `WriteAtomic`'s temp→fsync→rename does not fsync the containing directory
`internal/fsutil/atomic.go:34-80`. CONFIRMED (mechanism), INFERRED (practical risk on the user's actual filesystem/journaling mode).
The documented pattern (temp file, write, fsync, close, rename) is followed correctly for file *contents*, but the directory entry rename itself is not additionally `fsync`'d on the parent directory. On some filesystems/mount configurations (e.g. ext4 with `data=writeback`, or certain network filesystems) a power loss immediately after `rename()` can lose the rename despite the file contents being durable, because the directory metadata update wasn't forced to disk. This affects every caller: config `Save`, queue manifest/job persistence, and history's underlying files are unaffected (SQLite manages its own WAL durability). This is a narrow, classic POSIX gotcha rather than a bug specific to this codebase, and most default Linux configurations (ext4 `data=ordered`, xfs) order rename after the preceding fsync sufficiently for practical purposes — hence INFERRED rather than CONFIRMED as an actual bug.
Direction: optional — `fsync` the parent directory fd after `os.Rename` for the config/queue paths if stronger durability guarantees are ever required; not urgent for a single-machine, single-user deployment on typical Linux defaults. Effort: S if pursued.

### 12. `Config.Validate()` explicitly performs no filesystem checks, by design — noted as a boundary, not a gap
`internal/config/validate.go:13-21`. Documented in the doc comment itself: "Validation does not touch the filesystem... because Load runs before subsystems are initialized." This is a deliberate, sound layering choice (avoids ordering hazards between config validation and directory creation) — listed here so synthesis doesn't independently flag path-existence checks as "missing" without knowing it was a conscious decision.

### 13. Config env-var expansion is correctly scoped to path fields only, and matches the documented rule with no drift found
`internal/config/expand.go:1-72`. CONFIRMED, positive finding — see Positive section.

## Crash-consistency analysis

**Add-job** (`SQLiteStore.Add`, `sqlite_store.go:105-185`): SQL row inserts happen first inside the tx, then the manifest gzip file is written (`writeGzJSON`) to disk *before* `tx.Commit()`. Crash windows:
- Crash before manifest write completes, after INSERT statements ran but before commit → `defer tx.Rollback()` never actually executes (process is dead), but SQLite's own crash recovery (WAL/rollback journal) undoes the uncommitted transaction on next open. No inconsistency: neither the row nor a persisted manifest exists.
- Crash after `writeGzJSON` succeeds but before `tx.Commit()` → manifest file exists on disk, but SQLite rolls back the uncommitted jobs/job_files rows on restart. Result: orphaned manifest file, no job. Caught by `Prune()` on next `Load()`. No user-visible correctness issue, just a stray file until pruned.
- Crash after `tx.Commit()` returns → both persisted, consistent. Fine.
There is no window where a job row exists durably without its manifest, or vice versa, surviving past the next `Prune()`.

**Queue → history transition** (`MoveToHistory`, `sqlite_store.go:475-510`, invoked from `job_finalizer.go:34-90` and `queue.go:674-703`): Because `jobs`/`job_files`/`history` are the same SQLite file, the `history` INSERT and the `jobs`/`job_files` DELETE are genuinely one atomic commit — a crash anywhere before `tx.Commit()` leaves the job still active and un-historied (safe: retried or reconciled at next startup since the store, not RAM, is authoritative on reload); a crash after commit leaves it historied and removed, with the on-disk job payload (`internal/app/job_finalizer.go:54-60`, written *before* the DB transaction) and manifest/progress files becoming orphans cleaned by the next `Prune()`. The one real risk is upstream of the DB transaction: `job_finalizer.persistAndCommit` writes `jobPath` (`queue.SaveJob`) *before* calling `store.MoveToHistory`; if that succeeds but the process then crashes before `MoveToHistory` commits, the payload file for the (still-active, not-yet-historied) job exists — again picked up by the manifest/progress `Prune()` cleanup, or simply overwritten on the eventual successful `MoveToHistory` retry. No double-history-entry risk was found: `history.nzo_id` has a `UNIQUE` index (migration 001) and `AddTx`'s `INSERT` would fail on constraint violation rather than duplicate on any retried finalize.

**Config save** (`config.Save`, `loader.go:195-217`): YAML marshaled to memory under `RLock`, lock released, then `fsutil.WriteAtomicBytes` does create-temp → write → fsync → close → rename. A crash at any point leaves either the previous file (if before rename) or the new file (if after) — no torn write possible. The one gap is the missing parent-directory fsync (Finding 11) which is a filesystem-level edge case, not a logic bug. Note the marshal-under-RLock pattern is exactly the documented "snapshot then release" idiom from `go-standards.md` and is followed correctly — no stale/torn snapshot risk, since the entire struct is marshaled while holding the lock, not built field-by-field across multiple lock acquisitions.

**Manifest write** (`writeGzJSON` → `fsutil.WriteGzAtomic` → `WriteAtomic`): same atomic temp+fsync+rename primitive as config saves; the gzip trailer is flushed (`gz.Close()`) before the temp file's `Sync()` (`atomic.go:107-116` composed with `:34-80` — the `write` closure passed to `WriteAtomic` closes the gzip writer *inside* the closure, and the outer `WriteAtomic` fsyncs the underlying file only after `write()` returns), so the fsync genuinely covers the full compressed payload, not a partially-flushed stream. No defect found here.

## Positive / load-bearing

- **The single-database design (however undocumented) is a materially better architecture than the two-database design the brief hypothesized.** Sharing one SQLite file for `jobs`/`job_files`/`queue_meta`/`history` sidesteps the entire "can't ATTACH-transaction across files safely" problem class. This looks like a deliberate, good decision that just never made it back into the docs.
- **`go-standards.md`'s SQLite DSN-pragma rule is followed correctly.** `internal/history/db.go:44` puts `foreign_keys`, `busy_timeout`, and `synchronous` in the DSN (applies to every pooled connection), and only the database-scoped `journal_mode=WAL` (persists on disk, connection-independent) runs as a one-time `ExecContext` — exactly the distinction the lessons-learned doc calls for.
- **Manifest read failures are quarantined (`.corrupt` rename) rather than silently overwritten or ignored**, in both `sqlite_store.go:245` and `queue.go:684-691` — preserves forensic evidence for the operator.
- **`Repository.Delete`/`Prune` chunk large ID lists at 999 params** (`repository.go:298-336`) — directly matches the "batch large deletions" lesson in `go-standards.md` §5.
- **`Repository.Search`'s LIKE-escaping (`escapeLike`) and preallocation cap (`maxPrealloc = 10_000`)** (`repository.go:180-204,263-271`) is a nice piece of defensive engineering: bounds a caller-supplied `Limit` from being used to force a huge slice preallocation before any row is read, while still avoiding reallocation churn for the common bounded case.
- **`scanEntry`'s `sql.Null*` handling with a `TRACE-4` comment** (`repository.go:420-432`) explains a real, previously-considered NULL-safety concern clearly, rather than leaving a silent landmine.
- **Config `Save`'s snapshot-then-release-then-write pattern** correctly follows the documented lock-discipline idiom, with the actual comment ("--- No lock held below this line ---") the codebase's own convention calls for.
- **Config env-var expansion is precisely scoped** to path-typed fields (`expand.go`), never applied to the raw YAML bytes — directly avoiding the "Never use `os.ExpandEnv` on raw config file bytes" lesson, and the code comments explain why.
- **`Set`/`SetLocked`'s rollback-on-validation-failure** (`set.go:40-49,61-70`) is a clean, minimal-footprint way to guarantee `Validate()` and `Load`-time validation can never diverge — both paths funnel through the identical `Config.Validate()`.
- **`Queue.Add`'s TOCTOU reasoning is explicitly documented** (`queue.go:300-316,351-354`) rather than left implicit — a good model for how to justify holding a lock across I/O when it's genuinely necessary.
- **`resequenceTx`/`ShiftSortKey`** correctly keep `sort_key` contiguous inside the same transaction as the mutation that necessitated it, avoiding order drift.

## Open questions for synthesis

1. Is the legacy JSON-only `Queue.Save`/`Load` path (Finding 9) an intentional escape hatch worth keeping (e.g. for a future no-database mode), or safe to delete? This is a design decision, not something this lane should resolve unilaterally.
2. Should `docs/ARCHITECTURE.md` / `docs/sabnzbd_spec.md` be corrected in the same PR pass as any other doc-accuracy fixes surfaced by other lanes (Finding 1, 2), or tracked as one dedicated docs-only PR?
3. Is there a known/likely realistic history.db size ceiling for this user's actual usage that would tell us whether Finding 6 (VACUUM-on-every-open) is worth pre-emptively fixing, or is it fine to defer until someone notices slow startup?
4. Worth checking with the API-lane reviewer: are there other API handlers besides `modeSetConfig` (Finding 5) that call `config.Set` then `config.Save` with the same unrolled-back-on-Save-failure pattern? `internal/api/config.go:338-341` has a second occurrence in the same file already visible from this lane's read.

## git status proof

```
(clean — no output from `git status --short`)
```
