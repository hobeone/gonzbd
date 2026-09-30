pkg ./internal/downloader/
run TestTracker_ARemovedInstancesLateCompletionLeavesTheRetrysEntries|TestTracker_TwoInstancesUnderOneIDAreTrackedSeparately|TestTracker_AnEntryDoesNotKeepItsInstanceReachable|TestDownloader_CancelJobReapsTheTracker

# The tracker keys its entries on the instance a request was dispatched for
# (#665). The first mutation drops the instance from the key at its one
# constructor. The next three put back, one helper at a time, a key for
# whatever instance holds the ID, which after a retry is the retry. The three
# after those make each helper do nothing, and the three after those make each
# skip a request whose instance has been superseded; both leak the removed
# instance's own entries. The eleventh holds the instance strongly, so an entry
# left behind keeps its job and manifest in memory.
#
# The rest neuter the reap CancelJob runs, one condition at a time: the
# aborted ID's clause, the registration clause, a registration check that
# answers by ID alone, one that reaps every instance, and each of
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
