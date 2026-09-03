// Package store is the SQLite implementation of checkpoint.Store.
//
// It lives beside internal/checkpoint rather than inside it so that
// internal/checkpoint stays free of a SQL driver: the interface is declared by
// its consumer and satisfied here, which is the same shape internal/dispatch
// and its own store package already have.
//
// It writes the job_files table and nothing else. That table already exists —
// internal/history/migrations/001_initial.sql declares it — so no migration
// accompanies this package; what changes is which code writes those columns,
// not what the columns are.
//
// # What it deliberately does not write
//
// A job.Checkpoint carries an ID, the two lifecycle axes (State, Intent) and
// the progress record. The two axes are internal/dispatch's to persist, into
// dispatch_jobs, and are written there — this package must not write them a
// second time, which would be two owners for one fact.
//
// The job-level figures a JobProgress also holds — the download stamps and the
// par2 release reason — have no column in either table on this branch. They
// lived on the `jobs` row, which belongs to internal/queue until the swap
// retires it. They are not written here, and a home for them is outstanding
// work rather than a decision this package makes.
package store
