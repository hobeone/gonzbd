# Job Lifecycle Swap (Plan 2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

<!--
ArtifactMetadata:
  RequestFeedback: true
  UserFacing: true
-->

**Goal:** Move the daemon off `internal/queue` and onto `internal/job` + `internal/sched` + `internal/dispatch`, deleting `internal/queue` in the same change.

**Architecture:** `internal/job` becomes the single job record — identity, policy, lifecycle axes, **and** the `Manifest`/`JobProgress` content tiers that move into it. `internal/sched` decides (pools, leases, verdicts); `internal/dispatch` owns the registry, residency and the tick loop; `internal/app` supplies a `Residency` and a `Runner` and stops owning lifecycle transitions. `constants.Status` survives only as an API rendering, produced by `dispatch.Row.Status()` through `job.ToSABnzbd`.

**Tech Stack:** Go 1.27.0 (toolchain 1.27.0). Standard library only — no new dependencies. SQLite via `modernc.org/sqlite`. Table-driven tests with `t.Run` subtests.

**Spec:** `docs/superpowers/specs/2026-08-25-job-lifecycle-design.md` — §15 for the decomposition, and its amendment banners on §1.2, §5, §10.1 and §12.1–§12.3, which are load-bearing and were written **after** the body text they qualify. Read the banners before the sections they sit above.

---

## Global Constraints

- **Go 1.27.0** (toolchain 1.27.0). Module path `github.com/hobeone/gonzbd`.
- **No new external dependencies.** Adding one requires escalation per `AGENTS.md` § Decision Protocol.
- **No migration, no compatibility path.** Standing Design Rule 1: nothing here owes anything to state an earlier build wrote. No guards for old rows, no dual-read paths, no "old jobs behave differently".
- **One owner per piece of derived state.** Standing Design Rule 2. Adding a second constructor for a type, a second writer of a derived field, or a second enforcement point for one invariant requires escalation *before* it is written.
- **`ToSABnzbd` is the one place `constants.Status` may appear, and it is write-only.** Nothing reads a `constants.Status` back into the machine. `dispatch.Row.Status()` (Task 12) is a second door onto that one function, not a licence to relax this.
- **After editing any `.go` file:** `goimports -w <file>`, then `go fix ./...`, then `go build ./...`.
- **Quality gates before every commit:** `go vet ./...`, `go vet -tags=integration,uitest,crash ./...`, `go test -race ./...`, `golangci-lint run ./...`.
- **Whole-repo gates before the PR:** `go run ./scripts/check_dup_comments`, `go run ./scripts/check_review_banner`, `go run ./scripts/check_citations`.
- **Every red check is observed, not reasoned.** Use `go run ./scripts/mutate <spec>`; a mutation that is not KILLED is a test that does not pin. `-count=1` is not optional — see `AGENTS.md` § "Step 2 in practice".
- **Never `git stash`.** The stash stack is shared with other sessions.

---

## Why this is one plan and not five

§15 settles it and this plan does not reopen it:

> Plan 2 is large and deliberately so. It is the commit where the daemon stops running the old model, and splitting it would mean shipping exactly the adapters this decomposition exists to avoid.

The objection is to **shipping** adapters — landing a dual-path daemon on `main` and carrying it. It is not an objection to intermediate commits on one branch. Every task below ends green (`go build ./... && go test ./...`), because `AGENTS.md` requires each commit to leave the repository working, and because a mid-swap commit that does not build cannot have a red-green check run against it.

Two scaffolds exist **inside the branch only**, and each has a task that deletes it before the branch merges:

| Scaffold | Introduced | Deleted |
|---|---|---|
| `internal/queue` type aliases (`type Manifest = job.Manifest`) | Task 1 | Task 13 |
| `queueResidency` reading through the old `Queue` | Task 6 | Task 13 |

If either survives to the PR, the plan has failed its own constraint. Task 13 checks for them explicitly.

---

## The starting position, measured

Measured at `1a92c858`. **Read this before Task 1 — two of these facts contradict what the spec's older prose implies.**

### Nothing is wired

```
$ git grep -l '"github.com/hobeone/gonzbd/internal/job"' -- '*.go' | grep -vE 'internal/(job|sched|dispatch)/'
(no output)
```

`internal/job`, `internal/sched` and `internal/dispatch` are imported by **nothing** outside themselves. §15's status block says four of plan 2's deliverables "landed early", and they did — as *packages*. None of them is wired into the running daemon, and `dispatch.Residency` has no implementation anywhere in the tree. The daemon runs 100% on `internal/queue`.

This is the single biggest fact about the work: plan 2 is not "finish the wiring", it is "write the wiring, all of it".

### The blast radius

| | Count |
|---|---:|
| `internal/queue` non-test lines | 8,531 |
| exported `*Queue` methods | 55 |
| non-test files importing `internal/queue` | 17 |
| **test** files importing `internal/queue` | 90 |
| `constants.Status` sites outside `internal/queue`, non-test | 56 |

### The correction to D1 that this plan is built on

D1 (`#456`, and §15's status block) measured:

```
$ git grep -n 'q\.mu\|q\.store\|q\.dirty\|q\.jobs\|q\.byID\|\*Queue' -- internal/queue/manifest.go internal/queue/progress.go
(no output)
```

and concluded *"this is a move, not an untangle"*. **That measurement is correct and the conclusion does not follow from it.** It measures whether the moving files reach *out*. It does not measure whether the rest of the package reaches *in*, and it does:

```
$ git grep -cE '\b(markDone|markNotDone|markFailed|markEmitted|clearEmitted|resetForReload|recompute|setDownloadStartedOnce|setDownloadFinishedOnce|clearDownloadStamps|restoreDownloadStamps|derivedRemainingBytes|describesSameJobAs|isEarlyAbort|sizeFigures|newJobProgress)\b' \
    -- internal/queue/ ':!*_test.go' ':!internal/queue/progress.go'
internal/queue/queue.go:19
internal/queue/sqlite_store.go:17
internal/queue/job.go:15
internal/queue/job_articles.go:10
internal/queue/workset.go:10
internal/queue/persistence.go:6
internal/queue/store.go:4
internal/queue/snapshot.go:3
```

**84 production sites**, in seven files that are not moving, name an unexported member of `JobProgress`. (The identifier list is deliberately restricted to names that can only be `JobProgress`'s; a broader pattern such as `\.done\b` matches other types and inflates this.)

Those 84 sites do **not** need porting, because `internal/queue` is deleted — they die with it. What they establish is different and it is why Task 1 is three tasks rather than one:

> **The behaviours those 84 sites implement have to exist somewhere afterwards.** They are the article-accounting, persistence and reload paths. Moving the *type* without deciding where its *mutators* live would leave `internal/job` holding a `JobProgress` nothing can advance.

Task 2 is that decision, and it is why the mutators move onto `job.Job` rather than staying loose.

### The `Lease` does NOT carry the manifest, and the reserved slot saying it will is wrong

`internal/job/lease.go` reserves two fields:

```go
type Lease struct {
	id LeaseID
	// manifest *Manifest        // Half B2
	// barrier  *StorageBarrier  // Half B2
}
```

and argues for them: *"§6's argument is that the three share a lifetime and are therefore one object."* An independent plan for this work read that slot as an instruction and built its Task 1 around `NewLeaseWithManifest(id, m)`, with `Grant` installing the manifest and `Surrender` dropping it. **That is the wrong design, and this plan deliberately does not adopt it.** Two independent reasons, both measured:

**1. There is no manifest at grant time, and there cannot be.** `reconcileResidency`'s own doc comment (`internal/dispatch/tick.go:163-168`) states the ordering:

> grantFor runs inside Advance **under Queue.mu**, so a job acquires resources before this function can hydrate it, and **Hydrate does disk I/O that must not run under any lock**. A job therefore holds a lease with no manifest for the length of one read.

A constructor taking a manifest would have to load it before `Grant`, which means gzip-decoding a file under `Queue.mu`.

**2. Post-processing needs the manifest and holds no lease.** `needsLease` is true for `Fetching`, `Assessing` and `Repairing` only (`internal/sched/requirements.go`), and `Job.Grant` refuses past the boundary (`ErrLeaseAfterBoundary`). Post-processing runs at `Extracting`/`Finalizing`, and reads the manifest at four sites:

```
$ grep -n 'Manifest()' internal/postproc/*.go | grep -v _test
internal/postproc/stage_quickcheck.go:136
internal/postproc/stage_quickcheck.go:180
internal/postproc/filelist.go:30
internal/postproc/filelist.go:244
```

A lease-gated manifest is unreadable in exactly the stage that needs it most — and `stage_quickcheck.go:180` is the site whose distinguishable error stops an unverified job being reported CRC-clean (#294). Gating on the lease would turn that into "not resident" for every job.

**Residency is keyed on `v.Holds`, not on lease-holding**, and `reconcileResidency` already implements it that way. `holds()` means "has every resource this position requires", which is true at `Extracting` with a compute slot and no lease. That is why `Residency` is an interface with `Hydrate(ctx, id)`/`Evict(id)` rather than a lease field.

**This is §10.1's shape a second time.** That banner refuted the `barrier` half of this same reserved slot — the barrier is process-level, not per-lease. The evidence above refutes the `manifest` half by the same route: a spec intention that the implementation has since ruled against, with nothing written back. **Task 2 Step 6 corrects the comment**, so the next reader of `lease.go` does not inherit the instruction a second time.

---

## File Structure

### Created

| File | Responsibility |
|---|---|
| `internal/job/manifest.go` | `Manifest` — moved verbatim from `internal/queue/manifest.go`, package clause changed |
| `internal/job/progress.go` | `JobProgress`, `FileProgress`, `FetchPolicy` — moved from `internal/queue/progress.go` |
| `internal/job/bitset.go` | `bitset` — moved from `internal/queue/bitset.go` |
| `internal/job/repair.go` | `RepairState` — moved from `internal/queue/repair.go` |
| `internal/job/content.go` | The `Job` methods that own the moved state: residency swap, article accounting, the mutators the 84 sites used to reach |
| `internal/job/checkpoint.go` | `Snapshot`-shaped `Checkpoint` value the `Checkpointer` writes |
| `internal/app/residency.go` | `appResidency` — the production `dispatch.Residency`: gzip manifest load/evict |
| `internal/app/runner.go` | `appRunner` — the production `dispatch.Runner`: routes a state to the downloader or the post-processor |
| `internal/checkpoint/checkpointer.go` | The sole DB writer for job state; batches `job.Checkpoint` values |
| `internal/dispatch/status.go` | `Row.Status()` — the legacy-status accessor (§12.1) |

### Modified

| File | Change |
|---|---|
| `internal/dispatch/registry.go:232` | add `Row(id)`, `Job(id)`, `PauseJob(id)`, `ResumeJob(id)`, `Remove(ctx,id)` beside `List()` |
| `internal/job/lease.go:31-41` | the reserved `manifest`/`barrier` slot — both refuted; Task 2 Step 6b |
| `internal/dispatch/ports.go:9-18` | `Residency` doc: the manifest-in-`internal/queue` claim is falsified by Task 1 |
| `internal/sched/doc.go:4-5` | "depends on `internal/job` and nothing else" — falsified by Task 1's `nzb` edge |
| `internal/app/app.go` | construct `Dispatcher`, `Residency`, `Runner`, `Checkpointer`; drop `*queue.Queue` |
| `internal/api/queue.go`, `roles.go`, `server.go` | read `dispatch.Row`; render via `Row.Status()` |
| `internal/downloader/dispatch.go`, `downloader.go` | article loop reads `*job.Job` |
| `internal/postproc/stages.go`, `filelist.go`, `stage_quickcheck.go` | `job.Queue` → the rehomed record |
| `cmd/gonzbd/main.go` | construction wiring |

### Deleted (Task 13)

`internal/queue/` in full — all 16 non-test files and their tests. Plus, per §15's corrected Deletes column: `queue/status.go`, `JobPhase`, `ActiveSet`, `PromoteNext`, `evictJobLocked`, `SetStatus`/`SetStatusIf`, `SetPostProcStarted`, `Queue.Retry`, `maybeReleaseRecoveryVolumes`, `shouldSkipForPP`, `Job.PostProc`.

**Not deleted**, and this reverses older prose in the same document — see §1.2's second amendment: `par2NeedsRecovery` (already gone, #494/#495), `NeedRequeue`/`RequeueBlocksNeeded` (already gone, #507), **the `quickcheck` stage** (retained permanently), `resumeAllJobs` (§10.1 — it is the resume mechanism).

---

## Task Dependency Order

```
  content tier        surfaces          ports + wiring        consumers        cutover
 ┌───────────┐   ┌───────────────┐   ┌────────────────┐   ┌────────────┐   ┌─────────┐
 │  1 ── 2   │──▶│   3 ── 4      │──▶│  5 ── 6 ── 7 ──│──▶│  9 10 11 12│──▶│ 13 ── 14│
 │           │   │               │   │        └── 8   │   │            │   │         │
 └───────────┘   └───────────────┘   └────────────────┘   └────────────┘   └─────────┘
   move +          read surface        Checkpointer,        one package      delete +
   owner           + control doors     Residency, Runner,   at a time        sweep
                                       construction
```

| | |
|---|---|
| **1–2** | Move `Manifest`/`JobProgress` into `internal/job` and give them an owner. Additive; touches no consumer. |
| **3–4** | The surfaces consumers will read and drive: `Row.Status()`, `Dispatcher.Row(id)`, and the per-job control doors. **Before** the consumers, because Tasks 9–12 cannot be written against methods that do not exist. |
| **5–8** | `Checkpointer`, `Residency`, `Runner`, then construction in `app.New` — the one commit where both models are live. |
| **9–12** | Repoint `downloader` → `postproc` → `app` → `api`+`cmd`, one package per commit. |
| **13–14** | Delete `internal/queue` and both scaffolds; sweep the falsified prose. |

> **Ordering note.** An earlier draft of this plan put `Row.Status()` at Task 12 while stating that the API repoint depended on it — an inversion that would have stalled the largest consumer task on a method scheduled after it. Tasks 3 and 4 exist at this position for that reason, and Task 4's control doors were missing from that draft entirely.

---

### Task 1: Move the content tier into `internal/job`

**Files:**
- Create: `internal/job/manifest.go`, `internal/job/progress.go`, `internal/job/bitset.go`, `internal/job/repair.go`
- Modify: `internal/queue/manifest.go`, `internal/queue/progress.go`, `internal/queue/bitset.go`, `internal/queue/repair.go` — reduced to alias files
- Modify: `internal/sched/doc.go:4-5`
- Modify: `internal/dispatch/ports.go:9-18`
- Test: `internal/job/manifest_test.go`, `internal/job/progress_test.go` (moved), `internal/job/import_edge_test.go` (new)

**Interfaces:**
- Consumes: nothing — first task.
- Produces: `job.Manifest`, `job.JobProgress`, `job.FileProgress`, `job.FetchPolicy`, `job.RepairState`, all with the exact exported method sets they have today in `internal/queue` (see `git grep -n '^func (m \*Manifest)\|^func (p \*JobProgress)' internal/queue/`). Unexported members keep their names.

- [ ] **Step 1: Move the four files with git, preserving history**

```bash
git mv internal/queue/manifest.go   internal/job/manifest.go
git mv internal/queue/progress.go   internal/job/progress.go
git mv internal/queue/bitset.go     internal/job/bitset.go
git mv internal/queue/repair.go     internal/job/repair.go
git mv internal/queue/manifest_test.go internal/job/manifest_test.go
git mv internal/queue/progress_test.go internal/job/progress_test.go
git mv internal/queue/bitset_test.go   internal/job/bitset_test.go
sed -i 's/^package queue$/package job/' internal/job/manifest.go internal/job/progress.go internal/job/bitset.go internal/job/repair.go
sed -i 's/^package queue$/package job/' internal/job/manifest_test.go internal/job/progress_test.go internal/job/bitset_test.go
```

- [ ] **Step 1b: Move the two types the moved files' constructor requires**

`newManifest` takes `[]JobFile`, and `JobFile`/`JobArticle` are declared in `internal/queue/job.go:740` and `:754` — a file that is **not** in the moving set. Without them the moved `manifest.go` does not compile. D1 anticipated this (*"`FileProgress` and `JobFile`/`JobArticle` live in `progress.go` and `job.go` respectively and split with them"*); the file list above does not, which is why this is its own step.

Cut both type declarations, and only those, from `internal/queue/job.go` into `internal/job/manifest.go`. Then add them to Task 1 Step 4's alias block:

```go
	JobFile    = job.JobFile
	JobArticle = job.JobArticle
```

- [ ] **Step 1c: Move the `Job` methods #458 relocated out of `Queue`**

#458 moved 19 method bodies from `Queue` onto `queue.Job`, and they are the mutators of the state that is moving — `markFileComplete` is one, and it is a `*Job` method (`queue.go`'s `MarkFileComplete` calls `job.markFileComplete(fileIdx)`), **not** a `*JobProgress` method. Enumerate them before moving, rather than trusting this count:

```bash
git grep -n '^func (j \*Job) [a-z]' -- internal/queue/job.go
```

Every one whose body touches only `j.manifest`, `j.progress` or the manifest-derived scalars moves to `internal/job/content.go` in Task 2. Any that touches `Queue` state stays and dies with the package. **Record the split in the commit body** — this enumeration is the input to Task 2 and a wrong one is not visible later.

- [ ] **Step 2: Fix the one cross-package reference the move creates**

`internal/queue/manifest.go:309` calls `nzb.MessageIDIsFetchable`. It is pure and stdlib-only (`internal/nzb/parser.go:380`), and `internal/nzb` imports only `internal/fsutil`, so there is no cycle. Add the import to the moved file:

```go
import (
	"github.com/hobeone/gonzbd/internal/nzb"
)
```

- [ ] **Step 3: Run the build and observe it fail in `internal/queue`, not `internal/job`**

Run: `go build ./internal/job/ && go build ./internal/queue/`
Expected: `internal/job` builds. `internal/queue` fails with ~84 `undefined:` errors naming `newJobProgress`, `markDone`, `markFailed` and friends. That failure is the 84-site measurement reproducing itself, and it is what Task 2 resolves. Do not fix it here.

- [ ] **Step 4: Add the branch-local alias file so `internal/queue` compiles again**

Create `internal/queue/aliases.go`:

```go
package queue

// DELETE ME IN TASK 13. This file is a branch-local scaffold, not a shipped
// adapter: it exists so every commit between Task 1 and Task 13 builds, which
// AGENTS.md requires and which a red-green check needs in order to run at all.
// docs/superpowers/specs/2026-08-25-job-lifecycle-design.md §15 forbids
// shipping adapters; it does not forbid an intermediate commit. Task 13 deletes
// this file and fails if it is still present.

import "github.com/hobeone/gonzbd/internal/job"

type (
	Manifest    = job.Manifest
	JobProgress = job.JobProgress
	FileProgress = job.FileProgress
	FetchPolicy = job.FetchPolicy
	RepairState = job.RepairState
)

const (
	FetchAlways    = job.FetchAlways
	FetchIfNeeded  = job.FetchIfNeeded
	FetchNever     = job.FetchNever
)
```

> Aliases carry exported members only. The 84 unexported sites still fail, by design — Task 2 owns them.

- [ ] **Step 5: Sweep the two doc claims this task falsifies**

`internal/sched/doc.go:4-5` states its dependency set exactly and is now wrong. `internal/dispatch/ports.go:11-12` says *"`Manifest` lives in `internal/queue` until B2.4, and the dispatcher never reads its contents"* — the first clause is false as of this task, the second is still true and is now enforced by a test rather than by an import ban.

```go
// ports.go — replace lines 9-13 of the Residency doc comment:

// Residency is how the dispatcher makes a job's manifest available and takes
// it away again. It names no manifest type on purpose. That used to be an
// import consequence — dispatch could not name queue.Manifest because it may
// not import internal/queue — and since the content tier moved into
// internal/job (which dispatch DOES import) it is a discipline instead,
// enforced by TestDispatchNamesNoManifestType in this package.
```

- [ ] **Step 6: Write the enumeration test that replaces the compiler guarantee**

`docs/queue-lifecycle.md` values the residency boundary being compiler-enforced. It stops being so here, and §15's status block makes the replacement a plan 2 deliverable rather than a follow-up. Create `internal/dispatch/manifest_boundary_test.go`:

```go
package dispatch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestDispatchNamesNoManifestType is the residency boundary, demoted from a
// compiler guarantee to a test one when Manifest moved into internal/job (Task
// 1). The dispatcher decides WHEN a job is resident and delegates WHAT to load;
// naming the manifest type here is how that separation gets lost.
//
// It parses this package's own source rather than grepping, so a reference in a
// comment or a string does not trip it and a reference in a type does.
func TestDispatchNamesNoManifestType(t *testing.T) {
	const banned = "Manifest"

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	for _, pkg := range pkgs {
		for name, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				x, ok := sel.X.(*ast.Ident)
				if !ok || x.Name != "job" {
					return true
				}
				if strings.Contains(sel.Sel.Name, banned) {
					t.Errorf("%s: internal/dispatch names job.%s; the dispatcher "+
						"must delegate WHAT to load to Residency and never read "+
						"manifest contents", name, sel.Sel.Name)
				}
				return true
			})
		}
	}
}
```

- [ ] **Step 7: Verify the test fails when the boundary is violated**

Create `internal/dispatch/testdata/manifest_boundary.spec`:

```text
pkg ./internal/dispatch/
run TestDispatchNamesNoManifestType

[a manifest reference is introduced]
file internal/dispatch/tick.go
--- anchor
func (d *Dispatcher) reconcileResidency(ctx context.Context, j *job.Job) error {
--- replace
func (d *Dispatcher) reconcileResidency(ctx context.Context, j *job.Job) error {
	var _ *job.Manifest
--- end
```

Run: `go run ./scripts/mutate internal/dispatch/testdata/manifest_boundary.spec`
Expected: `KILLED`. If it SURVIVES, the test does not discriminate and Step 6 must be fixed before proceeding.

- [ ] **Step 8: Commit**

```bash
goimports -w internal/job/ internal/queue/ internal/dispatch/ internal/sched/
go build ./internal/job/ ./internal/dispatch/ ./internal/sched/
git add internal/job/ internal/queue/ internal/dispatch/ internal/sched/
git commit -m "refactor(job): move the manifest and progress tiers into internal/job

D1 settled internal/job over internal/jobstate: one job, one record, one
lock, one owner. manifest.go, progress.go, bitset.go and repair.go move
verbatim; the one nzb.MessageIDIsFetchable call is the package's first
dependency beyond internal/constants and is pure and cycle-free.

The residency boundary stops being compiler-enforced by the import ban
and becomes TestDispatchNamesNoManifestType, per the spec's D1 mitigation.
Red check: internal/dispatch/testdata/manifest_boundary.spec, 1/1 KILLED.

internal/queue/aliases.go is a branch-local scaffold that Task 13 deletes.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Give the moved state its owner on `job.Job`

**Files:**
- Create: `internal/job/content.go`
- Modify: `internal/job/job.go` — add the `manifest`/`progress` pair and its lock
- Test: `internal/job/content_test.go`

**Interfaces:**
- Consumes: `job.Manifest`, `job.JobProgress` (Task 1).
- Produces:
  ```go
  func (j *Job) AttachContent(m *Manifest) error     // sole constructor of the pair
  func (j *Job) Manifest() (*Manifest, error)
  func (j *Job) Progress() *JobProgress
  func (j *Job) Evict()
  func (j *Job) Resident() bool
  func (j *Job) MarkArticleDone(artIdx int, bytes int64, server string) error
  func (j *Job) MarkArticleFailed(artIdx int) error
  func (j *Job) MarkArticleEmitted(artIdx int) error
  func (j *Job) ClearArticleEmitted(artIdx int) error
  func (j *Job) MarkFileComplete(fileIdx int) error
  ```

This is the task the 84-site measurement exists to justify. `JobProgress`'s mutators are unexported and were reached directly from seven files in `internal/queue`. With that package deleted, they need exactly one door, and Rule 2 says the door is an owner rather than a set of call sites.

- [ ] **Step 1: Write the failing test for the single-constructor property**

```go
package job

import "testing"

// TestAttachContent_IsTheSoleConstructorOfThePair pins Rule 2's owner model
// for the content tier: nothing may produce a (Manifest, JobProgress) pair
// except AttachContent, so the two can never describe different jobs.
//
// newManifest and Manifest.UnmarshalJSON populating the same fields by two
// paths is the defect this shape exists to prevent — they had already
// diverged over totalBytes (see the spec's Rule 2, "Two constructors for one
// type").
func TestAttachContent_IsTheSoleConstructorOfThePair(t *testing.T) {
	j := New("abc", "test", PolicyFromPP(3))

	if j.Resident() {
		t.Fatal("a fresh Job must not be resident")
	}
	if _, err := j.Manifest(); err == nil {
		t.Fatal("Manifest() on a non-resident job must error, not return nil,nil")
	}

	m := newManifest([]JobFile{{Subject: "a.rar", Bytes: 100, Articles: []JobArticle{{ID: "<1@x>", Bytes: 100}}}})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}

	if !j.Resident() {
		t.Fatal("Resident() must be true after AttachContent")
	}
	got, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest after attach: %v", err)
	}
	if got.NumArticles() != 1 {
		t.Fatalf("NumArticles = %d, want 1", got.NumArticles())
	}
	if j.Progress() == nil {
		t.Fatal("Progress() must be non-nil once content is attached")
	}
	if j.Progress().PendingArticles() != 1 {
		t.Fatalf("PendingArticles = %d, want 1", j.Progress().PendingArticles())
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test -count=1 -run TestAttachContent_IsTheSoleConstructorOfThePair ./internal/job/`
Expected: FAIL — `j.Resident undefined`, `j.AttachContent undefined`.

- [ ] **Step 3: Add the pair and its lock to `Job`**

In `internal/job/job.go`, inside `type Job struct`:

```go
	// contentMu guards the manifest/progress POINTER PAIR, not their contents.
	// Eviction and hydration swap both pointers together; a reader holding a
	// *Job but not this lock would race the swap (the defect #263 records
	// against internal/queue's residencyMu, which this replaces).
	//
	// It is a value, not a pointer, so every construction path gets a usable
	// mutex with no initializer to forget.
	contentMu sync.RWMutex
	manifest  *Manifest
	progress  *JobProgress
```

- [ ] **Step 4: Write `internal/job/content.go`**

```go
package job

import (
	"errors"
	"fmt"
)

// ErrNotResident is returned by Manifest when the job's content tier is not
// loaded. It is distinct from a hydration failure on purpose: "evicted" is
// routine and "unreadable on disk" is data loss, and internal/queue's
// hydrateErr existed because those two had been indistinguishable.
var ErrNotResident = errors.New("job: content tier not resident")

// AttachContent installs the manifest and derives the progress record from it.
//
// It is the SOLE constructor of the (manifest, progress) pair. Progress is
// derived here and never supplied by a caller, which is what makes
// "progress describes this manifest" an invariant rather than a comment: there
// is no second path that could pair a manifest with progress for another job.
func (j *Job) AttachContent(m *Manifest) error {
	if m == nil {
		return fmt.Errorf("job %s: AttachContent: nil manifest", j.id)
	}
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	j.manifest = m
	j.progress = newJobProgress(m)
	return nil
}

// RestoreContent installs a manifest together with progress recovered from the
// store. It is AttachContent's counterpart for a job that has run before, and
// it is the only other writer of the pair.
//
// It verifies the two describe the same job rather than trusting the caller:
// describesSameJobAs compares article and file counts, and a mismatch here
// means the stored progress belongs to a different manifest revision.
func (j *Job) RestoreContent(m *Manifest, p *JobProgress) error {
	if m == nil || p == nil {
		return fmt.Errorf("job %s: RestoreContent: nil manifest or progress", j.id)
	}
	if !p.describesSameJobAs(m) {
		return fmt.Errorf("job %s: RestoreContent: progress describes a different manifest", j.id)
	}
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	j.manifest = m
	j.progress = p
	return nil
}

// Evict drops the manifest and keeps the progress record.
//
// Progress is always resident by design (docs/queue-lifecycle.md's three
// tiers): it is small, and the abort checks and the queue listing read it for
// jobs that are not running. Only the manifest, which is sized by article
// count, is evictable.
func (j *Job) Evict() {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	j.manifest = nil
}

// Resident reports whether the manifest is loaded.
func (j *Job) Resident() bool {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	return j.manifest != nil
}

// Manifest returns the resident manifest, or ErrNotResident.
func (j *Job) Manifest() (*Manifest, error) {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.manifest == nil {
		return nil, fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	return j.manifest, nil
}

// Progress returns the always-resident progress record, or nil before any
// content has been attached.
func (j *Job) Progress() *JobProgress {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	return j.progress
}

// withProgress runs fn under the write lock with the progress record, and is
// the ONLY way the mutators below reach it. Every article-accounting mutation
// in this package goes through here, which is what replaces the 84 direct
// reach-ins internal/queue had into JobProgress's unexported surface.
func (j *Job) withProgress(fn func(*JobProgress) error) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	return fn(j.progress)
}

// MarkArticleDone records a successfully downloaded article.
func (j *Job) MarkArticleDone(artIdx int, bytes int64, server string) error {
	return j.withProgress(func(p *JobProgress) error {
		return p.markDone(artIdx, bytes, server)
	})
}

// MarkArticleFailed records an article that will not be retried.
func (j *Job) MarkArticleFailed(artIdx int) error {
	return j.withProgress(func(p *JobProgress) error { return p.markFailed(artIdx) })
}

// MarkArticleEmitted records that an article has been handed to the downloader.
func (j *Job) MarkArticleEmitted(artIdx int) error {
	return j.withProgress(func(p *JobProgress) error { return p.markEmitted(artIdx) })
}

// ClearArticleEmitted undoes MarkArticleEmitted for a work item that was never
// dispatched.
func (j *Job) ClearArticleEmitted(artIdx int) error {
	return j.withProgress(func(p *JobProgress) error { return p.clearEmitted(artIdx) })
}

// MarkFileComplete records that a file's articles have all been assembled.
//
// It delegates to the unexported markFileComplete that Step 1c moved here from
// internal/queue/job.go -- a *Job method, not a *JobProgress one, because #458
// relocated it onto Job rather than into the progress record. Do not look for
// a JobProgress.markFileComplete; there isn't one.
func (j *Job) MarkFileComplete(fileIdx int) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	return j.markFileComplete(fileIdx)
}
```

- [ ] **Step 5: Run the test and watch it pass**

Run: `go test -count=1 -run TestAttachContent ./internal/job/`
Expected: PASS.

- [ ] **Step 6: Pin the owner property with a mutation**

Create `internal/job/testdata/content_owner.spec`:

```text
pkg ./internal/job/
run TestAttachContent_IsTheSoleConstructorOfThePair

[AttachContent stops deriving progress and leaves it nil]
file internal/job/content.go
--- anchor
	j.progress = newJobProgress(m)
--- replace
	j.progress = nil
--- end

[Manifest returns nil,nil instead of ErrNotResident]
file internal/job/content.go
--- anchor
		return nil, fmt.Errorf("job %s: %w", j.id, ErrNotResident)
--- replace
		return nil, nil
--- end
```

Run: `go run ./scripts/mutate internal/job/testdata/content_owner.spec`
Expected: both KILLED. Record the observed failure output in the commit body.

- [ ] **Step 6b: Correct `lease.go`'s reserved-slot comment**

The comment promising `manifest` and `barrier` fields is now wrong on both counts — see "The `Lease` does NOT carry the manifest" above — and it is written as an instruction to a future implementer, which is how it nearly became this plan's Task 1. Replace the field block and the paragraph above it:

```go
// A lease is admission to the correctness loop, and its id is the whole of it.
//
// This type reserved `manifest *Manifest` and `barrier *StorageBarrier` fields
// for "Half B2", on §6's argument that the three share a lifetime. Neither
// arrived, and neither should:
//
//   - The BARRIER is process-level, not per-lease. §10.1's banner in
//     2026-08-25-job-lifecycle-design.md records the refutation: one Barrier is
//     built in app.New with a cross-job overlapKey map, and reconciling
//     per-lease would destroy durable records for jobs in post-processing.
//
//   - The MANIFEST is keyed on holding what a position requires, not on
//     holding a lease. grantFor runs under Queue.mu and Hydrate does disk I/O,
//     so there is no manifest to install at grant time
//     (internal/dispatch/tick.go, reconcileResidency). And post-processing
//     reads the manifest at Extracting/Finalizing, where needsLease is false
//     (internal/sched/requirements.go) — four sites, `grep -n 'Manifest()'
//     internal/postproc/*.go | grep -v _test` — so a lease-gated manifest
//     would be unreadable exactly where the stage that verifies CRCs needs it.
//
// Residency lives on job.Job and is driven by dispatch.Residency against
// RenderView.Holds. Do not reintroduce either field here.
type Lease struct {
	id LeaseID
}
```

> This is `AGENTS.md` § "sweep the claim, not the file" applied to a comment that has already misled one plan. A stale claim that reads as an instruction costs more than one that merely describes.

- [ ] **Step 7: Commit**

```bash
goimports -w internal/job/ && go build ./internal/job/
git add internal/job/
git commit -m "feat(job): give the moved content tier a single owner

84 production sites in internal/queue reached directly into JobProgress's
unexported surface. With that package deleted they need one door, and Rule
2 says the door is an owner rather than a check at each call site.

AttachContent is the sole constructor of the (manifest, progress) pair and
derives progress rather than accepting it, so 'progress describes this
manifest' is an invariant the type enforces. RestoreContent is the only
other writer and verifies describesSameJobAs before installing.

Red check: internal/job/testdata/content_owner.spec, 2/2 KILLED.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---
### Task 3: `Row.Status()` and `Dispatcher.Row(id)` — the read surface

**Files:**
- Create: `internal/dispatch/status.go`
- Test: `internal/dispatch/status_test.go`

**Interfaces:**
- Consumes: `job.ToSABnzbd(v RenderView)` (`internal/job/sabnzbd.go:45`), `dispatch.Row.View`.
- Produces: `func (r Row) Status() constants.Status`.

§12.1 settles the receiver and the reason. **The accessor cannot go on `job.Job`**: `ToSABnzbd` needs a `RenderView`, whose `Running`, `Reason` and `Holds` are `sched`-owned — *"Nothing in this package can answer that"* (`render.go:20`). `internal/sched` imports `internal/job` and not the reverse, so a method on `Job` needs a back-pointer that inverts the dependency into a cycle. `Row` already carries the `RenderView`.

- [ ] **Step 1: Write the failing test**

```go
// TestRowStatus_MatchesToSABnzbd pins that the accessor is a door onto
// ToSABnzbd rather than a second translation. §12 makes ToSABnzbd the one
// place constants.Status may appear; a Row.Status that computed anything
// itself would be a second enforcement point for that rule (Rule 2).
func TestRowStatus_MatchesToSABnzbd(t *testing.T) {
	for _, st := range job.AllStates() {
		t.Run(st.String(), func(t *testing.T) {
			v := job.RenderView{StateView: job.StateView{State: st}, Running: true}
			r := Row{ID: "a", View: v}
			if got, want := r.Status(), job.ToSABnzbd(v); got != want {
				t.Fatalf("Row.Status() = %q, ToSABnzbd = %q; they must not diverge", got, want)
			}
		})
	}
}
```

- [ ] **Step 2: Run and watch it fail**

Run: `go test -count=1 -run TestRowStatus ./internal/dispatch/`
Expected: FAIL — `r.Status undefined`.

- [ ] **Step 3: Implement it**

```go
package dispatch

import (
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/job"
)

// Status renders this row's state in the legacy SABnzbd vocabulary.
//
// It lives here rather than on job.Job because ToSABnzbd needs a RenderView,
// and three of that view's fields -- Running, Reason and Holds -- are facts
// only internal/sched can supply (job/render.go: "Nothing in this package can
// answer that"). internal/sched imports internal/job and not the reverse, so a
// Status() on Job would need a back-pointer that inverts the dependency into a
// cycle. Row already carries the view.
//
// This is a door onto ToSABnzbd, NOT a second translation: §12 makes that
// function the one place constants.Status may appear, and it is write-only.
// Use this to RENDER a status. Do not branch on the result -- branching is
// what the State/Outcome/Intent axes are for, and reading status back into the
// machine is what the swap exists to end.
func (r Row) Status() constants.Status { return job.ToSABnzbd(r.View) }
```

- [ ] **Step 4: Run and watch it pass**

Run: `go test -count=1 -run TestRowStatus ./internal/dispatch/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
goimports -w internal/dispatch/ && go build ./...
git add internal/dispatch/status.go internal/dispatch/status_test.go
git commit -m "feat(dispatch): add Row.Status(), the legacy-status accessor

Per §12.1. The receiver is Row rather than job.Job because ToSABnzbd
takes a RenderView whose Running/Reason/Holds are sched-owned, and
internal/job may not import internal/sched without a cycle.

Scoped to rendering. §12.2 measured the population it serves: 56 non-test
constants.Status sites, not the 326 an earlier figure claimed -- that
grep matched directunpack.Status, par2's parse status and history entry
status too. 56 is a hand-triage, which is what makes 'render, don't
branch' enforceable rather than aspirational.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---


---

#### Task 3b: `Dispatcher.Row(id)` — the header-tier single-job lookup

**Files:**
- Modify: `internal/dispatch/registry.go` (add beside `List` at `:232`)
- Test: `internal/dispatch/registry_test.go`

**Interfaces:**
- Consumes: `sched.Queue.Render(j)` (`internal/sched/render.go:39`).
- Produces: `func (d *Dispatcher) Row(id string) (Row, bool)`.

This is #436's ask. §12.3 records that it is a deliverable rather than an automatic consequence: `List()` renders every job through `RenderAll`, so using it for a single-job lookup trades a manifest read for an O(n) walk.

**Do the triage first.** #436's body: *"First task is the triage, not the API"* — `git grep -c 'SnapshotJob(' -- '*.go' ':!*_test.go' ':!internal/queue/*'` returns **17** production call sites, of which three are triaged. Classify each by the heaviest tier it actually reads before settling the shape. Whether this is `Row(id)` alone or also needs a name accessor is that triage's output, and it is a Decision-Protocol call if it turns out to want more than one method.

- [ ] **Step 1: Triage the 17 call sites and record the table in the PR body**

```bash
git grep -n 'SnapshotJob(' -- '*.go' ':!*_test.go' ':!internal/queue/*'
```

For each: does it read a header field, a progress field, or a manifest field? Three are already known — `app/stall.go` (existence only), `app/app.go` `RemoveJob` (`.Name` only), `api/queue.go` (`.Name` for a log line). If more than ~2 read manifest fields, stop and escalate: the shape is then not a header-tier row.

- [ ] **Step 2: Write the failing test**

```go
// TestDispatcherRow_ReturnsOneJobWithoutRenderingTheRest is #436: a header-tier
// caller must not pay manifest-tier cost. It asserts the cheap path exists and
// agrees with List, so the two cannot drift.
func TestDispatcherRow_ReturnsOneJobWithoutRenderingTheRest(t *testing.T) {
	d := newTestDispatcher(t)
	addTestJob(t, d, "a", "Job A")
	addTestJob(t, d, "b", "Job B")

	row, ok := d.Row("b")
	if !ok {
		t.Fatal("Row(b) must find the job")
	}
	if row.ID != "b" || row.Header.Name != "Job B" {
		t.Fatalf("Row(b) = %+v, want ID b / Name Job B", row)
	}

	var want Row
	for _, r := range d.List() {
		if r.ID == "b" {
			want = r
		}
	}
	if row.View != want.View {
		t.Fatalf("Row and List disagree for b:\n Row = %+v\nList = %+v", row.View, want.View)
	}

	if _, ok := d.Row("nope"); ok {
		t.Fatal("Row of an unknown id must report not-found")
	}
}
```

- [ ] **Step 3: Run and watch it fail**

Run: `go test -count=1 -run TestDispatcherRow ./internal/dispatch/`
Expected: FAIL — `d.Row undefined`.

- [ ] **Step 4: Implement it**

```go
// Row composes one job's listing entry: the header tier plus its rendered
// state, without loading or reading a manifest.
//
// It exists because List renders EVERY job through RenderAll, so using List
// for a single-job lookup trades one manifest read for an O(n) walk. That is
// #436: header-tier callers paying manifest-tier cost because the only safe
// single-job door was the expensive one.
func (d *Dispatcher) Row(id string) (Row, bool) {
	d.mu.Lock()
	e, ok := d.byID[id]
	if !ok {
		d.mu.Unlock()
		return Row{}, false
	}
	j, h := e.j, e.h
	d.mu.Unlock()

	return Row{ID: id, Header: h, View: d.q.Render(j)}, true
}
```

- [ ] **Step 5: Run and watch it pass**

Run: `go test -count=1 -run TestDispatcherRow ./internal/dispatch/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
goimports -w internal/dispatch/ && go build ./...
git add internal/dispatch/
git commit -m "feat(dispatch): add Row(id), the header-tier single-job lookup

Closes #436. List renders every job through RenderAll, so using it for a
single-job lookup swaps a manifest read for an O(n) walk -- which is why
this is a deliverable rather than something the swap resolves in passing.

The test asserts Row and List agree, so the two cannot drift into
answering differently for the same job.

Triage of the 17 SnapshotJob call sites is in the PR body.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

---

### Task 4: The `Dispatcher` control surface

**Files:**
- Modify: `internal/dispatch/registry.go`, `internal/dispatch/dispatch.go`
- Test: `internal/dispatch/control_test.go`

**Interfaces:**
- Consumes: `sched.Queue.Cancel(j)` (`internal/sched/cancel.go:9`), `job.Job.SetIntent` (`job.go`), `Dispatcher.remove` (`registry.go:214`).
- Produces:
  ```go
  func (d *Dispatcher) Job(id string) (*job.Job, bool)
  func (d *Dispatcher) PauseJob(id string) error
  func (d *Dispatcher) ResumeJob(id string) error
  func (d *Dispatcher) Remove(ctx context.Context, id string) error
  ```

**Why this task exists.** The mapping table in Tasks 9–12 maps `q.Pause(id)` to
`j.SetIntent(job.IntentPause)`, which is the right *mechanism* and says nothing
about how a caller reaches `j`. It cannot: there is no door.

```
$ grep -n 'func (d \*Dispatcher) Pause' internal/dispatch/dispatch.go
internal/dispatch/dispatch.go:152:func (d *Dispatcher) Pause() { d.q.Pause(); d.kick() }
```

`Pause`, `Resume` and `Paused` are **queue-wide** and take no id. `lookup(id)`
exists but is unexported (`dispatch.go:108`), and `Row` (Task 3b) deliberately
returns a value with no `*job.Job` in it. So today an API handler can pause the
whole queue and cannot pause one job, and cannot obtain a job to read its
progress.

**Naming.** The per-job doors are `PauseJob`/`ResumeJob`, not overloads of
`Pause`/`Resume`. Go has no overloading, and `Pause()` vs `Pause(id)` cannot
coexist — but the deeper reason is that they are different operations on
different subjects: one sets a queue flag, the other sets one job's intent, and
`ToSABnzbd` already has to tell those apart (`sabnzbd.go`: *"under a queue-wide
pause every job still carries IntentRun"*). Two names for two things.

- [ ] **Step 1: Write the failing test**

```go
package dispatch

import (
	"context"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestDispatcherControlSurface_PerJobDoors pins the doors the API needs and
// did not have: a job pointer, and per-job pause/resume distinct from the
// queue-wide flag.
func TestDispatcherControlSurface_PerJobDoors(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("a", "Job A", job.PolicyFromPP(3))
	if err := d.Add(j, Header{Name: "Job A"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, ok := d.Job("a")
	if !ok || got.ID() != "a" {
		t.Fatalf("Job(a) = %v, %v; want the job", got, ok)
	}

	if err := d.PauseJob("a"); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	if in := j.Intent(); in != job.IntentPause {
		t.Fatalf("Intent = %v, want IntentPause", in)
	}

	// Per-job pause must NOT set the queue-wide flag. Conflating the two is
	// what ToSABnzbd's WaitReason.IsPause() routing exists to survive, and a
	// control surface that sets both makes that distinction unobservable.
	if d.Paused() {
		t.Fatal("PauseJob must not set the queue-wide pause flag")
	}

	if err := d.ResumeJob("a"); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	if in := j.Intent(); in != job.IntentRun {
		t.Fatalf("Intent = %v, want IntentRun", in)
	}

	if _, ok := d.Job("nope"); ok {
		t.Fatal("Job of an unknown id must report not-found")
	}
	if err := d.PauseJob("nope"); err == nil {
		t.Fatal("PauseJob of an unknown id must error")
	}
}

// TestDispatcherRemove_IsIdempotentAndReturnsResources pins that Remove gives
// back what the job held. A removed job that keeps its lease or slot strands
// pool capacity for the life of the process, and nothing later reclaims it --
// the tick only walks registered jobs.
func TestDispatcherRemove_IsIdempotentAndReturnsResources(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("a", "Job A", job.PolicyFromPP(3))
	if err := d.Add(j, Header{Name: "Job A"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := d.Remove(context.Background(), "a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := d.Job("a"); ok {
		t.Fatal("Remove must deregister the job")
	}
	if err := d.Remove(context.Background(), "a"); err == nil {
		t.Fatal("Remove of an already-removed job must error, not silently succeed")
	}
}
```

- [ ] **Step 2: Run and watch both fail**

Run: `go test -count=1 -run 'TestDispatcherControlSurface|TestDispatcherRemove' ./internal/dispatch/`
Expected: FAIL — `d.Job`, `d.PauseJob`, `d.ResumeJob`, `d.Remove` undefined.

- [ ] **Step 3: Implement the doors in `internal/dispatch/registry.go`**

```go
// Job returns the live *job.Job for id.
//
// It is the content-tier door, and Row (above) is the header-tier one. A
// caller that needs progress counters or the manifest takes this; a caller
// that needs a name, a status or a byte total takes Row and must not reach
// for a job pointer to get them -- that is #436 reappearing one level down.
func (d *Dispatcher) Job(id string) (*job.Job, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.byID[id]
	if !ok {
		return nil, false
	}
	return e.j, true
}

// PauseJob asks one job to stop at its next gate.
//
// Distinct from Pause, which sets the QUEUE-wide flag. The two are different
// subjects and must stay observably different: ToSABnzbd routes through
// WaitReason.IsPause() precisely because a queue-wide pause leaves every job
// carrying IntentRun, and a PauseJob that also set the queue flag would make
// that unobservable.
func (d *Dispatcher) PauseJob(id string) error {
	j, ok := d.Job(id)
	if !ok {
		return fmt.Errorf("dispatch: pause %s: %w", id, ErrNotFound)
	}
	if err := j.SetIntent(job.IntentPause); err != nil {
		return fmt.Errorf("dispatch: pause %s: %w", id, err)
	}
	d.kick()
	return nil
}

// ResumeJob clears a pause request by restoring the default intent.
//
// It cannot un-cancel: SetIntent latches IntentCancel (job/intent.go,
// IsLatched), so this returns that error rather than silently doing nothing.
// Silently succeeding would tell the API a cancelled job had been resumed.
func (d *Dispatcher) ResumeJob(id string) error {
	j, ok := d.Job(id)
	if !ok {
		return fmt.Errorf("dispatch: resume %s: %w", id, ErrNotFound)
	}
	if err := j.SetIntent(job.IntentRun); err != nil {
		return fmt.Errorf("dispatch: resume %s: %w", id, err)
	}
	d.kick()
	return nil
}

// Remove cancels a job, deletes its persisted row and deregisters it.
//
// The order is deliberate: Cancel first so sched reclaims the lease and the
// compute slot while the job is still registered. Deregistering first would
// strand both -- the tick only walks registered jobs, so nothing would ever
// return them.
func (d *Dispatcher) Remove(ctx context.Context, id string) error {
	j, ok := d.Job(id)
	if !ok {
		return fmt.Errorf("dispatch: remove %s: %w", id, ErrNotFound)
	}
	if err := d.q.Cancel(j); err != nil {
		return fmt.Errorf("dispatch: remove %s: cancel: %w", id, err)
	}
	if err := d.store.Delete(ctx, id); err != nil {
		return fmt.Errorf("dispatch: remove %s: store: %w", id, err)
	}
	d.res.Evict(id)
	d.remove(id)
	d.kick()
	return nil
}
```

- [ ] **Step 4: Run and watch both pass**

Run: `go test -count=1 -run 'TestDispatcherControlSurface|TestDispatcherRemove' ./internal/dispatch/`
Expected: PASS.

- [ ] **Step 5: Pin the two orderings that fail silently**

Create `internal/dispatch/testdata/control_surface.spec`:

```text
pkg ./internal/dispatch/
run TestDispatcherControlSurface_PerJobDoors

[PauseJob also sets the queue-wide flag]
file internal/dispatch/registry.go
--- anchor
	if err := j.SetIntent(job.IntentPause); err != nil {
--- replace
	d.q.Pause()
	if err := j.SetIntent(job.IntentPause); err != nil {
--- end
```

```text
pkg ./internal/dispatch/
run TestDispatcherRemove_IsIdempotentAndReturnsResources

[Remove deregisters before cancelling, stranding the lease and slot]
file internal/dispatch/registry.go
--- anchor
	if err := d.q.Cancel(j); err != nil {
--- replace
	d.remove(id)
	if err := d.q.Cancel(j); err != nil {
--- end
```

Run: `go run ./scripts/mutate internal/dispatch/testdata/control_surface.spec`
Expected: mutation 1 KILLED. **Mutation 2 will SURVIVE** — the test asserts the
job is deregistered and that a second `Remove` errors, and both hold under the
wrong order. Resource return is what actually breaks, and nothing asserts it.
Add an assertion that pool capacity is back (grant a lease to a second job that
could not have got one before), then re-run and require both KILLED. Do not
drop the mutation.

- [ ] **Step 6: Commit**

```bash
goimports -w internal/dispatch/ && go build ./...
git add internal/dispatch/
git commit -m "feat(dispatch): add the per-job control surface

Pause, Resume and Paused are queue-wide and take no id; lookup is
unexported and Row deliberately carries no job pointer. So an API handler
could pause the whole queue and not one job, and could not obtain a job
to read progress at all. Tasks 9-12 map q.Pause(id) onto SetIntent, which
is the right mechanism and silent about the missing door.

PauseJob/ResumeJob rather than overloads: they act on a different subject
from the queue flag, and ToSABnzbd's WaitReason routing depends on the
two staying observably distinct.

Remove cancels before deregistering so sched reclaims the lease and slot
while the job is still registered; the reverse order strands both,
because the tick only walks registered jobs.

Red check: internal/dispatch/testdata/control_surface.spec, 2/2 KILLED.
The ordering mutation SURVIVED first -- the test asserted deregistration
but not resource return -- and the assertion was added.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---


### Task 5: The `Checkpointer` — one writer for job state

**Files:**
- Create: `internal/job/checkpoint.go`, `internal/checkpoint/checkpointer.go`
- Test: `internal/checkpoint/checkpointer_test.go`

**Interfaces:**
- Consumes: `job.Job` and its content tier (Task 2).
- Produces:
  ```go
  func (j *Job) Checkpoint() Checkpoint            // in internal/job
  type Checkpoint struct { ID string; State StateView; Intent Intent; Progress *JobProgress }

  func New(store Store, every time.Duration, log *slog.Logger) *Checkpointer  // internal/checkpoint
  func (c *Checkpointer) Mark(j *job.Job)
  func (c *Checkpointer) Run(ctx context.Context) error
  func (c *Checkpointer) Flush(ctx context.Context) error
  ```

§10.2's headline property already holds — `job.Job` does no I/O. What does not hold is "one Checkpointer reads snapshots and batches writes": there are **six** single-job writers today.

```
$ git grep -n 'store\.Update(' -- internal/queue/ ':!*_test.go'
internal/queue/queue.go:755    //lockio: persists the reset progress before PromoteNext's RestoreJobProgress re-reads a stale row
internal/queue/queue.go:898    //lockio: keeps RAM and SQLite views of the transition consistent
internal/queue/queue.go:1361   //lockio: … the PostProc transition …          tracked in #229
internal/queue/queue.go:1381   //lockio: … the finish timestamp …             tracked in #229
internal/queue/queue.go:1401   //lockio: … the start timestamp …              tracked in #229
internal/queue/workset.go:453  //lockio: persists the cleared Complete/CRC before re-hydration re-reads a stale row
```

Each of the six exists to close a read-after-write window against a *specific* transition. Five of those transitions are deleted by this plan (`PromoteNext`, the `PostProc` flag, the two timestamps under `SetPostProcStarted`/`MarkDownloadFinished`, and `SetStatus`). **`workset.go:453` is the one that survives**, because §10.1's banner establishes that `resumeAllJobs` is kept, and `ReplaceFromRuns` reaches it.

- [ ] **Step 1: Write the failing test for batching and for the surviving window**

```go
package checkpoint

import (
	"context"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

type recordingStore struct{ batches [][]job.Checkpoint }

func (s *recordingStore) SaveBatch(_ context.Context, cps []job.Checkpoint) error {
	cp := make([]job.Checkpoint, len(cps))
	copy(cp, cps)
	s.batches = append(s.batches, cp)
	return nil
}

// TestCheckpointer_CoalescesMarksIntoOneBatch pins the reason this type exists:
// six single-job writers in internal/queue each closed a read-after-write
// window against one transition, and every one of them was a second writer of
// state the periodic save already owned (Rule 2).
func TestCheckpointer_CoalescesMarksIntoOneBatch(t *testing.T) {
	st := &recordingStore{}
	c := New(st, time.Hour, nil) // never fires on its own; Flush drives it

	a := job.New("a", "A", job.PolicyFromPP(3))
	b := job.New("b", "B", job.PolicyFromPP(3))

	c.Mark(a)
	c.Mark(a) // same job twice must not produce two rows
	c.Mark(b)

	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(st.batches) != 1 {
		t.Fatalf("batches = %d, want 1", len(st.batches))
	}
	if got := len(st.batches[0]); got != 2 {
		t.Fatalf("batch size = %d, want 2 (a and b, a coalesced)", got)
	}
}

// TestCheckpointer_FlushIsSynchronous pins the surviving read-after-write
// window. workset.go:453 persisted a cleared Complete/CRC before re-hydration
// could re-read the stale row; §10.1's banner keeps resumeAllJobs, which
// reaches it via ReplaceFromRuns, so that window survives the swap and Flush
// is what closes it.
func TestCheckpointer_FlushIsSynchronous(t *testing.T) {
	st := &recordingStore{}
	c := New(st, time.Hour, nil)

	j := job.New("a", "A", job.PolicyFromPP(3))
	c.Mark(j)

	if len(st.batches) != 0 {
		t.Fatal("Mark must not write; only Flush and the ticker write")
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(st.batches) != 1 {
		t.Fatalf("Flush must write synchronously; batches = %d", len(st.batches))
	}
}
```

- [ ] **Step 2: Run and watch both fail**

Run: `go test -count=1 ./internal/checkpoint/`
Expected: FAIL — the package does not exist.

- [ ] **Step 3: Write `internal/job/checkpoint.go`**

```go
package job

// Checkpoint is one job's durable state as the Checkpointer sees it: the four
// axes plus the progress record.
//
// It is a VALUE taken under the Job's own lock, which is what lets the
// Checkpointer batch without holding anything. §10.2 requires Job to do no
// I/O, and handing out a value rather than a pointer is what keeps that true
// for the progress record too.
type Checkpoint struct {
	ID       string
	State    StateView
	Intent   Intent
	Progress *JobProgress
}

// Checkpoint takes the job's state for persistence.
func (j *Job) Checkpoint() Checkpoint {
	j.mu.RLock()
	st := j.currentLocked().view()
	in := j.intent
	j.mu.RUnlock()

	j.contentMu.RLock()
	p := j.progress
	j.contentMu.RUnlock()

	return Checkpoint{ID: j.id, State: st, Intent: in, Progress: p}
}
```

- [ ] **Step 4: Write `internal/checkpoint/checkpointer.go`**

```go
// Package checkpoint owns every write of job state to the database.
//
// It exists because internal/queue had SIX single-job writers beside its
// batched periodic save, each closing a read-after-write window against one
// transition (git grep -n 'store\.Update(' -- internal/queue/ ':!*_test.go'
// returned 6 lines before the swap). Five of those transitions are deleted by
// the swap; the sixth, ReplaceFromRuns' cleared Complete/CRC, survives because
// §10.1 keeps resumeAllJobs — and it is served here by Flush rather than by a
// second writer.
package checkpoint

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// Store is the persistence this package needs and no more.
type Store interface {
	SaveBatch(ctx context.Context, cps []job.Checkpoint) error
}

// Checkpointer batches job-state writes. Mark records that a job moved; the
// ticker and Flush are the only things that write.
type Checkpointer struct {
	store Store
	every time.Duration
	log   *slog.Logger

	mu    sync.Mutex
	dirty map[string]*job.Job
}

// New constructs a Checkpointer. every is the batch cadence.
func New(store Store, every time.Duration, log *slog.Logger) *Checkpointer {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Checkpointer{store: store, every: every, log: log, dirty: map[string]*job.Job{}}
}

// Mark records that a job's state has moved and should be written at the next
// batch. It is cheap and never writes: coalescing repeated marks for one job
// into one row is the whole point.
func (c *Checkpointer) Mark(j *job.Job) {
	c.mu.Lock()
	c.dirty[j.ID()] = j
	c.mu.Unlock()
}

// Flush writes every marked job now and clears the set. It is synchronous
// because ReplaceFromRuns needs the row on disk before re-hydration can read
// it — the one read-after-write window the swap does not delete.
func (c *Checkpointer) Flush(ctx context.Context) error {
	c.mu.Lock()
	if len(c.dirty) == 0 {
		c.mu.Unlock()
		return nil
	}
	cps := make([]job.Checkpoint, 0, len(c.dirty))
	for _, j := range c.dirty {
		cps = append(cps, j.Checkpoint())
	}
	clear(c.dirty)
	c.mu.Unlock()

	return c.store.SaveBatch(ctx, cps)
}

// Run drives the periodic batch until ctx is cancelled, then flushes once more.
func (c *Checkpointer) Run(ctx context.Context) error {
	t := time.NewTicker(c.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return c.Flush(context.WithoutCancel(ctx))
		case <-t.C:
			if err := c.Flush(ctx); err != nil {
				c.log.Error("checkpoint flush failed", "error", err)
			}
		}
	}
}
```

- [ ] **Step 5: Run and watch both pass**

Run: `go test -count=1 ./internal/checkpoint/`
Expected: PASS.

- [ ] **Step 6: Pin the coalescing with a mutation**

Create `internal/checkpoint/testdata/coalesce.spec`:

```text
pkg ./internal/checkpoint/
run TestCheckpointer_CoalescesMarksIntoOneBatch

[Mark appends instead of keying by ID, so a job marked twice writes twice]
file internal/checkpoint/checkpointer.go
--- anchor
	c.dirty[j.ID()] = j
--- replace
	c.dirty[j.ID()+string(rune(len(c.dirty)))] = j
--- end

[Flush writes without clearing, so the next Flush rewrites the same rows]
file internal/checkpoint/checkpointer.go
--- anchor
	clear(c.dirty)
--- replace

--- end
```

Run: `go run ./scripts/mutate internal/checkpoint/testdata/coalesce.spec`
Expected: mutation 1 KILLED. **Mutation 2 is expected to SURVIVE against the test as written** — the test flushes once and never checks that a second flush is a no-op. That is the point of running it: add the assertion below, then re-run and require both KILLED.

```go
	// appended to TestCheckpointer_CoalescesMarksIntoOneBatch
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	if len(st.batches) != 1 {
		t.Fatalf("a second Flush with nothing marked must not write; batches = %d", len(st.batches))
	}
```

- [ ] **Step 7: Commit**

```bash
goimports -w internal/job/ internal/checkpoint/ && go build ./...
git add internal/job/checkpoint.go internal/checkpoint/
git commit -m "feat(checkpoint): one writer for job state

§10.2 asks for one Checkpointer reading snapshots and batching writes.
Job already did no I/O; what did not hold was the single writer — six
single-job store.Update sites sat beside the batched periodic save.

Five of the six close windows against transitions this swap deletes. The
sixth, ReplaceFromRuns' cleared Complete/CRC, survives because §10.1
keeps resumeAllJobs, and Flush serves it synchronously rather than as a
second writer.

Red check: internal/checkpoint/testdata/coalesce.spec, 2/2 KILLED. The
second mutation SURVIVED on the first run -- nothing asserted that a
second Flush is a no-op -- and the assertion was added rather than the
mutation dropped.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: The production `Residency`

**Files:**
- Create: `internal/app/residency.go`
- Test: `internal/app/residency_test.go`

**Interfaces:**
- Consumes: `job.Job.AttachContent`/`RestoreContent`/`Evict` (Task 2), `dispatch.Residency` (`ports.go:16`).
- Produces: `func newAppResidency(lookup func(string) (*job.Job, bool), dir string, log *slog.Logger) *appResidency`, satisfying `dispatch.Residency`.

`dispatch.New` panics on a nil `Residency` (`dispatch.go:203`) and nothing in the tree implements it. This is the first of the two ports the daemon needs.

- [ ] **Step 1: Write the failing test**

```go
package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestAppResidency_HydrateThenEvict pins the contract dispatch.Residency
// states: Hydrate makes the manifest available and may block on disk; Evict
// takes it away. The dispatcher decides WHEN and delegates WHAT to here.
func TestAppResidency_HydrateThenEvict(t *testing.T) {
	dir := t.TempDir()
	j := job.New("abc123", "test", job.PolicyFromPP(3))
	writeTestManifest(t, filepath.Join(dir, "abc123.json.gz"), j)

	r := newAppResidency(func(id string) (*job.Job, bool) {
		if id == "abc123" {
			return j, true
		}
		return nil, false
	}, dir, nil)

	if j.Resident() {
		t.Fatal("precondition: job must start non-resident")
	}
	if err := r.Hydrate(context.Background(), "abc123"); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if !j.Resident() {
		t.Fatal("Hydrate must leave the job resident")
	}

	r.Evict("abc123")
	if j.Resident() {
		t.Fatal("Evict must leave the job non-resident")
	}
}

// TestAppResidency_HydrateUnknownJobErrors pins that a missing job is an error
// rather than a silent no-op: the dispatcher logs Residency failures
// (logResidencyError, dispatch.go:171) and a silent success would strand a job
// at Fetching with nothing to fetch from.
func TestAppResidency_HydrateUnknownJobErrors(t *testing.T) {
	r := newAppResidency(func(string) (*job.Job, bool) { return nil, false }, t.TempDir(), nil)
	if err := r.Hydrate(context.Background(), "nope"); err == nil {
		t.Fatal("Hydrate of an unknown job must error")
	}
}
```

- [ ] **Step 2: Run and watch it fail**

Run: `go test -count=1 -run TestAppResidency ./internal/app/`
Expected: FAIL — `newAppResidency` undefined.

- [ ] **Step 3: Implement it**

```go
package app

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/hobeone/gonzbd/internal/job"
)

// appResidency is the production dispatch.Residency: it loads a job's manifest
// from the gzip-JSON file the ingest path wrote, and drops it again.
//
// It holds no registry of its own. The lookup function is the dispatcher's,
// which is what keeps "which jobs exist" a single owner (Rule 2) rather than
// two maps that can disagree.
type appResidency struct {
	lookup func(string) (*job.Job, bool)
	dir    string
	log    *slog.Logger
}

func newAppResidency(lookup func(string) (*job.Job, bool), dir string, log *slog.Logger) *appResidency {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &appResidency{lookup: lookup, dir: dir, log: log}
}

// Hydrate loads the manifest and attaches it. It may block on disk I/O; the
// dispatcher calls it with no lock held (ports.go).
func (r *appResidency) Hydrate(ctx context.Context, id string) error {
	j, ok := r.lookup(id)
	if !ok {
		return fmt.Errorf("residency: hydrate %s: no such job", id)
	}
	if j.Resident() {
		return nil
	}

	m, err := r.readManifest(ctx, id)
	if err != nil {
		return fmt.Errorf("residency: hydrate %s: %w", id, err)
	}

	// A job that has run before has progress already; installing a fresh
	// JobProgress would zero its counters, which is the defect the
	// RestoreContent/AttachContent split exists to prevent.
	if p := j.Progress(); p != nil {
		return j.RestoreContent(m, p)
	}
	return j.AttachContent(m)
}

// Evict drops the manifest. Progress stays resident by design.
func (r *appResidency) Evict(id string) {
	j, ok := r.lookup(id)
	if !ok {
		return
	}
	j.Evict()
}

func (r *appResidency) readManifest(ctx context.Context, id string) (*job.Manifest, error) {
	path := filepath.Join(r.dir, id+".json.gz")
	f, err := os.Open(path) //nolint:gosec // path is dir + a validated job ID
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = zr.Close() }()

	var m job.Manifest
	if err := json.NewDecoder(zr).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, nil
}
```

- [ ] **Step 4: Run and watch both pass**

Run: `go test -count=1 -run TestAppResidency ./internal/app/`
Expected: PASS.

- [ ] **Step 5: Pin the restore-vs-attach branch**

This branch is the one with a silent failure mode — attaching fresh progress to a job that has run zeroes its counters, and nothing errors.

Create `internal/app/testdata/residency_restore.spec`:

```text
pkg ./internal/app/
run TestAppResidency_HydrateThenEvict

[hydration always attaches fresh progress, zeroing a re-hydrated job's counters]
file internal/app/residency.go
--- anchor
	if p := j.Progress(); p != nil {
		return j.RestoreContent(m, p)
	}
--- replace
	if false {
	}
--- end
```

> The test as written will **not** kill this — it hydrates a fresh job, where both branches behave identically. Add a second subtest that marks an article done, evicts, re-hydrates, and asserts the count survived. Then re-run and require KILLED. This is the "assertion on a value the code only produces under conditions the test never creates" failure `AGENTS.md` names, reproduced deliberately so it is caught here rather than in review.

- [ ] **Step 6: Commit**

```bash
goimports -w internal/app/ && go build ./...
git add internal/app/residency.go internal/app/residency_test.go internal/app/testdata/
git commit -m "feat(app): implement dispatch.Residency

dispatch.New panics on a nil Residency and nothing in the tree satisfied
the interface, so the dispatcher could not be constructed at all.

appResidency holds no registry: the lookup is the dispatcher's, which
keeps 'which jobs exist' one owner rather than two maps that can drift.

Red check: internal/app/testdata/residency_restore.spec, 1/1 KILLED --
after adding the evict-and-rehydrate subtest, since the original test
exercised only a fresh job where both branches behave identically.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: The production `Runner`

**Files:**
- Create: `internal/app/runner.go`
- Test: `internal/app/runner_test.go`

**Interfaces:**
- Consumes: `dispatch.Runner` (`ports.go:72`), `dispatch.Dispatcher.Finished`/`Yielded` (`worker.go:33`, `:84`).
- Produces: `func newAppRunner(app *Application) *appRunner`, satisfying `dispatch.Runner`.

`Runner.Run` must return **promptly** — the dispatcher calls it from the tick goroutine, and a blocking Runner stalls every job's advance. It reports completion via `Dispatcher.Finished(id, outcome)` and any other exit via `Dispatcher.Yielded(id)`; not calling either strands the job's lease and compute slot, because the Queue cannot distinguish "holding and working" from "holding and yielded".

- [ ] **Step 1: Write the failing test for the return-promptly contract**

```go
package app

import (
	"context"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestAppRunner_ReturnsPromptly pins ports.go's hardest requirement: Run is
// called from the dispatcher's tick goroutine, so blocking there stalls every
// other job's advance -- not just this one's.
func TestAppRunner_ReturnsPromptly(t *testing.T) {
	r := newAppRunner(newTestApplication(t))

	done := make(chan struct{})
	go func() {
		r.Run(context.Background(), "abc123", job.Fetching)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run blocked for 2s; it must dispatch work and return")
	}
}

// TestAppRunner_EveryStateReportsExactlyOnce pins the resource contract: a
// state that returns without calling Finished or Yielded strands the job's
// lease and compute slot forever, because the Queue cannot tell 'holding and
// working' from 'holding and yielded'.
func TestAppRunner_EveryStateReportsExactlyOnce(t *testing.T) {
	for _, st := range job.AllStates() {
		t.Run(st.String(), func(t *testing.T) {
			app := newTestApplication(t)
			rec := &reportRecorder{}
			r := newAppRunner(app)
			r.report = rec

			r.Run(context.Background(), "abc123", st)
			waitFor(t, func() bool { return rec.calls() == 1 })

			if got := rec.calls(); got != 1 {
				t.Fatalf("state %s reported %d times, want exactly 1", st, got)
			}
		})
	}
}
```

- [ ] **Step 2: Run and watch it fail**

Run: `go test -count=1 -run TestAppRunner ./internal/app/`
Expected: FAIL — `newAppRunner` undefined.

- [ ] **Step 3: Implement it**

```go
package app

import (
	"context"
	"log/slog"

	"github.com/hobeone/gonzbd/internal/job"
)

// reporter is how a runner tells the dispatcher a job's work ended. It is an
// interface so the exactly-once test can observe the calls; production passes
// the Dispatcher itself.
type reporter interface {
	Finished(id string, o job.Outcome) error
	Yielded(id string) error
}

// appRunner routes a job at one state to the subsystem that does that state's
// work, and returns immediately.
//
// Every branch must end in exactly one Finished or Yielded, on some goroutine.
// Returning without either strands the job's lease and compute slot: the Queue
// cannot distinguish "holding and working" from "holding and yielded", so
// nothing else can return them (ports.go, Runner).
type appRunner struct {
	app    *Application
	report reporter
	log    *slog.Logger
}

func newAppRunner(app *Application) *appRunner {
	return &appRunner{app: app, log: app.log}
}

func (r *appRunner) Run(ctx context.Context, id string, state job.State) {
	switch state {
	case job.Fetching:
		go r.runFetch(ctx, id)
	case job.Assessing:
		go r.runAssess(ctx, id)
	case job.Repairing, job.Extracting, job.Finalizing:
		go r.runPostProc(ctx, id, state)
	default:
		// A state with no work is still a state the dispatcher leased. Yield
		// rather than return silently, or the lease is never released.
		r.log.Warn("runner: no work for state; yielding", "job", id, "state", state)
		if err := r.report.Yielded(id); err != nil {
			r.log.Error("runner: yield failed", "job", id, "error", err)
		}
	}
}
```

> `runFetch`, `runAssess` and `runPostProc` are Tasks 8, 9 and 10 — each is written with the consumer it drives, because each needs that consumer already repointed. Until then they are three-line stubs that call `r.report.Yielded(id)`, which satisfies the exactly-once test and keeps the tree green.

- [ ] **Step 4: Run and watch both pass**

Run: `go test -count=1 -run TestAppRunner ./internal/app/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
goimports -w internal/app/ && go build ./...
git add internal/app/runner.go internal/app/runner_test.go
git commit -m "feat(app): implement dispatch.Runner

The second of the two ports dispatch.New requires. Run dispatches to a
goroutine and returns: ports.go states that a Runner blocking in the tick
goroutine stalls every job's advance, not only its own.

The exactly-once test enumerates job.AllStates() rather than the states
implemented today, so a state added later fails here rather than
silently stranding a lease and a compute slot.

Fetching/Assessing/post-proc bodies are stubs that yield; Tasks 8-10
fill each one alongside the consumer it drives.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: Construct the dispatcher in `app.New`

**Files:**
- Modify: `internal/app/app.go:344-355` (store and queue construction)
- Test: `internal/app/dispatcher_wiring_test.go`

**Interfaces:**
- Consumes: Tasks 3, 4, 5.
- Produces: `func (app *Application) Dispatcher() *dispatch.Dispatcher` replacing `func (app *Application) Queue() *queue.Queue` (`app.go:587`).

This is the first task where both models exist at once, and it is deliberately the *only* one: the dispatcher is constructed and started, but nothing routes work to it yet. `internal/queue` still runs the daemon. Tasks 7–11 move consumers across one at a time; Task 13 deletes the loser.

- [ ] **Step 1: Write the failing test**

```go
// TestApplicationConstructsAWiredDispatcher pins that app.New produces a
// dispatcher with both ports satisfied. dispatch.New panics on a nil Residency
// or Runner, so this test failing to panic IS the assertion.
func TestApplicationConstructsAWiredDispatcher(t *testing.T) {
	app := newTestApplication(t)
	if app.Dispatcher() == nil {
		t.Fatal("app.New must construct a Dispatcher")
	}
}
```

- [ ] **Step 2: Run and watch it fail**

Run: `go test -count=1 -run TestApplicationConstructsAWiredDispatcher ./internal/app/`
Expected: FAIL — `app.Dispatcher` undefined.

- [ ] **Step 3: Wire it in `app.New`, beside the existing queue**

```go
	// Both models are constructed here for exactly the span of this branch.
	// internal/queue still runs the daemon; the dispatcher runs nothing until
	// Tasks 7-11 repoint consumers onto it, and Task 13 deletes the loser.
	app.checkpointer = checkpoint.New(checkpointStore, 5*time.Second, log)
	app.residency = newAppResidency(app.lookupJob, manifestDir, log)
	app.runner = newAppRunner(app)
	app.dispatcher = dispatch.New(
		maxActiveJobs,  // pool A: leases — cfg.Downloads.MaxActiveJobs, default 4
		computeSlots,   // pool B: compute — SEE THE NOTE BELOW, this field does not exist yet
		time.Second,
		time.Now,
		schedWorkers,
		app.residency,
		dispatchStore,
		app.runner,
	)
```

- [ ] **Step 3b: Add the pool-B config field**

Pool A already has a config field: `cfg.Downloads.MaxActiveJobs` (`internal/config/downloads.go:50`, default 4 at `defaults.go:79`), which `internal/app/app.go:283` already reads and `reloader.go:102` already hot-reloads. Reuse it.

**Pool B has none.** `MaxComputeSlots` does not exist anywhere in `internal/config`. Adding it is a config-contract change, not a constant:

- add the field to `internal/config/downloads.go` with a doc comment
- add the default to `internal/config/defaults.go`
- add the commented entry to `gonzbd.yaml`
- add the `docs/sabnzbd_spec.md` §9.x row
- run `go test ./internal/config/ -run 'TestUI|TestAllFlat'`

Read `docs/config-contract.md` first — it owns the rule that these four move together, and the contract test fails if they do not.

- [ ] **Step 4: Run and watch it pass**

Run: `go test -count=1 -run TestApplicationConstructsAWiredDispatcher ./internal/app/`
Expected: PASS.

- [ ] **Step 5: Run the full suite — this is the first commit where both models are live**

Run: `go test -race -count=1 ./...`
Expected: PASS. A failure here is a real conflict between the two models (most likely both trying to own the SQLite `jobs` table) and must be diagnosed, not worked around.

- [ ] **Step 6: Commit**

```bash
goimports -w internal/app/ && go build ./... && go vet ./...
git add internal/app/
git commit -m "feat(app): construct the dispatcher, residency, runner and checkpointer

First commit where both models exist. internal/queue still runs the
daemon; the dispatcher is constructed and started but nothing routes work
to it until Tasks 7-11.

This is the ONE overlap commit. It is not a dual path being shipped --
§15 forbids that -- it is the point where the new model becomes
constructible, and Task 13 deletes the old one on the same branch.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Tasks 9–12: Repoint the consumers

These four tasks share one shape, and it is stated once here rather than repeated four times. **Each is a separate task because a reviewer can meaningfully reject one while approving its neighbours** — they touch disjoint packages and fail in different ways.

For each consumer package:

1. Replace `*queue.Queue` in the struct or signature with `*dispatch.Dispatcher`.
2. Replace each `queue.Queue` method call per the mapping table below.
3. Replace `*queue.Job` reads with `dispatch.Row` (header tier) or `*job.Job` (content tier).
4. Replace every `constants.Status` **branch** with a `job.State`/`Outcome`/`Intent` branch. Every `constants.Status` **render** becomes `row.Status()` (Task 12).
5. Update that package's tests; run `go test -race -count=1 ./internal/<pkg>/`.
6. Commit per package.

**The mapping table** — derived from the 55 exported `*Queue` methods:

| `internal/queue` | Replacement |
|---|---|
| `q.Add(job)` | `d.Add(j, dispatch.Header{...})` |
| `q.Remove(id)` | `d.Cancel(id)` then the finalizer's delete |
| `q.Pause(id)` / `q.Resume(id)` | `j.SetIntent(job.IntentPause)` / `job.IntentRun` |
| `q.PauseAll()` / `q.ResumeAll()` | `d.Pause()` / `d.Resume()` |
| `q.IsPaused()` | `d.Paused()` |
| `q.Retry(id)` | `d.Retry(id)` |
| `q.Snapshot()` | `d.List()` |
| `q.SnapshotJob(id)` | `d.Row(id)` (header) or the registry's `*job.Job` (content) |
| `q.GetJobStatus(id)` | `row.Status()` — **render only** |
| `q.SetStatus` / `q.SetStatusIf` | **deleted** — the machine owns state; `Transition`/`SetNext` |
| `q.SetPostProcStarted(id)` | **deleted** — `Cross(job.Extracting)` is the boundary |
| `q.PromoteNext(ctx)` | **deleted** — the dispatcher's tick loop |
| `q.ActiveSet()` | **deleted** — pool A |
| `q.MarkArticleEmittedByIdx(id, i)` | `j.MarkArticleEmitted(int(i))` |
| `q.MarkFileComplete(id, fi)` | `j.MarkFileComplete(fi)` |
| `q.CheckEarlyAbort(id)` | `j.Progress().EarlyAborted()` |
| `q.TotalRemainingBytes()` | sum `row` progress over `d.List()` |
| `q.Save(dir)` | `checkpointer.Flush(ctx)` |

- [ ] **Task 9: `internal/downloader`** — `dispatch.go`, `downloader.go`. 2 files, and both are scheduling-only with **no** `constants.Status` reads, which is why they go first: they cannot regress status rendering because they never do any. Fill `appRunner.runFetch` here.
- [ ] **Task 10: `internal/postproc`** — `stages.go`, `filelist.go`, `stage_quickcheck.go`. The `quickcheck` stage is **retained** (§1.2 amendment item 3); its six `job.Queue` reads are repointed, and `:180`'s error return must stay distinguishable so an unreadable manifest is never reported as CRC-verified (#294). This package reads the manifest at four post-boundary sites — `stage_quickcheck.go:136,180`, `filelist.go:30,244` — which is the measurement that refutes a lease-gated manifest; see the starting-position section. Fill `runPostProc`.
- [ ] **Task 11: `internal/app`** — `app.go`, `pipeline.go`, `statusinfo.go`, `stall.go`, `durability.go`, `ingest.go`, `resume_startup.go`, `par2names.go`. The largest. `resume_startup.go` is **kept** (§10.1) and repointed, not deleted. Fill `runAssess`.
- [ ] **Task 12: `internal/api` + `cmd`** — `queue.go`, `roles.go`, `server.go`, `apitest/nopapp.go`, `main.go`. This is where the surviving `constants.Status` renders live, so Task 3's accessor must already exist.

Each task ends with `go test -race -count=1 ./...` green and its own commit.

---

### Task 13: Delete `internal/queue` and both scaffolds

**Files:**
- Delete: `internal/queue/` entirely
- Test: `internal/dispatch/manifest_boundary_test.go` (Task 1) must still pass

- [ ] **Step 1: Prove nothing imports it**

```bash
git grep -l '"github.com/hobeone/gonzbd/internal/queue"' -- '*.go'
```
Expected: **no output**. Any hit is a consumer Tasks 9–12 missed; go back and finish it rather than deleting under it.

- [ ] **Step 2: Delete the package and the scaffolds**

```bash
git rm -r internal/queue/
```

This removes `internal/queue/aliases.go` (Task 1's scaffold) with it. Verify the second scaffold is gone too:

```bash
git grep -n 'queueResidency\|DELETE ME IN TASK 13'
```
Expected: no output. A hit means a branch-local scaffold is about to ship, which is the one thing this plan's own constraint forbids.

- [ ] **Step 3: Verify the Deletes column is actually empty**

```bash
git grep -nE '\b(JobPhase|ActiveSet|PromoteNext|evictJobLocked|SetStatusIf|SetPostProcStarted|shouldSkipForPP)\b' -- '*.go'
```
Expected: no output.

Then confirm the three entries that must **survive**, because §15's column is wrong about them and a zealous sweep would take them out:

```bash
git grep -c 'resumeAllJobs' -- '*.go'          # must be non-zero: §10.1, it is the mechanism
wc -l internal/postproc/stage_quickcheck.go     # must exist: §1.2 amendment item 3
```

- [ ] **Step 4: Full gates**

```bash
go build ./... && go vet ./... && go vet -tags=integration,uitest,crash ./...
go test -race -count=1 ./...
golangci-lint run ./...
./scripts/run_tests.sh
go test -v -tags=integration ./test/integration/... ./internal/par2/...
```

- [ ] **Step 5: Commit**

```bash
git commit -m "refactor(queue)!: delete internal/queue

8,531 non-test lines and 55 exported Queue methods, replaced by
internal/job (the record and its content tier), internal/sched (the
decisions) and internal/dispatch (the registry, residency and loop).

Both branch-local scaffolds go with it: internal/queue/aliases.go and
the queueResidency shim. Neither ever appeared on main.

BREAKING CHANGE: the on-disk queue state is now written by
internal/checkpoint against the dispatch_jobs schema. Per Standing
Design Rule 1 there is no migration and none is owed.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 14: Sweep the claims the swap falsified

**Files:** `docs/ARCHITECTURE.md`, `docs/queue-lifecycle.md`, `docs/durability-contract.md`, `docs/post-processing-contract.md`, `docs/superpowers/specs/2026-08-25-job-lifecycle-design.md`

This is `AGENTS.md` § "Step 4 in practice", and it is a task rather than a step because the swap falsifies more prose than any change in this repository's history.

- [ ] **Step 1: Grep the literals, from the repository root**

```bash
git grep -n 'internal/queue'
git grep -n 'queue.Queue\|queue\.Job\|SnapshotJob\|PromoteNext\|ActiveSet'
git grep -n 'three tiers\|header tier\|manifest tier\|progress tier'
```

- [ ] **Step 2: Read `docs/ARCHITECTURE.md` and `docs/queue-lifecycle.md` in full**

Not grep. `AGENTS.md`: *"`git grep` is blind to paraphrase, and the docs are where paraphrase lives."* `docs/queue-lifecycle.md` is the doc this change most invalidates — it is named for the deleted package and states the residency invariant as compiler-enforced, which Task 1 downgraded to a test.

- [ ] **Step 3: Recompute every stated figure whose terms changed**

`docs/queue-lifecycle.md` carries a per-article memory budget. The article bitsets moved packages but did not change shape; confirm the figure still holds rather than assuming it, and if a term was removed, recompute the total rather than deleting one term and leaving the arithmetic visibly wrong.

- [ ] **Step 4: Update §15's status block to record plan 2 as landed**

- [ ] **Step 5: Run the three whole-repo gates and `comment-analyzer`**

```bash
go run ./scripts/check_dup_comments
go run ./scripts/check_review_banner
go run ./scripts/check_citations
```

Then `pr-review-toolkit:comment-analyzer` over the cumulative PR diff — **once, on the last round**, since each round's fix creates fresh drift.

- [ ] **Step 6: Commit**

---

## Self-Review

**Spec coverage.** §15's plan 2 row: `Manifest`/`JobProgress` → Tasks 1–2. `Lease` → landed (#447); its reserved manifest/barrier slot is refuted, not owed (Task 2 Step 6b). `Assess`+`Verdict` → landed and partly not owed (§1.2). New `Queue` with pools and lease issuance → landed as `internal/sched`. `Checkpointer` → Task 5. Barrier self-reconciliation → not owed (§10.1). `app`/`downloader`/`postproc` rewired → Tasks 9–12. Deletes column → Task 13. §12.1's accessor → Task 3. §12.3's #436 → Task 3b. Residency enumeration test → Task 1 Step 6. Per-job control doors → Task 4.

### What this revision took from an independently-written plan

A second plan for this work was written separately. Three of its judgements are adopted here, and one is rejected with evidence. Recording both directions, because the rejected one is the most likely thing for a third plan to re-propose:

| Its position | Outcome |
|---|---|
| The `Dispatcher` control surface is missing and needs building | **Adopted** — Task 4. This plan previously mapped `q.Pause(id)` onto `SetIntent` without noticing that no door exists to reach the job. |
| `Row.Status()` and `Dispatcher.Row(id)` belong early, before the consumers | **Adopted** — Tasks 3/3b. This plan had the accessor at Task 12 while asserting the API repoint depended on it. |
| `Remove` must return pool resources, and job lookup needs its own door | **Adopted** — Task 4, with the ordering pinned by mutation. |
| `Lease` should carry the `*Manifest`, per the reserved slot in `lease.go` | **Rejected** — see "The `Lease` does NOT carry the manifest" above. Grant runs under `Queue.mu` where no I/O may happen, and post-processing reads the manifest at four sites while holding no lease. The reserved comment is a stale instruction and Task 2 Step 6b deletes it. |

The rejection was reached by testing the idea rather than by preferring the existing design: the first review of that plan conceded the point on the strength of the reserved slot and `§15`'s *"the `Lease` makes residency an object the compiler can see"*, and reversed only after reading `requirements.go` and the four `postproc` call sites. **A reserved field with an argument attached is not evidence that the argument survived.**

**Gaps I am naming rather than papering over:**

1. **Tasks 9–12 are specified as a shared shape plus a mapping table, not as per-file code.** Four packages, 17 non-test files and 90 test files cannot be written out as literal diffs in advance without inventing code for files whose post-Task-8 state does not exist yet. The mapping table is the actual instruction and it is complete for all 55 methods; the per-file work is mechanical against it. **This is the plan's weakest region and the most likely place for it to be wrong.**
2. **The 90 test files have no task of their own.** They are folded into Tasks 9–12 by package. If that proves to be the bulk of the work — plausible, since tests outnumber production files 5:1 here — Task 11 in particular may need splitting during execution.
3. **`newTestApplication`, `writeTestManifest`, `waitFor` and `reportRecorder`** are referenced by Tasks 4–6 and not defined. They are small test helpers whose shape depends on `app.Application` after Task 8; whoever executes Task 6 writes them.
4. **Pool B has no config field.** Pool A reuses `cfg.Downloads.MaxActiveJobs`, which exists and is already hot-reloaded. Pool B's does not exist and Task 8 Step 3b adds it as a full config-contract change. An earlier draft of this plan named `cfg.Misc.MaxComputeSlots` and `cfg.Misc.MaxActiveDownloads` — **neither exists**; both were invented, and the error is recorded here rather than quietly corrected because it is the kind a reader would otherwise inherit as fact.

**Three identifier errors were found by verification during self-review**, all of which would have failed at Task 1 or Task 8 rather than being caught by review:

| Asserted | Actual |
|---|---|
| `JobFile`/`JobArticle` move with `manifest.go` | they live in `internal/queue/job.go:740,754`, which is not in the moving set — Step 1b |
| `JobProgress.markFileComplete` | no such method; it is `(*queue.Job).markFileComplete`, relocated there by #458 — Step 1c |
| `cfg.Misc.MaxComputeSlots`, `cfg.Misc.MaxActiveDownloads` | neither exists; pool A is `cfg.Downloads.MaxActiveJobs`, pool B is unbuilt |

Assume the same error rate in Tasks 9–12's mapping table, which was derived the same way and has had no equivalent verification pass. **Verify each row against the source before acting on it.**

**Type consistency.** `AttachContent`/`RestoreContent`/`Evict`/`Resident`/`Manifest`/`Progress` are used consistently in Tasks 2, 4 and 3. `Row`/`Row.Status()`/`Dispatcher.Row(id)` are consistent across 7, 11 and 12. `Checkpoint`/`Mark`/`Flush` consistent across 3 and 13.

---

## Risks

| Risk | Why it is real here | Mitigation in the plan |
|---|---|---|
| Task 1 is bigger than "a file move" | D1's measurement was one-directional; 84 sites reach *in* | Task 2 exists solely to give them an owner, and Task 1 Step 3 makes the failure visible rather than surprising |
| The overlap commit (Task 8) ships | Both models constructed at once | Task 13 Step 2 greps for both scaffolds and fails if either survives |
| A zealous sweep deletes what §15 wrongly lists | `resumeAllJobs` and `quickcheck` are on the Deletes column and must **not** go | Task 13 Step 3 asserts both still exist |
| Tests are the bulk, and are unplanned | 90 test files vs 17 production | Named as gap 2 above; expect to split Task 11 |
| Pool sizing needs a config change | `MaxComputeSlots` does not exist | Named as gap 4; `docs/config-contract.md` governs it |
