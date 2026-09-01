// Package store is the SQLite implementation of dispatch.Store.
//
// It lives beside internal/dispatch rather than inside it so that
// internal/dispatch stays free of a SQL driver: the interface is declared by
// its consumer and satisfied here, the same shape dispatch.Residency and
// dispatch.Runner already have.
//
// It is the only reader or writer of the dispatch_jobs table. That table is
// deliberately separate from `jobs`, which belongs to internal/queue until the
// swap retires it — see the table's own comment block in
// internal/history/migrations/001_initial.sql for the argument.
//
// Nothing in production constructs a Store yet. Plan 2 of
// docs/superpowers/specs/2026-08-25-job-lifecycle-design.md §15 repoints the
// application onto internal/dispatch and is where that happens. (An earlier
// decomposition called that step B2.4; it was withdrawn in favour of §15.)
package store
