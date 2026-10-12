// Package dispatch drives internal/sched. It owns the three things sched
// deliberately does not have (D-B5): the job registry in queue order, manifest
// residency, and the loop.
//
// The loop is the owner of liveness (D-B7). A ticker walks the registry and
// calls sched.Queue.Advance on each job; the kick is a latency optimisation
// that must remain deletable without changing what the system computes. The
// legacy queue implementation this replaced made promotion event-driven,
// which put nine call sites in charge of one condition, where forgetting one
// yielded a job that was eligible, unblocked and never started, silently.
//
// The dispatcher also launches every worker and observes every exit (D-B14).
// That is not bookkeeping: the Queue cannot distinguish "holding and working"
// from "holding and yielded", so without an exit path a cancelled job's worker
// is aborted, surrenders nothing, and finishCancel re-aborts it every tick
// forever while the job never settles.
//
// Its non-test files import internal/sched, internal/job, internal/constants,
// internal/keyedlatch, internal/unwanted and the standard library. It must not
// import internal/downloader or internal/postproc. The one edge between them
// points the other way: internal/downloader's non-test files import dispatch,
// and internal/postproc's do not import it.
package dispatch
