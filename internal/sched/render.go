package sched

import (
	"github.com/hobeone/gonzbd/internal/job"
)

// Render composes a job.RenderView for ONE job — the view job.ToSABnzbd
// consumes. RenderAll (below) composes the same view for many under a single
// lock; between them they are the package's two rendering doors. `grep -n '^func (q \*Queue) [A-Z]' internal/sched/*.go
// | grep -v _test.go` finds 13 exported methods: Advance, Cancel, Park,
// Retry and Settle (advance.go, cancel.go, settle.go) write or gate; Pause and
// Resume (queue.go) write the pause flag; Paused (queue.go) is a pure getter
// of q.paused; SetCaps, LeaseCap and SlotCap (queue.go) manage pool capacities;
// and Render and RenderAll (below) compose RenderViews.
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
	return q.renderLocked(j)
}

// RenderAll composes one view per job in js, in the same order, under a SINGLE
// q.mu acquisition. A loop over Render would take the lock once per job, and a
// transition landing between two of them yields a listing that was true at no
// instant — job 3 rendered Downloading and job 300 Queued when nothing ever
// held both. That is the same tear Render's own comment rejects, one layer up.
//
// It shares renderLocked with Render rather than duplicating the composition:
// there is one function that computes a RenderView, and two doors that differ
// only in how many jobs they lock around.
//
// The returned slice is always non-nil, so a caller may range over it without
// a nil check.
func (q *Queue) RenderAll(js []*job.Job) []job.RenderView {
	out := make([]job.RenderView, 0, len(js))
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range js {
		out = append(out, q.renderLocked(j))
	}
	return out
}

// renderLocked is the sole computer of a job.RenderView. The caller must hold
// q.mu. Lock order is prior spec §7.1's: q.mu already held here, then Job.mu
// inside Snapshot.
func (q *Queue) renderLocked(j *job.Job) job.RenderView {
	s := j.Snapshot()
	reason, _ := q.waitReason(j.ID(), s)
	return job.RenderView{
		StateView: s.State,
		Running:   q.running(j.ID(), s),
		Reason:    reason,
		Intent:    s.Intent,
		Holds:     q.holds(j.ID(), s),
	}
}
