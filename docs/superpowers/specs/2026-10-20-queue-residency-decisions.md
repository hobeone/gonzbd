# Queue residency — decision log

Judgement calls made while implementing
[`2026-10-20-queue-residency-design.md`](2026-10-20-queue-residency-design.md),
recorded for the owner to review. Each entry: the question, the call, why,
and what it costs if wrong. Entries marked **owner** were the owner's own
rulings; the rest were made autonomously while the owner was away.

## 2026-10-20

### D1 — Withdraw the narrow fix (**owner**)

The morning's `fix(app): load jobs restored paused before they ever ran`
(local commit `5352b3fa`, never pushed) is dropped rather than landed
first. The design's stage 3 deletes `verifyPausedJobs` outright, and the
owner judged the bug not urgent enough to carry an interim change.
Branch deleted; nothing on `origin` references it.

### D2 — Design document lands as its own PR

**Call:** the approved design and this log go in a `docs/` PR before stage 1,
rather than riding stage 1's branch.
**Why:** the owner reviews the design on its own diff; stage PRs can cite a
merged path instead of a sibling branch.
**Cost:** one extra PR to merge; none if the design is what was approved.

### D3 — Decision log is a separate file, committed with each stage

**Call:** this file, beside the design, appended by every stage PR that
makes a call. PR bodies summarise and link; the file is the record.
**Why:** one place to review; the design document stays the approved text
rather than growing a changelog.
**Cost:** a merge conflict on this file between parallel stages — avoided
by running the stages serially, which their dependencies require anyway.

### D4 — PRs are filed without the show-for-approval pause

**Call:** each PR body still goes through the math scan and the cold-reader
audit, but is filed without waiting for the owner to read the draft.
**Why:** the owner asked for PRs to be opened autonomously while away; the
PR itself is the review surface and can be edited.
**Cost:** a body the owner would have reworded ships as filed; `gh pr edit`
fixes it.

## 2026-10-20 — stage 1

### D5 — `is_par2` is not a column

**Call:** `job_files` carries `subject`, `bytes` and `article_count` only;
`newFileTable` computes the recovery-volume flag.
**Why:** `IsRecoveryVolume(subject)` is a pure function of `subject` (its
regex requires `.par2`), so storing the flag would be a persisted derived
value.
**Cost:** one regex match per file when a table is built; none if the
predicate stays pure.

### D6 — `date` dropped from `JobFile` and the manifest JSON

**Call:** the field is deleted rather than carried.
**Why:** `FileDate` has no reader (`git grep -n '\.FileDate(' -- '*.go'
':!*_test.go'` returns 0 lines on 2026-10-20).
**Cost:** a future reader of the posting date must re-add the field.

### D7 — `Manifest` forwards `NumFiles()` and `FileRange()`; content accessors move

**Call:** `Manifest` keeps `NumFiles()` and `FileRange()` as forwarders to
its file table; the content accessors (`FileSubject`, `FileBytes`,
`FileIsPar2Recovery`, `TotalBytes`, `RecoveryBytes`, `RecoveryFiles`) move
to `FileTable`, reached as `m.Files()` or `j.Files()`.
**Why:** `FileRange`/`NumFiles` have ~25 internal callers that hold a
`*Manifest` for article work; the content accessors are what callers hold a
manifest only to read.
**Cost:** two spellings for file structure until callers settle.

### D8 — Constructor shape

**Call:** `NewManifestFromArticles(ft, articles) (*Manifest, error)` is the
constructor; `NewManifest([]JobFile) *Manifest` keeps its name and signature
as the one wrapper over `NewFileTable`+`NewManifestFromArticles` for ingest
and tests; `AttachArticles` refuses a manifest whose `Files()` is not the
job's own `FileTable` pointer.
**Why:** one constructor owns the derived state, and the pointer check makes
a mismatched table unattachable.
**Cost:** none beyond the design's `BuildContent` name not being used.

### D9 — `SetRecoveryBytes` and `dispatch_jobs.recovery_bytes` stay until stage 2

**Call:** both remain through stage 1.
**Why:** deleting them now would make a restored job report 0 recovery bytes
before hydration, a behaviour change stage 1 must not make.
**Cost:** a derived value stays persisted for one more stage.
Closed by stage 2 (Task 4).

### D10 — Stage 1 is executed subagent-driven, implementers on Sonnet

**Call:** subagent-driven execution in a worktree.
**Why:** nine tasks whose interfaces depend on each other, and a mistake
lands in the durability core.
**Cost:** a fresh context per task and per review.

### D11 — The stage-1 PR stacks on PR #1030

**Call:** base `docs/queue-residency-design`, retargeted to `main` when
#1030 merges.
**Why:** the decision log it appends to exists only on that branch.
**Cost:** a stacked PR the owner must merge in order.

### D12 — `AttachContent(m)` stays as the composite door

**Call:** `AttachContent(m)` stays (`AttachFiles(m.Files())` then
`AttachArticles(m)`); `RestoreContent` is deleted in favour of
`AttachArticles`. The design (§5) said to delete both.
**Why:** the enumeration tests pin `AttachFiles` and `AttachArticles` as the
sole writers of their fields, which is the owner property the design wants;
the composite wrapper writes nothing itself. ~90 test sites and the ingest
path call it, and churning them buys nothing.
**Cost if wrong:** one exported composite function the design said to delete.

### D13 — Nothing attaches before the record verifies

**Call:** `Hydrate` builds the `FileTable` locally and `verifyAndAttach`
calls `AttachFiles` then `AttachArticles` only after verification and
commit. The stage-1 plan had `AttachFiles` run before `verifyJobFiles` and
chose the articles-only branch on "the job already had progress".
**Why:** a hydration that faults after attaching the table would leave the
job with a table and progress, so the next `Hydrate` would take the
articles-only branch and never verify — `docs/durability-contract.md` §3
broken. Pinned by `TestVerifyAndAttach_MissingDirectoryParksTheJob` and
`TestHydrate_CancelledDuringArticleReadAttachesNothing`.
**Consequence for stage 2:** once the startup pass attaches a table to every
queued job, "has a table" no longer means "ran in this process"; the
articles-only branch must key on the Verified flag, not on `Files() != nil`.
Realised in stage 2: `Hydrate` branches on `AllVerified()` (D18).
**Cost if wrong:** none for stage 1; behaviour matches the former
`RestoreContent` / `AttachContent` split.

### D14 — The queue API lists files for a job evicted within this process

**Call:** `api.firstIncompleteFile` and `api.buildQueueFiles` read
`j.Files()` instead of the fallible `j.Manifest()`. `Job.Evict` keeps the
table, so an evicted job now reports its file list where it reported
nothing. The only visible behaviour change in stage 1.
**Why:** the table is the always-resident tier the design asks for, and
reading file structure must not depend on manifest residency.
`screenUnwanted` moved the same way but both production callers run right
after `BuildIngestJob`, so its change is unreachable.
**Cost if wrong:** a `j.Resident()` gate at the two API sites restores the
old output.

### D15 — The article prefix sum is computed twice

**Call:** `newJobProgressSized` (`internal/job/progress.go`) rebuilds the
article prefix sum into `JobProgress.fileStarts` that `newFileTable`
(`internal/job/filetable.go`) already owns. Stage 1 leaves the duplicate;
the field comment no longer calls it the table's own sum.
**Why:** design §5 defers sharing one slice between the two tiers. Both are
built from the same per-file article counts in one pass each, so they cannot
disagree until one of them is changed.
**Cost if wrong:** a second computation of one figure, against Standing Design
Rule 2. If a change makes the two diverge, `fileRange` and `FileTable.Range`
disagree on a file's span; sharing the table's slice removes the second
computation.

### D16 — A retry keeps the first parse's `job_files` structure

**Call:** `Store.Admit` keeps `ON CONFLICT DO NOTHING`, so a retry's
re-seed leaves the subject, bytes and article count the first parse wrote.
The assumption is stated at `Admit` and in `docs/job-lifecycle.md` §14, not
enforced.
**Why:** within one build, re-parsing the backed-up NZB yields the same
structure, so the rows cannot disagree with the table. The only state that
would make `DO UPDATE` necessary is a parser change across builds, which
Standing Rule 1 waives.
**Cost if wrong:** one SQL clause. Stage 2, which makes `job_files` the
restore source for every queued job, revisits this.

### D17 — `AttachFiles(ft, seed)`

**Call:** the door gains a `[]FileSeed` parameter. `nil` is ingest and the
composite door (every file Verified); non-nil is the restore pass.
**Why:** the design wrote this signature; stage 1 landed the one-argument form
so each commit built; one door keeps one writer.
**Cost if wrong:** every `AttachFiles(` caller gains `, nil`.

### D18 — `Hydrate` requires an attached table

**Call:** after the restore pass every registered job has a table (no store
means no restored job), so the stage-1 `fileTableFromRows` branch inside
`Hydrate` is unreachable and becomes an error. `fileTableFromRows` moves to
the restore pass; the articles-only branch keys on `AllVerified()` (D13).
**Why:** Standing Rule 1: a guard whose only justification is a state that
cannot occur is deleted.
**Cost if wrong:** tests that hydrate a bare job must seed a table first.

### D19 — The restore read is its own row type

**Call:** `durability.SeedRow{FileRow; Recorded int}` from one query over every
job; `FileRow`/`FileRows` are unchanged.
**Why:** a field meaningful only on one read path must not sit on the shared
type.
**Cost if wrong:** one small type.

### D20 — The header-bytes fallback stays, narrowed, for the unrestorable class

**Call:** `Dispatcher.List`, `rowFor` and `api.buildSlot` keep
`if j.HasProgress() { rem = j.RemainingBytes() }` over the header's NZB total.
The design (§4.4, §5) said to delete it; only `recovery_bytes` and
`SetRecoveryBytes` go.
**Why:** a registered job lacks progress only when the restore pass could not
rebuild its structure (`failUnrestorable`, `internal/app/restore_progress.go`)
or before that pass has run. Deleting the fallback would list such a job with
zero remaining bytes, i.e. as finished; the header's total is the honest
figure. `Job.RecoveryBytes()` on a job with no table returns 0, and that is
what the API's `recovery_bytes` shows for it.
**Cost if wrong:** one branch that a stricter reading would have removed.

## 2026-10-20 — stage 3

### D21 — Stage 3's scope is the verifier alone

**Call:** stage 3 deletes `verifyPausedJobs`, `defaultPausedVerifyTimeout`, `pausedVerifyTimeout`, `pausedVerifiedHook`, `paused_verify_test.go`, `testdata/paused_verify.spec`, `testdata/paused_verify_stop.spec` (which §5 does not list) and the paused-verify mutation in `testdata/loose_record_waits.spec`. Nothing else in design §5 remains: `SetRecoveryBytes`, `RestoreContent`, `describesSameJobAs`, `dispatch_jobs.recovery_bytes`, `manifestFile` and `manifestJSONFile` are already gone; `AttachContent` stays by D12; `JobFile` and `NewManifest([]JobFile)` stay by D8 as the ingest shape.
**Why:** design §7 item 3 says "everything in §5 not yet removed"; re-derived from source on 2026-10-20, that set is the first bullet.
**Cost if wrong:** a §5 item nobody re-checked survives into stage 4's sweep, where the contract re-read would find it.

### D22 — The prose sweep is stage 3's, not stage 4's

**Call:** every sentence in code comments and `docs/` that describes the verifier — including the second paragraph of `docs/durability-contract.md`'s Accepted limitation 1 — is deleted or rewritten in stage 3's PR. Stage 4 becomes the holistic re-read of the four contracts for paraphrase and structure.
**Why:** design §7 item 4 listed "limitation 1 (deleted)" as stage-4 work, but the commit cycle's Step 4 requires the sentences a change falsifies to land with the change.
**Cost if wrong:** stage 4 has less to do; nothing is lost.

### D23 — A job restored paused at `Fetching` is verified on resume, not at startup

**Call:** after a restart such a job reports the recorded-count estimate with the "progress from the download record" badge until the user resumes or renames it (a rename hydrates it through `Dispatcher.SetName`'s `LoadProgress`, after which it stays resident until resumed), and holds no manifest until then.
**Why:** the verifier's purpose was to make a paused job's progress visible without a resume (design §4.7); stage 2's restore pass does that for every job. Not holding a manifest per restored paused job also saves about 1.6 MB per 20k articles (`docs/job-lifecycle.md` §13).
**Cost if wrong:** the owner wanted the verified figure to arrive without a resume; design §9 records that a background verification pass remains a pure addition.
