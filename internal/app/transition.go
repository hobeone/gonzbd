package app

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"weak"

	"github.com/hobeone/gonzbd/internal/job"
)

// jobTransitions admits one actor at a time to the state keyed by a job ID:
// its job_files and written_articles rows, its queue manifest,
// its NZB backup, its history entry and its download directory. A retry, a
// finalization, a queue removal and a history change each act on that state,
// and none of them can rely on the dispatcher to exclude the others: several
// act after the dispatcher has let the job go, and the history paths act on a
// job the dispatcher no longer holds.
//
// One exception is bounded rather than excluded: a finalizer proceeds without
// the lock once it has waited finalizeTransitionWait, or at once when app.ctx
// has ended (jobFinalizer.persistAndCommit). A retry is still excluded from
// it. persistAndCommit records its ID in finalizing (beginFinalize) before it
// does anything, and releases it on return. While the ID is recorded,
// tryAcquire does not claim it, and a retryHistoryJob that claimed it first
// refuses (isFinalizing) once it has found no instance registered under the
// ID and before it changes anything. The finalizer's own retry of a job it
// filed (jobFinalizer.retryWithHeldVolumes) is called from finalize after
// persistAndCommit has returned, so the record has ended by then.
//
// That check covers a finalizer that began before it, because the record
// precedes the finalizer's CancelJob and an instance leaves the dispatcher
// only after a cancel. The cancels and removals
// (`git grep -n 'dispatcher\.\(Cancel\|CancelJob\|Remove\|RemoveJob\)(' -- 'internal/app/*.go' ':!*_test.go'`
// finds 7 lines) are the finalizer's own, cancelled's for a job a RemoveJob
// took, startup's before the first tick, and RemoveJob's Cancel and Remove,
// whose removeFor cancels again. So a retry that finds the finalizer's
// instance gone either sees the record, or a RemoveJob cancelled the
// instance. That RemoveJob marked the instance before its Cancel and keeps
// the mark whether or not its Remove succeeds (see removed below), and the
// finalizer reads the mark before any by-ID step. So a finalizer of that
// instance that begins after the retry's one finalizing check, whether
// before the retry registers or after, files nothing and acts on nothing
// under the ID
// (TestRetryHistoryJob_AFinalizerBetweenItsChecksLeavesItsState,
// TestRetryHistoryJob_AFinalizerAfterItsLastCheckLeavesItsState); a second
// check before registering would guard no state the first does not already
// cover, so retryHistoryJob checks the record only once, right after the
// registration check. A retry is the one way a later instance takes an ID
// (`git grep -n 'dispatcher\.Add(' -- 'internal/app/*.go' ':!*_test.go'`
// finds 2 lines: retryHistoryJob's reuses an ID, and AddJob's mints one), so
// no other instance can register under the ID while its finalizer commits.
//
// A channel per held ID rather than a mutex per ID, because a waiter has to be
// able to give up when its context ends and sync.Mutex.Lock cannot. It is the
// idiom of Dispatcher.claimLaunched / waitLaunched.
//
// Lock order: a holder may wait inside the dispatcher (dispatcher.Remove waits
// on a job's launch claim), so no one may wait for this lock while holding
// something the dispatcher waits on. Of the sites TestJobTransitions_LockSites
// pins, the finalizer alone runs holding such a thing — post-processing's
// launch claim, which its own Yielded clears — so it takes the lock only after
// that call. retryHistoryJob also runs on the post-processing worker, from
// jobFinalizer.retryWithHeldVolumes, once persistAndCommit has returned; it
// never waits for the lock, taking it with tryAcquire.
//
// removed records the job instances a RemoveJob has taken, including one
// whose dispatcher.Remove failed, so a finalizer can tell a removal from the
// other ways a job leaves the dispatcher (the tick evicts a never-run job the
// finalizer's own Cancel made evictable, and that one must still be filed),
// and so that neither a post-processing hand-over (beginHandOver) nor a
// DirectUnpack start (directUnpackOrchestrator.maybeStart) begins for the
// instance once it is marked: the RemoveJob stops only those that began
// before. TestJobTransitions_RemovedSites pins those three readers.
// A mark is not withdrawn while the job is reachable: the one delete from
// removed (`git grep -n 'delete(t\.removed' -- 'internal/app/*.go'` finds 1
// line) is forgetRemoved's, which runs once the job is collected. Keyed by
// instance, not ID, so a later job under the same ID is not affected; weakly,
// so a removed job is not kept alive by this record.
//
// mu is never held across I/O or a wait: it guards held, removed, finalizing
// and each claim's released flag.
type jobTransitions struct {
	mu      sync.Mutex
	held    map[string]chan struct{} // an ID is present only while claimed; its channel closes on release
	removed map[weak.Pointer[job.Job]]runtime.Cleanup
	// finalizing counts the finalizers committing each ID; an ID is present
	// only while one is.
	finalizing map[string]int
}

// beginFinalize records that a finalizer is committing id, and returns the
// call that ends the record. TestJobTransitions_FinalizingSites pins
// persistAndCommit as its only caller.
func (t *jobTransitions) beginFinalize(id string) (end func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finalizing == nil {
		t.finalizing = make(map[string]int)
	}
	t.finalizing[id]++
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.finalizing[id]--
		if t.finalizing[id] <= 0 {
			delete(t.finalizing, id)
		}
	}
}

// isFinalizing reports whether a finalizer is committing id.
func (t *jobTransitions) isFinalizing(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.finalizing[id] > 0
}

// markRemoved records that a RemoveJob has taken j. TestJobTransitions_RemovedSites
// pins RemoveJob as its only caller.
func (t *jobTransitions) markRemoved(j *job.Job) {
	key := weak.Make(j)
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.removed[key]; ok {
		return
	}
	if t.removed == nil {
		t.removed = make(map[weak.Pointer[job.Job]]runtime.Cleanup)
	}
	t.removed[key] = runtime.AddCleanup(j, t.forgetRemoved, key)
}

func (t *jobTransitions) forgetRemoved(key weak.Pointer[job.Job]) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.removed, key)
}

// wasRemoved reports whether a RemoveJob has taken j.
func (t *jobTransitions) wasRemoved(j *job.Job) bool {
	key := weak.Make(j)
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.removed[key]
	return ok
}

// errJobInTransition refuses an action on a job ID another actor holds.
var errJobInTransition = errors.New("another change to this job is in progress")

// transitionClaim is what a holder releases. Until then it holds exactly the
// IDs in ids.
type transitionClaim struct {
	t        *jobTransitions
	ids      map[string]struct{}
	released bool // guarded by t.mu
}

// tryAcquire claims every one of ids that no one holds and no finalizer is
// committing, skips the rest, and never waits. The claim is never nil; holds
// says which ids it took.
func (t *jobTransitions) tryAcquire(ids ...string) *transitionClaim {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := &transitionClaim{t: t, ids: make(map[string]struct{}, len(ids))}
	for _, id := range ids {
		if t.finalizing[id] > 0 {
			continue
		}
		if t.claimLocked(id) {
			c.ids[id] = struct{}{}
		}
	}
	return c
}

// acquire claims id, waiting while another claim holds it. If ctx ends while
// it waits, it returns ctx.Err() and holds nothing. A free id is claimed
// without consulting ctx, even one that has already ended — a contract
// golang.org/x/sync/semaphore.Weighted.Acquire also permits — so a finalizer
// on a stopping process still takes a lock no one holds.
func (t *jobTransitions) acquire(ctx context.Context, id string) (*transitionClaim, error) {
	for {
		holder := t.claimOrHolder(id)
		if holder == nil {
			return &transitionClaim{t: t, ids: map[string]struct{}{id: {}}}, nil
		}
		// A closed channel means that holder released, not that id is ours:
		// every waiter wakes on the same close, so each goes round again.
		select {
		case <-holder:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// claimOrHolder claims id and returns nil, or returns the current holder's
// channel without claiming.
func (t *jobTransitions) claimOrHolder(id string) chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.claimLocked(id) {
		return nil
	}
	return t.held[id]
}

func (t *jobTransitions) claimLocked(id string) bool {
	if _, busy := t.held[id]; busy {
		return false
	}
	if t.held == nil {
		t.held = make(map[string]chan struct{})
	}
	t.held[id] = make(chan struct{})
	return true
}

// release frees every ID the claim holds and wakes their waiters. Releasing a
// claim twice is a programming error and panics: the second call would free an
// ID a later claim may already hold.
func (c *transitionClaim) release() {
	c.t.mu.Lock()
	defer c.t.mu.Unlock()
	if c.released {
		panic("app: transition claim released twice")
	}
	c.released = true
	for id := range c.ids {
		close(c.t.held[id])
		delete(c.t.held, id)
	}
}

func (c *transitionClaim) holds(id string) bool {
	_, ok := c.ids[id]
	return ok
}
