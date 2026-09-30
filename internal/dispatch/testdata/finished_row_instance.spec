pkg ./internal/dispatch/
run TestFinishedJob_LeavesALaterInstanceAlone$|TestRowJob_LeavesALaterInstanceAlone$

# The instance checks of FinishedJob and RowJob, each neutered on its own.
# FinishedJob's check is lookupFor's, which CancelJob, YieldedJob and handoff
# share, so the mutation drops the instance FinishedJob passes it instead.

[FinishedJob ignores the expected instance]
file internal/dispatch/worker.go
--- anchor
	return d.finishedFor(j.ID(), j, o)
--- replace
	return d.finishedFor(j.ID(), nil, o)
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
