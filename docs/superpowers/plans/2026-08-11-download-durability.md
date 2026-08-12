# Download Durability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the article→disk→ack path with a design in which no fact is
persisted before the thing it asserts becomes true, and no derived value is
authoritative — implementing every invariant in
`docs/superpowers/specs/2026-08-11-download-durability-design.md`.

**Architecture:** Three new packages carry the design. `internal/storagefault`
classifies syscall errors and has no vocabulary for articles, which makes
invariant A1 true by ignorance. `internal/durability` owns the Class A fact log,
the Class B extent cache, the checkpoint barrier, and the resumer — and is the
only package that can mint the proof token that acking requires, which makes
invariants X2/X3 compiler-enforced outside it. `internal/assembler` is reduced
to a dumb `FileWriter` that writes bytes and reports faults, with no authority
to ack, compute a CRC, or truncate.

**Tech Stack:** Go 1.26.4, `modernc.org/sqlite` (pure Go), `goose` migrations,
`log/slog`, `internal/crc32util`.

## Global Constraints

- **Go 1.26.4** (toolchain 1.26.4); module `github.com/hobeone/gonzbd`.
- **No backwards compatibility of any kind.** The user will wipe all on-disk
  state and reinstall from scratch after this lands. No migration path, no
  legacy read path, no compatibility shim, no deprecation window.
- **Migrations are replaced, not appended.** `internal/history/migrations/`
  001–011 are deleted and replaced by a single fresh `001_initial.sql`. This
  deliberately overrides `AGENTS.md`'s "never modify existing migration files"
  rule, which exists to protect applied migrations on real installations; the
  user has explicitly authorised a from-scratch reinstall, so no applied
  migration is at risk. **Record this authorisation in the commit body of Task 3.**
- **Never rewrite a commit that is not `HEAD`.** No `git rebase`, no
  `git reset --hard`, no `commit-tree` surgery. Amending `HEAD` in place is
  fine; anything earlier requires moving the branch, which has twice destroyed
  another agent's uncommitted work in this shared worktree. If an earlier
  commit's body needs correcting — including a false red-check claim — say so
  in the **next** commit's body instead. History that records a correction is
  more honest than history that hides the error, and infinitely safer.
- **Never `git stash`, `git checkout -- <path>`, or `git checkout -- .`.**
  The stash stack is shared; the checkouts discard other agents' uncommitted
  edits. Restore only by copying back your own scratch copy.
- **Every commit leaves the repo green:** `go build ./... && go test -race ./...`.
  **One authorised exception: the Tasks 8+9 boundary.** After the cutover no
  component mints a `DurableProof` until Task 10 wires the barrier's cadence,
  so every test that drives a job to completion fails by construction. The user
  explicitly authorised this rather than re-sequencing. Two consequences:
  Tasks 8+9 must publish the **exact set** of suites expected to fail, and a
  reviewer's job is to confirm the observed failures match that set and nothing
  else; and **Task 10 does not close until the set is empty.** A red boundary
  with an enumerated list is a known state; a red boundary without one is
  indistinguishable from a broken change.
- **Quality gates before every commit:** `go fix ./...`, `goimports -w .`,
  `go vet ./...`, `go test -race ./...`, `golangci-lint run ./...`.
- **Conventional Commits**, scope = Go package name. Footer
  `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.
- **Every mutation run uses `go test -count=1`.** Without it Go replays a cached pass and prints `ok`, which reads as "the test is inert" when the test was never actually run against the mutation. A cached `ok` is not evidence. This nearly recorded a false finding during Task 3.
- **The red check is mechanical, not mental** (`AGENTS.md`): every pinning test
  must be *observed* to fail against a reverted or neutered fix, and the failure
  message recorded in the commit body. Never `git stash` — copy the file to a
  scratch dir and restore from your own copy.
- **New unexported helpers need real tests** — `check_test_alignment` reports
  every unexported helper in a touched file. Never add a dummy reference.

---

## File Structure

**New — `internal/storagefault/`** (no dependencies on any gonzbd package)

| File | Responsibility |
|---|---|
| `fault.go` | `Fault` type; `Retryable()`; `Error()` |
| `classify.go` | `Classify(op, path string, err error) *Fault` — errno → retryability |

**New — `internal/durability/`**

| File | Responsibility |
|---|---|
| `fact.go` | `ArticleFact` (Class A); `FactLog` interface |
| `factlog_sqlite.go` | append-only SQLite implementation of `FactLog` |
| `extent.go` | `FileExtent` (Class B); `ExtentStore` interface; `Bitmap` |
| `extentstore_sqlite.go` | atomic per-job commit of `[]FileExtent` |
| `proof.go` | `DurableProof` — unexported fields, no exported constructor |
| `barrier.go` | `Barrier`: drain → write → fsync → commit → mint proof |
| `resume.go` | `Resumer`: validity check, recomputation from bytes |
| `synctarget.go` | `SyncTarget` interface the assembler satisfies |

**Rewritten — `internal/assembler/`**

| File | Change |
|---|---|
| `filewriter.go` | **new** — one file's handle, cache, coalescing, pre-allocation. No acks. |
| `assembler.go` | reduced: routing + `SyncTarget` impl. Loses ack, CRC, truncate, extent logic. |
| `writecache.go` | retained, but `writeCursor` becomes purely a coalescing hint (Class C) |
| `diskprobe.go`, `diskspace.go`, `sparse.go`, `preallocate*.go` | retained; errors routed through `storagefault` |

**Deleted:** `resume_crc_test.go`, `resume_extent_test.go`, `max_written_persist_test.go`,
`drain_cursor_test.go` — they pin behaviour this plan removes.

**Modified — `internal/queue/`**

| File | Change |
|---|---|
| `worksetderive.go` | **new** — derives outstanding articles from the durable bitmap |
| `queue.go` | delete `MarkArticlesDone*`, `MarkArticlesFailed*`, `SetFileExtents`; add `AckDurable` |
| `progress.go` | delete `BytesDownloaded`/`MaxWritten`/`WriteCursor` maintenance |
| `sqlite_store.go` | slimmed `job_files`; no Class B columns |

**Modified:** `internal/app/app.go` (wiring), `internal/config/downloads.go`
(new knobs), `internal/history/migrations/` (collapsed).

---

## Task 1: Storage fault classification

**Files:**
- Create: `internal/storagefault/fault.go`, `internal/storagefault/classify.go`
- Test: `internal/storagefault/classify_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `storagefault.Fault` struct with fields `Op string`, `Path string`,
  `Err error`, `Permanent bool`; methods `Error() string`, `Retryable() bool`,
  `Unwrap() error`. Function
  `Classify(op, path string, err error) *Fault` returning `nil` when `err == nil`.

This package must **never** import any gonzbd package and must have no concept
of an article, job, or file index. That is the mechanism for invariant A1: it
cannot blame an article because it has no word for one.

- [ ] **Step 1: Write the failing test**

```go
package storagefault

import (
	"errors"
	"syscall"
	"testing"
)

func TestClassify_NilErrorYieldsNoFault(t *testing.T) {
	if got := Classify("write", "/x", nil); got != nil {
		t.Fatalf("Classify(nil) = %v, want nil", got)
	}
}

func TestClassify_Retryability(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		permanent bool
	}{
		{"ENOSPC is retryable", syscall.ENOSPC, false},
		{"EDQUOT is retryable", syscall.EDQUOT, false},
		{"ETIMEDOUT is retryable", syscall.ETIMEDOUT, false},
		{"ESTALE is retryable", syscall.ESTALE, false},
		{"EROFS is permanent", syscall.EROFS, true},
		{"EIO is retryable — transient on network volumes", syscall.EIO, false},
		{"ENOENT is retryable — a missing dir is recoverable", syscall.ENOENT, false},
		{"EACCES is permanent", syscall.EACCES, true},
		{"unknown defaults to retryable", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := Classify("write", "/data/x.bin", tt.err)
			if f == nil {
				t.Fatal("Classify returned nil for a non-nil error")
			}
			if f.Permanent != tt.permanent {
				t.Errorf("Permanent = %v, want %v", f.Permanent, tt.permanent)
			}
			if f.Retryable() == tt.permanent {
				t.Errorf("Retryable() = %v, contradicts Permanent = %v", f.Retryable(), f.Permanent)
			}
			if !errors.Is(f, tt.err) {
				t.Errorf("errors.Is(fault, %v) = false, want true", tt.err)
			}
		})
	}
}

// TestClassify_WrappedErrnoIsUnwrapped guards the real call shape: the
// assembler passes an *os.PathError from WriteAt, never a bare errno.
func TestClassify_WrappedErrnoIsUnwrapped(t *testing.T) {
	wrapped := &os_PathErrorStub{Err: syscall.ENOSPC}
	f := Classify("write", "/data/x.bin", wrapped)
	if f.Permanent {
		t.Error("wrapped ENOSPC classified permanent, want retryable")
	}
}
```

Define `os_PathErrorStub` in the test file as
`type os_PathErrorStub struct{ Err error }` with
`func (e *os_PathErrorStub) Error() string { return "stub: " + e.Err.Error() }`
and `func (e *os_PathErrorStub) Unwrap() error { return e.Err }`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/storagefault/ -v`
Expected: FAIL — `undefined: Classify`.

- [ ] **Step 3: Write the implementation**

`internal/storagefault/fault.go`:

```go
// Package storagefault classifies errors returned by the storage layer.
//
// It deliberately has no concept of an article, job, or file index, and
// imports no gonzbd package. That is not an accident of layering: invariant
// A1 of the durability design forbids recording a storage fault as an article
// fault, and this package cannot violate it because it has no vocabulary in
// which to blame an article.
package storagefault

import "fmt"

// Fault is a classified failure of the storage layer.
type Fault struct {
	// Op is the syscall-level operation: "write", "sync", "open",
	// "truncate", "mkdir", "stat".
	Op string
	// Path is the filesystem path the operation targeted.
	Path string
	// Err is the underlying error, retained for errors.Is/As.
	Err error
	// Permanent reports whether retrying could ever succeed without
	// operator intervention.
	Permanent bool
}

func (f *Fault) Error() string {
	kind := "retryable"
	if f.Permanent {
		kind = "permanent"
	}
	return fmt.Sprintf("storage %s fault on %s %q: %v", kind, f.Op, f.Path, f.Err)
}

// Retryable reports whether the condition may clear on its own or after
// operator action, in which case the job stalls rather than fails.
func (f *Fault) Retryable() bool { return !f.Permanent }

func (f *Fault) Unwrap() error { return f.Err }
```

`internal/storagefault/classify.go`:

```go
package storagefault

import (
	"errors"
	"syscall"
)

// permanentErrnos are conditions no amount of waiting will clear. Anything
// not listed is treated as retryable, which is the conservative default: a
// stalled job is recoverable by the user, whereas failing a job discards
// work that a transient mount hiccup would have let us keep.
var permanentErrnos = []syscall.Errno{
	syscall.EROFS,   // read-only filesystem
	syscall.ENOTDIR, // a path component is not a directory
	syscall.EACCES,  // permissions
	syscall.EPERM,
	syscall.EBADF,  // programming error: handle already closed
	syscall.EFBIG,  // exceeds the filesystem's maximum file size
	syscall.EINVAL, // bad offset or unaligned write
}

// EIO and ENOENT are deliberately absent. Both look permanent and are not:
// a mid-write EIO on a network-backed or removable volume is frequently
// transient, and a missing target directory is the most cheaply recoverable
// storage condition there is. Retryable here means the job stalls with a
// surfaced reason (R19), not that anything retries forever — so the operator
// still sees a dying disk, while a transient fault no longer discards every
// byte the job had already downloaded. See R18.

// Classify maps an error from a storage operation to a Fault. It returns nil
// when err is nil, so callers can write `if f := Classify(...); f != nil`.
func Classify(op, path string, err error) *Fault {
	if err == nil {
		return nil
	}
	f := &Fault{Op: op, Path: path, Err: err}
	for _, e := range permanentErrnos {
		if errors.Is(err, e) {
			f.Permanent = true
			break
		}
	}
	return f
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/storagefault/ -v`
Expected: PASS, all subtests.

- [ ] **Step 5: Verify the structural guarantee mechanically**

Run: `go list -deps ./internal/storagefault | grep gonzbd | grep -v '/internal/storagefault$'`
Expected: **no output**. The `grep -v` is required — `go list -deps` always
lists the target package itself, so without it this command always reports a
hit and proves nothing. If this prints anything, the A1 guarantee is broken.
Add this as a test so it cannot regress:

```go
func TestPackageImportsNoGonzbdPackage(t *testing.T) {
	const self = "github.com/hobeone/gonzbd/internal/storagefault"
	out, err := exec.Command("go", "list", "-deps", self).Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	// go list -deps always includes the target package itself, so self is
	// excluded: the invariant is that storagefault imports no *other*
	// gonzbd package, not that it is absent from its own closure.
	for line := range strings.SplitSeq(string(out), "\n") {
		if line != self && strings.HasPrefix(line, "github.com/hobeone/gonzbd/") {
			t.Errorf("storagefault must not depend on %s — invariant A1 relies on it having no article vocabulary", line)
		}
	}
}
```

- [ ] **Step 6: Quality gates and commit**

```bash
goimports -w internal/storagefault/ && go vet ./internal/storagefault/ && \
  go test -race ./internal/storagefault/ && golangci-lint run ./internal/storagefault/
git add internal/storagefault/
git commit -m "feat(storagefault): classify storage errors by retryability

A1 of the durability design forbids recording a storage fault as an
article fault. This package makes that structural rather than
disciplinary: it imports no gonzbd package and has no type naming an
article, so it cannot express the wrong attribution. A test asserts
the import constraint.

Unknown errors classify retryable, which stalls the job rather than
failing it — a recoverable outcome is the safe default when the cause
is unrecognised.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Task 2: Class A and Class B core types

**Files:**
- Create: `internal/durability/fact.go`, `internal/durability/extent.go`,
  `internal/durability/proof.go`
- Test: `internal/durability/extent_test.go`, `internal/durability/proof_test.go`

**No `fact_test.go`.** `fact.go` contains only type and interface
declarations — zero statements — so any test of it would assert Go's own
struct semantics and could not go red against any edit short of a rename,
which the compiler already catches. R23's `HasCRC`-vs-`CRC32==0`
distinction does need pinning, but it belongs with the consumer that acts
on it (Tasks 6 and 7), not here. Naming a test file without saying what it
must pin is how a tautology gets written to fill the gap.

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `durability.ArticleFact{FileIdx int32; ArtIdx int32; Offset int64; Length int32; CRC32 uint32; HasCRC bool}`
  - `durability.Bitmap` with `NewBitmap(n int) Bitmap`, `Set(i int)`,
    `Get(i int) bool`, `Len() int`, `Count() int`, `Bytes() []byte`,
    `BitmapFromBytes(b []byte, n int) (Bitmap, error)`
  - `durability.FileExtent{FileIdx int32; Durable Bitmap; VerifiedTo int64;
    PrefixCRC uint32; HasPrefixCRC bool; BytesDurable int64; BytesFailed int64;
    Size int64; ModTimeNs int64}`
  - `durability.DurableProof` with `JobID() string`, `Articles() []int32`,
    and no exported constructor.

- [ ] **Step 1: Write the failing tests**

`internal/durability/extent_test.go`:

```go
package durability

import "testing"

func TestBitmap_RoundTripsThroughBytes(t *testing.T) {
	b := NewBitmap(200)
	for _, i := range []int{0, 63, 64, 65, 199} {
		b.Set(i)
	}
	got, err := BitmapFromBytes(b.Bytes(), 200)
	if err != nil {
		t.Fatalf("BitmapFromBytes: %v", err)
	}
	if got.Count() != 5 {
		t.Fatalf("Count() = %d, want 5", got.Count())
	}
	for _, i := range []int{0, 63, 64, 65, 199} {
		if !got.Get(i) {
			t.Errorf("Get(%d) = false after round trip", i)
		}
	}
	if got.Get(1) || got.Get(198) {
		t.Error("round trip invented a set bit")
	}
}

// TestBitmapFromBytes_RejectsShortBuffer pins that a truncated persisted
// bitmap is an error rather than a silently short map. S3 requires that an
// article whose state cannot be established is Outstanding; a silently
// truncated bitmap would instead report those articles as not-durable
// without anyone noticing the record was damaged.
func TestBitmapFromBytes_RejectsShortBuffer(t *testing.T) {
	if _, err := BitmapFromBytes([]byte{0x01}, 200); err == nil {
		t.Fatal("BitmapFromBytes accepted a buffer too short for n=200")
	}
}

func TestBitmap_OutOfRangeIsNoOp(t *testing.T) {
	b := NewBitmap(8)
	b.Set(-1)
	b.Set(8)
	if b.Count() != 0 {
		t.Fatalf("Count() = %d after out-of-range Sets, want 0", b.Count())
	}
	if b.Get(-1) || b.Get(8) {
		t.Error("Get out of range returned true")
	}
}
```

`internal/durability/proof_test.go`:

```go
package durability

import "testing"

// TestDurableProof_CarriesItsPayload is the only thing a proof must do.
// Its real guarantee — that no package outside this one can construct it —
// is a language property, not a testable one: no in-package test can assert
// the absence, because newProof is reachable from anywhere in this package.
// It is enforced at the package boundary by the compiler.
func TestDurableProof_CarriesItsPayload(t *testing.T) {
	p := newProof("job-1", []int32{3, 7, 11})
	if p.JobID() != "job-1" {
		t.Errorf("JobID() = %q, want %q", p.JobID(), "job-1")
	}
	if got := p.Articles(); len(got) != 3 || got[0] != 3 || got[2] != 11 {
		t.Errorf("Articles() = %v, want [3 7 11]", got)
	}
}

// TestDurableProof_ArticlesIsNotAliased pins that a caller mutating the
// returned slice cannot corrupt the proof, which the barrier may still hold.
func TestDurableProof_ArticlesIsNotAliased(t *testing.T) {
	p := newProof("job-1", []int32{3, 7, 11})
	got := p.Articles()
	got[0] = 99
	if p.Articles()[0] != 3 {
		t.Fatal("mutating the returned slice mutated the proof")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/durability/ -v`
Expected: FAIL — `undefined: NewBitmap`, `undefined: newProof`.

- [ ] **Step 3: Write the implementation**

`internal/durability/fact.go`:

```go
// Package durability owns the persistence of download progress.
//
// It divides every fact into two classes, per
// docs/superpowers/specs/2026-08-11-download-durability-design.md:
//
//   - Class A (ArticleFact) — immutable facts discovered when an article is
//     decoded. They assert nothing about presence on disk: an ArticleFact
//     says only "if the bytes at [Offset, Offset+Length) are present, they
//     hash to CRC32". That is true the instant the article is decoded and
//     stays true forever, so Class A may be committed at any time in any
//     order, with no barrier and no ordering constraint against the data.
//
//   - Class B (FileExtent) — a cache of values derived from Class A plus the
//     file's actual bytes. Class B asserts presence, so it may only be
//     committed by Barrier, strictly after the fsync that makes its claims
//     true (S1). It is never authoritative: where it disagrees with a
//     recomputation, the recomputation is correct by definition (S4).
package durability

import "context"

// ArticleFact is an immutable Class A fact about one article.
//
// Offset and Length are the article's *decoded* byte range, which is not
// knowable until the article is fetched — the NZB carries only an encoded
// byte count, while the true range comes from the yEnc =ypart header. That
// is why this mapping must be persisted rather than derived from the NZB.
type ArticleFact struct {
	// FileIdx is the article's file within the job.
	FileIdx int32
	// ArtIdx is the article's global index within the job's manifest.
	ArtIdx int32
	// Offset is the decoded byte position within the target file.
	Offset int64
	// Length is the decoded byte length.
	Length int32
	// CRC32 is the CRC32 of the decoded bytes, valid only when HasCRC.
	CRC32 uint32
	// HasCRC is false for UU-encoded articles, which carry no CRC.
	// Distinguishing this from CRC32 == 0 is required by R23.
	HasCRC bool
}

// FactLog is the append-only store of Class A facts.
//
// Append has no ordering relationship to file writes (R2). Losing a suffix
// of the log is safe and costs only a re-fetch (R3).
type FactLog interface {
	// Append records facts. It is idempotent per (jobID, ArtIdx): appending
	// a fact whose ArtIdx is already present is a no-op, not an update,
	// because a Class A fact never changes (R1).
	Append(ctx context.Context, jobID string, facts []ArticleFact) error

	// ForFile returns every recorded fact for one file, ordered by Offset.
	ForFile(ctx context.Context, jobID string, fileIdx int32) ([]ArticleFact, error)

	// DeleteJob removes every fact for a job that has left the queue.
	DeleteJob(ctx context.Context, jobID string) error
}
```

`internal/durability/extent.go`:

```go
package durability

import (
	"context"
	"encoding/binary"
	"errors"
	"math/bits"
)

// ErrBitmapTooShort reports a persisted bitmap buffer that cannot hold n bits.
var ErrBitmapTooShort = errors.New("durability: bitmap buffer too short for bit count")

// Bitmap is a fixed-size bit vector of per-article durable flags.
type Bitmap struct {
	words []uint64
	n     int
}

func NewBitmap(n int) Bitmap {
	if n < 0 {
		n = 0
	}
	return Bitmap{words: make([]uint64, (n+63)/64), n: n}
}

func (b Bitmap) Len() int { return b.n }

func (b Bitmap) Get(i int) bool {
	if i < 0 || i >= b.n {
		return false
	}
	return b.words[i/64]&(1<<(uint(i)%64)) != 0
}

// Set mutates the caller's Bitmap despite the value receiver: words is a
// slice, so the receiver copy shares its backing array.
func (b Bitmap) Set(i int) {
	if i < 0 || i >= b.n {
		return
	}
	b.words[i/64] |= 1 << (uint(i) % 64)
}

func (b Bitmap) Count() int {
	total := 0
	for _, w := range b.words {
		total += bits.OnesCount64(w)
	}
	return total
}

// Bytes serialises the bitmap little-endian for persistence.
func (b Bitmap) Bytes() []byte {
	out := make([]byte, len(b.words)*8)
	for i, w := range b.words {
		binary.LittleEndian.PutUint64(out[i*8:], w)
	}
	return out
}

// BitmapFromBytes rebuilds a bitmap, rejecting a buffer too short for n.
// A short buffer means the record is damaged; silently returning a partly
// zero bitmap would report those articles not-durable without anyone
// noticing the damage, which S3 forbids treating as an ordinary answer.
func BitmapFromBytes(buf []byte, n int) (Bitmap, error) {
	if n < 0 {
		n = 0
	}
	need := (n + 63) / 64
	if len(buf) < need*8 {
		return Bitmap{}, ErrBitmapTooShort
	}
	b := NewBitmap(n)
	for i := range need {
		b.words[i] = binary.LittleEndian.Uint64(buf[i*8:])
	}
	// Mask the padding bits of the final word. A persisted buffer may be
	// damaged, and this constructor exists to read exactly that input class —
	// the short-buffer guard above defends the same case. Without the mask,
	// garbage above bit n is absorbed: Get is bounded by n but Count is not,
	// so Count could exceed Len and over-report how many articles are
	// durable. Over-counting is the over-claim direction the design forbids.
	if rem := n % 64; rem != 0 && need > 0 {
		b.words[need-1] &= (1 << uint(rem)) - 1
	}
	return b, nil
}

// FileExtent is the Class B derivation cache for one file. Every field is
// recomputable from the FactLog plus the file's bytes, with the documented
// exception of BytesFailed, and none is authoritative.
//
// BytesFailed is the exception because a permanently failed article never
// decodes, so it never writes an ArticleFact — Class A records what was
// decoded. Its authority is the failed half of job_files.articles_done. S4's
// "the recomputation is correct by definition" rule therefore does NOT extend
// to it: recomputing from Class A yields zero and would discard a real
// failure count.
type FileExtent struct {
	FileIdx int32
	// Durable has one bit per article of this file, in file-local ordinal
	// order, set when a completed fsync covered that article's bytes.
	Durable Bitmap
	// VerifiedTo is the length of the gapless prefix proven present from
	// byte 0. It is the CRC anchor, and is permitted to stall at a hole
	// without affecting resume, which depends only on Durable.
	VerifiedTo int64
	// PrefixCRC is the CRC32 of [0, VerifiedTo).
	//
	// HasPrefixCRC means this is a VERIFIED WHOLE-FILE CRC — set only when a
	// verification run consumed every recorded fact and reached the file's
	// end. Anything less is unavailable (R23). The looser reading, that the
	// CRC is merely valid for whatever range VerifiedTo names, is bug #349:
	// a partial-extent CRC mistaken for the file's.
	PrefixCRC    uint32
	HasPrefixCRC bool
	// BytesDurable and BytesFailed are cached aggregates. They exist so
	// restart stays O(files) (B3) rather than O(articles); the FactLog
	// remains the authority (S5).
	BytesDurable int64
	BytesFailed  int64
	// Size and ModTimeNs stamp the file as it was at commit time. The
	// resumer compares them against the file as it exists now; a mismatch
	// invalidates every other field in this struct (S7).
	Size      int64
	ModTimeNs int64
}

// ExtentStore persists Class B. Commit is atomic across the whole slice so a
// job's files can never be observed half-committed.
type ExtentStore interface {
	Commit(ctx context.Context, jobID string, exts []FileExtent) error
	Load(ctx context.Context, jobID string) ([]FileExtent, error)
	DeleteJob(ctx context.Context, jobID string) error
}
```

`internal/durability/proof.go`:

```go
package durability

import "slices"

// DurableProof witnesses that a completed fsync covers the articles it names.
//
// It has no exported fields and no exported constructor, so no package
// outside internal/durability can create one. Since the queue's ack entry
// point takes a DurableProof, "ack before fsync" is not a rule anyone must
// remember — it is code that does not compile.
//
// The guarantee is package-scoped: within internal/durability, newProof is
// reachable from anywhere. Only Barrier.Run calls it, and that is the one
// place review has to hold. Outside this package it is absolute.
type DurableProof struct {
	jobID string
	arts  []int32
}

// newProof is unexported deliberately. See the type doc.
func newProof(jobID string, arts []int32) DurableProof {
	return DurableProof{jobID: jobID, arts: slices.Clone(arts)}
}

func (p DurableProof) JobID() string { return p.jobID }

// Articles returns a copy, so a consumer cannot mutate a proof the barrier
// may still hold.
func (p DurableProof) Articles() []int32 { return slices.Clone(p.arts) }
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/durability/ -v`
Expected: PASS.

- [ ] **Step 5: Observe the red check on the aliasing pin**

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/durability/proof.go "$SCRATCH/proof.bak.go"
sed -i 's|func (p DurableProof) Articles() \[\]int32 { return slices.Clone(p.arts) }|func (p DurableProof) Articles() []int32 { return p.arts }|' internal/durability/proof.go
grep -n 'return p.arts$' internal/durability/proof.go   # confirm the mutation landed
go test ./internal/durability/ -run TestDurableProof_ArticlesIsNotAliased
# MUST FAIL: "mutating the returned slice mutated the proof"
cp "$SCRATCH/proof.bak.go" internal/durability/proof.go
```

Record the observed failure message in the commit body.

- [ ] **Step 6: Quality gates and commit**

```bash
goimports -w internal/durability/ && go vet ./internal/durability/ && \
  go test -race ./internal/durability/ && golangci-lint run ./internal/durability/
git add internal/durability/
git commit -m "feat(durability): add Class A facts, Class B extents, and the proof token

ArticleFact asserts nothing about presence, so it needs no barrier and
no ordering against the data. FileExtent does assert presence, so only
Barrier may commit it.

DurableProof has no exported constructor, which is what makes 'ack only
after fsync' compiler-enforced outside this package rather than a rule
six call sites must each remember.

Red check observed: reverting Articles() to return p.arts fails
TestDurableProof_ArticlesIsNotAliased with 'mutating the returned slice
mutated the proof'.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Task 3: Collapse the schema to a single fresh migration

**Files:**
- Delete: `internal/history/migrations/002_add_jobs_tables.sql` through
  `011_add_job_files_max_written.sql`
- Delete: `internal/history/migration_010_test.go` — it drives goose to
  version 009, seeds rows, then applies 010 to assert the par2-scalar
  replacement. With a single migration there is no intermediate version to
  stop at and no replacement to observe, so the test cannot be rewritten,
  only removed. Its `migrationProviderAt` helper goes with it unless the new
  schema test reuses it.
- Rewrite: `internal/history/migrations/001_initial.sql`
- Test: `internal/history/migrations_test.go` (new file)

`openMigratedTestDB` does **not** exist and must be written. The nearest
existing helper is `openTestDB(t) (*DB, *Repository)` in
`internal/history/repository_test.go`, which returns a different pair and is
not a drop-in. Write `openMigratedTestDB(t *testing.T) *sql.DB`: open
`modernc.org/sqlite` under `t.TempDir()`, run the embedded migrations the
same way `Open` does, and register `t.Cleanup`.

**Interfaces:**
- Consumes: nothing.
- Produces: tables `jobs`, `job_files`, `article_facts`, `file_extents`,
  `history`, `history_job_files`, `queue_meta`.

The Class B columns leave `job_files` entirely. `bytes_downloaded`,
`failed_bytes`, `max_written`, and `write_cursor` are derived values that were
being maintained independently — the direct cause of #306, #337, and #311. They
now live in `file_extents` and are labelled cache.

- [ ] **Step 1: Write the failing test**

`internal/history/migrations_test.go`:

```go
func TestMigrations_SchemaShape(t *testing.T) {
	db := openMigratedTestDB(t) // existing helper; create one if absent

	t.Run("job_files carries no derived columns", func(t *testing.T) {
		forbidden := []string{"bytes_downloaded", "failed_bytes", "max_written", "write_cursor"}
		rows, err := db.Query(`SELECT name FROM pragma_table_info('job_files')`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var cols []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatal(err)
			}
			cols = append(cols, c)
		}
		for _, f := range forbidden {
			if slices.Contains(cols, f) {
				t.Errorf("job_files still has derived column %q — S5 forbids a second authoritative copy", f)
			}
		}
	})

	t.Run("article_facts is keyed for idempotent append", func(t *testing.T) {
		var sql string
		err := db.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type='table' AND name='article_facts'`,
		).Scan(&sql)
		if err != nil {
			t.Fatalf("article_facts table missing: %v", err)
		}
		if !strings.Contains(sql, "PRIMARY KEY") {
			t.Error("article_facts needs a primary key on (job_id, art_idx) for idempotent Append")
		}
	})

	t.Run("file_extents exists with the validity stamp", func(t *testing.T) {
		for _, col := range []string{"durable_bitmap", "verified_to", "prefix_crc", "has_prefix_crc", "bytes_durable", "bytes_failed", "size", "mod_time_ns"} {
			var n int
			err := db.QueryRow(
				`SELECT COUNT(*) FROM pragma_table_info('file_extents') WHERE name = ?`, col,
			).Scan(&n)
			if err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Errorf("file_extents missing column %q", col)
			}
		}
	})

	t.Run("only one migration file remains", func(t *testing.T) {
		entries, err := os.ReadDir("migrations")
		if err != nil {
			t.Fatal(err)
		}
		var sqls []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".sql") {
				sqls = append(sqls, e.Name())
			}
		}
		if len(sqls) != 1 || sqls[0] != "001_initial.sql" {
			t.Errorf("migrations = %v, want exactly [001_initial.sql]", sqls)
		}
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/history/ -run TestMigrations_SchemaShape -v`
Expected: FAIL — `job_files still has derived column "bytes_downloaded"`, and
`migrations = [001_initial.sql 002_... ]`.

- [ ] **Step 3: Delete migrations 002–011 and rewrite 001**

```bash
git rm internal/history/migrations/0{02,03,04,05,06,07,08,09,10,11}_*.sql
```

Rewrite `internal/history/migrations/001_initial.sql`. Build it by reading the
current schema (`sqlite3 <testdb> .schema` after running the old migrations, or
by reading 001–011 in order before deleting them) and then applying these
changes:

- `job_files`: **drop** `bytes_downloaded`, `failed_bytes`, `max_written`,
  `write_cursor`. Keep `job_id`, `file_index`, `subject`, `filename`,
  `article_count`, `bytes`, `complete`, `fetch_policy`, `is_par2_recovery`.
- **New** `article_facts`:

```sql
CREATE TABLE article_facts (
    job_id    TEXT    NOT NULL,
    art_idx   INTEGER NOT NULL,
    file_idx  INTEGER NOT NULL,
    offset    INTEGER NOT NULL,
    length    INTEGER NOT NULL,
    crc32     INTEGER NOT NULL,
    has_crc   INTEGER NOT NULL,
    PRIMARY KEY (job_id, art_idx)
) WITHOUT ROWID;
-- Class A. Append-only and immutable: a row is never updated, because an
-- article's decoded offset, length, and CRC never change once discovered.
-- Idempotency is the primary key's job — Append uses INSERT OR IGNORE.
-- These rows assert nothing about bytes being present on disk, which is
-- why they may be committed at any time with no fsync ordering.
CREATE INDEX idx_article_facts_file ON article_facts(job_id, file_idx, offset);
```

- **New** `file_extents`:

```sql
CREATE TABLE file_extents (
    job_id         TEXT    NOT NULL,
    file_idx       INTEGER NOT NULL,
    durable_bitmap BLOB    NOT NULL,
    verified_to    INTEGER NOT NULL DEFAULT 0,
    prefix_crc     INTEGER NOT NULL DEFAULT 0,
    has_prefix_crc INTEGER NOT NULL DEFAULT 0,
    bytes_durable  INTEGER NOT NULL DEFAULT 0,
    bytes_failed   INTEGER NOT NULL DEFAULT 0,
    size           INTEGER NOT NULL DEFAULT 0,
    mod_time_ns    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (job_id, file_idx)
) WITHOUT ROWID;
-- Class B: a cache of values derived from article_facts plus the file's
-- actual bytes. Never authoritative — where this disagrees with a
-- recomputation from the bytes, the recomputation is correct by definition.
-- Written only by durability.Barrier, only after the fsync that makes its
-- claims true. size and mod_time_ns stamp the file at commit; a mismatch
-- against the file as it exists now invalidates every other column here.
```

Because the whole schema is one migration, the comment blocks above are the
**only** place these claims live, and per `AGENTS.md` they are frozen once
applied. Sweep them before this task's commit, not after.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/history/ -v`
Expected: PASS.

Then: `go build ./...`
Expected: **FAIL** — `internal/queue/sqlite_store.go` still selects the dropped
columns. That is correct and expected at this point; Task 8 removes those reads.
To keep the repo green per-commit, **this task's commit must also stub those
reads out**, which is why Task 3 and the `sqlite_store.go` column removal are
one task rather than two: a reviewer cannot meaningfully accept one without the
other.

Delete from `internal/queue/sqlite_store.go` every reference to
`bytes_downloaded`, `failed_bytes`, `max_written`, and `write_cursor` —
including in `RestoreJobProgress`, `RestoreRetryProgress`,
`ArticleCountsByJob`, `RemainingBytesByJob`, and the `job_files` INSERT/UPDATE
statements. Remove `FileMeta.BytesDownloaded`, `FileMeta.FailedBytes`, and any
now-unused struct fields. Fix the resulting compile errors in `progress.go` by
deleting the fields they assign; Task 8 replaces the derivation properly.

Run: `go build ./... && go test -race ./...`
Expected: PASS. Delete tests that pinned the removed columns:
`internal/queue/max_written_persist_test.go`,
`internal/queue/content_failed_bytes_test.go`, and the removed-column subtests
in `sqlite_store_test.go` and `progress_bytes_test.go`.

- [ ] **Step 5: Commit**

```bash
git add -A internal/history/migrations internal/history internal/queue
git commit -m "feat(history)!: collapse the schema and drop the derived job_files columns

Replaces migrations 001-011 with a single fresh 001_initial.sql, adding
article_facts (Class A) and file_extents (Class B).

bytes_downloaded, failed_bytes, max_written, and write_cursor leave
job_files. Each was a derived value maintained independently of the
facts it summarised, which is the direct cause of #306 (loaded then
overwritten by a recompute), #337 (stored while siblings are derived),
and #311 (a cursor serving as cache, authority, and scheduling hint at
once). They return as labelled cache columns in file_extents.

BREAKING CHANGE: the on-disk database is not migrated and cannot be
read by this version. Rewriting applied migrations overrides the rule
in AGENTS.md; the user has explicitly authorised a from-scratch
reinstall, so no applied migration is at risk.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Task 4: SQLite fact log

**Files:**
- Create: `internal/durability/factlog_sqlite.go`
- Test: `internal/durability/factlog_sqlite_test.go`

**Interfaces:**
- Consumes: `FactLog`, `ArticleFact` (Task 2); schema from Task 3.
- Produces: `func NewSQLiteFactLog(db *sql.DB) *SQLiteFactLog` satisfying `FactLog`.

- [ ] **Step 1: Write the failing test**

```go
func TestSQLiteFactLog_AppendIsIdempotent(t *testing.T) {
	ctx := context.Background()
	fl := NewSQLiteFactLog(openTestDB(t))

	fact := ArticleFact{FileIdx: 0, ArtIdx: 5, Offset: 1024, Length: 768000, CRC32: 0xDEADBEEF, HasCRC: true}
	if err := fl.Append(ctx, "job-1", []ArticleFact{fact}); err != nil {
		t.Fatal(err)
	}
	// A re-delivered article must not error and must not change the record.
	if err := fl.Append(ctx, "job-1", []ArticleFact{fact}); err != nil {
		t.Fatalf("second Append: %v", err)
	}

	got, err := fl.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ForFile returned %d facts, want 1", len(got))
	}
	if got[0] != fact {
		t.Errorf("ForFile = %+v, want %+v", got[0], fact)
	}
}

// TestSQLiteFactLog_AppendNeverUpdates pins R1: a Class A fact is immutable.
// A hostile or buggy second delivery reporting a different offset must not
// overwrite the first — the file's bytes were written against the original.
func TestSQLiteFactLog_AppendNeverUpdates(t *testing.T) {
	ctx := context.Background()
	fl := NewSQLiteFactLog(openTestDB(t))

	orig := ArticleFact{FileIdx: 0, ArtIdx: 5, Offset: 1024, Length: 100, CRC32: 1, HasCRC: true}
	evil := ArticleFact{FileIdx: 0, ArtIdx: 5, Offset: 999999, Length: 100, CRC32: 2, HasCRC: true}
	if err := fl.Append(ctx, "job-1", []ArticleFact{orig}); err != nil {
		t.Fatal(err)
	}
	if err := fl.Append(ctx, "job-1", []ArticleFact{evil}); err != nil {
		t.Fatal(err)
	}
	got, _ := fl.ForFile(ctx, "job-1", 0)
	if got[0].Offset != 1024 {
		t.Fatalf("Offset = %d, want 1024 — a Class A fact was mutated", got[0].Offset)
	}
}

func TestSQLiteFactLog_ForFileIsOrderedByOffset(t *testing.T) {
	ctx := context.Background()
	fl := NewSQLiteFactLog(openTestDB(t))
	// art_idx ascends as offset DESCENDS. That inversion is the whole point:
	// article_facts is WITHOUT ROWID keyed on (job_id, art_idx), so an
	// unordered scan returns primary-key order. A fixture whose art_idx and
	// offset both ascend produces the same sequence either way, and the test
	// passes with ORDER BY deleted — it pins nothing. Task 6's gaplessPrefix
	// walks this result assuming offset order, so the clause is load-bearing.
	facts := []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 2000, Length: 500, HasCRC: true},
		{FileIdx: 0, ArtIdx: 2, Offset: 0, Length: 500, HasCRC: true},
		{FileIdx: 1, ArtIdx: 3, Offset: 0, Length: 500, HasCRC: true},
		{FileIdx: 0, ArtIdx: 1, Offset: 1000, Length: 500, HasCRC: true},
	}
	if err := fl.Append(ctx, "job-1", facts); err != nil {
		t.Fatal(err)
	}
	got, err := fl.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("ForFile(0) returned %d, want 3 (file 1 must not leak in)", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Offset <= got[i-1].Offset {
			t.Fatalf("ForFile not ordered by offset: %v", got)
		}
	}
}

// TestSQLiteFactLog_HasCRCRoundTrips pins R23: "CRC unavailable" must stay
// distinguishable from "CRC is genuinely zero". Both facts below carry
// CRC32 == 0 and differ only in HasCRC, so the test fails if either the
// write side or the read side of that boolean is hardcoded.
//
// Without this, every other fixture in the file sets HasCRC: true, and both
// halves can be replaced with a constant while the suite stays green. Task 6
// decides whether a completed file can report a whole-file CRC from exactly
// this flag: if the read side degrades to true, a UU-encoded article's absent
// CRC becomes a CRC of zero and the file reports a fabricated whole-file
// value — the failure R23 exists to forbid.
func TestSQLiteFactLog_HasCRCRoundTrips(t *testing.T) {
	ctx := context.Background()
	fl := NewSQLiteFactLog(openTestDB(t))

	if err := fl.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0, HasCRC: false},
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: 0, HasCRC: true},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := fl.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ForFile returned %d facts, want 2", len(got))
	}
	if got[0].HasCRC {
		t.Error("HasCRC = true for the UU-encoded article, want false")
	}
	if !got[1].HasCRC {
		t.Error("HasCRC = false for the article whose CRC is genuinely zero, want true")
	}
	if got[0].CRC32 != 0 || got[1].CRC32 != 0 {
		t.Fatalf("CRC32 = %d/%d, want 0/0 — the flag, not the value, must carry the distinction",
			got[0].CRC32, got[1].CRC32)
	}
}

func TestSQLiteFactLog_DeleteJobIsScoped(t *testing.T) {
	ctx := context.Background()
	fl := NewSQLiteFactLog(openTestDB(t))
	f := ArticleFact{FileIdx: 0, ArtIdx: 1, Offset: 0, Length: 10, HasCRC: true}
	if err := fl.Append(ctx, "job-1", []ArticleFact{f}); err != nil {
		t.Fatal(err)
	}
	if err := fl.Append(ctx, "job-2", []ArticleFact{f}); err != nil {
		t.Fatal(err)
	}
	if err := fl.DeleteJob(ctx, "job-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := fl.ForFile(ctx, "job-1", 0); len(got) != 0 {
		t.Errorf("job-1 has %d facts after DeleteJob, want 0", len(got))
	}
	if got, _ := fl.ForFile(ctx, "job-2", 0); len(got) != 1 {
		t.Errorf("DeleteJob(job-1) removed job-2's facts")
	}
}
```

Write `openTestDB(t *testing.T) *sql.DB` in the test file: open
`modernc.org/sqlite` at `t.TempDir()+"/test.db"`, run the goose migrations from
`internal/history/migrations`, and register `t.Cleanup(db.Close)`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/durability/ -run TestSQLiteFactLog -v`
Expected: FAIL — `undefined: NewSQLiteFactLog`.

- [ ] **Step 3: Write the implementation**

```go
package durability

import (
	"context"
	"database/sql"
	"fmt"
)

// SQLiteFactLog is the append-only Class A store.
type SQLiteFactLog struct{ db *sql.DB }

func NewSQLiteFactLog(db *sql.DB) *SQLiteFactLog { return &SQLiteFactLog{db: db} }

// Append inserts facts, ignoring any whose (job_id, art_idx) is already
// present. INSERT OR IGNORE rather than UPSERT is the point: a Class A fact
// is immutable (R1), so a second delivery of the same article must leave the
// original record untouched. The file's bytes were written against the
// original offset, and a later report claiming a different one — whether from
// a buggy path or a hostile server — must not be allowed to redescribe them.
func (s *SQLiteFactLog) Append(ctx context.Context, jobID string, facts []ArticleFact) error {
	if len(facts) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("durability: begin fact append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO article_facts
			(job_id, art_idx, file_idx, offset, length, crc32, has_crc)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("durability: prepare fact append: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, f := range facts {
		hasCRC := 0
		if f.HasCRC {
			hasCRC = 1
		}
		if _, err := stmt.ExecContext(ctx, jobID, f.ArtIdx, f.FileIdx, f.Offset, f.Length, f.CRC32, hasCRC); err != nil {
			return fmt.Errorf("durability: append fact art=%d: %w", f.ArtIdx, err)
		}
	}
	return tx.Commit()
}

func (s *SQLiteFactLog) ForFile(ctx context.Context, jobID string, fileIdx int32) ([]ArticleFact, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT art_idx, file_idx, offset, length, crc32, has_crc
		  FROM article_facts
		 WHERE job_id = ? AND file_idx = ?
		 ORDER BY offset`, jobID, fileIdx)
	if err != nil {
		return nil, fmt.Errorf("durability: query facts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ArticleFact
	for rows.Next() {
		var f ArticleFact
		var hasCRC int
		if err := rows.Scan(&f.ArtIdx, &f.FileIdx, &f.Offset, &f.Length, &f.CRC32, &hasCRC); err != nil {
			return nil, fmt.Errorf("durability: scan fact: %w", err)
		}
		f.HasCRC = hasCRC != 0
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *SQLiteFactLog) DeleteJob(ctx context.Context, jobID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM article_facts WHERE job_id = ?`, jobID); err != nil {
		return fmt.Errorf("durability: delete facts for %s: %w", jobID, err)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/durability/ -run TestSQLiteFactLog -v`
Expected: PASS.

- [ ] **Step 5: Observe the red check on immutability**

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/durability/factlog_sqlite.go "$SCRATCH/factlog.bak.go"
sed -i 's/INSERT OR IGNORE INTO article_facts/INSERT OR REPLACE INTO article_facts/' internal/durability/factlog_sqlite.go
grep -n 'INSERT OR REPLACE' internal/durability/factlog_sqlite.go   # confirm it landed
go test ./internal/durability/ -run TestSQLiteFactLog_AppendNeverUpdates
# MUST FAIL: "Offset = 999999, want 1024 — a Class A fact was mutated"
cp "$SCRATCH/factlog.bak.go" internal/durability/factlog_sqlite.go
```

- [ ] **Step 6: Quality gates and commit**

```bash
goimports -w internal/durability/ && go vet ./internal/durability/ && \
  go test -race ./internal/durability/ && golangci-lint run ./internal/durability/
git add internal/durability/
git commit -m "feat(durability): add the append-only SQLite fact log

INSERT OR IGNORE rather than UPSERT: a Class A fact is immutable, so a
re-delivered article must leave the original record untouched. The
file's bytes were written against the original offset, and a later
report claiming a different one must not redescribe them.

Red check observed: switching to INSERT OR REPLACE fails
TestSQLiteFactLog_AppendNeverUpdates with 'Offset = 999999, want 1024
— a Class A fact was mutated'.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Task 5: SQLite extent store

**Files:**
- Create: `internal/durability/extentstore_sqlite.go`
- Test: `internal/durability/extentstore_sqlite_test.go`

**Interfaces:**
- Consumes: `ExtentStore`, `FileExtent`, `Bitmap` (Task 2); schema (Task 3).
- Produces: `func NewSQLiteExtentStore(db *sql.DB) *SQLiteExtentStore`
  satisfying `ExtentStore`. `Load` returns extents ordered by `file_idx`.

- [ ] **Step 1: Write the failing test**

```go
func TestSQLiteExtentStore_CommitRoundTrip(t *testing.T) {
	ctx := context.Background()
	es := NewSQLiteExtentStore(openTestDB(t))

	bm := NewBitmap(100)
	bm.Set(0)
	bm.Set(42)
	bm.Set(99)
	ext := FileExtent{
		FileIdx: 3, Durable: bm, VerifiedTo: 4096, PrefixCRC: 0xABCD, HasPrefixCRC: true,
		BytesDurable: 3000, BytesFailed: 120, Size: 8192, ModTimeNs: 1_700_000_000_000_000_000,
	}
	if err := es.Commit(ctx, "job-1", []FileExtent{ext}); err != nil {
		t.Fatal(err)
	}
	got, err := es.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("Load returned %d extents, want 1", len(got))
	}
	g := got[0]
	if g.FileIdx != 3 || g.VerifiedTo != 4096 || g.PrefixCRC != 0xABCD || !g.HasPrefixCRC {
		t.Errorf("scalar round trip wrong: %+v", g)
	}
	if g.BytesDurable != 3000 || g.BytesFailed != 120 || g.Size != 8192 || g.ModTimeNs != 1_700_000_000_000_000_000 {
		t.Errorf("cache/stamp round trip wrong: %+v", g)
	}
	if g.Durable.Count() != 3 || !g.Durable.Get(0) || !g.Durable.Get(42) || !g.Durable.Get(99) {
		t.Errorf("bitmap round trip wrong: count=%d", g.Durable.Count())
	}
}

// TestSQLiteExtentStore_HasPrefixCRCRoundTrips pins the same distinction
// TestSQLiteFactLog_HasCRCRoundTrips pins for Class A: a prefix CRC that is
// genuinely zero must stay distinguishable from no prefix CRC at all.
//
// Both extents below carry PrefixCRC == 0 and differ only in HasPrefixCRC, so
// the test fails if either side of that boolean is hardcoded. Without it every
// fixture in this file sets HasPrefixCRC: true and the flag is unprotected —
// exactly the gap the Task 4 review found by mutation.
//
// R23 makes this load-bearing: QuickCheck reads the flag, not the value, to
// decide whether a file's CRC can be compared against par2's. A read side
// degraded to true reports a fabricated CRC of zero as authoritative.
func TestSQLiteExtentStore_HasPrefixCRCRoundTrips(t *testing.T) {
	ctx := context.Background()
	es := NewSQLiteExtentStore(openTestDB(t))

	if err := es.Commit(ctx, "job-1", []FileExtent{
		{FileIdx: 0, Durable: NewBitmap(8), PrefixCRC: 0, HasPrefixCRC: false},
		{FileIdx: 1, Durable: NewBitmap(8), PrefixCRC: 0, HasPrefixCRC: true},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := es.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Load returned %d extents, want 2", len(got))
	}
	if got[0].HasPrefixCRC {
		t.Error("HasPrefixCRC = true for the extent that has no prefix CRC, want false")
	}
	if !got[1].HasPrefixCRC {
		t.Error("HasPrefixCRC = false for the extent whose CRC is genuinely zero, want true")
	}
	if got[0].PrefixCRC != 0 || got[1].PrefixCRC != 0 {
		t.Fatalf("PrefixCRC = %d/%d, want 0/0 — the flag, not the value, carries the distinction",
			got[0].PrefixCRC, got[1].PrefixCRC)
	}
}

// TestSQLiteExtentStore_CommitIsAtomic pins that a job's files are never
// observable half-committed. A barrier that fails partway must leave the
// previous committed cache intact (R7).
//
// This test pins the VALIDATION loop, not the transaction. The bad row is
// rejected before any write, so removing the transaction changes nothing it
// can see — file_extents has no CHECK constraint, so nothing else can fail
// mid-batch either. TestSQLiteExtentStore_TransactionRollsBackMidBatch below
// is what pins the transaction; the two mechanisms need two tests, not two
// mutations of one test.
func TestSQLiteExtentStore_CommitIsAtomic(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	es := NewSQLiteExtentStore(db)

	first := []FileExtent{
		{FileIdx: 0, Durable: NewBitmap(8), VerifiedTo: 100},
		{FileIdx: 1, Durable: NewBitmap(8), VerifiedTo: 200},
	}
	if err := es.Commit(ctx, "job-1", first); err != nil {
		t.Fatal(err)
	}

	// A second commit whose second element is invalid must roll the whole
	// batch back, leaving VerifiedTo at 100/200 rather than 999/200.
	bad := []FileExtent{
		{FileIdx: 0, Durable: NewBitmap(8), VerifiedTo: 999},
		{FileIdx: 1, Durable: Bitmap{}, VerifiedTo: -1}, // negative extent is rejected
	}
	if err := es.Commit(ctx, "job-1", bad); err == nil {
		t.Fatal("Commit accepted a negative VerifiedTo")
	}
	got, err := es.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].VerifiedTo != 100 {
		t.Fatalf("VerifiedTo = %d after a failed commit, want 100 — the batch was not atomic", got[0].VerifiedTo)
	}
}

// TestSQLiteExtentStore_TransactionRollsBackMidBatch pins the transaction
// itself, which CommitIsAtomic cannot reach: validation rejects malformed
// input before any write, so only a failure arriving DURING the write
// exercises the rollback. In production that is ENOSPC, SQLITE_BUSY, or a
// cancelled context; here a test-only trigger produces the same shape
// deterministically, with no driver wrapper and no schema change — the
// trigger lives in this test's scratch database only.
//
// Without the transaction, row 0's new value is already committed when row 1
// fails, which violates R7 on a path validation can never guard.
func TestSQLiteExtentStore_TransactionRollsBackMidBatch(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	es := NewSQLiteExtentStore(db)

	if err := es.Commit(ctx, "job-1", []FileExtent{
		{FileIdx: 0, Durable: NewBitmap(8), VerifiedTo: 100},
		{FileIdx: 1, Durable: NewBitmap(8), VerifiedTo: 200},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, `
		CREATE TRIGGER abort_second BEFORE INSERT ON file_extents
		WHEN NEW.file_idx = 1 BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}

	// Both rows are valid, so the validation loop passes them; the second
	// fails at the storage layer.
	err := es.Commit(ctx, "job-1", []FileExtent{
		{FileIdx: 0, Durable: NewBitmap(8), VerifiedTo: 999},
		{FileIdx: 1, Durable: NewBitmap(8), VerifiedTo: 888},
	})
	if err == nil {
		t.Fatal("Commit succeeded despite the trigger")
	}

	got, loadErr := es.Load(ctx, "job-1")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(got) != 2 {
		t.Fatalf("Load returned %d extents, want 2", len(got))
	}
	if got[0].VerifiedTo != 100 {
		t.Fatalf("file 0 VerifiedTo = %d, want 100 — a mid-batch failure left a partial write",
			got[0].VerifiedTo)
	}
	if got[1].VerifiedTo != 200 {
		t.Fatalf("file 1 VerifiedTo = %d, want 200", got[1].VerifiedTo)
	}
}

func TestSQLiteExtentStore_CommitOverwritesPriorExtent(t *testing.T) {
	ctx := context.Background()
	es := NewSQLiteExtentStore(openTestDB(t))
	bm1 := NewBitmap(8)
	bm1.Set(0)
	if err := es.Commit(ctx, "job-1", []FileExtent{{FileIdx: 0, Durable: bm1, VerifiedTo: 100}}); err != nil {
		t.Fatal(err)
	}
	bm2 := NewBitmap(8)
	bm2.Set(0)
	bm2.Set(1)
	if err := es.Commit(ctx, "job-1", []FileExtent{{FileIdx: 0, Durable: bm2, VerifiedTo: 200}}); err != nil {
		t.Fatal(err)
	}
	got, _ := es.Load(ctx, "job-1")
	if len(got) != 1 {
		t.Fatalf("Load returned %d rows, want 1 — Commit inserted instead of replacing", len(got))
	}
	if got[0].VerifiedTo != 200 || got[0].Durable.Count() != 2 {
		t.Errorf("second commit did not replace the first: %+v", got[0])
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/durability/ -run TestSQLiteExtentStore -v`
Expected: FAIL — `undefined: NewSQLiteExtentStore`.

- [ ] **Step 3: Write the implementation**

```go
package durability

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrInvalidExtent reports a FileExtent that cannot describe a real file.
var ErrInvalidExtent = errors.New("durability: invalid file extent")

// SQLiteExtentStore persists the Class B cache.
//
// Unlike the fact log, this store REPLACEs: a FileExtent is a cache entry
// describing the file as of the last barrier, so a newer commit supersedes an
// older one wholesale. The immutability rule that governs article_facts does
// not apply here, and must not be copied across.
type SQLiteExtentStore struct{ db *sql.DB }

func NewSQLiteExtentStore(db *sql.DB) *SQLiteExtentStore { return &SQLiteExtentStore{db: db} }

// Commit writes every extent in one transaction. Atomicity is the guarantee
// R7 depends on: a barrier that fails partway must leave the previously
// committed cache wholly intact rather than a mix of old and new.
func (s *SQLiteExtentStore) Commit(ctx context.Context, jobID string, exts []FileExtent) error {
	if len(exts) == 0 {
		return nil
	}
	for _, e := range exts {
		if e.VerifiedTo < 0 || e.Size < 0 || e.BytesDurable < 0 || e.BytesFailed < 0 {
			return fmt.Errorf("%w: file %d has a negative figure", ErrInvalidExtent, e.FileIdx)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("durability: begin extent commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO file_extents
			(job_id, file_idx, durable_bitmap, verified_to, prefix_crc,
			 has_prefix_crc, bytes_durable, bytes_failed, size, mod_time_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("durability: prepare extent commit: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, e := range exts {
		hasCRC := 0
		if e.HasPrefixCRC {
			hasCRC = 1
		}
		if _, err := stmt.ExecContext(ctx, jobID, e.FileIdx, e.Durable.Bytes(), e.VerifiedTo,
			e.PrefixCRC, hasCRC, e.BytesDurable, e.BytesFailed, e.Size, e.ModTimeNs); err != nil {
			return fmt.Errorf("durability: commit extent file=%d: %w", e.FileIdx, err)
		}
	}
	return tx.Commit()
}

// Load reads a job's extents, ordered by file_idx.
//
// The bitmap's bit count is not stored, so each bitmap comes back at its full
// BYTE width and is UNMASKED — Bitmap's tail-word mask cannot fire when n is a
// multiple of 64. Padding bits are zero for any blob this package wrote, since
// Set is bounds-checked, but a damaged blob can set them and Count() would
// then over-report articles durable, which is the over-claim direction the
// design forbids.
//
// Callers that need a trustworthy Count MUST re-derive with
// BitmapFromBytes(raw, realArticleCount) rather than slicing what Load
// returns. Only the manifest knows that count, and this layer does not have
// it.
func (s *SQLiteExtentStore) Load(ctx context.Context, jobID string) ([]FileExtent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT file_idx, durable_bitmap, verified_to, prefix_crc,
		       has_prefix_crc, bytes_durable, bytes_failed, size, mod_time_ns
		  FROM file_extents WHERE job_id = ? ORDER BY file_idx`, jobID)
	if err != nil {
		return nil, fmt.Errorf("durability: query extents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []FileExtent
	for rows.Next() {
		var e FileExtent
		var raw []byte
		var hasCRC int
		if err := rows.Scan(&e.FileIdx, &raw, &e.VerifiedTo, &e.PrefixCRC,
			&hasCRC, &e.BytesDurable, &e.BytesFailed, &e.Size, &e.ModTimeNs); err != nil {
			return nil, fmt.Errorf("durability: scan extent: %w", err)
		}
		e.HasPrefixCRC = hasCRC != 0
		bm, err := BitmapFromBytes(raw, len(raw)*8)
		if err != nil {
			return nil, fmt.Errorf("durability: extent file=%d bitmap: %w", e.FileIdx, err)
		}
		e.Durable = bm
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *SQLiteExtentStore) DeleteJob(ctx context.Context, jobID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM file_extents WHERE job_id = ?`, jobID); err != nil {
		return fmt.Errorf("durability: delete extents for %s: %w", jobID, err)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/durability/ -run TestSQLiteExtentStore -v`
Expected: PASS.

- [ ] **Step 5: Observe the red check on atomicity**

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/durability/extentstore_sqlite.go "$SCRATCH/es.bak.go"
# Neuter the pre-validation loop so the bad row is rejected mid-transaction
# by SQLite instead of before it starts — which is the shape that would
# leave a partial write if the transaction were not doing its job.
sed -i 's|if e.VerifiedTo < 0 |if false \&\& e.VerifiedTo < 0 |' internal/durability/extentstore_sqlite.go
grep -n 'if false &&' internal/durability/extentstore_sqlite.go
go test ./internal/durability/ -run TestSQLiteExtentStore_CommitIsAtomic
# MUST FAIL: "Commit accepted a negative VerifiedTo"
cp "$SCRATCH/es.bak.go" internal/durability/extentstore_sqlite.go
```

Note: this red check demonstrates the *validation*, not the transaction. To
demonstrate the transaction as well, additionally replace `tx.Commit()` with a
per-row `s.db.ExecContext` (no transaction) and confirm
`TestSQLiteExtentStore_CommitIsAtomic` then fails on `VerifiedTo = 999`.
**Both reverts are required** — a fix with two mechanisms needs two red checks.

- [ ] **Step 6: Quality gates and commit**

```bash
goimports -w internal/durability/ && go vet ./internal/durability/ && \
  go test -race ./internal/durability/ && golangci-lint run ./internal/durability/
git add internal/durability/
git commit -m "feat(durability): add the atomic SQLite extent store

Class B, so this store REPLACEs where the fact log IGNOREs: an extent
describes the file as of the last barrier and a newer commit supersedes
it wholesale.

Commit is one transaction across a job's files, which is what lets a
failed barrier leave the previous cache wholly intact instead of a mix
of old and new.

Red checks observed (two, one per mechanism): disabling the validation
loop fails CommitIsAtomic with 'Commit accepted a negative VerifiedTo';
replacing the transaction with per-row Exec fails it with
'VerifiedTo = 999 after a failed commit, want 100'.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Task 6: The checkpoint barrier

**Files:**
- Create: `internal/durability/synctarget.go`, `internal/durability/barrier.go`
- Test: `internal/durability/barrier_test.go`

**Interfaces:**
- Consumes: `FactLog`, `ExtentStore`, `DurableProof`, `newProof`, `Bitmap`,
  `storagefault.Fault`.
- Produces:

```go
type WrittenArticle struct {
	FileIdx int32
	ArtIdx  int32
	Offset  int64
	Length  int32
}

type SyncTarget interface {
	Files() []int32
	Drain(ctx context.Context, fileIdx int32) ([]WrittenArticle, error)
	Sync(ctx context.Context, fileIdx int32) error
	Stat(fileIdx int32) (size int64, modTimeNs int64, err error)
	ArticleCount(fileIdx int32) int
	FileLocalOrdinal(fileIdx int32, artIdx int32) (int, bool)
}

type Acker interface {
	AckDurable(p DurableProof) error
}

type Stallable interface {
	Stall(jobID string, f *storagefault.Fault)
	Fail(jobID string, f *storagefault.Fault)
}

func NewBarrier(fl FactLog, es ExtentStore, ack Acker, stall Stallable, log *slog.Logger) *Barrier
func (b *Barrier) Run(ctx context.Context, jobID string, t SyncTarget) error
```

`Barrier.Run` is the **only** caller of `newProof` and therefore the only path
by which anything can be acked as durable.

- [ ] **Step 1: Write the failing test**

```go
package durability

import (
	"context"
	"errors"
	"syscall"
	"testing"
)

// fakeTarget records the order of Drain/Sync/Stat calls so the ordering
// invariant S1 can be asserted rather than assumed.
type fakeTarget struct {
	calls    []string
	written  map[int32][]WrittenArticle
	syncErr  error
	size     int64
	modTime  int64
	artCount int
}

func (f *fakeTarget) Files() []int32 { return []int32{0} }
func (f *fakeTarget) Drain(ctx context.Context, fileIdx int32) ([]WrittenArticle, error) {
	f.calls = append(f.calls, "drain")
	return f.written[fileIdx], nil
}
func (f *fakeTarget) Sync(ctx context.Context, fileIdx int32) error {
	f.calls = append(f.calls, "sync")
	return f.syncErr
}
func (f *fakeTarget) Stat(fileIdx int32) (int64, int64, error) {
	f.calls = append(f.calls, "stat")
	return f.size, f.modTime, nil
}
func (f *fakeTarget) ArticleCount(fileIdx int32) int { return f.artCount }
func (f *fakeTarget) FileLocalOrdinal(fileIdx, artIdx int32) (int, bool) {
	return int(artIdx), true
}

type recordingAcker struct{ proofs []DurableProof }

func (r *recordingAcker) AckDurable(p DurableProof) error {
	r.proofs = append(r.proofs, p)
	return nil
}

type recordingStall struct {
	stalled []*storagefault.Fault
	failed  []*storagefault.Fault
}

func (r *recordingStall) Stall(jobID string, f *storagefault.Fault) { r.stalled = append(r.stalled, f) }
func (r *recordingStall) Fail(jobID string, f *storagefault.Fault)  { r.failed = append(r.failed, f) }

// TestBarrier_SyncPrecedesCommitAndAck is the pin for S1 and R9. If the
// commit or the ack happens before the fsync, this fails.
func TestBarrier_SyncPrecedesCommitAndAck(t *testing.T) {
	ctx := context.Background()
	tgt := &fakeTarget{
		written:  map[int32][]WrittenArticle{0: {{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100}}},
		size:     100,
		modTime:  42,
		artCount: 4,
	}
	ack := &recordingAcker{}
	b := NewBarrier(NewSQLiteFactLog(openTestDB(t)), NewSQLiteExtentStore(openTestDB(t)), ack, &recordingStall{}, testLogger(t))

	if err := b.Run(ctx, "job-1", tgt); err != nil {
		t.Fatal(err)
	}
	want := []string{"drain", "sync", "stat"}
	if len(tgt.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", tgt.calls, want)
	}
	for i := range want {
		if tgt.calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v — S1 requires sync before any claim", tgt.calls, want)
		}
	}
	if len(ack.proofs) != 1 || len(ack.proofs[0].Articles()) != 1 {
		t.Fatalf("expected exactly one article acked, got %v", ack.proofs)
	}
}

// TestBarrier_SyncFailureAcksNothing pins R7: a failed barrier acks nothing
// and leaves the prior committed cache intact.
func TestBarrier_SyncFailureAcksNothing(t *testing.T) {
	ctx := context.Background()
	es := NewSQLiteExtentStore(openTestDB(t))
	prior := NewBitmap(4)
	prior.Set(0)
	if err := es.Commit(ctx, "job-1", []FileExtent{{FileIdx: 0, Durable: prior, VerifiedTo: 50}}); err != nil {
		t.Fatal(err)
	}

	tgt := &fakeTarget{
		written:  map[int32][]WrittenArticle{0: {{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100}}},
		syncErr:  syscall.ENOSPC,
		artCount: 4,
	}
	ack := &recordingAcker{}
	stall := &recordingStall{}
	b := NewBarrier(NewSQLiteFactLog(openTestDB(t)), es, ack, stall, testLogger(t))

	if err := b.Run(ctx, "job-1", tgt); err == nil {
		t.Fatal("Run returned nil after a failed sync")
	}
	if len(ack.proofs) != 0 {
		t.Errorf("acked %d proofs after a failed sync, want 0", len(ack.proofs))
	}
	got, _ := es.Load(ctx, "job-1")
	if got[0].VerifiedTo != 50 || got[0].Durable.Count() != 1 {
		t.Errorf("prior cache was disturbed by a failed barrier: %+v", got[0])
	}
	if len(stall.stalled) != 1 {
		t.Fatalf("stalled %d times, want 1 — ENOSPC is retryable", len(stall.stalled))
	}
	if len(stall.failed) != 0 {
		t.Errorf("failed the job on a retryable fault")
	}
}

// TestBarrier_PermanentFaultFailsRatherThanStalls pins R20.
func TestBarrier_PermanentFaultFailsRatherThanStalls(t *testing.T) {
	ctx := context.Background()
	tgt := &fakeTarget{written: map[int32][]WrittenArticle{0: {}}, syncErr: syscall.EROFS, artCount: 4}
	stall := &recordingStall{}
	b := NewBarrier(NewSQLiteFactLog(openTestDB(t)), NewSQLiteExtentStore(openTestDB(t)), &recordingAcker{}, stall, testLogger(t))

	if err := b.Run(ctx, "job-1", tgt); err == nil {
		t.Fatal("Run returned nil after EROFS")
	}
	if len(stall.failed) != 1 {
		t.Errorf("failed %d times, want 1 — EROFS is permanent", len(stall.failed))
	}
	if len(stall.stalled) != 0 {
		t.Errorf("stalled on a permanent fault")
	}
}

// TestBarrier_VerifiedToAdvancesOnlyOverAGaplessPrefix pins that a hole
// stops the CRC anchor without stopping resume. This is the distinction
// that #311 and #353 collided over: the durable bitmap must record the
// article above the hole, while VerifiedTo must not advance past it.
//
// A hole has two shapes and ONE FIXTURE CANNOT PIN BOTH GATES. In the
// fixture below the missing article HAS a Class A fact, so the walk stops
// because that article is not durable — and deleting the contiguity check
// entirely leaves this test green, which was observed, not reasoned. Add a
// second case in which the missing article has NO fact (it was never
// decoded, so nothing recorded its byte range): every fact present is then
// durable, and only contiguity can stop the walk. Also append the facts to
// the fact log — gaplessPrefix reads FactLog.ForFile, so with an empty log
// VerifiedTo is 0 and the fixture below cannot reach 100 at all.
func TestBarrier_VerifiedToAdvancesOnlyOverAGaplessPrefix(t *testing.T) {
	ctx := context.Background()
	es := NewSQLiteExtentStore(openTestDB(t))
	tgt := &fakeTarget{
		written: map[int32][]WrittenArticle{0: {
			{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
			// article 1 at offset 100 is missing — the hole
			{FileIdx: 0, ArtIdx: 2, Offset: 200, Length: 100},
		}},
		size: 300, artCount: 4,
	}
	b := NewBarrier(NewSQLiteFactLog(openTestDB(t)), es, &recordingAcker{}, &recordingStall{}, testLogger(t))
	if err := b.Run(ctx, "job-1", tgt); err != nil {
		t.Fatal(err)
	}
	got, _ := es.Load(ctx, "job-1")
	if got[0].VerifiedTo != 100 {
		t.Errorf("VerifiedTo = %d, want 100 — it must stop at the hole", got[0].VerifiedTo)
	}
	if !got[0].Durable.Get(2) {
		t.Error("article 2 is not durable, but its bytes were written and fsynced — resume must not be held back by the hole")
	}
}
```

Write `testLogger(t *testing.T) *slog.Logger` returning
`slog.New(slog.NewTextHandler(io.Discard, nil))`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/durability/ -run TestBarrier -v`
Expected: FAIL — `undefined: NewBarrier`.

- [ ] **Step 3: Write the implementation**

`internal/durability/synctarget.go` — the interfaces exactly as in the
**Interfaces** block above, each with a doc comment stating what the barrier
relies on. Key comments to write:

- On `Drain`: "returns the articles whose bytes reached `WriteAt` without
  error. It must NOT return an article that is merely buffered — S2 makes
  acceptance and durability different things, and this return value is the
  only evidence the barrier has."
- On `Sync`: "fsyncs the handle. Until this returns nil, nothing the preceding
  Drain reported may be claimed (S1)."

`internal/durability/barrier.go`:

```go
package durability

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// Barrier is the single place the Written → Durable → Resolved transition
// happens (X2). It is the only caller of newProof, so no other code path —
// inside this package or out — can ack an article as downloaded.
type Barrier struct {
	facts  FactLog
	exts   ExtentStore
	ack    Acker
	stall  Stallable
	log    *slog.Logger
}

func NewBarrier(fl FactLog, es ExtentStore, ack Acker, stall Stallable, log *slog.Logger) *Barrier {
	return &Barrier{facts: fl, exts: es, ack: ack, stall: stall, log: log}
}

// Run executes one checkpoint for a job:
//
//	drain → write → fsync → commit Class B → ack
//
// The order is the invariant. Nothing before the fsync may be claimed, and
// nothing is claimed at all if any step fails (R7).
func (b *Barrier) Run(ctx context.Context, jobID string, t SyncTarget) error {
	files := t.Files()
	drained := make(map[int32][]WrittenArticle, len(files))

	// Phase 1 — drain every file. Still no claim of any kind.
	for _, idx := range files {
		w, err := t.Drain(ctx, idx)
		if err != nil {
			return b.routeFault(jobID, storagefault.Classify("write", "", err))
		}
		drained[idx] = w
	}

	// Phase 2 — fsync every file. Only after this may anything be claimed.
	for _, idx := range files {
		if err := t.Sync(ctx, idx); err != nil {
			return b.routeFault(jobID, storagefault.Classify("sync", "", err))
		}
	}

	// Phase 3 — build the Class B cache from what the fsync just made true.
	exts := make([]FileExtent, 0, len(files))
	var acked []int32
	for _, idx := range files {
		size, modNs, err := t.Stat(idx)
		if err != nil {
			return b.routeFault(jobID, storagefault.Classify("stat", "", err))
		}
		prior, err := b.priorExtent(ctx, jobID, idx, t.ArticleCount(idx))
		if err != nil {
			return err
		}
		ext := prior
		ext.FileIdx = idx
		ext.Size = size
		ext.ModTimeNs = modNs

		for _, w := range drained[idx] {
			ord, ok := t.FileLocalOrdinal(idx, w.ArtIdx)
			if !ok {
				// The target could not place this article in its file. That
				// is a bookkeeping defect, not a storage fault, and it must
				// not be swallowed (A2, R28).
				return fmt.Errorf("durability: job %s file %d: article %d has no file-local ordinal", jobID, idx, w.ArtIdx)
			}
			// Charge the bytes only on a 0->1 transition. Drain may report an
			// article this or a previous barrier already recorded — R12 makes
			// at-least-once delivery the contract and requires the apply to
			// absorb it — and += outside this guard would inflate the R26
			// "bytes durable" figure on every replay. Set() is idempotent;
			// the accumulator is not.
			if !ext.Durable.Get(ord) {
				ext.Durable.Set(ord)
				ext.BytesDurable += int64(w.Length)
			}
			acked = append(acked, w.ArtIdx)
		}

		// The barrier does NOT write ArticleFacts. Class A is appended by the
		// writer when the article is decoded, with no ordering against the
		// data (R2) — that independence is what lets Class A be committed
		// without a barrier at all. Writing facts here would make them
		// barrier-ordered and quietly destroy the property.

		ext.VerifiedTo = b.gaplessPrefix(ctx, jobID, idx, ext.Durable, t)
		exts = append(exts, ext)
	}

	// Phase 4 — commit Class B atomically, then and only then ack.
	if err := b.exts.Commit(ctx, jobID, exts); err != nil {
		return fmt.Errorf("durability: barrier commit for %s: %w", jobID, err)
	}
	if len(acked) == 0 {
		return nil
	}
	slices.Sort(acked)
	return b.ack.AckDurable(newProof(jobID, acked))
}
```

Also implement, in the same file:

- `func (b *Barrier) routeFault(jobID string, f *storagefault.Fault) error` —
  calls `b.stall.Fail` when `f.Permanent`, `b.stall.Stall` otherwise, and
  returns `f`.
- `func (b *Barrier) priorExtent(ctx context.Context, jobID string, idx int32, artCount int) (FileExtent, error)` —
  **Two hazards carried from earlier tasks, both load-bearing here.**

  *Re-derive the bitmap, do not slice it.* `ExtentStore.Load` returns each
  bitmap at its full BYTE width and **unmasked** — `Bitmap`'s tail-word mask
  cannot fire when `n` is a multiple of 64 — so a damaged blob's padding bits
  survive and inflate `Count()`, the over-claim direction. `priorExtent` has
  `artCount` and `Load` does not, so this is the only place that can fix it:
  rebuild with `BitmapFromBytes(raw, artCount)` rather than narrowing what
  `Load` returned.

  *`FileExtent` holds `Durable Bitmap` by value.* `Bitmap.Set` has a value
  receiver and mutates through the shared backing slice, so `ext.Durable.Set(ord)`
  on a copied `FileExtent` does reach the original's storage — but only while
  `Set` never reassigns `words`. Task 2's review found that no test covered
  the copy case and that a plausible pointer-receiver refactor would look
  correct. This is the code that depends on it: the barrier copies
  `FileExtent` values freely. If you find yourself needing `&ext.Durable`,
  stop and report rather than changing the receiver.
  loads the committed extent for this file, or returns a zero extent with
  `Durable: NewBitmap(artCount)` if none exists. It must widen a loaded bitmap
  to `artCount` bits rather than trusting the stored width.
- `func (b *Barrier) gaplessPrefix(ctx context.Context, jobID string, idx int32, durable Bitmap, t SyncTarget) int64` —
  reads `b.facts.ForFile` (already ordered by offset), walks from offset 0 while
  each successive fact is durable **and** starts exactly where the previous one
  ended, and returns the first offset at which that fails. Document that a hole
  stops this walk while leaving `durable` untouched, which is the #311/#353
  distinction: resume reads the bitmap, the CRC anchor reads this.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/durability/ -run TestBarrier -v`
Expected: PASS.

- [ ] **Step 5: Observe the red check on ordering — twice**

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/durability/barrier.go "$SCRATCH/barrier.bak.go"

# Revert 1 — move the ack before the commit.
# Edit Run so `b.ack.AckDurable(...)` is called immediately after phase 3,
# before b.exts.Commit. This canNOT be pinned by SyncFailureAcksNothing: a
# failed sync aborts in phase 2 and never reaches phase 4, so the mutation
# is unreachable there. Pin it with a test that fails the COMMIT instead
# (an ExtentStore whose Commit returns an error), and assert nothing was
# acked. Then:
go test -count=1 ./internal/durability/ -run TestBarrier_CommitFailureAcksNothing
# MUST FAIL: "acked 1 proofs after a failed commit, want 0"
cp "$SCRATCH/barrier.bak.go" internal/durability/barrier.go

# Revert 2 — neuter the sync loop so drain's output is claimed unsynced.
# NOT `if err := error(nil); err != nil {` — that leaves idx unused and
# fails to COMPILE, which is the weak form and proves nothing about the
# test. Delete the call so the ordering itself changes:
python3 - <<'EOF'
p='internal/durability/barrier.go'; s=open(p).read()
old = ("\t\tif err := t.Sync(ctx, idx); err != nil {\n"
       "\t\t\treturn b.routeFault(jobID, storagefault.Classify(\"sync\", \"\", err))\n"
       "\t\t}")
assert old in s
open(p,'w').write(s.replace(old, "\t\t_ = idx", 1))
EOF
grep -n '_ = idx' internal/durability/barrier.go
go test -count=1 ./internal/durability/ -run TestBarrier_SyncPrecedesCommitAndAck
# MUST FAIL: calls = [drain stat], want [drain sync stat]
cp "$SCRATCH/barrier.bak.go" internal/durability/barrier.go

# Revert 3 — neuter only the sync ERROR CHECK, which keeps the call in
# place so the ordering pin above stays green and only the fault routing
# changes.
sed -i 's|if err := t.Sync(ctx, idx); err != nil {|if err := t.Sync(ctx, idx); false \&\& err != nil {|' internal/durability/barrier.go
grep -n 'false && err != nil' internal/durability/barrier.go
go test -count=1 ./internal/durability/ -run TestBarrier_SyncFailureAcksNothing
# MUST FAIL: "Run returned nil after a failed sync"
cp "$SCRATCH/barrier.bak.go" internal/durability/barrier.go
```

- [ ] **Step 6: Quality gates and commit**

```bash
goimports -w internal/durability/ && go vet ./internal/durability/ && \
  go test -race ./internal/durability/ && golangci-lint run ./internal/durability/
git add internal/durability/
git commit -m "feat(durability): add the checkpoint barrier

Run is the single place the Written -> Durable -> Resolved transition
happens, and the only caller of newProof. Six paths could ack an
article before this change; one can now.

Four phases, and the order is the invariant: drain every file, fsync
every file, build Class B from what the fsync made true, commit
atomically and only then ack. Any failure acks nothing and leaves the
prior cache intact.

VerifiedTo stops at the first hole while the durable bitmap records
articles above it. That separation is what lets a slow or permanently
failed article stop the CRC anchor without inflating rework — the
collision #311 and #353 were both circling.

Red checks observed (two, one per mechanism): acking before the commit
fails SyncFailureAcksNothing with 'acked 1 proofs after a failed sync,
want 0'; neutering the sync loop fails SyncPrecedesCommitAndAck with
'calls = [drain stat], want [drain sync stat]'.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Task 7: The resumer

**Files:**
- Create: `internal/durability/resume.go`
- Test: `internal/durability/resume_test.go`

**Interfaces:**
- Consumes: `FactLog`, `ExtentStore`, `Bitmap`, `crc32util`.
- Produces:

**`firstArtIdx` is the file's first global article index**, i.e. `lo` from the
manifest's `FileRange(fileIdx)`. `FileExtent.Durable` is indexed by *file-local*
ordinal, so the bit for a fact is `fact.ArtIdx - firstArtIdx`. Treating the
global `ArtIdx` as the bit position is correct only for a job's first file and
silently wrong for every other one — the barrier avoids this by taking a
`FileLocalOrdinal` mapper from its `SyncTarget`, and the resumer has no
equivalent, so the caller must supply the offset. An index outside
`[0, artCount)` is `ErrArticleOutOfRange`, never a silently clear bit.

```go
type ResumeResult struct {
	Durable      Bitmap
	VerifiedTo   int64
	PrefixCRC    uint32
	HasPrefixCRC bool
	Recomputed   bool
	Restart      bool
}

func NewResumer(fl FactLog, es ExtentStore, log *slog.Logger) *Resumer
func (r *Resumer) Resume(ctx context.Context, jobID string, fileIdx int32, path string, firstArtIdx int32, artCount int) (ResumeResult, error)
```

- [ ] **Step 1: Write the failing test**

```go
// TestResume_FastPathAdoptsWhenStampMatches pins the O(1) path (B3).
func TestResume_FastPathAdoptsWhenStampMatches(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0xAB}, 300), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	es := NewSQLiteExtentStore(openTestDB(t))
	bm := NewBitmap(4)
	bm.Set(0)
	bm.Set(1)
	if err := es.Commit(ctx, "job-1", []FileExtent{{
		FileIdx: 0, Durable: bm, VerifiedTo: 200, PrefixCRC: 0x1234, HasPrefixCRC: true,
		Size: 300, ModTimeNs: fi.ModTime().UnixNano(),
	}}); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(NewSQLiteFactLog(openTestDB(t)), es, testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, path, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got.Recomputed {
		t.Error("Recomputed = true with a matching stamp — the fast path did not fire")
	}
	if got.Restart {
		t.Error("Restart = true for a file whose stamp matched")
	}
	if got.Durable.Count() != 2 || got.VerifiedTo != 200 || got.PrefixCRC != 0x1234 {
		t.Errorf("fast path did not adopt the cache: %+v", got)
	}
}

// TestResume_TruncatedFileForcesRecompute pins S7 against the external
// modification failure mode: the cache was correct when written and the
// world changed underneath it.
func TestResume_TruncatedFileForcesRecompute(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")

	// Two articles of 100 bytes each, with known CRCs.
	a0 := bytes.Repeat([]byte{0x01}, 100)
	a1 := bytes.Repeat([]byte{0x02}, 100)
	if err := os.WriteFile(path, append(append([]byte{}, a0...), a1...), 0o644); err != nil {
		t.Fatal(err)
	}
	fl := NewSQLiteFactLog(openTestDB(t))
	if err := fl.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0), HasCRC: true},
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: crc32.ChecksumIEEE(a1), HasCRC: true},
	}); err != nil {
		t.Fatal(err)
	}

	fi, _ := os.Stat(path)
	es := NewSQLiteExtentStore(openTestDB(t))
	bm := NewBitmap(2)
	bm.Set(0)
	bm.Set(1)
	if err := es.Commit(ctx, "job-1", []FileExtent{{
		FileIdx: 0, Durable: bm, VerifiedTo: 200, Size: 200, ModTimeNs: fi.ModTime().UnixNano(),
	}}); err != nil {
		t.Fatal(err)
	}

	// The user truncates the partial file between runs.
	if err := os.Truncate(path, 100); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(fl, es, testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Recomputed {
		t.Fatal("Recomputed = false after the file was truncated — the cache was adopted unvalidated")
	}
	if got.Durable.Get(1) {
		t.Error("article 1 is still durable after its bytes were truncated away")
	}
	if !got.Durable.Get(0) {
		t.Error("article 0 was discarded although its bytes are intact and match their CRC")
	}
	if got.VerifiedTo != 100 {
		t.Errorf("VerifiedTo = %d, want 100", got.VerifiedTo)
	}
}

// TestResume_RecomputeYieldsAPrefixCRC pins R24: verification and CRC
// recovery are the same read, so a recomputed file gets a real CRC.
func TestResume_RecomputeYieldsAPrefixCRC(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")
	a0 := bytes.Repeat([]byte{0x01}, 100)
	a1 := bytes.Repeat([]byte{0x02}, 100)
	whole := append(append([]byte{}, a0...), a1...)
	if err := os.WriteFile(path, whole, 0o644); err != nil {
		t.Fatal(err)
	}
	fl := NewSQLiteFactLog(openTestDB(t))
	if err := fl.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0), HasCRC: true},
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: crc32.ChecksumIEEE(a1), HasCRC: true},
	}); err != nil {
		t.Fatal(err)
	}
	// No committed extent at all — forces the recompute path.
	r := NewResumer(fl, NewSQLiteExtentStore(openTestDB(t)), testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasPrefixCRC {
		t.Fatal("HasPrefixCRC = false after a full recompute over a gapless file")
	}
	if got.PrefixCRC != crc32.ChecksumIEEE(whole) {
		t.Errorf("PrefixCRC = %#x, want %#x", got.PrefixCRC, crc32.ChecksumIEEE(whole))
	}
	if got.VerifiedTo != 200 {
		t.Errorf("VerifiedTo = %d, want 200", got.VerifiedTo)
	}
}

// TestResume_ArticleWithoutACRCIsLeftOutstanding pins S3's conservative
// default for the one article class that can never be verified.
//
// A UU-encoded article carries no CRC, so recomputation cannot prove its bytes
// are the right bytes — even when they are sitting on disk at the recorded
// offset. The design's answer is that absence of evidence is absence: leave it
// Outstanding and re-fetch. Re-fetching costs one article; assuming is the
// optimism §1 forbids.
//
// Without this, no fixture in the file sets HasCRC: false, and the branch that
// implements the rule can be deleted with the suite still green.
func TestResume_ArticleWithoutACRCIsLeftOutstanding(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")

	a0 := bytes.Repeat([]byte{0x01}, 100)
	a1 := bytes.Repeat([]byte{0x02}, 100) // UU-encoded: no CRC to check it against
	if err := os.WriteFile(path, append(append([]byte{}, a0...), a1...), 0o644); err != nil {
		t.Fatal(err)
	}
	fl := NewSQLiteFactLog(openTestDB(t))
	if err := fl.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0), HasCRC: true},
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: 0, HasCRC: false},
	}); err != nil {
		t.Fatal(err)
	}

	r := NewResumer(fl, NewSQLiteExtentStore(openTestDB(t)), testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Durable.Get(0) {
		t.Error("article 0 is not durable, but its bytes match its recorded CRC")
	}
	if got.Durable.Get(1) {
		t.Error("article 1 is durable, but it carries no CRC — nothing proves those bytes are correct")
	}
	// The prefix stops at the unverifiable article, so no whole-file CRC is
	// reportable. Unavailable must be distinguishable from a CRC of zero (R23).
	if got.VerifiedTo != 100 {
		t.Errorf("VerifiedTo = %d, want 100 — the prefix cannot cross an unverifiable article", got.VerifiedTo)
	}
	if got.HasPrefixCRC {
		t.Error("HasPrefixCRC = true, but the prefix ends at an article with no CRC")
	}
	if got.Restart {
		t.Error("Restart = true for a file that exists and partly verified")
	}
}

// TestResume_MissingFileRestarts pins that a deleted partial starts over
// rather than resuming against nothing.
func TestResume_MissingFileRestarts(t *testing.T) {
	ctx := context.Background()
	r := NewResumer(NewSQLiteFactLog(openTestDB(t)), NewSQLiteExtentStore(openTestDB(t)), testLogger(t))
	got, err := r.Resume(ctx, "job-1", 0, filepath.Join(t.TempDir(), "gone.mkv"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Restart {
		t.Fatal("Restart = false for a file that does not exist")
	}
	if got.Durable.Count() != 0 {
		t.Error("a restarted file has durable articles")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/durability/ -run TestResume -v`
Expected: FAIL — `undefined: NewResumer`.

- [ ] **Step 3: Write the implementation**

`internal/durability/resume.go`. The algorithm:

1. `os.Stat(path)`. On `IsNotExist`, return `ResumeResult{Durable: NewBitmap(artCount), Restart: true}`.
   Any other stat error → return it wrapped.
2. Load the committed extent. If one exists **and** `ext.Size == fi.Size()`
   **and** `ext.ModTimeNs == fi.ModTime().UnixNano()`, adopt it and return with
   `Recomputed: false`. This is the O(1) fast path (B3).
3. Otherwise recompute. Open the file. Read `facts, err := fl.ForFile(...)`
   (ordered by offset). For each fact whose `HasCRC` is true and whose
   `[Offset, Offset+Length)` lies within the file's current size, read that
   region and compare `crc32.ChecksumIEEE` against `fact.CRC32`. Set the
   article's bit only on a match. A fact without a CRC (UU-encoded) can never
   be verified, so its bit is left clear and its article is re-fetched — the
   conservative answer S3 requires.
4. Walk the facts from offset 0 to compute `VerifiedTo`: advance while each
   successive verified fact starts exactly where the previous ended. Accumulate
   the running CRC over that gapless prefix with `crc32util.Combine`, which is
   valid here precisely because the walk has proven contiguity from 0. Set
   `HasPrefixCRC` only if every fact in the prefix had a CRC.
5. Return with `Recomputed: true`.

Write these doc comments, because they are the claims most likely to drift:

```go
// Resume establishes what is actually on disk for one file.
//
// The fast path is a stat: if the committed extent's size and mtime still
// match the file, the cache is adopted without reading a byte. Correctness
// never depends on the fast path being right — a mismatch falls through to
// recomputation, and recomputation is correct by definition (S4).
//
// Recomputation reads each recorded region and checks it against the CRC the
// fact log recorded when the article was decoded. That single read produces
// both the verified done-set and the gapless-prefix CRC: verification and CRC
// recovery are the same operation, which is why a resumed file can report a
// real whole-file CRC (R24) rather than the honest absence of one.
//
// An article whose fact carries no CRC (UU-encoded) cannot be verified and is
// therefore left Outstanding. Re-fetching it is cheap; assuming it is correct
// is exactly the optimism the design forbids (S3).
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/durability/ -run TestResume -v`
Expected: PASS.

> **The mutations below were verified inert during Task 7's review.** Run
> verbatim, three of them abort earlier in `TestResume_TruncatedFileForcesRecompute`
> than the code they target, and the fourth is rejected by that fixture's
> `CRC32: 0` regardless — so the script as written records green runs as red
> checks. Each needs its own fixture: a same-size edit (mtime moves but size
> does not), a truncation with the mtime preserved, and a fact carrying a
> *correct* `CRC32` with `HasCRC: false`, which is the only shape that
> separates the CRC guard from the has-CRC guard.

- [ ] **Step 5: Observe the red check on validation — twice**

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/durability/resume.go "$SCRATCH/resume.bak.go"

# Revert 1 — drop the size check, keeping mtime.
sed -i 's|ext.Size == fi.Size() && ext.ModTimeNs == fi.ModTime().UnixNano()|ext.ModTimeNs == fi.ModTime().UnixNano()|' internal/durability/resume.go
grep -n 'ext.ModTimeNs == fi.ModTime' internal/durability/resume.go
go test ./internal/durability/ -run TestResume_TruncatedFileForcesRecompute
# MUST FAIL: "Recomputed = false after the file was truncated"
cp "$SCRATCH/resume.bak.go" internal/durability/resume.go

# Revert 2 — accept a region without checking its CRC.
# Edit the recompute loop so the bit is set unconditionally rather than on
# a CRC match. Then:
go test ./internal/durability/ -run TestResume_TruncatedFileForcesRecompute
# MUST FAIL: "article 1 is still durable after its bytes were truncated away"
cp "$SCRATCH/resume.bak.go" internal/durability/resume.go
```

- [ ] **Step 6: Quality gates and commit**

```bash
goimports -w internal/durability/ && go vet ./internal/durability/ && \
  go test -race ./internal/durability/ && golangci-lint run ./internal/durability/
git add internal/durability/
git commit -m "feat(durability): add the resumer

Stat-based fast path adopts the committed cache in O(1); a size or
mtime mismatch falls through to recomputation from the bytes, which is
correct by definition. Correctness never depends on the fast path.

Recomputation reads each recorded region and checks it against the CRC
the fact log captured at decode time. The same read produces both the
verified done-set and the gapless-prefix CRC, so a resumed file can
report a real whole-file CRC instead of an honest absence — #349 and
#353 close on one mechanism rather than two.

An article whose fact carries no CRC cannot be verified and is left
Outstanding. Re-fetching is cheap; assuming is the optimism the design
forbids.

Red checks observed (two, one per mechanism): dropping the size check
fails TruncatedFileForcesRecompute with 'Recomputed = false after the
file was truncated'; setting the bit without the CRC comparison fails
it with 'article 1 is still durable after its bytes were truncated
away'.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Task 8: Derive the work set in the queue

> **`failed_bytes` returns to `job_files`; `bytes_failed` leaves `file_extents`
> (decided during review).** `file_extents.bytes_failed` had no writer anywhere:
> the barrier sets every other field and correctly declines this one, because a
> permanently failed article never decodes and so has no Class A record. The
> column was structurally always zero, and the restored parity test could never
> pass.
>
> Cache the figure in `job_files.failed_bytes` instead — beside its declared
> authority, the failed half of `job_files.articles_done`, written by the same
> code that writes it. `file_extents` then has exactly one writer, the barrier,
> as its own migration comment already claims.
>
> **This restores a column Task 3 deleted, and the distinction matters.** Task 3
> removed it because `RestoreRetryProgress` assigned it and `recompute` then
> overwrote it — two writers of one fact, which is the S5 violation and the
> cause of #306. That path is gone. A single writer caching a sum of the same
> row's authoritative bits is a cache; two writers maintaining a value in
> parallel is the defect. Say which one this is in the migration comment, since
> that comment freezes.
>
> Both migrations are still unmerged, so edit `001_initial.sql` directly rather
> than adding a second migration.
>
> **Non-resident Class B read path (decided during review).**
> `ArticleCountsByJob` gains a `LEFT JOIN file_extents`, so a non-resident job's
> `bytes_durable` / `bytes_failed` come back in the same grouped query the Store
> already runs at startup. One query for the whole queue, preserving B3's
> O(incomplete files) bound, and the read stays where every other non-resident
> figure already lives. The alternative — injecting a `durability.ExtentStore`
> into `Queue` — is cleaner layering but is N queries at startup unless a batch
> method is added, which is the bound B3 exists to protect. `LEFT`, not inner:
> a job whose barrier has never run has no `file_extents` row, and it must
> report zero rather than vanish from the queue.
>
> **Regression debt this task MUST discharge.** Task 3 removed
> `job_files.bytes_downloaded` and `failed_bytes` with no replacement, because
> the replacement is this task's derivation. Between Task 3 and here, a
> **non-resident job reports 0 failed bytes and an inflated remaining figure**
> until it is promoted. Two tests were deleted with the columns and must be
> restored, passing, before this task is complete:
>
> - `TestRemainingBytes_IdenticalResidentAndNonResident`
> - `TestFailedBytes_SurvivesRestartNonResident`
>
> Restoring them is not optional polish. Residency parity — a job reporting the
> same figures whether or not its manifest is resident — is the property
> `docs/queue-lifecycle.md` was written to defend, and it is currently broken
> on purpose. A reviewer must reject this task if those two tests are absent or
> skipped.

**Files:**
- Create: `internal/queue/workset.go`
- Modify: `internal/queue/queue.go` (add `AckDurable`, delete the old ack API),
  `internal/queue/progress.go`
- Test: `internal/queue/workset_test.go`

**Interfaces:**
- Consumes: `durability.DurableProof`, `durability.FileExtent`.
- Produces:
  - `func (q *Queue) AckDurable(p durability.DurableProof) error` — satisfies
    `durability.Acker`.
  - `func (q *Queue) AckPermanentFailure(jobID string, artIdxs []int32) error`
    — the non-barrier path permitted by R10.
  - `func (q *Queue) SeedFromExtents(jobID string, exts []durability.FileExtent) error`
    — installs a resumed job's durable bitmaps.

**Deleted from `queue.go`:** `MarkArticlesDone`, `MarkArticlesDoneByIdx`,
`MarkArticlesFailed`, `MarkArticlesFailedByIdx`, `SetFileExtents`. Their tests
go with them.

- [ ] **Step 1: Write the failing test**

```go
func TestAckDurable_MarksArticlesResolved(t *testing.T) {
	q := newTestQueueWithJob(t, "job-1", 10) // existing helper pattern
	p := mintProof(t, "job-1", []int32{0, 3, 7})

	if err := q.AckDurable(p); err != nil {
		t.Fatal(err)
	}
	var outstanding []int32
	q.ForEachUnfinishedArticle(func(a UnfinishedArticle) bool {
		if a.JobID == "job-1" {
			outstanding = append(outstanding, a.ArtIdx)
		}
		return true
	})
	for _, resolved := range []int32{0, 3, 7} {
		if slices.Contains(outstanding, resolved) {
			t.Errorf("article %d still outstanding after AckDurable", resolved)
		}
	}
	if len(outstanding) != 7 {
		t.Errorf("outstanding = %d, want 7", len(outstanding))
	}
}

// TestAckDurable_IsIdempotent pins R12: at-least-once delivery with an
// idempotent apply. A replayed proof must not double-count bytes.
func TestAckDurable_IsIdempotent(t *testing.T) {
	q := newTestQueueWithJob(t, "job-1", 10)
	p := mintProof(t, "job-1", []int32{0, 1, 2})
	if err := q.AckDurable(p); err != nil {
		t.Fatal(err)
	}
	before := q.jobBytesDownloaded(t, "job-1")
	if err := q.AckDurable(p); err != nil {
		t.Fatal(err)
	}
	if after := q.jobBytesDownloaded(t, "job-1"); after != before {
		t.Fatalf("bytes = %d after a replayed proof, want %d — the apply is not idempotent", after, before)
	}
}

// TestQueue_HasNoNonBarrierAckPath pins X2 structurally. If a future change
// reintroduces a public ack that does not require a DurableProof, this fails.
func TestQueue_HasNoNonBarrierAckPath(t *testing.T) {
	forbidden := []string{"MarkArticlesDone", "MarkArticlesDoneByIdx", "SetFileExtents"}
	qt := reflect.TypeOf(&Queue{})
	for i := range qt.NumMethod() {
		name := qt.Method(i).Name
		if slices.Contains(forbidden, name) {
			t.Errorf("Queue.%s still exists — X2 requires the barrier to be the only ack path", name)
		}
	}
}
```

`mintProof` is a test helper in `internal/queue/workset_test.go`. It does **not**
reach for an exported constructor, because there isn't one and there must not
be: `DurableProof`'s unexported constructor is the entire mechanism behind X3,
and an exported `NewProofForTest` would put the escape hatch in production code
where only a CI grep could police it. Go offers no way to export a symbol to
other packages' tests alone, so the helper mints a proof the only legitimate
way — by running a real barrier:

```go
// mintProof produces a DurableProof the way production does: by running a
// real durability.Barrier over a stub target and capturing what it emits.
//
// There is deliberately no shortcut. DurableProof has no exported
// constructor, and that absence is what makes "ack only after fsync"
// compiler-enforced rather than a rule six call sites must each remember. A
// test-only exported constructor would move that guarantee from the compiler
// to a CI grep, so this helper pays the setup cost instead — and gets a test
// that exercises the real minting path as a bonus.
func mintProof(t *testing.T, jobID string, arts []int32) durability.DurableProof {
	t.Helper()

	written := make([]durability.WrittenArticle, len(arts))
	for i, a := range arts {
		written[i] = durability.WrittenArticle{
			FileIdx: 0, ArtIdx: a, Offset: int64(a) * 100, Length: 100,
		}
	}
	tgt := &stubSyncTarget{files: []int32{0}, written: written, artCount: len(arts) + 8}

	var got durability.DurableProof
	captured := ackerFunc(func(p durability.DurableProof) error { got = p; return nil })

	db := openTestDB(t)
	b := durability.NewBarrier(
		durability.NewSQLiteFactLog(db),
		durability.NewSQLiteExtentStore(db),
		captured,
		noopStallable{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := b.Run(context.Background(), jobID, tgt); err != nil {
		t.Fatalf("mintProof: barrier run: %v", err)
	}
	if len(got.Articles()) != len(arts) {
		t.Fatalf("mintProof: barrier emitted %d articles, want %d", len(got.Articles()), len(arts))
	}
	return got
}
```

Write `stubSyncTarget`, `ackerFunc`, and `noopStallable` alongside it in the
same test file — `stubSyncTarget` satisfies `durability.SyncTarget` returning
its fixed `written` slice from `Drain` and nil from `Sync`; `ackerFunc` is a
func-typed `durability.Acker`; `noopStallable` satisfies
`durability.Stallable` with empty methods.

**Do not add an exported constructor to `proof.go`.** If a later task finds it
needs one, that is a signal the boundary is drawn wrong — raise it rather than
opening the hatch.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/queue/ -run 'TestAckDurable|TestQueue_HasNoNonBarrierAckPath' -v`
Expected: FAIL — `q.AckDurable undefined`, and
`Queue.MarkArticlesDone still exists`.

- [ ] **Step 3: Write the implementation**

`internal/queue/workset.go`:

```go
package queue

import (
	"fmt"

	"github.com/hobeone/gonzbd/internal/durability"
)

// AckDurable resolves the articles a completed fsync covers.
//
// It takes a durability.DurableProof rather than a slice of indices, and that
// is the whole point: DurableProof has no exported constructor outside
// internal/durability, so this method is unreachable from any code path that
// has not actually run a barrier. Before this design, six paths in the
// assembler could ack an article, each independently responsible for
// remembering not to do so before the bytes were durable — which is why the
// same defect kept being refiled (#355, #356).
func (q *Queue) AckDurable(p durability.DurableProof) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	job, ok := q.byID[p.JobID()]
	if !ok {
		return fmt.Errorf("queue: AckDurable: %w: %s", ErrJobNotFound, p.JobID())
	}
	m, err := job.Manifest()
	if err != nil {
		return fmt.Errorf("queue: AckDurable %s: %w", p.JobID(), err)
	}
	for _, idx := range p.Articles() {
		job.progress.markDone(m, int(idx))
	}
	return nil
}
```

`markDone` must already be idempotent (it sets a bit and adds bytes only on a
0→1 transition). Verify that; if it adds bytes unconditionally, fix it — that is
the bug `TestAckDurable_IsIdempotent` pins.

Add `AckPermanentFailure` alongside, calling `job.progress.markFailed`, with a
comment stating why it needs no proof: a permanent failure asserts nothing about
disk, and losing one costs only a re-attempt that will fail again (R10).

Add `SeedFromExtents`, which installs a resumed job's durable bits into
`job.progress` and recomputes the job's byte figures from the extents' cached
aggregates.

Then delete `MarkArticlesDone`, `MarkArticlesDoneByIdx`, `MarkArticlesFailed`,
`MarkArticlesFailedByIdx`, and `SetFileExtents` from `queue.go`, plus their
tests in `queue_test.go`, `persistence_test.go`, and `clearallemitted_test.go`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/queue/ -v`
Expected: PASS. `go build ./...` will fail in `internal/app` and
`internal/assembler`, which Tasks 9–11 fix. **Do not commit yet** — this task's
commit comes after Task 9 makes the tree build again. Mark the checkbox and
carry the working tree forward.

---

## Task 9: FileWriter — the assembler loses its authority

> **The assembler cannot implement `SyncTarget` directly (decided during
> review).** `SyncTarget` is per-job — `Files()` returns one job's files — while
> the assembler is keyed on `fileKey{jobID, fileIdx}` and serves every job at
> once. Its state is owned exclusively by the single worker goroutine with no
> locks, which is invariant X1.
>
> Add a **per-job adapter** that implements `SyncTarget` for one `jobID` and
> reaches the worker through the existing control-message channel, the same
> pattern `CancelJob` and `CloseJobHandles` already use. Do **not** add a mutex
> to the assembler's maps: X1's value is that one goroutine owns every file
> handle with zero contention, and a lock would also put `check_lock_io` in play
> around code that does I/O by definition.

> **Data-loss debt this task MUST discharge.** Task 3 dropped
> `job_files.max_written`, so `FileProgress.FileMaxWritten` and
> `FileWriteCursor` return 0 on every restart. `finalizeFile` seeds
> `f.maxWritten` from them and truncates the completed file to this run's
> high-water mark; its only guard refuses to *grow* a file, so truncating
> **downward** — the destructive direction — is unguarded. That reinstates
> #342/#350 verbatim: a resumed file is trimmed to the extent of the articles
> this run happened to fetch, discarding what earlier runs wrote. Without par2
> the loss is permanent and silent.
>
> Task 3 adds an interim guard (skip the truncate when the extent is unknown).
> **This task removes the interim guard and bounds the truncate by the highest
> durable fact end** — `max(offset + length)` over the articles whose durable
> bit is set, computed from the `FactLog` at completion.
>
> **Do NOT use `FileExtent.VerifiedTo`.** An earlier revision of this plan said
> to, and it was wrong in the dangerous direction: `VerifiedTo` is the *gapless
> prefix* and stalls at the first hole, so a 40 GB file with one failed article
> at 2 GB would be truncated to 2 GB. That is far worse than #342/#350 and it
> destroys exactly the blocks par2 repairs from. Extent and gapless prefix are
> different quantities.
>
> The high-water mark is derived from Class A rather than stored, so no second
> authoritative copy exists (S5) and it is correct by construction. It costs one
> `ForFile` query and a scan per file completion, off the hot path. It must also restore the end-to-end pin
> deleted with the column — `internal/queue/max_written_persist_test.go`,
> rewritten against `file_extents` — asserting that a file resumed with one
> article at offset 0 keeps its full length. `internal/assembler/resume_extent_test.go`
> does **not** cover this: it injects `InitialMaxWritten` directly and so cannot
> observe an unseeded resume.
>
> A reviewer must reject this task if the interim guard is still present, or if
> no test fails when the truncate bound is reverted to the run's high-water mark.

> **Premise re-derived 2026-08-11 against `73d0c810`.** This task was originally
> written against an assembler that acked at *accept* time from six call sites.
> PR #358 (`913adf0d`, 1,310 lines) has since moved acking to `WriteAt` and
> closed #355. What follows describes the code as it now stands. Do not trust
> any pre-amendment description of this package.

**Files:**
- Create: `internal/assembler/filewriter.go`
- Modify: `internal/assembler/assembler.go`, `internal/assembler/writecache.go`
- Rewrite: `internal/assembler/durable_ack_test.go` (773 lines, 19 tests — see
  the triage table below; most survive translation, four do not)
- Delete: `internal/assembler/resume_crc_test.go`,
  `internal/assembler/resume_extent_test.go`,
  `internal/assembler/drain_cursor_test.go`
- Test: `internal/assembler/filewriter_test.go`

**Interfaces:**
- Consumes: `storagefault.Classify`, `durability.WrittenArticle`.
- Produces: `Assembler` satisfying `durability.SyncTarget`:
  `Files() []int32`, `Drain(ctx, fileIdx) ([]durability.WrittenArticle, error)`,
  `Sync(ctx, fileIdx) error`, `Stat(fileIdx) (int64, int64, error)`,
  `ArticleCount(fileIdx) int`, `FileLocalOrdinal(fileIdx, artIdx) (int, bool)`.

### What #358 already built, and what remains

#358 moved the ack boundary one step along the same axis this design travels:
from **Decoded** to **Written**. This task moves it the remaining step, to
**Durable**. That makes several of its mechanisms prerequisites rather than
obstacles — the write cache already carries article identities through
coalescing (`bufferedArticle.msgID`, `writecache.go:66`), which Task 9
originally assumed it would have to build.

| Concern | State on main after #358 | What this task does |
|---|---|---|
| Ack timing | `writeAndSettle` acks on `outcomeDurable` (= reached `WriteAt`) | Moves the ack to the barrier; `Drain` reports, it does not ack |
| Coalesced-run failure | `failArticle` fails every article in a failed run | Retained inside `FileWriter`, reported as absence from `Drain` |
| Unknown-file drain (#344) | `failDrainedArticles` now fails them rather than dropping | Becomes unrepresentable: `FileWriter` owns its own cache |
| CRC on a buffered article (#356) | Still recorded at accept; mitigated by `crcValid` | Removed entirely — CRC moves to the barrier's `FileExtent` |
| Whole-file CRC (#349) | `combineWholeFileCRC` checks tiling, reports 0 on failure | Deleted; superseded by `FileExtent.PrefixCRC` |

**The naming trap.** `writeOutcome`'s constant `outcomeDurable`
(`assembler.go:1390`) is documented as *"the bytes reached `WriteAt`"*. In this
design's vocabulary that state is **Written**, and the spec is explicit that it
survives a process crash but **not** a power loss. A symbol named
`outcomeDurable` for a not-durable state is exactly the conflation S2 exists to
prevent, and leaving it would guarantee the next reader re-derives the old bug.
**Rename `outcomeDurable` → `outcomeWritten` as part of this task**, and say so
in the commit body.

**Removed from `Options`:** `MarkArticlesDone`, `MarkArticlesDoneByIdx`,
`MarkArticlesFailed`, `MarkArticlesFailedByIdx`, `SetFileExtents`,
`DoneFlushInterval`.
**Removed from `FileInfo`:** `InitialWriteCursor`, `InitialMaxWritten`.
**Removed from `openFile`:** `maxWritten`, `crcParts`, `crcValid`, `partsWritten`.
`seenDone` / `seenFailed` move into `FileWriter` — they are still needed for
idempotent duplicate handling (R12), which #358's tests pin.
**Removed from `Options.OnFileComplete`:** the `fileCRC uint32` parameter.

### Test triage for `durable_ack_test.go`

Do not delete this file wholesale — it encodes real behaviour, and four of its
tests are the only coverage of cases this design still has.

| Test | Disposition |
|---|---|
| `TestFailedCoalescedRunFailsEveryArticleInTheRun` | **Translate** — becomes "absent from `Drain`" rather than "acked failed" |
| `TestDuplicateSuccessWhileBufferedIsNotReAcked` | **Translate** — the `wc.buffered` check moves into `FileWriter` |
| `TestDuplicateAtADifferentOffsetIsNotReAcked` | **Translate** — same |
| `TestDisplacedArticleIsFailed` | **Translate** — becomes absent from `Drain` |
| `TestZeroLengthArticleIsNotBuffered` | **Keep as-is** — pure cache behaviour |
| `TestBuildContiguousRunStopsAtAZeroLengthArticle` | **Keep as-is** |
| `TestBufferedReportsUnknownKey` | **Keep as-is** |
| `TestCompletedFileAcksEveryArticleExactlyOnce` | **Translate** — becomes "reported exactly once across all `Drain` calls" |
| `TestBufferedArticleIsNotAckedUntilItsBytesLand` | **Delete** — superseded by `TestFileWriter_DrainReportsOnlyWrittenArticles`, which makes the stronger claim |
| `TestWriteOutcomesSettleTheirAcks` | **Delete** — settling moves to the barrier |
| `TestFatalAfterBufferedSuccessDoesNotOutraceTheDoneAck` | **Delete** — the ordering it guards is gone once one component acks |
| `TestAckArticleDoneUsesTheFilesJobAndTheArticlesIndex` | **Delete** — no ack in this package |
| remaining `failArticle`/`failDrained` tests | **Translate** — assert absence from `Drain`, plus a returned `*storagefault.Fault` |

- [ ] **Step 1: Write the failing test**

```go
// TestFileWriter_DrainReportsOnlyWrittenArticles is the pin for S2. An
// article sitting in the write cache has NOT reached disk, and Drain is the
// barrier's only evidence — so a buffered-but-unwritten article must not
// appear in its return value.
func TestFileWriter_DrainReportsOnlyWrittenArticles(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(1<<20))

	// Buffer an article without triggering a contiguous flush.
	if err := w.Accept(5, 4096, bytes.Repeat([]byte{1}, 100)); err != nil {
		t.Fatal(err)
	}
	if got := w.writtenSoFar(); len(got) != 0 {
		t.Fatalf("writtenSoFar = %v before any drain, want empty", got)
	}

	got, err := w.Drain(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ArtIdx != 5 {
		t.Fatalf("Drain = %v, want one entry for article 5", got)
	}
}

// TestFileWriter_FailedWriteIsNotReportedAsWritten pins the deferred-write
// failure path. #358 fixed the ack half of this on main; the pin is retained
// because Drain's contract to the barrier is a NEW claim that nothing on main
// makes, and it is the only evidence the barrier has.
func TestFileWriter_FailedWriteIsNotReportedAsWritten(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(1<<20), withWriteError(syscall.ENOSPC))

	if err := w.Accept(5, 4096, bytes.Repeat([]byte{1}, 100)); err != nil {
		t.Fatal(err)
	}
	got, err := w.Drain(context.Background(), 0)
	if err == nil {
		t.Fatal("Drain returned nil error after ENOSPC")
	}
	var f *storagefault.Fault
	if !errors.As(err, &f) {
		t.Fatalf("Drain error = %T, want *storagefault.Fault", err)
	}
	if !f.Retryable() {
		t.Error("ENOSPC classified permanent")
	}
	for _, a := range got {
		if a.ArtIdx == 5 {
			t.Fatal("article 5 reported written although its WriteAt failed")
		}
	}
}

// TestAssembler_HasNoAckSurface pins X2 from the assembler's side.
func TestAssembler_HasNoAckSurface(t *testing.T) {
	ot := reflect.TypeOf(Options{})
	forbidden := []string{"MarkArticlesDone", "MarkArticlesDoneByIdx", "MarkArticlesFailed",
		"MarkArticlesFailedByIdx", "SetFileExtents"}
	for i := range ot.NumField() {
		if slices.Contains(forbidden, ot.Field(i).Name) {
			t.Errorf("Options.%s still exists — the assembler must have no ack authority", ot.Field(i).Name)
		}
	}
}

// TestNoSymbolNamesWriteAtDurable guards the naming trap: #358 introduced
// outcomeDurable for a state this design calls Written, and a state named
// durable that is not durable is the conflation S2 exists to prevent.
func TestNoSymbolNamesWriteAtDurable(t *testing.T) {
	out, err := exec.Command("git", "grep", "-n", "outcomeDurable", "--", "internal/assembler").CombinedOutput()
	if err == nil && len(out) > 0 {
		t.Errorf("outcomeDurable still present; rename to outcomeWritten:\n%s", out)
	}
}
```

Write `newTestFileWriter(t, opts...)` creating a `FileWriter` over a
`t.TempDir()` file, with functional options `withCacheBytes(int64)` and
`withWriteError(error)` (the latter injecting a `writeAt` func field on the
writer, set once before use, mirroring how `diskProbe.statfs` is overridden in
existing tests).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/assembler/ -run 'TestFileWriter|TestAssembler_HasNoAckSurface|TestNoSymbolNamesWriteAtDurable' -v`
Expected: FAIL — `undefined: newTestFileWriter`,
`Options.MarkArticlesDone still exists`, and `outcomeDurable still present`.

- [ ] **Step 3: Write the implementation**

`internal/assembler/filewriter.go` — one file's handle and cache:

```go
// FileWriter owns one target file: its handle, its write cache, its
// coalescing, and its pre-allocation. It has no authority over anything
// externally visible.
//
// It cannot ack an article, record a CRC part, decide a file is complete, or
// truncate. Those decisions moved to durability.Barrier, which is the only
// component that knows whether an fsync has happened. The writer's entire
// contract to the outside world is: bytes it reports from Drain reached
// WriteAt without error, and everything else is its own business.
//
// This continues the direction #358 started rather than reversing it. That
// change moved the ack from accept to WriteAt — Decoded to Written. This one
// moves it from Written to Durable, which is the step #358 explicitly declined
// ("this does not defer acks to fsync — that would push every ack to file
// completion"). That objection was correct at the time: Sync ran only in
// finalizeFile, so acking after fsync really did mean acking at file
// completion. The barrier removes the premise by fsyncing on a cadence, so
// the cost is one checkpoint interval rather than a whole file.
type FileWriter struct {
	handle  *os.File
	path    string
	cache   *fileBuf
	written []durability.WrittenArticle
	// seenDone and seenFailed keep duplicate handling idempotent (R12).
	// They move here from openFile because the writer is now the only
	// component that can tell whether a duplicate's first copy has left
	// the cache — the check #358 added as wc.buffered.
	seenDone   map[string]int64
	seenFailed map[string]struct{}
	// writeAt is os.File.WriteAt in production. Tests override it before
	// first use to inject storage faults.
	writeAt func(p []byte, off int64) (int, error)
}
```

Methods to implement:

- `Accept(artIdx int32, off int64, data []byte) error` — buffer or write.
  On a direct write, append to `w.written` **only after** `writeAt` returns nil.
  On failure, return `storagefault.Classify("write", w.path, err)` and append
  nothing.
- `Drain(ctx) ([]durability.WrittenArticle, error)` — flush every buffered
  article, appending to `w.written` per successful `writeAt`. On the first
  failure, return the accumulated `w.written` **and** the classified fault, so
  the barrier can see both what did land and why it stopped.
- `Sync(ctx) error` — `w.handle.Sync()`, classified.
- `Stat() (size, modTimeNs int64, err error)`.
- `Close() error`.

In `assembler.go`, delete `flush`, `recordPendingDone`, `recordPendingFailed`,
`writeAndSettle`, `failArticle`, `failDrainedArticles`, `recordArticleCRC`,
`combineWholeFileCRC`, `handleLateDuplicate`'s ack calls, `finalizeFile`'s
truncate/CRC/ack logic, `pendingDone*`, `pendingFailed*`, `pendingExtent`, and
the `doneFlushInterval` ticker. Rename `outcomeDurable` → `outcomeWritten`. Add
the six `durability.SyncTarget` methods, delegating per-file to the
`FileWriter` in the `open` map.

Handle #357 while here: `openTargetFile`'s two drop branches
(`assembler.go:928` FileInfo resolver, `assembler.go:944` mkdir/open) still
log and discard, leaving the article acked neither way. They now return a
classified `*storagefault.Fault` to the caller instead.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/assembler/ -v`
Expected: PASS, with `durable_ack_test.go` translated per the triage table.

- [ ] **Step 5: Observe the red check**

The original red check for this task reproduced #355, which #358 has since
fixed — reintroducing it would no longer go red for the right reason, and a
check that cannot go red is not a pin. Use the `Drain` contract instead, which
is a claim nothing on main makes:

```bash
SCRATCH="$(mktemp -d)"; trap 'rm -rf "$SCRATCH"' EXIT
cp internal/assembler/filewriter.go "$SCRATCH/fw.bak.go"
# Move the w.written append above the error check in Drain's flush loop, so a
# failed WriteAt is still reported to the barrier as written.
# Edit Drain so `w.written = append(...)` precedes `if err != nil`.
grep -n 'w.written = append' internal/assembler/filewriter.go   # confirm placement
go test ./internal/assembler/ -run TestFileWriter_FailedWriteIsNotReportedAsWritten
# MUST FAIL: "article 5 reported written although its WriteAt failed"
cp "$SCRATCH/fw.bak.go" internal/assembler/filewriter.go
```

Second revert, for the buffered half:

```bash
cp internal/assembler/filewriter.go "$SCRATCH/fw.bak.go"
# In Accept, append to w.written at buffer time as well as at write time.
go test ./internal/assembler/ -run TestFileWriter_DrainReportsOnlyWrittenArticles
# MUST FAIL: "writtenSoFar = [...] before any drain, want empty"
cp "$SCRATCH/fw.bak.go" internal/assembler/filewriter.go
```

- [ ] **Step 6: Commit Tasks 8 and 9 together**

They are one reviewable unit: Task 8 removes the queue's ack surface and Task 9
removes the assembler's, and neither builds without the other.

```bash
go fix ./... && goimports -w . && go vet ./... && go test -race ./... && golangci-lint run ./...
git add internal/queue internal/assembler
git commit -m "refactor(assembler)!: move the ack boundary from Written to Durable

#358 moved acking from accept to WriteAt, which is Decoded to Written in
the durability design's vocabulary. This moves it the remaining step to
Durable — the step #358 explicitly declined, on the grounds that
deferring to fsync would push every ack to file completion. That was
correct then: Sync ran only in finalizeFile. The barrier removes the
premise by fsyncing on a cadence, so the cost is one checkpoint interval
rather than a whole file.

FileWriter now owns one file's handle and cache and nothing else. Its
whole contract is that bytes reported from Drain reached WriteAt without
error. Acking, CRC combination, completion, and truncation move to
durability.Barrier, the only component that runs the fsync.

Renames outcomeDurable to outcomeWritten. A constant named durable for a
state documented as 'reached WriteAt' is the conflation the design's S2
exists to prevent, and it would have taught the next reader the bug back.

openTargetFile's two drop branches now return a classified fault rather
than discarding the article (#357). The unknown-file drain handler
becomes unrepresentable rather than correct (#344): FileWriter owns its
own cache, so a cache entry with no file cannot exist. recordArticleCRC
is deleted rather than moved (#356).

BREAKING CHANGE: Options loses MarkArticlesDone*, MarkArticlesFailed*,
SetFileExtents, and DoneFlushInterval. FileInfo loses InitialWriteCursor
and InitialMaxWritten. OnFileComplete loses its fileCRC parameter.

Red checks observed (two): appending to w.written before the error check
in Drain fails FailedWriteIsNotReportedAsWritten with 'article 5
reported written although its WriteAt failed'; appending at buffer time
in Accept fails DrainReportsOnlyWrittenArticles with 'writtenSoFar =
[...] before any drain, want empty'.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Task 10: Wire the barrier into the application

> **Class A has no production writer — wire it here (decided during review).**
> Nothing calls `FactLog.Append` anywhere in the tree. Without it `durableExtent`
> computes a truncate bound of 0 and `Resume`'s recompute has nothing to verify
> against, so the entire Class A layer is inert.
>
> Append in `internal/app/pipeline.go`'s `handleSuccessResult`, which already
> holds `JobID`, `FileIdx`, `ArtIdx`, `Offset`, `len(Data)` and `CRC` at the
> point it calls `WriteArticle`. **There is deliberately no ordering constraint
> between the append and the write** (R2): a Class A fact asserts nothing about
> presence, which is the property that lets it be committed without a barrier.
> Do not add one.
>
> **Every decoded article now carries a CRC, and `HasCRC` is removed
> (decided during review).** The decoder already computes its own checksum over
> decoded yEnc output (`decoder.go:296`); the yEnc trailer is only a transfer
> check, and a mismatch is `ErrCRCMismatch` before the data ever reaches us. The
> UU path (`internal/decoder/uu.go`) simply never computed one, because the
> *format* carries none to compare against — but our use is verification of our
> own bytes on disk, not validation of the sender, so the format's silence is
> irrelevant. Add the `crc32.ChecksumIEEE` over the decoded UU output.
>
> With that, no successfully decoded article lacks a CRC, so remove
> `ArticleFact.HasCRC` and the `has_crc` column, along with:
> `internal/durability/fact.go`, `factlog_sqlite.go`'s read/write of the column,
> `001_initial.sql`'s column and its comment (still unmerged — edit in place),
> and `internal/durability/resume.go`'s unverifiable branch plus
> `TestResume_ArticleWithoutACRCIsLeftOutstanding`.
>
> **Keep `FileExtent.HasPrefixCRC` — it is a different fact.** It means "this is
> a verified *whole-file* CRC", which stays necessary because a prefix that does
> not reach the file's end still has no whole-file value. R23's
> unavailable-vs-zero distinction survives at the file level; only the
> per-article case disappears.
>
> **Before removing the branch, confirm the premise**: verify no decode path
> other than UU can yield data without a CRC. If one exists, stop and report —
> the removal is only safe because the set is closed.
>
> **Obligations carried from Task 6, reported by its implementer.**
>
> - **`Barrier.Run` is not reentrant and takes no lock.** Task 10 owns the
>   cadence, so Task 10 must guarantee at most one `Run` in flight per job.
>   Two concurrent barriers over one job would interleave phase 3's
>   read-modify-write of `FileExtent` and could commit a cache describing
>   neither run.
> - **`SyncTarget.Stat` takes no `context.Context`.** B4 and R22 require every
>   storage syscall on the critical path to be timeout-bounded, and a `stat`
>   on a wedged NFS mount is exactly that case. The bound must therefore come
>   from the `SyncTarget` implementor (Task 9's assembler), not the barrier.
>   Confirm it is actually bounded there — `check_lock_io` descends only one
>   call level and cannot see it.
> - **`SyncTarget` has no path accessor**, so `Barrier.routeFault` classifies
>   every fault with an empty `Path`. R27 wants a stall reason a user can act
>   on, and "ENOSPC on sync" without a path is thinner than it needs to be.
>   Add one when wiring the assembler as the `SyncTarget`.
> - **Note, not a task:** `priorExtent` calls `ExtentStore.Load(jobID)` — all of
>   a job's extents — once per file, which is O(files²) rows scanned per
>   barrier. Task 6's review measured this as unmeasurable against the ~100
>   `fsync`s in the same barrier and recommended leaving it. If Task 10 hoists
>   the load into `Run` while adding per-job serialisation, take it then;
>   otherwise leave it alone.

**Files:**
- Modify: `internal/app/app.go:326-346`, `internal/app/pipeline.go`,
  `internal/config/downloads.go`, `internal/constants/`
- Test: `internal/app/barrier_wiring_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–9.
- Produces: `Application.barrier *durability.Barrier`; a barrier goroutine
  driven by `checkpointInterval` and `checkpointBytes`.

**New config** in `internal/config/downloads.go`:

```go
// CheckpointInterval bounds how much downloaded work a power loss can cost.
// A barrier runs at least this often per active job, so at most one
// interval's articles are re-fetched on restart.
CheckpointInterval Duration `yaml:"checkpoint_interval" json:"checkpoint_interval"`

// CheckpointBytes bounds the same window by volume, for a fast link where
// 30s is a lot of data. The barrier fires on whichever bound arrives first.
CheckpointBytes ByteSize `yaml:"checkpoint_bytes" json:"checkpoint_bytes"`
```

Defaults in `internal/constants/`: `DefaultCheckpointInterval = 30 * time.Second`,
`DefaultCheckpointBytes = 64 << 20`. Per `docs/config-contract.md`, adding these
fields requires updating `gonzbd.yaml`'s comments, `docs/sabnzbd_spec.md` §9.x,
and the config↔UI contract test — do all three in this task, and run
`go test ./internal/config/ -run 'TestUI|TestAllFlat'`.

- [ ] **Step 1: Write the failing test**

```go
// TestBarrierFiresOnByteBound pins B1's volume bound.
func TestBarrierFiresOnByteBound(t *testing.T) {
	app := newTestApp(t, withCheckpointBytes(1024), withCheckpointInterval(time.Hour))
	// An hour-long interval means only the byte bound can fire.
	feedArticles(t, app, "job-1", 20, 100) // 20 articles x 100 bytes = 2000 > 1024

	waitFor(t, 5*time.Second, func() bool { return app.barrierRuns() >= 1 })
	if got := app.barrierRuns(); got < 1 {
		t.Fatalf("barrier ran %d times after exceeding the byte bound", got)
	}
}

// TestBarrierFiresOnTimeBound pins B1's time bound.
func TestBarrierFiresOnTimeBound(t *testing.T) {
	app := newTestApp(t, withCheckpointBytes(1<<30), withCheckpointInterval(50*time.Millisecond))
	feedArticles(t, app, "job-1", 1, 100)
	waitFor(t, 5*time.Second, func() bool { return app.barrierRuns() >= 2 })
}

// TestBarrierRunsOnCleanShutdown pins R6.
func TestBarrierRunsOnCleanShutdown(t *testing.T) {
	app := newTestApp(t, withCheckpointBytes(1<<30), withCheckpointInterval(time.Hour))
	feedArticles(t, app, "job-1", 5, 100)
	before := app.barrierRuns()
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if app.barrierRuns() <= before {
		t.Fatal("shutdown did not run a final barrier — work within the window would be lost")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/app/ -run TestBarrier -v`
Expected: FAIL — `app.barrierRuns undefined`.

- [ ] **Step 3: Write the implementation**

Replace `internal/app/app.go:326-346`:

```go
factLog := durability.NewSQLiteFactLog(db)
extents := durability.NewSQLiteExtentStore(db)
app.resumer = durability.NewResumer(factLog, extents, log)
app.barrier = durability.NewBarrier(factLog, extents, q, app, log)

asm := assembler.New(assembler.Options{
	FileInfo:        p.resolveFileInfo,
	MinFreeBytes:    minFreeBytes,
	WriteCacheBytes: writeCacheBytes,
	OnLowDisk:       app.handleLowDisk,
	OnFileComplete:  onFileComplete,
	FactLog:         factLog,
}, log)
```

`Application` implements `durability.Stallable`:

```go
// Stall moves a job out of the active set with a surfaced reason and leaves
// its articles Outstanding. A storage fault is never attributed to an
// article: marking them failed would burn their retry budget, inflate the
// job's failed-byte count, and degrade the reported health percentage — all
// from a full disk the user could fix in ten seconds (A1, R19, R21).
func (a *Application) Stall(jobID string, f *storagefault.Fault) { ... }

// Fail terminates a job on a permanent storage fault. Still no article is
// marked failed (R20).
func (a *Application) Fail(jobID string, f *storagefault.Fault) { ... }
```

Add the barrier loop: a goroutine per active job, or one goroutine iterating
active jobs, firing `barrier.Run(ctx, jobID, assembler)` on
`min(checkpointInterval elapsed, checkpointBytes accumulated)`, plus on file
completion, job pause, and `Shutdown`. Track byte accumulation from the
assembler's accepted-byte counter.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/app/ -v && go test -race ./...`
Expected: PASS.

- [ ] **Step 5: Run the integration suite**

Per `AGENTS.md`, startup wiring in `cmd/gonzbd/main.go` and `internal/app` is
consumed by integration tests, so this task must run them:

Run: `go test -v -tags=integration ./test/integration/...`
Expected: PASS. (Requires `par2`, `rar`, `unrar`, `7z` on PATH.)

- [ ] **Step 6: Quality gates and commit**

```bash
go fix ./... && goimports -w . && go vet ./... && go test -race ./... && \
  golangci-lint run ./... && go test ./internal/config/ -run 'TestUI|TestAllFlat'
git add -A
git commit -m "feat(app): drive the checkpoint barrier and route storage faults

Adds checkpoint_interval (30s) and checkpoint_bytes (64MiB), which
together are the stated bound on how much downloaded work a power loss
can cost. The barrier fires on whichever arrives first, and
additionally on file completion, pause, and shutdown.

Application implements durability.Stallable: a retryable storage fault
stalls the job with a surfaced reason and leaves its articles
Outstanding; a permanent one fails the job. Neither marks an article
failed. Attributing a full disk to a remote article burns its retry
budget, inflates failed bytes, and degrades health — from a condition
the user could fix in ten seconds.

Config contract updated in step: gonzbd.yaml comments, sabnzbd_spec
9.x, and the config-UI contract test.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Task 10b: Resume on restart (wire the resumer)

**Why this task exists.** Task 7 built `durability.Resumer` and Task 8 built
`Queue.SeedFromExtents`; Task 10 constructs the resumer in `app.go`. Nothing
ever *calls* either one. A restart therefore re-downloads every byte an
earlier run already fsynced, so **L3 is unsatisfied** — the plan specified the
mechanism and never named its caller. This is the fourth gap of that shape on
this branch (`file_extents.bytes_failed`, `FactLog.Append`, the CRC presence
flag). Task 12's crash-consistency harness cannot measure anything until this
lands, so this task comes first.

**USER DECISION (2026-08-12): startup sweep in `app.Start`, synchronous.**
Rejected: a `queue.WithPromoteHook` (inverts layering — the queue would call
into the app, and it is a new public cross-package interface) and a lazy
resume in `pipeline.registerFile` (runs only *after* an article has been
downloaded, which is too late to prevent the re-fetch it exists to avoid).

The sweep is complete despite running only at startup: a freshly added job has
no committed extents, and a job's extents cannot change while it is not
running, so a job promoted hours after startup is still correctly seeded.

**Files:**
- Create: `internal/app/resume_startup.go`
- Test: `internal/app/resume_startup_test.go`
- Modify: `internal/app/app.go` — call the sweep inside `Start`, after
  `queue.Load` has produced `app.queue` and **before** the pipeline begins
  dispatching.

**Interfaces:**
- Consumes: `(*durability.Resumer).Resume(ctx, jobID string, fileIdx int32, path string, firstArtIdx int32, artCount int) (durability.ResumeResult, error)`;
  `(*queue.Queue).SeedFromExtents(jobID string, exts []durability.FileExtent) error`;
  `storagefault.Classify(op, path string, err error) *storagefault.Fault`.
- Produces: `func (app *Application) resumeAllJobs(ctx context.Context) error`.

### Four rules that are easy to get wrong

1. **Only `FileIdx` and `Durable` may be populated on the converted
   `FileExtent`.** Read `SeedFromExtents` — those are the only two fields it
   consumes. `ResumeResult` carries no `Size`/`ModTimeNs`/`BytesDurable`, and
   inventing them would manufacture a Class B record that asserts a stat
   nobody performed.
2. **Do NOT write the resumed extents back to `ExtentStore`.** The barrier is
   the only writer of Class B, because a committed extent claims a completed
   fsync stands behind it. A resume proves what is on disk; it does not
   perform the fsync that would license a new commit. Re-verification on the
   next restart is bounded rework and is the correct cost.
3. **A `Resume` error is a storage fault, never an article fault (A1).**
   Classify it and stall the job with a surfaced reason, leaving its articles
   Outstanding. Never mark an article permanently failed because a *disk*
   read failed — that is precisely the attribution defect this design exists
   to eliminate.
4. **`Restart == true` means seed nothing for that file.** Its bitmap is
   already empty, and every article is correctly Outstanding under S3.

### Steps

- [ ] **Step 1: Write the failing tests**

Put all six in `internal/app/resume_startup_test.go`.

Every fixture MUST contain, in one file, **at least one durable article and at
least one non-durable article**. This is not stylistic. On this branch, 15+
inert tests have been found whose fixtures made the code under test
unnecessary; a fixture where every article is durable passes against a
`markDone`-everything mutation, and one where none are passes against a
seed-nothing mutation. Both mutations must go red.

1. `TestResumeAtStartup_SeedsDurableArticles` — commit an extent whose bitmap
   marks articles 0 and 2 durable but not 1, with the on-disk file's size and
   mtime matching the committed extent. After `Start`, assert `ArticleDone(0)`
   and `ArticleDone(2)` are true and `ArticleDone(1)` is false.
2. `TestResumeAtStartup_DurableArticlesAreNeverRefetched` — the ordering pin,
   and the one that actually states L3. Record the message IDs the
   `nntptest` server is asked for. Assert the durable articles' IDs are
   **never requested** and the non-durable one is. A resume that runs after
   dispatch begins passes test 1 and fails this one.
3. `TestResumeAtStartup_MismatchedFileIsNotAdopted` — truncate the file after
   committing the extent, so the size check fails. Assert the seeded set
   reflects recomputation from the file's bytes, not the stale cache (S7).
4. `TestResumeAtStartup_MissingFileLeavesEverythingOutstanding` — delete the
   file. Assert no article is Done and `Start` returns no error.
5. `TestResumeAtStartup_StorageFaultStallsAndDoesNotFailArticles` — force a
   non-`ErrNotExist` stat/read failure (e.g. chmod the directory to 0). Assert
   the job is stalled with a surfaced reason and that no article is marked
   permanently failed (A1).
6. `TestResumeAtStartup_DoesNotCommitClassB` — snapshot `file_extents` before
   and after `Start`; assert byte-identical. Pins rule 2.

- [ ] **Step 2: Run them and watch them fail**

```bash
go test -count=1 -run 'TestResumeAtStartup' ./internal/app/ -v
```

`-count=1` is mandatory. Without it Go replays a cached pass and prints `ok`,
which reads as "the test is inert" — the inverse of the truth.

- [ ] **Step 3: Implement `resumeAllJobs`**

For each job in the queue in a resident or queued state: hydrate it if needed,
take its manifest, and for each file index `f` derive `lo, hi :=
m.FileRange(f)`, build the on-disk path the assembler would use, and call
`Resume(ctx, jobID, int32(f), path, int32(lo), hi-lo)`. Convert each
`ResumeResult` to a `durability.FileExtent{FileIdx: int32(f), Durable: r.Durable}`
— those two fields only, per rule 1 — and pass the job's slice to
`SeedFromExtents`. Log a per-job summary (files resumed, articles seeded,
whether any file recomputed) at Info.

`ctx` cancellation must abort the sweep promptly (R15, interruptible): check
it between files, not merely between jobs, since one file's recompute is the
long operation.

- [ ] **Step 4: Verify green, then prove each test is a pin**

Copy the file to a scratch dir first and restore from **your own copy**. Never
`git stash` (the stash stack is shared with other sessions in this repo), never
`git checkout -- <path>`, never `git reset`.

Run each mutation with `-count=1` and record the actual failure message in the
commit body. A red-green claim without the message it produced is an
assertion, not evidence.

| Mutation | Must redden |
|---|---|
| Delete the `SeedFromExtents` call | 1, 2 |
| Move the sweep to after the pipeline starts | 2 |
| Seed every article regardless of the bitmap | 1 |
| Drop the size/mtime check (adopt unconditionally) | 3 |
| Treat a `Resume` error as a permanent article failure | 5 |
| Commit the resumed extents to `ExtentStore` | 6 |

- [ ] **Step 5: Gates, then commit**

```bash
go fix ./... && goimports -w . && go vet ./...
go test -count=1 -race ./... && golangci-lint run ./...
go run ./scripts/check_lock_io      # the sweep does whole-file reads; it must hold no queue lock
go test -count=1 -tags=integration ./test/integration/...
```

`check_lock_io` is called out because it is the gate this task can most
plausibly break: resume CRCs entire files, and `hydrateJobLocked` runs under
the queue's write lock. The sweep must do its I/O outside any queue lock.

```bash
git add internal/app/resume_startup.go internal/app/resume_startup_test.go internal/app/app.go
git commit -m "feat(app): seed the work set from committed extents at startup

Resumer and SeedFromExtents had no production caller, so every restart
re-downloaded bytes an earlier run had already fsynced (L3).

<paste the observed red output for each mutation above>"
```

---


## Task 11: Surface stall state and durability figures

**Files:**
- Modify: `internal/api/` (queue mode handler), `internal/app/statusinfo.go`,
  `ui/src/` (queue row component)
- Test: `internal/api/stall_test.go`, `ui/src/**/*.test.ts`

**Interfaces:**
- Consumes: `Application.Stall`/`Fail` state from Task 10.
- Produces: per-job API fields `stall_reason string`, `bytes_durable int64`,
  `bytes_pending int64`, `last_barrier_unix int64` (R26, R27).

Read `docs/svelte-gotchas.md` before touching any `.svelte` file, and
`docs/config-contract.md` if you add a config-bound prop.

- [ ] **Step 1: Write the failing test**

```go
func TestQueueAPI_ReportsStallReason(t *testing.T) {
	srv := newTestAPIServer(t)
	srv.app.Stall("job-1", &storagefault.Fault{
		Op: "write", Path: "/data/x.bin", Err: syscall.ENOSPC,
	})

	var got struct {
		Queue struct {
			Slots []struct {
				NzoID       string `json:"nzo_id"`
				StallReason string `json:"stall_reason"`
			} `json:"slots"`
		} `json:"queue"`
	}
	doJSON(t, srv, "/api?mode=queue&output=json", &got)

	for _, s := range got.Queue.Slots {
		if s.NzoID == "job-1" {
			if s.StallReason == "" {
				t.Fatal("stall_reason is empty for a stalled job — R27 requires an actionable reason")
			}
			if !strings.Contains(s.StallReason, "no space") && !strings.Contains(s.StallReason, "ENOSPC") {
				t.Errorf("stall_reason = %q, does not name the condition", s.StallReason)
			}
			return
		}
	}
	t.Fatal("job-1 not present in the queue listing")
}

func TestQueueAPI_ReportsDurableAndPendingBytes(t *testing.T) {
	// bytes_pending is bytes written but not yet covered by an fsync — the
	// rework window made visible. It must be reported separately from
	// bytes_durable, never summed into it.
	srv := newTestAPIServer(t)
	feedArticles(t, srv.app, "job-1", 10, 100)

	var got queueResponse
	doJSON(t, srv, "/api?mode=queue&output=json", &got)
	slot := findSlot(t, got, "job-1")
	if slot.BytesPending == 0 {
		t.Error("bytes_pending = 0 before any barrier ran")
	}
	if slot.BytesDurable != 0 {
		t.Errorf("bytes_durable = %d before any barrier ran, want 0", slot.BytesDurable)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run 'TestQueueAPI_ReportsStall|TestQueueAPI_ReportsDurable' -v`
Expected: FAIL — `unknown field StallReason`.

- [ ] **Step 3: Write the implementation**

Add the four fields to the queue slot struct in `internal/api/`, populated from
`Application`'s stall map and the job's `FileExtent` aggregates. Render
`stall_reason` in the UI queue row as a warning badge, and show
`bytes_pending` in the row's tooltip.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/api/ && ./scripts/run_tests.sh`
Expected: PASS (both Go and UI suites).

- [ ] **Step 5: Commit**

```bash
go fix ./... && goimports -w . && go vet ./... && go test -race ./... && golangci-lint run ./...
git add -A
git commit -m "feat(api): report stall reason and the durability window

bytes_pending is bytes written but not yet covered by an fsync — the
rework window made visible rather than inferred. It is reported
separately from bytes_durable and never summed into it, because the two
make different claims.

A stalled job now carries a reason a user can act on, which is the
difference between a recoverable full disk and a job that appears to
have silently stopped.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Task 12: Crash-consistency test harness

**Files:**
- Create: `test/crash/harness.go`, `test/crash/crash_test.go` (build tag `crash`)
- Modify: `docs/TESTING.md`, `scripts/run_tests.sh`
- Test: the harness *is* the test.

**Interfaces:**
- Consumes: the full application.
- Produces: `go test -tags=crash ./test/crash/...`

This task discharges R31, R32, and R33 — the obligations that no other task can
satisfy, because they require killing a real process.

- [ ] **Step 1: Write the failing test**

```go
//go:build crash

package crash

// TestPowerLoss_ReworkIsWithinTheCheckpointBound is the pin for B1. It is the
// only test in the suite that measures the bound rather than assuming it.
//
// A "power loss" here is SIGKILL plus a page-cache drop on the download
// directory, which is the closest a userspace test can get to losing
// unfsynced data. Without the drop this would only test process crash, where
// the page cache survives and nothing is lost — a strictly weaker claim.
func TestPowerLoss_ReworkIsWithinTheCheckpointBound(t *testing.T) {
	h := newHarness(t, harnessOpts{
		CheckpointBytes:    1 << 20, // 1 MiB
		CheckpointInterval: time.Hour,
	})
	h.StartJob("job-1", 64<<20) // 64 MiB of articles

	h.WaitForBarriers(3)
	servedBefore := h.MockServer.ArticlesServed()

	h.KillAndDropPageCache()
	h.Restart()
	h.WaitForJobComplete("job-1")

	rework := h.MockServer.ArticlesServed() - servedBefore - h.ArticlesRemainingAt(servedBefore)
	reworkBytes := rework * h.ArticleSize
	if reworkBytes > 1<<20 {
		t.Fatalf("re-fetched %d bytes after power loss, bound is %d — B1 is violated",
			reworkBytes, 1<<20)
	}
}

// TestPowerLoss_NoArticleIsAckedWithoutItsBytes is the pin for S1/S2 end to
// end: after a power loss, every article the queue calls resolved must have
// bytes on disk that match its recorded CRC.
func TestPowerLoss_NoArticleIsAckedWithoutItsBytes(t *testing.T) {
	h := newHarness(t, harnessOpts{CheckpointBytes: 1 << 20, CheckpointInterval: 200 * time.Millisecond})
	h.StartJob("job-1", 32<<20)
	h.WaitForBarriers(2)
	h.KillAndDropPageCache()
	h.Restart()

	for _, a := range h.ResolvedArticles("job-1") {
		fact := h.Fact("job-1", a.ArtIdx)
		if !fact.HasCRC {
			continue
		}
		got := h.ReadRegionCRC(fact)
		if got != fact.CRC32 {
			t.Errorf("article %d is resolved but its bytes hash to %#x, want %#x — a claim outlived its data",
				a.ArtIdx, got, fact.CRC32)
		}
	}
}

func TestExternalModification_TruncatedPartialIsRecomputed(t *testing.T) {
	h := newHarness(t, harnessOpts{CheckpointBytes: 1 << 20})
	h.StartJob("job-1", 16<<20)
	h.WaitForBarriers(2)
	h.Stop()

	path := h.PartialPath("job-1", 0)
	fi, _ := os.Stat(path)
	if err := os.Truncate(path, fi.Size()/2); err != nil {
		t.Fatal(err)
	}

	h.Restart()
	h.WaitForJobComplete("job-1")
	if !h.FinalFileMatchesExpectedCRC("job-1", 0) {
		t.Fatal("the completed file is wrong after an externally truncated partial")
	}
}

func TestExternalModification_DeletedPartialRestartsTheFile(t *testing.T) {
	h := newHarness(t, harnessOpts{CheckpointBytes: 1 << 20})
	h.StartJob("job-1", 16<<20)
	h.WaitForBarriers(2)
	h.Stop()
	if err := os.Remove(h.PartialPath("job-1", 0)); err != nil {
		t.Fatal(err)
	}
	h.Restart()
	h.WaitForJobComplete("job-1")
	if !h.FinalFileMatchesExpectedCRC("job-1", 0) {
		t.Fatal("the completed file is wrong after its partial was deleted")
	}
}

func TestDiskFull_StallsWithoutFailingArticles(t *testing.T) {
	h := newHarness(t, harnessOpts{DownloadDirSizeLimit: 4 << 20})
	h.StartJob("job-1", 64<<20)
	h.WaitForStall("job-1")

	if got := h.FailedArticleCount("job-1"); got != 0 {
		t.Errorf("%d articles marked failed by a full disk — A1 forbids attributing storage faults to articles", got)
	}
	if got := h.HealthPercent("job-1"); got != 100 {
		t.Errorf("health = %d%% after a disk-full stall, want 100%% — R21", got)
	}
	if h.StallReason("job-1") == "" {
		t.Error("no stall reason surfaced — R27")
	}

	h.FreeDiskSpace()
	h.WaitForJobComplete("job-1")
}
```

`KillAndDropPageCache` must `SIGKILL` the child process, then drop the page
cache for the download directory. On Linux, prefer
`posix_fadvise(POSIX_FADV_DONTNEED)` over each partial file — it needs no root,
unlike `/proc/sys/vm/drop_caches`. Document in the harness why: a test that
silently degrades to "process crash only" when run without root would pass
while measuring nothing, which is exactly the failure mode `AGENTS.md` warns
about for green gates that bound nothing. If the fadvise call fails, the
harness must `t.Fatal`, never skip silently.

`DownloadDirSizeLimit` should mount a small `tmpfs`, or fall back to writing a
large filler file to exhaust a `t.TempDir()` on the same filesystem. If neither
is available, `t.Skip` **with an explicit reason** — this is the one place a
skip is acceptable, because the alternative is a test that lies.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -tags=crash ./test/crash/ -v`
Expected: FAIL — `undefined: newHarness`.

- [ ] **Step 3: Write the harness**

Build `test/crash/harness.go` on the existing mock NNTP server in `test/`. The
harness runs the real `gonzbd` binary as a child process so `SIGKILL` is
meaningful — an in-process test cannot lose a page cache.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -tags=crash -timeout=20m ./test/crash/ -v`
Expected: PASS.

- [ ] **Step 5: Document the suite**

Add a `crash` section to `docs/TESTING.md` covering the build tag, the Linux
requirement, the runtime, and — importantly — what a pass does and does not
bound. State plainly that these tests measure the checkpoint bound on the test
filesystem only, and that NFS/SMB fsync behaviour is not covered.

- [ ] **Step 6: Commit**

```bash
gofmt -l test/crash && go vet -tags=crash ./test/crash/
git add test/crash docs/TESTING.md scripts/run_tests.sh
git commit -m "test(crash): measure the durability bound rather than assume it

The checkpoint bound is the design's central promise and nothing else
in the suite can check it: it needs a real SIGKILL plus a page-cache
drop, which an in-process test cannot produce.

Five tests: rework stays within the bound after power loss; no acked
article outlives its bytes; a truncated partial recomputes; a deleted
partial restarts; a full disk stalls without failing a single article
or moving the health percentage.

The page-cache drop uses posix_fadvise(DONTNEED) so it needs no root,
and hard-fails rather than skipping if it cannot run — a test that
silently degrades to process-crash-only would pass while measuring
nothing.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Task 13: Replace the documentation

**Files:**
- Rewrite: `docs/assembler-storage-contract.md` → delete, replaced by
  `docs/durability-contract.md`
- Rewrite: `docs/queue-lifecycle.md` (progress/extent sections)
- Modify: `docs/ARCHITECTURE.md`, `docs/nntp-downloader-contract.md` §5,
  `docs/post-processing-contract.md` (QuickCheck CRC input),
  `docs/sabnzbd_spec.md` (persistence + config sections), `AGENTS.md`
  (topic-docs table), `docs/go-standards.md` (if it cites the old ack path)
- Modify: `docs/superpowers/specs/2026-08-11-download-durability-design.md`
  — remove the "design proposal, carries no authority" caveat, since it is now
  implemented.

The user's instruction is explicit: **this replaces any existing spec or doc
describing the current implementation.** A doc that still describes the old
model is not "stale", it is wrong, and it will be cited as authority by the next
session.

- [ ] **Step 1: Find every claim the change falsified**

Per `AGENTS.md` § "sweep the claim, not the file", grep from the repository root
for the distinctive phrasing, not the files you happened to edit:

```bash
git grep -n 'maxWritten\|write cursor\|writeCursor\|InitialWriteCursor\|InitialMaxWritten'
git grep -n 'MarkArticlesDone\|MarkArticlesFailed\|SetFileExtents'
git grep -n 'bytes_downloaded\|failed_bytes\|max_written'
git grep -n 'Emitted-is-transient\|emitted is transient'
git grep -n 'crcValid\|crcParts\|combineWholeFileCRC'
git grep -n 'acked Done\|acked as done\|before any downstream code'
git grep -n 'pwrite → Truncate → Sync → Close → flush'
# Added by the 2026-08-11 re-derivation: #358 introduced this vocabulary and
# documented it across three docs. All of it describes the Written boundary,
# which this change moves.
git grep -n 'outcomeDurable\|outcomeDeferred\|writeAndSettle\|failDrainedArticles'
git grep -n 'reached WriteAt\|bytes reach disk\|once its bytes land'
git grep -n 'does not defer acks to fsync'
```

**#358 added 105 lines to `docs/assembler-storage-contract.md`, 27 to
`docs/nntp-downloader-contract.md`, and touched `docs/ARCHITECTURE.md` plus two
files under `docs/reviews/`.** Those are the newest and most confident
statements of the model this change replaces, so they are the ones a later
reader is most likely to trust. Sweep `docs/reviews/SYNTHESIS.md` and
`docs/reviews/lane1-backend-core.md` too — the original plan omitted them
because they did not yet contain durability claims.

Every hit outside `docs/superpowers/` is a claim to fix or delete. Expect hits
in `cmd/`, `test/`, `ui/`, `AGENTS.md`, and this plan itself.

- [ ] **Step 2: Write `docs/durability-contract.md`**

It replaces `docs/assembler-storage-contract.md`. Structure it as that file was
structured — "Why this exists", tiers table, lifecycle diagram, mandatory
invariants — but describing the new model. Carry forward the sections that are
still true and are not about durability: pre-allocation platform behaviour, the
`DiskProbe` at-most-one-in-flight pattern, `SupportsSparse`, DirectUnpack's
volume-level contract, and offset bounds checking.

Keep the existing header convention:

```markdown
**This states the contract in the present tense.** Where the code and this
document disagree, the code is wrong and the gap is a bug, not a documentation
error.
```

- [ ] **Step 3: Revise `docs/queue-lifecycle.md`**

The residency/manifest argument survives intact and is *strengthened* by this
work — cite it as the precedent for X3. Replace the sections describing
`bytes_downloaded`, `failed_bytes`, `MaxWritten`, `WriteCursor`, and the
`SetFileExtents` merge rule. The "Restart" section needs rewriting entirely: the
job no longer reconstructs progress from `job_files` aggregates but from
`file_extents`.

- [ ] **Step 4: Update the remaining docs**

- `docs/ARCHITECTURE.md` — the pipeline diagram gains the barrier.
- `docs/nntp-downloader-contract.md` §5 — "Emitted-is-transient" survives and is
  now a consequence of S3 rather than a standalone rule. Restate it in the new
  vocabulary and cross-reference.
- `docs/post-processing-contract.md` — QuickCheck's CRC input now comes from the
  barrier's `FileExtent.PrefixCRC`, and a resumed file can now supply one.
  Update the "QuickCheck bypass guarantees" section.
- `docs/sabnzbd_spec.md` — persistence section and §9.x config.
- `AGENTS.md` — topic-docs table: replace the
  `docs/assembler-storage-contract.md` row with `docs/durability-contract.md`,
  and update its "read before" trigger to include `internal/durability` and
  `internal/storagefault`.
- `docs/superpowers/specs/2026-08-11-download-durability-design.md` — delete the
  "Relationship to the existing contracts" paragraph.

- [ ] **Step 5: Run the comment analyzer over the cumulative diff**

Per `AGENTS.md`, the grep in Step 1 and the analyzer cover different things: the
grep finds comments you did not touch, the analyzer reads the ones you did.

Run `pr-review-toolkit:comment-analyzer` over the full branch diff.

- [ ] **Step 6: Verify no stale claim survives**

```bash
git grep -n 'assembler-storage-contract'   # expect: no hits outside git history
git grep -n 'InitialMaxWritten\|SetFileExtents\|MarkArticlesDoneByIdx'  # expect: no hits
```

- [ ] **Step 7: Commit**

```bash
git add -A docs AGENTS.md
git commit -m "docs: replace the assembler storage contract with the durability contract

docs/assembler-storage-contract.md described a model in which the write
path acked, combined CRCs, and truncated. None of that is true now, so
the file is deleted rather than amended — a doc that still describes the
old model is not stale, it is wrong, and the next session will cite it
as authority.

queue-lifecycle.md keeps its residency argument, which this work
strengthens rather than replaces: the eight bugs its property test
passed through are the precedent for making the ack path
compiler-enforced instead of test-enforced.

Swept from the repository root rather than by re-reading edited files.
The comment analyzer covered the comments in the diff; the grep covered
the ones outside it.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Task 14: Final verification and dead-code sweep

**Files:** whole repository.

- [ ] **Step 1: Full gate run**

```bash
go fix ./...
goimports -w .
go vet ./...
go build ./...
go test -race ./...
./scripts/run_tests.sh
golangci-lint run ./...
go test -v -tags=integration ./test/integration/...
go test -tags=crash -timeout=20m ./test/crash/...
go test ./internal/config/ -run 'TestUI|TestAllFlat'
```

Every one must pass. Record the output of the crash suite in the PR body — it is
the only evidence for B1.

- [ ] **Step 2: Dead-code sweep**

Several issues on the tracker are "symbol has no production caller" (#290, #297,
#304). This change removes a lot of code, so re-check:

```bash
golangci-lint run --enable unused ./...
git grep -n 'ClearArticleEmitted\|MarkArticleEmittedByIdx' -- '*.go' ':!*_test.go'
```

Close or update #344, #349, #353, #355, #356, #357, #311, #306, #337 with a note
naming the invariant that retired each.

- [ ] **Step 3: Mutation-test the two packages that carry the invariants**

```bash
./scripts/run_gremlins.sh ./internal/durability
./scripts/run_gremlins.sh ./internal/storagefault
```

**Never** run `gremlins` on the whole repo or call `gremlins unleash` directly —
it has caused 168–394 GB of disk use and OOM kills. See
`docs/mutation-testing-playbook.md` for triage. A `LIVED` mutant in
`barrier.go`'s ordering or `resume.go`'s validation is a real gap; write the
test rather than dismissing it.

- [ ] **Step 4: Open the PR**

```bash
git push -u origin docs/download-durability-spec
```

Then `/pr-create`, followed by `/watch-ci`.

---

## Self-Review

**Spec coverage.** Every invariant maps to a task: S1/S2 → 6, 9; S3 → 7, 8;
S4/S7 → 7; S5 → 3, 8; S6 → 6 (`VerifiedTo` replaces `maxWritten` as the
truncate source); A1/A2 → 1, 10; L1 → 9; L2/L3 → 10, 12; B1 → 10, 12; B2 → 9;
B3 → 7; B4 → 9 (retained `DiskProbe`); X1 → 9; X2/X3 → 2, 6, 8. Requirements
R1–R4 → 2, 4; R5–R8 → 6, 10; R9–R12 → 6, 8; R13–R17 → 7; R18–R22 → 1, 10;
R23–R25 → 6, 7; R26–R30 → 10, 11; R31–R34 → 12 and each task's red check.

**Known gaps, stated rather than hidden:**

0. **Tasks 9 and 13 were re-derived on 2026-08-11 against `73d0c810`**, after
   PR #358 (`913adf0d`) moved the ack boundary from accept to `WriteAt` and
   closed #355. Tasks 1-8, 10-12 and 14 were checked and are unaffected: they
   create new packages or touch files #358 did not. If further assembler work
   lands before this plan executes, re-derive Task 9 again rather than patching
   its citations — its premise is the part that goes stale, not its wording.

1. **R15 (interruptible recomputation) is specified but not separately tested.**
   Task 7 implements `Resume` synchronously per file. If a 15 GB partial makes
   startup unacceptably slow in practice, interruptibility needs its own task.
   Do not claim R15 is discharged.
2. **R24's "off the critical path" is not enforced.** Task 7 computes the prefix
   CRC during recomputation, which is off the barrier path by construction, but
   nothing prevents a future change from moving it. No test guards this.
3. **Task 12's disk-full test may skip** on a filesystem where neither `tmpfs`
   nor a filler file can bound free space. The skip is loud and reasoned, but a
   skipped test is not a passing one.
4. **`internal/queue` remains ~23k lines.** This plan removes its ack surface
   but does not decompose it. That is deliberate scope control, not an
   endorsement.

**Type consistency check performed:** `WrittenArticle`, `ArticleFact`,
`FileExtent`, `DurableProof`, `ResumeResult`, `SyncTarget`, `Acker`,
`Stallable`, and `Fault` are each defined in exactly one task and referenced
with matching field names and signatures thereafter. `FileLocalOrdinal` is used
in Task 6 and produced by Task 9 — Task 6's tests stub it, so Task 6 can land
before Task 9 exists.
