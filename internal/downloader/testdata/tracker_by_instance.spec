pkg ./internal/downloader/
run TestTracker_ARemovedInstancesLateCompletionLeavesTheRetrysEntries|TestTracker_TwoInstancesUnderOneIDAreTrackedSeparately|TestTracker_AnEntryDoesNotKeepItsInstanceReachable|TestDownloader_CancelJobReapsTheTracker|TestDownloader_RegisteredRejectsACollectedUnregisteredInstance

# The tracker keys its entries on the instance a request was dispatched for
# (#665). The first mutation drops the instance from the key at its one
# constructor. The next three put back, one helper at a time, a key for
# whatever instance holds the ID, which after a retry is the retry. The three
# after those make each helper do nothing, and the three after those make each
# skip a request whose instance has been superseded; both leak the removed
# instance's own entries. The eleventh holds the instance strongly, so an entry
# left behind keeps its job and manifest in memory.
#
# The next two make Instances skip one of its two maps, so a stale instance
# whose only entry is in the skipped map is never offered to the reap.
# TestDownloader_CancelJobReapsTheTracker's "j4" and "j5" each have an entry
# in only one map, so each mutation is caught by exactly one of them.
#
# The rest neuter the reap CancelJob runs, one condition at a time: the
# aborted ID's clause, the registration clause, a registration check that
# answers by ID alone, a registration check that answers by instance identity
# alone (so a collected, unregistered instance's nil instance compares equal
# to the dispatcher's nil miss), one that reaps every instance, and each of
# DropInstances's two map sweeps.
#
# Every mutation is killed by a test that reads the tracker's state directly.
#
# tryDispatch's keyFor(a.Job, a.ArtIdx) has no by-ID mutation: a.Job is the
# instance the dispatch pass just took from Dispatcher.Job, so a lookup there
# finds the same instance.

[the key drops the instance]
file internal/downloader/tracker.go
--- anchor
	return articleKey{jobID: j.ID(), inst: weak.Make(j), artIdx: artIdx}
--- replace
	return articleKey{jobID: j.ID(), artIdx: artIdx}
--- end

[clearInFlight decrements whatever instance holds the ID]
file internal/downloader/dispatch.go
--- anchor
	d.tracker.DecrementInFlight(keyFor(req.job, req.artIdx))
--- replace
	cur := req.job
	if j, ok := d.dispatcher.Job(req.jobID()); ok {
		cur = j
	}
	d.tracker.DecrementInFlight(keyFor(cur, req.artIdx))
--- end

[unmarkTried unmarks whatever instance holds the ID]
file internal/downloader/dispatch.go
--- anchor
	d.tracker.UnmarkTried(keyFor(req.job, req.artIdx), serverIdx)
--- replace
	cur := req.job
	if j, ok := d.dispatcher.Job(req.jobID()); ok {
		cur = j
	}
	d.tracker.UnmarkTried(keyFor(cur, req.artIdx), serverIdx)
--- end

[clearTried clears whatever instance holds the ID]
file internal/downloader/dispatch.go
--- anchor
	d.tracker.ClearTried(keyFor(req.job, req.artIdx))
--- replace
	cur := req.job
	if j, ok := d.dispatcher.Job(req.jobID()); ok {
		cur = j
	}
	d.tracker.ClearTried(keyFor(cur, req.artIdx))
--- end

[clearInFlight decrements nothing]
file internal/downloader/dispatch.go
--- anchor
	d.tracker.DecrementInFlight(keyFor(req.job, req.artIdx))
--- replace
	_ = req
--- end

[unmarkTried unmarks nothing]
file internal/downloader/dispatch.go
--- anchor
	d.tracker.UnmarkTried(keyFor(req.job, req.artIdx), serverIdx)
--- replace
	_, _ = req, serverIdx
--- end

[clearTried clears nothing]
file internal/downloader/dispatch.go
--- anchor
	d.tracker.ClearTried(keyFor(req.job, req.artIdx))
--- replace
	_ = req
--- end

[clearInFlight skips a superseded instance]
file internal/downloader/dispatch.go
--- anchor
	d.tracker.DecrementInFlight(keyFor(req.job, req.artIdx))
--- replace
	if cur, ok := d.dispatcher.Job(req.jobID()); ok && cur != req.job {
		return
	}
	d.tracker.DecrementInFlight(keyFor(req.job, req.artIdx))
--- end

[unmarkTried skips a superseded instance]
file internal/downloader/dispatch.go
--- anchor
	d.tracker.UnmarkTried(keyFor(req.job, req.artIdx), serverIdx)
--- replace
	if cur, ok := d.dispatcher.Job(req.jobID()); ok && cur != req.job {
		return
	}
	d.tracker.UnmarkTried(keyFor(req.job, req.artIdx), serverIdx)
--- end

[clearTried skips a superseded instance]
file internal/downloader/dispatch.go
--- anchor
	d.tracker.ClearTried(keyFor(req.job, req.artIdx))
--- replace
	if cur, ok := d.dispatcher.Job(req.jobID()); ok && cur != req.job {
		return
	}
	d.tracker.ClearTried(keyFor(req.job, req.artIdx))
--- end

[the key holds its instance strongly]
file internal/downloader/tracker.go
--- anchor
	inst   weak.Pointer[job.Job]
	artIdx int32
}

// keyFor is the constructor of articleKey. Its zero value, a nil instance,
// is unreachable through it: j.ID() dereferences j.
// `git grep -n 'articleKey{' -- 'internal/downloader/*.go' ':(exclude)*_test.go'`
// finds 2: the literal below, and this comment quoting the pattern.
func keyFor(j *job.Job, artIdx int32) articleKey {
	return articleKey{jobID: j.ID(), inst: weak.Make(j), artIdx: artIdx}
}
--- replace
	inst   weak.Pointer[job.Job]
	artIdx int32
	pin    *job.Job
}

func keyFor(j *job.Job, artIdx int32) articleKey {
	return articleKey{jobID: j.ID(), inst: weak.Make(j), artIdx: artIdx, pin: j}
}
--- end

[Instances ignores tryList]
file internal/downloader/tracker.go
--- anchor
	for k := range m.tryList {
		add(k)
	}
--- replace
	for range m.tryList {
	}
--- end

[Instances ignores inFlight]
file internal/downloader/tracker.go
--- anchor
	for k := range m.inFlight {
		add(k)
	}
--- replace
	for range m.inFlight {
	}
--- end

[the reap spares the aborted ID]
file internal/downloader/downloader.go
--- anchor
		if ref.jobID == aborted || !d.registered(ref) {
--- replace
		if !d.registered(ref) {
--- end

[the reap spares an unregistered instance]
file internal/downloader/downloader.go
--- anchor
		if ref.jobID == aborted || !d.registered(ref) {
--- replace
		if ref.jobID == aborted {
--- end

[registration is checked by ID alone]
file internal/downloader/downloader.go
--- anchor
	return ok && cur == ref.inst.Value()
--- replace
	_ = cur
	return ok
--- end

[registration ignores whether the ID is even registered]
file internal/downloader/downloader.go
--- anchor
	return ok && cur == ref.inst.Value()
--- replace
	_ = ok
	return cur == ref.inst.Value()
--- end

[the reap takes every instance]
file internal/downloader/downloader.go
--- anchor
	return ok && cur == ref.inst.Value()
--- replace
	_, _ = cur, ok
	return false
--- end

[DropInstances leaves the try-list]
file internal/downloader/tracker.go
--- anchor
			delete(m.tryList, k)
--- replace
			_ = k
--- end

[DropInstances leaves the in-flight counts]
file internal/downloader/tracker.go
--- anchor
			delete(m.inFlight, k)
--- replace
			_ = k
--- end
