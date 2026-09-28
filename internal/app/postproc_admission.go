package app

import (
	"sync"

	"github.com/hobeone/gonzbd/internal/job"
)

// postProcAdmissions records which job instances have been handed to
// post-processing and not yet handed back, so enqueuePostProc hands each one
// over once.
//
// An admission spans more than PostProcessor.Has does: it starts before the
// DirectUnpack wait, when the post-processor has not been given the job yet,
// and ends only after jobFinalizer.finalize or jobFinalizer.cancelled has run
// for it, where Has stops reporting the job before either callback runs. An
// admission a shutdown interrupts is never ended.
//
// It is keyed by instance, not by ID: a retry registered under the ID of a job
// whose finalizer is still running is a different job, and is admitted.
//
// The admitted call keeps everything its enqueuePostProc gathered, including
// the DirectUnpack results, which duOrch.collect hands out once. A refused
// call contributes only its failure reason, and only when the admission has
// none: enqueuePostProc reads the reason when it builds the postproc.Job, and
// finalize reads it again before building the history entry.
type postProcAdmissions struct {
	mu   sync.Mutex
	jobs map[*job.Job]*postProcAdmission
}

type postProcAdmission struct {
	failMsg string
	// sealed is set once finalize has read failMsg; a reason arriving after
	// that cannot reach the history entry.
	sealed bool
}

// admitOutcome is what admit did with a call.
type admitOutcome int

const (
	// admitted: the caller hands the job to the post-processor.
	admitted admitOutcome = iota
	// refused: already admitted, and the caller brought no new reason.
	refused
	// refusedReasonKept: already admitted; the caller's reason was kept.
	refusedReasonKept
	// refusedReasonDropped: already admitted; the caller's reason was not
	// kept, because the admission has a different one or is sealed.
	refusedReasonDropped
)

// admit admits j unless it is already admitted. A refused call's failMsg is
// kept if the admission has no reason and is not sealed.
func (a *postProcAdmissions) admit(j *job.Job, failMsg string) admitOutcome {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cur, ok := a.jobs[j]; ok {
		if failMsg == "" || failMsg == cur.failMsg {
			return refused
		}
		if cur.failMsg != "" || cur.sealed {
			return refusedReasonDropped
		}
		cur.failMsg = failMsg
		return refusedReasonKept
	}
	if a.jobs == nil {
		a.jobs = make(map[*job.Job]*postProcAdmission)
	}
	a.jobs[j] = &postProcAdmission{failMsg: failMsg}
	return admitted
}

// failMsg returns j's admission's failure reason, or "" when j is not
// admitted.
func (a *postProcAdmissions) failMsg(j *job.Job) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cur, ok := a.jobs[j]; ok {
		return cur.failMsg
	}
	return ""
}

// seal returns j's admission's failure reason and refuses any later one.
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

// release ends j's admission.
func (a *postProcAdmissions) release(j *job.Job) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.jobs, j)
}
