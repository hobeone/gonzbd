pkg ./internal/app/
run TestRemoveJob_(ReleasesARunningPostProcessingJob|ReleasesAQueuedPostProcessingJob|WaitsForTheCancelledStageToStop)$|TestJobFinalizerCancelled_(LeavesALaterInstanceAlone|LogsACancelFailure|LeavesALaterInstancesLaunchClaimAlone)$|TestWarnUnlessGone$

# The release of a cancelled post-processing job's launch claim
# (jobFinalizer.cancelled), each part removed on its own. One property has no
# mutation here: the ordering of its cancel before its yield. A tick
# relaunching the parked job in that gap could not be produced
# deterministically, so the ordering is stated at the call site instead.

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
	warnUnlessGone(app.log, "postproc cancel: releasing the job's launch claim failed", id,
		app.dispatcher.YieldedJob(ppJob.Job))
--- replace
	warnUnlessGone(app.log, "postproc cancel: releasing the job's launch claim failed", id,
		error(nil))
--- end

[the handback yields by ID instead of by instance]
file internal/app/job_finalizer.go
--- anchor
	warnUnlessGone(app.log, "postproc cancel: releasing the job's launch claim failed", id,
		app.dispatcher.YieldedJob(ppJob.Job))
--- replace
	warnUnlessGone(app.log, "postproc cancel: releasing the job's launch claim failed", id,
		app.dispatcher.Yielded(id))
--- end

[the handback cancels whatever instance holds the ID]
file internal/app/job_finalizer.go
--- anchor
	warnUnlessGone(app.log, "postproc cancel: cancelling the job failed", id,
		app.dispatcher.CancelJob(ppJob.Job))
--- replace
	warnUnlessGone(app.log, "postproc cancel: cancelling the job failed", id,
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
