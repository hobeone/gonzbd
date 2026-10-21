# Queue Residency Stage 4 — Contract Re-read — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** every sentence in the four contract documents, the architecture overview, the Go standards, the schema comments and the crash suite's account of itself describes the residency model as it is after stages 1–3, with no sentence left describing the model before them.

**Architecture:** this stage changes no code. Stages 1–3 swept the sentences their diffs falsified by grep; this stage is the read that grep cannot do — each document read in full against source by a reader who knows what changed, producing findings (quote, location, why it is false now, proposed sentence) that one implementer then applies. The design's §7 item 4 lists the targets; stages 2 and 3 already handled several (durability §3 and limitation 1, the "one window" section, `reportDownloadComplete`'s comment), so the list here is what remains plus what the re-read finds.

**Tech Stack:** Markdown, Go comments, SQL comments; the four doc gates; `scripts/mutate --check-all`.

**Spec:** `docs/superpowers/specs/2026-10-20-queue-residency-design.md` §7 item 4 (and §4 as the description of the model the docs must now match). Decision log: `docs/superpowers/specs/2026-10-20-queue-residency-decisions.md`, D1–D23; the rulings that changed the design's own text are D12 (`AttachContent` stays), D8 (`NewManifest([]JobFile)` stays), D15 (the prefix sum is computed twice: `FileTable.offsets` and `JobProgress.fileStarts`), D20 (the header-bytes fallback stays for the unrestorable class), D23 (a paused job is verified on resume or rename).

## What changed in stages 1–3 (the reader's checklist)

A sentence is stale if it says any of the following, which were true before and are false now:

- The manifest carries per-file structure (subject, bytes, date, article count, is_par2). Now: the manifest file is a flat `{"articles":[...]}`; `job_files` (`subject`, `bytes`, `article_count`, `complete`, `filename`, `fetch_policy`) is the per-file structure; `FileTable` (`internal/job/filetable.go`) is its in-memory form; `Manifest.Files()` points at the job's own table.
- A restored job is header-only until its first hydration; progress exists only after a manifest load. Now: `Application.restoreProgress` (`internal/app/restore_progress.go`) attaches a `FileTable` and a seeded `JobProgress` to every registered job whose `job_files` rows are readable, before the first tick; a job whose rows are missing or gapped is settled Failed (`Dispatcher.SettleFailed`) if it has an open attempt, else left table-less until the tick fails it (D20).
- Progress after a restart is zero / the NZB total until verified. Now: an unverified file's downloaded bytes are the estimate `Bytes × Recorded / ArticleCount` (`fileBytesDownloaded`); `FileProgress.Verified` and `Recorded`; `InstallFileVerification` is the only writer of `Verified = true` after construction; the API reports `progress_verified` and per-file `verified`; the UI shows an outlined bar and a hint.
- Hydration decides what to do by whether the job has progress / a table. Now: `Hydrate` requires a table (D18), and takes the articles-only branch iff `Job.AllVerified()` (D13); a seeded job's first hydration always verifies every file, `complete=1` and row-less ones included.
- `AttachContent` installs progress / `RestoreContent` exists / `SetRecoveryBytes` / `dispatch_jobs.recovery_bytes` / `describesSameJobAs` / `JobProgress.fileStarts` is "the" range source. Now: `AttachFiles(ft, seed)` is the sole writer of `files` and `progress`; `AttachArticles(m)` of `manifest`; `AttachContent(m)` is the composite for ingest (seed nil); `RestoreContent`, `SetRecoveryBytes`, `recovery_bytes`, `describesSameJobAs` are gone; recovery bytes are derived from the table at attach; `fileStarts` still exists beside `FileTable.offsets` (D15).
- `verifyPausedJobs` verifies paused jobs after `Start`; a job restored paused stays resident; limitation 1 has a startup paragraph. Now: deleted (stage 3); a job restored paused at `Fetching` keeps the estimate until resumed, or until a rename's `LoadProgress` hydrates it (D23); the loads outside the residency rule are `SetName`'s `LoadProgress` and `fileOwedUnwantedFailures`' direct `Hydrate`.
- `seedJobFiles`/`Admit` writes only subject rows; `job_files.fetch_policy`'s `CHECK` is its only range guard. Now: `Admit` writes the widened rows in one transaction; `seedRowOf` (`internal/durability/progress.go`) range-checks every integer column on the restore read, so the `CHECK` is not the only guard.

Things that did NOT change and must not be "corrected": durability §3 (a Done bit after a restart exists only from a verified row or a `complete=1` file; the seed sets none); the residency rule (hydrated when the job holds what its position requires; a paused job keeps a manifest it already has); the recorder as the record's one writer; the header-bytes fallback for a job with no progress (D20).

## Global Constraints

- No production code changes. Go comments and SQL comments are in scope; `.go` bodies, `.svelte`, `.ts` are not. If a finding needs a code change, it is recorded in the PR body as a follow-up, not made.
- Standing Rule 4: every "only/sole/every/never/always" written or rewritten names its check (grep + count, or the branch); narrowing a referent must not broaden a scope; removing a term from a stated formula recomputes the rest.
- `docs/superpowers/` is not edited except to append to the decision log (D24+ if a ruling is made) and to add `doccite:ok` markers.
- Each commit passes `go run ./scripts/check_doc_citations && go run ./scripts/check_citations && go run ./scripts/check_dup_comments && go run ./scripts/check_review_banner && go run ./scripts/mutate --check-all` (`check_citations` after `git add`, it reads tracked files).
- Worktree `.claude/worktrees/queue-residency-stage4`, branch `worktree-queue-residency-stage4`, based on `origin/feat/queue-residency-stage3` (PR #1035) at 24a0283a. Push refspec `HEAD:docs/queue-residency-stage4`. PR base `feat/queue-residency-stage3` until #1035 merges.

## Review Focus

1. A sentence that is now true only on one branch (a resumed job vs a renamed one; a job with readable rows vs the unrestorable class) and names neither. Task 2 names the branch.
2. A table row or list that lost or gained a member without the prose around it changing (the attach-path list, the "who hydrates first" list, the loads-outside-the-rule count, the invariants list at the end of the durability contract). Task 1 auditors re-count each against source.
3. A figure (bytes per article, per file, per job) that stage 2 measured but the text still calls derived, or vice versa. Task 1B checks §13 against the stage-2 measurement recorded in PR #1034's body and `FileProgress`'s layout.
4. The crash suite's own account (`docs/durability-contract.md` § "What the crash suite actually pins", `docs/TESTING.md` §3a) describing a restart path that no longer exists (e.g. a manifest with file structure, a header-only restore). Task 1D.
5. A `check_citations` count that still agrees only because the comment's grep pattern no longer means what the prose says (e.g. the `job_files` statement count after `SeedRows` was added). Task 1C re-reads each cited grep's meaning, not only its count.

---

### Task 1: Four read-only audits, in parallel

Each auditor reads its documents in full against the current source and writes a findings file; none edits anything. Findings are one block each: **Quote** (verbatim), **Where** (`file:line`), **Why false now** (with the source `file:line` that shows it), **Proposed** (the replacement sentence, or "delete"), **Confidence** (certain / probable / check). A reader who finds nothing in a section says so by section heading, so an empty section is a check performed, not skipped.

- **1A — `docs/durability-contract.md`**, whole file, with attention to §3, §6, "Hand-over to post-processing", "Clean shutdown", "Stalls and their re-evaluation", "Accepted limitations" (the `setNameLoadTimeout` mention stage 3 deleted belongs somewhere: limitation 1 or the `SetName` sentence in §3's attach-path list), "What the crash suite actually pins", "Invariants". Output: `.superpowers/sdd/2026-10-20-queue-residency-stage4/audit-1A.md`.
- **1B — `docs/job-lifecycle.md` §11–§14** (lines from "## 11." to the end of "## 14."), with attention to the three tiers, "Always resident, and the interval before the restore pass", "Residency is bounded by what a job holds", "Who writes what", "Article resolution is derived, not stored", "Verification at the first hydration", §13 "Memory" (the `FileProgress` 88 B figure was measured with `unsafe.Sizeof` in stage 2; the table says derived), "Terminal compaction". Output: `audit-1B.md`.
- **1C — `docs/dispatch-contract.md`** (whole), **`docs/ARCHITECTURE.md`** (the Compiler-Enforced Residency and Article Addressing bullets, the startup sequence, any paragraph naming manifests, `job_files`, restore or verification), **`docs/go-standards.md`** (the persistence/queue-state bullet near line 53, the per-job locking bullet near line 46, and the lessons near lines 370–400: `newJobProgress(ft)` is now `newJobProgress(ft, seed)`; "both are reached from `appResidency.Hydrate`" is false — `AttachFiles` is reached from `restoreProgress` and from `AttachContent` in ingest; re-derive from `git grep -n '\.AttachFiles(\|\.AttachArticles(' -- '*.go' ':!*_test.go'`), **`internal/history/migrations/001_initial.sql`** (every comment on `job_files`, `written_articles`, `dispatch_jobs`; the "fetch_policy CHECK is the only guard" sentence is false since `seedRowOf`; re-run each embedded grep and judge whether its pattern still means what the prose says). Output: `audit-1C.md`.
- **1D — `test/crash/` against durability §3** and **`docs/TESTING.md` §3a**: read `test/crash/*.go` and the testdata; for each test say which §3 sentence it pins and whether its fixture or assertions assume a pre-redesign shape (a manifest with file entries, a header-only restore, `recovery_bytes`, a verifier after `Start`). The design predicts "no change"; confirm or refute with `file:line`. Also read `docs/TESTING.md` §3a's per-test table against the tests that exist. Output: `audit-1D.md`.

- [ ] Step 1: dispatch 1A–1D in one message (read-only, Sonnet). Each gets: this plan's "What changed" section as its checklist, its file list, the output path, the findings format.
- [ ] Step 2: controller triages every finding (apply / reject with reason / defer to follow-up) into `.superpowers/sdd/2026-10-20-queue-residency-stage4/triage.md`, and records any ruling in the decision log (D24+). <!-- doccite:ok .superpowers/sdd/2026-10-20-queue-residency-stage4/triage.md — gitignored session scratch, written during execution -->

### Task 2: Apply the triaged findings

**Files:** whatever `triage.md` lists as apply; one commit per document group (durability; job-lifecycle; dispatch+ARCHITECTURE+go-standards; schema comments; TESTING/crash if any).

- [ ] Step 1: for each finding, re-read the surrounding paragraph and apply the proposed sentence or a better one that says what holds now and names its branch; for any rewritten universal run the grep and state the count.
- [ ] Step 2: after each group, the gate block from Global Constraints; commit `docs(<area>): <what claim changed>` with a one-line-per-sentence body.
- [ ] Step 3: `git grep` the distinctive phrase of every rewritten claim from the root to find its copies outside the audited files (`cmd/`, `test/`, `ui/`, `AGENTS.md`).

### Task 3: Gates and review

- [ ] Step 1: `go build ./... && go vet ./... && go vet -tags=integration,uitest,crash ./...` (comments only changed, so this proves nothing broke the build), the five doc/spec gates, `go run ./scripts/check_test_doubles --all`.
- [ ] Step 2: `./scripts/run_tests.sh` once on the final tree (docs-only, but it is the gate of record and `check_citations` runs inside it).
- [ ] Step 3: whole-branch review of the diff against this plan's "What changed" checklist and Review Focus; a docs-only diff takes a Sonnet reviewer with the checklist, escalating to Opus only if a finding disputes what the code does.
