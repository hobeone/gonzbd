# External LLM review — `internal/job`

Scope: `internal/job/` at the close of the job-lifecycle-core plan.
Repo state at review: `design/job-lifecycle-state-machine` @ `54d5fb43`, working tree clean.

> **Frozen record.** This is a dated snapshot of two independent external
> reviews of `internal/job` at `54d5fb43`, not a living contract. It is kept
> unedited as a record of what was found and how it was resolved. Where it
> disagrees with the code today, the code wins. Both findings below were fixed
> on this branch, so the code it describes as defective is already gone — the
> value here is the *shape* of what two outside readers caught that ten
> in-process reviews did not.

## Why this exists

The package was built by ten sequential subagent tasks, each with its own
spec-compliance and quality review, followed by a whole-branch review — eleven
review passes in total, all sharing the plan author's framing. Two external
models were then given the finished package with no knowledge of that process.

Both found real defects. Both defects were in the same blind spot, and it was
not a blind spot any of the eleven internal passes could have covered, because
they inherited the assumption that produced it.

## The blind spot

**Every escape was found one layer up from where the invariant was enforced.**

The one-way boundary is enforced on `Attempt`. It was verified there
exhaustively — a throwaway probe walked every 1-, 2- and 3-step path out of
`Extracting` using both doors to every state, 2744 paths, zero escapes. That
probe was the strongest evidence produced during construction and it was
scoped to exactly the layer where the invariant already held.

- Gemini 3.1 Pro (High) asked what `Job` does with a *list* of attempts, and
  found `BeginAttempt` appending a fresh one to a job that had already
  crossed — walking the whole job back into Correctness.
- Gemini 3.7 Flash (High) asked what could change `a.state` out from under a
  check that reads it, and found `hold` doing exactly that — setting
  `Waiting` so `finish`'s `IsProduction(a.state)` guard stopped seeing a
  crossed attempt.

Neither is visible as an illegal edge. Both are visible only by asking what
sits one level above, or forty lines away, from the check.

## Finding 1 — the boundary held within an attempt, not across attempts

Reported by Gemini 3.1 Pro (High). Severity: Critical.

`Job.BeginAttempt` appended a new attempt to any job without an open one.
A job that crossed into `Extracting`, failed, and settled `OutcomeFailed`
could therefore start a fresh attempt at `Fetching`.

Confirmed by probe before acceptance:

```
attempt 1 reached Extracting (IsProduction=true)
BeginAttempt after crossing: err=<nil> -> attempts=2 state=Fetching next=Waiting
```

The spec settles that this is a defect rather than a design choice, in its own
rationale for D3: crossing means "the inputs a later attempt would need are
consumed. Not crossing keeps the job retryable." D8 adds that re-adding the
NZB — a new job — is how a user asks for a full redo.

**Resolved** by latching `Attempt.crossed` when `transition` arrives in a
Production state, and refusing `BeginAttempt` with `ErrBoundaryConsumed`
once a prior attempt crossed. Holding *toward* Production does not latch; only
arrival does.

## Finding 2 — the refusal read a transient field instead of the latch

Reported by Gemini 3.7 Flash (High). Severity: Important. Found in the fix for
Finding 1.

`finish` refused `OutcomeUnrecoverable` past the boundary by testing
`IsProduction(a.state)`. But `hold` writes `a.state = Waiting`, so a
crossed-then-paused attempt slipped past:

```
state=Extracting   crossed=true   IsProduction(state)=true
after hold:        state=Waiting  crossed=true  IsProduction(state)=false
finish(Unrecoverable) while held in Production: err=<nil>
```

The result was a job contradicting itself: its outcome said
`Unrecoverable` — which per D3 means never crossed and still retryable —
while `BeginAttempt` refused a retry with `ErrBoundaryConsumed`, because
`crossed` was latched.

**Resolved** by guarding on `a.crossed` rather than `IsProduction(a.state)`.

Note the remedy that was *not* taken. The reviewer proposed
`a.crossed || IsProduction(a.state)`. The second disjunct would have been
dead code: `crossed` latches inside `transition`, and `transition` is the
only writer that can produce a Production state (`newAttempt` starts at
`Fetching`; `hold` only ever writes `Waiting`), so
`IsProduction(a.state)` structurally implies `a.crossed`. Adding it would
have introduced a sixth unreachable guard into a package that had already
shipped five. Right diagnosis, wrong prescription — which is the general
lesson about acting on any reviewer's suggested fix rather than its finding.

## Also reported, accepted as accurate

- `TestOnlyAssessingBranchesWithinCorrectness` skips non-Correctness states
  outright, while `doc.go` claims `Assessing` is the only branching state
  globally. True today, unenforced for Production states. (Pro)
- The standing constants-import test counts occurrences of
  `internal/constants` but never asserts the absence of *other* repository
  imports, which is half of what `doc.go` claims. (Pro)
- The `started`/`ended` doc comment named one test reader when two exist.
  (Flash) — fixed.
- The `crossed` field's "BeginAttempt is the only reader" claim cited no
  enumeration. (Flash, on the boundary fix) — fixed.

## Tooling notes

Both runs used the local `agy` CLI against the working tree, with no GitHub
round-trip. `GEMINI.md` is a symlink to `AGENTS.md`, so both models
reviewed against this repository's Standing Design Rules without being taught
them.

Pro's `file:line` citations were diff offsets rather than file lines and did
not resolve; Flash's were accurate and anchored. That is the reverse of what
model tier alone would predict, and it is worth checking rather than assuming
on any future run.
