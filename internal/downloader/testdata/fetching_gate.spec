pkg ./internal/downloader/
run TestBuildDispatchPlan_SkipsNonFetchingJob|TestHasDownloadableJobs_SkipsNonFetchingJob

[forEachUnfinishedArticle never skips a non-Fetching job]
file internal/downloader/dispatch.go
--- anchor
		if !row.View.Running || row.View.State != job.Fetching {
--- replace
		if false {
--- end

[hasDownloadableJobs never skips a non-Fetching job]
file internal/downloader/dispatch.go
--- anchor
		if row.View.State == job.Fetching || (row.View.State == job.StateUnset && row.View.Next == job.Fetching) {
--- replace
		if true {
--- end
