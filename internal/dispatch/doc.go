// Package dispatch drives internal/sched. It owns the three things sched
// deliberately does not have (D-B5): the job registry in queue order, manifest
// residency, and the loop.
//
// The loop is the owner of liveness (D-B7). A ticker walks the registry and
// calls sched.Queue.Advance on each job; the kick is a latency optimisation
// that must remain deletable without changing what the system computes. The
// package this replaces made promotion event-driven, which put nine call sites
// in charge of one condition — `grep -n 'q\.PromoteNext(' internal/queue/queue.go`
// returns 9 lines — where forgetting one yields a job that is eligible, unblocked
// and never starts, silently.
//
// The dispatcher also launches every worker and observes every exit (D-B14).
// That is not bookkeeping: the Queue cannot distinguish "holding and working"
// from "holding and yielded", so without an exit path a cancelled job's worker
// is aborted, surrenders nothing, and finishCancel re-aborts it every tick
// forever while the job never settles.
//
// It imports internal/sched, internal/job and the standard library. It must not
// import internal/queue, internal/downloader or internal/postproc: the
// dependency points from B2 to B1, never the reverse.
//
// # What this package does not have yet
//
// No application code imports it. The one importer is internal/dispatch/store,
// which exists only to satisfy this package's own Store interface and is itself
// unwired: `git grep -l 'gonzbd/internal/dispatc[h]"' -- '*.go' ':!*_test.go'`
// returns 1 file. The bracket around the last letter is deliberate — it makes
// the pattern match a real import line while NOT matching this sentence, and a
// citation that counts its own prose is worse than none.
// Plan 2 of docs/superpowers/specs/2026-08-25-job-lifecycle-design.md §15 —
// "the swap" — repoints production onto it. That plan moves Manifest and
// JobProgress into internal/job rather than deleting internal/queue outright;
// what remains of internal/queue after the move is what the swap deletes.
// (An earlier decomposition called this step B2.4 and had it delete
// internal/queue wholesale. It was withdrawn in favour of §15, which it
// contradicted without citing.)
//
// Row carries a Header supplied at Add rather than byte and article progress,
// because internal/job.Job holds only id, name and policy — the progress tier
// is still in internal/queue until that move.
package dispatch
