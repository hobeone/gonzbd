pkg ./internal/downloader/
run Test(FetchArticle_HandedOffJobIsNotFetched|BuildDispatchPlan_HandOffDuringTheArticleLoop|FetchArticle_HandedOffJobClearsTriedMark)$

# fetchArticle's per-job check must drop a handed-off job's article before
# any network I/O, the same way it drops a paused/cancelled/superseded one.
# TestFetchArticle_HandedOffJobClearsTriedMark pins the unmarkTried call in
# the second mutation below by reading the tracker's try-list entry directly
# after the drop, rather than inferring the clear from a pause/resume
# download count (TestDownloaderPerJobPauseResume, the prior pin here, saw it
# only through timing-sensitive article counts — #676).
# TestFetchArticle_HandedOffJobIsNotFetched pre-marks
# its article emitted so the third mutation's ClearArticleEmitted removal is
# observable through it.

[the hand-off check dropped from fetchArticle's per-job gate]
file internal/downloader/dispatch.go
--- anchor
	if cur, ok := d.dispatcher.Job(req.jobID()); !ok || cur != req.job || req.job.Intent() != job.IntentRun || d.handedOff(req.job) {
		d.unmarkTried(req.jobID(), req.artIdx, serverIdx)
		_ = req.job.ClearArticleEmitted(int(req.artIdx))
		return nil, false
	}
--- replace
	if cur, ok := d.dispatcher.Job(req.jobID()); !ok || cur != req.job || req.job.Intent() != job.IntentRun {
		d.unmarkTried(req.jobID(), req.artIdx, serverIdx)
		_ = req.job.ClearArticleEmitted(int(req.artIdx))
		return nil, false
	}
--- end

[the tried-mark clear dropped from the same gate]
file internal/downloader/dispatch.go
--- anchor
	if cur, ok := d.dispatcher.Job(req.jobID()); !ok || cur != req.job || req.job.Intent() != job.IntentRun || d.handedOff(req.job) {
		d.unmarkTried(req.jobID(), req.artIdx, serverIdx)
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
		d.unmarkTried(req.jobID(), req.artIdx, serverIdx)
		_ = req.job.ClearArticleEmitted(int(req.artIdx))
		return nil, false
	}
--- replace
	if cur, ok := d.dispatcher.Job(req.jobID()); !ok || cur != req.job || req.job.Intent() != job.IntentRun || d.handedOff(req.job) {
		d.unmarkTried(req.jobID(), req.artIdx, serverIdx)
		return nil, false
	}
--- end
