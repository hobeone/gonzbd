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
// Two admissions are never ended. One is a run a shutdown interrupts. The other
// is a job removed during a DirectUnpack wait that never finishes:
// duOrch.collect has already taken the unpacker, so no cancel reaches it, and
// the admission holds the job until shutdown.
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
// call contributes only its failure reason. Until enqueuePostProc seals the
// admission, when it builds the postproc.Job, the first reason becomes the
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
	a.jobs[j] = &postProcAdmission{failMsg: failMsg}
	return admitted
}

// seal returns j's admission's failure reason, the run's FailMsg, and turns
// any later reason into a note. It returns "" when j is not admitted.
func (a *postProcAdmissions) seal(j *job.Job) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	cur, ok := a.jobs[j]
	if !ok {
		return ""
	}
	cur.sealed = true
	return cur.failMsg
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

// release ends j's admission.
func (a *postProcAdmissions) release(j *job.Job) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.jobs, j)
}
