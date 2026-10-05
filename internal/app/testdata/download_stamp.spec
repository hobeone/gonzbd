pkg ./internal/app/
run TestDownloadFinish_IsStampedWhenTheJobLeavesFetching|TestDemotionToFetching_RestampsTheDownloadFinish|TestEnqueuePostProc_FromFetching_StampsTheFinishWhenNoneIsSet|TestEnqueuePostProc_KeepsTheFinishAJobLeftFetchingWith|TestAdvanceToFetching_WithADeferredFailureReason_KeepsTheFinish

[the Fetching exit report no longer stamps the finish, leaving only the hand-over fallback]
file internal/app/app.go
--- anchor
	if err := j.MarkDownloadFinished(time.Now()); err != nil {
		app.log.Warn("could not record the download finish time", "job", j.ID(), "err", err)
	}
--- replace
--- end

[the hand-over from Fetching no longer stamps the finish]
file internal/app/app.go
--- anchor
	if err := j.MarkDownloadFinished(time.Now()); err != nil {
		app.log.Warn("postproc: could not record the download finish time", "job", j.ID(), "err", err)
	}
--- replace
--- end

[the demotion to Fetching keeps the finish it recorded on the way out]
file internal/app/runner.go
--- anchor
		if next == job.Fetching {
--- replace
		if false {
--- end

[the demotion reopens the finish before the deferred failure reason has had the job]
file internal/app/runner.go
--- anchor
	if r.app != nil {
		if reasons := r.app.postProcAdmissions.takeDeferred(j); len(reasons) > 0 {
--- replace
	if r.app != nil {
		if next == job.Fetching {
			_ = j.ClearDownloadFinished()
		}
		if reasons := r.app.postProcAdmissions.takeDeferred(j); len(reasons) > 0 {
--- end
