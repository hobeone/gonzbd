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
package sched
