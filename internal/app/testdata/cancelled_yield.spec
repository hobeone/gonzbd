pkg ./internal/app/
run TestRemoveJob_(ReleasesARunningPostProcessingJob|ReleasesAQueuedPostProcessingJob|WaitsForTheCancelledStageToStop)$|TestJobFinalizerCancelled_LeavesALaterInstanceAlone$

# The release of a cancelled post-processing job's launch claim
# (jobFinalizer.cancelled), each part removed on its own. Two properties have
# no mutation here. The ordering of its cancel before its yield: a tick
# relaunching the parked job in that gap could not be produced
# deterministically, so the ordering is stated at the call site instead. And
# the yield's instance check (YieldedJob rather than Yielded by ID): the later
# instance in TestJobFinalizerCancelled_LeavesALaterInstanceAlone never holds a
# launch claim, which only a launched worker takes, so parking it by ID would
# change nothing the test can see. YieldedFor's own check is pinned in
# internal/dispatch by TestYieldedFor_JobMismatch_NoOpsAndPreservesNewAttempt.

[post-processing is not told where to hand a cancelled job back]
file internal/app/app.go
--- anchor
		OnJobCancelled: app.finalizer.cancelled,
--- replace
		OnJobCancelled: nil,
--- end

[the handback does not release the launch claim]
file internal/app/job_finalizer.go
--- anchor
	_ = app.dispatcher.YieldedJob(ppJob.Job)
--- replace
	_ = ppJob.Job
--- end

[the handback cancels whatever instance holds the ID]
file internal/app/job_finalizer.go
--- anchor
	_ = app.dispatcher.CancelFor(id, ppJob.Job)
--- replace
	_ = app.dispatcher.CancelFor(id, nil)
--- end
