package sched

import "github.com/hobeone/gonzbd/internal/job"

// needsLease and needsSlot are design §3.4's resource table. They answer about
// a POSITION, never about an attempt: settledness must be checked by the caller
// first, because a settled attempt keeps the position it settled at and would
// otherwise be asked to hold a lease it no longer needs.
//
// The table lives here rather than in internal/job because the Queue is its
// only consumer (spec §8, open question 2). A table in internal/job with no
// in-package caller would be a second place to maintain one fact.
func needsLease(s job.State) bool {
	return s == job.Fetching || s == job.Assessing || s == job.Repairing
}

// needsSlot reports whether s needs a compute slot from pool B. Fetching does
// not: it is network-bound and its concurrency is pool A's business.
func needsSlot(s job.State) bool {
	return s == job.Assessing || s == job.Repairing || s == job.Extracting || s == job.Finalizing
}
