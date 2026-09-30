pkg ./internal/app/
run TestFinalize_RetriedJobIsNotRetriedAgain$

# #651's loop bound, pinned by the loop test alone: a retry of a par2 failure
# with held volumes releases them, so its own par2 failure is final. Either
# half removed lets the retry be retried again.

[the trigger ignores whether volumes are held]
file internal/app/job_finalizer.go
--- anchor
	return ppJob.Job != nil && ppJob.ParError && ppJob.Job.HasDeferredPar2()
--- replace
	return ppJob.Job != nil && ppJob.ParError
--- end

[the retry keeps its volumes held]
file internal/app/app.go
--- anchor
	if prepare != nil {
--- replace
	if false {
--- end
