pkg ./internal/app/
run TestCheckpointJob_ACommitErrorStallsTheJobUntilReevaluated|TestCheckpointJob_KeepsPendingBytesOverNoFiles|TestCheckpointJob_LeavesPendingBytesWhenNoBarrierRan

[a failed barrier settles the accumulator]
file internal/app/durability.go
--- anchor
		app.log.Warn("checkpoint barrier failed", "job", jobID, "err", err)
--- replace
		app.log.Warn("checkpoint barrier failed", "job", jobID, "err", err)
		app.settleJobBytes(jobID, pending)
--- end

[a successful barrier never settles]
file internal/app/durability.go
--- anchor
	app.settleJobBytes(jobID, pending)
	return true
--- replace
	_ = pending
	return true
--- end

[a checkpoint over no files settles]
file internal/app/durability.go
--- anchor
		app.log.Debug("checkpoint skipped, the job has no open files to sync", "job", jobID)
--- replace
		app.log.Debug("checkpoint skipped, the job has no open files to sync", "job", jobID)
		app.settleJobBytes(jobID, app.pendingBytesFor(jobID))
--- end

[the nil-target branch settles]
file internal/app/durability.go
--- anchor
		app.log.Debug("checkpoint skipped, no sync target for the job", "job", jobID)
--- replace
		app.log.Debug("checkpoint skipped, no sync target for the job", "job", jobID)
		app.settleJobBytes(jobID, app.pendingBytesFor(jobID))
--- end
