pkg ./internal/downloader/
run Test(FetchArticle_HandedOffJobIsNotFetched|BuildDispatchPlan_HandOffDuringTheArticleLoop)$

# fetchArticle's per-job check must drop a handed-off job's article before
# any network I/O, the same way it drops a paused/cancelled/superseded one.
# TestFetchArticle_HandedOffJobIsNotFetched pins the unmarkTried-related
# mutations below by pre-marking the article tried on two servers and
# reading the tracker's try-list entry directly after the drop: server 0
# (the one the dropped request was on) must clear, server 1 must not. A
# single marked server can't discriminate unmarkTried (clears one server)
# from clearTried (clears the whole entry) — both leave the same empty
# result — which is what let the "demoted to clearTried" mutation below
# survive against an earlier, single-server version of this test.
# TestDownloaderPerJobPauseResume, this pin's predecessor for the "dropped
# from the gate" mutation, is no longer in the run line: it saw the missing
# call only through a pause/resume download count sensitive to timing under
# load (#676). TestFetchArticle_HandedOffJobIsNotFetched also pre-marks its
# article emitted so the emitted-bit mutation below is observable through
# it.

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

[the tried-mark clear demoted to clearTried in the same gate]
file internal/downloader/dispatch.go
--- anchor
	if cur, ok := d.dispatcher.Job(req.jobID()); !ok || cur != req.job || req.job.Intent() != job.IntentRun || d.handedOff(req.job) {
		d.unmarkTried(req.jobID(), req.artIdx, serverIdx)
		_ = req.job.ClearArticleEmitted(int(req.artIdx))
		return nil, false
	}
--- replace
	if cur, ok := d.dispatcher.Job(req.jobID()); !ok || cur != req.job || req.job.Intent() != job.IntentRun || d.handedOff(req.job) {
		d.clearTried(req.jobID(), req.artIdx)
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
