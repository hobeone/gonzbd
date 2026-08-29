// Package sched holds the scheduling decisions for the job lifecycle: which
// jobs may run, what they are waiting for, and what moves them.
//
// It depends on internal/job and nothing else. Every decision is a function of
// a job.Snapshot — a value, taken under one lock — so no decision can acquire a
// resource as a side effect of being asked. Acquisition happens in exactly one
// place, grantFor.
//
// It does NOT contain the dispatcher, the workers, or persistence. Those are
// Half B2, which also retires internal/queue. It does hold one rendering
// door, Render (see below) — composing the view a caller renders from is a
// decision over a job.Snapshot like any other, and B2 supplies the HTTP
// layer that calls it, not the composition itself. Nothing imports this
// package yet, by design.
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
// Half B2 still owes this package two things, neither of which it can supply
// for itself:
//
//   - A discard path for a cancelled job that never ran. finishCancel returns
//     nil for one, because Outcome lives on the Attempt and there is none. The
//     job therefore survives, and Render reports it as not running with a
//     NoLease reason — which job.ToSABnzbd turns into StatusQueued. A job the
//     user deleted renders as queued, forever. Closing it needs residency and
//     a store, which D-B5 keeps out of this package: B2's dispatcher must
//     evict StateUnset && IntentCancel from the active set and the store.
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
//   - A Workers implementation whose Abort neither blocks nor takes a lock a
//     caller could hold across a call into Queue. See the Workers interface.
package sched
