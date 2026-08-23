# SABnzbd Post-Download Pipeline — Go Reimplementation Spec

**Scope:** Everything that happens after an NNTP article is decoded and handed to the
assembler: writing to disk, integrity checking, par2 repair, unpack (rar/7z/join),
deobfuscation/renaming, and the final move. Hard-won corner cases are
called out inline with the test that pins them.

> **Note:** Sorting (TV/movie/date template-based renaming) is **NOT IMPLEMENTED**
> in GoNZBD. This functionality is handled by external tools such as Sonarr,
> Radarr, and similar media managers. The sorting section below (§11) is
> preserved as reference documentation for the original SABnzbd behavior only.

This document is a sibling to `sabnzbd_spec.md` (full system) and goes deeper on the
post-download pipeline only.

**Source files of record** (Python; read these when in doubt):

| Concern | File |
|---|---|
| Disk write, per-file CRC | `sabnzbd/assembler.py` |
| yEnc/UU decode, per-article CRC | `sabnzbd/decoder.py` |
| Memory→disk article spill | `sabnzbd/articlecache.py` |
| Job/file/article model | `sabnzbd/nzb/{object,file,article}.py` |
| State machine, history, scripts | `sabnzbd/postproc.py` |
| par2 packet parser | `sabnzbd/par2file.py` |
| par2 repair, unrar, 7z, join | `sabnzbd/newsunpack.py` |
| Concurrent unrar-during-download | `sabnzbd/directunpacker.py` |
| Obfuscation detection & rename | `sabnzbd/deobfuscate_filenames.py` |
| TV/movie/date sorter | `sabnzbd/sorting.py` | **NOT IMPLEMENTED in GoNZBD** |
| Path safety, sanitize, rename | `sabnzbd/filesystem.py` |
| Charset/NFC | `sabnzbd/encoding.py` |
| History DB | `sabnzbd/database.py` |
| PP bits, statuses | `sabnzbd/constants.py` |

---

## 1. Pipeline Overview

```
                        ┌──────────────┐
NNTP article ─► Decoder ┤  yEnc/UU     │
                        │  CRC32 check │
                        └──────┬───────┘
                               │ Article{data, data_begin, data_size, crc32, md5of16k}
                               ▼
                        ┌──────────────┐
                        │ ArticleCache │   memory dict + disk spill (90% trigger)
                        └──────┬───────┘
                               ▼
                        ┌──────────────┐
                        │  Assembler   │   pwrite (Unix) / lseek+write (Win)
                        │  Append OR   │   per-file crc32 via crc32_combine
                        │  Direct mode │   sabctools.sparse() preallocation
                        └──────┬───────┘
                               │ (per file: nzf.assembled=True)
                               │ (per job: post_done=True)
                               ▼
                        ┌──────────────┐
                        │ PostProcessor│   linear state machine, PP-bit gated
                        └──────┬───────┘
                               │
        ┌──────────────────────┼────────────────────────┐
        ▼                      ▼                        ▼
   QuickCheck             par2 Repair               (skip if PP<R)
   (par2 hashes)          if needed
        │                      │
        └──────────┬───────────┘
                   ▼
            ┌─────────────┐
            │  Unpack     │   filejoin → unrar → 7z → ts
            │  (recursive, depth ≤3)
            └──────┬──────┘
                   ▼
            ┌──────────────────┐
            │ recover_par2     │   16K-MD5 → real filename
            │ _names           │
            └──────┬───────────┘
                   ▼
            ┌──────────────────┐
            │ deobfuscate      │   biggest-file heuristic, ext-guess,
            │                  │   subtitle pairing
            └──────┬───────────┘
                   ▼
            ┌──────────────────┐
            │ Sorter           │   NOT IMPLEMENTED in GoNZBD
            │ (external tools) │   (Sonarr, Radarr handle this)
            └──────┬───────────┘
                   ▼
            ┌──────────────────┐
            │ Final move +     │   tmp `_UNPACK_*` → category dir
            │ cleanup          │
            └──────┬───────────┘
                   ▼
            ┌──────────────────┐
            │ User script      │   9 args + ENV, exit-code interpreted
            └──────┬───────────┘
                   ▼
              History DB
            (sqlite, zlib log)
```

The whole chain is driven by a single PostProcessor worker per host, with one job
in flight at a time. The assembler is its own thread; everything downstream runs
synchronously inside the postproc worker (subprocesses for par2/unrar/7z/script).

---

## 2. Decoder → Assembler Contract

### 2.1 Article-level state set by decoder

`sabnzbd/decoder.py` consumes raw NNTP responses via the `sabctools` C extension's
`NNTPResponse` object. For each successfully decoded article it sets on the
`Article` instance:

- `data` — decoded bytes (yEnc or UU body)
- `data_begin` — offset into the final file (from yEnc `=ypart begin=` header)
- `data_size` — declared part size
- `decoded_size` — actual bytes after decode (must equal `data_size`)
- `crc32` — CRC32 from the yEnc/UU trailer; `None` if header was missing
- `md5of16k` — MD5 of the first 16 KB of decoded data **if this is the first
  article of the file** (used later by deobfuscation)
- `decoded = True`, `on_disk = False`

**`sabctools` primitives the Go port must reimplement / find equivalents for:**

| Function | Purpose | Go equivalent |
|---|---|---|
| `NNTPResponse(buf)` | yEnc/UU decode + CRC | port yEnc decoder; `hash/crc32` (IEEE poly) |
| `crc32_combine(a, b, len_b)` | combine two CRCs | port from zlib `crc32_combine64` (not in std lib) |
| `crc32_multiply(crc, coef)` | GF(2) multiply for combine | helper for combine |
| `crc32_zero_unpad(crc, n)` | strip zero-pad | helper for tail-of-slice |
| `crc32_xpow8n(slice_size)` | precompute coefficient | helper for combine |
| `sparse(fd, size)` | preallocate sparse file | `unix.Fallocate`, `windows.SetFileValidData` |

### 2.2 Failure paths

- **No CRC in yEnc**: treated as missing article; if pre-check off, no retry. If
  pre-check on, alternate server retry.
- **CRC mismatch**: `BadData` exception. Article passed to
  `search_new_server()` which adds the original server to the article's
  `TryList` and re-queues to another server. After all servers tried with no
  success → `bad_articles_counter += 1`.
- **`bad_articles > MAX_BAD_ARTICLES` (5)**: whole job aborts immediately.
- **`bytes_missing / (bytes - bytes_par2) > availability_threshold`** (default
  5%): job aborts before reaching post-proc.
- **First-article bad rate > 80%**: immediate abort (detects fully-DMCA'd jobs
  cheaply).

### 2.3 Article cache (memory pressure)

`articlecache.py`:

- `__cache_limit`: capped at 4 GiB (64-bit) / 1 GiB (32-bit), user-configurable.
- `__non_contiguous_trigger`: 90 % of limit. When exceeded, cache calls
  `Assembler.process(..., allow_non_contiguous=True)` for every active file,
  forcing direct-mode writes even with gaps.
- Spillover: articles overflowing cache are written to disk in the job's admin
  dir as `SABnzbd_article_<id>` and reloaded on demand via `load_article()`.
- The cache and the assembler exchange back-pressure through `ready_bytes`
  (per-file byte counter); the downloader respects a `Soft Queue Limit`
  (50 % of cache) before slowing intake.

**Go design note:** A bounded `chan Article` plus `sync.Cond` for the 90 %
trigger; spillover to disk is the same temp-file pattern. Don't use unbuffered
channels — the cache is intentionally elastic.

---

## 3. Assembler

### 3.1 Two write modes

1. **Direct-write (preferred, yEnc + sparse alloc OK):** file is preallocated
   with `sabctools.sparse()`, articles are written to their exact offsets via
   `os.pwrite()` (Linux/macOS) or `os.lseek()+os.write()` (Windows, no atomic
   pwrite). Writes can happen out of order; `Article.on_disk = True` once
   written.
2. **Append (fallback):** strictly sequential. Used when the file is UU, when
   sparse alloc failed, or when an article in direct mode is missing data. Once
   an append write happens, the file is committed to append mode for the
   remainder of that pass.

State carried on `NzbFile`:

- `assembler_next_index` — index into the contiguous-region cursor (resumes
  here on subsequent `assemble()` calls or after process restart).
- `contiguous_offset()` — byte offset corresponding to that index.
- `crc32` — running combined CRC32 of all contiguous bytes written so far.
  Set to `None` the moment a non-contiguous write happens (because crc-combine
  requires sequential consumption).
  **GoNZBD does not do this** — see the note in §3.3.

### 3.2 Disk-space checks

Run once per **file completion** (not per article) in `assembler.py:324-357`:

- Required: `nzf.bytes + cfg.download_free` must fit in download dir.
- Additionally: if job is > 95 % complete **or** `DirectUnpacker` is running,
  also check complete dir has space.
- On failure: pause downloader, schedule auto-resume (unless
  `fulldisk_autoresume=False`), email + notify user.

### 3.3 Per-file CRC accumulation

```python
# at every successful write
nzf.crc32 = sabctools.crc32_combine(nzf.crc32, article.crc32, article.decoded_size)
```

This produces a CRC32 that matches what par2 records as the **file-level CRC32**
(the par2 spec uses CRC32 over the entire file). The quickcheck stage compares
`nzf.crc32` against the par2 `filehash` field without re-reading the file.
**This is the optimization that lets SABnzbd skip repair on most healthy jobs.**

> **Superseded for GoNZBD by the download-durability work. This section
> describes SABnzbd and remains accurate about SABnzbd; the `Go:` directive
> below is dead and must not be implemented as written.**
>
> GoNZBD does **not** maintain a running combined CRC at write time, and the
> assembler has no authority to record a whole-file CRC at all. The value comes
> from the durability record instead: each `durable_runs` row carries the
> `crc32_combine` of the articles that merged into it, so a file whose articles
> all abut collapses to a **single** row at offset 0 whose `crc32` *is* the
> whole-file CRC — already computed, on stable storage, with no read of the
> file. It is a query, not a walk.
>
> `Application.recordAssembledCRC` threads that value to `Queue.SetFileCRC32`
> when the file finalizes, so `FileProgress.AssembledCRC32` is populated for a
> file whose bytes tile it exactly. A file that keeps more than one row — a hole,
> or an article overlapping a sibling — supplies nothing, and zero is the
> documented "unavailable" value: the verify reads it as `NoCRC` and par2 runs.
> See §4 of [`durability-contract.md`](durability-contract.md).

Go (superseded): implement `crc32Combine(crc1, crc2 uint32, len2 int64) uint32`
using GF(2) matrix doubling — see zlib's `crc32_combine64`. Keep
`nullableCRC = *uint32` because the value must be invalidatable on
non-contiguous writes.

### 3.4 File completion → job completion

```python
articles_left, file_done, post_done = nzo.remove_article(article, success)
```

- `file_done == True` ⇔ `nzf.articles` empty AND `nzf.import_finished`.
- `post_done == True` ⇔ all `nzf` in job are in `finished_files`.
- On `file_done`: `nzo.remove_nzf(nzf)` moves the file to `finished_files`.
- On `post_done`: assembler calls `sabnzbd.PostProcessor.process(nzo)`.

### 3.5 Tests pinning assembler corner cases

`tests/test_assembler.py`:

| Test | Input | Pins |
|---|---|---|
| `test_assemble_direct_write` | 2 articles, both direct | Sparse alloc, pwrite to declared offsets, `crc32` non-None |
| `test_assemble_direct_write_aborted_to_append` | [direct, non-direct, direct] | Mode commits to append after first failure; remaining direct article is appended, not pwritten |
| `test_assemble_direct_append_direct_append` | Out-of-order arrival with cache flush | Direct holes filled by a later append pass |
| `test_assemble_direct_write_aborted_to_append_second_attempt` | Partial direct, resume with append | `assembler_next_index` survives across calls |
| `test_force_append` | 5 articles with gaps, `allow_non_contiguous=True` | Two-pass: direct-write [0,2,4] first, append [1,3] second |
| `test_force_force_direct` | Force-flush mid-job, then client restart | `assembler_next_index = 0` on restart safely skips `on_disk=True` articles |
| `test_assemble_append_first_not_decoded` | First article not decoded | No write until first arrives (append cannot start mid-file) |

`tests/test_decoder.py`:

| Test | Input | Pins |
|---|---|---|
| `test_short_data` | yEnc body truncated mid-line | `BadUu`/`BadData` |
| `test_missing_uu_begin` | UU body without `begin` header | `BadUu` |
| `test_singlepart` (parameterized) | UU with assorted header combos | `filename_checked=True`, decoded content matches |
| `test_multipart` | 4-part UU | Reassembled bytes concatenate correctly |
| `test_broken_uu` | Non-ASCII in UU body | `BadData`, no partial write |

**Go corner-case checklist:**

- An article whose `crc32` is `None` (yEnc trailer absent) must not poison
  `nzf.crc32`; treat as if the file CRC is unknown for repair purposes.
- Restart safety: on startup, scan `finished_files` *and* the on-disk file
  size to decide whether append-mode resume is safe.
- Windows: no atomic pwrite. Use `nzf.file_lock` to serialize seek+write; on
  Linux take advantage of `pwrite`'s atomicity to drop the per-file lock.

### 3.6 Constants

| Constant | Value | Use |
|---|---|---|
| `ASSEMBLER_TRIGGER_PERCENTAGE` | 0.05 | Bytes-ready ratio to wake assembler |
| `ASSEMBLER_DELAY_FACTOR_DIRECT_WRITE` | 1.5 | Delay multiplier in direct mode |
| `ASSEMBLER_WRITE_INTERVAL` | 5.0 s | Per-file write coalescing |
| `ARTICLE_CACHE_NON_CONTIGUOUS_FLUSH_PERCENTAGE` | 0.9 | Cache-pressure flush |
| `SOFT_ASSEMBLER_QUEUE_LIMIT` | 0.5 | Below this, no downloader back-pressure |
| `MAX_BAD_ARTICLES` | 5 | Job abort threshold |

---

## 4. par2 Packet Parser (`par2file.py`)

This is the keystone of the whole rename system. **The Go reimplementation
should treat par2 metadata as the canonical source of filenames and per-file
hashes**, exactly as SABnzbd does.

### 4.1 Packet framing

Every par2 packet:

```
+-------+----------+----------+-------+--------+----------+
|"PAR2  |  length  |  MD5 of  |  set  | packet |  body    |
| \0PKT"|  (LE u64)|  body    |  id   |  type  | (multi-  |
|  8 B  |   8 B    |  16 B    |  16 B |  16 B  |  of 4 B) |
+-------+----------+----------+-------+--------+----------+
```

Length must be a multiple of 4 and ≥ 20. The 16-byte body MD5 is **validated** —
mismatched packets are dropped. Set ID is the recovery set's fingerprint.

### 4.2 Packet types of interest

| Type ID | Constant | Body |
|---|---|---|
| `PAR 2.0\0Main\0\0\0\0` | `PAR_MAIN_ID` | slice size (u64), file count, file IDs |
| `PAR 2.0\0FileDesc` | `PAR_FILE_ID` | per-file metadata (see below) |
| `PAR 2.0\0IFSC\0\0\0\0` | `PAR_SLICE_ID` | per-slice MD5 + CRC32 |
| `PAR 2.0\0Creator\0` | `PAR_CREATOR_ID` | tool identifier |
| `PAR 2.0\0RecvSlic` | `PAR_RECOVERY_ID` | recovery block (counted, not parsed) |

### 4.3 FileDesc body layout

```
+--------+----------+----------+--------+---------------+
| FileID | MD5 full | MD5 16K  | length | filename      |
| 16 B   | 16 B     | 16 B     | 8 B LE | UTF-8, null-  |
|        |          |          |        | padded to 4 B |
+--------+----------+----------+--------+---------------+
```

The filename is **not length-prefixed**; trailing nulls are stripped. Encoding
may not be valid UTF-8 (older creators wrote OS-local bytes); SABnzbd runs each
filename through `correct_unknown_encoding()` which tries UTF-8 →
ISO-8859-1 → `charset_normalizer` and applies NFC.

### 4.4 Per-file CRC32 reconstruction

The IFSC packet carries CRC32 *per slice*. SABnzbd reconstructs the file-level
CRC32 by combining slice CRCs:

```python
crc32 = 0
for slice_nr in range(num_slices):
    crc32 = sabctools.crc32_multiply(crc32, xpow8_slice_size) ^ filecrc32[slice_nr]
if tail := filesize % slice_size:
    crc32 = sabctools.crc32_combine(
        crc32,
        sabctools.crc32_zero_unpad(last_slice_crc, slice_size - tail),
        tail,
    )
```

This is the CRC compared against `NzbFile.crc32` during quickcheck.

### 4.5 Scan optimization

For par2 files larger than 10 MiB: stop parsing once all FileDesc + IFSC
packets have been read (recovery blocks are huge and irrelevant for rename).
Small par2s are scanned fully so the creator string can be logged.

### 4.6 16K-MD5 deduplication

The dict returned to callers is keyed by `md5of16k`. When two distinct files
share an identical first 16 KB (common with multi-volume rars whose headers are
identical), **both entries are marked `has_duplicate=True` and excluded from
the rename map**. Without this, quickcheck would silently mis-rename files —
fixed in issue #3164.

### 4.7 Tests

`tests/test_par2file.py`:

| Test | Input | Pins |
|---|---|---|
| `test_parse_par2_french_german_demo` | `frènch_german_demö.rar.vol0+1.par2` | Mixed Latin-1 / UTF-8 filename decode |
| `test_parse_par2_chinese` | `我喜欢编程.par2` | UTF-8 multibyte filename round-trip |
| `test_basic_16k` | 18 KB single-slice file | 16K-MD5 differs from full MD5; CRC32 matches |
| `test_duplicate_16k` | Two files sharing first 16 KB | `has_duplicate=True` on both; not in rename map |

### 4.8 Go data model

```go
// As implemented in internal/par2/parser.go.
type FileDesc struct {
    FileID       [16]byte  // unique file identifier within the recovery set
    FileName     string
    FullHash     [16]byte  // MD5 of the entire file
    Hash16k      [16]byte  // MD5 of first 16 KB
    FileSize     uint64
    FileCRC32    uint32    // reconstructed from IFSC slices; 0 = unknown
    HasDuplicate bool      // true if another FileDesc shares this Hash16k
}

type Par2Set struct {
    SetID          [16]byte
    SliceSize      uint64
    Files          []FileDesc             // all file descriptions (slice, not a map)
    FilesByID      map[[16]byte]*FileDesc // keyed by FileID for IFSC lookup
    By16k          map[[16]byte]*FileDesc // keyed by Hash16k (entries removed if HasDuplicate)
    RecoveryBlocks int
    Creator        string
}
```

---

## 5. Quick-Check (par2 pre-check)

Runs **before** invoking `par2 r`. Saves a subprocess + tens of seconds on every
healthy job. `newsunpack.py:quick_check_set`:

1. For each file recorded in `nzo.par2packs[setname]`:
   - First, look up by filename. If present and `nzf.crc32 == filehash` and
     `filesize` matches → mark good.
   - Otherwise, look up by `(crc32, filesize)` across all unmatched downloaded
     files. If exactly one candidate → record rename `{real_name:
     downloaded_name}`.
   - Use `found_paths` set to prevent a single download from matching two par2
     entries.
2. If every par2 entry resolves → quickcheck wins; the rename map is applied
   and par2 repair is skipped entirely.
3. Extensions in `cfg.quick_check_ext_ignore()` (default `.sfv`) are not
   compared.

**Note:** quickcheck depends on `nzf.crc32` being non-None — i.e., the
assembler wrote the file contiguously. If a force-flush happened
mid-file, quickcheck is impossible for that file and par2 repair runs.

---

## 6. Post-Processor State Machine (`postproc.py`)

### 6.1 PP bits

From `constants.py:PP_LOOKUP`:

| Bit | Value | Means |
|---|---|---|
| 0 | "" | download only |
| 1 | "R" | + repair |
| 2 | "U" | + unpack |
| 3 | "D" | + delete sources |

Normalized at the start of `process_job`:

```python
if nzo.delete:   flag_unpack = True
if flag_unpack:  flag_repair = True
```

(Delete implies Unpack; Unpack implies Repair. Cannot delete without unpacking.)

### 6.2 Stages

| Order | Stage | Sets `Status` to | Skipped if |
|---|---|---|---|
| 1 | Pre-checks (empty job, only `__ADMIN__`) | — | — |
| 2 | Save attribs to disk | — | — |
| 3 | `parring()` — quickcheck + repair | `QUICK_CHECK`, `VERIFYING`, `REPAIRING` | `flag_repair=False` or `all_ok=False` |
| 4 | `unpacker()` — recursive filejoin/rar/7z/ts | `EXTRACTING` | `flag_unpack=False` or `all_ok=False` |
| 5 | Move leftovers to `tmp_workdir_complete` | `MOVING` | `all_ok=False` |
| 6 | `recover_par2_names()` (16K-MD5 rename) | — | `cfg.process_unpacked_par2=False` |
| 7 | `deobfuscate()` | — | — |
| 8 | Sorter rename | — | sorter not active for category/type |
| 9 | `cleanup_list()` — extension-based cleanup | — | — |
| 10 | Sample removal | — | `cfg.ignore_samples=False` |
| 11 | Final move into `workdir_complete` | — | — |
| 12 | User script (`external_processing`) | `RUNNING` | no script configured |
| 13 | History DB insert | `COMPLETED` or `FAILED` | — |
| 14 | Notification + email | — | — |
| 15 | `nzo.purge_data(all_ok)` | — | — |

The script (step 12) **runs even when the job failed**, with the failure code in
its env. Job final status is set immediately after the script.

### 6.3 Re-add path

`parring()` can return `(par_error, re_add=True)`. This means par2 said
"You need N more recovery blocks" — the job is pushed back into the download
queue at `REPAIR_PRIORITY` to fetch additional par2 volumes, then revisited.
Implementation:

```python
nzo.status = Status.FETCHING
nzo.priority = REPAIR_PRIORITY
sabnzbd.NzbQueue.add(nzo, position=0)
return  # don't continue postproc this round
```

### 6.4 Persisted state for crash recovery

- **`save_attribs()`**: `(cat, pp, script, priority, final_name, password, url)` →
  GoNZBD writes `<downloadDir>/__ADMIN__/gonzbd_attrib` (SABnzbd used
  `<admin>/SABnzbd_nzo_<id>/ATTRIB_FILE`).
- **`__verified__`** map: per-set bool, written whenever a par2 set finishes
  verification. On restart, sets with `True` are not re-verified. GoNZBD stores
  this as a JSON file at `<downloadDir>/__ADMIN__/__verified__` (SABnzbd pickled
  it).
- **PostProc queue snapshot**: `POSTPROC_QUEUE_FILE_NAME` (pickle v2) is the
  entire history list, written periodically.
- **DirectUnpacker `success_sets`**: in-memory; on restart, already-extracted
  sets are detected by scanning the work dir and skipped by unrar (overwrite-
  protect).

**Go note:** GoNZBD persists `__verified__` as a small JSON file (not pickle,
not SQLite). Pickle is a landmine in a multi-version environment.

### 6.5 Script invocation contract

Positional args (9 of them, even if unused are `""`):

```
script_path <complete_dir> <nzb_filename> <final_name> "" <cat> <group> <status> <failure_url>
```

`status` values:

| Value | Meaning |
|---|---|
| `0` | All OK |
| `1` | par2 repair failed (unpack still attempted/succeeded) |
| `2` | unpack failed |
| `3` | par2 + unpack failed |
| `-1` | empty download (nothing to process) |

Environment variables (in addition to inherited env):

| Var | Source |
|---|---|
| `failure_url` | `nzo.nzo_info["failure"]` |
| `complete_dir` | clipped path to final dir |
| `pp_status` | same int as arg 7 |
| `download_time` | seconds |
| `avg_bps` | average bytes/sec |
| `age` | days since article posted |
| `orig_nzb_gz` | clipped path to gzipped original NZB |
| `PYTHONUNBUFFERED` | `1` if script ends in `.py` |

`SAB_*` env vars (full set of job metadata) are also injected; see
`create_env()` for the canonical list.

**Output capture:**

- stdout is read line-by-line via `readline()` (no timeout, relies on script
  completing); HTML tags are stripped (`<.*?>` → space).
- The full output is stored in history as zlib-compressed BLOB.
- `script_line` (history column) is the **last non-empty line, trimmed to 150
  chars** — surfaced in the UI.

**Exit code:** non-zero is rendered as `"Exit(N): <last_line>"`. If
`cfg.script_can_fail()` is enabled, non-zero sets `all_ok=False` and the job
goes to `FAILED`.

**Security:** Scripts are constrained by `make_script_path()` /
`is_valid_script()` — they must live under `cfg.script_dir()` and be executable.
No shell interpolation; argv passed directly to `Popen`.

### 6.6 Final directory layout

```
<complete_dir>/                  ← cfg.complete_dir + category dir
└── <final_name>/                ← collision-resolved via get_unique_dir (.1, .2…)
    ├── (optional marker file)
    ├── <extracted contents>
    └── (NZB kept if NZB-only download)
```

While work is in progress: directory is named `_UNPACK_<final_name>` (if
`cfg.folder_rename()` is on); renamed to `<final_name>` on success or
`_FAILED_<final_name>` on failure.

**Category dir trailing `*`**: suppresses the per-job subfolder — files are
moved directly into the category dir. This is the "flat" layout escape hatch.

### 6.7 History DB schema (sqlite, `history.db`)

The schema is managed by goose migrations (`internal/history/migrations/`),
not a `PRAGMA user_version`. The current schema (`001_initial.sql`) is:

```sql
CREATE TABLE history (
    id              INTEGER PRIMARY KEY,
    completed       INTEGER,           -- unix ts
    name            TEXT,
    nzb_name        TEXT,
    category        TEXT,
    pp              TEXT,              -- 'R'|'U'|'D'|'X'|''
    script          TEXT,
    report          TEXT,
    url             TEXT,
    status          TEXT,              -- 'Completed' | 'Failed'
    nzo_id          TEXT UNIQUE,
    storage         TEXT,              -- incomplete path (for retry)
    path            TEXT,              -- final output path
    script_log      BLOB,              -- zlib-compressed
    script_line     TEXT,
    download_time   INTEGER,
    postproc_time   INTEGER,
    stage_log       TEXT,              -- "Repair:::QuickCheck OK\r\n…"
    downloaded      INTEGER,
    completeness    INTEGER,
    fail_message    TEXT,
    url_info        TEXT,
    bytes           INTEGER,
    meta            TEXT,
    series          TEXT,
    md5sum          TEXT,
    password        TEXT,
    duplicate_key   TEXT,              -- TV/movie identifier for smart dupes
    archive         INTEGER DEFAULT 0,
    time_added      INTEGER
);
CREATE UNIQUE INDEX idx_history_nzo_id ON history(nzo_id);
CREATE INDEX idx_history_archive_completed ON history(archive, completed DESC);
```

> Note: This column set is **identical** (column-for-column, in order) to the
> upstream SABnzbd v5 schema in `sabnzbd/database.py`, so an existing
> `history.db` can be opened by either daemon. GoNZBD relaxes a few constraints
> (no `NOT NULL` on `completed`/`name`/`nzb_name`, `nzo_id` is `UNIQUE`, and two
> covering indexes are added). An earlier draft of this spec listed
> `status_text`/`nzo_info_pickle` columns — those never existed in SABnzbd v5
> and are not in GoNZBD.

`stage_log` format: `Stage:::action1;action2\r\nStage:::…`. Stage names:
`Source`, `Download`, `Repair`, `Filejoin`, `Unpack`, `Servers`, `Script`,
`Notification`. Use this verbatim for compatibility if users may migrate dbs.

### 6.8 Tests (`tests/test_postproc.py`)

| Test | Input | Pins |
|---|---|---|
| `test_rar_renamer_obfuscated_single_rar_set` | 7 hex-named files, all in same rar set | All 7 renamed to `name.part00N.rar` |
| `test_rar_renamer_obfuscated_two_rar_sets` | 16 files, 2 interleaved sets | Content-matching disambiguates sets (compare last block of N to first block of N+1) |
| `test_rar_renamer_missing_first_rar` | Single set, first volume missing | Remaining volumes still renamed |
| `test_rar_renamer_missing_middle_rar` | Two-set, gap in middle | No renaming attempted (staircase check fails) |
| `test_rar_renamer_fully_encrypted` | RAR with encrypted headers | Empty filelist → no rename |
| `test_prepare_extraction_path` (param) | Various combos of category override, sorter, `folder_rename`, marker | `_UNPACK_` prefix iff `folder_rename`; sorter overrides path; marker iff configured |
| `test_process_nzb_only_download_single` | NZB-only download, 1 NZB | `process_single_nzb()` called with original NZB name |
| `test_process_nzb_only_download_multiple` | NZB-only, N NZBs | Each enqueued as `"<original> - <filename>"` |
| `test_process_nzb_only_download_empty` | NZB-only dir is empty | Returns None |

---

## 7. par2 Repair (`newsunpack.py`)

### 7.1 Binary discovery (startup, in `find_programs()`)

Priority order:

- **Windows**: `win/par2/par2.exe` (or `win/par2/arm64/par2.exe`)
- **macOS**: `macos/par2/par2` (universal2 binary)
- **Linux**: first `par2` on PATH (then `par2cmdline-turbo` detected by parsing
  `par2 -V` output → sets `MULTI_PAR2 = True`).

The discovered binary is stored at module scope as `PAR2_COMMAND`. `rarfile.UNRAR_TOOL` is also set so the `py-rarfile` library uses the same binary.

### 7.2 Command line

```
PAR2_COMMAND r [options...] <parfile> <wildcard>
```

Conditional flags (decided by parsing `par2 --help` once at startup):

- `-N` — if help text mentions "No data skipping" (works around an older
  par2cmdline bug that mis-reports recovery counts when files are pre-good).
- `-B <download_path>` — if help text mentions "Set the basepath" (needed
  with some multicore builds that otherwise resolve paths against `cwd`).

Wildcard logic:

- Single par2 set in the dir, **or** fewer than 2 files matching the set name:
  `*` (let par2 consider every file in the dir).
- Otherwise: `<setname>*` (constrain to the set).

User-configurable extras: `cfg.par_option()` — inserted at position 2.

### 7.3 Stdout parser (state machine)

Reading line-by-line. Phases: `VERIFYING → EXTRA_FILES → REPAIR →
VERIFYING_REPAIR`.

| Line substring | Action |
|---|---|
| `"All files are correct"` | `finished=True`, exit |
| `"Repair is required"` | enter REPAIR phase |
| `"Main packet not found"` | request the smallest par2 to be re-downloaded |
| `"You need N more recovery blocks"` | call `get_extra_blocks(N)` to fetch more par2 volumes, return `(finished=False, readd=True)` |
| `"Repair is possible"` | begin progress bar |
| `"Repairing: NN.N%"` / `"Processing: NN.N%"` | update `nzo.set_action_line("Repairing", "%")` |
| `"Repair complete"` | enter VERIFYING_REPAIR |
| `"Verifying repaired files"` | progress per file |
| `"<file>" is a match for "<other>"` | record rename `{correct: actual}` |
| `"<file> - found N of M data blocks from"` | obfuscated joinable detected; record in `used_joinables` |
| `"Could not write … at offset 0"` | join-output collision; skip if alternative joinables exist |
| `"cannot be renamed to"` | hard fail (likely disk write protected) |
| `"not enough space on the disk"` | hard fail; mark process for kill |

Returns `(finished, readd, used_joinables, used_for_repair)`. The latter two
lists name files par2 already consumed; those must NOT be deleted in the
delete-sources stage.

### 7.4 Tests for par2 repair

`tests/test_newsunpack.py` (focused subset):

| Test | Input | Pins |
|---|---|---|
| `test_par2_quickcheck_only` | All files present and matching | Skips subprocess entirely |
| `test_par2_repair_simple` | One file 1-byte damaged | Single repair pass, no re-add |
| `test_par2_repair_need_more_blocks` | Damage exceeds available recovery | `readd=True`; subsequent run after fetching extra blocks succeeds |
| `test_par2_obfuscated_rename` | Files named with hashes | par2 reports "is a match for", rename map applied, no actual repair |
| `test_par2_in_subdir` | `.par2` in nested dir | Wildcard collapses to `*`; par2 still finds files |

---

## 8. Unpack: filejoin / unrar / 7z / ts

Recursive driver (`unpacker()` in `newsunpack.py`):

```
depth = 0
while True:
    new = []
    new += filejoin_set(...)
    new += rar_unpack(...)
    new += sevenzip_unpack(...)
    new += ts_join(...)
    if not new or depth == 3:  break
    depth += 1
```

Max recursion depth: **3**. Rationale: enough for `outer.rar → inner.tar → final.iso`,
not enough for fork-bomb archives.

### 8.1 File join

For `.001`/`.002`/… and `.ts.001`/`.ts.002`/…:

1. Group by stripped base.
2. Sort lexically.
3. Validate sequence: extract numeric suffix via `get_seq_number`. First
   segment must be `1` (or `001`); gaps fail with
   `nzo.set_unpack_info("Sequence error in <base>")`.
4. Open `base` in `ab` mode, `shutil.copyfileobj(src, dst, length=24<<20)`
   each segment.
5. If `nzo.delete`, delete sources after success (skip sources listed in
   `used_joinables` from par2).

If a joined output already exists (par2 reconstructed it), skip join, delete
sources.

### 8.2 unrar

Binary priority: `unrar` → `rar` → `unrar3` → `rar3` (Linux); bundled on Win/Mac.

Version probe (`unrar_check`): parse `RAR (N)\.(M)` from `unrar` (no args)
output → `version = major*100 + minor`. GoNZBD sets `HasProblem = true` if
`version < 550` (RAR 5.50) **or** the version is unparseable
(see `internal/unpack/version.go`). HasProblem disables `-scf` and `-or`.

Command:

```
unrar {x|e} -idp -scf [-o+|-o-|-or] [-ai] [-tsm-] [-p<password>] <rar> <outdir>
```

- `x` (with paths) vs `e` (flat) chosen by `one_folder` / `cfg.flat_unpack()`.
- `-scf` forces UTF-8 stdout (without it, names are mojibake on Windows-built
  archives).
- Overwrite: `-o+` if `cfg.overwrite_files()`, `-o-` otherwise; `-or`
  (auto-rename) is **incompatible with `-o-`** so they're mutually exclusive.
- `-tsm-` preserves modification timestamps if `cfg.ignore_unrar_dates()` is
  off — actually, the flag *disables* timestamp restoration; check the
  config inversion in `rar_extract_core` carefully when porting.

Windows quirk: unrar's argument parsing is non-standard (it treats `\` as
escape in some contexts). `build_and_run_command(..., windows_unrar_command=
True)` normalizes the command before `CreateProcessW`.

Stdout state: `inrecovery` (true while unrar is processing recovery volumes).

| Line | Action | Fail code |
|---|---|---|
| `RAR_EXTRACTFROM_RE` ("Extracting from …") | track current rar | — |
| `RAR_EXTRACTED_RE` ("… OK") | add to extracted list | — |
| `"recovery volumes found"` | `inrecovery = True` | — |
| `"Reconstruct"` | `inrecovery = False` | — |
| `"Cannot find volume"` (not in recovery) | missing multi-volume part | 1 |
| `"CRC failed"` | corrupt data | 2 (treated as possible-password first) |
| `"File too large"` | FAT 4 GiB limit | 1 |
| `"Write error"` / `"not enough space"` | disk problem | 1 |
| `"Cannot create"` | invalid name or perms | 1 |
| `"password is incorrect"` | wrong password | 2 (try next from `nzo.password_list`) |
| `"is not RAR archive"` | not a rar | 3 |
| `"checksum error"` / `"Unexpected end"` | corrupt or passworded | 3 |

Return code semantics:

- `0`: success
- `1`: recoverable (disk/perm issue)
- `2`: wrong password → outer loop tries next password from
  `get_all_passwords(nzo)` (includes nzo.password, NZB metadata, `cfg.password()`)
- `3`: hard CRC/format failure

Multi-volume ordering (`rar_sort`): `.rar` → `.partNN.rar` → `.rNN`. Critical
because `.rar` carries the main header; processing `.r00` first will fail
spuriously.

### 8.3 7-Zip

Binary priority (`internal/unpack/sevenzip.go`): `7zz` (official, modern) →
`7zzs` (static) → `7z` (p7zip) → `7za` (p7zip legacy). Version probe:
`(\d+\.\d+).*Copyright` regex.

Command:

```
7z {x|e} -y [-aoa|-aou] [-ssc-|-ssc] [-p<password>] -o<outdir> <archive>
```

- `-y`: assume yes to all prompts.
- `-aoa` (overwrite all) vs `-aou` (rename if exists).
- `-ssc-` (case-insensitive) on Win/macOS, `-ssc` on Linux.

Output handling differs from unrar: 7-Zip's output is read **all at once**
(`p.stdout.read()`), not line-by-line. Substring searches:

| Pattern | Action | Code |
|---|---|---|
| `"Data Error in encrypted file. Wrong password?"` | retry with next pw | 2 |
| `"Disk full."` / `"No space left on device"` | hard fail | 1 |
| `"ERROR: CRC Failed"` | corrupt | 1 |
| else (exit ≥ 1) | generic failure | 1 |

Exit `2+` from 7z is fatal; `1` is "warning" and is treated as failure by
SABnzbd anyway.

### 8.4 ts join

Variant of file join for `.ts.NNN` segments — identical algorithm except the
sequence regex is `\.ts\.(\d+)$` instead of `\.(\d+)$`.

### 8.5 Tests for unpack

`tests/test_newsunpack.py`:

| Test | Input | Pins |
|---|---|---|
| `test_sfv_check_*` | Good vs bad `.sfv` | Validate ≥1 line, reject if all comments |
| `test_seven_extract_basic` | `test_7zip/testfile.7z` | Extracted list + file sizes |
| `test_unrar_password_list` | RAR with known password, list of bad+good | Iterates `get_all_passwords()`, succeeds on correct one |
| `test_unrar_obfuscated_renamed_by_par2` | Hash-named rar set, par2 maps them | Quickcheck renames before unrar runs |
| `test_filejoin_with_gap` | `.001`, `.003` (no `.002`) | Sequence error, no output written |

---

## 9. DirectUnpacker — concurrent unrar during download

`directunpacker.py`. Runs `unrar x` in parallel with the still-downloading job,
feeding it volumes as they're written. Saves the entire unpack stage of post-
processing on big jobs.

### 9.1 Eligibility

All must be true:

- `cfg.direct_unpack` on, `cfg.enable_unrar` on, unpack PP-bit set.
- First-articles obfuscation **not** detected (otherwise volume ordering is
  unknown).
- No bad articles so far.
- `RAR_PROBLEM` false (old unrar can't handle `-vp`).

### 9.2 Lifecycle

```
                     start (assembler signals first .rar done)
                      │
                      ▼
              create unrar instance ──► writes to stdin "C\n" to continue
                      │
              ┌───────┴────────┐
              ▼                ▼
   line "All OK"         line matches error pattern
              │                │
   wait for next set       abort(): kill, delete extracted, reset
              │
              ▼
   (or: stop & cleanup
     on nzo abort/kill)
```

### 9.3 Critical implementation detail: stdin/stdout

- Stdin: subprocess `PIPE`. Used to send `"C\n"` (continue) or `"Q\n"`
  (quit) when unrar prompts at volume boundaries.
- Stdout: `text_mode=False` (raw bytes). Read **character by character** until
  whitespace, because the `[C]ontinue, [Q]uit ` prompt has no trailing newline.
  A line-based reader will deadlock.

```python
while not killed:
    ch = p.stdout.read(1)
    if not ch: break
    linebuf += ch
    if ch in b" \n":
        # process linebuf as a token
```

Special tokens:

| Token | Meaning | Reply |
|---|---|---|
| `[C]ontinue,` | unrar wants next volume | write `b"C\n"` after `have_next_volume()` |
| `[R]etry,` | volume read failed | write `b"A\n"` (abort) |
| `All OK` | set finished | save to `success_sets`, wait for next set |
| Any error pattern | corrupt / pw / disk | abort() |

Bug-guard: if `[C]ontinue` shows up 10 times without volume progress (because
`have_next_volume()` keeps returning False due to obfuscation), bail out —
the assembler will never deliver that volume.

### 9.4 Volume parsing

`analyze_rar_filename`:

- `.partNN.rar` → `(setname, NN)` where setname is everything before `.partNN`.
- `.rNN` → `(setname, NN+1)` (because `.r00` is volume 2, after `.rar`).
- `.rar` → `(setname, 1)`.
- Anything else → `(None, None)` — direct-unpack ineligible.

### 9.5 Abort

`abort()`:

1. Try `stdin.write(b"Q\n")` (graceful), wait 0.2 s.
2. `p.kill()` if still running.
3. Always `p.wait()` to reap zombie.
4. Use `SABRarFile` to enumerate what unrar would have extracted; delete those
   files. If listing fails (corrupt header), delete the entire output dir if
   `one_folder` is set.
5. Clear `success_sets`.

Thread-safe via `START_STOP_LOCK`; the postproc thread may try to call
`stop()` while the unpacker thread is in the middle of a read.

---

## 10. Deobfuscation (`deobfuscate_filenames.py`)

Three-phase rename pipeline, in this order:

```
recover_par2_names()  ── 16K-MD5 lookup against par2 packets
        │
        ▼
deobfuscate()
   │
   ├── what_is_most_likely_extension()  ── file-signature based ext fix
   ├── get_biggest_file()               ── size-ratio rename heuristic
   └── deobfuscate_subtitles()          ── pair .srt to biggest file
```

### 10.1 Is-obfuscated decision

`is_probably_obfuscated(filebasename)`:

```python
DEFINITELY_OBFUSCATED = [
    r"^[a-f0-9]{32}$",           # MD5 hex
    r"^[a-f0-9.]{40,}$",          # long hex with dots
    r"\[\w+\].*[a-f0-9]{30}",     # [tag] + 30 hex
    r"^abc\.xyz",                 # known marker
]

LIKELY_CLEAN_IF = [
    upper >= 2 and lower >= 2 and spacesdots >= 1,
    spacesdots >= 3,
    alpha >= 4 and digit >= 4 and spacesdots >= 1,
    starts_upper and ratio(upper, lower) <= 0.25,  # Title Case
]
```

Default if no pattern matches: `True` (obfuscated). This is conservative —
better to falsely rename a clean file than leave an obfuscated one.

### 10.2 par2-based rename (`decode_par2`)

For each `.par2` file in the dir:

1. Parse → `Par2Set` with files indexed `By16k` (keyed by `md5of16k`).
2. For each non-par2 file in the dir:
   - Read first 16 KB.
   - `key = md5(first_16k)`.
   - If `key` in par2 dict and that entry isn't a duplicate:
     - `os.rename(actual, get_unique_filename(par2_filename))`.
3. Skip files in `IGNORED_MOVIE_FOLDERS` (`VIDEO_TS/`, `BDMV/`, `AUDIO_TS/`).

### 10.3 Extension guessing

For files with extensions not in the known-good set (or no extension), open
the file and read enough bytes to identify (`file_extension.py` has signatures
for ISO, ZIP, RAR, common video containers, PDF, PNG, JPG, etc.).

If detected: rename `xyz.bin` → `xyz.bin.iso` (preserve the original token; the
caller may want it). The next size-based pass will then rename to a useful
name.

### 10.4 Biggest-file heuristic

```python
files_by_size = sorted(filelist, key=lambda f: -size(f))
if size(files_by_size[0]) > size(files_by_size[1]) * 3.0:
    biggest = files_by_size[0]
    target = f"{usefulname}{ext(biggest)}"
    rename(biggest, target)
    # rename "siblings" (same basename, different middle suffix)
    for f in filelist:
        if same_basename(f, biggest) and f != biggest:
            rename(f, f.replace(basename(biggest), usefulname))
```

The `3.0×` ratio is empirical — small enough to catch the obvious case (one
big movie + small subs/samples), large enough to avoid multi-part collections
where files are inherently similar size.

### 10.5 Subtitle pairing

Find `.srt` files whose basename ≠ biggest-file basename. Rename to
`<biggest_basename>.<original_srt_name>`. This preserves language tags
(`14_English.srt` → `Mymovie.14_English.srt`).

### 10.6 Pre-conditions / skip rules

- Files in `IGNORED_MOVIE_FOLDERS` are never touched.
- Excluded extensions (never renamed): `.vob, .rar, .par2, .mts, .m2ts, .cpi,
  .clpi, .mpl, .mpls, .bdm, .bdmv`.
- Files < 10 MiB are not considered "main content" for the biggest-file rule
  (still eligible for subtitle pairing).

### 10.7 Tests (`tests/test_deobfuscate_filenames.py`)

| Test | Input | Expected |
|---|---|---|
| `test_is_probably_obfuscated` (parameterized) | Each entry exercises one regex branch | Match the truth table in 10.1 |
| `test_deobfuscate_filelist_lite` | `111c1c9e2bdfb5114044bf25152b7eab.bin` (15 MB), jobname `"My Important Download 2020"` | Renamed to `My Important Download 2020.bin` |
| `test_deobfuscate_big_file_small_accompanying_files` | `myiso.iso` (15 MB), `myiso.srt`, `myiso-sample.iso`, `something.txt` | ISO + SRT renamed to jobname; sample untouched (matches sample pattern); unrelated `.txt` untouched |
| `test_deobfuscate_collection_with_same_size` | 4 × 15 MB `.bin` files | No renaming (ratio < 3×) |
| `test_no_deobfuscate_DVD_dir` | Obfuscated file inside `VIDEO_TS/` | No rename |
| `test_deobfuscate_par2` | Obfuscated file + matching par2 16K-MD5 | Renamed via par2 mapping (takes precedence over size rule) |

---

## 11. Sorting (`sorting.py`) — NOT IMPLEMENTED IN GONZBD

> **This section is preserved as reference documentation only.** GoNZBD does not
> implement sorting. File organization by media type (TV, movie, date) is handled
> by external tools such as Sonarr, Radarr, and similar media managers. The API
> returns `false` for all sorting-related config toggles to satisfy third-party
> app compatibility (e.g., Sonarr category validation).

### 11.1 Activation

`Sorter.match_sorters()`:

```python
for sorter in config.get_ordered_sorters():
    if not sorter.is_active: continue
    if sorter.sort_cats and self.cat not in sorter.sort_cats: continue
    self.guess = guess_what(self.original_job_name)  # uses python-guessit
    self.type = guess["type"]
    if guess["type"] == "episode" and guess.get("date"):
        self.type = "date"
    if sorter.sort_type and self.type not in sorter.sort_type: continue
    self.sorter_active = True
    break  # first match wins
```

Types: `tv`, `movie`, `date`, `unknown`. The sorter holds the destination
template (per category, per type), which is then expanded.

### 11.2 guessit quirks (`guess_what`)

- **Names starting with a digit** confuse guessit (it thinks the digit is the
  year/episode). Workaround: prefix `"FIX"`, call guessit, strip it from
  results.
- **Anime / seasonless episodes**: if `episode` found but no `season`,
  force `season = 1` (because every sorter template expects `%s`).
- **`Setup.exe` / NZB URLs / single-file installers**: guessit will gladly
  classify them as movies. SABnzbd rejects movie-type guesses when the job is
  a single small file with a suspicious name.

### 11.3 Template placeholders

| Marker | Substitutes |
|---|---|
| `%title` / `%t` | guess["title"] (spaces) |
| `%.title`, `%_title` | dots/underscores instead of spaces |
| `%s`, `%0s` | season (`1`, `01`) |
| `%e`, `%0e` | episode |
| `%en`, `%e.n`, `%e_n` | episode title variants |
| `%y`, `%year` | year |
| `%r` | resolution (720p, 1080p, 2160p) |
| `%m`, `%0m`, `%d`, `%0d` | month/day (date-based) |
| `%dn` | original directory name |
| `%GI<prop>` | any guessit property (`%GI<video_codec>`) |
| `%1`, `%2`, … | multipart sequence number |
| `%ext` | file extension (always last expansion to avoid `%e` collision) |

Substitution is **longest-match-first** left-to-right (so `%ext` doesn't get
chewed by `%e`).

### 11.4 Cleanup phase (after substitution)

In order:

1. `{lowercase}` → `lowercase` (lowercase any `{}`-wrapped span)
2. `()` → empty
3. `..` → `.`
4. `__` → `_`
5. `"  "` → `" "`
6. ` .%ext` → `.%ext`
7. For each path component (except a Windows drive/UNC), `strip("._")`

### 11.5 Season pack + multipart

- Detected via guessit: episode list with multiple values, or single season
  with no episode marker but multiple matching files.
- For each file, run guessit again to extract per-file episode → expand
  template per-file.
- Sample files (matched by `is_sample()`) are skipped.

### 11.6 Tests (`tests/test_sorting.py`, `tests/test_functional_sorting.py`)

| Test | Input | Expected |
|---|---|---|
| `test_guess_what[setup.exe]` | `Setup.exe` | `type=unknown` (movie rejected) |
| `test_guess_what[anime]` | `Show.E08.2160p` | `type=episode, season=1, episode=8` |
| `test_guess_what[date]` | `Daily.Show.2024.03.15` | `type=date` |
| `test_guess_what[badly-named-tv]` | `25.817.hdtv-rofl` | `type=episode, season=8, episode=17` (the `FIX` prefix workaround in action) |
| `test_path_subst_*` | Template + guess dict | Template expansion matches expected path |
| `test_seasonpack_rename` | 10 episode files, sorter template | Each file renamed with its own episode number filled into `%e` |

### 11.7 Go reimplementation note

`guessit` is a battle-tested Python library — there's no Go equivalent. Two
options:

1. Port the regex sets (heavy, brittle).
2. Embed guessit via a subprocess or HTTP shim. Subprocess is simpler.

Recommendation: subprocess wrapper, parse JSON output. Cache results keyed by
filename to avoid repeated spawns within a job.

---

## 12. Filesystem safety (`filesystem.py`)

### 12.1 Sanitization

`sanitize_filename(name)`:

1. NFC-normalize.
2. Translate illegal chars to `_`:
   - Always: `\0`, `/`
   - Windows or `cfg.sanitize_safe()`: `\\/<>?*|":` + control chars `\x01-\x1f`
   - macOS: also `:` (par2 metadata is colon-separated; preserves quickcheck)
3. `replace_win_devices()`: prefix with `_` if name is `CON`/`PRN`/`AUX`/`NUL`/
   `COMx`/`LPTx` (or starts with `name.`). Replace leading `$MFT` with `SMFT`.
4. Truncate to `MaxFileNameLen` (245 UTF-8 bytes, = 255 − 10). Preserve extension —
   `DEF_FILE_EXTENSION_MAX` bytes max for ext, rest is name.
5. Lowercase `.par2` extension (par2 binary is case-sensitive on Linux).

`sanitize_foldername(name)`: same plus:

- `rstrip(".")` and `rstrip(" ")` in a loop (Windows hates trailing dots and
  spaces, silently strips them and breaks downstream paths).
- Honors `cfg.max_foldername_length()`.

### 12.2 Collisions

`get_unique_filename(path)`:

```
file.iso  exists → file.1.iso
file.1.iso exists → file.2.iso
…
```

Suffix is inserted **before** the extension; pure-suffix collision logic.

`get_unique_dir(path, create=True)`: same, but for directories.

### 12.3 Cross-filesystem move

```python
try:
    os.rename(src, dst)         # cheap, atomic, same-fs
except OSError:
    shutil.copyfile(src, dst)   # cross-fs fallback
    os.remove(src)
```

If `cfg.overwrite_files()` is off and `dst` exists: `dst =
get_unique_filename(dst)` first.

### 12.4 Permissions (Unix)

`set_permissions(path, recursive)`:

- If `cfg.permissions()` set: `chmod` to that octal value.
- Otherwise: strip `S_ISUID | S_ISGID | S_IXUSR | S_IXGRP | S_IXOTH` (remove
  execute & suid bits, but preserve r/w).
- Walks the tree if `recursive`.

Windows: no-op. ACL handling is out of scope.

### 12.5 Long paths (Windows)

`long_path(p)`:

- If already `\\?\…`: return as-is.
- If UNC (`\\server\share\…`): `\\?\UNC\server\share\…`.
- Else: `\\?\` + p.

`clip_path(p)`: inverse — strip the `\\?\` prefix for display/script args.

### 12.6 Rename retry loop (Windows)

`renamer(src, dst)`:

```python
for attempt in range(3):
    try: return os.rename(src, dst)
    except OSError as e:
        if e.winerror == 17:  # cross-disk
            return shutil.move(src, dst)
        if e.winerror in (32, 5):  # busy, access denied
            time.sleep(2)
            continue
        raise
return shutil.move(src, dst)  # last-ditch
```

The 2-second sleep handles the common case where Windows Explorer or an
indexer briefly opens a file SABnzbd just wrote.

### 12.7 Tests (`tests/test_filesystem.py`)

| Test | Input | Expected |
|---|---|---|
| `test_sanitize_filename_*` | Names with each illegal char | `_` substitution; NFC normalized |
| `test_sanitize_foldername_trailing_dots` | `"Foo..."` | `"Foo"` (trailing dots stripped) |
| `test_get_unique_filename` | `file.iso` (exists), `file.1.iso` (exists) | `file.2.iso` |
| `test_replace_win_devices` | `CON.txt`, `con`, `$MFT` | `_CON.txt`, `_con`, `SMFT` |
| `test_long_path_*` | local, UNC, already-prefixed | Correct `\\?\` form |
| `test_move_to_path_cross_fs` | Move across simulated mounts | Falls back to copy+unlink |
| `test_renamer_busy_retry` (Windows) | Mock `os.rename` raising winerror 32 | Retries 3×, then `shutil.move` |

---

## 13. Encoding (`encoding.py`)

Functions to port:

- `unicode_nfc_normalize(s)`: NFC normalize. Always applied to filenames.
- `correct_unknown_encoding(b)`: try UTF-8 (surrogateescape) → ISO-8859-1 →
  `charset_normalizer.from_bytes()` ML fallback. Used by par2 filename decode
  and NZB subject parsing.
- `platform_btou(b)`: decode subprocess stdout using the locale's preferred
  encoding (`locale.getpreferredencoding(False)`), falling back to UTF-8 with
  replacement.

Go: `unicode/norm` (NFC), `golang.org/x/text/encoding/ianaindex` +
`golang.org/x/text/encoding/charmap` for fallback. There's no clean charset-
auto-detect lib; `golang.org/x/net/html/charset` is close.

---

## 14. End-to-end corner-case index

Quick lookup table of behavior tests across the pipeline. **Each entry is a
behavior any reimplementation must preserve.**

| # | Behavior | Pinned by |
|---|---|---|
| 1 | yEnc article with missing CRC → no retry if precheck off | `test_decoder.py::test_short_data` |
| 2 | `>5` bad articles → immediate job abort | constants + integration tests |
| 3 | Non-contiguous write invalidates `nzf.crc32` | `test_assembler.py::test_assemble_direct_append_direct_append` |
| 4 | Append mode is sticky once entered for a file | `test_assemble_direct_write_aborted_to_append` |
| 5 | Restart mid-assembly resumes via `assembler_next_index` + `Article.on_disk` | `test_force_force_direct` |
| 6 | par2 with mixed-encoding filename round-trips correctly | `test_par2file.py::test_parse_par2_french_german_demo` |
| 7 | par2 entries with duplicate 16K-MD5 are excluded from rename map (issue #3164) | `test_par2file.py::test_duplicate_16k` |
| 8 | Quickcheck renames obfuscated files by `(crc32, size)` match | `test_postproc.py::test_rar_renamer_obfuscated_single_rar_set` |
| 9 | par2 "need more blocks" re-queues at `REPAIR_PRIORITY` | `postproc.py:parring()` (no direct unit test; functional) |
| 10 | par2 "main packet not found" triggers smallest-par2 redownload | `newsunpack.py:1224` |
| 11 | unrar wrong-password (`exit 2`) iterates `get_all_passwords` | `test_newsunpack.py::test_unrar_password_list` |
| 12 | unrar `.rar` must process before `.r00` | `rar_sort` ordering |
| 13 | DirectUnpacker reads stdout char-by-char to catch prompt without newline | `directunpacker.py:187` |
| 14 | DirectUnpacker bails after 10 duplicate `[C]ontinue` (obfuscated next-volume) | `directunpacker.py:333` |
| 15 | filejoin sequence gaps fail without partial output | `test_newsunpack.py::test_filejoin_with_gap` |
| 16 | Deobfuscation skipped inside `VIDEO_TS/`/`BDMV/` | `test_deobfuscate_filenames.py::test_no_deobfuscate_DVD_dir` |
| 17 | Biggest-file rule requires >3× size ratio | `test_deobfuscate_collection_with_same_size` |
| 18 | par2 rename takes precedence over size rule | `test_deobfuscate_par2` |
| 19 | `.srt` siblings of biggest file are renamed alongside | `test_deobfuscate_big_file_small_accompanying_files` |
| 20 | Sorter rejects `Setup.exe`-style movie misclassifications | `test_sorting.py::test_guess_what[setup.exe]` |
| 21 | Anime-style seasonless episodes default to season 1 | `test_guess_what[anime]` |
| 22 | Names starting with a digit are prefixed `FIX` before guessit | `test_guess_what[badly-named-tv]` |
| 23 | Folder trailing dots/spaces are stripped (Windows compat) | `test_sanitize_foldername_trailing_dots` |
| 24 | Windows reserved names (`CON`, `PRN`, …) prefixed `_` | `test_replace_win_devices` |
| 25 | Cross-fs move falls back to copy+unlink | `test_move_to_path_cross_fs` |
| 26 | Windows rename retries on `winerror 32`/`5` for 3 attempts × 2 s | `test_renamer_busy_retry` |
| 27 | macOS sanitizes `:` (par2 metadata uses it) | `test_sanitize_filename_macos` |
| 28 | Path collision adds `.1`/`.2`/… suffix before extension | `test_get_unique_filename` |
| 29 | Category dir ending in `*` suppresses per-job subfolder | `test_prepare_extraction_path` (param) |
| 30 | `_UNPACK_` prefix used iff `folder_rename` is on | `test_prepare_extraction_path` (param) |
| 31 | Script runs even when job failed; receives `status=1/2/3` arg | `postproc.py:585` |
| 32 | Script non-zero exit only fails job if `cfg.script_can_fail()` | `postproc.py:596` |
| 33 | History `script_log` is zlib-compressed | `database.py` |
| 34 | History `script_line` is last non-empty stdout line, ≤ 150 chars | `newsunpack.py:1217` |
| 35 | `recover_par2_names` runs only if `cfg.process_unpacked_par2` | `postproc.py:576` |
| 36 | NFC normalization applied uniformly to all filenames (incl. on macOS NFD source) | `encoding.py:31` |
| 37 | par2 scan stops at 10 MiB once all FileDesc seen | `par2file.py:201` |
| 38 | 7-Zip reads stdout in one shot, not line-by-line | `newsunpack.py:980` |
| 39 | Recursive unpack capped at depth 3 | `newsunpack.py:277` |
| 40 | Files listed in `used_joinables`/`used_for_repair` are never deleted in delete-sources stage | par2 return contract |

---

## 15. Go reimplementation sketch

> **Status:** This section was the pre-implementation design sketch. The system
> has since been built; names below have been updated to the realized code. The
> supervisor type is `postproc.PostProcessor` and the real `postproc.Job` wraps
> a `*queue.Job` (it does not own the flat field set shown here — the struct
> below remains only as a conceptual illustration of the inputs). See
> `internal/postproc/postproc.go` and `internal/app/stages.go` for the actual
> stage wiring.

```go
package postproc

// PostProcessor is the post-download supervisor.
type PostProcessor struct {
    cfg    *Config
    db     *History
    binDir BinaryPaths
    log    *slog.Logger
}

// Job (conceptual) is the input contract from the downloader. The real
// postproc.Job wraps *queue.Job and exposes DownloadDir / FinalDir rather than
// the flat fields shown here.
type Job struct {
    NzoID         string
    DownloadPath  string
    FinalName     string
    Category      string
    PP            PPBits        // R|U|D
    Script        string
    Password      string
    PasswordList  []string
    NzbFilename   string
    Files         []*JobFile    // assembled output
    Par2Packs     map[string]*Par2Set  // keyed by setname
    BytesMissing  int64
    NzoInfo       map[string]string
    DirectUnpack  *DirectUnpacker  // nil if not run during DL
}

type JobFile struct {
    Filename string
    Bytes    int64
    CRC32    *uint32   // nil if non-contiguous
    MD5of16K []byte    // from first article
    Path     string    // absolute, in DownloadPath
}

func (p *PostProcessor) Process(ctx context.Context, j *Job) (*Result, error) {
    // Realized stage order (internal/app/stages.go buildStages):
    //  1. quickcheck        (par2 vs assembled CRC; relocate flat files)
    //  2. repair            (par2 verify/repair)
    //  3. unpack            (filejoin → unrar → 7z, depth ≤ 3)
    //  4. sample_cleanup    (remove samples if enabled)
    //  5. par2names         (recover obfuscated names from par2)
    //  6. par2_cleanup      (delete .par2 once no longer needed)
    //  7. deobfuscate       (heuristic rename)
    //  8. extension_cleanup (delete files matching cleanup list)
    //  9. finalize          (move to complete dir) — runs BEFORE script
    // 10. cleanup           (remove __ADMIN__ and temp state)
    // 11. script            (user post-processing script; sees final dir)
    // Sorting (TV/movie templates) is NOT implemented — see §11.
    // Stage errors are recorded in the StageLog but do NOT abort the pipeline.
}
```

Suggested package split:

Realized package split:

- `internal/par2` — packet parser, CRC reconstruction (no I/O); also hosts the
  quick-check / par2-vs-JobFile match logic (`quickcheck.go`, `verifycrc.go`)
- `internal/assembler` — write strategy (interface for fs)
- `internal/unpack` — subprocess drivers (`Par2Repair`, `Unrar`, `SevenZip`,
  `FileJoin`); shared streaming abstraction
- `internal/directunpack` — concurrent unrar (depends on `unpack.Unrar`)
- `internal/deobfuscate` — heuristics
- `internal/fsutil` — sanitize, rename retries, name-length limits, collisions
  (no separate `safefs`/`sort` packages; sorting is NOT implemented)
- `internal/postproc` — the supervisor (ties it together, owns the DB)

Concurrency: one `PostProcessor` worker per host (matches SABnzbd's behavior). The
`DirectUnpacker` is the only concurrent piece and is owned by the downloader,
not the pipeline. Use `context.Context` for cancel/abort throughout.

Subprocess discipline:

- Use `exec.CommandContext` so `ctx.Cancel()` kills children.
- For unrar/par2: `bufio.Scanner` with `ScanLines`.
- For DirectUnpacker: raw `io.Reader.Read([]byte, 1)` loop to avoid
  prompt-deadlock.
- For 7-Zip: `io.ReadAll(stdout)` — matches Python's behavior.
- Always `cmd.Wait()` after `cmd.Process.Kill()` to reap.

DB: SQLite via `modernc.org/sqlite` (pure Go, no cgo) is simplest. Schema
matches §6.7 verbatim if you want migration compatibility; otherwise design
fresh and provide an importer.

---

## 16. Open questions / things to verify before porting

- **Exact byte boundaries of par2 packet padding** when filename's UTF-8 length
  is a multiple of 4 — does SABnzbd require trailing nulls, or treat them as
  optional? Inspect `par2file.py:172` carefully against a fixture.
- **`crc32_zero_unpad` algebra**: re-derive from zlib source before porting;
  the exact formula matters for tail-of-slice handling on non-multiple file
  sizes.
- **Windows `WerErr_FileInUse` distinct from `ERROR_ACCESS_DENIED`**: the
  retry loop handles both as equivalent; on NTFS with antivirus, they can
  mean different things. Worth keeping the retry but adding a debug log
  to distinguish.
- **macOS APFS clone-on-write**: a same-fs `rename` is already atomic; no
  perf concern. But cross-volume on a Time Capsule mount fails with
  `EXDEV` — make sure the fallback path is exercised in CI.
- **guessit version pinning**: SABnzbd pins a specific guessit release because
  newer versions change classification for ambiguous strings. If using a
  subprocess shim, pin the version explicitly.

---

*End of spec.*
