# B2.2 — a SQLite `dispatch.Store` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** give `dispatch.Store` a SQLite implementation, and the schema to hold it, so the
dispatcher's restore/persist paths run against a real database instead of a test fake.

**Architecture:** a new `dispatch_jobs` table owned solely by the new
`internal/dispatch/store` package, which satisfies the `dispatch.Store` interface
`internal/dispatch` already declares. `Persisted` gains the two fields restore needs and
does not currently carry — queue order and the job's `Policy`.

**Tech Stack:** Go 1.27, `modernc.org/sqlite` via `internal/history`, `goose` migrations.

**RFC:** #454. Decisions D1–D4 settled there; D3 was reversed in a follow-up comment and
this plan implements the reversal (persist `Policy`, not `PP`). D5 withdrawn.

**Spec:** `docs/superpowers/specs/2026-08-28-sched-exported-surface-design.md`

**Review:** this plan was reviewed against source by a second model
(`gemini-3.7-flash-high`, read-only) before any code was written. It returned seven
findings, two blocking; all seven were verified against source and applied. Where a
revision exists because of that review, the text says so and names the finding, so a reader
can tell a considered decision from an original one. The two blocking findings are worth
knowing about even if you only skim: the first draft's `restore` silently renumbered the
database on its first tick, and it did not update `testdata/schema.golden`.

## Global Constraints

- **No new `goose` migration.** `001_initial.sql` is edited in place, per direction on
  #454. No version bump.
- **Standing Design Rule 2:** `dispatch_jobs` has exactly one writer, the new store. No
  other package reads or writes it.
- **Standing Design Rule 4:** any comment quantifying over writers/callers states the
  command that enumerated them.
- `internal/dispatch` must not import a SQL driver. The store package does.
- Nothing in production is repointed. B2.2 leaves `internal/dispatch` with no production
  importer, exactly as B2.3 left it.

---

## Ordering: why a monotonic insertion key is sufficient

`Persisted.SortKey` needs an owner, and the answer is the dispatcher's `d.order`.

The key can be a **monotonic insertion sequence, assigned at `Add` and never revised**,
because only two operations change `d.order`, and neither invalidates such a key:

- `Add` appends (`registry.go`: `d.order = append(d.order, j.ID())`).
- `remove` deletes in place with `slices.Delete`, which is order-preserving — its own
  comment says so, and gives the reason (a swap-with-last would silently reorder jobs
  behind the removed one).

So insertion order and `d.order` cannot diverge, and no renumbering pass is needed. Task 2
pins that enumeration with a test rather than a comment, per Rule 4.

**The trap, and it is sharper than the first draft of this plan allowed.** It is not
enough for `restore` to resume the counter above the highest restored key. If `restore`
registers through today's `Add`, `Add` assigns each restored job a *fresh* sequence
(`0, 1, 2`) while `d.written` holds the row's real `SortKey` from disk. On the first tick,
`persistIfChanged` builds a row whose `SortKey` is now `0`, compares it against a
`lastWritten` whose `SortKey` is `10`, finds them unequal, and fires a spurious `Save` that
**renumbers the database** — falsifying this section's own "assigned at Add and never
revised" claim on the very first tick.

**So there is one registration path, and it takes the sequence as a parameter.** An
unexported `register(j, h, seq)` owns `d.byID`, `d.order` and `d.nextSeq`; `Add` calls it
with `d.nextSeq`, and `restore` calls it with `p.SortKey`. `register` maintains
`d.nextSeq = max(d.nextSeq, seq+1)`, which resolves the resume problem in the same place
rather than as a separate step someone can forget. One writer, per Rule 2 — two
registration paths that must agree about a field is exactly the smell the rule names.

Found by cross-model review of this plan (#454); the first draft had `restore` call `Add`
and fix up the counter afterwards.

---

## Task 1: schema — the `dispatch_jobs` table

**Files:**
- Modify: `internal/history/migrations/001_initial.sql`
- Modify: `internal/history/testdata/schema.golden`

**Interfaces:**
- Produces: table `dispatch_jobs`, consumed by Task 4.

- [ ] **Step 1: add the table to the `+goose Up` section**, after the `jobs` block

```sql
-- +goose StatementBegin
-- dispatch_jobs is internal/dispatch/store's table and nothing else's. It is
-- deliberately NOT columns on `jobs`: that table is internal/queue's until
-- B2.4 deletes the package, and its `status` column is a lossy projection of
-- the same position `state` records here. Two writers to one row could let
-- the two disagree, which is the second-writer smell Standing Design Rule 2
-- names. The duplicated identity/header columns are the price, and they go
-- away with `jobs`.
--
-- The four axes (state, next, activity, outcome) plus intent are stored as
-- their integer enum values, matching internal/job's uint8 constants. They
-- are validated on the way back in rather than by a CHECK constraint:
-- reconstruct replays a restored row forward through job.Job's own doors, so
-- an illegal position, an inadmissible outcome, or a next that is not a legal
-- edge is refused by the state machine itself. A CHECK would be a second
-- enforcement point for one invariant.
--
-- `crossed` is absent on purpose: it is derived from state via
-- Attempt.crossed, and storing it would create a second source of truth that
-- could disagree with state after a restore.
CREATE TABLE dispatch_jobs (
    id          TEXT PRIMARY KEY,
    -- sort_key is queue order: a monotonic insertion sequence the dispatcher
    -- assigns at Add and never revises. See the plan's ordering section for
    -- why it needs no renumbering.
    sort_key    INTEGER NOT NULL,

    -- Header: display metadata the caller supplies at Add, which job.Job does
    -- not carry.
    name        TEXT NOT NULL,
    category    TEXT NOT NULL DEFAULT '',
    priority    INTEGER NOT NULL DEFAULT 0,
    bytes       INTEGER NOT NULL DEFAULT 0,

    -- Policy: what the job is permitted to do, resolved at ingestion. Stored
    -- resolved rather than as the upstream PP integer because PP is external
    -- vocabulary that "does not exist past App" (internal/job/policy.go).
    verify      INTEGER NOT NULL DEFAULT 0,
    repair      INTEGER NOT NULL DEFAULT 0,
    unpack      INTEGER NOT NULL DEFAULT 0,
    delete_ok   INTEGER NOT NULL DEFAULT 0,

    -- The StateView axes.
    state       INTEGER NOT NULL DEFAULT 0,
    next        INTEGER NOT NULL DEFAULT 0,
    activity    INTEGER NOT NULL DEFAULT 0,
    outcome     INTEGER NOT NULL DEFAULT 0,
    assessed    INTEGER NOT NULL DEFAULT 0,

    intent      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_dispatch_jobs_sort_key ON dispatch_jobs(sort_key);
-- +goose StatementEnd
```

- [ ] **Step 2: add the matching `DROP TABLE dispatch_jobs;` to the `+goose Down` section**,
      in the same relative position the file's other drops use.

- [ ] **Step 3: watch the golden schema test fail, then regenerate it**

`TestMigrations_GoldenSchema` (`internal/history/migrations_test.go:298`) dumps every
table, index and foreign key from `sqlite_master` and asserts strict equality against
`internal/history/testdata/schema.golden`. Adding a table fails it by construction, which
is the point — it is the gate that notices a schema change nobody declared.

```bash
go test -count=1 ./internal/history/ -run TestMigrations_GoldenSchema   # MUST fail
go test -count=1 ./internal/history/ -run TestMigrations_GoldenSchema -update
git diff internal/history/testdata/schema.golden                        # review the delta
```

Read the regenerated diff rather than trusting `-update`: it should add `dispatch_jobs` and
its index and change nothing else. Anything else in that diff is a schema change this task
did not intend.

- [ ] **Step 4: verify the whole package**

```bash
go test -count=1 ./internal/history/
```
Expected: PASS. If a local `gonzbd.db` exists it must be deleted — see #454's D5 note.

- [ ] **Step 5: commit**

```bash
git add internal/history/migrations/001_initial.sql internal/history/testdata/schema.golden
git commit -m "feat(history): add the dispatch_jobs table"
```

---

## Task 2: `Persisted` gains `SortKey`; pin the order enumeration

**Files:**
- Modify: `internal/dispatch/ports.go` (the `Persisted` struct)
- Modify: `internal/dispatch/registry.go` (`Add` assigns the sequence; `entry` holds it)
- Modify: `internal/dispatch/dispatch.go` (the counter field)
- Modify: `internal/dispatch/tick.go` (`persistIfChanged` populates `SortKey`)
- Test: `internal/dispatch/registry_test.go`

**Interfaces:**
- Produces: `Persisted.SortKey int64`, consumed by Tasks 3 and 4.

- [ ] **Step 1: write the failing test** — pin that the only two operations changing
      `d.order` are an append and an order-preserving delete, so an insertion sequence
      reproduces queue order.

```go
func TestSortKey_ReproducesQueueOrderAcrossRemoval(t *testing.T) {
	d := newTestDispatcher(t)
	for _, id := range []string{"a", "b", "c", "d"} {
		if err := d.Add(job.New(id, id, job.Policy{}), Header{Name: id}); err != nil {
			t.Fatalf("Add(%s): %v", id, err)
		}
	}
	d.remove("b")

	var got []string
	var keys []int64
	for _, id := range d.snapshotIDs() {
		got = append(got, id)
		keys = append(keys, d.sortKeyOf(id))
	}
	if want := []string{"a", "c", "d"}; !slices.Equal(got, want) {
		t.Fatalf("order after removal = %v, want %v", got, want)
	}
	// Exact keys, not slices.IsSorted. IsSorted is non-strict, so an
	// implementation that never assigns seq at all yields [0 0 0] and PASSES
	// — while SortKey is entirely broken. The removal of "b" is what makes
	// the gap at 1 meaningful: keys are an insertion sequence, not a
	// position, so they do not renumber.
	if wantKeys := []int64{0, 2, 3}; !slices.Equal(keys, wantKeys) {
		t.Fatalf("sort keys = %v, want %v", keys, wantKeys)
	}
}
```

- [ ] **Step 2: run it and confirm it fails** (`sortKeyOf`/`snapshotIDs` undefined)

```bash
go test -count=1 ./internal/dispatch/ -run TestSortKey_ReproducesQueueOrderAcrossRemoval
```
Expected: build failure naming the undefined helpers.

- [ ] **Step 3: implement, as ONE registration path.** Add `seq int64` to `entry` and
      `nextSeq int64` to `Dispatcher`. Extract `Add`'s body into an unexported
      `register(j *job.Job, h Header, seq int64) error` that owns `d.byID`, `d.order` and
      `d.nextSeq` under one `d.mu` span, including
      `d.nextSeq = max(d.nextSeq, seq+1)`. `Add` becomes a call to `register` with
      `d.nextSeq`. Add the two unexported test accessors, and set `SortKey: e.seq` in
      `persistIfChanged`.

      Do **not** give `restore` its own registration path — see the ordering section. Task 3
      makes it the second `register` caller.

- [ ] **Step 4: run to verify it passes, then the package**

```bash
go test -count=1 -race ./internal/dispatch/
```

- [ ] **Step 5: red-green check, both halves.** The test pins two independent things, so
      it needs two reverts (AGENTS.md: "revert each half separately"):
      1. Neuter `remove`'s `slices.Delete` to a swap-with-last → the order assertion must fail.
      2. Neuter `register` so it never assigns `seq` (leave it zero) → the key assertion
         must fail. This half is what Finding 3 of the plan review showed the original
         `slices.IsSorted` form could not catch.

      Restore from your own copy each time (never `git stash` — see AGENTS.md). Record both
      observed failure messages in the commit body.

- [ ] **Step 6: commit**

```bash
git add internal/dispatch/
git commit -m "feat(dispatch): carry queue order on Persisted as an insertion sequence"
```

---

## Task 3: `Persisted` gains `Policy`; restore resumes the sequence

**Files:**
- Modify: `internal/dispatch/ports.go` (`Persisted.Policy`)
- Modify: `internal/dispatch/dispatch.go` (`reconstruct` signature and `restore`)
- Modify: `internal/dispatch/tick.go` (`persistIfChanged` populates `Policy`)
- Test: `internal/dispatch/dispatch_test.go`

**Interfaces:**
- Consumes: `Persisted.SortKey` from Task 2.
- Produces: `reconstruct(id, name string, pol job.Policy, v job.StateView, intent job.Intent, now time.Time)`.

- [ ] **Step 1: write two failing tests**

```go
func TestRestore_PreservesPolicy(t *testing.T) {
	pol := job.Policy{Verify: true, Repair: true, Unpack: true, Delete: true}
	// seed a store row carrying pol, Start the dispatcher, assert the
	// registered job reports pol rather than the zero Policy.
}

func TestRestore_PreservesSortKeyAndResumesAbove(t *testing.T) {
	// Seed rows with NON-CONTIGUOUS, NON-ZERO keys — 10, 50, 100. Contiguous
	// keys starting at 0 are exactly what a broken restore would invent, so
	// they cannot distinguish a preserved key from a reassigned one.
	//
	// Assert on sortKeyOf, NOT on List order. Add appends unconditionally, so
	// a fresh job is last in d.order whether or not the counter resumed —
	// which is what Finding 4 of the plan review caught in the first draft.
	for id, want := range map[string]int64{"a": 10, "b": 50, "c": 100} {
		if got := d.sortKeyOf(id); got != want {
			t.Fatalf("restored %s sortKey = %d, want %d (restore reassigned it)", id, got, want)
		}
	}
	// ... Add a fresh job ...
	if got := d.sortKeyOf("fresh"); got <= 100 {
		t.Fatalf("fresh job sortKey = %d, want > 100 (counter did not resume)", got)
	}
}

func TestRestore_DoesNotRewriteRowsItJustRead(t *testing.T) {
	// The spurious-Save regression, pinned directly: seed rows, Start, run one
	// tick with nothing changed, and assert the store recorded ZERO Saves.
	// Without register() preserving p.SortKey this fails with three.
}
```

- [ ] **Step 2: run both and confirm they fail**

```bash
go test -count=1 ./internal/dispatch/ -run 'TestRestore_'
```
Expected: the first fails on a zero `Policy`; the second on a restored key of `0` rather
than `10`; the third on three unexpected `Save` calls.

- [ ] **Step 3: implement.** Thread `p.Policy` into `reconstruct`'s `job.New` call,
      replacing today's `job.Policy{}`. In `restore`, call Task 2's
      `register(j, p.Header, p.SortKey)` instead of `Add`, which both preserves the stored
      key and resumes `d.nextSeq` in one place.

      Sort `rows` before registering rather than trusting `Load`'s slice order, and match
      `Load`'s tiebreak exactly — `SortKey` ascending, then `ID` ascending, via
      `slices.SortStableFunc`. A Go sort keyed on `SortKey` alone disagrees with
      `ORDER BY sort_key ASC, id ASC` whenever two keys collide, and the resulting order is
      then non-deterministic (plan review, Finding 7).

      The rollback `defer` already calls `remove` for each registered ID, and `remove`
      prunes every per-job map — but note it does **not** rewind `d.nextSeq`. That is
      correct and deliberate: a retry re-reads the same rows and `register`'s
      `max(d.nextSeq, seq+1)` is idempotent, so a burned range costs nothing but leaves
      keys monotonic.

- [ ] **Step 4: update `reconstruct`'s doc comment.** It currently explains why there is no
      `job.Restore` constructor and does not mention `Policy`; the sentence about `job.New`
      taking an empty Policy is falsified by this task. Sweep for the claim from the repo
      root, not just in the file:

```bash
git grep -n 'job.Policy{}'
```

- [ ] **Step 5: run to verify, then the package with race**

```bash
go test -count=1 -race ./internal/dispatch/
```

- [ ] **Step 6: commit**

```bash
git add internal/dispatch/
git commit -m "feat(dispatch): restore a job's Policy and resume the order sequence"
```

---

## Task 4: `internal/dispatch/store` — the SQLite implementation

**Files:**
- Create: `internal/dispatch/store/store.go`
- Create: `internal/dispatch/store/doc.go`
- Test: `internal/dispatch/store/store_test.go`
- Test: `internal/dispatch/store/lifecycle_test.go` (dispatcher against real SQLite)

**Interfaces:**
- Consumes: `dispatch.Persisted` (with `SortKey` and `Policy`), table `dispatch_jobs`.
- Produces: `func New(db *sql.DB) *Store`, satisfying `dispatch.Store`.

- [ ] **Step 1: write the failing round-trip test** against a real migrated database,
      following `internal/queue/sqlite_store_test.go:26-32`'s pattern
      (`history.Open` → `history.NewRepository` → `.DB()`).

```go
func TestStore_RoundTripsEveryAxis(t *testing.T) {
	s := newTestStore(t)
	want := dispatch.Persisted{
		ID:      "j1",
		SortKey: 7,
		Header:  dispatch.Header{Name: "n", Category: "tv", Priority: 2, Bytes: 1 << 20},
		// Every Policy field true. A mixed literal leaves the false fields
		// agreeing with the column DEFAULT 0, so a swapped or unmapped
		// `repair`/`delete_ok` column round-trips correctly by accident
		// (plan review, Finding 5). Step 4 covers the other combinations.
		Policy:  job.Policy{Verify: true, Repair: true, Unpack: true, Delete: true},
		State: job.StateView{
			State: job.Repairing, Next: job.Extracting,
			Activity: job.ActPar2Repair, Outcome: job.OutcomePending, Assessed: true,
		},
		Intent: job.IntentCancel,
	}
	if err := s.Save(t.Context(), want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("Load = %+v, want exactly [%+v]", got, want)
	}
}
```

- [ ] **Step 2: run it and confirm it fails** (package does not exist)

```bash
go test -count=1 ./internal/dispatch/store/
```

- [ ] **Step 3: implement `Load`, `Save`, `Delete`.**
  - `Save` is an `INSERT ... ON CONFLICT(id) DO UPDATE` over every column — the dispatcher
    calls it for both first write and update, and `persistIfChanged` has already decided
    the row changed.
  - `Load` is `SELECT ... FROM dispatch_jobs ORDER BY sort_key ASC, id ASC`. The `id`
    tiebreak makes the result total rather than merely mostly-ordered.
  - `Delete` is `DELETE FROM dispatch_jobs WHERE id = ?`. Deleting an absent row is not an
    error — see Step 4b.3 for why.
  - Booleans go through `0`/`1` integers; enums through `uint8` casts.

- [ ] **Step 4: add the table-driven exhaustiveness test.** Round-trip every `job.State`
      from `AllStates()`, every `Outcome`, every `Intent` and every `Activity`, so a new
      enum member fails here rather than silently persisting as its zero value. Include all
      sixteen `Policy` combinations — that is what makes a swapped boolean column
      detectable.

- [ ] **Step 4b: test the paths the round-trip does not reach.** `Save` is an upsert and
      `Delete` has a documented absent-row contract, and neither is exercised by a
      single-row round-trip (plan review, Finding 5):
      1. `Save` an existing ID with every axis changed → `Load` returns the new values, and
         exactly one row.
      2. `Delete` an existing ID → `Load` no longer returns it.
      3. `Delete` an absent ID → returns `nil`, not an error. This one is load-bearing:
         `evictCancelledNeverRun` treats the job's pass as over whether or not the delete
         succeeded, so making absence an error would turn a redundant evict into a logged
         failure.

- [ ] **Step 4c: add the dispatcher-against-real-SQLite lifecycle test.** Every existing
      `internal/dispatch` test uses `fakeStore`, so nothing yet runs the dispatcher against
      real driver behaviour — type coercion, `_txlock=immediate` semantics, scan errors on
      a NULL, a real constraint violation. Wire `dispatch.New` with `store.New(db)` and run
      Add → tick → mutate → Stop → fresh Dispatcher → Start, asserting the restored queue
      matches what was persisted, in order (plan review, Finding 6).

      It lives in `internal/dispatch/store/` rather than `internal/dispatch/`: the
      dependency runs store → dispatch, and putting it the other way would make
      `internal/dispatch`'s own tests import a SQL driver.

- [ ] **Step 5: add the compile-time interface assertion**

```go
var _ dispatch.Store = (*Store)(nil)
```

- [ ] **Step 6: run gates**

```bash
goimports -w ./internal/dispatch/store/
go vet ./... && go test -count=1 -race ./internal/dispatch/...
golangci-lint run ./internal/dispatch/...
```

- [ ] **Step 7: commit**

```bash
git add internal/dispatch/store/
git commit -m "feat(dispatch): add the SQLite Store implementation"
```

---

## Task 5: gates, sweep, and the roadmap note

**Files:**
- Modify: `internal/dispatch/ports.go` (the `Store` doc comment's "B2.2 implements it"
  sentence, now satisfied)
- Modify: `docs/superpowers/specs/2026-08-28-sched-exported-surface-design.md` (the B2.2
  roadmap row)

- [ ] **Step 1: sweep the claims this change falsified**, from the repository root

```bash
git grep -n 'B2\.2'
git grep -n 'job.Policy{}'
git grep -n 'implements it against SQLite'
```

- [ ] **Step 2: run the whole-repo gates** — none is diff-scoped, so each can fail on a
      file this change did not touch

```bash
go run ./scripts/check_dup_comments
go run ./scripts/check_review_banner
go run ./scripts/check_citations
```
Note `check_citations` scans **tracked** `.go` files, so run it after `git add`.

- [ ] **Step 3: run the full local gate block**

```bash
go fix ./... && goimports -w . && go vet ./...
go test -race ./...
./scripts/run_tests.sh
golangci-lint run ./...
```

- [ ] **Step 4: commit and open the PR**

```bash
git commit -m "docs(dispatch): record B2.2 as landed"
```

---

## Inconclusive / Deferred items

1. **Does `Save` need to be transactional with anything?** Assumed no: it writes one row of
   one table with a single writer, and the dispatcher already tolerates a failed `Save` by
   logging and continuing. **Probe:** read `persistIfChanged`'s error path.
   **Expected branches:** if a failed `Save` must not leave `d.written` updated, the
   existing `markWritten`-after-`Save` ordering already covers it and nothing changes; if
   some caller needs Save+Delete atomicity, Task 4 grows a transaction.

2. **Is `Header.Category` ever legitimately absent?** The schema above defaults it to `''`.
   Assumed a job with no category stores the empty string rather than NULL.
   **Probe:** `git grep -n 'Category' internal/dispatch/`. **Expected branches:** if
   nothing distinguishes "" from unset, the DEFAULT stands; if something does, the column
   becomes nullable and `Header.Category` becomes a pointer, which would be a wider change
   than this task and should be escalated rather than absorbed.

3. **Whether `restore` should sort, or trust `Load`.** ~~Open.~~ **Settled by the plan
   review:** `restore` sorts, with a tiebreak matching `Load`'s `ORDER BY sort_key ASC,
   id ASC`. The redundancy is cheap and the failure it prevents — a Go sort keyed on
   `SortKey` alone diverging non-deterministically from SQLite on a key collision — is
   silent. Keeping the sort also means `Load`'s ordering is a performance property rather
   than a correctness one, which is the safer place for it.
