pkg ./internal/app/
run TestReportDownloadComplete_

[the flush before the report skipped]
file internal/app/app.go
--- anchor
	err := app.recorder.flush(flushCtx)
	flushCancel()
	if err != nil {
		app.log.Warn("recorder flush before the download-complete report failed", "job", j.ID(), "err", err)
--- replace
	err := flushCtx.Err()
	flushCancel()
	if err != nil {
		app.log.Warn("recorder flush before the download-complete report failed", "job", j.ID(), "err", err)
--- end

[a failed flush withholds the report]
file internal/app/app.go
--- anchor
	return true, rep.AdvanceFrom(j, job.Fetching, job.Assessing)
--- replace
	if err != nil {
		return false, nil
	}
	return true, rep.AdvanceFrom(j, job.Fetching, job.Assessing)
--- end
