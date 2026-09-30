pkg ./internal/app/
run Test(Finalize_ParErrorWithHeldVolumesRetriesWithThemReleased|Finalize_RetriedJobIsNotRetriedAgain|Finalize_RetryThatCannotStartLeavesTheFailureVisible|Finalize_RetriesOnlyAParErrorWithHeldVolumes|RetryHistoryJob_PrepareErrorAbortsTheRetry)$

# #651: a par2 failure while recovery volumes were held back retries the job
# with them released. Each clause of the trigger, the release in the retry,
# the loop bound, the notification rule and the prepare's abort, one at a time.

[the trigger ignores whether par2 failed]
file internal/app/job_finalizer.go
--- anchor
	return ppJob.Job != nil && ppJob.ParError && ppJob.Job.HasDeferredPar2()
--- replace
	return ppJob.Job != nil && ppJob.Job.HasDeferredPar2()
--- end

[the trigger ignores whether volumes are held, so the retry is retried again]
file internal/app/job_finalizer.go
--- anchor
	return ppJob.Job != nil && ppJob.ParError && ppJob.Job.HasDeferredPar2()
--- replace
	return ppJob.Job != nil && ppJob.ParError
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
