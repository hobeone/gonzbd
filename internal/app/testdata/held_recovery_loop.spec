pkg ./internal/app/
run ^(TestFinalize_RetriedJobIsNotRetriedAgain|TestFinalize_RetriesOnlyAParErrorWithHeldVolumes|TestFinalize_UnknownShortfallRetriesUnsized|TestFinalize_KnownShortfallRetriesSized|TestFinalize_ParErrorWithHeldVolumesRetriesWithThemReleased|TestFinalize_SizedRetryThatFallsShortRetriesOnceUnsized)$

# #651's loop bound: a retry of a par2 failure with held volumes releases
# them, so its own par2 failure has none held. Either half removed breaks a
# test of the retry (the ledger bounds the retries on its own as well, so the
# loop test no longer pins these halves alone; held_retry_sizing.spec pins the
# ledger).

[the trigger ignores whether volumes are held]
file internal/app/job_finalizer.go
--- anchor
	return ppJob.ParError && ppJob.Job.HasDeferredPar2()
--- replace
	return ppJob.ParError
--- end

[the retry keeps its volumes held]
file internal/app/app.go
--- anchor
	if prepare != nil {
--- replace
	if false {
--- end
