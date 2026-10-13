pkg ./internal/app/
run ^(TestPeek_FlaggedVolumeIsNotFedToDirectUnpack|TestPeek_HitWhileDirectUnpackRuns_AbortsIt)$

# Blocking a job aborts its DirectUnpack, and the feed refuses a Blocked job,
# so a job the completion loop files as an unwanted failure
# (fileOwedUnwantedFailure) has no unpacker for enqueuePostProc to collect,
# and no DirectUnpack wait runs for it.

[blockForUnwanted leaves the running unpacker alive]
file internal/app/archive_peek.go
--- anchor
	app.duOrch.abortJob(jobID)
--- replace
	_ = jobID
--- end

[maybeStart feeds a Blocked job]
file internal/app/directunpack_orchestrator.go
--- anchor
	if st, _ := app.dispatcher.UnwantedState(fc.JobID); st == unwanted.StateBlocked {
--- replace
	if st, _ := app.dispatcher.UnwantedState(fc.JobID); st == unwanted.StateBlocked && false {
--- end
