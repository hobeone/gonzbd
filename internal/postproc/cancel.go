package postproc

import (
	"slices"

	"github.com/hobeone/gonzbd/internal/job"
)

// CancelJob removes j, this instance rather than any job with its ID, from the
// pending queue, or, if it is currently being processed, cancels its derived
// context so the active stage observes ctx.Done() and returns promptly. Stages
// must respect ctx.Done() for this to take effect during execution. Returns
// true if the job was found pending or in-flight.
//
// A pending job is handed to OnJobCancelled before CancelJob returns: nothing
// runs for it, so it can be handed back at once. An in-flight job is handed
// back by the worker once its stage returns (see run).
func (p *PostProcessor) CancelJob(j *job.Job) bool {
	if j == nil {
		return false
	}
	queued, removed := p.q.CancelJob(j)

	p.busyMu.Lock()
	inFlight := p.currentJob != nil && p.currentJob.Job == j && p.currentJobCancel != nil
	cancel := p.currentJobCancel
	p.busyMu.Unlock()

	if inFlight {
		cancel()
	}
	if removed && p.onJobCancelled != nil {
		p.onJobCancelled(queued)
	}
	return removed || inFlight
}

// CancelJob removes the queued entry carrying j, by pointer identity rather
// than by ID, and returns it, or returns nil, false if no queued entry
// carries j.
func (q *ppQueue) CancelJob(j *job.Job) (*Job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if idx := slices.IndexFunc(q.jobs, func(queued *Job) bool { return queued.Job == j }); idx >= 0 {
		removed := q.jobs[idx]
		// slices.Delete zeroes the vacated tail slot, so the backing array
		// does not keep the removed job reachable.
		q.jobs = slices.Delete(q.jobs, idx, idx+1)
		return removed, true
	}
	return nil, false
}
