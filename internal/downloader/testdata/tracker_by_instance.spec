pkg ./internal/downloader/
run TestTracker_ARemovedInstancesLateCompletionLeavesTheRetrysEntries|TestTracker_TwoInstancesUnderOneIDAreTrackedSeparately|TestTracker_AnEntryDoesNotKeepItsInstanceReachable

# The tracker keys its entries on the instance a request was dispatched for
# (#665). The first mutation drops the instance from the key at its one
# constructor. The next three put back, one helper at a time, a key for
# whatever instance holds the ID, which after a retry is the retry. The last
# holds the instance strongly, so an entry left behind keeps its job and
# manifest in memory.
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
