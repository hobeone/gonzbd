pkg ./internal/dispatch/
run TestFinishedJob_LeavesALaterInstanceAlone$|TestRowJob_LeavesALaterInstanceAlone$

# The instance checks of FinishedJob and RowJob, each neutered on its own,
# and each door's hand-off of its instance to the shared body. FinishedJob's
# check is lookupFor's, which CancelJob, YieldedJob and handoff share, so only
# the hand-off is mutated there.

[FinishedJob ignores the expected instance]
file internal/dispatch/worker.go
--- anchor
	return d.finishedFor("FinishedJob", j.ID(), j, o)
--- replace
	return d.finishedFor("FinishedJob", j.ID(), nil, o)
--- end

[RowJob ignores the expected instance]
file internal/dispatch/registry.go
--- anchor
	if !ok || (expected != nil && e.j != expected) {
		d.mu.Unlock()
		return Row{}, false
--- replace
	if !ok {
		d.mu.Unlock()
		return Row{}, false
--- end

[RowJob drops its instance]
file internal/dispatch/registry.go
--- anchor
	return d.rowFor(j.ID(), j)
--- replace
	return d.rowFor(j.ID(), nil)
--- end
