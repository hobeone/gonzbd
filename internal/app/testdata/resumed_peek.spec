pkg ./internal/app/
run Test(InstallVerification_PeeksAFinishedFileBeforeMarkingItComplete|InstallVerification_TheResidencyPeekBlocksTheJobBeforeTheMark|CompleteFinalizedFile_ResumedCompletionIsNotPeeked|Hydrate_PeeksEachFileTheVerifierFinishedBeforeItIsMarked|Hydrate_RealPeekBlocksTheJobFromInsideTheTicksHydration)$

[the hydration drops the peek's failure message]
file internal/app/residency.go
--- anchor
		peek = func(fi int) { failMsgs[fi] = r.peek(j, fi) }
--- replace
		peek = func(fi int) { _ = r.peek(j, fi) }
--- end

[the Resumed completion is enqueued without the message]
file internal/app/record.go
--- anchor
	fc := FileComplete{JobID: jobID, FileIdx: fileIdx, Resumed: true, FailMsg: failMsg}
--- replace
	fc := FileComplete{JobID: jobID, FileIdx: fileIdx, Resumed: true}
--- end

[the consumer ignores the carried message]
file internal/app/app.go
--- anchor
		unwantedFail := fc.FailMsg
--- replace
		unwantedFail := ""
--- end

[installVerification marks a finished file before it peeks it]
file internal/app/residency.go
--- anchor
			if peek != nil {
				peek(fi)
			}
			if err := j.MarkFileComplete(fi); err != nil {
--- replace
			_ = j.MarkFileComplete(fi)
			if peek != nil {
				peek(fi)
			}
			if err := j.MarkFileComplete(fi); err != nil {
--- end

[installVerification never peeks a finished file]
file internal/app/residency.go
--- anchor
			if peek != nil {
				peek(fi)
			}
--- replace
--- end

[Hydrate does not hand the peek to installVerification]
file internal/app/residency.go
--- anchor
		peek = func(fi int) { failMsgs[fi] = r.peek(j, fi) }
--- replace
		peek = nil
--- end

[the application installs no peek on its residency]
file internal/app/app.go
--- anchor
	app.residency.peek = app.peekResumedFile
--- replace
--- end

[the Resumed consumer peeks again]
file internal/app/app.go
--- anchor
		if !fc.Resumed {
			unwantedFail = app.peekArchiveForUnwanted(j, fc)
--- replace
		if true {
			unwantedFail = app.peekArchiveForUnwanted(j, fc)
--- end
