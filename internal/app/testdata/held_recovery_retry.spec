pkg ./internal/app/
run Test(Finalize_ParErrorWithHeldVolumesRetriesWithThemReleased|Finalize_RetriedJobIsNotRetriedAgain|Finalize_RetryThatCannotStartLeavesTheFailureVisible|Finalize_RetriesOnlyAParErrorWithHeldVolumes|RetryHistoryJob_PrepareErrorAbortsTheRetry|Finalize_HeldVolumesEntryCarriesTheRetryNote|Finalize_HeldRecoveryRetry_DoesNotEmitJobFinalized|Application_HasPostProcJob)$

# #651: a par2 failure while recovery volumes were held back retries the job
# with them released. Each clause of the trigger, the release in the retry,
# the loop bound, the notification rule and the prepare's abort, one at a time.

[the trigger ignores whether par2 failed]
file internal/app/job_finalizer.go
--- anchor
	return ppJob.ParError && ppJob.Job.HasDeferredPar2()
--- replace
	return ppJob.Job.HasDeferredPar2()
--- end

[the trigger ignores whether volumes are held, so the retry is retried again]
file internal/app/job_finalizer.go
--- anchor
	return ppJob.ParError && ppJob.Job.HasDeferredPar2()
--- replace
	return ppJob.ParError
--- end

[the retry does not release the held volumes]
file internal/app/app.go
--- anchor
	if prepare != nil {
--- replace
	if false {
--- end

[a prepare error does not abort the retry]
file internal/app/app.go
--- anchor
		if err := prepare(j); err != nil {
--- replace
		if err := prepare(j); err != nil && false {
--- end

[a queued retry still sends the failure notification]
file internal/app/job_finalizer.go
--- anchor
	app.log.Info("finalize: par2 repair failed while recovery volumes were held back; retrying the job to fetch them",
		"job", jobID)
	return true
--- replace
	app.log.Info("finalize: par2 repair failed while recovery volumes were held back; retrying the job to fetch them",
		"job", jobID)
	return false
--- end

[a queued retry still reports post-processing complete]
file internal/app/job_finalizer.go
--- anchor
	if !retry || !f.retryWithHeldVolumes(ppJob.Job.ID()) {
		select {
		case app.postProcComplete <- PostProcComplete{JobID: ppJob.Job.ID()}:
		default:
		}
--- replace
	select {
	case app.postProcComplete <- PostProcComplete{JobID: ppJob.Job.ID()}:
	default:
	}
	if !retry || !f.retryWithHeldVolumes(ppJob.Job.ID()) {
--- end

[a queued retry still emits job_finalized]
file internal/app/job_finalizer.go
--- anchor
		// job_finalized signals a queue→history transition so both stores
		// refresh from a single trigger and reach the new state together.
		app.emit(Event{Type: "job_finalized", NzoID: ppJob.Job.ID()})
		f.fireCompletionNotification(entry)
	}
--- replace
		f.fireCompletionNotification(entry)
	}
	app.emit(Event{Type: "job_finalized", NzoID: ppJob.Job.ID()})
--- end

[a retry that could not start suppresses the failure notification]
file internal/app/job_finalizer.go
--- anchor
			"job", jobID, "err", err)
		return false
	}
	app.log.Info("finalize: par2 repair failed while
--- replace
			"job", jobID, "err", err)
		return true
	}
	app.log.Info("finalize: par2 repair failed while
--- end

# A prepare runs inside the cleanup's scope, so an abort after it reclaims
# what the retry wrote. Excusing a retry with a prepare from the cleanup is
# the state before the prepare moved inside that scope.
[an aborted retry with a prepare skips the cleanup]
file internal/app/app.go
--- anchor
		if admitted {
--- replace
		if admitted || prepare != nil {
--- end

# A retry that cannot start, as at shutdown, leaves the Failed entry, and its
# note is what tells the user a retry fetches the held volumes.
[the entry of a job the finalizer retries carries no note]
file internal/app/job_finalizer.go
--- anchor
		extra = append(extra, heldVolumesRetryNote)
--- replace
		_ = heldVolumesRetryNote
--- end

[HasPostProcJob ignores active postProcAdmissions by ID]
file internal/app/dispatcher_wiring.go
--- anchor
	if app.postProcAdmissions.hasID(id) {
		return true
	}
--- replace
	if false {
		return true
	}
--- end

[HasPostProcJob checks pointer identity on postProcessor instead of ID]
file internal/app/dispatcher_wiring.go
--- anchor
	return app.postProcessor.HasJobID(id)
--- replace
	return app.postProcessor.HasJob(j)
--- end
