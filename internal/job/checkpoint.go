package job

// Checkpoint is one job's durable state as the Checkpointer sees it: the four
// axes plus the progress record.
//
// It is a VALUE taken under the Job's own locks, which is what lets the
// Checkpointer batch without holding anything. §10.2 requires Job to do no
// I/O, and handing out a value rather than a pointer is what keeps that true
// for the progress record too. Progress is a *JobProgress, but it is a
// CLONE (JobProgress.clone, progress.go) taken while contentMu is held, not
// an alias to the job's live record — a live pointer would let a Store walk
// it outside any lock while MarkArticleFailed writes p.failed, p.failedBytes
// and p.articlesFailed under contentMu on another goroutine, which is an
// unsynchronised read/write and can persist a row whose failed bit and
// failedBytes total come from different moments.
type Checkpoint struct {
	ID       string
	State    StateView
	Intent   Intent
	Progress *JobProgress
}

// Checkpoint takes the job's state for persistence.
//
// It takes the two snapshots SEQUENTIALLY rather than nesting mu inside
// contentMu (or the reverse): mu's own comment states the order as mu before
// contentMu and says a future reader wanting both a StateView and a progress
// figure should prefer sequential snapshots to nesting at all. Doing so here
// means neither lock is ever held while the other is acquired, so this
// method cannot introduce the reverse order regardless of what a future
// caller does concurrently.
//
// The progress record is cloned while contentMu is held, not merely read —
// see Checkpoint's own doc comment for why a live pointer would race a
// concurrent article-accounting mutation. clone() itself is nil-safe, which
// is what lets a job with no content attached yet (j.progress == nil)
// checkpoint cleanly rather than needing a special case here.
func (j *Job) Checkpoint() Checkpoint {
	j.mu.RLock()
	var st StateView
	if a := j.currentLocked(); a != nil {
		st = a.view()
	}
	in := j.intent
	j.mu.RUnlock()

	j.contentMu.RLock()
	p := j.progress.clone()
	j.contentMu.RUnlock()

	return Checkpoint{ID: j.id, State: st, Intent: in, Progress: p}
}
