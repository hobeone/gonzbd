package sched

import (
	"github.com/hobeone/gonzbd/internal/job"
)

// Render is the only door that composes a job.RenderView — the view
// job.ToSABnzbd consumes. `grep -n '^func (q \*Queue) [A-Z]' internal/sched/*.go
// | grep -v _test.go` finds nine exported methods: Advance, Cancel, Park,
// Retry and Settle (advance.go, cancel.go, settle.go) write or gate; Pause and
// Resume (queue.go) write the pause flag; Paused (queue.go) is a pure getter
// of q.paused — it neither writes nor gates, so it is not grouped with the
// seven above. Render is still the one distinguished door: it is the only one
// that reads q.waitReason and q.running together under one lock to build a
// RenderView, rather than reporting a single flag back to its caller.
//
// It is one method rather than exported Running and WaitReason predicates, for
// two reasons that each rule the pair out on their own.
//
// First, waitReason's (0, false) is three-ways ambiguous: it means "no reason"
// when the attempt is settled, when the job is running, AND when work has
// ended and the job already holds what Next requires. A caller handed only
// that cannot decide RenderView.Running, so an exported WaitReason without an
// exported Running would not actually let anyone build a view.
//
// Second, two exported predicates are two lock acquisitions. A renderer
// calling q.Running(j) then q.WaitReason(j) takes q.mu twice and j.Snapshot()
// twice, and a transition landing between them yields a view that was true at
// no instant — the tear job.Snapshot exists to remove (see its comment on why
// IsOpen lives on Snapshot rather than Job), reintroduced one layer up. One
// snapshot under one lock cannot tear.
//
// It takes the *job.Job rather than a caller-supplied Snapshot so the snapshot
// and the queue predicates come from the same instant by construction; a
// caller cannot hand in a stale one. Lock order is prior spec §7.1's:
// Queue.mu here, then Job.mu inside Snapshot.
func (q *Queue) Render(j *job.Job) job.RenderView {
	q.mu.Lock()
	defer q.mu.Unlock()
	s := j.Snapshot()
	reason, _ := q.waitReason(j.ID(), s)
	return job.RenderView{
		StateView: s.State,
		Running:   q.running(j.ID(), s),
		Reason:    reason,
		Intent:    s.Intent,
	}
}
