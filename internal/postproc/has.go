package postproc

import (
	"slices"

	"github.com/hobeone/gonzbd/internal/job"
)

// HasJob reports whether j, this instance rather than any job with its ID, is
// either pending in the queue or currently being processed by the worker. It
// stops reporting a job when the worker clears its busy marker, which is
// before OnJobDone or OnJobCancelled runs, so it cannot serve as a gate
// against handing the job over twice.
//
// The queue read and the busy read happen under one q.mu -> busyMu critical
// section (via ppQueue.withLock), for the same reason given on
// PostProcessor.Empty.
func (p *PostProcessor) HasJob(j *job.Job) bool {
	if j == nil {
		return false
	}
	var found bool
	p.q.withLock(func(jobs []*Job) {
		found = slices.ContainsFunc(jobs, func(queued *Job) bool { return queued.Job == j })
		if found {
			return
		}
		p.busyMu.Lock() //lockio: q.mu -> busyMu is intentional acyclic order
		found = p.currentJob != nil && p.currentJob.Job == j
		p.busyMu.Unlock()
	})
	return found
}
