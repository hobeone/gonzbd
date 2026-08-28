// Package sched holds the scheduling decisions for the job lifecycle: which
// jobs may run, what they are waiting for, and what moves them.
//
// It depends on internal/job and nothing else. Every decision is a function of
// a job.Snapshot — a value, taken under one lock — so no decision can acquire a
// resource as a side effect of being asked. Acquisition happens in exactly one
// place, grantFor.
//
// It does NOT contain the dispatcher, the workers, persistence, or rendering.
// Those are Half B2, which also retires internal/queue. Nothing imports this
// package yet, by design.
//
// # Known obligations left for Half B2
//
// These are open items, not a description of anything this package currently
// exposes. B2 must add doors for them to THIS package — beside reclaim and
// releaseFor, which already own the lease and slot return paths — rather than
// invent a parallel owner in the dispatcher or elsewhere:
//
//   - No public door returns a worker-yielded lease to the Queue. park does
//     this internally, but nothing exported lets a caller outside the
//     package hand back a lease the way a dispatcher's yield handling needs
//     to (see park's own doc comment in advance.go).
//   - park is unexported, though the design spec's traces (§5.1, §5.2) show
//     the dispatcher calling it directly on a worker's yield. B2 needs an
//     exported equivalent, or park itself exported, once a real dispatcher
//     exists to call it.
//   - q.paused has no setter. Nothing in this package can currently flip the
//     global-pause gate gatedBy reads; B2's wiring needs one.
package sched
