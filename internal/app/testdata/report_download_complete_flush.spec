pkg ./internal/app/
run TestReportDownloadComplete_RecordsTheFileBeforeTheReport

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
