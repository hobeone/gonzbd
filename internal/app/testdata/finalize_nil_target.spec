pkg ./internal/app/
run TestHandleFileComplete_ANonResident(JobsFileIsNotDeliveredUntrimmed|CompletionDrainedAtShutdown)$

[a queued job's nil-target finalize answered nil]
file internal/app/durability.go
--- anchor
			if _, queued := app.dispatcher.Job(jobID); queued {
--- replace
			if _, queued := app.dispatcher.Job(jobID); false && queued {
--- end

[the retry's nil-target refusal not marked non-resident]
file internal/app/stall.go
--- anchor
			"can be run over it: %w", ErrNotFinalized, jobID, fileIdx, job.ErrNotResident)
--- replace
			"can be run over it: %v", ErrNotFinalized, jobID, fileIdx, job.ErrNotResident)
--- end

[a non-resident finalize routed as a storage fault]
file internal/app/durability.go
--- anchor
	if errors.Is(err, job.ErrNotResident) {
		app.log.Info("completed file was not finalized because its job is not resident; "+
--- replace
	if false {
		app.log.Info("completed file was not finalized because its job is not resident; "+
--- end

[a non-resident finalize logged as an error]
file internal/app/durability.go
--- anchor
		app.log.Info("completed file was not finalized because its job is not resident; "+
--- replace
		app.log.Error("completed file was not finalized because its job is not resident; "+
--- end
