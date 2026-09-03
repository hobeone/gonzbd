pkg ./internal/downloader/
run TestBuildDispatchPlan_SkipsCancelledJobs|TestHasDownloadableJobs_SkipsCancelledJobs|TestDownloader_FetchArticle_Coverage

[buildDispatchPlan offers a cancelled job's articles]
file internal/downloader/dispatch.go
--- anchor
		if row.View.Intent != job.IntentRun {
--- replace
		if row.View.Intent == job.IntentPause {
--- end

[hasDownloadableJobs counts a cancelled job as downloadable]
file internal/downloader/dispatch.go
--- anchor
		if row.View.Intent != job.IntentRun || row.View.Outcome.IsSettled() {
--- replace
		if row.View.Intent == job.IntentPause || row.View.Outcome.IsSettled() {
--- end

[fetchArticle keeps fetching a cancelled job's in-flight article]
file internal/downloader/dispatch.go
--- anchor
	if j, ok := d.dispatcher.Job(req.jobID); !ok || j.Intent() != job.IntentRun {
--- replace
	if j, ok := d.dispatcher.Job(req.jobID); ok && j.Intent() == job.IntentPause {
--- end
