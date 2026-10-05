pkg ./internal/app/
run TestEnqueuePostProc_StampsTheDownloadFinishAndHistoryRecordsTheDuration|TestRetriedJob_RestampsTheDownloadFinish

[the admission no longer stamps the download finish]
file internal/app/app.go
--- anchor
	if err := j.MarkDownloadFinished(time.Now()); err != nil {
--- replace
	if err := error(nil); err != nil {
--- end
