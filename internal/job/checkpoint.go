package job

// Checkpoint is one job's durable state as the Checkpointer sees it: the four
// axes plus the progress record.
//
// It is a VALUE taken under the Job's own lock, which is what lets the
// Checkpointer batch without holding anything. §10.2 requires Job to do no
// I/O, and handing out a value rather than a pointer is what keeps that true
// for the progress record too.
type Checkpoint struct {
	ID       string
	State    StateView
	Intent   Intent
	Progress *JobProgress
}

// Checkpoint takes the job's state for persistence.
//
// The snapshot is taken atomically under nested j.mu.RLock() then
// j.contentMu.RLock(), adhering to the global lock hierarchy (mu outer, contentMu inner).
func (j *Job) Checkpoint() Checkpoint {
	j.mu.RLock()
	st := StateView{}
	if a := j.currentLocked(); a != nil {
		st = a.view()
	}
	in := j.intent

	j.contentMu.RLock()
	var p *JobProgress
	if j.progress != nil {
		p = j.progress.clone()
	}
	j.contentMu.RUnlock()
	j.mu.RUnlock()

	return Checkpoint{ID: j.id, State: st, Intent: in, Progress: p}
}
