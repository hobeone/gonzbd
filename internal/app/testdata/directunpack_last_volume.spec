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
	//
	// The settle, the peek and the feed above each read the manifest and
	// do nothing for an evicted job, and the mark is what lets the job
	// reach Assessing. So a completion that finds the job evicted here is
	// not marked: one of them may have been skipped. The check reads
	// residency now, not at each step, so an eviction and a re-hydration
	// both landing after a step and before it go unseen. MarkFileComplete
	// itself needs only progress (#868), so an eviction after this check
	// still takes the mark, behind a settle, peek and feed that ran.
	if !fc.Resumed {
		if !j.Resident() {
			err := fmt.Errorf("job %s: %w", fc.JobID, job.ErrNotResident)
			app.logQueueWriteFailure("mark file complete", fc.JobID, fc.FileIdx, err)
			return err
		}
		if app.residentCheckedHook != nil {
			app.residentCheckedHook(j)
		}
		if err := j.MarkFileComplete(fc.FileIdx); err != nil {
			app.logQueueWriteFailure("mark file complete", fc.JobID, fc.FileIdx, err)
			return err
		}
		app.markFileDirty(j, fc.FileIdx)
	}
--- replace
	if !fc.Resumed {
		if !j.Resident() {
			err := fmt.Errorf("job %s: %w", fc.JobID, job.ErrNotResident)
			app.logQueueWriteFailure("mark file complete", fc.JobID, fc.FileIdx, err)
			return err
		}
		if app.residentCheckedHook != nil {
			app.residentCheckedHook(j)
		}
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
