// Package sched holds the scheduling decisions for the job lifecycle: which
// jobs may run, what they are waiting for, and what moves them.
//
// It depends on internal/job and nothing else. internal/job's own dependency
// set widened when the manifest and progress tiers moved into it — it now
// reaches internal/nzb for one pure Message-ID predicate, alongside
// internal/constants — which changes what this package transitively pulls in
// but not what it may name. Every decision is a function of
// a job.Snapshot — a value, taken under one lock — so no decision can acquire a
// resource as a side effect of being asked. Acquisition happens in exactly one
// place, grantFor.
//
// It does NOT contain the dispatcher, the workers, or persistence. Those are
// Half B2, which also retires internal/queue. It does hold the two rendering
// doors, Render and RenderAll (see below) — composing the view a caller
// renders from is a decision over a job.Snapshot like any other, and B2
// supplies the HTTP layer that calls them, not the composition itself.
//
// internal/dispatch (Half B2.3) is this package's first caller: it
// constructs the one *Queue a process runs (sched.New) and drives every job
// through Advance, Cancel, Park, Retry, Settle, Pause, Resume, Paused, Render
// and RenderAll. `git grep -n '"github.com/hobeone/gonzbd/internal/sched"'
// -- '*.go' ':!internal/sched/*' | grep -v _test.go` returns exactly one
// line, dispatch.go's import — the two dispatch test files import it too,
// which the filter drops.
//
// # What this package exports, and what B2 still owes it
//
// `grep -n '^func (q \*Queue) [A-Z]' internal/sched/*.go | grep -v
// _test.go` finds ten lines: Advance, Cancel, Park, Retry and Settle
// (advance.go, cancel.go, settle.go) write or gate; Pause and Resume (queue.go)
// write the pause flag; Paused (queue.go) reads it back; and Render and
// RenderAll (render.go) are the doors that compose a job.RenderView.
// Acquisition happens only in grantFor; return happens only through reclaim
// and releaseFor, which Settle, Park and Cancel all route through.
//
// Half B2 owed this package two things it could not supply for itself. One is
// closed:
//
//   - CLOSED (B2.3). A discard path for a cancelled job that never ran.
//     finishCancel returns nil for one, because Outcome lives on the Attempt
//     and there is none. The job therefore survives here, and Render reports
//     it as not running with a NoLease reason — which job.ToSABnzbd turns
//     into StatusQueued. This package still cannot close that gap itself
//     (D-B5 keeps residency and the store out of it), but internal/dispatch's
//     tick now does: evictCancelledNeverRun (internal/dispatch/tick.go)
//     removes StateUnset && IntentCancel from the registry and the store
//     immediately after Advance, on every tick.
//
//     Note this also bounds gatedBy's stated reason for ignoring IntentCancel
//     ("advance handles it first, so no cancel value reaches the render
//     path"): true for every job that has run, false for one that has not —
//     except between a Park and the next tick. A job cancelled while running
//     and then Parked rather than Settled (teardown, shutdown) sits open at
//     Fetching with no lease and IntentCancel: Render reports it not settled
//     and not running, gatedBy ignores IntentCancel as documented, and it
//     falls through to NoLease → StatusQueued until the next Advance routes
//     it through finishCancel. Transient and self-healing, but real for the
//     tick it lasts.
//
//   - STILL OWED. A Workers implementation whose Abort neither blocks nor
//     takes a lock a caller could hold across a call into Queue. See the
//     Workers interface. `git grep -n ') Abor[t](jobID string)' -- '*.go'`
//     (the bracket keeps this citation from matching its own quoted text)
//     finds two hits, both named stubWorkers and both in _test.go files
//     (internal/dispatch/fakes_test.go, internal/sched/queue_test.go); no
//     non-test file declares an Abort method at all.
package sched
