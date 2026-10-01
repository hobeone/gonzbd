package app

import (
	"runtime"
	"slices"
	"sync"
	"weak"

	"github.com/hobeone/gonzbd/internal/job"
)

// postProcAdmissions records which job instances have been handed to
// post-processing and not yet handed back, and which have been handed back,
// so that an instance has at most one post-processing run.
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
// hand-over had begun is queued or running for the PostProcessor.CancelJob
// RemoveJob makes next.
//
// An admission a shutdown interrupts is never ended.
//
// An instance is admitted at most once. Ending an admission does not
// deregister the instance: the dispatcher still holds it when the finalizer's
// removal or a RemoveJob's fails. So release leaves the instance in ended, and
// admit refuses it from then on, rather than start a second run whose finalize
// would file the job in history again. Nothing re-runs an instance whose
// admission ended. release runs from finalize, once the run is done, and from
// cancelled: `git grep -n 'postProcAdmissions\.release(' -- 'internal/app/*.go' ':!*_test.go'`
// returns 2 lines. cancelled follows a RemoveJob, through its
// PostProcessor.CancelJob or the hand-over refusing a removed job
// (`git grep -n 'postProcessor\.CancelJob(' -- 'internal/app/*.go' ':!*_test.go'`
// returns 1 line, in RemoveJob), and latches the instance's cancel intent
// before it releases. A retry or a restart registers a new
// instance. ended holds each instance weakly, so its entry goes when the job
// is collected.
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
//
// enqueuePostProc's own close-time CloseJobHandles fault offers its reason
// through this same admit call, on the instance its own earlier call already
// admitted — so that second call is itself a refused call by this rule, and
// contributes only the fault's reason string: `git grep -n
// 'postProcAdmissions\.admit(' internal/app/app.go` finds 1 line, this one.
// enqueuePostProc's entry admission takes admit, or admitUnlessAssessing, as a
// value rather than calling it there.
//
// A hand-off by job ID (maybeFinalize) is not admitted while the job is at
// Assessing; see admitUnlessAssessing. Its reason waits in assessing for the
// job's Assessing worker, which hands the job over itself.
type postProcAdmissions struct {
	mu    sync.Mutex
	jobs  map[*job.Job]*postProcAdmission
	ended map[weak.Pointer[job.Job]]runtime.Cleanup
	// assessing holds, per instance, the reasons deferred to its Assessing
	// worker, and the visit that worker owns from beginAssess to endAssess.
	assessing map[weak.Pointer[job.Job]]*assessVisit
}

// assessVisit is one Assessing worker's record of the reasons deferred to it.
// Before a worker owns it, it holds reasons deferred to the worker the tick is
// yet to launch.
type assessVisit struct {
	reasons []string
	owned   bool
	cleanup runtime.Cleanup
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
	// refusedEnded: j's admission has ended. The caller's reason has no run
	// left to reach.
	refusedEnded
	// deferredToAssessing: j is at Assessing and not admitted. The caller's
	// reason is kept for j's Assessing worker, which hands j over.
	deferredToAssessing
)

// admit admits j unless it is admitted or its admission has ended. A call
// refused as already admitted has its failMsg become the admission's reason if
// it has none and is not sealed, and otherwise kept as a note unless it
// repeats the reason or a note.
func (a *postProcAdmissions) admit(j *job.Job, failMsg string) admitOutcome {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.admitLocked(j, failMsg)
}

// admitUnlessAssessing is admit, except that it admits no instance at
// Assessing: it keeps failMsg for the instance's Assessing worker and returns
// deferredToAssessing. That worker hands such an instance over itself
// (appRunner.runAssess), so a run this would have admitted does not start
// beside it.
//
// The state is read under mu, which the admission is recorded under, so no
// admission or deferral of j interleaves with the read. A job reads as at
// Assessing from its last file's MarkFileComplete on (awaitsAssessing), and
// the download-complete report and the tick's move into Assessing both follow
// that. So an instance this admits at Fetching was not yet complete, and its
// download-complete report, if one is made, follows the admission: runFetch's
// reads the admission and is not made, and completeFinalizedFile's does not
// read it.
func (a *postProcAdmissions) admitUnlessAssessing(j *job.Job, failMsg string) admitOutcome {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := weak.Make(j)
	if _, ended := a.ended[key]; ended {
		return refusedEnded
	}
	if _, admitted := a.jobs[j]; !admitted && awaitsAssessing(j) {
		v := a.assessing[key]
		if v == nil {
			v = a.newVisitLocked(j, key)
		}
		v.reasons = append(v.reasons, failMsg)
		return deferredToAssessing
	}
	return a.admitLocked(j, failMsg)
}

// awaitsAssessing reports whether j's open attempt is at Assessing, has
// Assessing recorded as its next state, or is a complete job at Fetching,
// whose download-complete report records it.
func awaitsAssessing(j *job.Job) bool {
	s := j.Snapshot()
	if !s.IsOpen() {
		return false
	}
	switch {
	case s.State.Next == job.Assessing:
		return true
	case s.State.Next != job.StateUnset:
		return false
	case s.State.State == job.Assessing:
		return true
	case s.State.State == job.Fetching:
		return j.IsComplete()
	}
	return false
}

// beginAssess starts j's Assessing worker's visit and returns it for
// endAssess. The visit takes over any reasons deferred before the worker
// launched. A visit still owned is an earlier worker's that has not reached
// endAssess; the new visit replaces it, with its reasons, so that the earlier
// worker's endAssess takes none of the reasons deferred to this one.
func (a *postProcAdmissions) beginAssess(j *job.Job) *assessVisit {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := weak.Make(j)
	v := a.assessing[key]
	switch {
	case v == nil:
		v = a.newVisitLocked(j, key)
	case v.owned:
		v = &assessVisit{reasons: v.reasons, cleanup: v.cleanup}
		a.assessing[key] = v
	}
	v.owned = true
	return v
}

// takeDeferred returns the reasons deferred to j's Assessing worker since it
// last looked, and forgets them.
func (a *postProcAdmissions) takeDeferred(j *job.Job) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := a.assessing[weak.Make(j)]
	if v == nil {
		return nil
	}
	reasons := v.reasons
	v.reasons = nil
	return reasons
}

// endAssess ends visit, the one beginAssess returned, and returns the reasons
// deferred to it since the worker last looked. It returns nothing for a visit
// a later one replaced.
func (a *postProcAdmissions) endAssess(j *job.Job, visit *assessVisit) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := weak.Make(j)
	if a.assessing[key] != visit {
		return nil
	}
	delete(a.assessing, key)
	visit.cleanup.Stop()
	return visit.reasons
}

// newVisitLocked records an empty visit for j, dropped if j is collected
// first: an instance removed before any worker owned its visit leaves it
// behind. mu is held.
func (a *postProcAdmissions) newVisitLocked(j *job.Job, key weak.Pointer[job.Job]) *assessVisit {
	if a.assessing == nil {
		a.assessing = make(map[weak.Pointer[job.Job]]*assessVisit)
	}
	v := &assessVisit{cleanup: runtime.AddCleanup(j, a.forgetVisit, key)}
	a.assessing[key] = v
	return v
}

// forgetVisit drops a collected job's visit.
func (a *postProcAdmissions) forgetVisit(key weak.Pointer[job.Job]) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.assessing, key)
}

// admitLocked is admit's body; mu is held.
func (a *postProcAdmissions) admitLocked(j *job.Job, failMsg string) admitOutcome {
	if _, ok := a.ended[weak.Make(j)]; ok {
		return refusedEnded
	}
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
// still j's current step: a release has already closed it, and a later step
// of the admission holds a token of its own.
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

// release ends j's admission, and records j in ended so admit refuses it from
// now on. It also ends a step still in progress: a job the post-processor
// finishes before its enqueue reaches endStep is released first, and a
// withdraw waiting on that step must not wait for an admission that no longer
// exists.
func (a *postProcAdmissions) release(j *job.Job) {
	key := weak.Make(j)
	a.mu.Lock()
	defer a.mu.Unlock()
	if cur, ok := a.jobs[j]; ok && cur.busy != nil {
		close(cur.busy)
	}
	delete(a.jobs, j)
	if _, ok := a.ended[key]; ok {
		return
	}
	if a.ended == nil {
		a.ended = make(map[weak.Pointer[job.Job]]runtime.Cleanup)
	}
	a.ended[key] = runtime.AddCleanup(j, a.forgetEnded, key)
}

// forgetEnded drops a collected job's entry from ended.
func (a *postProcAdmissions) forgetEnded(key weak.Pointer[job.Job]) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.ended, key)
}
