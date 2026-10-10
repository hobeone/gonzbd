pkg ./internal/app/
run TestCompleteFinalizedFile_FeedsTheLastVolumeBeforeReportingTheDownload$

# The last file's volume reaches the job's DirectUnpacker before the report
# that the download finished, from which the tick can launch the
# post-processing that collects the unpacker.

[the volume is fed after the download-finished report]
file internal/app/app.go
--- anchor
		if pp := app.config.GetPostProc(); pp.DirectUnpack && pp.EnableUnrar && !fc.Resumed {
			app.duOrch.maybeStart(fc)
		}
		// A resumed completion neither marks the file nor dirties it: the
		// hydration or retry that finished it already committed complete = 1
		// (hydration through its SetComplete verdict; a retry through
		// verifyRetry's verdict and its retryFileStates row).
		if !fc.Resumed {
			if err := j.MarkFileComplete(fc.FileIdx); err != nil {
				app.logQueueWriteFailure("mark file complete", fc.JobID, fc.FileIdx, err)
				return err
			}
			app.markFileDirty(j, fc.FileIdx)
		}
--- replace
		// A resumed completion neither marks the file nor dirties it: the
		// hydration or retry that finished it already committed complete = 1
		// (hydration through its SetComplete verdict; a retry through
		// verifyRetry's verdict and its retryFileStates row).
		if !fc.Resumed {
			if err := j.MarkFileComplete(fc.FileIdx); err != nil {
				app.logQueueWriteFailure("mark file complete", fc.JobID, fc.FileIdx, err)
				return err
			}
			app.markFileDirty(j, fc.FileIdx)
		}
		if reported, _ := app.reportDownloadComplete(j, app.dispatcher); reported && app.downloadReportedHook != nil {
			app.downloadReportedHook(fc.JobID)
		}
		if pp := app.config.GetPostProc(); pp.DirectUnpack && pp.EnableUnrar && !fc.Resumed {
			app.duOrch.maybeStart(fc)
		}
--- end
