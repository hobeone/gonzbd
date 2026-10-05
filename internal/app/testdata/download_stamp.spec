pkg ./internal/app/
run TestDownloadFinish_IsStampedWhenTheJobLeavesFetching|TestDemotionToFetching_RestampsTheDownloadFinish|TestEnqueuePostProc_FromFetching_StampsTheFinishWhenNoneIsSet|TestEnqueuePostProc_KeepsTheFinishAJobLeftFetchingWith

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
