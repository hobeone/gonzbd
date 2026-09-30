pkg ./internal/app/
run TestPersistAndCommit_RefusesARetryWhileItCommits$|TestRetryHistoryJob_AFinalizerBetweenItsChecksLeavesItsState$|TestRetryHistoryJob_AFinalizerAfterItsLastCheckLeavesItsState$|TestRetryHistoryJob_RefusedByAFinalizingRecordBeforeItRegisters$|TestRetryHistoryJob_AFinalizerStartingAfterTheClaimKeepsItsState$|TestPruneHistory_SkipsAJobBeingFinalized$|TestFinalize_ParErrorWithHeldVolumesRetriesWithThemReleased$|TestFinalize_SkipsAJobWhoseRemovalFailed$

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

[a retry registers under an ID a finalizer is committing]
file internal/app/app.go
--- anchor
	// ID a finalizer is committing (jobTransitions).
	if app.transitions.isFinalizing(jobID) {
--- replace
	// ID a finalizer is committing (jobTransitions).
	if false {
--- end

# The removal mark a RemoveJob keeps when its dispatcher.Remove fails,
# withdrawn on that path instead. A finalizer of the instance then
# takes its fallback teardown by ID, whether it runs between the retry's two
# finalizing checks or after the last, and the retry registers without the
# manifest and rows it wrote.

[a RemoveJob whose Remove failed gives its mark back]
file internal/app/app.go
--- anchor
			app.checkpointer.Unprune(j)
		}
		return rmErr
--- replace
			app.checkpointer.Unprune(j)
		}
		app.transitions.mu.Lock()
		for key, cleanup := range app.transitions.removed {
			if key.Value() == j {
				cleanup.Stop()
				delete(app.transitions.removed, key)
			}
		}
		app.transitions.mu.Unlock()
		return rmErr
--- end
