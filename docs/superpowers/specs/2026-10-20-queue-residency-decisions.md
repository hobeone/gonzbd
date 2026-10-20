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
