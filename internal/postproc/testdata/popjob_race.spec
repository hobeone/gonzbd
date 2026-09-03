pkg ./internal/postproc/
# `run` is a single global filter applied to every mutation below (see
# scripts/mutate/spec.go), not a per-mutation setting, so all four target
# tests are named here as one alternation rather than as separate `run`
# lines before each mutation.
run TestPopJob_HasAndEmptyBlockDuringTransition|TestPPQueueTryPop_MarkRunsWhileQueueLockHeld|TestHas_RequiresQueueLockEvenForCurrentJob|TestEmpty_RequiresQueueLockEvenWhenBusy

# popJob reverted to the original two-step form: q.Pop with no mark,
# followed by a separate setBusyWithJob call after Pop has already
# returned (and released q.mu). tryPop then releases q.mu as soon as the
# job is removed from the slice, well before touching busyMu, so q.mu goes
# back to free almost immediately -- the waitUntil below never observes it
# held and times out.
[popJob's mark closure removed -- the atomic dequeue-and-mark reverted]
file internal/postproc/postproc.go
--- anchor
func (p *PostProcessor) popJob() (*Job, context.Context, bool) {
	var jobCtx context.Context
	job, ok := p.q.Pop(p.workerCtx, func(j *Job) {
		var jobCancel context.CancelFunc
		jobCtx, jobCancel = context.WithCancel(p.workerCtx)
		p.setBusyWithJob(true, j.JobID(), jobCancel)
	})
	if !ok {
		return nil, nil, false
	}
	return job, jobCtx, true
}
--- replace
func (p *PostProcessor) popJob() (*Job, context.Context, bool) {
	job, ok := p.q.Pop(p.workerCtx, nil)
	if !ok {
		return nil, nil, false
	}
	jobCtx, jobCancel := context.WithCancel(p.workerCtx)
	p.setBusyWithJob(true, job.JobID(), jobCancel)
	return job, jobCtx, true
}
--- end

# tryPop's call to mark neutered. This mutation is invisible to
# TestPopJob_HasAndEmptyBlockDuringTransition above (see its own doc
# comment for why -- without mark ever running, popJob's busyMu contention
# is never entered), so it needs the ppQueue-level test instead: with mark
# never invoked, "entered" is never closed and the test times out waiting
# for it.
[mark's invocation inside tryPop dropped]
file internal/postproc/queue.go
--- anchor
	if mark != nil {
		mark(job)
	}
--- replace
	if false {
		mark(job)
	}
--- end

# Has reverted to the pre-fix sequential form: busyMu taken and released as
# its own critical section, then p.q.Has (which takes q.mu itself)
# consulted only as a fallback. With the test's job already the current
# busy job, this mutant answers straight from the busyMu step and never
# touches q.mu at all -- it returns almost immediately even though the
# test holds q.mu, instead of blocking for the full 100ms window.
[Has's combined q.mu -> busyMu critical section split back into two]
file internal/postproc/postproc.go
--- anchor
func (p *PostProcessor) Has(jobID string) bool {
	var found bool
	p.q.withLock(func(jobs []*Job) {
		if findJob(jobs, jobID) >= 0 {
			found = true
			return
		}
		p.busyMu.Lock()
		found = p.currentJobID == jobID
		p.busyMu.Unlock()
	})
	return found
}
--- replace
func (p *PostProcessor) Has(jobID string) bool {
	p.busyMu.Lock()
	current := p.currentJobID
	p.busyMu.Unlock()
	if current == jobID {
		return true
	}
	return p.q.Has(jobID)
}
--- end

# Empty reverted to the pre-fix sequential form: "!busy && p.q.Empty()".
# With busy already true, Go's && short-circuits and p.q.Empty() -- the
# only call that would touch q.mu -- is never evaluated, so this mutant
# returns false almost immediately even though the test holds q.mu.
[Empty's combined q.mu -> busyMu critical section split back into two]
file internal/postproc/postproc.go
--- anchor
func (p *PostProcessor) Empty() bool {
	var empty bool
	p.q.withLock(func(jobs []*Job) {
		if len(jobs) != 0 {
			empty = false
			return
		}
		p.busyMu.Lock()
		empty = !p.busy
		p.busyMu.Unlock()
	})
	return empty
}
--- replace
func (p *PostProcessor) Empty() bool {
	p.busyMu.Lock()
	busy := p.busy
	p.busyMu.Unlock()
	return !busy && p.q.Empty()
}
--- end
