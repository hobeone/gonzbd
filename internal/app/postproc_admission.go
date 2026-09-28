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
// An admission spans more than PostProcessor.Has does: it starts before the
// DirectUnpack wait, when the post-processor has not been given the job yet,
// and ends only after jobFinalizer.finalize or jobFinalizer.cancelled has run
// for it, where Has stops reporting the job before either callback runs.
//
// A removed job's post-processing does not start. RemoveJob marks the instance
// removed (jobTransitions.markRemoved) and then calls withdraw, and
// beginHandOver, which enqueuePostProc passes before PostProcessor.Process,
// refuses an instance with that mark. withdraw also ends a DirectUnpack wait
// the enqueue is in, which duOrch.abortJob cannot reach once duOrch.collect has
// taken the unpacker, and waits out a hand-over already past the check, so the
// PostProcessor.Cancel RemoveJob makes next finds the job.
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
	// handing is non-nil from beginHandOver until endHandOver closes it.
	handing chan struct{}
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

// beginHandOver starts handing j to the post-processor. It refuses, returning
// false, when j is not admitted or t records j as removed. Otherwise it seals
// the admission, turning any later reason into a note, and returns the run's
// FailMsg; the caller hands the job over and then calls endHandOver.
//
// The removal mark is read under mu, which withdraw takes only after the mark
// is set, so withdraw's caller sees either this call refuse or its hand-over
// end. t.mu is taken inside mu, and jobTransitions holds t.mu across no I/O,
// no wait and no lock of this package (transition.go).
func (a *postProcAdmissions) beginHandOver(j *job.Job, t *jobTransitions) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cur, ok := a.jobs[j]
	if !ok || t.wasRemoved(j) {
		return "", false
	}
	cur.sealed = true
	cur.handing = make(chan struct{})
	return cur.failMsg, true
}

// endHandOver ends the hand-over beginHandOver started, releasing a withdraw
// waiting for it.
func (a *postProcAdmissions) endHandOver(j *job.Job) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cur, ok := a.jobs[j]; ok && cur.handing != nil {
		close(cur.handing)
		cur.handing = nil
	}
}

// withdraw is RemoveJob's notice that j is removed, given after
// jobTransitions.markRemoved. It closes the channel removal returns and, while
// a hand-over of j is in progress, waits for endHandOver. It does nothing for
// a job that is not admitted.
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
	handing := cur.handing
	a.mu.Unlock()
	if handing != nil {
		<-handing
	}
}

// removal returns the channel withdraw closes for j, or nil when j is not
// admitted.
func (a *postProcAdmissions) removal(j *job.Job) <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cur, ok := a.jobs[j]; ok {
		return cur.removed
	}
	return nil
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

// release ends j's admission. It also ends a hand-over still open: a job the
// post-processor finishes before its enqueue reaches endHandOver is released
// first, and a withdraw waiting on that hand-over must not wait for an
// admission that no longer exists.
func (a *postProcAdmissions) release(j *job.Job) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cur, ok := a.jobs[j]; ok && cur.handing != nil {
		close(cur.handing)
	}
	delete(a.jobs, j)
}
