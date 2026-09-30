pkg ./internal/app/
run TestPersistAndCommit_RefusesARetryWhileItCommits$|TestRetryHistoryJob_RefusesWhenAFinalizerStartsDuringIt$|TestRetryHistoryJob_AFinalizerStartingAfterTheClaimKeepsItsState$|TestPruneHistory_SkipsAJobBeingFinalized$|TestFinalize_ParErrorWithHeldVolumesRetriesWithThemReleased$

# The finalizing record that keeps a retry off an ID while its finalizer
# commits, each part neutered on its own. retryHistoryJob meets the record
# three times: at tryAcquire, after its registration check, and before it
# registers. TestPersistAndCommit_RefusesARetryWhileItCommits asserts the
# retry never claims the ID, so it kills the tryAcquire mutation itself.
# TestFinalize_ParErrorWithHeldVolumesRetriesWithThemReleased is here for the
# finalizer's own retry, which must be admitted once the record has ended.

[the finalizer never records its ID]
file internal/app/job_finalizer.go
--- anchor
		defer app.transitions.beginFinalize(ppJob.Job.ID())()
--- replace
		defer func() {}()
--- end

[the finalizer never ends its record]
file internal/app/job_finalizer.go
--- anchor
		defer app.transitions.beginFinalize(ppJob.Job.ID())()
--- replace
		app.transitions.beginFinalize(ppJob.Job.ID())
--- end

[tryAcquire claims an ID a finalizer is committing]
file internal/app/transition.go
--- anchor
		if t.finalizing[id] > 0 {
			continue
		}
--- replace
		if false {
			continue
		}
--- end

[a retry that claimed the ID first changes state under the finalizer]
file internal/app/app.go
--- anchor
	// filing; see jobTransitions for why this check is late enough.
	if app.transitions.isFinalizing(jobID) {
--- replace
	// filing; see jobTransitions for why this check is late enough.
	if false {
--- end

[a retry registers under a finalizer that began during it]
file internal/app/app.go
--- anchor
	// does not depend on that ordering holding.
	if app.transitions.isFinalizing(jobID) {
--- replace
	// does not depend on that ordering holding.
	if false {
--- end
