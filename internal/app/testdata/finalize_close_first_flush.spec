pkg ./internal/app/
run TestFinalizeCompletedFile_(WithoutABarrier_ACloseFaultStopsTheCompletion|WithNoSyncTarget_ACloseFaultStopsTheCompletion|ACloseFaultAfterACommittedFinalizeIsTolerated|AStoppedAssemblerIsNotAFailedFirstFlush)$|TestHandleFileComplete_(AFailedFirstFlushIsNotShipped|AStoppedAssemblerDoesNotStallTheJob)$|TestRouteFinalizeFailure_FailsTheJobOnAPermanentFaultNothingRouted$

[a first-flush close fault read as post-hoc]
file internal/app/durability.go
--- anchor
		if !closeIsFirstFlush || errors.Is(cerr, assembler.ErrAssemblerStopped) {
--- replace
		if true || !closeIsFirstFlush || errors.Is(cerr, assembler.ErrAssemblerStopped) {
--- end

[a first-flush close fault logged but not returned]
file internal/app/durability.go
--- anchor
		err = fmt.Errorf("%w: job %s file %d: the close was its only flush: %w",
			ErrNotFinalized, jobID, fileIdx, cerr)
--- replace
		_ = cerr
--- end

[a first-flush close fault logged below Warn]
file internal/app/durability.go
--- anchor
		app.log.Warn("completed file's only flush failed at close; the completion is stopped",
--- replace
		app.log.Debug("completed file's only flush failed at close; the completion is stopped",
--- end

[the committed and closed-elsewhere paths held to the strict reading]
file internal/app/durability.go
--- anchor
	closeIsFirstFlush = false

	// Ask the assembler directly
--- replace
	_ = closeIsFirstFlush

	// Ask the assembler directly
--- end

[the no-barrier return read as post-hoc]
file internal/app/durability.go
--- anchor
	if app.barrier == nil {
		return nil
	}
	tgt := app.syncTargetFor(jobID)
--- replace
	if app.barrier == nil {
		closeIsFirstFlush = false
		return nil
	}
	tgt := app.syncTargetFor(jobID)
--- end

[the nil-target return read as post-hoc]
file internal/app/durability.go
--- anchor
	if tgt == nil {
		// A job still in the queue has lost its manifest but not its place,
--- replace
	if tgt == nil {
		closeIsFirstFlush = false
		// A job still in the queue has lost its manifest but not its place,
--- end

[a stopped assembler's unrun close read as a failed first flush]
file internal/app/durability.go
--- anchor
		if !closeIsFirstFlush || errors.Is(cerr, assembler.ErrAssemblerStopped) {
--- replace
		if !closeIsFirstFlush || false {
--- end

[an unrouted permanent fault stalled rather than failed]
file internal/app/durability.go
--- anchor
	if f.Permanent {
		app.Fail(jobID, f)
		return
	}
--- replace
	if false {
		app.Fail(jobID, f)
		return
	}
--- end

[an unrouted retryable fault not recorded for retry]
file internal/app/durability.go
--- anchor
	app.Stall(jobID, f)
	app.notePendingFinalize(jobID, fileIdx)
}
--- replace
	app.Stall(jobID, f)
}
--- end
