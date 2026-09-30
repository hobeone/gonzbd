pkg ./internal/app/
run TestHandleFileComplete_ACompletionDrainedAfterTheAssemblerStopsIsWithheld$|TestResume_ACompletionDrainedAfterTheAssemblerStopsIsRederived$|TestWithholdUntrimmed_AnswersByWhetherTheJobIsQueued$

[a stopped-assembler completion answered nil]
file internal/app/durability.go
--- anchor
		return app.withholdUntrimmed(jobID, fileIdx,
			fmt.Errorf("the assembler stopped before it was trimmed: %w", err))
--- replace
		return nil
--- end

[the stopped-assembler refusal not marked as a stop]
file internal/app/durability.go
--- anchor
			fmt.Errorf("the assembler stopped before it was trimmed: %w", err))
--- replace
			fmt.Errorf("the assembler stopped before it was trimmed: %v", err))
--- end

[a queued job's untrimmed completion answered nil]
file internal/app/durability.go
--- anchor
		if _, queued := app.dispatcher.Job(jobID); queued {
--- replace
		if _, queued := app.dispatcher.Job(jobID); false && queued {
--- end

[a departed job's untrimmed completion withheld]
file internal/app/durability.go
--- anchor
		if _, queued := app.dispatcher.Job(jobID); queued {
--- replace
		if _, queued := app.dispatcher.Job(jobID); true || queued {
--- end

[a stopped-assembler finalize routed as a halt]
file internal/app/durability.go
--- anchor
	if errors.Is(err, assembler.ErrAssemblerStopped) {
		app.log.Info("completed file was not finalized because the assembler has stopped; "+
--- replace
	if false {
		app.log.Info("completed file was not finalized because the assembler has stopped; "+
--- end

[a stopped-assembler finalize logged as an error]
file internal/app/durability.go
--- anchor
		app.log.Info("completed file was not finalized because the assembler has stopped; "+
--- replace
		app.log.Error("completed file was not finalized because the assembler has stopped; "+
--- end

[a stopped-assembler finalize not recorded pending]
file internal/app/durability.go
--- anchor
			"the next start re-derives it from its durable runs",
			"job", jobID, "fileidx", fileIdx, "err", err)
		app.notePendingFinalize(jobID, fileIdx)
--- replace
			"the next start re-derives it from its durable runs",
			"job", jobID, "fileidx", fileIdx, "err", err)
--- end
