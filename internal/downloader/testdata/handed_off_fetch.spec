pkg ./internal/downloader/
run Test(FetchArticle_HandedOffJobIsNotFetched|BuildDispatchPlan_HandOffDuringTheArticleLoop|DownloaderPerJobPauseResume)$

# fetchArticle's per-job check must drop a handed-off job's article before
# any network I/O, the same way it drops a paused/cancelled/superseded one.
# TestDownloaderPerJobPauseResume is the existing test that pins the
# unmarkTried call in the second mutation below — it is shared by every
# trigger of this block, and this is the only trigger (pause) a resumed job
# can observe it through. TestFetchArticle_HandedOffJobIsNotFetched pre-marks
# its article emitted so the third mutation's ClearArticleEmitted removal is
# observable through it.

[the hand-off check dropped from fetchArticle's per-job gate]
file internal/downloader/dispatch.go
--- anchor
	if cur, ok := d.dispatcher.Job(req.jobID()); !ok || cur != req.job || req.job.Intent() != job.IntentRun || d.handedOff(req.job) {
		d.unmarkTried(req, serverIdx)
		_ = req.job.ClearArticleEmitted(int(req.artIdx))
		return nil, false
	}
--- replace
	if cur, ok := d.dispatcher.Job(req.jobID()); !ok || cur != req.job || req.job.Intent() != job.IntentRun {
		d.unmarkTried(req, serverIdx)
		_ = req.job.ClearArticleEmitted(int(req.artIdx))
		return nil, false
	}
--- end

[the tried-mark clear dropped from the same gate]
file internal/downloader/dispatch.go
--- anchor
	if cur, ok := d.dispatcher.Job(req.jobID()); !ok || cur != req.job || req.job.Intent() != job.IntentRun || d.handedOff(req.job) {
		d.unmarkTried(req, serverIdx)
		_ = req.job.ClearArticleEmitted(int(req.artIdx))
		return nil, false
	}
--- replace
	if cur, ok := d.dispatcher.Job(req.jobID()); !ok || cur != req.job || req.job.Intent() != job.IntentRun || d.handedOff(req.job) {
		_ = req.job.ClearArticleEmitted(int(req.artIdx))
		return nil, false
	}
--- end

[the emitted-bit clear dropped from the same gate]
file internal/downloader/dispatch.go
--- anchor
	if cur, ok := d.dispatcher.Job(req.jobID()); !ok || cur != req.job || req.job.Intent() != job.IntentRun || d.handedOff(req.job) {
		d.unmarkTried(req, serverIdx)
		_ = req.job.ClearArticleEmitted(int(req.artIdx))
		return nil, false
	}
--- replace
	if cur, ok := d.dispatcher.Job(req.jobID()); !ok || cur != req.job || req.job.Intent() != job.IntentRun || d.handedOff(req.job) {
		d.unmarkTried(req, serverIdx)
		return nil, false
	}
--- end
