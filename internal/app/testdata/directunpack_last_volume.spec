pkg ./internal/app/
run TestCompleteFinalizedFile_FeedsTheLastVolumeBeforeReportingTheDownload$

# The last file's volume reaches the job's DirectUnpacker before the report
# that the download finished, from which the tick can launch the
# post-processing that collects the unpacker.

[the volume is fed after the download-finished report]
file internal/app/app.go
--- anchor
		if pp := app.config.GetPostProc(); pp.DirectUnpack && pp.EnableUnrar {
			app.duOrch.maybeStart(fc)
		}
		if err := j.MarkFileComplete(fc.FileIdx); err != nil {
			app.logQueueWriteFailure("mark file complete", fc.JobID, fc.FileIdx, err)
			return err
		}
		if app.checkpointer != nil {
			app.checkpointer.Mark(j)
		}
		// The Fetching worker's exit report. A stale one is a repeat for a
		// job that has already moved on, and must leave its next state alone.
		if reported, err := app.reportDownloadComplete(j, app.dispatcher); reported {
			if err != nil && !errors.Is(err, dispatch.ErrStaleReport) {
				app.logQueueWriteFailure("report download complete", fc.JobID, fc.FileIdx, err)
			}
			if app.downloadReportedHook != nil {
				app.downloadReportedHook(fc.JobID)
			}
		}
--- replace
		if err := j.MarkFileComplete(fc.FileIdx); err != nil {
			app.logQueueWriteFailure("mark file complete", fc.JobID, fc.FileIdx, err)
			return err
		}
		if app.checkpointer != nil {
			app.checkpointer.Mark(j)
		}
		// The Fetching worker's exit report. A stale one is a repeat for a
		// job that has already moved on, and must leave its next state alone.
		if reported, err := app.reportDownloadComplete(j, app.dispatcher); reported {
			if err != nil && !errors.Is(err, dispatch.ErrStaleReport) {
				app.logQueueWriteFailure("report download complete", fc.JobID, fc.FileIdx, err)
			}
			if app.downloadReportedHook != nil {
				app.downloadReportedHook(fc.JobID)
			}
		}
		if pp := app.config.GetPostProc(); pp.DirectUnpack && pp.EnableUnrar {
			app.duOrch.maybeStart(fc)
		}
--- end
