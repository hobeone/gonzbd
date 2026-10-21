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
