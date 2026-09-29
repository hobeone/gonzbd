package app

import (
	"slices"
	"sync"

	"github.com/hobeone/gonzbd/internal/job"
)

// postProcAdmissions records which job instances have been handed to
// post-processing and not yet handed back, so that at most one
// post-processing run of an instance is in progress at a time.
//
// An admission spans more than PostProcessor.HasJob does: it starts before the
// DirectUnpack wait, when the post-processor has not been given the job yet,
// and ends only after jobFinalizer.finalize or jobFinalizer.cancelled has run
// for it, where HasJob stops reporting the job before either callback runs.
//
// A removed job's post-processing does not start. RemoveJob marks the instance
// removed (jobTransitions.markRemoved) and then calls withdraw, and
// beginHandOver, which enqueuePostProc passes before PostProcessor.Process,
// refuses an instance with that mark. The enqueue's two steps, the DirectUnpack
// wait (beginWait) and the hand-over (beginHandOver), each hold the admission
// busy until endStep. withdraw signals the wait to abort its unpacker, which
// duOrch.abortJob cannot reach once duOrch.collect has taken it, and returns
// only once the step in progress has ended: the unpacker has stopped writing
// into the download directory RemoveJob goes on to delete, and a job whose
// hand-over had begun is queued or running for the PostProcessor.Cancel
// RemoveJob makes next.
//
// An admission a shutdown interrupts is never ended.
//
// Ending an admission does not deregister the instance. If the dispatcher still
// holds it afterwards, because a removal failed, a later call can admit it
// again.
//
// It is keyed by instance, not by ID: a retry registered under the ID of a job
// whose finalizer is still running is a different job, and is admitted.
//
// The admitted call keeps everything its enqueuePostProc gathered, including
// the DirectUnpack results, which duOrch.collect hands out once. A refused
// call contributes only its failure reason. Until beginHandOver seals the
// admission, before enqueuePostProc builds the postproc.Job, the first reason becomes the
// run's FailMsg. Any other reason is kept as a note, which finalize adds to
// the history entry's stage log without changing its status.
type postProcAdmissions struct {
	mu   sync.Mutex
	jobs map[*job.Job]*postProcAdmission
}

type postProcAdmission struct {
	failMsg string
	// sealed is set once the run's FailMsg has been taken from failMsg.
	sealed bool
	// notes are the reasons that did not become the run's FailMsg.
	notes []string
	// removed is closed by withdraw.
	removed chan struct{}
	// busy is the token of the step in progress, from beginWait or
	// beginHandOver until endStep or release closes it; nil between steps.
	busy chan struct{}
}

// admitOutcome is what admit did with a call.
type admitOutcome int

const (
	// admitted: the caller hands the job to the post-processor.
	admitted admitOutcome = iota
	// refused: already admitted, and the caller brought no new reason.
	refused
	// refusedReasonKept: already admitted; the caller's reason becomes the
	// run's FailMsg.
	refusedReasonKept
	// refusedReasonNoted: already admitted; the caller's reason is kept as a
	// note, because the admission has a different reason or is sealed.
	refusedReasonNoted
)

// admit admits j unless it is already admitted. A refused call's failMsg
// becomes the admission's reason if it has none and is not sealed, and is
// otherwise kept as a note unless it repeats the reason or a note.
func (a *postProcAdmissions) admit(j *job.Job, failMsg string) admitOutcome {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cur, ok := a.jobs[j]; ok {
		if failMsg == "" || failMsg == cur.failMsg {
			return refused
		}
		if cur.failMsg == "" && !cur.sealed {
			cur.failMsg = failMsg
			return refusedReasonKept
		}
		if slices.Contains(cur.notes, failMsg) {
			return refused
		}
		cur.notes = append(cur.notes, failMsg)
		return refusedReasonNoted
	}
	if a.jobs == nil {
		a.jobs = make(map[*job.Job]*postProcAdmission)
	}
	a.jobs[j] = &postProcAdmission{failMsg: failMsg, removed: make(chan struct{})}
	return admitted
}

// beginWait starts the DirectUnpack wait of j's enqueue. It returns the
// channel withdraw closes, on which the wait aborts its unpacker, and the
// step's token for endStep. Both are nil when j is not admitted.
func (a *postProcAdmissions) beginWait(j *job.Job) (removed <-chan struct{}, token chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cur, ok := a.jobs[j]
	if !ok {
		return nil, nil
	}
	cur.busy = make(chan struct{})
	return cur.removed, cur.busy
}

// beginHandOver starts handing j to the post-processor. It refuses, returning
// false, when j is not admitted or t records j as removed. Otherwise it seals
// the admission, turning any later reason into a note, and returns the run's
// FailMsg and the step's token; the caller hands the job over and then calls
// endStep with the token.
//
// The removal mark is read under mu, and withdraw takes mu only after the mark
// is set. So a withdraw that takes mu first makes this call refuse, and one
// that takes it later finds this step's token and returns once endStep or
// release has closed it. t.mu is taken inside mu; jobTransitions holds t.mu
// across no I/O, no wait and no lock of this package (transition.go).
func (a *postProcAdmissions) beginHandOver(j *job.Job, t *jobTransitions) (failMsg string, token chan struct{}, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cur, ok := a.jobs[j]
	if !ok || t.wasRemoved(j) {
		return "", nil, false
	}
	cur.sealed = true
	cur.busy = make(chan struct{})
	return cur.failMsg, cur.busy, true
}

// endStep ends the step whose token beginWait or beginHandOver returned,
// releasing a withdraw waiting for it. It closes the token only while it is
// still j's current step: a release has already closed it, and a later
// admission of j holds a token of its own.
func (a *postProcAdmissions) endStep(j *job.Job, token chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cur, ok := a.jobs[j]; ok && token != nil && cur.busy == token {
		close(cur.busy)
		cur.busy = nil
	}
}

// withdraw is RemoveJob's notice that j is removed, given after
// jobTransitions.markRemoved. It closes the channel beginWait returned and, if
// a step of j's enqueue is in progress, waits until that step ends. It does
// nothing for a job that is not admitted.
func (a *postProcAdmissions) withdraw(j *job.Job) {
	a.mu.Lock()
	cur, ok := a.jobs[j]
	if !ok {
		a.mu.Unlock()
		return
	}
	select {
	case <-cur.removed:
	default:
		close(cur.removed)
	}
	busy := cur.busy
	a.mu.Unlock()
	if busy != nil {
		<-busy
	}
}

// notes returns a copy of the reasons j's admission kept as notes.
func (a *postProcAdmissions) notes(j *job.Job) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	cur, ok := a.jobs[j]
	if !ok || len(cur.notes) == 0 {
		return nil
	}
	return append([]string(nil), cur.notes...)
}

// has reports whether j is admitted. It is the downloader's HandedOff: an
// admitted job is not dispatched, because enqueuePostProc admits before it
// closes the job's file handles.
func (a *postProcAdmissions) has(j *job.Job) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.jobs[j]
	return ok
}

// unlessAdmitted runs fn unless j is admitted, and reports whether it ran. mu is
// held across fn, so no admission of j begins while fn runs: fn must not call
// into a, and must not block.
func (a *postProcAdmissions) unlessAdmitted(j *job.Job, fn func()) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.jobs[j]; ok {
		return false
	}
	fn()
	return true
}

// release ends j's admission. It also ends a step still in progress: a job the
// post-processor finishes before its enqueue reaches endStep is released
// first, and a withdraw waiting on that step must not wait for an admission
// that no longer exists.
func (a *postProcAdmissions) release(j *job.Job) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cur, ok := a.jobs[j]; ok && cur.busy != nil {
		close(cur.busy)
	}
	delete(a.jobs, j)
}
