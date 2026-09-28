pkg ./internal/app/
run TestRemoveJob_(ReleasesARunningPostProcessingJob|ReleasesAQueuedPostProcessingJob|WaitsForTheCancelledStageToStop)$|TestJobFinalizerCancelled_(LeavesALaterInstanceAlone|LogsACancelFailure)$|TestWarnUnlessGone$

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
		app.dispatcher.YieldedJob(ppJob.Job))
--- replace
		error(nil))
--- end

[the handback cancels whatever instance holds the ID]
file internal/app/job_finalizer.go
--- anchor
		app.dispatcher.CancelJob(ppJob.Job))
--- replace
		app.dispatcher.Cancel(id))
--- end

[a vanished instance is reported as a failure]
file internal/app/job_finalizer.go
--- anchor
	if err == nil || errors.Is(err, dispatch.ErrNotFound) {
--- replace
	if err == nil || (false && errors.Is(err, dispatch.ErrNotFound)) {
--- end

[a failure is not logged]
file internal/app/job_finalizer.go
--- anchor
	log.Warn(msg, "job", id, "err", err)
--- replace
	_, _, _, _ = log, msg, id, err
--- end
