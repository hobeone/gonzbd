pkg ./internal/app/
run TestLooseRecord_ResumedCompletionSurvivesEviction

[a resumed completion dirties the file again]
file internal/app/app.go
--- anchor
		if !fc.Resumed {
			if err := j.MarkFileComplete(fc.FileIdx); err != nil {
				app.logQueueWriteFailure("mark file complete", fc.JobID, fc.FileIdx, err)
				return err
			}
			app.markFileDirty(j, fc.FileIdx)
		}
--- replace
		if !fc.Resumed {
			if err := j.MarkFileComplete(fc.FileIdx); err != nil {
				app.logQueueWriteFailure("mark file complete", fc.JobID, fc.FileIdx, err)
				return err
			}
		}
		app.markFileDirty(j, fc.FileIdx)
--- end
