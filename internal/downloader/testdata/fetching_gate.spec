pkg ./internal/downloader/
run TestBuildDispatchPlan_SkipsNonFetchingJob|TestHasDownloadableJobs_SkipsNonFetchingJob

[forEachUnfinishedArticle never skips a non-Fetching job]
file internal/downloader/dispatch.go
--- anchor
		if j.State().State != job.Fetching {
--- replace
		if false {
--- end

[hasDownloadableJobs never skips a non-Fetching job]
file internal/downloader/dispatch.go
--- anchor
		if !ok || !j.Resident() || j.State().State != job.Fetching {
--- replace
		if !ok || !j.Resident() {
--- end
