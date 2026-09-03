pkg ./internal/downloader/
run TestBuildDispatchPlan_SkipsCancelledJobs|TestHasDownloadableJobs_SkipsCancelledJobs|TestDownloader_FetchArticle_Coverage

[forEachUnfinishedArticle offers a cancelled job's articles]
file internal/downloader/dispatch.go
--- anchor
		if a.jobIntent != job.IntentRun {
--- replace
		if a.jobIntent == job.IntentPause {
--- end

[hasDownloadableJobs counts a cancelled job as downloadable]
file internal/downloader/dispatch.go
--- anchor
		if j.Intent() != job.IntentRun {
--- replace
		if j.Intent() == job.IntentPause {
--- end

[fetchArticle keeps fetching a cancelled job's in-flight article]
file internal/downloader/dispatch.go
--- anchor
	if j, ok := d.jobs.Job(req.jobID); ok && j.Intent() != job.IntentRun {
--- replace
	if j, ok := d.jobs.Job(req.jobID); ok && j.Intent() == job.IntentPause {
--- end
